// Package addons detects installed third-party controllers and
// compares each one against the curated compatibility matrix shipped
// with this binary. The matrix is hand-curated from upstream release
// notes; PRs welcome to extend coverage.
package addons

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
)

// Compat is one row in the matrix: a controller version range,
// the K8s minor range it supports, and the recommended replacement
// when the user's K8s target falls outside its support window.
type Compat struct {
	Name               string   // user-facing addon name
	DetectNamespace    []string // candidate install namespaces
	DetectDeployment   []string // candidate Deployment name patterns
	ImageRegex         *regexp.Regexp
	MinVersion         apis.Semver // controller min ver in this row
	MaxVersion         apis.Semver // controller max ver (exclusive); zero = +inf
	K8sMin             apis.Semver
	K8sMax             apis.Semver // K8s max minor (inclusive)
	Notes              string
	Docs               []string
	RecommendedUpgrade string // "bump to ≥ vX.Y.Z" hint
}

// matrix is the curated compatibility table. Hand-maintained from
// upstream release notes — PRs should append new rows on each release.
//
// IMPORTANT: each addon's rows must be ordered by MinVersion ascending
// for findRow() to pick the right one.
var matrix = []Compat{
	// ---- cert-manager ----
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.13.0", "v1.14.0", "v1.25", "v1.28", "Bump to ≥ v1.14.4 for K8s 1.29+",
		"https://cert-manager.io/docs/installation/upgrade/"),
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.14.0", "v1.15.0", "v1.26", "v1.30", "",
		"https://cert-manager.io/docs/installation/upgrade/"),
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.15.0", "v1.16.0", "v1.27", "v1.31", "",
		"https://cert-manager.io/docs/installation/upgrade/"),
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.16.0", "v1.17.0", "v1.28", "v1.32", "",
		"https://cert-manager.io/docs/installation/upgrade/"),
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.17.0", "v1.18.0", "v1.29", "v1.33", "",
		"https://cert-manager.io/docs/installation/upgrade/"),
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.18.0", "v1.19.0", "v1.29", "v1.33", "",
		"https://cert-manager.io/docs/installation/upgrade/"),
	row("cert-manager", []string{"cert-manager"}, []string{"cert-manager", "cert-manager-controller"},
		`(?i)cert-manager(?:-controller)?:v?(\d+\.\d+\.\d+)`,
		"v1.19.0", "v1.20.0", "v1.30", "v1.34", "v1.19.0 was retracted (mass-renewal bug); use v1.19.1+.",
		"https://cert-manager.io/docs/installation/upgrade/"),

	// ---- karpenter ----
	row("karpenter", []string{"karpenter", "kube-system"}, []string{"karpenter"},
		`(?i)karpenter:v?(\d+\.\d+\.\d+)`,
		"v0.36.0", "v0.37.0", "v1.23", "v1.30", "v0.36 cannot upgrade directly to v1.0 — chain via v0.37.",
		"https://karpenter.sh/docs/upgrading/upgrade-guide/"),
	row("karpenter", []string{"karpenter", "kube-system"}, []string{"karpenter"},
		`(?i)karpenter:v?(\d+\.\d+\.\d+)`,
		"v0.37.0", "v1.0.0", "v1.24", "v1.31", "",
		"https://karpenter.sh/docs/upgrading/upgrade-guide/"),
	row("karpenter", []string{"karpenter", "kube-system"}, []string{"karpenter"},
		`(?i)karpenter:v?(\d+\.\d+\.\d+)`,
		"v1.0.0", "v1.1.0", "v1.25", "v1.32", "",
		"https://karpenter.sh/docs/upgrading/upgrade-guide/"),
	row("karpenter", []string{"karpenter", "kube-system"}, []string{"karpenter"},
		`(?i)karpenter:v?(\d+\.\d+\.\d+)`,
		"v1.1.0", "v1.2.0", "v1.26", "v1.33", "",
		"https://karpenter.sh/docs/upgrading/upgrade-guide/"),

	// ---- istio ----
	row("istio", []string{"istio-system"}, []string{"istiod", "istio-pilot"},
		`(?i)pilot:(\d+\.\d+\.\d+)`,
		"v1.20.0", "v1.21.0", "v1.26", "v1.29", "Single-minor data-plane skew — bump sidecars before control plane.",
		"https://istio.io/latest/news/releases/"),
	row("istio", []string{"istio-system"}, []string{"istiod", "istio-pilot"},
		`(?i)pilot:(\d+\.\d+\.\d+)`,
		"v1.21.0", "v1.22.0", "v1.27", "v1.30", "",
		"https://istio.io/latest/news/releases/"),
	row("istio", []string{"istio-system"}, []string{"istiod", "istio-pilot"},
		`(?i)pilot:(\d+\.\d+\.\d+)`,
		"v1.22.0", "v1.23.0", "v1.28", "v1.31", "",
		"https://istio.io/latest/news/releases/"),
	row("istio", []string{"istio-system"}, []string{"istiod", "istio-pilot"},
		`(?i)pilot:(\d+\.\d+\.\d+)`,
		"v1.23.0", "v1.24.0", "v1.28", "v1.31", "",
		"https://istio.io/latest/news/releases/"),
	row("istio", []string{"istio-system"}, []string{"istiod", "istio-pilot"},
		`(?i)pilot:(\d+\.\d+\.\d+)`,
		"v1.24.0", "v1.25.0", "v1.29", "v1.32", "",
		"https://istio.io/latest/news/releases/"),
	row("istio", []string{"istio-system"}, []string{"istiod", "istio-pilot"},
		`(?i)pilot:(\d+\.\d+\.\d+)`,
		"v1.25.0", "v1.26.0", "v1.30", "v1.33", "",
		"https://istio.io/latest/news/releases/"),

	// ---- argocd ----
	row("argocd", []string{"argocd", "argo-cd"}, []string{"argocd-server"},
		`(?i)argocd:v?(\d+\.\d+\.\d+)`,
		"v2.10.0", "v2.11.0", "v1.25", "v1.29", "",
		"https://argo-cd.readthedocs.io/en/stable/operator-manual/upgrading/"),
	row("argocd", []string{"argocd", "argo-cd"}, []string{"argocd-server"},
		`(?i)argocd:v?(\d+\.\d+\.\d+)`,
		"v2.11.0", "v2.12.0", "v1.26", "v1.30", "",
		"https://argo-cd.readthedocs.io/en/stable/operator-manual/upgrading/"),
	row("argocd", []string{"argocd", "argo-cd"}, []string{"argocd-server"},
		`(?i)argocd:v?(\d+\.\d+\.\d+)`,
		"v2.12.0", "v2.13.0", "v1.27", "v1.31", "",
		"https://argo-cd.readthedocs.io/en/stable/operator-manual/upgrading/"),
	row("argocd", []string{"argocd", "argo-cd"}, []string{"argocd-server"},
		`(?i)argocd:v?(\d+\.\d+\.\d+)`,
		"v2.13.0", "v3.0.0", "v1.28", "v1.32", "",
		"https://argo-cd.readthedocs.io/en/stable/operator-manual/upgrading/"),
	row("argocd", []string{"argocd", "argo-cd"}, []string{"argocd-server"},
		`(?i)argocd:v?(\d+\.\d+\.\d+)`,
		"v3.0.0", "v3.1.0", "v1.29", "v1.33", "",
		"https://argo-cd.readthedocs.io/en/stable/operator-manual/upgrading/"),

	// ---- prometheus-operator ----
	row("prometheus-operator", []string{"monitoring", "prometheus", "kube-prometheus", "openshift-monitoring"}, []string{"prometheus-operator"},
		`(?i)prometheus-operator:v?(\d+\.\d+\.\d+)`,
		"v0.70.0", "v0.71.0", "v1.25", "v1.29", "",
		"https://github.com/prometheus-operator/prometheus-operator/releases"),
	row("prometheus-operator", []string{"monitoring", "prometheus", "kube-prometheus", "openshift-monitoring"}, []string{"prometheus-operator"},
		`(?i)prometheus-operator:v?(\d+\.\d+\.\d+)`,
		"v0.71.0", "v0.75.0", "v1.26", "v1.30", "",
		"https://github.com/prometheus-operator/prometheus-operator/releases"),
	row("prometheus-operator", []string{"monitoring", "prometheus", "kube-prometheus", "openshift-monitoring"}, []string{"prometheus-operator"},
		`(?i)prometheus-operator:v?(\d+\.\d+\.\d+)`,
		"v0.75.0", "v0.80.0", "v1.27", "v1.32", "",
		"https://github.com/prometheus-operator/prometheus-operator/releases"),
	row("prometheus-operator", []string{"monitoring", "prometheus", "kube-prometheus", "openshift-monitoring"}, []string{"prometheus-operator"},
		`(?i)prometheus-operator:v?(\d+\.\d+\.\d+)`,
		"v0.80.0", "v0.85.0", "v1.28", "v1.34", "",
		"https://github.com/prometheus-operator/prometheus-operator/releases"),

	// ---- ingress-nginx ----
	// Special: project retired 2026-03-31. Always emit a HIGH advisory.
	row("ingress-nginx", []string{"ingress-nginx", "kube-system"}, []string{"ingress-nginx-controller", "nginx-ingress-controller"},
		`(?i)(?:ingress-nginx-)?controller:v?(\d+\.\d+\.\d+)`,
		"v0.0.0", "v99.0.0", "v1.0", "v1.99", "ingress-nginx is retired as of 2026-03-31 — migrate to InGate (gateway-api) or another controller.",
		"https://kubernetes.io/blog/2025/11/11/ingress-nginx-retirement/"),

	// ---- flux ----
	row("flux", []string{"flux-system"}, []string{"source-controller"},
		`(?i)source-controller:v?(\d+\.\d+\.\d+)`,
		"v1.3.0", "v1.4.0", "v1.26", "v1.30", "",
		"https://fluxcd.io/flux/installation/upgrade/"),
	row("flux", []string{"flux-system"}, []string{"source-controller"},
		`(?i)source-controller:v?(\d+\.\d+\.\d+)`,
		"v1.4.0", "v1.5.0", "v1.27", "v1.32", "",
		"https://fluxcd.io/flux/installation/upgrade/"),
	row("flux", []string{"flux-system"}, []string{"source-controller"},
		`(?i)source-controller:v?(\d+\.\d+\.\d+)`,
		"v1.5.0", "v2.0.0", "v1.28", "v1.34", "",
		"https://fluxcd.io/flux/installation/upgrade/"),

	// ---- kyverno ----
	row("kyverno", []string{"kyverno"}, []string{"kyverno-admission-controller", "kyverno"},
		`(?i)kyverno:v?(\d+\.\d+\.\d+)`,
		"v1.11.0", "v1.12.0", "v1.26", "v1.29", "",
		"https://kyverno.io/docs/installation/upgrading/"),
	row("kyverno", []string{"kyverno"}, []string{"kyverno-admission-controller", "kyverno"},
		`(?i)kyverno:v?(\d+\.\d+\.\d+)`,
		"v1.12.0", "v1.13.0", "v1.27", "v1.31", "",
		"https://kyverno.io/docs/installation/upgrading/"),
	row("kyverno", []string{"kyverno"}, []string{"kyverno-admission-controller", "kyverno"},
		`(?i)kyverno:v?(\d+\.\d+\.\d+)`,
		"v1.13.0", "v1.14.0", "v1.28", "v1.33", "",
		"https://kyverno.io/docs/installation/upgrading/"),
}

