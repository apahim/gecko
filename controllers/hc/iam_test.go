package hc_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/client/transport/mock"
	"github.com/openshift-online/gecko/controllers/hc"
	"github.com/openshift-online/gecko/controllers/util/catalog/catalogtest"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recordingObservations struct {
	*mock.Client
	observations int
}

func (tr *recordingObservations) GetStatus(ctx context.Context, target, key string) (*transport.Status, error) {
	tr.observations++
	return tr.Client.GetStatus(ctx, target, key)
}

func dispatchFixture(t *testing.T) (*privatev1.Cluster, *privatev1.Version, *privatev1.WIFConfigTemplate, client.Client) {
	t.Helper()
	c := buildReadyCluster("fresh-install", "5.0.0")
	c.Status.IAMDispatch = nil
	v := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "5.0.0"}, Spec: privatev1.VersionSpec{ReleaseImage: c.Status.VersionResolution.ReleaseImage, ChannelGroups: []string{"stable"}}}
	w := catalogtest.Ready(t, v)
	store := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(&privatev1.Cluster{}, &privatev1.Version{}).WithObjects(c, v, w).Build()
	return c, v, w, store
}

func TestInitialIAMDispatchIsDurableBeforeApplyAndRedelivers(t *testing.T) {
	c, v, w, store := dispatchFixture(t)
	tr := &recordingObservations{Client: mock.New()}
	r := hc.New(tr, testLogger(t), store, nil, "", "").WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	current := &privatev1.Cluster{}
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
	require.NotNil(t, current.Status.IAMDispatch.Pending)
	require.Equal(t, *v.Spec.WIFConfigTemplateRef, current.Status.IAMDispatch.Pending.TemplateRef)
	// Crash/restart after commit, then revoke eligibility before RPC redelivery.
	w.Spec.Verified = new(false)
	require.NoError(t, store.Update(t.Context(), w))
	require.NoError(t, store.Delete(t.Context(), v))
	r = hc.New(tr, testLogger(t), store, nil, "", "").WithAPIReader(store)
	_, err = r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Len(t, tr.ApplyCalls, 1)
	require.Zero(t, tr.observations, "a matching committed intent redelivers instead of freezing a partial BulkWriter")
}

func TestInitialMissingIAMCannotApplyOrFabricateObservation(t *testing.T) {
	c, _, _, store := dispatchFixture(t)
	c.Spec.Platform.GCP.IAM = nil
	require.NoError(t, store.Update(t.Context(), c))
	tr := &recordingObservations{Client: mock.New()}
	r := hc.New(tr, testLogger(t), store, nil, "", "").WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	require.Zero(t, tr.observations)
	current := &privatev1.Cluster{}
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
	require.Nil(t, current.Status.IAMDispatch)
	require.Equal(t, privatev1.WIFConfigUpdateRequired, meta.FindStatusCondition(current.Status.Conditions, privatev1.IAMPermissionsReady).Reason)
}

type rejectDispatchStatus struct{ client.Client }
type rejectDispatchWriter struct{ client.SubResourceWriter }

func (c rejectDispatchStatus) Status() client.SubResourceWriter {
	return rejectDispatchWriter{c.Client.Status()}
}
func (w rejectDispatchWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if c, ok := obj.(*privatev1.Cluster); ok && c.Status.IAMDispatch != nil && c.Status.IAMDispatch.Pending != nil {
		return apierrors.NewConflict(schema.GroupResource{Resource: "clusters"}, c.Name, nil)
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestIAMDispatchConflictHasZeroApply(t *testing.T) {
	c, _, _, store := dispatchFixture(t)
	tr := mock.New()
	r := hc.New(tr, testLogger(t), rejectDispatchStatus{store}, nil, "", "").WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	current := &privatev1.Cluster{}
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
	require.Nil(t, current.Status.IAMDispatch)
}

func TestNewDispatchNegativeMatrixHasZeroApply(t *testing.T) {
	for _, scenario := range []string{"unverified", "availability-false", "availability-unknown", "availability-stale", "wrong-application-uid", "wrong-application-digest", "empty-application-history", "payload-changed", "pin-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			c, v, w, store := dispatchFixture(t)
			switch scenario {
			case "unverified":
				w.Spec.Verified = new(false)
				require.NoError(t, store.Update(t.Context(), w))
			case "availability-false", "availability-unknown", "availability-stale":
				if scenario == "availability-false" {
					v.Status.Conditions[0].Status = metav1.ConditionFalse
				}
				if scenario == "availability-unknown" {
					v.Status.Conditions[0].Status = metav1.ConditionUnknown
				}
				if scenario == "availability-stale" {
					v.Status.Conditions[0].ObservedGeneration--
				}
				require.NoError(t, store.Status().Update(t.Context(), v))
			case "wrong-application-uid":
				c.Spec.Platform.GCP.IAM.TemplateApplications[0].UID = "not-the-pinned-template"
				require.NoError(t, store.Update(t.Context(), c))
			case "wrong-application-digest":
				c.Spec.Platform.GCP.IAM.TemplateApplications[0].Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
				require.NoError(t, store.Update(t.Context(), c))
			case "empty-application-history":
				c.Spec.Platform.GCP.IAM.TemplateApplications = nil
				require.NoError(t, store.Update(t.Context(), c))
			case "payload-changed":
				v.Spec.ReleaseImage = "changed-after-resolution"
				require.NoError(t, store.Update(t.Context(), v))
			case "pin-replaced":
				require.NoError(t, store.Delete(t.Context(), w))
				w.UID = "replacement"
				w.ResourceVersion = ""
				require.NoError(t, store.Create(t.Context(), w))
			}
			tr := &recordingObservations{Client: mock.New()}
			r := hc.New(tr, testLogger(t), store, nil, "", "").WithAPIReader(store)
			_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
			require.NoError(t, err)
			require.Empty(t, tr.ApplyCalls)
			require.Zero(t, tr.observations)
			current := &privatev1.Cluster{}
			require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
			require.Nil(t, current.Status.IAMDispatch)
		})
	}
}

