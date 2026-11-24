package garaccess

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestClusterEventsQueueTheCustomerProject(t *testing.T) {
	for _, name := range []string{"existing-cluster", "new-cluster"} {
		cluster := &privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "customer-project", Name: name}}
		require.Equal(t, []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: "customer-project"}}},
			clusterToProject(context.Background(), cluster))
	}
}

func TestClusterEventPredicate(t *testing.T) {
	filter := clusterEventPredicate()
	cluster := &privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{
		Namespace: "customer-project", Name: "cluster", Generation: 1, ResourceVersion: "1",
	}}
	for _, initialList := range []bool{false, true} {
		require.True(t, filter.Create(event.CreateEvent{Object: cluster, IsInInitialList: initialList}))
	}
	require.True(t, filter.Generic(event.GenericEvent{Object: cluster}))

	for _, test := range []struct {
		name   string
		change func(*privatev1.Cluster)
		want   bool
	}{
		{"generation-changed", func(c *privatev1.Cluster) { c.Generation++ }, true},
		{"status-only", func(c *privatev1.Cluster) {
			c.Status.Conditions = []metav1.Condition{{Type: "HostedClusterAvailable", Status: metav1.ConditionTrue}}
		}, false},
		{"annotation-only", func(c *privatev1.Cluster) {
			c.Annotations = map[string]string{"operator-note": "updated"}
		}, false},
		{"deletion-started", func(c *privatev1.Cluster) {
			now := metav1.Now()
			c.DeletionTimestamp = &now
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			updated := cluster.DeepCopy()
			updated.ResourceVersion = "2"
			test.change(updated)
			require.Equal(t, test.want, filter.Update(event.UpdateEvent{ObjectOld: cluster, ObjectNew: updated}))
		})
	}

	deleted := cluster.DeepCopy()
	now := metav1.Now()
	deleted.DeletionTimestamp = &now
	for _, stateUnknown := range []bool{false, true} {
		require.True(t, filter.Delete(event.DeleteEvent{Object: deleted, DeleteStateUnknown: stateUnknown}),
			"final deletion must queue grant cleanup, including deletes recovered after a missed event")
	}
}

func TestNewCommandRequiresExplicitRepository(t *testing.T) {
	cmd := NewCommand(nil)
	repository, err := cmd.Flags().GetString("gar-repository")
	require.NoError(t, err)
	require.Empty(t, repository, "no environment-specific repository fallback")
	namespace, err := cmd.Flags().GetString("iam-namespace")
	require.NoError(t, err)
	require.Equal(t, "gecko-system", namespace)

	for _, test := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing repository", nil, "gar-repository must be set"},
		{"invalid repository", []string{"--gar-repository=invalid"}, "gar-repository must be projects/"},
		{"invalid namespace", []string{
			"--gar-repository=projects/platform-project/locations/us-central1/repositories/gcp-hcp-quay",
			"--iam-namespace=INVALID",
		}, "iam-namespace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Invalid configuration must return before dereferencing RootFlags.
			command := NewCommand(nil)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs(test.args)
			require.ErrorContains(t, command.Execute(), test.wantErr)
		})
	}
}