func row(name string, ns, dep []string, imageRegex, minV, maxV, k8sMin, k8sMax, notes, doc string) Compat {
	return Compat{
		Name:             name,
		DetectNamespace:  ns,
		DetectDeployment: dep,
		ImageRegex:       regexp.MustCompile(imageRegex),
		MinVersion:       apis.MustParse(minV),
		MaxVersion:       apis.MustParse(maxV),
		K8sMin:           apis.MustParse(k8sMin),
		K8sMax:           apis.MustParse(k8sMax),
		Notes:            notes,
		Docs:             []string{doc},
	}
}

// Analyze walks Deployments cluster-wide, identifies installed addons,
// matches them against the matrix, and emits findings for any pair
// whose K8s support window does not include the target version.
func Analyze(ctx context.Context, core kubernetes.Interface, target apis.Semver) ([]finding.Finding, []error) {
	deps, err := core.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, []error{fmt.Errorf("permission denied listing Deployments cluster-wide")}
		}
		return nil, []error{fmt.Errorf("list deployments: %w", err)}
	}

	var (
		out  []finding.Finding
		errs []error
	)
	seen := map[string]bool{} // addon-name -> true (avoid dup findings for replicasets etc.)

	for _, d := range deps.Items {
		row, ver, image := matchDeployment(d)
		if row == nil {
			continue
		}
		key := row.Name + "@" + d.Namespace + "/" + d.Name
		if seen[key] {
			continue
		}
		seen[key] = true

		f := evaluateAddon(*row, ver, image, d, target)
		if f != nil {
			out = append(out, *f)
		}
	}
	return out, errs
}

