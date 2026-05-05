// Package crds implements three CRD-specific upgrade checks that no
// other tool covers today:
//
//  1. Deprecated CRD versions in use — reads spec.versions[].deprecated
//     (a first-class apiextensions.k8s.io/v1 field every well-behaved
//     operator should set) and flags any CR currently using a
//     deprecated version. Pluto/kubent only flag K8s built-in
//     deprecations; they don't read CRD-side metadata.
//
//  2. Conversion-webhook cert expiry — for every CRD using a webhook
//     conversion strategy, base64-decodes the caBundle, parses the
//     X.509 certificate, and computes days-to-expiry. When that cert
//     expires, the apiserver can no longer call the webhook and ALL
//     CR ops on that type return 503/TLS errors. Genuine zero-coverage
//     today; cert-manager itself doesn't proactively monitor this.
//
//  3. Orphan CRDs — CRDs whose owning controller is gone (operator
//     uninstalled, helm release deleted) but CRs of that type still
//     exist. Their finalizers will deadlock on namespace delete; this
//     is the #1 cause of stuck Terminating namespaces. Detected
//     proactively here rather than after you're already stuck.
//
// All three are zero-maintenance — they read what the cluster already
// knows. No external feeds, no curated lists.
package crds

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/saiyam1814/upgrade/internal/finding"
)

// Options narrows the analysis. The default (zero-value) is
// "every CRD, every check, in any namespace."
type Options struct {
	Namespace string // limit CR-existence checks to one namespace; "" = cluster-wide
}

// Analyze runs all three checks and returns findings + per-check errors.
// Errors are non-fatal: one detector failing never aborts the others.
func Analyze(
	ctx context.Context,
	cfg *rest.Config,
	core kubernetes.Interface,
	dyn dynamic.Interface,
	opts Options,
) ([]finding.Finding, []error) {
	apiext, err := apiextclient.NewForConfig(cfg)
	if err != nil {
		return nil, []error{fmt.Errorf("apiextensions client: %w", err)}
	}
	list, err := apiext.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, []error{fmt.Errorf("permission denied listing CustomResourceDefinitions; rerun with cluster-reader RBAC")}
		}
		return nil, []error{fmt.Errorf("list CRDs: %w", err)}
	}

	var (
		out  []finding.Finding
		errs []error
	)

	// Pre-compute installed controllers from helm release secrets so we
	// can attribute each CRD's owner with one bulk query, not N.
	installed := loadInstalledControllers(ctx, core)

	for i := range list.Items {
		crd := &list.Items[i]

		// Skip the apiextensions group itself — it's not a tenant CRD.
		if crd.Spec.Group == "apiextensions.k8s.io" {
			continue
		}

		// Check 1: deprecated served versions actually used by CRs.
		if fs, e := checkDeprecatedVersions(ctx, dyn, crd, opts.Namespace); e != nil {
			errs = append(errs, e)
		} else {
			out = append(out, fs...)
		}

		// Check 2: conversion-webhook cert expiry.
		if f := checkConversionWebhookCert(crd); f != nil {
			out = append(out, *f)
		}

		// Check 3: orphan CRDs (controller gone, CRs exist).
		if fs, e := checkOrphan(ctx, dyn, crd, installed); e != nil {
			errs = append(errs, e)
		} else {
			out = append(out, fs...)
		}
	}

	return out, errs
}

// ---- check 1: deprecated CRD versions ---------------------------------

