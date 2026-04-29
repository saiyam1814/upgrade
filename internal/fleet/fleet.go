// Package fleet runs commands across MANY kubeconfig contexts in
// parallel, with per-context timeouts and identity scoping.
//
// The 50-cluster admin needs `kubectl upgrade preflight --contexts
// a,b,c,...` to fan out, walk every cluster concurrently, and
// produce one combined report — without a slow cluster blocking the
// whole run. This package owns that fan-out plus the cross-cluster
// drift report.
package fleet

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/sources/live"
)

// ContextResult bundles the outcome of a single-cluster run inside
// a fan-out. Errors are per-cluster — one cluster's failure never
// aborts the others.
type ContextResult struct {
	Context       string
	ServerVersion string
	Findings      []finding.Finding
	Errors        []error
	Elapsed       time.Duration
}

// Job is one cluster-bound function the caller wants run.
// `ctx` carries the per-cluster timeout deadline. The returned
// findings are merged into the result; errors are appended.
type Job func(ctx context.Context, c *live.Client) ([]finding.Finding, []error)

// Options narrow a fan-out.
type Options struct {
	Kubeconfig    string
	Contexts      []string      // explicit contexts (empty = AllContexts)
	AllContexts   bool          // when true and Contexts is empty, walk every context in kubeconfig
	Exclude       []string      // glob-ish substring matches (e.g. "sandbox")
	Parallel      int           // max concurrent goroutines (default: min(8, len(contexts)))
	PerCtxTimeout time.Duration // per-cluster wall-clock cap (default: 90s)
}

// ListContexts returns the contexts the user asked for, after
// resolving --all-contexts and applying --exclude.
func ListContexts(o Options) ([]string, error) {
	if len(o.Contexts) > 0 {
		return filterExcluded(o.Contexts, o.Exclude), nil
	}
	if !o.AllContexts {
		return nil, fmt.Errorf("no contexts specified (set --contexts or --all-contexts)")
	}
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.Kubeconfig != "" {
		loader.ExplicitPath = o.Kubeconfig
	}
	cfg, err := loader.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	out := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		out = append(out, name)
	}
	sort.Strings(out)
	return filterExcluded(out, o.Exclude), nil
}

func filterExcluded(in, excludes []string) []string {
	if len(excludes) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, c := range in {
		drop := false
		for _, ex := range excludes {
			if strings.Contains(c, ex) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, c)
		}
	}
	return out
}

// Run fans out the job across every context, returning per-context
// results in the same order as ListContexts. Concurrency is capped
// by Options.Parallel, and per-context wall-clock by PerCtxTimeout.
func Run(parent context.Context, o Options, job Job) ([]ContextResult, error) {
	contexts, err := ListContexts(o)
	if err != nil {
		return nil, err
	}
	if len(contexts) == 0 {
		return nil, fmt.Errorf("no contexts after exclusions")
	}
	parallel := o.Parallel
	if parallel <= 0 {
		parallel = 8
	}
	if parallel > len(contexts) {
		parallel = len(contexts)
	}
	timeout := o.PerCtxTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}

	results := make([]ContextResult, len(contexts))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i, ctxName := range contexts {
		i, ctxName := i, ctxName
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			r := ContextResult{Context: ctxName}
			cctx, cancel := context.WithTimeout(parent, timeout)
			defer cancel()

			client, err := live.Connect(o.Kubeconfig, ctxName)
			if err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("connect: %w", err))
				r.Elapsed = time.Since(start)
				results[i] = r
				return
			}
			r.ServerVersion, _ = client.ServerVersion()
			fs, errs := job(cctx, client)
			r.Findings = append(r.Findings, fs...)
			r.Errors = append(r.Errors, errs...)
			r.Elapsed = time.Since(start)
			results[i] = r
		}()
	}
	wg.Wait()
	return results, nil
}

// AggregateCounts returns BLOCKER/HIGH/MEDIUM totals across the fleet.
func AggregateCounts(results []ContextResult) map[string]int {
	out := map[string]int{}
	for _, r := range results {
		for sev, n := range finding.Counts(r.Findings) {
			out[string(sev)] += n
		}
	}
	return out
}

// VersionDistribution returns "v1.31"->count, "v1.30"->count, ...
// (major.minor only).
func VersionDistribution(results []ContextResult) map[string]int {
	out := map[string]int{}
	for _, r := range results {
		v := normalizeMinor(r.ServerVersion)
		if v == "" {
			v = "unknown"
		}
		out[v]++
	}
	return out
}

func normalizeMinor(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return ""
	}
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return "v" + parts[0] + "." + parts[1]
}

// FindingDistribution maps a finding "title hash" to the list of
// contexts that surfaced it. Used by the drift report's "top
// issues across the fleet" section.
func FindingDistribution(results []ContextResult) map[string][]string {
	out := map[string][]string{}
	for _, r := range results {
		seen := map[string]bool{}
		for _, f := range r.Findings {
			key := string(f.Severity) + ": " + f.Title
			// Strip per-object suffixes ("PDB ns/name ...") so we group
			// "same-class" findings across clusters. Heuristic: cut at the
			// first em-dash or "(target ".
			if i := strings.Index(key, " (target "); i > 0 {
				key = key[:i]
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out[key] = append(out[key], r.Context)
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}
