package controlplaneupgrade

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/client/transport"
	"github.com/openshift-online/gecko/controllers/client/transport/mock"
	"github.com/openshift-online/gecko/controllers/hc"
	"github.com/openshift-online/gecko/controllers/util/constants"
	"github.com/openshift-online/gecko/controllers/util/setup"
	versionresolution "github.com/openshift-online/gecko/controllers/versionresolution"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReconcile_CatalogWaitRevalidatesMaintenance(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	cluster := readyCluster("4.23.8", []string{"4.23.9"})
	cluster.Spec.Maintenance = &privatev1.ClusterMaintenanceSpec{Window: &privatev1.ControlPlaneMaintenanceWindow{
		StartTime: metav1.NewTime(start), DurationSeconds: 1200,
		Recurrence: privatev1.ControlPlaneMaintenanceRecurrence{Frequency: "weekly", DaysOfWeek: []string{strings.ToLower(start.Weekday().String())}},
	}}
	r, store := buildReconciler(t, cluster)
	r.now = func() time.Time { return start }
	store.persistUpdates = true
	for range 2 {
		_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	require.Equal(t, "4.23.9", store.cluster.Spec.Release.Version)

	// The catalog rejection waits while the authorizing window is still open.
	meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
		Type: "VersionResolved", Status: metav1.ConditionFalse, Reason: "VersionNotFoundInCatalog",
		ObservedGeneration: store.cluster.Generation})
	_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.Equal(t, "4.23.9", store.cluster.Spec.Release.Version)
	require.Equal(t, "WaitingForVersionCatalog", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).Reason)

	// Once the window closes, the wait must cancel and restore the old release;
	// any later attempt is a fresh approval that passes maintenance again.
	closed := NewReconciler(store, testLogger(t))
	closed.now = func() time.Time { return start.Add(30 * time.Minute) }
	_, err = closed.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.True(t, privatev1.ClusterAutomaticReleaseCanceling(store.cluster))
	require.False(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, conditionDegraded), "closing a maintenance window is a policy wait")
	_, err = closed.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.False(t, privatev1.ClusterUpgradeActive(store.cluster))
	require.Equal(t, "4.23.8", store.cluster.Spec.Release.Version)
	require.False(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, conditionDegraded), "restoring the observed release must preserve the policy wait")
	require.Equal(t, "OutsideMaintenanceWindow", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionDegraded).Reason)

	_, err = closed.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.Equal(t, "OutsideMaintenanceWindow", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).Reason)
	require.Equal(t, "4.23.8", store.cluster.Spec.Release.Version)
}

