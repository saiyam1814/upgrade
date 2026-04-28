package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/vcluster"
)

type vclusterOpts struct {
	kubeconfig  string
	contextName string
	target      string // target vCluster version, e.g. "v0.34.0"
	namespace   string // limit to a single namespace
	releaseName string // limit to a single release
	format      string
	explain     bool
	failOn      string
}

func newVClusterCmd() *cobra.Command {
	o := &vclusterOpts{}
	cmd := &cobra.Command{
		Use:   "vcluster",
		Short: "vCluster Tenant Cluster upgrade pre-flight (distro, etcd, topology, version path)",
		Long: `vcluster discovers vCluster Tenant Clusters in the Control Plane Cluster
and runs the loft.sh-recommended pre-upgrade decision tree:

  - Distro removal gates (k3s removed v0.33; k0s removed v0.26; eks v0.20)
  - Backing-store transitions (etcd 3.5 → 3.6 across v0.29; safe-hop patches)
  - Topology safety (Deployment topology with non-external backing store)
  - Skip-minor refusal — emits a chained-version plan instead
  - Snapshot reminders (Virtual Control Plane must be awake)

This command never executes mutating operations; it only reports and
emits the runbook commands you should run yourself.

Examples:
  upgrade vcluster --target v0.34.0
  upgrade vcluster --explain         # dump the entire decision tree
  upgrade vcluster --namespace team-a`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVCluster(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig (Control Plane Cluster)")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVar(&o.target, "target", "", "Target vCluster version (e.g. v0.34.0)")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "Limit to a namespace")
	cmd.Flags().StringVar(&o.releaseName, "release", "", "Limit to a single Helm release name")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().BoolVar(&o.explain, "explain", false, "Print the full decision tree instead of running it")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "blocker", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	return cmd
}

func runVCluster(ctx context.Context, o *vclusterOpts) error {
	if o.explain {
		fmt.Println(vcluster.ExplainTree())
		return nil
	}
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	hostVersion, _ := client.ServerVersion()

	var target *apis.Semver
	if o.target != "" {
		t, ok := apis.Parse(o.target)
		if !ok {
			return fmt.Errorf("invalid --target %q", o.target)
		}
		target = &t
	}

	findings, errs := vcluster.Analyze(ctx, client.Core, vcluster.Options{
		Namespace:   o.namespace,
		ReleaseName: o.releaseName,
		Target:      target,
		HostVersion: hostVersion,
	})
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	finding.Sort(findings)
	header := report.Header{
		Tool:          "kubectl-upgrade",
		ToolVersion:   version,
		Source:        "live",
		SourceVersion: hostVersion,
	}
	if target != nil {
		header.Target = target.String()
	}
	if err := report.Render(os.Stdout, header, findings, format); err != nil {
		return err
	}
	return failOnExit(findings, o.failOn)
}
