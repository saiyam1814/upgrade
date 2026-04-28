package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/rules/featuregates"
)

type simulateOpts struct {
	from   string
	to     string
	format string
}

func newSimulateCmd() *cobra.Command {
	o := &simulateOpts{}
	cmd := &cobra.Command{
		Use:   "simulate",
		Short: "Simulate the forward impact of upgrading from --from to --to (feature gates, defaults, kubelet flags)",
		Long: `simulate is the "what changes if I bump K8s" report. It goes beyond
removed APIs to surface:

  - Feature gates that graduate Beta → GA (default flips on)
  - Default value changes (e.g. PSA enforcement, container runtime defaults)
  - Kubelet/kube-proxy/kube-scheduler flag removals
  - Kernel / cgroup / runtime requirements

Use this in pair with 'scan' — scan finds today's deprecated objects;
simulate predicts behavioral surprises.

Example:
  upgrade simulate --from v1.31 --to v1.34`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSimulate(o)
		},
	}
	cmd.Flags().StringVar(&o.from, "from", "", "Current Kubernetes version. Required.")
	cmd.Flags().StringVar(&o.to, "to", "", "Target Kubernetes version. Required.")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	return cmd
}

func runSimulate(o *simulateOpts) error {
	if o.from == "" || o.to == "" {
		return fmt.Errorf("--from and --to are required")
	}
	from, ok := apis.Parse(o.from)
	if !ok {
		return fmt.Errorf("invalid --from %q", o.from)
	}
	to, ok := apis.Parse(o.to)
	if !ok {
		return fmt.Errorf("invalid --to %q", o.to)
	}
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}

	findings := featuregates.Simulate(from, to)
	finding.Sort(findings)
	header := report.Header{
		Tool:        "kubectl-upgrade",
		ToolVersion: version,
		Source:      "simulate",
		Target:      to.String(),
	}
	return report.Render(os.Stdout, header, findings, format)
}
