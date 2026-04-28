package vcluster

import (
	"strings"
	"testing"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
)

func TestEvaluate_K3sRemovedAtTarget(t *testing.T) {
	target := apis.MustParse("v0.34.0")
	tn := Tenant{
		Namespace: "team-a", ReleaseName: "vc-team-a",
		Version:  apis.MustParse("v0.32.0"),
		Distro:   "k3s",
		Topology: "statefulset", BackingStore: "embedded-etcd",
	}
	got := evaluate(tn, &target)
	if !hasBlocker(got, "distro=k3s") {
		t.Fatalf("expected k3s-removal BLOCKER; got %v", titles(got))
	}
}

func TestEvaluate_SkipMinorRefused(t *testing.T) {
	target := apis.MustParse("v0.34.0")
	tn := Tenant{
		Version:      apis.MustParse("v0.30.0"),
		Distro:       "k8s",
		Topology:     "statefulset",
		BackingStore: "embedded-etcd",
	}
	got := evaluate(tn, &target)
	if !hasBlocker(got, "Skip-minor") {
		t.Fatalf("expected skip-minor BLOCKER; got %v", titles(got))
	}
}

func TestEvaluate_TopologySafety(t *testing.T) {
	tn := Tenant{
		Version:      apis.MustParse("v0.33.0"),
		Distro:       "k8s",
		Topology:     "deployment",
		BackingStore: "embedded-etcd",
	}
	got := evaluate(tn, nil)
	if !hasBlocker(got, "Deployment topology") {
		t.Fatalf("expected topology BLOCKER; got %v", titles(got))
	}
}

func TestEvaluate_HappyPath_OneMinor_K8sDistro(t *testing.T) {
	target := apis.MustParse("v0.34.0")
	tn := Tenant{
		Version:      apis.MustParse("v0.33.0"),
		Distro:       "k8s",
		Topology:     "statefulset",
		BackingStore: "external-etcd",
	}
	got := evaluate(tn, &target)
	if hasSeverity(got, finding.Blocker) {
		t.Fatalf("expected no BLOCKERs on a clean one-minor k8s upgrade; got %v", titles(got))
	}
}

func TestEvaluate_EtcdTransition_v0_29(t *testing.T) {
	target := apis.MustParse("v0.29.0")
	tn := Tenant{
		Version:      apis.MustParse("v0.28.0"),
		Distro:       "k8s",
		Topology:     "statefulset",
		BackingStore: "embedded-etcd",
	}
	got := evaluate(tn, &target)
	want := "etcd 3.5 → 3.6"
	if !hasContains(got, finding.High, want) {
		t.Fatalf("expected HIGH containing %q; got %v", want, titles(got))
	}
}

// ---- helpers ----

func hasBlocker(fs []finding.Finding, sub string) bool {
	return hasContains(fs, finding.Blocker, sub)
}

func hasContains(fs []finding.Finding, sev finding.Severity, sub string) bool {
	for _, f := range fs {
		if f.Severity == sev && strings.Contains(f.Title, sub) {
			return true
		}
	}
	return false
}

func hasSeverity(fs []finding.Finding, sev finding.Severity) bool {
	for _, f := range fs {
		if f.Severity == sev {
			return true
		}
	}
	return false
}

func titles(fs []finding.Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, string(f.Severity)+":"+f.Title)
	}
	return out
}
