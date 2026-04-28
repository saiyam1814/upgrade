// Package finding defines the unified Finding type that every check
// — API deprecation, PDB deadlock, addon incompat, vCluster gate,
// feature-gate flip, etc. — emits. The render layer consumes this
// without caring which source produced it.
package finding

import (
	"fmt"
	"sort"
	"strings"
)

// Severity controls exit codes, sort order, and visual treatment.
type Severity string

const (
	Blocker Severity = "BLOCKER" // Will fail the upgrade. Must fix.
	High    Severity = "HIGH"    // Will likely fail or cause downtime. Fix before upgrade.
	Medium  Severity = "MEDIUM"  // May cause issues. Review and fix if applicable.
	Low     Severity = "LOW"     // Cleanup / hygiene. Non-blocking.
	Info    Severity = "INFO"    // Informational only.
)

func (s Severity) Rank() int {
	switch s {
	case Blocker:
		return 0
	case High:
		return 1
	case Medium:
		return 2
	case Low:
		return 3
	case Info:
		return 4
	}
	return 5
}

// Category groups findings in the report.
type Category string

const (
	CategoryAPI         Category = "deprecated-api"
	CategoryFeatureGate Category = "feature-gate"
	CategoryDefault     Category = "default-change"
	CategoryKubelet     Category = "kubelet-flag"
	CategoryKernel      Category = "kernel-runtime"
	CategoryPDB         Category = "pdb-drain"
	CategoryAddon       Category = "addon-compat"
	CategoryWebhook     Category = "webhook"
	CategoryCRD         Category = "crd-storage"
	CategoryVCluster    Category = "vcluster"
	CategoryGitOps      Category = "gitops"
	CategoryBackup      Category = "backup"
)

// Source identifies where the offending object came from.
type Source struct {
	Kind     string // "live", "manifest", "helm-release", "argocd", "flux"
	Location string // file path, helm release name, ArgoCD app, etc.
}

// Object names the offending Kubernetes object (if any).
type Object struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// Owner is the controller (Deployment/StatefulSet/Operator) that
// likely re-emits the offending object after upgrade. Empty if
// the object is user-applied or owner-walking failed.
type Owner struct {
	Kind      string
	Namespace string
	Name      string
	Image     string // controller image:tag
}

// Finding is one discrete issue surfaced by any check.
type Finding struct {
	Severity     Severity
	Category     Category
	Title        string   // one-line summary
	Detail       string   // longer human-readable explanation
	Source       Source   // where we found it
	Object       *Object  // the offending object (optional)
	Owner        *Owner   // controller that owns it (optional)
	Fix          string   // actionable remediation steps
	Docs         []string // reference URLs
	RemovedIn    string   // K8s version where the API/feature is removed
	DeprecatedIn string   // K8s version where deprecated
	Replacement  string   // replacement API/feature
}

// ID is a stable identifier for deduplication across sources.
func (f Finding) ID() string {
	parts := []string{string(f.Category), f.Title}
	if f.Object != nil {
		parts = append(parts, f.Object.APIVersion, f.Object.Kind, f.Object.Namespace, f.Object.Name)
	}
	return strings.Join(parts, "|")
}

// Sort sorts findings by (severity, category, title) for stable display.
func Sort(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity.Rank() != findings[j].Severity.Rank() {
			return findings[i].Severity.Rank() < findings[j].Severity.Rank()
		}
		if findings[i].Category != findings[j].Category {
			return findings[i].Category < findings[j].Category
		}
		return findings[i].Title < findings[j].Title
	})
}

// Dedupe collapses findings with identical IDs, preferring the most
// specific source/owner attribution.
func Dedupe(findings []Finding) []Finding {
	seen := map[string]int{}
	out := []Finding{}
	for _, f := range findings {
		id := f.ID()
		if idx, ok := seen[id]; ok {
			if out[idx].Owner == nil && f.Owner != nil {
				out[idx].Owner = f.Owner
			}
			continue
		}
		seen[id] = len(out)
		out = append(out, f)
	}
	return out
}

// Counts returns counts by severity for summary lines.
func Counts(findings []Finding) map[Severity]int {
	c := map[Severity]int{}
	for _, f := range findings {
		c[f.Severity]++
	}
	return c
}

// HighestRank returns the most severe rank present (lowest number).
func HighestRank(findings []Finding) int {
	if len(findings) == 0 {
		return -1
	}
	best := 99
	for _, f := range findings {
		if f.Severity.Rank() < best {
			best = f.Severity.Rank()
		}
	}
	return best
}

func (o Object) String() string {
	if o.APIVersion == "" {
		return ""
	}
	gvk := o.Kind
	if o.APIVersion != "" {
		gvk = fmt.Sprintf("%s %s", o.APIVersion, o.Kind)
	}
	if o.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", gvk, o.Namespace, o.Name)
	}
	return fmt.Sprintf("%s %s", gvk, o.Name)
}
