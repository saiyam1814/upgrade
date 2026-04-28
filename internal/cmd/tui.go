package cmd

import (
	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/tui"
)

func newTUICmd() *cobra.Command {
	var (
		target      string
		kubeconfig  string
		contextName string
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive visual upgrade dashboard (bubbletea)",
		Long: `tui opens a side-by-side dashboard with the upgrade steps on the
left and per-step details / findings / commands on the right.

Keys:
  ↑ / k       up
  ↓ / j       down
  enter / r   run the highlighted step (read-only)
  q / esc     quit`,
		Example: `  kubectl upgrade tui --target v1.34`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(target, kubeconfig, contextName)
		},
	}
	cmd.Flags().StringVar(&target, "target", "v1.34", "Target Kubernetes version")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&contextName, "context", "", "Kubeconfig context name")
	return cmd
}
