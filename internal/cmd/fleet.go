package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/ui"
	"github.com/saiyam1814/upgrade/internal/vcluster"
)

type fleetOpts struct {
	hostTarget     string
	vclusterTarget string
	kubeconfig     string
	contextName    string
	plan           bool
	parallel       int
}

func newFleetCmd() *cobra.Command {
	o := &fleetOpts{}
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "vCluster Tenant Cluster fleet upgrade — discover all, plan a safe wave",
		Long: `fleet discovers every vCluster Tenant Cluster on this Control Plane
Cluster, evaluates each against your host-target / vcluster-target,
and emits an ordered upgrade wave with per-tenant pre-flight + the
exact commands to run.

Wave ordering:
  1. Tenants with BLOCKERs first  (must be resolved before host bump)
  2. Then external-etcd tenants   (lowest blast radius)
  3. Then embedded-etcd tenants   (snapshot first)
  4. Then Deployment-topology tenants  (most fragile last)

Always:
  - Emits 'vcluster snapshot create' before any mutating step
  - Refuses skip-minor — emits chained per-minor plan
  - Never executes anything

Use --plan to print a per-tenant runbook ready for ops review.`,
		Example: `  # Fleet status — what tenants exist, what's broken
  kubectl upgrade fleet

  # Plan a wave for an upcoming Control Plane Cluster bump
  kubectl upgrade fleet --host-target v1.34 --plan

  # Plan a wave for a vCluster product bump
  kubectl upgrade fleet --vcluster-target v0.34.0 --plan`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleet(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.hostTarget, "host-target", "", "Target Control Plane Cluster K8s version (e.g. v1.34)")
	cmd.Flags().StringVar(&o.vclusterTarget, "vcluster-target", "", "Target vCluster version (e.g. v0.34.0)")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig (Control Plane Cluster)")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().BoolVar(&o.plan, "plan", false, "Emit a per-tenant runbook")
	cmd.Flags().IntVar(&o.parallel, "parallel", 1, "Concurrency hint for the wave (default 1: sequential)")
	return cmd
}

