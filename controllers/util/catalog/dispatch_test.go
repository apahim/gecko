package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func availableVersion(pin *privatev1.WIFConfigTemplateReference, status metav1.ConditionStatus, reason string, generation int64) *privatev1.Version {
	v := &privatev1.Version{
		ObjectMeta: metav1.ObjectMeta{Name: "5.0.6", Generation: generation},
		Spec:       privatev1.VersionSpec{ReleaseImage: "release-image", ChannelGroups: []string{"stable"}, WIFConfigTemplateReference: pin},
	}
	if status != "" || reason != "" {
		v.Status.Conditions = []metav1.Condition{{Type: privatev1.VersionAvailable, Status: status, Reason: reason, ObservedGeneration: generation}}
	}
	return v
}

func pinnedCluster(pin *privatev1.WIFConfigTemplateReference, apps ...privatev1.WIFConfigTemplateReference) *privatev1.Cluster {
	cluster := &privatev1.Cluster{Spec: privatev1.ClusterSpec{Release: privatev1.ClusterReleaseSpec{Version: "5.0.6", ChannelGroup: "stable"}}}
	if len(apps) > 0 {
		cluster.Spec.Platform = privatev1.ClusterPlatformSpec{GCP: &privatev1.GCPClusterPlatform{
			IAM: &privatev1.ClusterIAMSpec{TemplateApplications: apps},
		}}
	}
	_ = pin
	return cluster
}

var testPin = privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("a", 64)}

func dispatchStore(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithObjects(objects...).Build()
}

// The shared predicate must fail closed on every unavailability shape and
// distinguish customer coverage gaps (WIFConfigUpdateRequired) from platform
// catalog gaps (VersionIAMUnavailable).
func TestEvaluateDispatchNewWork(t *testing.T) {
	for _, tc := range []struct {
		name         string
		version      *privatev1.Version
		cluster      *privatev1.Cluster
		wantEligible bool
		wantReason   string
	}{
		{
			name:         "available, pinned and covered",
			version:      availableVersion(&testPin, metav1.ConditionTrue, "Available", 3),
			cluster:      pinnedCluster(&testPin, testPin),
			wantEligible: true,
		},
		{
			name:       "missing version",
			version:    nil,
			cluster:    pinnedCluster(&testPin, testPin),
			wantReason: privatev1.VersionIAMUnavailable,
		},
		{
			name: "wrong channel group",
			version: func() *privatev1.Version {
				v := availableVersion(&testPin, metav1.ConditionTrue, "Available", 1)
				v.Spec.ChannelGroups = []string{"nightly"}
				return v
			}(),
			cluster:    pinnedCluster(&testPin, testPin),
			wantReason: privatev1.VersionIAMUnavailable,
		},
		{
			name:       "unverified template blocks availability",
			version:    availableVersion(&testPin, metav1.ConditionFalse, "TemplateUnverified", 1),
			cluster:    pinnedCluster(&testPin, testPin),
			wantReason: privatev1.VersionIAMUnavailable,
		},
		{
			name:       "unpinned version blocks",
			version:    availableVersion(nil, metav1.ConditionFalse, "TemplateReferenceMissing", 1),
			cluster:    pinnedCluster(&testPin, testPin),
			wantReason: privatev1.VersionIAMUnavailable,
		},
		{
			name:       "missing availability condition blocks",
			version:    availableVersion(&testPin, "", "", 1),
			cluster:    pinnedCluster(&testPin, testPin),
			wantReason: privatev1.VersionIAMUnavailable,
		},
		{
			name: "stale availability generation blocks",
			version: func() *privatev1.Version {
				v := availableVersion(&testPin, metav1.ConditionTrue, "Available", 1)
				v.Generation = 2
				return v
			}(),
			cluster:    pinnedCluster(&testPin, testPin),
			wantReason: privatev1.VersionIAMUnavailable,
		},
		{
			name:       "missing cluster coverage",
			version:    availableVersion(&testPin, metav1.ConditionTrue, "Available", 1),
			cluster:    pinnedCluster(&testPin),
			wantReason: privatev1.WIFConfigUpdateRequired,
		},
		{
			name:       "wrong application digest does not cover the pin",
			version:    availableVersion(&testPin, metav1.ConditionTrue, "Available", 1),
			cluster:    pinnedCluster(&testPin, privatev1.WIFConfigTemplateReference{Name: "5.0.0", UID: "template-uid", Digest: "sha256:" + strings.Repeat("b", 64)}),
			wantReason: privatev1.WIFConfigUpdateRequired,
		},
		{
			name:       "later boundary with equal digest does not cover the pin",
			version:    availableVersion(&testPin, metav1.ConditionTrue, "Available", 1),
			cluster:    pinnedCluster(&testPin, privatev1.WIFConfigTemplateReference{Name: "5.0.3", UID: "template-uid", Digest: testPin.Digest}),
			wantReason: privatev1.WIFConfigUpdateRequired,
		},
		{
			name:         "retained earlier assertion covers the pin",
			version:      availableVersion(&testPin, metav1.ConditionTrue, "Available", 1),
			cluster:      pinnedCluster(&testPin, privatev1.WIFConfigTemplateReference{Name: "other", UID: "other-uid", Digest: "sha256:" + strings.Repeat("c", 64)}, testPin),
			wantEligible: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{tc.cluster}
			if tc.version != nil {
				objects = append(objects, tc.version)
			}
			evaluation, err := EvaluateDispatch(context.Background(), dispatchStore(objects...), tc.cluster, "5.0.6", "stable")
			require.NoError(t, err, "infrastructure failures must be retryable errors, not evaluations")
			permitted, reason, message := evaluation.NewWorkPermitted()
			if tc.wantEligible {
				assert.True(t, permitted)
				assert.Empty(t, reason)
				require.NotNil(t, evaluation.Intent)
				assert.Equal(t, DispatchIntent{ReleaseVersion: "5.0.6", ReleaseImage: "release-image", ChannelGroup: "stable", TemplateReference: testPin}, *evaluation.Intent)
				return
			}
			assert.False(t, permitted)
			assert.Equal(t, tc.wantReason, reason)
			assert.NotEmpty(t, message)
		})
	}
}

