package recommend

import (
	"strings"
	"testing"

	"github.com/saiyam1814/upgrade/internal/finding"
)

func mkFinding(sev finding.Severity, cat finding.Category, title string) finding.Finding {
	return finding.Finding{Severity: sev, Category: cat, Title: title}
}

func TestNextStep_PreflightClean(t *testing.T) {
	got := NextStep(Context{Command: "preflight", Target: "v1.34"})
	if !strings.Contains(got, "kubectl upgrade run plan --target v1.34") {
		t.Errorf("preflight clean should suggest run plan; got %q", got)
	}
}

func TestNextStep_PreflightWithPDBBlocker(t *testing.T) {
	fs := []finding.Finding{mkFinding(finding.Blocker, finding.CategoryPDB, "PDB stuck")}
	got := NextStep(Context{Command: "preflight", Target: "v1.34", Findings: fs})
	if !strings.Contains(got, "PDB will deadlock drain") {
		t.Errorf("PDB blocker hint missing; got %q", got)
	}
}

func TestNextStep_PreflightWithAPIBlocker(t *testing.T) {
	fs := []finding.Finding{mkFinding(finding.Blocker, finding.CategoryAPI, "deprecated API")}
	got := NextStep(Context{Command: "preflight", Target: "v1.34", Findings: fs})
	if !strings.Contains(got, "kubectl-convert") {
		t.Errorf("API blocker should suggest kubectl-convert; got %q", got)
	}
}

func TestNextStep_PreflightVClusterFollowOn(t *testing.T) {
	got := NextStep(Context{Command: "run verify", Target: "v1.34", HasVCluster: true})
	if !strings.Contains(got, "fleet --host-target v1.34") {
		t.Errorf("post-verify w/ vcluster should suggest fleet host-target; got %q", got)
	}
}

func TestNextStep_RunPlanGKE(t *testing.T) {
	got := NextStep(Context{Command: "run plan", Provider: "gke", Target: "v1.34"})
	if !strings.Contains(got, "run watch") {
		t.Errorf("run plan on cloud provider should suggest run watch; got %q", got)
	}
}

func TestNextStep_RunPlanKubeadm(t *testing.T) {
	// Non-cloud provider falls through to the generic hint.
	got := NextStep(Context{Command: "run plan", Provider: "kubeadm", Target: "v1.34"})
	if !strings.Contains(strings.ToLower(got), "run the emitted commands") {
		t.Errorf("non-cloud run plan should remind to run emitted cmds; got %q", got)
	}
}

func TestNextStep_FleetDriftWithOutliers(t *testing.T) {
	fs := []finding.Finding{mkFinding(finding.High, finding.CategoryAPI, "outlier")}
	got := NextStep(Context{Command: "fleet drift", Findings: fs})
	if !strings.Contains(got, "preflight on the laggards") {
		t.Errorf("drift w/ outliers should suggest preflight on laggards; got %q", got)
	}
}

func TestNextStep_FleetDriftClean(t *testing.T) {
	got := NextStep(Context{Command: "fleet drift"})
	if !strings.Contains(got, "consistent") {
		t.Errorf("clean drift should call out fleet consistency; got %q", got)
	}
}

func TestNextStep_VClusterClean(t *testing.T) {
	got := NextStep(Context{Command: "vcluster", Target: "v0.34.0"})
	if !strings.Contains(got, "fleet --vcluster-target v0.34.0 --plan") {
		t.Errorf("clean vcluster should chain to fleet plan; got %q", got)
	}
}

func TestNextStep_UnknownCommand(t *testing.T) {
	got := NextStep(Context{Command: "wat"})
	if got != "" {
		t.Errorf("unknown command should return empty string; got %q", got)
	}
}

func TestNextStep_DefaultTargetPlaceholder(t *testing.T) {
	// No target supplied → "<target>" placeholder.
	got := NextStep(Context{Command: "preflight"})
	if !strings.Contains(got, "<target>") {
		t.Errorf("missing target should produce <target> placeholder; got %q", got)
	}
}
