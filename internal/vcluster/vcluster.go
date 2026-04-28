// Package vcluster implements the loft.sh-recommended pre-upgrade
// decision tree for vCluster Tenant Clusters. The CLI never executes
// mutating operations; it only reports gates and emits the runbook
// commands the operator should run themselves.
//
// Terminology: Tenant Cluster, Control Plane Cluster, Virtual Control
// Plane, Tenant Isolation, AI Cloud — never the legacy terms.
package vcluster

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
)

// Options narrows the analysis.
type Options struct {
	Namespace   string       // limit to a namespace
	ReleaseName string       // limit to one release
	Target      *apis.Semver // upgrade target (vCluster product version, e.g. v0.34)
	HostVersion string       // current Control Plane Cluster apiserver version (gitVersion)
	HostTarget  *apis.Semver // future Control Plane Cluster K8s minor (e.g. v1.34) — checks per-tenant compat with the host bump
}

// Tenant is a discovered vCluster Tenant Cluster on the Control Plane Cluster.
type Tenant struct {
	Namespace    string
	ReleaseName  string
	Version      apis.Semver // vCluster chart version
	Distro       string      // k8s | k3s | k0s | eks (eks gone v0.20)
	Topology     string      // statefulset | deployment
	BackingStore string      // embedded-etcd | external-etcd | sqlite | unknown
}

// Analyze discovers Tenant Clusters and applies the decision tree.
func Analyze(ctx context.Context, core kubernetes.Interface, opts Options) ([]finding.Finding, []error) {
	tenants, errs := Discover(ctx, core, opts.Namespace, opts.ReleaseName)
	if len(tenants) == 0 {
		errs = append(errs, fmt.Errorf("no vCluster Tenant Clusters detected on this Control Plane Cluster"))
		return nil, errs
	}
	var findings []finding.Finding
	for _, t := range tenants {
		findings = append(findings, evaluate(t, opts.Target)...)
		if opts.HostTarget != nil {
			findings = append(findings, evaluateHostUpgrade(t, *opts.HostTarget)...)
		}
	}
	return findings, errs
}

// Discover finds Helm releases whose chart is "vcluster" / "vcluster-k8s".
//
// Helm v3 stores ONE Secret per release REVISION, named
// `sh.helm.release.v1.<release>.v<rev>`. We collapse to one row per
// (namespace, release-name) — keeping only the highest revision —
// before evaluation, otherwise a release that's been upgraded shows
// up N times in fleet output.
func Discover(ctx context.Context, core kubernetes.Interface, ns, name string) ([]Tenant, []error) {
	var errs []error
	secrets, err := core.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "owner=helm",
	})
	if err != nil {
		return nil, []error{fmt.Errorf("list helm secrets: %w", err)}
	}
	// Bucket by (namespace, release-name) → highest-revision secret.
	type bucketKey struct{ ns, rel string }
	type latest struct {
		secret corev1.Secret
		rev    int
	}
	buckets := map[bucketKey]latest{}
	for _, s := range secrets.Items {
		if s.Type != "helm.sh/release.v1" || len(s.Data["release"]) == 0 {
			continue
		}
		relName := s.Labels["name"]
		if relName == "" {
			continue
		}
		rev, _ := strconv.Atoi(s.Labels["version"])
		k := bucketKey{s.Namespace, relName}
		if cur, ok := buckets[k]; !ok || rev > cur.rev {
			buckets[k] = latest{secret: s, rev: rev}
		}
	}

	out := make([]Tenant, 0, len(buckets))
	for _, b := range buckets {
		s := b.secret
		rel, err := decodeRelease(s.Data["release"])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", s.Namespace, s.Name, err))
			continue
		}
		if !isVClusterChart(rel.Chart.Metadata.Name) {
			continue
		}
		if name != "" && rel.Name != name {
			continue
		}
		ver, ok := apis.Parse(rel.Chart.Metadata.Version)
		if !ok {
			ver = apis.Semver{Major: 0, Minor: 0}
		}
		t := Tenant{
			Namespace:    s.Namespace,
			ReleaseName:  rel.Name,
			Version:      ver,
			Distro:       inferDistro(rel.Config),
			Topology:     inferTopology(rel.Config),
			BackingStore: inferBackingStore(rel.Config),
		}
		out = append(out, t)
	}
	return out, errs
}

func isVClusterChart(name string) bool {
	switch name {
	case "vcluster", "vcluster-k8s", "vcluster-k3s", "vcluster-k0s", "vcluster-eks":
		return true
	}
	return false
}

