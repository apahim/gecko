package nodepoolvrresolution

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/openshift-online/gecko/controllers/nodepoolvrresolution"
	"github.com/openshift-online/gecko/controllers/util/setup"
	privatev1 "github.com/openshift-online/gecko/platform-api/api/private/v1"

	ctrl "sigs.k8s.io/controller-runtime"
)

// NewCommand returns the nodepool-vr subcommand.
func NewCommand(rf *setup.RootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodepool-vr",
		Short: "Run the nodepool version-resolution controller",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			log, err := rf.NewLogger("nodepool-vr-controller")
			if err != nil {
				return fmt.Errorf("create logger: %w", err)
			}

			scheme := setup.NewScheme()
			mgr, err := rf.NewManager(scheme, log)
			if err != nil {
				return fmt.Errorf("create manager: %w", err)
			}

			rec := nodepoolvrresolution.NewReconciler(log, mgr.GetClient()).WithAPIReader(mgr.GetAPIReader())

			if err := ctrl.NewControllerManagedBy(mgr).
				For(&privatev1.NodePool{}).
				WithOptions(rf.ControllerOpts()).
				Complete(rec); err != nil {
				return fmt.Errorf("setup controller: %w", err)
			}

			return mgr.Start(ctx)
		},
	}

	return cmd
}