func TestDispatchIntentReceiptMatching(t *testing.T) {
	intent := &DispatchIntent{ReleaseVersion: "5.0.6", ReleaseImage: "image", ChannelGroup: "stable", TemplateReference: testPin}
	receipt := intent.Receipt("object-uid", "cluster-uid")
	require.Equal(t, "object-uid", receipt.ObjectUID)
	require.Equal(t, "cluster-uid", receipt.ClusterUID)

	assert.True(t, intent.MatchesReceipt(receipt, "object-uid", "cluster-uid"))
	assert.False(t, intent.Committed(nil, "object-uid", "cluster-uid"))
	assert.False(t, intent.Committed(&privatev1.IAMDispatchState{}, "object-uid", "cluster-uid"))

	// A committed pending or dispatched receipt authorizes only its own
	// exact target; any field difference is a different target.
	assert.True(t, intent.Committed(&privatev1.IAMDispatchState{Pending: receipt}, "object-uid", "cluster-uid"))
	assert.True(t, intent.Committed(&privatev1.IAMDispatchState{Dispatched: receipt}, "object-uid", "cluster-uid"))

	other := intent.Receipt("object-uid", "cluster-uid")
	other.ReleaseImage = "different-image"
	assert.False(t, intent.MatchesReceipt(other, "object-uid", "cluster-uid"))
	assert.False(t, intent.Committed(&privatev1.IAMDispatchState{Pending: other, Dispatched: other}, "object-uid", "cluster-uid"))

	other = intent.Receipt("object-uid", "cluster-uid")
	other.TemplateReference.UID = "different-uid"
	assert.False(t, intent.MatchesReceipt(other, "object-uid", "cluster-uid"))

	// Receipts bound to a different object or parent identity never match,
	// mirroring the public requirements projection: a recreated object or
	// parent can never inherit a prior commitment.
	other = intent.Receipt("different-object-uid", "cluster-uid")
	assert.False(t, intent.MatchesReceipt(other, "object-uid", "cluster-uid"))
	assert.False(t, intent.Committed(&privatev1.IAMDispatchState{Pending: other, Dispatched: other}, "object-uid", "cluster-uid"))

	other = intent.Receipt("object-uid", "different-cluster-uid")
	assert.False(t, intent.MatchesReceipt(other, "object-uid", "cluster-uid"))
	assert.False(t, intent.Committed(&privatev1.IAMDispatchState{Pending: other, Dispatched: other}, "object-uid", "cluster-uid"))
}

// EvaluateDispatch distinguishes retryable infrastructure failures from
// fail-closed evaluations: only the former returns an error.
func TestEvaluateDispatchInfrastructureFailure(t *testing.T) {
	reader := failingReader{err: assert.AnError}
	_, err := EvaluateDispatch(context.Background(), reader, pinnedCluster(&testPin), "5.0.6", "stable")
	assert.Error(t, err)
}
