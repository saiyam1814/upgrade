package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/addons"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/pdb"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/rules/featuregates"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/vcluster"
)

type reportOpts struct {
	target      string
	kubeconfig  string
	contextName string
	format      string
	outFile     string
	skipVC      bool
}

func newReportCmd() *cobra.Command {
	o := &reportOpts{}
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Aggregate scan + simulate + pdb + addons + vcluster into one report",
		Long: `report runs every check end-to-end against the live cluster and
produces a single combined output suitable for sharing in PRs, issues,
or upgrade-readiness reviews.

Example:
  upgrade report --target v1.34 --format md -o upgrade-readiness.md`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReport(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version. Required.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().StringVarP(&o.outFile, "output", "o", "", "Write report to file instead of stdout")
	cmd.Flags().BoolVar(&o.skipVC, "skip-vcluster", false, "Skip the vCluster Tenant-Cluster sub-check")
	return cmd
}

func runReport(ctx context.Context, o *reportOpts) error {
	if o.target == "" {
		return fmt.Errorf("--target is required")
	}
	target, ok := apis.Parse(o.target)
	if !ok {
		return fmt.Errorf("invalid --target %q", o.target)
	}
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}

	engine, err := apis.Load()
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	hostVersion, _ := client.ServerVersion()

	var all []finding.Finding

	// scan
	objs, walkErrs := client.Walk(ctx, liveFilter(engine))
	helmObjs, helmErrs := client.HelmReleases(ctx)
	objs = append(objs, helmObjs...)
	all = append(all, scanObjects(objs, engine, target)...)

	// addons
	addonFindings, addonErrs := addons.Analyze(ctx, client.Core, target)
	all = append(all, addonFindings...)

	// pdb
	pdbFindings, pdbErrs := pdb.Analyze(ctx, client.Core)
	all = append(all, pdbFindings...)

	// simulate forward (feature gates / defaults)
	if from, ok := apis.Parse(hostVersion); ok {
		all = append(all, featuregates.Simulate(from, target)...)
	}

	// vcluster
	if !o.skipVC {
		vcFindings, vcErrs := vcluster.Analyze(ctx, client.Core, vcluster.Options{
			HostVersion: hostVersion,
		})
		all = append(all, vcFindings...)
		for _, e := range vcErrs {
			fmt.Fprintf(os.Stderr, "warning: %v\n", e)
		}
	}

	for _, e := range append(walkErrs, append(helmErrs, append(addonErrs, pdbErrs...)...)...) {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	w := os.Stdout
	if o.outFile != "" {
		f, err := os.Create(o.outFile)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	header := report.Header{
		Tool:          "kubectl-upgrade",
		ToolVersion:   version,
		Source:        "live (combined)",
		SourceVersion: hostVersion,
		Target:        target.String(),
		RulesData:     apis.DataPath,
	}
	return report.Render(w, header, all, format)
}
