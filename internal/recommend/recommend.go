// Package recommend turns a finishing command into a one-line "what
// to run next" suggestion. The whole point: a Kubernetes admin
// shouldn't have to memorize the upgrade flow — the previous
// command's output should hand them the right next call.
//
// Rules are intentionally simple and explicit. Each command's
// finishing handler builds a Context, then NextStep() emits a single
// suggestion. Multiple suggestions are joined with " · " so the user
// sees them inline.
//
// Design principle: be specific. "Run preflight" is bad. "1 BLOCKER:
// fix PDB e2e/redis-pdb-broken (kubectl patch ...) then re-run
// preflight" is good.
package recommend

import (
	"fmt"
	"strings"

	"github.com/saiyam1814/upgrade/internal/finding"
)

// Context bundles everything the engine looks at.
type Context struct {
	Command string // "preflight", "scan", "run plan", "run watch", "run verify",
	// "fleet", "fleet drift", "vcluster", "pdb", "addons",
	// "volumes", "unstick", "plan", "simulate", "report", "tui"
	Target       string // e.g. "v1.34"
	Findings     []finding.Finding
	HasVCluster  bool   // tenants discovered
	NewSinceLast int    // findings new since the previous run of this command
	Provider     string // "gke" / "eks" / "aks" / ... or empty
	WaveID       string // for fleet: in-progress wave ID, if any
	Resumable    bool   // fleet: incomplete wave detected
}

// NextStep returns the recommendation. Empty string = no recommendation.
func NextStep(c Context) string {
	counts := finding.Counts(c.Findings)
	blockers := counts[finding.Blocker]
	highs := counts[finding.High]

	switch c.Command {
	case "preflight":
		if blockers > 0 {
			return preflightBlockerHint(c.Findings)
		}
		if highs > 0 {
			return fmt.Sprintf("Review %d HIGH finding(s); when ready run: kubectl upgrade run plan --target %s", highs, defaultTarget(c.Target))
		}
		return fmt.Sprintf("Looking clean. Next: kubectl upgrade run plan --target %s", defaultTarget(c.Target))

	case "scan":
		if blockers > 0 {
			return "Fix the deprecated apiVersions, then re-run scan. For a richer multi-source check use: kubectl upgrade preflight --target " + defaultTarget(c.Target)
		}
		return "Next: kubectl upgrade preflight --target " + defaultTarget(c.Target) + " (adds PDB, addons, volumes, vcluster checks)"

	case "run plan":
		hint := "Run the emitted commands yourself."
		if c.Provider == "gke" || c.Provider == "eks" || c.Provider == "aks" {
			hint = "In a separate terminal: kubectl upgrade run watch (monitors for stuck states while you run the cloud-CLI command)"
		}
		return hint

	case "run watch":
		return "When the watch settles for 3 cycles: kubectl upgrade run verify --target " + defaultTarget(c.Target)

	case "run verify":
		if blockers > 0 || highs > 0 {
			return "Stuck states post-upgrade. Run: kubectl upgrade unstick"
		}
		return "Upgrade looks landed. " + ifVCluster(c.HasVCluster, "Tenant Clusters next: kubectl upgrade fleet --host-target "+defaultTarget(c.Target)+" --plan", "")

	case "fleet":
		if c.Resumable {
			return fmt.Sprintf("Incomplete wave detected (%s). Resume: kubectl upgrade fleet --resume", c.WaveID)
		}
		if blockers > 0 {
			return "Tenants block the host bump. Resolve BLOCKERs, then re-run with --plan."
		}
		return "Re-run with --plan to emit the per-tenant runbook, or with --vcluster-target X.Y.Z to plan a tenant-version wave."

	case "fleet drift":
		return driftHint(c.Findings)

	case "vcluster":
		if blockers > 0 {
			return "Resolve the per-tenant BLOCKERs first. Use kubectl upgrade vcluster --explain for the full decision tree."
		}
		return "Looks safe. For fleet-wide planning: kubectl upgrade fleet --vcluster-target " + defaultTarget(c.Target) + " --plan"

	case "pdb":
		if blockers > 0 {
			return "Fix the ALLOWED DISRUPTIONS == 0 PDBs (raise replicas or set maxUnavailable=1), then re-run."
		}
		return "PDBs look drainable. Next: kubectl upgrade preflight --target " + defaultTarget(c.Target)

	case "addons":
		if highs > 0 || blockers > 0 {
			return "Bump the flagged addon BEFORE the K8s control-plane upgrade — that's the loft-recommended order."
		}
		return "Addons compatible. Next: kubectl upgrade run plan --target " + defaultTarget(c.Target)

	case "volumes":
		if blockers > 0 {
			return "Install the missing CSI driver(s) BEFORE the upgrade — in-tree provisioners are removed in 1.31+."
		}
		return "Volumes safe. Next: kubectl upgrade preflight --target " + defaultTarget(c.Target)

	case "unstick":
		if blockers > 0 || highs > 0 {
			return "Apply the SAFE fixes: kubectl upgrade unstick --auto-fix --execute (only uncordons nodes; risky fixes still emitted as commands)."
		}
		return "No stuck states. If you're mid-upgrade: kubectl upgrade run watch"

	case "plan":
		return "For each hop, run: kubectl upgrade preflight --target <hop-version>"

	case "simulate":
		return "Pair with: kubectl upgrade preflight --target " + defaultTarget(c.Target) + " (simulate predicts behavior, preflight finds actual broken objects)"

	case "report":
		return "Share this report in the PR / change-window ticket. Next step depends on findings — see the per-section recommendations."
	}
	return ""
}

func preflightBlockerHint(fs []finding.Finding) string {
	// Specialize the hint based on the FIRST blocker's category.
	for _, f := range fs {
		if f.Severity != finding.Blocker {
			continue
		}
		switch f.Category {
		case finding.CategoryPDB:
			return "PDB will deadlock drain. Run: kubectl upgrade unstick (read-only) or fix the PDB directly."
		case finding.CategoryAPI:
			return "Deprecated API in cluster. Run kubectl-convert on offending manifests, then re-run preflight."
		case finding.CategoryVCluster:
			return "Tenant Cluster gate failed. Run: kubectl upgrade vcluster --explain"
		case finding.CategoryDefault:
			return "Volume / CSI risk. Install the missing CSI driver before upgrade."
		case finding.CategoryAddon:
			return "Addon compat issue. Bump the flagged addon to a release that supports the target K8s version."
		}
	}
	return "Fix BLOCKERs before proceeding. Run kubectl upgrade unstick to inspect cluster-side issues."
}

func driftHint(fs []finding.Finding) string {
	// If drift surfaces outliers, the next move is per-cluster preflight.
	for _, f := range fs {
		if f.Severity == finding.Blocker || f.Severity == finding.High {
			return "Outlier clusters detected. Run preflight on the laggards: kubectl upgrade preflight --target <next> --contexts <outlier1,outlier2,...>"
		}
	}
	return "Fleet is consistent. To plan upgrades: kubectl upgrade fleet --host-target <next> --plan"
}

func ifVCluster(yes bool, ifTrue, ifFalse string) string {
	if yes {
		return ifTrue
	}
	return ifFalse
}

func defaultTarget(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return "<target>"
	}
	return t
}
