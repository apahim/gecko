package garaccess

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const retryPending = 5 * time.Second

// Reconciler maintains one shared grant per customer project and repository.
type Reconciler struct {
	reader client.Reader
	kcc    client.Client
	config Config
	log    logger.Logger
}

// New requires a direct Gecko API reader and a direct regional KCC client.
func New(log logger.Logger, reader client.Reader, kcc client.Client, config Config) (*Reconciler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Reconciler{reader: reader, kcc: kcc, config: config, log: log}, nil
}

// Reconcile uses the logical customer-project namespace as its queue key.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	project := req.Namespace
	if project == "" {
		return reconcile.Result{}, fmt.Errorf("customer project namespace is empty")
	}
	consumerCount, member, err := r.consumers(ctx, project)
	if err != nil {
		return reconcile.Result{}, err
	}
	r.log.Infof(ctx, "gar-access: customer project %s has %d Clusters", project, consumerCount)
	if consumerCount == 0 {
		return r.removeGrants(ctx, project)
	}
	if member == "" {
		return reconcile.Result{}, nil
	}
	return r.ensureGrant(ctx, project, member)
}

func (r *Reconciler) consumers(ctx context.Context, project string) (int, string, error) {
	var clusters privatev1.ClusterList
	if err := r.reader.List(ctx, &clusters, client.InNamespace(project)); err != nil {
		return 0, "", fmt.Errorf("list Clusters for customer project %s: %w", project, err)
	}
	member := ""
	// Every extant Cluster counts, including deleting Clusters and those
	// without NodePools. The Cluster finalizer protects normal worker cleanup.
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		if cluster.Namespace != project {
			return 0, "", fmt.Errorf("cluster %s must belong to customer project %s", cluster.Name, project)
		}
		clusterMember, err := customerPrincipalSet(cluster)
		if err != nil {
			r.log.Warnf(ctx, "gar-access: skipping invalid identity for Cluster %s/%s; still counted for retention",
				cluster.Namespace, cluster.Name)
			continue
		}
		if member != "" && member != clusterMember {
			return 0, "", fmt.Errorf("clusters in customer project %s disagree on the project number", project)
		}
		member = clusterMember
	}
	return len(clusters.Items), member, nil
}

// PolicyMemberToProject recovers reconciliation on KCC updates and restarts.
func (r *Reconciler) PolicyMemberToProject(ctx context.Context, obj client.Object) []reconcile.Request {
	policy, ok := obj.(*unstructured.Unstructured)
	if !ok || policy.GetNamespace() != r.config.IAMNamespace || policy.GetLabels()[managedLabel] != managedBy {
		return nil
	}
	repository, _, err := unstructured.NestedString(policy.Object, "spec", "resourceRef", "external")
	if err != nil {
		r.log.Errorf(ctx, "gar-access: read spec.resourceRef.external of IAMPolicyMember %s/%s: %v", policy.GetNamespace(), policy.GetName(), err)
		return nil
	}
	project := policy.GetLabels()[projectLabel]
	if repository != r.config.Repository || project == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: project}}}
}
