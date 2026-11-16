// Package versioncatalog maps Version catalog changes to Clusters that resolve them.
package versioncatalog

import (
	"context"

	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// ClusterInstallationVersionField is the cache index for the release version
	// that version resolution currently resolves for a Cluster.
	ClusterInstallationVersionField = "clusterInstallationVersion"
)

// ClusterInstallationVersion indexes a Cluster by the release version that
// version resolution resolves for it. It follows dispatch evidence so catalog
// events also wake Clusters with an in-flight rollout.
func ClusterInstallationVersion(obj client.Object) []string {
	cluster, ok := obj.(*privatev1.Cluster)
	if !ok {
		return nil
	}
	if version := privatev1.ClusterInstallationVersion(cluster); version != "" {
		return []string{version}
	}
	return nil
}

// ToClusters maps a Version catalog change to Clusters whose installation
// version matches the changed release.
func ToClusters(apiClient client.Client, log logger.Logger) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		changed, ok := obj.(*privatev1.Version)
		if !ok {
			return nil
		}

		var clusters privatev1.ClusterList
		if err := apiClient.List(ctx, &clusters, client.MatchingFields{ClusterInstallationVersionField: changed.Name}); err != nil {
			// MapFunc cannot return an error. Version-resolution requeues
			// Clusters, so a missed catalog event converges on its next reconcile.
			log.Errorf(ctx, "list Clusters for Version %s: %v", changed.Name, err)
			return nil
		}

		requests := make([]reconcile.Request, 0, len(clusters.Items))
		for i := range clusters.Items {
			cluster := &clusters.Items[i]
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{
				Namespace: cluster.Namespace,
				Name:      cluster.Name,
			}})
		}
		return requests
	}
}