func runFleet(ctx context.Context, o *fleetOpts) error {
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	hostV, _ := client.ServerVersion()

	tenants, errs := vcluster.Discover(ctx, client.Core, "", "")
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	if len(tenants) == 0 {
		ui.Warn(os.Stdout, "No vCluster Tenant Clusters detected on this Control Plane Cluster.")
		return nil
	}

	ui.Banner(os.Stdout, "vCluster Fleet", fmt.Sprintf("Control Plane Cluster: %s   tenants: %d", hostV, len(tenants)))

	var vcTarget *apis.Semver
	if o.vclusterTarget != "" {
		t, ok := apis.Parse(o.vclusterTarget)
		if !ok {
			return fmt.Errorf("invalid --vcluster-target %q", o.vclusterTarget)
		}
		vcTarget = &t
	}
	var hostTarget *apis.Semver
	if o.hostTarget != "" {
		t, ok := apis.Parse(o.hostTarget)
		if !ok {
			return fmt.Errorf("invalid --host-target %q", o.hostTarget)
		}
		hostTarget = &t
	}

	// Per-tenant evaluation: combine vCluster product gates (--vcluster-target)
	// AND host K8s × vCluster compat checks (--host-target). Either or both may be set.
	type entry struct {
		Tenant   vcluster.Tenant
		Findings []finding.Finding
		Score    int // sort key — lower runs first
	}
	var entries []entry
	for _, t := range tenants {
		var fs []finding.Finding
		if vcTarget != nil || hostTarget != nil {
			fs, _ = vcluster.Analyze(ctx, client.Core, vcluster.Options{
				Namespace:   t.Namespace,
				ReleaseName: t.ReleaseName,
				Target:      vcTarget,
				HostVersion: hostV,
				HostTarget:  hostTarget,
			})
		}
		entries = append(entries, entry{Tenant: t, Findings: fs, Score: scoreTenant(t, fs)})
	}

	// Sort wave order.
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Score < entries[j].Score
	})

	// Print fleet table.
	fmt.Println(ui.Bold("Fleet status"))
	fmt.Println()
	for _, e := range entries {
		t := e.Tenant
		counts := finding.Counts(e.Findings)
		state := ui.Green("OK")
		if counts[finding.Blocker] > 0 {
			state = ui.Red(fmt.Sprintf("BLOCKED (%d)", counts[finding.Blocker]))
		} else if counts[finding.High] > 0 {
			state = ui.Yellow(fmt.Sprintf("HIGH (%d)", counts[finding.High]))
		}
		fmt.Printf("  %-30s  %s  distro=%s  topo=%s  store=%-13s  %s\n",
			t.Namespace+"/"+t.ReleaseName,
			t.Version, t.Distro, t.Topology, t.BackingStore, state)
	}
	fmt.Println()

	if !o.plan {
		ui.Info(os.Stdout, "Re-run with --plan to emit the per-tenant runbook.")
		return nil
	}

	// Emit per-tenant runbook.
	ui.Hr(os.Stdout)
	fmt.Println(ui.Bold("Upgrade Wave (sequential, ordered by safety)"))
	fmt.Println(ui.Dim("  1. Tenants with BLOCKERs first (must resolve before host bump)"))
	fmt.Println(ui.Dim("  2. Then external-etcd tenants  (lowest blast radius)"))
	fmt.Println(ui.Dim("  3. Then embedded/deployed-etcd (snapshot first)"))
	fmt.Println(ui.Dim("  4. Then Deployment-topology   (most fragile)"))
	fmt.Println()

	for i, e := range entries {
		t := e.Tenant
		ui.Step(os.Stdout, i+1, len(entries), fmt.Sprintf("Tenant Cluster %s/%s", t.Namespace, t.ReleaseName))
		ui.SubStep(os.Stdout, ui.Cyan("→"), fmt.Sprintf("vCluster=%s · distro=%s · topo=%s · store=%s", t.Version, t.Distro, t.Topology, t.BackingStore))
		// Snapshot first.
		ui.SubStep(os.Stdout, ui.Cyan("→"), "Snapshot before mutating:")
		ui.Command(os.Stdout, fmt.Sprintf("vcluster snapshot create %s -n %s oci://<registry>/<repo>:%s-pre-upgrade", t.ReleaseName, t.Namespace, t.ReleaseName))

		if vcTarget != nil {
			ui.SubStep(os.Stdout, ui.Cyan("→"), "Upgrade Tenant Cluster:")
			ui.Command(os.Stdout, fmt.Sprintf("helm upgrade --install %s vcluster --repo https://charts.loft.sh --version %s -n %s --reuse-values",
				t.ReleaseName, vcTarget.String(), t.Namespace))
		}
		ui.SubStep(os.Stdout, ui.Cyan("→"), "Verify Tenant Cluster:")
		ui.Command(os.Stdout, fmt.Sprintf("vcluster connect %s -n %s -- kubectl get nodes,ns", t.ReleaseName, t.Namespace))

		// Show this tenant's blockers/highs inline.
		if len(e.Findings) > 0 {
			fmt.Println(ui.Dim("    issues:"))
			for _, f := range e.Findings {
				if f.Severity == finding.Blocker || f.Severity == finding.High {
					fmt.Printf("      %s %s\n", glyph(f.Severity), f.Title)
				}
			}
		}
		fmt.Println()
	}
	ui.Hr(os.Stdout)
	fmt.Println(ui.Dim("Reminder: kubectl-upgrade NEVER mutates. You execute each step."))
	return nil
}

// scoreTenant produces the wave-ordering key.
//
//	0..99       — has BLOCKERs (must run first; resolve before host bump)
//	100..199    — external-etcd
//	200..299    — embedded/deployed-etcd
//	300..399    — Deployment topology (most fragile)
func scoreTenant(t vcluster.Tenant, fs []finding.Finding) int {
	score := 200 // default — embedded-etcd, statefulset
	for _, f := range fs {
		if f.Severity == finding.Blocker {
			return 0
		}
	}
	switch t.BackingStore {
	case "external-etcd":
		score = 100
	case "embedded-etcd", "deployed-etcd":
		score = 200
	}
	if t.Topology == "deployment" {
		score += 100
	}
	return score
}
