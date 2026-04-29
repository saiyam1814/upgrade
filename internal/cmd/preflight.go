package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/addons"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/fleet"
	"github.com/saiyam1814/upgrade/internal/pdb"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/rules/featuregates"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/ui"
	"github.com/saiyam1814/upgrade/internal/vcluster"
	"github.com/saiyam1814/upgrade/internal/volumes"
)

type preflightOpts struct {
	target       string
	kubeconfig   string
	contextName  string
	contexts     []string
	allContexts  bool
	exclude      []string
	parallel     int
	timeoutSec   int
	format       string
	outFile      string
	failOn       string
	skipVCluster bool
}

func newPreflightCmd() *cobra.Command {
	o := &preflightOpts{}
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Aggregate every pre-flight check for a target K8s version",
		Long: `preflight is the day-1 entry point. It runs every workload-side
safety check we have and produces one combined report:

  scan       — deprecated APIs (live + Helm releases)
  simulate   — feature gates / defaults / kubelet / kernel changes
  addons     — cert-manager / Istio / Karpenter / ArgoCD compat
  pdb        — drain-deadlock detector
  volumes    — PV/PVC/CSI/StorageClass safety
  vcluster   — Tenant Cluster decision tree (if any tenants exist)

This is read-only. Run it before every upgrade.`,
		Example: `  # Single cluster (default kubeconfig context)
  kubectl upgrade preflight --target v1.34

  # Multi-cluster fan-out, parallel
  kubectl upgrade preflight --target v1.34 --contexts prod-east,prod-west,staging
  kubectl upgrade preflight --target v1.34 --all-contexts --parallel 10

  # CI gate
  kubectl upgrade preflight --target v1.34 --fail-on blocker

  # Markdown report for a PR
  kubectl upgrade preflight --target v1.34 --format md -o readiness.md`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPreflight(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Required.")
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name (single-cluster mode)")
	cmd.Flags().StringSliceVar(&o.contexts, "contexts", nil, "Comma-separated kubeconfig contexts to fan out across")
	cmd.Flags().BoolVar(&o.allContexts, "all-contexts", false, "Fan out across every context in the kubeconfig")
	cmd.Flags().StringSliceVar(&o.exclude, "exclude", nil, "Substrings of context names to skip in fan-out (e.g. sandbox,scratch)")
	cmd.Flags().IntVar(&o.parallel, "parallel", 8, "Max concurrent context walks in fan-out mode")
	cmd.Flags().IntVar(&o.timeoutSec, "timeout", 90, "Per-context timeout in seconds (fan-out mode)")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().StringVarP(&o.outFile, "output", "o", "", "Write report to file instead of stdout")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "blocker", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	cmd.Flags().BoolVar(&o.skipVCluster, "skip-vcluster", false, "Skip the vCluster Tenant-Cluster sub-check")
	return cmd
}

func runPreflight(ctx context.Context, o *preflightOpts) error {
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

	// Multi-context fan-out path. When --contexts or --all-contexts is set,
	// preflight runs once per context in parallel and aggregates findings.
	if o.allContexts || len(o.contexts) > 0 {
		return runPreflightFanout(ctx, o, target, format)
	}

	engine, err := apis.Load()
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	hostV, _ := client.ServerVersion()

	if format == report.FormatHuman {
		ui.Banner(os.Stdout, "Preflight", fmt.Sprintf("%s → %s", hostV, target))
	}

	var all []finding.Finding

	// 1. Deprecated APIs (live + helm).
	objs, walkErrs := client.Walk(ctx, liveFilter(engine))
	helmObjs, helmErrs := client.HelmReleases(ctx)
	objs = append(objs, helmObjs...)
	all = append(all, scanObjects(objs, engine, target)...)
	if format == report.FormatHuman {
		ui.OK(os.Stdout, fmt.Sprintf("scan: %d objects walked", len(objs)))
	}

	// 2. Forward simulate (feature gates / defaults / kubelet / kernel).
	if from, ok := apis.Parse(hostV); ok {
		all = append(all, featuregates.Simulate(from, target)...)
		if format == report.FormatHuman {
			ui.OK(os.Stdout, "simulate: forward changes added")
		}
	}

	// 3. Addons.
	addonFs, addonErrs := addons.Analyze(ctx, client.Core, target)
	all = append(all, addonFs...)
	if format == report.FormatHuman {
		ui.OK(os.Stdout, "addons: compat checked")
	}

	// 4. PDB.
	pdbFs, pdbErrs := pdb.Analyze(ctx, client.Core)
	all = append(all, pdbFs...)
	if format == report.FormatHuman {
		ui.OK(os.Stdout, "pdb: drain feasibility checked")
	}

	// 5. Volumes.
	volFs, volErrs := volumes.Analyze(ctx, client.Core, target)
	all = append(all, volFs...)
	if format == report.FormatHuman {
		ui.OK(os.Stdout, "volumes: PV/PVC/CSI checked")
	}

	// 6. vCluster (best-effort; ignore "no tenants" warning).
	if !o.skipVCluster {
		vcFs, _ := vcluster.Analyze(ctx, client.Core, vcluster.Options{HostVersion: hostV})
		all = append(all, vcFs...)
		if format == report.FormatHuman {
			ui.OK(os.Stdout, "vcluster: Tenant-Cluster gates checked")
		}
	}

	if format == report.FormatHuman {
		fmt.Println()
		ui.Hr(os.Stdout)
	}

	for _, e := range append(walkErrs, append(helmErrs, append(addonErrs, append(pdbErrs, volErrs...)...)...)...) {
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
		Source:        "live (preflight)",
		SourceVersion: hostV,
		Target:        target.String(),
		RulesData:     apis.DataPath,
	}
	if err := report.Render(w, header, all, format); err != nil {
		return err
	}

	emitRecommendation(format, recommend.Context{
		Command:     "preflight",
		Target:      target.String(),
		Findings:    all,
		HasVCluster: hasCategory(all, finding.CategoryVCluster),
	})
	return failOnExit(all, o.failOn)
}

// runPreflightFanout fans the same preflight work across many contexts
// in parallel via the fleet package. Output is per-cluster table + the
// underlying findings emitted in the chosen format.
func runPreflightFanout(ctx context.Context, o *preflightOpts, target apis.Semver, format report.Format) error {
	engine, err := apis.Load()
	if err != nil {
		return err
	}

	job := fleet.Job(func(jctx context.Context, c *live.Client) ([]finding.Finding, []error) {
		var all []finding.Finding
		var allErrs []error

		hostV, _ := c.ServerVersion()
		objs, walkErrs := c.Walk(jctx, liveFilter(engine))
		allErrs = append(allErrs, walkErrs...)
		helmObjs, helmErrs := c.HelmReleases(jctx)
		allErrs = append(allErrs, helmErrs...)
		objs = append(objs, helmObjs...)
		all = append(all, scanObjects(objs, engine, target)...)

		if from, ok := apis.Parse(hostV); ok {
			all = append(all, featuregates.Simulate(from, target)...)
		}
		af, ae := addons.Analyze(jctx, c.Core, target)
		all = append(all, af...)
		allErrs = append(allErrs, ae...)
		pf, pe := pdb.Analyze(jctx, c.Core)
		all = append(all, pf...)
		allErrs = append(allErrs, pe...)
		vf, ve := volumes.Analyze(jctx, c.Core, target)
		all = append(all, vf...)
		allErrs = append(allErrs, ve...)
		if !o.skipVCluster {
			vcf, _ := vcluster.Analyze(jctx, c.Core, vcluster.Options{HostVersion: hostV})
			all = append(all, vcf...)
		}
		return all, allErrs
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

	if format == report.FormatHuman {
		renderDrift(os.Stdout, results, target, true)
	} else {
		// JSON / md / sarif: emit aggregated report by flattening.
		all := flatten(results)
		header := report.Header{
			Tool:        "kubectl-upgrade",
			ToolVersion: version,
			Source:      fmt.Sprintf("fan-out (%d contexts)", len(results)),
			Target:      target.String(),
			RulesData:   apis.DataPath,
		}
		w := os.Stdout
		if o.outFile != "" {
			f, ferr := os.Create(o.outFile)
			if ferr != nil {
				return ferr
			}
			defer f.Close()
			w = f
		}
		if err := report.Render(w, header, all, format); err != nil {
			return err
		}
	}

	all := flatten(results)
	emitRecommendation(format, recommend.Context{
		Command:     "preflight",
		Target:      target.String(),
		Findings:    all,
		HasVCluster: hasCategory(all, finding.CategoryVCluster),
	})
	return failOnExit(all, o.failOn)
}