func TestCatalogRejectionAndRecoverySequence(t *testing.T) {
	scheme := setup.NewScheme()
	cluster := readyCluster("4.23.8", []string{"4.23.9"})
	channel := &privatev1.Channel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Spec: privatev1.ChannelSpec{FleetMinorVersion: "4.23"}}
	initialRelease := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.23.9"},
		Spec: privatev1.VersionSpec{ReleaseImage: "catalog-image", ChannelGroups: []string{"stable"}}}
	initialRelease.SetGroupVersionKind(privatev1.GroupVersion.WithKind("Version"))
	apiClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cluster).
		WithObjects(cluster, channel, initialRelease).Build()
	getCluster := func() *privatev1.Cluster {
		t.Helper()
		current := &privatev1.Cluster{}
		require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(cluster), current))
		return current
	}

	// Select and prepare the automatic target.
	upgrade := NewReconciler(apiClient, testLogger(t))
	for range 2 {
		_, err := upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	require.Equal(t, "4.23.9", getCluster().Spec.Release.Version)

	// The catalog loses the selected target; version resolution rejects it.
	require.NoError(t, apiClient.Delete(t.Context(), initialRelease))
	resolver := versionresolution.NewReconciler(testLogger(t), apiClient)
	_, err := resolver.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(getCluster().Status.Conditions, "VersionResolved").Status)

	// Restart: the upgrade controller waits with its approved target intact.
	upgrade = NewReconciler(apiClient, testLogger(t))
	_, err = upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	waiting := getCluster()
	require.Equal(t, "4.23.9", waiting.Spec.Release.Version, "catalog lag must not cancel or re-select")
	require.True(t, privatev1.ClusterUpgradeActive(waiting))
	require.Equal(t, "WaitingForVersionCatalog", meta.FindStatusCondition(waiting.Status.Conditions, conditionProgressing).Reason)

	// Repair the catalog; a restarted version-resolution controller recovers.
	release := initialRelease.DeepCopy()
	release.ResourceVersion = ""
	require.NoError(t, apiClient.Create(t.Context(), release))
	resolver = versionresolution.NewReconciler(testLogger(t), apiClient)
	_, err = resolver.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	recovered := getCluster()
	require.True(t, meta.IsStatusConditionTrue(recovered.Status.Conditions, "VersionResolved"))
	require.Equal(t, "catalog-image", recovered.Status.VersionResolution.ReleaseImage)

	// Restart: the same rollout proceeds without a new selection.
	upgrade = NewReconciler(apiClient, testLogger(t))
	_, err = upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	proceeding := getCluster()
	require.Equal(t, "4.23.9", proceeding.Spec.Release.Version)
	require.True(t, privatev1.ClusterUpgradeActive(proceeding))
	require.Equal(t, "HostedClusterProgressing", meta.FindStatusCondition(proceeding.Status.Conditions, conditionProgressing).Reason)

	// Once dispatched, later catalog changes must not cancel or replace the
	// rollout, and the pinned image must survive.
	dispatched := getCluster()
	dispatched.Status.HostedClusterResult.DesiredVersion = "4.23.9"
	meta.SetStatusCondition(&dispatched.Status.Conditions, metav1.Condition{
		Type: privatev1.ControlPlaneUpgradeSubmitted, Status: metav1.ConditionTrue, ObservedGeneration: dispatched.Generation})
	require.NoError(t, apiClient.Status().Update(t.Context(), dispatched))
	release.Spec.ChannelGroups = []string{"candidate"}
	require.NoError(t, apiClient.Update(t.Context(), release))
	resolver = versionresolution.NewReconciler(testLogger(t), apiClient)
	_, err = resolver.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	upgrade = NewReconciler(apiClient, testLogger(t))
	_, err = upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	rolled := getCluster()
	require.Equal(t, "4.23.9", rolled.Spec.Release.Version)
	require.True(t, privatev1.ClusterUpgradeActive(rolled))
	require.Equal(t, "catalog-image", rolled.Status.VersionResolution.ReleaseImage)
}

func TestReconcile_CatalogRecoveryAfterWindowCloseDoesNotProceed(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	cluster := readyCluster("4.23.8", []string{"4.23.9"})
	cluster.Spec.Maintenance = &privatev1.ClusterMaintenanceSpec{Window: &privatev1.ControlPlaneMaintenanceWindow{
		StartTime: metav1.NewTime(start), DurationSeconds: 1200,
		Recurrence: privatev1.ControlPlaneMaintenanceRecurrence{Frequency: "weekly", DaysOfWeek: []string{strings.ToLower(start.Weekday().String())}},
	}}
	r, store := buildReconciler(t, cluster)
	r.now = func() time.Time { return start }
	store.persistUpdates = true
	for range 2 {
		_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	require.Equal(t, "4.23.9", store.cluster.Spec.Release.Version)

	meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
		Type: "VersionResolved", Status: metav1.ConditionFalse, Reason: "VersionNotFoundInCatalog",
		ObservedGeneration: store.cluster.Generation})
	_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.Equal(t, "WaitingForVersionCatalog", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).Reason)

	// Version resolution recovers first, after the window has closed. The old
	// approval must not let the rollout proceed without re-authorization.
	recovered := NewReconciler(store, testLogger(t))
	recovered.now = func() time.Time { return start.Add(30 * time.Minute) }
	meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
		Type: "VersionResolved", Status: metav1.ConditionTrue, Reason: "VersionResolved",
		ObservedGeneration: store.cluster.Generation})
	_, err = recovered.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.True(t, privatev1.ClusterAutomaticReleaseCanceling(store.cluster),
		"catalog recovery after the window closed must cancel, not proceed")
	_, err = recovered.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.False(t, privatev1.ClusterUpgradeActive(store.cluster))
	require.Equal(t, "4.23.8", store.cluster.Spec.Release.Version)
}

