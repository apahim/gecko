package nodepool

import (
	"fmt"

	"github.com/spf13/cobra"

	fstransport "github.com/openshift-online/gecko/controllers/client/transport/firestore"
	"github.com/openshift-online/gecko/controllers/garaccess"
	nodepool "github.com/openshift-online/gecko/controllers/nodepool"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewCommand returns the nodepool subcommand.
func NewCommand(rf *setup.RootFlags) *cobra.Command {
	var garConfig garaccess.Config
	cmd := &cobra.Command{
		Use:   "nodepool",
		Short: "Run the nodepool controller",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := garConfig.Validate(); err != nil {
				return err
			}

			log, err := rf.NewLogger("nodepool-controller")
			if err != nil {
				return fmt.Errorf("create logger: %w", err)
			}

			t := fstransport.New(log)
			defer t.Close()

			scheme := setup.NewScheme()
			mgr, err := rf.NewManager(scheme, log)
			if err != nil {
				return fmt.Errorf("create manager: %w", err)
			}

			rec := nodepool.New(t, log, mgr.GetClient())
			kccConfig, err := ctrl.GetConfig()
			if err != nil {
				return fmt.Errorf("get regional Kubernetes config: %w", err)
			}
			kcc, err := client.New(kccConfig, client.Options{Scheme: scheme})
			if err != nil {
				return fmt.Errorf("create Config Connector reader: %w", err)
			}
			rec.GARAccess, err = garaccess.NewGate(ctx, kcc, garConfig)
			if err != nil {
				return fmt.Errorf("create GAR access gate: %w", err)
			}

			if err := ctrl.NewControllerManagedBy(mgr).
				For(&privatev1.NodePool{}).
				WithOptions(rf.ControllerOpts()).
				Complete(rec); err != nil {
				return fmt.Errorf("setup controller: %w", err)
			}

			return mgr.Start(ctx)
		},
	}

	cmd.Flags().StringVar(&garConfig.Repository, "gar-repository", garaccess.DefaultRepository, "GAR resource: projects/PROJECT/locations/LOCATION/repositories/REPOSITORY")
	cmd.Flags().StringVar(&garConfig.IAMNamespace, "iam-namespace", garaccess.DefaultIAMNamespace, "Existing regional namespace containing shared GAR IAMPolicyMembers")
	return cmd
}