// inferDistro reads .controlPlane.distro.{k8s,k3s,k0s,eks}.enabled or
// the legacy top-level keys. Defaults to k8s.
func inferDistro(cfg map[string]any) string {
	if cp, ok := cfg["controlPlane"].(map[string]any); ok {
		if distro, ok := cp["distro"].(map[string]any); ok {
			for _, name := range []string{"k8s", "k3s", "k0s", "eks"} {
				if d, ok := distro[name].(map[string]any); ok {
					if e, ok := d["enabled"].(bool); ok && e {
						return name
					}
				}
			}
		}
	}
	for _, name := range []string{"k3s", "k0s", "eks"} {
		if cfg[name] != nil {
			return name
		}
	}
	return "k8s"
}

func inferTopology(cfg map[string]any) string {
	if cp, ok := cfg["controlPlane"].(map[string]any); ok {
		if ss, ok := cp["statefulSet"].(map[string]any); ok {
			if e, ok := ss["enabled"].(bool); ok && !e {
				return "deployment"
			}
		}
	}
	return "statefulset"
}

func inferBackingStore(cfg map[string]any) string {
	if cp, ok := cfg["controlPlane"].(map[string]any); ok {
		if bs, ok := cp["backingStore"].(map[string]any); ok {
			if e, ok := bs["etcd"].(map[string]any); ok {
				if emb, ok := e["embedded"].(map[string]any); ok {
					if v, ok := emb["enabled"].(bool); ok && v {
						return "embedded-etcd"
					}
				}
				if ext, ok := e["external"].(map[string]any); ok {
					if v, ok := ext["enabled"].(bool); ok && v {
						return "external-etcd"
					}
				}
				if dep, ok := e["deploy"].(map[string]any); ok {
					if v, ok := dep["enabled"].(bool); ok && v {
						return "deployed-etcd"
					}
				}
			}
		}
	}
	return "unknown"
}