func TestReconcile_CustomerCatalogRejectionStaysTerminal(t *testing.T) {
	cluster := readyCluster("4.23.8", []string{"4.23.9"})
	cluster.Spec.Release.Version = "4.23.9"
	r, store := buildReconciler(t, cluster)
	store.persistUpdates = true
	_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.True(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted))

	// Version resolution terminally rejects the customer target.
	meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
		Type: "VersionResolved", Status: metav1.ConditionFalse, Reason: "VersionNotFoundInCatalog",
		ObservedGeneration: store.cluster.Generation})
	_, err = r.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.False(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted))
	require.Equal(t, "PreparationFailed", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionDegraded).Reason)

	// Repeated reconciles must not alternate approval with the terminal
	// version-resolution rejection.
	for range 3 {
		r = NewReconciler(store, testLogger(t))
		_, err = r.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
		require.False(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, privatev1.ControlPlaneUpgradeAccepted),
			"customer catalog rejection must stay terminal until the spec changes")
		require.Equal(t, "PreparationFailed", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionDegraded).Reason)
	}
}

func TestCatalogRecovery_InstallerFirstAfterWindowClosesDoesNotDispatch(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	cluster := readyCluster("4.23.8", []string{"4.23.9"})
	cluster.UID = "550e8400-e29b-41d4-a716-446655440000"
	cluster.Finalizers = []string{constants.FinalizerCluster}
	cluster.Spec.Platform = privatev1.ClusterPlatformSpec{Type: "GCP", GCP: &privatev1.GCPClusterPlatform{ProjectID: "project", Region: "us-central1", Network: "network", Subnet: "subnet"}}
	cluster.Status.PlacementResult = &privatev1.PlacementResult{ManagementClusterName: "management", BaseDomain: "example.com"}
	cluster.Spec.Maintenance = &privatev1.ClusterMaintenanceSpec{Window: &privatev1.ControlPlaneMaintenanceWindow{
		StartTime: metav1.NewTime(start), DurationSeconds: 1200,
		Recurrence: privatev1.ControlPlaneMaintenanceRecurrence{Frequency: "weekly", DaysOfWeek: []string{strings.ToLower(start.Weekday().String())}},
	}}
	release := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.23.9"}, Spec: privatev1.VersionSpec{ReleaseImage: "new-target-image", ChannelGroups: []string{"stable"}}}
	apiClient := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(cluster).WithObjects(cluster, release).Build()
	upgrade := NewReconciler(apiClient, testLogger(t))
	upgrade.now = func() time.Time { return start }
	resolver := versionresolution.NewReconciler(testLogger(t), apiClient)
	for range 2 {
		_, err := upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	require.NoError(t, apiClient.Delete(t.Context(), release))
	_, err := resolver.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	_, err = upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster))
	require.Equal(t, "WaitingForVersionCatalog", meta.FindStatusCondition(cluster.Status.Conditions, conditionProgressing).Reason)
	permitted, _, _, _ := maintenancePermitted(cluster, time.Now().UTC())
	require.False(t, permitted, "fixture must have a closed window at actual dispatch time")
	release.ResourceVersion = ""
	require.NoError(t, apiClient.Create(t.Context(), release))
	_, err = resolver.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	tr := mock.New()
	hcKey := transport.ResourceKey(constants.HyperShiftGroup, constants.HyperShiftVersion, "hostedclusters",
		"clusters-"+string(cluster.UID), privatev1.DefaultSafeName(cluster.Name, cluster.UID))
	groupKey, err := transport.ClusterGroupKey(cluster.Namespace, cluster.Name)
	require.NoError(t, err)
	tr.StatusOverrides["management/"+groupKey] = &transport.Status{
		ResourceStatuses: map[string]map[string]string{hcKey: {
			"version": "4.23.8", "desiredVersion": "4.23.8", "availableCondition": "True",
			"versionConditions": `[{"type":"Degraded","status":"False"}]`, "availableVersions": `["4.23.9"]`,
		}},
	}
	installer := hc.New(tr, testLogger(t), apiClient, nil, "", "")
	for range 2 {
		_, err = installer.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	upgrade.now = func() time.Time { return start.Add(time.Hour) }
	_, err = upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster))
	require.Zero(t, len(tr.ApplyCalls),
		"installer-first ordering must not dispatch after the window closes; submitted=%v", meta.FindStatusCondition(cluster.Status.Conditions, privatev1.ControlPlaneUpgradeSubmitted))
}