func TestPendingIntentAloneDoesNotAuthorizeEmptyGroupObservation(t *testing.T) {
	c, _, _, store := dispatchFixture(t)
	tr := &recordingObservations{Client: mock.New()}
	r := hc.New(tr, testLogger(t), store, nil, "", "").WithAPIReader(store)
	_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	current := &privatev1.Cluster{}
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
	require.NotNil(t, current.Status.IAMDispatch.Pending)
	// Simulate missing coverage before delivery. Pending proves intent,
	// never that the external group exists; no synthetic success can be observed.
	current.Spec.Platform.GCP.IAM.TemplateApplications = nil
	require.NoError(t, store.Update(t.Context(), current))
	_, err = r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	require.Zero(t, tr.observations)
	require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
	current.Status.HostedClusterResult = &privatev1.HostedClusterResult{Version: "5.0.0"}
	require.NoError(t, store.Status().Update(t.Context(), current))
	_, err = r.Reconcile(t.Context(), clusterReq(c.Name))
	require.NoError(t, err)
	require.Equal(t, 1, tr.observations)
}

type countPromotions struct {
	client.Client
	promotions *int
}
type countPromotionWriter struct {
	client.SubResourceWriter
	promotions *int
}

func (c countPromotions) Status() client.SubResourceWriter {
	return countPromotionWriter{c.Client.Status(), c.promotions}
}
func (w countPromotionWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if c, ok := obj.(*privatev1.Cluster); ok && c.Status.IAMDispatch != nil && c.Status.IAMDispatch.Pending == nil && c.Status.IAMDispatch.Dispatched != nil {
		*w.promotions++
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestControllerPromotesOnlyNonStaleFeedbackAtomically(t *testing.T) {
	for _, observeOnly := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			t.Run(fmt.Sprintf("observe=%t/stale=%t", observeOnly, stale), func(t *testing.T) {
				c, _, _, store := dispatchFixture(t)
				tr := &recordingObservations{Client: mock.New()}
				r := hc.New(tr, testLogger(t), store, nil, "", "").WithAPIReader(store)
				_, err := r.Reconcile(t.Context(), clusterReq(c.Name))
				require.NoError(t, err)
				current := &privatev1.Cluster{}
				require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
				pending := current.Status.IAMDispatch.Pending.DeepCopy()
				if observeOnly {
					current.Spec.Platform.GCP.IAM.TemplateApplications = nil
					require.NoError(t, store.Update(t.Context(), current))
					current.Status.HostedClusterResult = &privatev1.HostedClusterResult{Version: "5.0.0"}
					require.NoError(t, store.Status().Update(t.Context(), current))
				}
				key := transport.ResourceKey("hypershift.openshift.io", "v1beta1", "hostedclusters", "clusters-"+string(c.UID), c.Spec.SafeName)
				group, err := transport.ClusterGroupKey(c.Namespace, c.Name)
				require.NoError(t, err)
				tr.StatusOverrides[c.Status.PlacementResult.ManagementClusterName+"/"+group] = &transport.Status{Stale: stale, ResourceStatuses: map[string]map[string]string{key: {"version": "5.0.0", "desiredVersion": "5.0.0"}}}
				count := 0
				r = hc.New(tr, testLogger(t), countPromotions{store, &count}, nil, "", "").WithAPIReader(store)
				_, err = r.Reconcile(t.Context(), clusterReq(c.Name))
				require.NoError(t, err)
				require.NoError(t, store.Get(t.Context(), client.ObjectKeyFromObject(c), current))
				if stale {
					require.Equal(t, pending, current.Status.IAMDispatch.Pending)
					require.Zero(t, count)
				} else {
					require.Nil(t, current.Status.IAMDispatch.Pending)
					require.Equal(t, pending, current.Status.IAMDispatch.Dispatched)
					require.Equal(t, 1, count)
				}
			})
		}
	}
}