// checkDeprecatedVersions surfaces every CRD whose served versions
// include a `deprecated: true` entry that still has CRs stored under
// it. Severity scales:
//
//   - any deprecated version + 0 CRs       → INFO (surface but harmless)
//   - any deprecated version + ≥1 CRs      → HIGH (operator wants you off it)
//   - deprecated AND not served            → skipped (the operator already cut it)
func checkDeprecatedVersions(
	ctx context.Context,
	dyn dynamic.Interface,
	crd *apiextv1.CustomResourceDefinition,
	ns string,
) ([]finding.Finding, error) {
	var out []finding.Finding
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if !v.Deprecated || !v.Served {
			continue
		}
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  v.Name,
			Resource: crd.Spec.Names.Plural,
		}
		count, err := countCRs(ctx, dyn, gvr, ns)
		if err != nil {
			// Listing one deprecated version returning Forbidden /
			// NotFound shouldn't fail the whole check.
			if !apierrors.IsForbidden(err) && !apierrors.IsNotFound(err) {
				return out, fmt.Errorf("list %s: %w", gvr, err)
			}
		}
		warn := ""
		if v.DeprecationWarning != nil {
			warn = strings.TrimSpace(*v.DeprecationWarning)
		}
		if warn == "" {
			warn = fmt.Sprintf("%s/%s/%s is deprecated by the operator", crd.Spec.Group, v.Name, crd.Spec.Names.Kind)
		}
		switch {
		case count > 0:
			out = append(out, finding.Finding{
				Severity:     finding.High,
				Category:     finding.CategoryCRD,
				Title:        fmt.Sprintf("CRD %s: %d %s using deprecated version %s", crd.Name, count, crd.Spec.Names.Plural, v.Name),
				Detail:       warn,
				Source:       finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
				Object:       &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
				DeprecatedIn: v.Name,
				Fix:          fmt.Sprintf("Migrate the %d %s to a non-deprecated served version. Then bump the operator (or its CRD definition) to remove this version from spec.versions.", count, crd.Spec.Names.Plural),
			})
		default:
			out = append(out, finding.Finding{
				Severity: finding.Info,
				Category: finding.CategoryCRD,
				Title:    fmt.Sprintf("CRD %s: deprecated version %s is served but unused", crd.Name, v.Name),
				Detail:   warn,
				Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
				Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
				Fix:      "Safe to leave; remove from spec.versions when the operator releases a CRD update.",
			})
		}
	}
	return out, nil
}

// countCRs returns the number of objects of the given GVR in ns
// (cluster-wide if ns == ""). Pagination is intentionally not used —
// the count is the only signal we need.
func countCRs(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns string) (int, error) {
	r := dyn.Resource(gvr)
	if ns != "" {
		l, err := r.Namespace(ns).List(ctx, metav1.ListOptions{Limit: 500})
		if err != nil {
			return 0, err
		}
		return len(l.Items), nil
	}
	l, err := r.List(ctx, metav1.ListOptions{Limit: 500})
	if err != nil {
		return 0, err
	}
	return len(l.Items), nil
}

// ---- check 2: conversion-webhook cert expiry -------------------------