func TestCatalogRecovery_PendingResolutionDoesNotCancelHealthyUpgrade(t *testing.T) {
	cluster := readyCluster("4.23.8", []string{"4.23.9"})
	cluster.UID = "550e8400-e29b-41d4-a716-446655440000"
	cluster.Finalizers = []string{constants.FinalizerCluster}
	cluster.Spec.Platform = privatev1.ClusterPlatformSpec{Type: "GCP", GCP: &privatev1.GCPClusterPlatform{ProjectID: "project", Region: "us-central1", Network: "network", Subnet: "subnet"}}
	cluster.Status.PlacementResult = &privatev1.PlacementResult{ManagementClusterName: "management", BaseDomain: "example.com"}
	cluster.Status.VersionResolution = &privatev1.VersionResolutionResult{ReleaseVersion: "4.23.8", ReleaseImage: "old-image", ChannelGroup: "stable", CincinnatiChannel: "stable-4.23"}
	release := &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.23.9"}, Spec: privatev1.VersionSpec{ReleaseImage: "new-image", ChannelGroups: []string{"stable"}}}
	apiClient := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithStatusSubresource(cluster).WithObjects(cluster, release).Build()
	upgrade := NewReconciler(apiClient, testLogger(t))
	for range 2 {
		_, err := upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	tr := mock.New()
	hcKey := transport.ResourceKey(constants.HyperShiftGroup, constants.HyperShiftVersion, "hostedclusters",
		"clusters-"+string(cluster.UID), privatev1.DefaultSafeName(cluster.Name, cluster.UID))
	groupKey, err := transport.ClusterGroupKey(cluster.Namespace, cluster.Name)
	require.NoError(t, err)
	tr.StatusOverrides["management/"+groupKey] = &transport.Status{
		ResourceStatuses: map[string]map[string]string{hcKey: {
			"version": "4.23.8", "desiredVersion": "4.23.8", "availableCondition": "True",
			"versionConditions": `[{"type":"Degraded","status":"False"}]`, "availableVersions": `["4.23.9"]`,
		}},
	}
	installer := hc.New(tr, testLogger(t), apiClient, nil, "", "")
	_, err = installer.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.Empty(t, tr.ApplyCalls)
	require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster))
	require.Equal(t, "VersionMismatch", meta.FindStatusCondition(cluster.Status.Conditions, "ResourcesApplied").Reason)
	require.True(t, meta.IsStatusConditionTrue(cluster.Status.Conditions, "HostedClusterAvailable"),
		"a preparation wait must preserve the observed HostedCluster availability")
	_, err = upgrade.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.NoError(t, apiClient.Get(t.Context(), client.ObjectKeyFromObject(cluster), cluster))
	require.False(t, privatev1.ClusterAutomaticReleaseCanceling(cluster),
		"waiting for the selected version to resolve is expected preparation, not observed cluster health degradation")

	resolver := versionresolution.NewReconciler(testLogger(t), apiClient)
	_, err = resolver.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	for range 2 {
		_, err = installer.Reconcile(t.Context(), clusterReq(cluster.Name))
		require.NoError(t, err)
	}
	require.Len(t, tr.ApplyCalls, 1, "the healthy automatic upgrade must proceed once resolution is ready")
	hasTarget := false
	for _, manifest := range tr.ApplyCalls[0].Manifests {
		hasTarget = hasTarget || strings.Contains(string(manifest), "new-image")
	}
	require.True(t, hasTarget, "the approved target image must be dispatched")
}

