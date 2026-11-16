package versioncatalog

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openshift-online/gecko/controllers/util/logger"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestClusterInstallationVersionIndex(t *testing.T) {
	tests := []struct {
		name     string
		object   client.Object
		expected []string
	}{
		{name: "concrete spec version", object: &privatev1.Cluster{
			Spec: privatev1.ClusterSpec{Release: privatev1.ClusterReleaseSpec{Version: "4.22.14"}}}, expected: []string{"4.22.14"}},
		{name: "unset version", object: &privatev1.Cluster{}},
		{name: "other resource", object: &privatev1.Version{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, ClusterInstallationVersion(tt.object))
		})
	}
}

func TestToClustersUsesInstallationVersionIndex(t *testing.T) {
	apiClient := fake.NewClientBuilder().WithScheme(setup.NewScheme()).WithObjects(
		&privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "project-a", Name: "matching"},
			Spec: privatev1.ClusterSpec{Release: privatev1.ClusterReleaseSpec{Version: "4.22.14"}}},
		&privatev1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "project-b", Name: "other"},
			Spec: privatev1.ClusterSpec{Release: privatev1.ClusterReleaseSpec{Version: "4.22.15"}}},
	).WithIndex(&privatev1.Cluster{}, ClusterInstallationVersionField, ClusterInstallationVersion).Build()
	log, err := logger.NewLogger(logger.Config{Level: "error", Format: logger.FormatText, Writer: &bytes.Buffer{}, Component: "test"})
	require.NoError(t, err)

	mapVersion := ToClusters(apiClient, log)
	require.Equal(t, []reconcile.Request{
		{NamespacedName: client.ObjectKey{Namespace: "project-a", Name: "matching"}},
	}, mapVersion(context.Background(), &privatev1.Version{ObjectMeta: metav1.ObjectMeta{Name: "4.22.14"}}))
	require.Empty(t, mapVersion(context.Background(), &privatev1.Cluster{}))
}