func checkConversionWebhookCert(crd *apiextv1.CustomResourceDefinition) *finding.Finding {
	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextv1.WebhookConverter || conv.Webhook == nil {
		return nil
	}
	caBundle := conv.Webhook.ClientConfig.CABundle
	if len(caBundle) == 0 {
		// CABundle is set lazily by cert-manager's ca-injector; an empty
		// bundle on a webhook strategy is itself a finding (the apiserver
		// will reject the conversion call).
		return &finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryWebhook,
			Title:    fmt.Sprintf("CRD %s conversion webhook has no caBundle (apiserver can't authenticate the webhook)", crd.Name),
			Detail:   "spec.conversion.webhook.clientConfig.caBundle is empty. cert-manager's ca-injector or your installer should populate it. Until it's set, every CR conversion call will fail.",
			Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
			Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
			Fix:      "Verify cert-manager + the cainjector controller are healthy, OR populate caBundle manually with the webhook service's CA cert.",
		}
	}
	// caBundle in apiextensions/v1 is already raw bytes (PEM or DER).
	// Most operators set PEM; we handle both.
	cert, err := parseFirstCert(caBundle)
	if err != nil {
		return &finding.Finding{
			Severity: finding.Medium,
			Category: finding.CategoryWebhook,
			Title:    fmt.Sprintf("CRD %s conversion webhook caBundle could not be parsed", crd.Name),
			Detail:   "Could not extract an X.509 cert from caBundle: " + err.Error(),
			Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
			Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
			Fix:      "Inspect: kubectl get crd " + crd.Name + " -o jsonpath='{.spec.conversion.webhook.clientConfig.caBundle}' | base64 -d | openssl x509 -noout -text",
		}
	}
	now := time.Now()
	left := cert.NotAfter.Sub(now)
	days := int(left.Hours() / 24)

	switch {
	case left < 0:
		return &finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryWebhook,
			Title:    fmt.Sprintf("CRD %s conversion webhook cert EXPIRED %d days ago — every CR op is failing", crd.Name, -days),
			Detail:   fmt.Sprintf("cert NotAfter=%s; subject=%q; issuer=%q. The apiserver cannot call the conversion webhook; reads/writes of CRs of this type return TLS errors.", cert.NotAfter.Format(time.RFC3339), cert.Subject.String(), cert.Issuer.String()),
			Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
			Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
			Fix:      "Re-sign the webhook cert immediately. cert-manager users: bounce cainjector + the issuing Certificate. Otherwise rotate the webhook server's cert and re-populate caBundle.",
		}
	case days < 7:
		return &finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryWebhook,
			Title:    fmt.Sprintf("CRD %s conversion webhook cert expires in %d days", crd.Name, days),
			Detail:   fmt.Sprintf("cert NotAfter=%s; subject=%q. Auto-rotation is unlikely to succeed inside a week if it hasn't already happened.", cert.NotAfter.Format(time.RFC3339), cert.Subject.String()),
			Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
			Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
			Fix:      "Force a cert rotation now. cert-manager: bounce the issuing Certificate. Verify cainjector logs for errors.",
		}
	case days < 30:
		return &finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryWebhook,
			Title:    fmt.Sprintf("CRD %s conversion webhook cert expires in %d days", crd.Name, days),
			Detail:   fmt.Sprintf("cert NotAfter=%s. Most cert-manager rotations happen 30 days before expiry; if it hasn't rotated yet, investigate cainjector.", cert.NotAfter.Format(time.RFC3339)),
			Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
			Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
			Fix:      "Check cert-manager / cainjector health. Verify a renewal is queued: kubectl get certificate -A | grep -i webhook",
		}
	}
	// > 30 days: don't surface — too noisy. We could emit INFO but
	// it'd dwarf the actually-actionable findings.
	return nil
}

// parseFirstCert decodes either a PEM-armored cert or raw DER from
// the caBundle bytes. cert-manager + most installers ship PEM.
func parseFirstCert(b []byte) (*x509.Certificate, error) {
	// Helm sometimes stores caBundle base64'd inside an already-decoded
	// Secret value — try that path first if the bytes look like base64.
	if isAsciiBase64(b) {
		if dec, err := base64.StdEncoding.DecodeString(string(b)); err == nil {
			b = dec
		}
	}
	// Try PEM.
	if block, _ := pem.Decode(b); block != nil && block.Type == "CERTIFICATE" {
		return x509.ParseCertificate(block.Bytes)
	}
	// Fallback: raw DER.
	return x509.ParseCertificate(b)
}

func isAsciiBase64(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' || c == '\n' || c == '\r') {
			return false
		}
	}
	return true
}

// ---- check 3: orphan CRDs --------------------------------------------

