package cmd

import (
	"github.com/spf13/cobra"
)

func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "kubectl-upgrade",
		Short: "The conductor for any Kubernetes upgrade — managed, self-managed, vCluster fleet",
		Long: `kubectl-upgrade is the missing pre-flight + watch + verify layer that
wraps any Kubernetes upgrade. It tells you what will break before you start,
generates the exact provider-specific upgrade commands for you to run,
watches the upgrade as it progresses (catching stuck PDBs, stalled CRD
migrations, addon dependency cycles), tells you how to unstick it, and
verifies success after.

It does NOT run cloud CLIs itself — it makes them safe to run.

Three top-level flows:

  preflight  scan + simulate + addons + pdb + volumes + vcluster
  run        emit cloud-CLI command + watch + verify (never-execute-default)
  fleet      vCluster Tenant Cluster fleet upgrade wave

Plus toolkit commands:

  scan       deprecated APIs in manifests/Helm/live cluster
  simulate   forward sim: feature gates, default flips, kubelet, kernel
  pdb        drain deadlock detector
  addons     cert-manager / Istio / Karpenter / ArgoCD compat check
  volumes    PV/PVC/CSI/StorageClass safety
  vcluster   per-Tenant-Cluster decision tree
  unstick    stuck-state recovery toolkit
  plan       chained one-minor-at-a-time path
  report     combined report — Markdown / JSON / SARIF
  tui        interactive visual upgrade dashboard

Safety: every command is read-only by default. Anything that would mutate
requires --execute AND a per-action confirmation. Run with --dry-run to
preview the exact commands without invoking anything.`,
		Example: `  # Day-1 production flow
  kubectl upgrade preflight --target v1.34
  kubectl upgrade run plan --target v1.34
  # ... you run the emitted cloud CLI command ...
  kubectl upgrade run watch
  kubectl upgrade run verify --target v1.34

  # vCluster fleet upgrade
  kubectl upgrade fleet --host-target v1.33

  # If something gets stuck
  kubectl upgrade unstick

  # Visual mode
  kubectl upgrade tui --target v1.34`,
		SilenceUsage: true,
	}

	root.AddCommand(
		newPreflightCmd(),
		newRunCmd(),
		newFleetCmd(),
		newScanCmd(),
		newPlanCmd(),
		newPDBCmd(),
		newAddonsCmd(),
		newVolumesCmd(),
		newVClusterCmd(),
		newSimulateCmd(),
		newUnstickCmd(),
		newReportCmd(),
		newTUICmd(),
		newVersionCmd(),
	)
	return root
}
