package controlplaneupgrade

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	_, err = closed.Reconcile(t.Context(), clusterReq(cluster.Name))
	require.NoError(t, err)
	require.False(t, privatev1.ClusterUpgradeActive(store.cluster))
	require.Equal(t, "4.23.8", store.cluster.Spec.Release.Version)

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