// checkOrphan flags CRDs whose owning controller is gone but CRs still
// exist. The owning controller is identified via the standard
// app.kubernetes.io/managed-by label or the helm release annotations
// already on the CRD object.
func checkOrphan(
	ctx context.Context,
	dyn dynamic.Interface,
	crd *apiextv1.CustomResourceDefinition,
	installed map[string]bool,
) ([]finding.Finding, error) {
	owner := identifyOwner(crd)
	if owner == "" {
		// Unlabeled — can't classify; skip rather than false-flag.
		return nil, nil
	}
	if installed[owner] {
		return nil, nil // controller still installed; not orphaned
	}
	// Controller is missing. Are there any CRs?
	storage := storageVersion(crd)
	if storage == "" {
		return nil, nil
	}
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  storage,
		Resource: crd.Spec.Names.Plural,
	}
	count, err := countCRs(ctx, dyn, gvr, "")
	if err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s: %w", gvr, err)
	}
	if count == 0 {
		return nil, nil // CRD is orphaned but harmless without CRs
	}
	return []finding.Finding{{
		Severity: finding.High,
		Category: finding.CategoryCRD,
		Title:    fmt.Sprintf("Orphan CRD %s: managed-by=%q is not installed but %d %s still exist", crd.Name, owner, count, crd.Spec.Names.Plural),
		Detail:   "When you delete a namespace containing these CRs, finalizers will deadlock with no controller to clear them. This is the #1 cause of stuck Terminating namespaces.",
		Source:   finding.Source{Kind: "live", Location: "apiextensions.k8s.io/v1/customresourcedefinitions/" + crd.Name},
		Object:   &finding.Object{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition", Name: crd.Name},
		Fix:      fmt.Sprintf("Either re-install %q OR delete the %d %s before they pin a finalizer (kubectl get %s -A; then remove finalizers if needed).", owner, count, crd.Spec.Names.Plural, crd.Spec.Names.Plural),
	}}, nil
}

func identifyOwner(crd *apiextv1.CustomResourceDefinition) string {
	if v := crd.Labels["app.kubernetes.io/managed-by"]; v != "" {
		// helm-managed: also check the release name annotation
		if name := crd.Annotations["meta.helm.sh/release-name"]; name != "" {
			ns := crd.Annotations["meta.helm.sh/release-namespace"]
			if ns != "" {
				return ns + "/" + name
			}
			return name
		}
		return v
	}
	if v := crd.Annotations["meta.helm.sh/release-name"]; v != "" {
		ns := crd.Annotations["meta.helm.sh/release-namespace"]
		if ns != "" {
			return ns + "/" + v
		}
		return v
	}
	return ""
}

// loadInstalledControllers returns a set of every "owner identifier"
// currently installed in the cluster — keyed by either a helm release
// "ns/name" pair or the bare app.kubernetes.io/managed-by label value
// for non-helm controllers (Deployments / StatefulSets).
func loadInstalledControllers(ctx context.Context, core kubernetes.Interface) map[string]bool {
	out := map[string]bool{}

	// Helm v3 release secrets — owner=helm, type=helm.sh/release.v1.
	secrets, err := core.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: "owner=helm"})
	if err == nil {
		for _, s := range secrets.Items {
			if s.Type != "helm.sh/release.v1" {
				continue
			}
			name := s.Labels["name"]
			if name == "" {
				continue
			}
			out[s.Namespace+"/"+name] = true
			out[name] = true // also match bare name
		}
	}

	// Bare deployments / statefulsets carrying app.kubernetes.io/managed-by.
	deps, err := core.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, d := range deps.Items {
			if v := d.Labels["app.kubernetes.io/managed-by"]; v != "" {
				out[v] = true
			}
		}
	}
	stss, err := core.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, s := range stss.Items {
			if v := s.Labels["app.kubernetes.io/managed-by"]; v != "" {
				out[v] = true
			}
		}
	}
	return out
}

func storageVersion(crd *apiextv1.CustomResourceDefinition) string {
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

// Explain returns the human-readable decision tree for `crds --explain`.
func Explain() string {
	return strings.TrimSpace(`
CRD upgrade decision tree
=========================

For every CustomResourceDefinition on this cluster, the CLI evaluates:

  1. Deprecated CRD versions in use
     - reads spec.versions[].deprecated (a first-class apiextensions/v1 field)
     - HIGH if any CRs exist under a deprecated served version
     - INFO if a deprecated version is served but unused

  2. Conversion-webhook cert expiry
     - decodes spec.conversion.webhook.clientConfig.caBundle, parses X.509
     - BLOCKER if expired (every CR op is already 503'ing)
     - BLOCKER if expires in < 7 days
     - HIGH if expires in < 30 days
     - silent if > 30 days (would be too noisy)

  3. Orphan CRDs
     - CRD's owning controller (managed-by label or helm release) is gone
     - HIGH if CRs still exist (finalizers will deadlock on namespace delete)
     - silent otherwise

This command is read-only. It only reports.
`)
}
