package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/cloud"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/ui"
	"github.com/saiyam1814/upgrade/internal/unstick"
)

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "The conductor — emit cloud-CLI commands, watch progress, verify",
		Long: `run orchestrates a real Kubernetes upgrade by integrating preflight,
provider-specific commands, watch, and verify. It NEVER mutates the
cluster or invokes cloud CLIs itself — it tells you exactly what to
run, watches the result, and confirms success.

Subcommands:

  run plan     — emit the runbook (preflight + provider-CLI commands)
  run watch    — observe an in-flight upgrade for stuck patterns
  run verify   — post-upgrade verification (server version + rescan)`,
	}
	cmd.AddCommand(newRunPlanCmd(), newRunWatchCmd(), newRunVerifyCmd())
	return cmd
}

// ---- run plan ----

type runPlanOpts struct {
	target      string
	kubeconfig  string
	contextName string
}

func newRunPlanCmd() *cobra.Command {
	o := &runPlanOpts{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Emit the upgrade runbook for this cluster (cloud-CLI commands + checklist)",
		Long: `plan detects your cluster's provider (EKS / GKE / AKS / OpenShift /
RKE2 / k3s / Talos / kubeadm / vCluster), prints a step-by-step
runbook with the EXACT cloud-CLI commands to upgrade the control
plane and node pools, and a per-step checklist.

It does not execute any of the commands. Copy/paste from this output.`,
		Example: `  kubectl upgrade run plan --target v1.34`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunPlan(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Required.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	return cmd
}

func runRunPlan(ctx context.Context, o *runPlanOpts) error {
	if o.target == "" {
		return fmt.Errorf("--target is required")
	}
	target, ok := apis.Parse(o.target)
	if !ok {
		return fmt.Errorf("invalid --target %q", o.target)
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	cluster, err := cloud.Detect(ctx, client.Core)
	if err != nil {
		return err
	}

	ui.Banner(os.Stdout, "Upgrade Runbook", fmt.Sprintf("%s → %s   provider: %s", cluster.GitVersion, target, cluster.Provider))

	// Step 1 — preflight reminder
	ui.Step(os.Stdout, 1, 6, "Pre-flight scan")
	ui.SubStep(os.Stdout, ui.Cyan("→"), "Run pre-flight to find every workload-side time bomb:")
	ui.Command(os.Stdout, fmt.Sprintf("kubectl upgrade preflight --target %s", target))
	fmt.Println()

	// Step 2 — backup
	ui.Step(os.Stdout, 2, 6, "Backup")
	ui.SubStep(os.Stdout, ui.Cyan("→"), "Take a fresh etcd snapshot or Velero backup. Verify the backup restores in a sandbox before proceeding.")
	ui.Command(os.Stdout, "velero backup create pre-upgrade-$(date +%s) --include-cluster-resources")
	ui.SubStep(os.Stdout, ui.Cyan("→"), "If you have vCluster Tenant Clusters, snapshot each one:")
	ui.Command(os.Stdout, "kubectl upgrade fleet --host-target "+target.String()+" --plan")
	fmt.Println()

	// Step 3 — provider commands
	plan := cluster.Plan(target.String())
	ui.Step(os.Stdout, 3, 6, "Control-plane upgrade ("+string(plan.Provider)+")")
	if len(plan.PreReqs) > 0 {
		ui.SubStep(os.Stdout, ui.Cyan("→"), "Verify auth + current state:")
		for _, c := range plan.PreReqs {
			ui.Command(os.Stdout, c)
		}
		fmt.Println()
	}
	if len(plan.ControlPlane) > 0 {
		ui.SubStep(os.Stdout, ui.Cyan("→"), "Run the control-plane upgrade:")
		for _, c := range plan.ControlPlane {
			ui.Command(os.Stdout, c)
		}
	} else {
		ui.Warn(os.Stdout, "No automated commands available for provider="+string(plan.Provider)+". Consult your distro docs.")
	}
	fmt.Println()

	// Step 4 — watch
	ui.Step(os.Stdout, 4, 6, "Watch for stuck states (separate terminal)")
	ui.Command(os.Stdout, "kubectl upgrade run watch")
	fmt.Println()

	// Step 5 — node pools
	ui.Step(os.Stdout, 5, 6, "Node pools / kubelet upgrade")
	if len(plan.NodePools) > 0 {
		for _, c := range plan.NodePools {
			ui.Command(os.Stdout, c)
		}
	}
	fmt.Println()

	// Step 6 — verify
	ui.Step(os.Stdout, 6, 6, "Post-upgrade verification")
	ui.Command(os.Stdout, fmt.Sprintf("kubectl upgrade run verify --target %s", target))
	fmt.Println()

	if len(plan.Notes) > 0 {
		ui.Hr(os.Stdout)
		fmt.Println(ui.Bold("Provider notes:"))
		for _, n := range plan.Notes {
			fmt.Println("  • " + n)
		}
		fmt.Println()
	}

	ui.Hr(os.Stdout)
	fmt.Println(ui.Dim("Reminder: kubectl-upgrade NEVER runs the cloud CLI itself. You execute each step."))
	return nil
}

// ---- run watch ----

type runWatchOpts struct {
	kubeconfig   string
	contextName  string
	intervalSec  int
	stopAfterMin int
	namespace    string
}

func newRunWatchCmd() *cobra.Command {
	o := &runWatchOpts{}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch an in-flight upgrade for stuck states (every --interval sec)",
		Long: `watch polls the cluster on an interval and surfaces every stuck
pattern detected by 'unstick' as soon as it appears. Run this in a
separate terminal while the upgrade is in progress.

Stops after --stop-after minutes (default 60), or on Ctrl-C, or when
no findings appear for 3 consecutive cycles AND the server version
matches the target you specified.`,
		Example: `  kubectl upgrade run watch                # default 30s interval, 60min cap
  kubectl upgrade run watch --interval 10  # be more aggressive`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunWatch(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().IntVar(&o.intervalSec, "interval", 30, "Poll interval in seconds")
	cmd.Flags().IntVar(&o.stopAfterMin, "stop-after", 60, "Stop after N minutes")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "Limit to a namespace")
	return cmd
}

func runRunWatch(ctx context.Context, o *runWatchOpts) error {
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	deadline := time.Now().Add(time.Duration(o.stopAfterMin) * time.Minute)
	tick := time.Duration(o.intervalSec) * time.Second
	ui.Banner(os.Stdout, "Watching upgrade", fmt.Sprintf("interval=%s · stop-after=%dm", tick, o.stopAfterMin))

	cycle := 0
	cleanCycles := 0
	prevServerVer := ""

	for time.Now().Before(deadline) {
		cycle++
		v, _ := client.ServerVersion()
		if v != prevServerVer && prevServerVer != "" {
			ui.Info(os.Stdout, fmt.Sprintf("[cycle %d] server version %s → %s", cycle, prevServerVer, v))
		}
		prevServerVer = v

		findings, _ := unstick.Analyze(ctx, client.Core, unstick.Options{
			Namespace:      o.namespace,
			StuckThreshold: 3 * time.Minute,
		})
		if len(findings) == 0 {
			cleanCycles++
			ui.OK(os.Stdout, fmt.Sprintf("[cycle %d  %s] all clean (%d clean cycles)", cycle, time.Now().Format("15:04:05"), cleanCycles))
			if cleanCycles >= 3 {
				ui.OK(os.Stdout, "3 clean cycles — upgrade looks settled. Run 'kubectl upgrade run verify' next.")
				return nil
			}
		} else {
			cleanCycles = 0
			counts := finding.Counts(findings)
			ui.Warn(os.Stdout, fmt.Sprintf("[cycle %d  %s] %d BLOCKER · %d HIGH · %d MEDIUM",
				cycle, time.Now().Format("15:04:05"),
				counts[finding.Blocker], counts[finding.High], counts[finding.Medium]))
			finding.Sort(findings)
			for _, f := range firstN(findings, 5) {
				fmt.Printf("    %s %s\n", glyph(f.Severity), f.Title)
			}
			if len(findings) > 5 {
				fmt.Printf("    %s\n", ui.Dim(fmt.Sprintf("... and %d more (run 'kubectl upgrade unstick' for full list)", len(findings)-5)))
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tick):
		}
	}
	ui.Warn(os.Stdout, "stop-after deadline reached")
	return nil
}

func firstN(s []finding.Finding, n int) []finding.Finding {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func glyph(s finding.Severity) string {
	switch s {
	case finding.Blocker:
		return ui.Red("✗")
	case finding.High:
		return ui.Yellow("⚠")
	case finding.Medium:
		return ui.Cyan("•")
	}
	return ui.Dim("·")
}

// ---- run verify ----

type runVerifyOpts struct {
	target      string
	kubeconfig  string
	contextName string
	failOn      string
}

func newRunVerifyCmd() *cobra.Command {
	o := &runVerifyOpts{}
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Post-upgrade verification: server version + rescan + smoke checks",
		Long: `verify confirms an upgrade landed safely:

  - Server version matches --target
  - No NotReady nodes
  - No critical operator Pods in CrashLoopBackoff
  - Rescan: no new BLOCKER/HIGH findings vs. before
  - Sample workloads (default kube-system) all Running

This is read-only and idempotent — safe to re-run.`,
		Example: `  kubectl upgrade run verify --target v1.34`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRunVerify(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Required.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "high", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	return cmd
}

func runRunVerify(ctx context.Context, o *runVerifyOpts) error {
	if o.target == "" {
		return fmt.Errorf("--target is required")
	}
	target, ok := apis.Parse(o.target)
	if !ok {
		return fmt.Errorf("invalid --target %q", o.target)
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	ui.Banner(os.Stdout, "Verify upgrade", "target "+target.String())

	// Server version check.
	v, err := client.ServerVersion()
	if err != nil {
		ui.Err(os.Stdout, "server version unreachable: "+err.Error())
		return err
	}
	got, ok := apis.Parse(v)
	if !ok {
		ui.Warn(os.Stdout, "server version "+v+" — could not parse, skipping equality check")
	} else if got.Equal(target) {
		ui.OK(os.Stdout, "server version "+v+" matches target")
	} else {
		ui.Err(os.Stdout, "server version "+v+" ≠ target "+target.String())
	}

	// Run unstick — anything stuck = failure.
	findings, errs := unstick.Analyze(ctx, client.Core, unstick.Options{StuckThreshold: 1 * time.Minute})
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	finding.Sort(findings)

	counts := finding.Counts(findings)
	if len(findings) == 0 {
		ui.OK(os.Stdout, "no stuck states detected")
	} else {
		ui.Warn(os.Stdout, fmt.Sprintf("%d BLOCKER · %d HIGH · %d MEDIUM stuck states",
			counts[finding.Blocker], counts[finding.High], counts[finding.Medium]))
		for _, f := range firstN(findings, 10) {
			fmt.Printf("    %s %s\n", glyph(f.Severity), f.Title)
		}
	}

	return failOnExit(findings, o.failOn)
}
