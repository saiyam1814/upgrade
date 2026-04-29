package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/addons"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
)

type addonsOpts struct {
	target      string
	kubeconfig  string
	contextName string
	format      string
	failOn      string
}

func newAddonsCmd() *cobra.Command {
	o := &addonsOpts{}
	cmd := &cobra.Command{
		Use:   "addons",
		Short: "Check installed addons (cert-manager, Istio, ArgoCD, Karpenter, …) against a target K8s version",
		Long: `addons detects installed third-party controllers and validates each one
against the curated compatibility matrix shipped with this binary.

Example:
  upgrade addons --target v1.34`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAddons(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Required.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "high", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	return cmd
}

func runAddons(ctx context.Context, o *addonsOpts) error {
	if o.target == "" {
		return fmt.Errorf("--target is required (e.g. --target v1.34)")
	}
	target, ok := apis.Parse(o.target)
	if !ok {
		return fmt.Errorf("invalid --target %q", o.target)
	}
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	v, _ := client.ServerVersion()
	findings, errs := addons.Analyze(ctx, client.Core, target)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	finding.Sort(findings)
	header := report.Header{
		Tool:          "kubectl-upgrade",
		ToolVersion:   version,
		Source:        "live",
		SourceVersion: v,
		Target:        target.String(),
	}
	if err := report.Render(os.Stdout, header, findings, format); err != nil {
		return err
	}
	emitRecommendation(format, recommend.Context{
		Command:  "addons",
		Target:   target.String(),
		Findings: findings,
	})
	return failOnExit(findings, o.failOn)
}
