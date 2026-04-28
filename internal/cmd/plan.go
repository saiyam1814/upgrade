package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/rules/apis"
)

type planOpts struct {
	from string
	to   string
}

func newPlanCmd() *cobra.Command {
	o := &planOpts{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Plan a chained, one-minor-at-a-time upgrade path",
		Long: `plan emits the recommended chained upgrade path between two minor
Kubernetes versions. Skipping minors is unsupported by the upstream
project and silently breaks operators, conversion webhooks, and CRD
storage versions — so plan refuses to compress hops.

Example:
  upgrade plan --from v1.30 --to v1.34
  → v1.30 → v1.31 → v1.32 → v1.33 → v1.34 (4 hops)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(o)
		},
	}
	cmd.Flags().StringVar(&o.from, "from", "", "Current Kubernetes version (e.g. v1.30). Required.")
	cmd.Flags().StringVar(&o.to, "to", "", "Target Kubernetes version (e.g. v1.34). Required.")
	return cmd
}

func runPlan(o *planOpts) error {
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
	if from.Major != to.Major {
		return fmt.Errorf("cross-major upgrades (%s → %s) are not supported by Kubernetes", from, to)
	}
	if !from.Less(to) && !from.Equal(to) {
		return fmt.Errorf("--from must be ≤ --to (got %s → %s)", from, to)
	}
	hops := []string{}
	for v := from; v.Less(to) || v.Equal(to); v.Minor++ {
		hops = append(hops, v.String())
	}
	fmt.Printf("Chained upgrade plan: %s\n", strings.Join(hops, " → "))
	fmt.Printf("Hops: %d\n\n", len(hops)-1)
	fmt.Println("Per-hop checklist:")
	fmt.Println("  1. Take a fresh etcd snapshot / Velero backup.")
	fmt.Println("  2. Run `upgrade scan --target <next-hop>`. Resolve all BLOCKERS.")
	fmt.Println("  3. Run `upgrade addons --target <next-hop>`. Bump incompatible operators FIRST.")
	fmt.Println("  4. Run `upgrade pdb`. Fix ALLOWED DISRUPTIONS == 0 PDBs.")
	fmt.Println("  5. Pause GitOps reconciliation (ArgoCD auto-sync, Flux suspend).")
	fmt.Println("  6. Upgrade control plane.")
	fmt.Println("  7. Upgrade nodes (rolling, max-surge ≥ 1).")
	fmt.Println("  8. Verify workloads, resume GitOps, re-snapshot as new baseline.")
	fmt.Println()
	fmt.Println("Tip: run `upgrade scan --target", to, "` once first to surface")
	fmt.Println("     deprecations that span the whole chain — fixing them up-front")
	fmt.Println("     amortizes the work across hops.")
	return nil
}
