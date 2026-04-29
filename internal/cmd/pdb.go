package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/pdb"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/sources/live"
)

type pdbOpts struct {
	kubeconfig  string
	contextName string
	format      string
	failOn      string
}

func newPDBCmd() *cobra.Command {
	o := &pdbOpts{}
	cmd := &cobra.Command{
		Use:   "pdb",
		Short: "Detect PodDisruptionBudgets that would deadlock a node drain",
		Long: `pdb walks every PodDisruptionBudget in the cluster and flags any
where ALLOWED DISRUPTIONS == 0 — the canonical "stuck upgrade" pattern.
A drain hitting one of these will hang indefinitely on the affected
node, which is the most common cause of stalled managed-K8s upgrades.

Example:
  upgrade pdb
  upgrade pdb --format md > pdb-report.md`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPDB(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "high", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	return cmd
}

func runPDB(ctx context.Context, o *pdbOpts) error {
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	v, _ := client.ServerVersion()
	findings, errs := pdb.Analyze(ctx, client.Core)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	finding.Sort(findings)
	header := report.Header{
		Tool:          "kubectl-upgrade",
		ToolVersion:   version,
		Source:        "live",
		SourceVersion: v,
	}
	if err := report.Render(os.Stdout, header, findings, format); err != nil {
		return err
	}
	emitRecommendation(format, recommend.Context{Command: "pdb", Findings: findings})
	return failOnExit(findings, o.failOn)
}