// evaluate runs every gate against a Tenant.
func evaluate(t Tenant, target *apis.Semver) []finding.Finding {
	var out []finding.Finding

	// Gate 1: pre-v0.20 — vcluster.yaml conversion required.
	if t.Version.Major == 0 && t.Version.Minor < 20 {
		out = append(out, finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("Tenant Cluster %s/%s is on vCluster %s — values.yaml format required conversion at v0.20", t.Namespace, t.ReleaseName, t.Version),
			Detail:   "Pre-v0.20 chart values are not loadable by current charts.",
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
			Fix:      "Run: vcluster convert config --distro " + t.Distro + " -f values.yaml > vcluster.yaml  (Helm ≥ 3.10 required)",
			Docs:     []string{"https://www.vcluster.com/docs/vcluster/reference/migrations/0-20-migration"},
		})
	}

	// Gate 2: distro removal gates (only meaningful when target is set).
	if target != nil {
		switch t.Distro {
		case "k3s":
			if !target.Less(apis.MustParse("v0.33")) {
				out = append(out, finding.Finding{
					Severity: finding.Blocker,
					Category: finding.CategoryVCluster,
					Title:    fmt.Sprintf("Tenant Cluster %s/%s uses distro=k3s — removed in vCluster v0.33", t.Namespace, t.ReleaseName),
					Detail:   "k3s distro was deprecated in v0.25 and removed in v0.33; in-place upgrade is unsupported.",
					Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
					Fix:      "Snapshot the Tenant Cluster, delete it, and recreate with distro=k8s from the snapshot. Run: vcluster snapshot create " + t.ReleaseName + " oci://<registry>/<repo>:" + t.ReleaseName + "-pre-distro-migrate",
					Docs:     []string{"https://www.vcluster.com/docs/vcluster/manage/backup-restore"},
				})
			}
		case "k0s":
			if !target.Less(apis.MustParse("v0.26")) {
				out = append(out, finding.Finding{
					Severity: finding.Blocker,
					Category: finding.CategoryVCluster,
					Title:    fmt.Sprintf("Tenant Cluster %s/%s uses distro=k0s — removed in vCluster v0.26", t.Namespace, t.ReleaseName),
					Detail:   "k0s distro was deprecated in v0.25 and removed in v0.26.",
					Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
					Fix:      "Snapshot the Tenant Cluster and recreate with distro=k8s.",
					Docs:     []string{"https://www.vcluster.com/docs/vcluster/manage/backup-restore"},
				})
			}
		case "eks":
			if !target.Less(apis.MustParse("v0.20")) {
				out = append(out, finding.Finding{
					Severity: finding.Blocker,
					Category: finding.CategoryVCluster,
					Title:    fmt.Sprintf("Tenant Cluster %s/%s uses distro=eks — discontinued in vCluster v0.20", t.Namespace, t.ReleaseName),
					Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
					Fix:      "Convert to distro=k8s; swap container images per release notes.",
				})
			}
		}
	}

	// Gate 3: skip-minor refusal — emit chained plan.
	if target != nil {
		if target.Minor-t.Version.Minor > 1 && target.Major == t.Version.Major {
			path := chainedPath(t.Version, *target)
			out = append(out, finding.Finding{
				Severity: finding.Blocker,
				Category: finding.CategoryVCluster,
				Title:    fmt.Sprintf("Skip-minor upgrade %s → %s is unsupported by vCluster (one minor at a time)", t.Version, target),
				Detail:   "Upstream loft.sh policy: minor-version skipping is not actively tested or supported. Operators have hit broken sync state in skip-minor jumps.",
				Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
				Fix:      "Chain through: " + strings.Join(path, " → "),
				Docs:     []string{"https://www.vcluster.com/docs/vcluster/deploy/upgrade/upgrade-version"},
			})
		}
	}

	// Gate 4: etcd 3.5→3.6 transition crossing v0.29.
	if target != nil &&
		t.Version.Less(apis.MustParse("v0.29")) &&
		!target.Less(apis.MustParse("v0.29")) &&
		(t.BackingStore == "embedded-etcd" || t.BackingStore == "deployed-etcd") {
		out = append(out, finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("etcd 3.5 → 3.6 transition required between v0.28 and v0.29 (Tenant %s/%s)", t.Namespace, t.ReleaseName),
			Detail:   "Crossing into v0.29 with embedded/deployed etcd requires a stable etcd-3.5.25 base first; affected ranges (v0.26.0–4, v0.27.0–2, v0.28.0–1) must hop through their .5/.3/.2 patches before bumping minor.",
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
			Fix:      "Hop to a safe-patch version first (v0.26.5 / v0.27.3 / v0.28.2), then upgrade to v0.29+.",
			Docs:     []string{"https://www.vcluster.com/docs/vcluster/learn-how-to/control-plane/container/safely-upgrade-etcd"},
		})
	}

	// Gate 5: Topology safety.
	if t.Topology == "deployment" && t.BackingStore != "external-etcd" {
		out = append(out, finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("Tenant %s/%s — Deployment topology with %s will lose state on rollout", t.Namespace, t.ReleaseName, t.BackingStore),
			Detail:   "Deployment topology has no persistence; the backing store must be external for state to survive an upgrade.",
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
			Fix:      "Switch to StatefulSet topology OR move to an external etcd before upgrading. Distro/backing-store cannot be changed mid-upgrade — snapshot then recreate.",
		})
	}

	// Gate 6: backup reminder (always emit before any mutating upgrade).
	if target != nil && !target.Equal(t.Version) {
		out = append(out, finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryBackup,
			Title:    fmt.Sprintf("Take a snapshot before upgrading Tenant %s/%s", t.Namespace, t.ReleaseName),
			Detail:   "Downgrades are not supported after v0.20. Snapshots are the only revert path.",
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
			Fix:      fmt.Sprintf("vcluster snapshot create %s -n %s oci://<registry>/<repo>:%s-pre-%s", t.ReleaseName, t.Namespace, t.ReleaseName, target),
			Docs:     []string{"https://www.vcluster.com/docs/vcluster/manage/backup-restore"},
		})
	}

	// Gate 7: sleep state — informational reminder only (we don't read sleep status here yet).
	out = append(out, finding.Finding{
		Severity: finding.Info,
		Category: finding.CategoryVCluster,
		Title:    fmt.Sprintf("Tenant %s/%s discovered (vCluster %s, distro=%s, topology=%s, store=%s)", t.Namespace, t.ReleaseName, t.Version, t.Distro, t.Topology, t.BackingStore),
		Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)},
		Detail:   "Ensure the Virtual Control Plane is awake (not sleeping) before running snapshot or upgrade — sleeping Tenants block both operations.",
	})

	return out
}

// HostCompat is one row of the (vCluster minor) × (host K8s minor range)
// support matrix. Hand-curated from upstream vCluster release notes.
// PRs welcome to extend coverage as new vCluster releases land.
type HostCompat struct {
	VClusterMin apis.Semver // vCluster minor (inclusive)
	VClusterMax apis.Semver // vCluster minor (exclusive)
	HostK8sMin  apis.Semver // earliest tested host K8s minor (inclusive)
	HostK8sMax  apis.Semver // latest tested host K8s minor (inclusive)
	Notes       string
}

