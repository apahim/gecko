package garaccess

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openshift-online/gecko/controllers/util/logger"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const retryPending = 5 * time.Second

// Reconciler maintains one shared grant per customer account and repository.
type Reconciler struct {
	reader   client.Reader
	kcc      client.Client
	identity *identityLookup
	config   Config
	log      logger.Logger
}

// New requires a direct Gecko API reader and a direct regional KCC client.
func New(ctx context.Context, log logger.Logger, reader client.Reader, kcc client.Client, config Config) (*Reconciler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	identity, err := newIdentityLookup(ctx)
	if err != nil {
		return nil, err
	}
	return &Reconciler{reader: reader, kcc: kcc, identity: identity, config: config, log: log}, nil
}

// Reconcile uses the logical customer-project namespace as its queue key.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	project := req.Namespace
	if project == "" {
		return reconcile.Result{}, fmt.Errorf("customer project namespace is empty")
	}
	consumers, err := r.consumers(ctx, project)
	if err != nil {
		return reconcile.Result{}, err
	}
	r.log.Infof(ctx, "gar-access: customer project %s has %d NodePools", project, len(consumers))
	if len(consumers) == 0 {
		return r.removeGrants(ctx, project)
	}
	member, err := r.identity.customerMember(ctx, project)
	if err != nil {
		return reconcile.Result{}, err
	}
	return r.ensureGrant(ctx, project, member, strings.Join(consumers, "\n"))
}

func (r *Reconciler) consumers(ctx context.Context, project string) ([]string, error) {
	var pools privatev1.NodePoolList
	if err := r.reader.List(ctx, &pools, client.InNamespace(project)); err != nil {
		return nil, fmt.Errorf("list NodePools for customer project %s: %w", project, err)
	}
	consumers := make([]string, 0, len(pools.Items))
	for _, pool := range pools.Items {
		// No replica/deletion filtering or parent lookup: every extant pool counts,
		// even if its parent is unavailable while cleanup is in progress.
		consumers = append(consumers, pool.Spec.ClusterID+"/"+pool.Name)
	}
	sort.Strings(consumers)
	return consumers, nil
}

// PolicyMemberToProject recovers reconciliation on KCC updates and restarts.
func (r *Reconciler) PolicyMemberToProject(_ context.Context, obj client.Object) []reconcile.Request {
	policy, ok := obj.(*unstructured.Unstructured)
	if !ok || policy.GetNamespace() != r.config.IAMNamespace || policy.GetLabels()[managedLabel] != managedBy {
		return nil
	}
	repository, _, _ := unstructured.NestedString(policy.Object, "spec", "resourceRef", "external")
	project := policy.GetLabels()[projectLabel]
	if repository != r.config.Repository || project == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: project}}}
}
