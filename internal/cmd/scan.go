package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/sources/manifests"
)

type scanOpts struct {
	target        string
	sources       []string
	manifestPath  string
	kubeconfig    string
	contextName   string
	format        string
	outFile       string
	failOn        string
	includeHelm   bool
	skipDiscovery bool
}

func newScanCmd() *cobra.Command {
	o := &scanOpts{}
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Scan live cluster + Helm releases + manifests for upgrade-blocking issues",
		Long: `scan walks one or more sources and reports every workload-side issue
that will affect a Kubernetes upgrade to --target.

Sources (--source, repeatable; defaults to "live,helm"):
  live      — connect to the current kubeconfig context, walk every
              listable GVK that has a deprecation rule, list objects
  helm      — read all "helm.sh/release.v1" Secrets across namespaces;
              parse each release's rendered manifest for shadow APIs
  manifests — read --path (file or directory) of YAML/JSON manifests

Examples:
  upgrade scan --target v1.34
  upgrade scan --target v1.33 --source manifests --path ./k8s/
  upgrade scan --target v1.34 --format json -o report.json
  upgrade scan --target v1.34 --fail-on high   # CI gate`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScan(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.target, "target", "", "Target Kubernetes version (e.g. v1.34). Required.")
	f.StringSliceVar(&o.sources, "source", []string{"live", "helm"}, "Sources to scan: live,helm,manifests")
	f.StringVarP(&o.manifestPath, "path", "p", "", "Manifest path (file or directory) for --source manifests")
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig (default: KUBECONFIG env / ~/.kube/config)")
	f.StringVar(&o.contextName, "context", "", "Kubeconfig context name (default: current-context)")
	f.StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	f.StringVarP(&o.outFile, "output", "o", "", "Write report to file instead of stdout")
	f.StringVar(&o.failOn, "fail-on", "blocker", "Exit non-zero if any finding at or above: blocker|high|medium|low")
	f.BoolVar(&o.includeHelm, "include-helm", true, "Scan Helm release secrets (set false to disable when 'helm' is in --source)")
	f.BoolVar(&o.skipDiscovery, "skip-discovery", false, "Skip live discovery; useful when only manifests source is desired")
	return cmd
}

func runScan(ctx context.Context, o *scanOpts) error {
	if o.target == "" {
		return fmt.Errorf("--target is required (e.g. --target v1.34)")
	}
	target, ok := apis.Parse(o.target)
	if !ok {
		return fmt.Errorf("invalid --target %q (want vMAJOR.MINOR)", o.target)
	}
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}

	engine, err := apis.Load()
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	sourceSet := normalizeSources(o.sources)
	header := report.Header{
		Tool:        "kubectl-upgrade",
		ToolVersion: version,
		Source:      strings.Join(sourceSet, "+"),
		Target:      target.String(),
		RulesData:   apis.DataPath,
	}

	var (
		allObjects []manifests.Object
		errs       []error
	)

	if contains(sourceSet, "manifests") {
		if o.manifestPath == "" {
			return fmt.Errorf("--path is required when --source includes manifests")
		}
		objs, err := manifests.Read(o.manifestPath)
		if err != nil {
			return fmt.Errorf("manifests: %w", err)
		}
		allObjects = append(allObjects, objs...)
	}

	if contains(sourceSet, "live") || contains(sourceSet, "helm") {
		client, err := live.Connect(o.kubeconfig, o.contextName)
		if err != nil {
			return fmt.Errorf("live connect: %w", err)
		}
		if v, err := client.ServerVersion(); err == nil {
			header.SourceVersion = v
		}

		if contains(sourceSet, "live") && !o.skipDiscovery {
			filter := liveFilter(engine)
			objs, walkErrs := client.Walk(ctx, filter)
			allObjects = append(allObjects, objs...)
			errs = append(errs, walkErrs...)
		}
		if contains(sourceSet, "helm") && o.includeHelm {
			objs, helmErrs := client.HelmReleases(ctx)
			allObjects = append(allObjects, objs...)
			errs = append(errs, helmErrs...)
		}
	}

	findings := scanObjects(allObjects, engine, target)

	w := os.Stdout
	if o.outFile != "" {
		f, err := os.Create(o.outFile)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if err := report.Render(w, header, findings, format); err != nil {
		return err
	}

	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}

	return failOnExit(findings, o.failOn)
}

// scanObjects applies the API rule engine to every object and returns
// the findings.
func scanObjects(objs []manifests.Object, engine *apis.Engine, target apis.Semver) []finding.Finding {
	var out []finding.Finding
	for _, mo := range objs {
		if f := engine.FindingFor(mo.Obj, mo.Source, target); f != nil {
			out = append(out, *f)
		}
	}
	return out
}

// liveFilter only walks GVKs that have at least one deprecation rule
// — this keeps the live scan fast on large clusters.
func liveFilter(engine *apis.Engine) live.Filter {
	known := map[string]bool{}
	for _, r := range engine.All() {
		known[strings.ToLower(r.APIVersion)+"|"+strings.ToLower(r.Kind)] = true
	}
	return func(apiVersion, kind string) bool {
		return known[strings.ToLower(apiVersion)+"|"+strings.ToLower(kind)]
	}
}

func normalizeSources(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func failOnExit(findings []finding.Finding, threshold string) error {
	threshold = strings.ToLower(threshold)
	var thresh finding.Severity
	switch threshold {
	case "blocker":
		thresh = finding.Blocker
	case "high":
		thresh = finding.High
	case "medium":
		thresh = finding.Medium
	case "low":
		thresh = finding.Low
	case "none", "":
		return nil
	default:
		return fmt.Errorf("unknown --fail-on %q", threshold)
	}
	for _, f := range findings {
		if f.Severity.Rank() <= thresh.Rank() {
			os.Exit(2)
		}
	}
	return nil
}
