package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/addons"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/fleet"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/ui"
)

type driftOpts struct {
	target      string
	kubeconfig  string
	contexts    []string
	allContexts bool
	exclude     []string
	parallel    int
	timeoutSec  int
}

func newFleetDriftCmd() *cobra.Command {
	o := &driftOpts{}
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Fleet-wide K8s version distribution + top issues across many clusters",
		Long: `drift fans out across many kubeconfig contexts in parallel and
produces the headline "where is my fleet?" report:

  - K8s minor-version distribution: "42/50 on v1.31, 5 on v1.30, 3 on v1.29"
  - Top BLOCKER/HIGH issues with the cluster list each affects
  - Per-cluster outliers
  - Recommendations for which clusters to preflight first

Read-only. RBAC-scoped per kubeconfig context (each cluster only sees
what its identity is allowed to see).`,
		Example: `  # Drift across every context in the kubeconfig (default)
  kubectl upgrade fleet drift --target v1.34

  # Specific clusters
  kubectl upgrade fleet drift --target v1.34 --contexts prod-east,prod-west,staging

  # Big fleet, more parallelism, longer timeout
  kubectl upgrade fleet drift --target v1.34 --all-contexts --parallel 20 --timeout 120

  # Skip sandbox/dev contexts
  kubectl upgrade fleet drift --target v1.34 --all-contexts --exclude sandbox,scratch`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFleetDrift(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Optional; affects 'top issues' findings.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringSliceVar(&o.contexts, "contexts", nil, "Comma-separated kubeconfig contexts (default: --all-contexts implied if empty AND no other flag set)")
	cmd.Flags().BoolVar(&o.allContexts, "all-contexts", false, "Walk every context in the kubeconfig")
	cmd.Flags().StringSliceVar(&o.exclude, "exclude", nil, "Substrings of context names to skip (e.g. sandbox,scratch)")
	cmd.Flags().IntVar(&o.parallel, "parallel", 8, "Max concurrent context walks")
	cmd.Flags().IntVar(&o.timeoutSec, "timeout", 90, "Per-context timeout in seconds")
	return cmd
}

func runFleetDrift(ctx context.Context, o *driftOpts) error {
	// Default: if neither --contexts nor --all-contexts is set, imply --all-contexts.
	if !o.allContexts && len(o.contexts) == 0 {
		o.allContexts = true
	}

	// Build the per-cluster job. Each cluster gets a quick scan +
	// addons compat + a server-version readout (for distribution).
	target, hasTarget := apis.Parse(o.target)

	job := fleet.Job(func(ctx context.Context, c *live.Client) ([]finding.Finding, []error) {
		var findings []finding.Finding
		var errs []error
		// Addons: only when we have a target
		if hasTarget {
			fs, e := addons.Analyze(ctx, c.Core, target)
			findings = append(findings, fs...)
			errs = append(errs, e...)
		}
		// Light scan for the version distribution row — we don't run
		// the full deprecated-API walk here (too slow at fleet scale);
		// the user runs preflight on outliers afterwards.
		return findings, errs
	})

	results, err := fleet.Run(ctx, fleet.Options{
		Kubeconfig:    o.kubeconfig,
		Contexts:      o.contexts,
		AllContexts:   o.allContexts,
		Exclude:       o.exclude,
		Parallel:      o.parallel,
		PerCtxTimeout: time.Duration(o.timeoutSec) * time.Second,
	}, job)
	if err != nil {
		return err
	}

	renderDrift(os.Stdout, results, target, hasTarget)

	// Recommendation footer.
	all := flatten(results)
	hint := recommend.NextStep(recommend.Context{
		Command:  "fleet drift",
		Target:   target.String(),
		Findings: all,
	})
	if hint != "" {
		fmt.Println()
		fmt.Println(ui.Bold("→ Next: ") + hint)
	}
	return nil
}

func renderDrift(w *os.File, results []fleet.ContextResult, target apis.Semver, hasTarget bool) {
	tgtLabel := "(no target)"
	if hasTarget {
		tgtLabel = target.String()
	}
	ui.Banner(w, "Fleet drift", fmt.Sprintf("%d clusters · target %s", len(results), tgtLabel))

	// 1. Aggregate counts.
	agg := fleet.AggregateCounts(results)
	fmt.Printf("Aggregate findings: %s · %s · %s\n",
		ui.Red(fmt.Sprintf("%d BLOCKER", agg["BLOCKER"])),
		ui.Yellow(fmt.Sprintf("%d HIGH", agg["HIGH"])),
		ui.Cyan(fmt.Sprintf("%d MEDIUM", agg["MEDIUM"])))
	fmt.Println()

	// 2. K8s version distribution with bar chart.
	dist := fleet.VersionDistribution(results)
	versions := make([]string, 0, len(dist))
	for v := range dist {
		versions = append(versions, v)
	}
	sort.SliceStable(versions, func(i, j int) bool {
		// reverse-sort so newest first
		return versions[i] > versions[j]
	})
	maxN := 0
	for _, v := range versions {
		if dist[v] > maxN {
			maxN = dist[v]
		}
	}
	fmt.Println(ui.Bold("K8s version distribution:"))
	for _, v := range versions {
		n := dist[v]
		pct := float64(n) / float64(len(results)) * 100
		bar := strings.Repeat("█", barWidth(n, maxN, 30))
		fmt.Printf("  %-7s %s %d (%2.0f%%)\n", v, ui.Cyan(bar), n, pct)
	}
	fmt.Println()

	// 3. Per-cluster table.
	fmt.Println(ui.Bold("Per-cluster:"))
	for _, r := range results {
		state := ui.Green("✓ clean")
		c := finding.Counts(r.Findings)
		if c[finding.Blocker] > 0 {
			state = ui.Red(fmt.Sprintf("✗ BLOCKER (%d)", c[finding.Blocker]))
		} else if c[finding.High] > 0 {
			state = ui.Yellow(fmt.Sprintf("⚠ HIGH (%d)", c[finding.High]))
		}
		errMark := ""
		if len(r.Errors) > 0 {
			errMark = ui.Red(fmt.Sprintf("  (%d errors)", len(r.Errors)))
		}
		fmt.Printf("  %-30s  %-8s  %-22s  %s%s\n",
			truncate(r.Context, 30), prettyVersion(r.ServerVersion), state,
			ui.Dim(fmt.Sprintf("%5dms", r.Elapsed.Milliseconds())), errMark)
	}
	fmt.Println()

	// 4. Top issues across the fleet.
	dists := fleet.FindingDistribution(results)
	if len(dists) > 0 {
		fmt.Println(ui.Bold("Top issues across the fleet:"))
		// sort keys by count of clusters, desc
		type kv struct {
			Key string
			N   int
		}
		var rows []kv
		for k, v := range dists {
			rows = append(rows, kv{k, len(v)})
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].N > rows[j].N })
		shown := 0
		for _, r := range rows {
			if shown >= 8 {
				break
			}
			shown++
			titlePart := r.Key
			ctxs := dists[r.Key]
			ctxsLabel := strings.Join(ctxs, ", ")
			if len(ctxsLabel) > 60 {
				ctxsLabel = ctxsLabel[:57] + "..."
			}
			fmt.Printf("  %3d cluster(s)  %s\n", r.N, titlePart)
			fmt.Printf("                  %s\n", ui.Dim(ctxsLabel))
		}
		fmt.Println()
	}

	// 5. Per-cluster errors (warnings).
	for _, r := range results {
		for _, e := range r.Errors {
			ui.Warn(w, fmt.Sprintf("[%s] %v", r.Context, e))
		}
	}
}

func barWidth(n, max, width int) int {
	if max == 0 {
		return 0
	}
	w := n * width / max
	if w < 1 && n > 0 {
		return 1
	}
	return w
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func prettyVersion(v string) string {
	if v == "" {
		return "unknown"
	}
	// strip provider suffix for tabling, e.g. v1.30.4-gke.1234 → v1.30.4
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return v[:i]
	}
	return v
}

func flatten(results []fleet.ContextResult) []finding.Finding {
	var out []finding.Finding
	for _, r := range results {
		out = append(out, r.Findings...)
	}
	return out
}