// matchDeployment finds the matrix row whose namespace and deployment
// name pattern match this Deployment, AND whose image-tag-extracted
// version falls in the row's [MinVersion, MaxVersion) range.
func matchDeployment(d appsv1.Deployment) (*Compat, apis.Semver, string) {
	for i := range matrix {
		row := matrix[i]
		if !nsMatches(d.Namespace, row.DetectNamespace) {
			continue
		}
		if !nameMatches(d.Name, row.DetectDeployment) {
			continue
		}
		// Extract version from any container image.
		for _, c := range d.Spec.Template.Spec.Containers {
			m := row.ImageRegex.FindStringSubmatch(c.Image)
			if len(m) < 2 {
				continue
			}
			ver, ok := apis.Parse("v" + m[1])
			if !ok {
				continue
			}
			if !ver.Less(row.MinVersion) && ver.Less(row.MaxVersion) {
				return &row, ver, c.Image
			}
		}
	}
	return nil, apis.Semver{}, ""
}

func nsMatches(ns string, want []string) bool {
	for _, w := range want {
		if ns == w {
			return true
		}
	}
	return false
}

func nameMatches(name string, want []string) bool {
	for _, w := range want {
		if name == w || strings.HasPrefix(name, w) {
			return true
		}
	}
	return false
}

