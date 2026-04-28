// Package featuregates is the curated forward-simulation database
// for upgrade. Each row describes a behavior change — feature-gate
// graduation, default-value flip, kubelet flag removal, or
// runtime/kernel requirement — that activates between two minor
// Kubernetes releases. These are NOT API removals (those live in
// the pluto-derived rules under internal/rules/apis); these are
// the second-order surprises that caught LinkedIn, Reddit, GKE
// users, and AKS pipelines mid-upgrade.
//
// Sources are documented inline. PRs welcome to extend coverage.
package featuregates

import (
	"fmt"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
)

// Rule represents one behavior change that activates at ActivatedIn.
type Rule struct {
	ActivatedIn apis.Semver      // K8s minor where this change takes effect
	Category    finding.Category // feature-gate / default-change / kubelet-flag / kernel-runtime
	Title       string           // one-line summary
	Detail      string           // multi-line why this matters
	Severity    finding.Severity // BLOCKER / HIGH / MEDIUM / LOW
	Fix         string
	Docs        []string
}

// rules is the curated database. Add new rows in chronological order
// (lowest ActivatedIn first) when new minor releases ship.
var rules = []Rule{
	// ---------------- 1.25 ----------------
	{
		ActivatedIn: apis.MustParse("v1.25"),
		Category:    finding.CategoryDefault,
		Severity:    finding.High,
		Title:       "PodSecurityPolicy removed; Pod Security Admission becomes the default enforcement layer",
		Detail:      "PSPs no longer admit pods. Workloads relying on PSP-imposed defaults (privileged, runAsUser, fsGroup) will admit but may fail at runtime. Pod Security Admission labels on namespaces become the recommended path.",
		Fix:         "Audit existing PSPs, recreate equivalent rules as PSA labels (`pod-security.kubernetes.io/enforce`, `audit`, `warn`). Use https://kubernetes.io/docs/tasks/configure-pod-container/migrate-from-psp/ as a checklist.",
		Docs:        []string{"https://kubernetes.io/docs/concepts/security/pod-security-admission/"},
	},
	{
		ActivatedIn: apis.MustParse("v1.25"),
		Category:    finding.CategoryDefault,
		Severity:    finding.Medium,
		Title:       "PodDisruptionBudget empty selector ({}) now selects ALL pods in namespace (was none in v1beta1)",
		Detail:      "v1beta1 PDBs treated empty selector as 'no pods'; v1 treats it as 'all pods'. Stale PDBs that were silently doing nothing may now over-protect a namespace and deadlock drains.",
		Fix:         "Audit any PDB with an empty `spec.selector`. Set explicit labels.",
		Docs:        []string{"https://kubernetes.io/docs/reference/using-api/deprecation-guide/"},
	},

	// ---------------- 1.27 ----------------
	{
		ActivatedIn: apis.MustParse("v1.27"),
		Category:    finding.CategoryKubelet,
		Severity:    finding.Blocker,
		Title:       "kubelet --container-runtime flag removed (dockershim was removed in 1.24, flag was a stub)",
		Detail:      "Any node bootstrap (cloud-init, AMI build, Ignition) that still sets `--container-runtime=remote` or similar will fail kubelet startup.",
		Fix:         "Remove `--container-runtime` from kubelet args. Use `--container-runtime-endpoint` only.",
		Docs:        []string{"https://kubernetes.io/blog/2022/02/17/dockershim-faq/"},
	},
	{
		ActivatedIn: apis.MustParse("v1.27"),
		Category:    finding.CategoryFeatureGate,
		Severity:    finding.Medium,
		Title:       "SeccompDefault feature gate graduated to GA (default ON)",
		Detail:      "Pods without an explicit seccompProfile will get RuntimeDefault. Apps using syscalls outside the default profile (some debuggers, strace, low-level networking) will be denied.",
		Fix:         "Set spec.securityContext.seccompProfile.type=Unconfined for affected pods (NOT recommended) OR audit and ship a custom seccomp profile.",
		Docs:        []string{"https://kubernetes.io/docs/tutorials/security/seccomp/"},
	},

	// ---------------- 1.28 ----------------
	{
		ActivatedIn: apis.MustParse("v1.28"),
		Category:    finding.CategoryFeatureGate,
		Severity:    finding.Medium,
		Title:       "kubelet/kube-apiserver version skew widened to n−3 (was n−2)",
		Detail:      "Older kubelets are now supported against newer apiservers. This is permissive, not breaking — flagged informationally so cluster operators know the policy.",
		Fix:         "No action. Verify kube-proxy is included in your version skew testing.",
		Docs:        []string{"https://kubernetes.io/releases/version-skew-policy/"},
	},

	// ---------------- 1.29 ----------------
	{
		ActivatedIn: apis.MustParse("v1.29"),
		Category:    finding.CategoryFeatureGate,
		Severity:    finding.Medium,
		Title:       "SidecarContainers feature gate graduated to Beta (default ON)",
		Detail:      "Containers with restartPolicy=Always inside initContainers are now first-class sidecars. Operators that pre-rolled their own sidecar pattern (Istio injector, log-tailers) may interact unexpectedly with the native lifecycle.",
		Fix:         "Audit any operator that injects sidecars. Verify the operator version supports K8s 1.29's sidecar lifecycle hooks.",
		Docs:        []string{"https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/"},
	},

	// ---------------- 1.31 ----------------
	{
		ActivatedIn: apis.MustParse("v1.31"),
		Category:    finding.CategoryKernel,
		Severity:    finding.High,
		Title:       "cgroup v1 support deprecated in kubelet (warning only; removal in 1.36)",
		Detail:      "Nodes still running cgroup v1 will log warnings on every kubelet boot. Memory accounting, OOMKill behavior, and CPU throttling are subtly different on cgroup v2 — verify before forcing the switch.",
		Fix:         "Migrate node OS images to cgroup v2 (Ubuntu 22.04+ default; AL2023 default; RHEL 9 default). Test pod-level memory/CPU limits in staging.",
		Docs:        []string{"https://kubernetes.io/docs/concepts/architecture/cgroups/"},
	},
	{
		ActivatedIn: apis.MustParse("v1.31"),
		Category:    finding.CategoryFeatureGate,
		Severity:    finding.Medium,
		Title:       "In-tree-to-CSI volume migration GA for AWS EBS, GCE PD, Azure Disk, Azure File, vSphere",
		Detail:      "PVs annotated with the in-tree provisioner are silently translated to CSI. Workloads pinning to in-tree provisioner names in StorageClass parameters may behave unexpectedly.",
		Fix:         "Verify CSI drivers (ebs.csi.aws.com, pd.csi.storage.gke.io, etc.) are installed on every node before the upgrade.",
		Docs:        []string{"https://kubernetes.io/blog/2022/09/02/cosi-kubernetes-object-storage-management/"},
	},

	// ---------------- 1.32 ----------------
	{
		ActivatedIn: apis.MustParse("v1.32"),
		Category:    finding.CategoryFeatureGate,
		Severity:    finding.Medium,
		Title:       "Scheduler QueueingHints feature gate enabled by default",
		Detail:      "The scheduler now uses requeueing hints to skip irrelevant requeues. Custom schedulers that didn't implement QueueingHint plugins may schedule less efficiently or stall.",
		Fix:         "If you run a custom scheduler, audit its plugin set against the QueueingHint interface.",
		Docs:        []string{"https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/"},
	},

	// ---------------- 1.33 ----------------
	{
		ActivatedIn: apis.MustParse("v1.33"),
		Category:    finding.CategoryFeatureGate,
		Severity:    finding.Medium,
		Title:       "Endpoints v1 deprecated in favor of EndpointSlice",
		Detail:      "Tooling that watches the legacy `core/v1 Endpoints` API still works but operators are nudged to migrate. Some controllers (e.g. older versions of k8ssandra-operator) hit a 63-character name limit on EndpointSlice that doesn't exist on Endpoints — long Service names are silently rejected by the controller.",
		Fix:         "Verify Service names in your namespace are ≤ 63 characters. Bump operators that watch Endpoints to versions that watch EndpointSlice.",
		Docs:        []string{"https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/"},
	},

	// ---------------- 1.34 ----------------
	{
		ActivatedIn: apis.MustParse("v1.34"),
		Category:    finding.CategoryKubelet,
		Severity:    finding.Medium,
		Title:       "Kubelet --cgroup-driver flag deprecated (config-only path going forward)",
		Detail:      "Kubelet's CLI flag for setting the cgroup driver is now warning-only. Move the value into KubeletConfiguration.cgroupDriver.",
		Fix:         "Move `--cgroup-driver` into the kubelet config file under `cgroupDriver: systemd` (or `cgroupfs`).",
		Docs:        []string{"https://kubernetes.io/docs/reference/config-api/kubelet-config.v1/"},
	},

	// ---------------- 1.36 ----------------
	{
		ActivatedIn: apis.MustParse("v1.36"),
		Category:    finding.CategoryKernel,
		Severity:    finding.Blocker,
		Title:       "cgroup v1 support REMOVED from kubelet — nodes on cgroup v1 will not start",
		Detail:      "Following the deprecation in 1.31, kubelet 1.36 refuses to run on a host using the cgroup v1 hierarchy. Mixed-fleet upgrades will hit this on the first old-AMI node.",
		Fix:         "Bake cgroup v2 into every node OS image before bumping the cluster to 1.36. Verify with `stat -fc %T /sys/fs/cgroup` (expects `cgroup2fs`).",
		Docs:        []string{"https://kubernetes.io/docs/concepts/architecture/cgroups/"},
	},
	{
		ActivatedIn: apis.MustParse("v1.36"),
		Category:    finding.CategoryKernel,
		Severity:    finding.High,
		Title:       "containerd 1.6.x and 1.7.x no longer supported as the node CRI",
		Detail:      "1.36 raises the minimum CRI to containerd 2.0+. Older container runtimes will be unable to negotiate the CRI v1 contract.",
		Fix:         "Bump containerd on every node to ≥ 2.0 prior to upgrading kubelet.",
		Docs:        []string{"https://github.com/containerd/containerd/releases"},
	},
}

// Simulate returns every rule whose ActivatedIn is in the half-open
// range (from, to] — i.e., changes the cluster will newly experience
// when bumping from `from` to `to`.
func Simulate(from, to apis.Semver) []finding.Finding {
	var out []finding.Finding
	for _, r := range rules {
		if from.Less(r.ActivatedIn) && (r.ActivatedIn.Less(to) || r.ActivatedIn.Equal(to)) {
			out = append(out, finding.Finding{
				Severity: r.Severity,
				Category: r.Category,
				Title:    fmt.Sprintf("[%s] %s", r.ActivatedIn, r.Title),
				Detail:   r.Detail,
				Source:   finding.Source{Kind: "simulate", Location: r.ActivatedIn.String()},
				Fix:      r.Fix,
				Docs:     r.Docs,
			})
		}
	}
	return out
}

// All returns every rule (used by `upgrade simulate --explain`).
func All() []Rule { return rules }