// hostCompat is the curated compatibility table for vCluster ↔ host K8s.
// Each row covers a vCluster minor band and the host K8s window that band
// has been tested against. Rows must be ordered by VClusterMin ascending.
var hostCompat = []HostCompat{
	{VClusterMin: apis.MustParse("v0.20"), VClusterMax: apis.MustParse("v0.22"), HostK8sMin: apis.MustParse("v1.26"), HostK8sMax: apis.MustParse("v1.30")},
	{VClusterMin: apis.MustParse("v0.22"), VClusterMax: apis.MustParse("v0.24"), HostK8sMin: apis.MustParse("v1.27"), HostK8sMax: apis.MustParse("v1.31")},
	{VClusterMin: apis.MustParse("v0.24"), VClusterMax: apis.MustParse("v0.25"), HostK8sMin: apis.MustParse("v1.28"), HostK8sMax: apis.MustParse("v1.32")},
	{VClusterMin: apis.MustParse("v0.25"), VClusterMax: apis.MustParse("v0.28"), HostK8sMin: apis.MustParse("v1.29"), HostK8sMax: apis.MustParse("v1.33")},
	{VClusterMin: apis.MustParse("v0.28"), VClusterMax: apis.MustParse("v0.31"), HostK8sMin: apis.MustParse("v1.29"), HostK8sMax: apis.MustParse("v1.34")},
	{VClusterMin: apis.MustParse("v0.31"), VClusterMax: apis.MustParse("v0.34"), HostK8sMin: apis.MustParse("v1.30"), HostK8sMax: apis.MustParse("v1.35")},
	{VClusterMin: apis.MustParse("v0.34"), VClusterMax: apis.MustParse("v0.99"), HostK8sMin: apis.MustParse("v1.31"), HostK8sMax: apis.MustParse("v1.36")},
}

// findHostCompat returns the row whose vCluster band covers v.
func findHostCompat(v apis.Semver) *HostCompat {
	for i := range hostCompat {
		if !v.Less(hostCompat[i].VClusterMin) && v.Less(hostCompat[i].VClusterMax) {
			return &hostCompat[i]
		}
	}
	return nil
}

// minVClusterFor returns the lowest vCluster minor that supports the
// given host K8s version. Used to suggest a target tenant version when
// the host bump is going past the current tenant's compat window.
func minVClusterFor(hostK8s apis.Semver) (apis.Semver, bool) {
	for i := range hostCompat {
		row := hostCompat[i]
		if !hostK8s.Less(row.HostK8sMin) && !row.HostK8sMax.Less(hostK8s) {
			return row.VClusterMin, true
		}
	}
	return apis.Semver{}, false
}

// evaluateHostUpgrade emits findings about how a Control Plane Cluster
// K8s bump (--host-target) will affect this Tenant Cluster:
//
//  1. If the tenant's current vCluster version DOESN'T support the new
//     host K8s — emit BLOCKER with a target vCluster version that does.
//  2. If support is right at the edge (host K8s == HostK8sMax of band) —
//     emit MEDIUM advisory to plan a tenant bump after the host.
//  3. INFO line with the discovered (vCluster, host-window) pair.
func evaluateHostUpgrade(t Tenant, hostTarget apis.Semver) []finding.Finding {
	src := finding.Source{Kind: "live", Location: fmt.Sprintf("helm:%s/%s", t.Namespace, t.ReleaseName)}
	row := findHostCompat(t.Version)
	if row == nil {
		return []finding.Finding{{
			Severity: finding.Medium,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("Tenant %s/%s on vCluster %s — host-target %s compat unknown", t.Namespace, t.ReleaseName, t.Version, hostTarget),
			Detail:   "No compat row matches this vCluster version; treat as untested.",
			Source:   src,
			Fix:      "Verify the upstream vCluster release notes for host K8s support for this version.",
		}}
	}

	var out []finding.Finding
	switch {
	case hostTarget.Less(row.HostK8sMin) || row.HostK8sMax.Less(hostTarget):
		// Host target outside the tenant's support window → BLOCKER.
		min, ok := minVClusterFor(hostTarget)
		recommend := "an officially supported vCluster version for this host K8s"
		if ok {
			recommend = "vCluster ≥ " + min.String()
		}
		out = append(out, finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("Tenant %s/%s vCluster %s does NOT support host K8s %s (window %s … %s)", t.Namespace, t.ReleaseName, t.Version, hostTarget, row.HostK8sMin, row.HostK8sMax),
			Detail:   fmt.Sprintf("Bumping the Control Plane Cluster to %s without first bumping the tenant to a compatible vCluster version risks reconciliation drift, sync errors, and admission failures inside the tenant.", hostTarget),
			Source:   src,
			Fix:      fmt.Sprintf("BEFORE bumping the host: upgrade Tenant %s/%s to %s. Then bump the host. Tenant-bump first, host-bump second is the loft-recommended order.", t.Namespace, t.ReleaseName, recommend),
			Docs:     []string{"https://www.vcluster.com/docs/vcluster/manage/upgrade/supported_versions"},
		})
	case hostTarget.Equal(row.HostK8sMax):
		// Host hitting the upper edge of the tenant's window → advisory.
		out = append(out, finding.Finding{
			Severity: finding.Medium,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("Tenant %s/%s vCluster %s — host K8s %s is the upper edge of the supported window (%s … %s)", t.Namespace, t.ReleaseName, t.Version, hostTarget, row.HostK8sMin, row.HostK8sMax),
			Detail:   "Future host bumps will require a tenant vCluster bump first.",
			Source:   src,
			Fix:      "After this host upgrade, plan to bump this tenant to the next vCluster minor band before the next host bump.",
		})
	default:
		out = append(out, finding.Finding{
			Severity: finding.Info,
			Category: finding.CategoryVCluster,
			Title:    fmt.Sprintf("Tenant %s/%s vCluster %s supports host K8s %s ✓ (window %s … %s)", t.Namespace, t.ReleaseName, t.Version, hostTarget, row.HostK8sMin, row.HostK8sMax),
			Source:   src,
		})
	}
	return out
}