func TestReconcile_CatalogWaitExpiresAcrossRestarts(t *testing.T) {
	for _, tc := range []struct {
		name           string
		elapsed        time.Duration
		submitted      metav1.ConditionStatus
		feedbackActive bool
		recovered      bool
		wantRequeue    time.Duration
		expired        bool
	}{
		{name: "before deadline", elapsed: 19 * time.Minute, wantRequeue: 15 * time.Second},
		{name: "next retry at deadline", elapsed: 20*time.Minute - time.Second, wantRequeue: time.Second},
		{name: "recovery before deadline", elapsed: 19 * time.Minute, recovered: true},
		{name: "late recovery cannot bypass expiry", elapsed: 20 * time.Minute, recovered: true, expired: true},
		{name: "at deadline", elapsed: 20 * time.Minute, expired: true},
		{name: "after deadline", elapsed: 25 * time.Minute, expired: true},
		{name: "uncertain dispatch is preserved", elapsed: 25 * time.Minute, submitted: metav1.ConditionUnknown},
		{name: "acknowledged dispatch is preserved", elapsed: 25 * time.Minute, submitted: metav1.ConditionTrue},
		{name: "feedback proves dispatch", elapsed: 25 * time.Minute, feedbackActive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster := readyCluster("4.23.8", []string{"4.23.9"})
			require.NoError(t, cluster.Default(t.Context()))
			r, store := buildReconciler(t, cluster)
			store.persistUpdates = true
			for range 2 {
				_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
				require.NoError(t, err)
			}
			meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
				Type: "VersionResolved", Status: metav1.ConditionFalse, Reason: "VersionNotFoundInCatalog", ObservedGeneration: store.cluster.Generation,
			})
			_, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
			require.NoError(t, err)
			waitStarted := meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).LastTransitionTime
			for _, elapsed := range []time.Duration{5 * time.Minute, 10 * time.Minute, 15 * time.Minute} {
				if elapsed == 10*time.Minute {
					store.cluster.Generation++
				}
				r = NewReconciler(store, testLogger(t))
				r.now = func() time.Time { return waitStarted.Add(elapsed) }
				_, err = r.Reconcile(t.Context(), clusterReq(cluster.Name))
				require.NoError(t, err)
				require.Equal(t, waitStarted, meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).LastTransitionTime,
					"retries and restarts must preserve the original wait deadline")
				require.Equal(t, "WaitingForVersionCatalog", meta.FindStatusCondition(store.cluster.Status.Conditions, conditionProgressing).Reason)
			}
			if tc.recovered {
				meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
					Type: "VersionResolved", Status: metav1.ConditionTrue, Reason: "VersionResolved", ObservedGeneration: store.cluster.Generation,
				})
			}
			if tc.submitted != "" {
				meta.SetStatusCondition(&store.cluster.Status.Conditions, metav1.Condition{
					Type: privatev1.ControlPlaneUpgradeSubmitted, Status: tc.submitted, ObservedGeneration: store.cluster.Generation,
				})
			}
			if tc.feedbackActive {
				store.cluster.Status.HostedClusterResult.DesiredVersion = "4.23.9"
			}
			r = NewReconciler(store, testLogger(t))
			r.now = func() time.Time { return waitStarted.Add(tc.elapsed) }
			result, err := r.Reconcile(t.Context(), clusterReq(cluster.Name))
			require.NoError(t, err)
			if tc.wantRequeue != 0 {
				require.Equal(t, tc.wantRequeue, result.RequeueAfter)
			}
			if !tc.expired {
				require.False(t, privatev1.ClusterAutomaticReleaseCanceling(store.cluster))
				require.Equal(t, "4.23.9", store.cluster.Spec.Release.Version)
				return
			}
			require.True(t, privatev1.ClusterAutomaticReleaseCanceling(store.cluster), "a catalog wait must stop after twenty minutes")
			require.Equal(t, "VersionCatalogWaitExpired", store.cluster.Status.ControlPlaneUpgrade.FailureReason)
			require.False(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, conditionDegraded))
			r = NewReconciler(store, testLogger(t))
			_, err = r.Reconcile(t.Context(), clusterReq(cluster.Name))
			require.NoError(t, err)
			require.False(t, privatev1.ClusterUpgradeActive(store.cluster))
			require.Equal(t, "4.23.8", store.cluster.Spec.Release.Version)
			require.False(t, meta.IsStatusConditionTrue(store.cluster.Status.Conditions, conditionDegraded))
			changed := store.cluster.DeepCopy()
			changed.Spec.Release.Version = "4.23.9"
			require.NoError(t, changed.ValidateUpdate(t.Context(), store.cluster), "expiry must release the admission lock")
		})
	}
}
