package garaccess

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/openshift-online/gecko/controllers/garaccess"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// NewCommand returns the gar-access subcommand.
func NewCommand(rf *setup.RootFlags) *cobra.Command {
	var config garaccess.Config
	cmd := &cobra.Command{
		Use:   "gar-access",
		Short: "Reconcile shared customer GAR Reader grants",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := config.Validate(); err != nil {
				return err
			}
			log, err := rf.NewLogger("gar-access-controller")
			if err != nil {
				return fmt.Errorf("create logger: %w", err)
			}
			mgr, err := rf.NewManager(setup.NewScheme(), log)
			if err != nil {
				return fmt.Errorf("create manager: %w", err)
			}
			// KCC always lives in the regional Kubernetes API, not --orlop-url.
			kccConfig, err := ctrl.GetConfig()
			if err != nil {
				return fmt.Errorf("get regional Kubernetes config: %w", err)
			}
			kcc, err := client.New(kccConfig, client.Options{Scheme: mgr.GetScheme()})
			if err != nil {
				return fmt.Errorf("create Config Connector client: %w", err)
			}
			kccCache, err := cache.New(kccConfig, cache.Options{
				Scheme:            mgr.GetScheme(),
				DefaultNamespaces: map[string]cache.Config{config.IAMNamespace: {}},
			})
			if err != nil {
				return fmt.Errorf("create IAMPolicyMember cache: %w", err)
			}
			if err := mgr.Add(kccCache); err != nil {
				return fmt.Errorf("register IAMPolicyMember cache: %w", err)
			}
			rec, err := garaccess.New(log, mgr.GetAPIReader(), kcc, config)
			if err != nil {
				return fmt.Errorf("create GAR access reconciler: %w", err)
			}
			if err := ctrl.NewControllerManagedBy(mgr).
				Named("gar-access").
				Watches(&privatev1.Cluster{}, handler.EnqueueRequestsFromMapFunc(clusterToProject)).
				WatchesRawSource(source.Kind(kccCache, client.Object(garaccess.NewPolicyMember()), handler.EnqueueRequestsFromMapFunc(rec.PolicyMemberToProject))).
				WithOptions(rf.ControllerOpts()).
				Complete(rec); err != nil {
				return fmt.Errorf("setup controller: %w", err)
			}
			return mgr.Start(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&config.Repository, "gar-repository", "", "Required GAR resource: projects/PROJECT/locations/LOCATION/repositories/REPOSITORY")
	cmd.Flags().StringVar(&config.IAMNamespace, "iam-namespace", garaccess.DefaultIAMNamespace, "Existing regional namespace for shared IAMPolicyMembers")
	return cmd
}

func clusterToProject(_ context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace()}}}
}