func chainedPath(from, to apis.Semver) []string {
	out := []string{}
	for v := from; v.Less(to) || v.Equal(to); v.Minor++ {
		out = append(out, v.String())
	}
	return out
}

// ExplainTree dumps the entire decision tree as documentation.
func ExplainTree() string {
	return strings.TrimSpace(`
vCluster upgrade decision tree
==============================

For every Tenant Cluster on this Control Plane Cluster, the CLI evaluates:

  1. Source < v0.20            → BLOCKER. Run 'vcluster convert config' to migrate
                                 values.yaml → vcluster.yaml (Helm ≥ 3.10).

  2. Distro removed in target  → BLOCKER (no in-place path).
       k3s removed v0.33  (deprecated v0.25)
       k0s removed v0.26  (deprecated v0.25)
       eks discontinued v0.20
     → Snapshot, recreate as distro=k8s from snapshot.

  3. Skip-minor jump           → BLOCKER. Emit chained-version plan.
     Upstream policy: one minor at a time, period.

  4. etcd 3.5 → 3.6 (v0.29)    → HIGH if embedded/deployed etcd and crossing 0.28→0.29.
     Required hops: v0.26.5, v0.27.3, v0.28.2 are safe; v0.26.0–4 / v0.27.0–2 /
     v0.28.0–1 must patch first.

  5. Topology safety           → BLOCKER if topology=deployment AND backing store
     is not external — Virtual Control Plane has no persistence; rollout = data loss.

  6. Backup before mutating    → HIGH reminder. vcluster snapshot create.
     Note: downgrades are not supported after v0.20 — snapshot is the only revert path.

  7. Sleep state               → INFO. Sleeping Tenants block snapshot and upgrade.
     Resume before proceeding.

  8. Platform v3 → v4 (when running under vCluster Platform):
     - reach latest v3.x first
     - preserve projectNamespacePrefix: loft-p-
     - migrate OIDC clients to new CRDs post-upgrade

Mutating actions (snapshot, helm upgrade, vcluster upgrade) are NEVER executed
by this CLI — it only reports gates and emits the runbook commands.
`)
}

// ---- helm release decoding (full release shape: Chart + Config) ----

type releaseShape struct {
	Name   string         `json:"name"`
	Chart  releaseChart   `json:"chart"`
	Config map[string]any `json:"config"`
}

type releaseChart struct {
	Metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"metadata"`
}

// decodeRelease handles the Helm v3 "type=helm.sh/release.v1" Secret payload.
// (We deliberately mirror the parser in internal/sources/live to avoid
// pulling in the full helm.sh/helm/v3 dependency for one struct.)
func decodeRelease(raw []byte) (*releaseShape, error) {
	dec, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	plain, err := gunzip(dec)
	if err != nil {
		return nil, err
	}
	r := &releaseShape{}
	if err := json.Unmarshal(plain, r); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return r, nil
}

func gunzip(in []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, fmt.Errorf("gzip header: %w", err)
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
