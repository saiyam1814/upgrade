package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/volumes"
)

type volumesOpts struct {
	target      string
	kubeconfig  string
	contextName string
	format      string
	failOn      string
}

func newVolumesCmd() *cobra.Command {
	o := &volumesOpts{}
	cmd := &cobra.Command{
		Use:   "volumes",
		Short: "PV / PVC / CSI / StorageClass safety check before an upgrade",
		Long: `volumes inspects every PersistentVolumeClaim, StorageClass, and CSI
driver in the cluster and surfaces the data-loss + outage surface
that no managed provider's pre-flight catches:

  - In-tree → CSI migration (1.27 GA, in-tree removed in 1.31+)
    Detects StorageClasses on in-tree provisioners whose CSI driver
    is NOT installed. After upgrade, volume operations break.

  - Pending PVCs that will block pod reschedule during drain
  - Deployments with PVCs (wrong primitive — outage risk on rollout)
  - StatefulSets with replicas=1 (outage window during node drain)
  - ReadWriteMany volumes on cloud classes mid-removal
  - Released PVs that should be reclaimed
  - VolumeSnapshotClass presence (needed for safe pre-upgrade snapshots)

This is read-only. It only reports.`,
		Example: `  kubectl upgrade volumes --target v1.34
  kubectl upgrade volumes --target v1.31 --format md > volumes.md
  kubectl upgrade volumes --target v1.34 --fail-on blocker  # CI gate`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVolumes(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Required.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "blocker", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	return cmd
}

func runVolumes(ctx context.Context, o *volumesOpts) error {
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
	findings, errs := volumes.Analyze(ctx, client.Core, target)
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
		Command:  "volumes",
		Target:   target.String(),
		Findings: findings,
	})
	return failOnExit(findings, o.failOn)
}