// evaluateAddon decides severity for one (addon, version) detection.
func evaluateAddon(c Compat, ver apis.Semver, image string, d appsv1.Deployment, target apis.Semver) *finding.Finding {
	src := finding.Source{Kind: "live", Location: fmt.Sprintf("apps/v1/deployments/%s/%s", d.Namespace, d.Name)}
	obj := finding.Object{APIVersion: "apps/v1", Kind: "Deployment", Namespace: d.Namespace, Name: d.Name}

	// ingress-nginx is a special-case retirement notice.
	if c.Name == "ingress-nginx" {
		return &finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryAddon,
			Title:    fmt.Sprintf("ingress-nginx detected at %s/%s — project retired 2026-03-31", d.Namespace, d.Name),
			Detail:   c.Notes + " Image: " + image,
			Source:   src,
			Object:   &obj,
			Fix:      "Migrate to InGate (gateway-api implementation) or another ingress controller. ingress-nginx will not receive further security patches.",
			Docs:     c.Docs,
		}
	}

	// Outside the supported K8s window for the target?
	if target.Less(c.K8sMin) || c.K8sMax.Less(target) {
		hint := c.Notes
		if hint == "" {
			hint = "Bump the addon to a release whose support matrix includes K8s " + target.String() + "."
		}
		return &finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryAddon,
			Title:    fmt.Sprintf("%s %s does not support K8s %s (window: %s … %s)", c.Name, ver, target, c.K8sMin, c.K8sMax),
			Detail:   "Image: " + image,
			Source:   src,
			Object:   &obj,
			Fix:      hint,
			Docs:     c.Docs,
		}
	}

	// In-window — emit an INFO so users can see what was detected.
	return &finding.Finding{
		Severity: finding.Info,
		Category: finding.CategoryAddon,
		Title:    fmt.Sprintf("%s %s — supported on K8s %s (window %s … %s)", c.Name, ver, target, c.K8sMin, c.K8sMax),
		Detail:   "Image: " + image,
		Source:   src,
		Object:   &obj,
	}
}
