// Package cloud detects the upgrade-orchestration target — EKS, GKE,
// AKS, OpenShift / ROSA, RKE2, k3s, Talos, kubeadm self-managed — and
// emits the exact provider CLI command(s) the user should run to bump
// the control plane and node pools.
//
// Detection signals (cheap, no API calls beyond what we already do):
//   - kube-system "kube-root-ca.crt" CA subject (varies by distro)
//   - apiserver gitVersion suffix (-gke, -eks, -aks, etc.)
//   - presence of well-known DaemonSets / Deployments
//
// We deliberately don't run any cloud CLI ourselves; this package just
// recognizes the cluster and produces text the user runs.
package cloud

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Provider identifies which control-plane upgrade flow applies.
type Provider string

const (
	ProviderEKS          Provider = "eks"
	ProviderEKSAuto      Provider = "eks-auto"
	ProviderGKE          Provider = "gke"
	ProviderGKEAutopilot Provider = "gke-autopilot"
	ProviderAKS          Provider = "aks"
	ProviderOpenShift    Provider = "openshift"
	ProviderRancher      Provider = "rancher"
	ProviderRKE2         Provider = "rke2"
	ProviderK3s          Provider = "k3s"
	ProviderTalos        Provider = "talos"
	ProviderKubeadm      Provider = "kubeadm"
	ProviderCAPI         Provider = "cluster-api"
	ProviderVCluster     Provider = "vcluster"
	ProviderUnknown      Provider = "unknown"
)

// Cluster bundles the detection result.
type Cluster struct {
	Provider     Provider
	GitVersion   string
	NodeCount    int
	NodePoolHint string // e.g. EKS managed nodegroup name, GKE node pool, ...
	Region       string // best-effort
	Zone         string // single zone if cluster is zonal; empty for regional
	Notes        []string
}

// Detect inspects the cluster and returns the detection result. Any
// missing signal is silently treated as "not this provider".
func Detect(ctx context.Context, core kubernetes.Interface) (*Cluster, error) {
	srv, err := core.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	c := &Cluster{GitVersion: srv.GitVersion, Provider: ProviderUnknown}

	gv := strings.ToLower(srv.GitVersion)
	switch {
	case strings.Contains(gv, "-eks-"):
		c.Provider = ProviderEKS
	case strings.Contains(gv, "+gke") || strings.Contains(gv, "-gke"):
		c.Provider = ProviderGKE
	case strings.Contains(gv, "-aks") || strings.Contains(gv, "azure"):
		c.Provider = ProviderAKS
	case strings.Contains(gv, "+rke2") || strings.Contains(gv, "-rke2"):
		c.Provider = ProviderRKE2
	case strings.Contains(gv, "+k3s") || strings.Contains(gv, "-k3s"):
		c.Provider = ProviderK3s
	case strings.Contains(gv, "+talos") || strings.Contains(gv, "-talos"):
		c.Provider = ProviderTalos
	case strings.Contains(gv, "openshift") || strings.Contains(gv, "+ocp"):
		c.Provider = ProviderOpenShift
	}

	// Secondary signals — namespaces / well-known deployments.
	if c.Provider == ProviderUnknown {
		c.Provider = detectByNamespaces(ctx, core)
	}

	// Node count + pool hint.
	nodes, err := core.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 200})
	if err == nil {
		c.NodeCount = len(nodes.Items)
		c.NodePoolHint = poolHint(c.Provider, nodes.Items)
		c.Region = regionHint(nodes.Items)
		c.Zone = zoneHint(nodes.Items)
	}

	// vCluster check — the apiserver might *be* a vCluster.
	if isVClusterAPIServer(ctx, core) {
		c.Provider = ProviderVCluster
	}

	return c, nil
}

func detectByNamespaces(ctx context.Context, core kubernetes.Interface) Provider {
	nsList, err := core.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 200})
	if err != nil {
		return ProviderUnknown
	}
	have := map[string]bool{}
	for _, n := range nsList.Items {
		have[n.Name] = true
	}
	switch {
	case have["openshift-apiserver"], have["openshift-cluster-version"]:
		return ProviderOpenShift
	case have["cattle-system"], have["cattle-fleet-system"]:
		return ProviderRancher
	case have["capi-system"], have["capa-system"], have["capz-system"], have["capg-system"], have["capv-system"]:
		return ProviderCAPI
	}
	// Kubeadm: bare-metal / on-prem / kind / minikube — every cluster
	// installed via `kubeadm init` ships a kube-system/kubeadm-config
	// ConfigMap. This is the canonical kubeadm marker.
	if _, err := core.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubeadm-config", metav1.GetOptions{}); err == nil {
		return ProviderKubeadm
	}
	// kops self-managed: drops a kops-controller Deployment in kube-system.
	if _, err := core.AppsV1().Deployments("kube-system").Get(ctx, "kops-controller", metav1.GetOptions{}); err == nil {
		return ProviderKubeadm // kops drives kubeadm under the hood; same runbook applies
	}
	return ProviderUnknown
}

func poolHint(p Provider, nodes []corev1.Node) string {
	for _, n := range nodes {
		switch p {
		case ProviderEKS:
			if v, ok := n.Labels["eks.amazonaws.com/nodegroup"]; ok {
				return v
			}
		case ProviderGKE:
			if v, ok := n.Labels["cloud.google.com/gke-nodepool"]; ok {
				return v
			}
		case ProviderAKS:
			if v, ok := n.Labels["agentpool"]; ok {
				return v
			}
		}
	}
	return ""
}

func regionHint(nodes []corev1.Node) string {
	for _, n := range nodes {
		if v, ok := n.Labels["topology.kubernetes.io/region"]; ok {
			return v
		}
	}
	return ""
}

// zoneHint returns the single zone all nodes share — i.e. the cluster
// is zonal. Returns empty string for regional / multi-zone clusters.
func zoneHint(nodes []corev1.Node) string {
	zone := ""
	for _, n := range nodes {
		v, ok := n.Labels["topology.kubernetes.io/zone"]
		if !ok || v == "" {
			continue
		}
		if zone == "" {
			zone = v
			continue
		}
		if v != zone {
			return "" // mixed zones → regional/multi-zone
		}
	}
	return zone
}

func isVClusterAPIServer(ctx context.Context, core kubernetes.Interface) bool {
	// vCluster sets a "vcluster.loft.sh/managed" label on its own
	// kube-system or has the loft owner annotation. Cheap probe.
	cm, err := core.CoreV1().ConfigMaps("kube-system").Get(ctx, "vcluster-info", metav1.GetOptions{})
	if err == nil && cm != nil {
		return true
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return false
	}
	return false
}

// UpgradeCommands returns the runbook of cloud-CLI commands the user
// must run to perform the upgrade. The CLI emits these — never executes.
type UpgradeCommands struct {
	Provider     Provider
	ControlPlane []string // commands to bump the control plane
	NodePools    []string // commands to bump worker nodes
	PreReqs      []string // any cloud-CLI auth / login the user needs
	Notes        []string // gotchas / context
}

// Plan returns the upgrade runbook for this cluster against a target K8s minor.
func (c *Cluster) Plan(target string) UpgradeCommands {
	cluster := "<cluster-name>"
	region := strOrDefault(c.Region, "<region>")
	pool := strOrDefault(c.NodePoolHint, "<nodegroup-or-nodepool>")

	switch c.Provider {
	case ProviderEKS, ProviderEKSAuto:
		return UpgradeCommands{
			Provider: c.Provider,
			PreReqs: []string{
				"aws sts get-caller-identity",
				"aws eks describe-cluster --name " + cluster + " --region " + region + " --query cluster.version",
			},
			ControlPlane: []string{
				"aws eks update-cluster-version --name " + cluster + " --kubernetes-version " + bareVersion(target) + " --region " + region,
				"aws eks wait cluster-active --name " + cluster + " --region " + region,
			},
			NodePools: []string{
				"# For each managed node group:",
				"aws eks update-nodegroup-version --cluster-name " + cluster + " --nodegroup-name " + pool + " --kubernetes-version " + bareVersion(target) + " --region " + region,
				"aws eks wait nodegroup-active --cluster-name " + cluster + " --nodegroup-name " + pool + " --region " + region,
			},
			Notes: []string{
				"EKS managed node groups respect PodDisruptionBudgets — fix any ALLOWED DISRUPTIONS == 0 PDBs before upgrade.",
				"Force-upgrade is gated by extended-support EOL; you have N+2 minors of grace.",
			},
		}
	case ProviderGKE, ProviderGKEAutopilot:
		// GKE has both zonal and regional clusters with different gcloud
		// flags. Zonal uses --zone us-central1-a; regional uses
		// --region us-central1. Detect via shared-zone heuristic.
		locFlag := "--region " + region
		if c.Zone != "" {
			locFlag = "--zone " + c.Zone
		}
		return UpgradeCommands{
			Provider: c.Provider,
			PreReqs: []string{
				"gcloud auth list",
				"gcloud container clusters describe " + cluster + " " + locFlag + " --format='value(currentMasterVersion)'",
			},
			ControlPlane: []string{
				"gcloud container clusters upgrade " + cluster + " --master --cluster-version=" + bareVersion(target) + " " + locFlag,
			},
			NodePools: []string{
				"# For each node pool:",
				"gcloud container clusters upgrade " + cluster + " --node-pool=" + pool + " --cluster-version=" + bareVersion(target) + " " + locFlag,
			},
			Notes: []string{
				"GKE Autopilot ignores --node-pool — node version follows the control plane automatically.",
				"Set a maintenance window if you don't already have one: gcloud container clusters update " + cluster + " --maintenance-window-start=…",
			},
		}
	case ProviderAKS:
		return UpgradeCommands{
			Provider: c.Provider,
			PreReqs: []string{
				"az account show",
				"az aks get-versions --location " + region + " --output table",
			},
			ControlPlane: []string{
				"az aks upgrade --resource-group <rg> --name " + cluster + " --kubernetes-version " + bareVersion(target) + " --control-plane-only",
			},
			NodePools: []string{
				"# For each agent pool:",
				"az aks nodepool upgrade --resource-group <rg> --cluster-name " + cluster + " --name " + pool + " --kubernetes-version " + bareVersion(target),
			},
			Notes: []string{
				"AKS pre-checks block on PDB and quota issues — `kubectl upgrade pdb` first.",
			},
		}
	case ProviderOpenShift:
		return UpgradeCommands{
			Provider: c.Provider,
			ControlPlane: []string{
				"oc adm upgrade --to=" + bareVersion(target),
				"oc get clusterversion",
			},
			Notes: []string{"OpenShift / ROSA / OKD orchestrate node upgrades automatically via Machine Config Operator."},
		}
	case ProviderRKE2:
		return UpgradeCommands{
			Provider: c.Provider,
			ControlPlane: []string{
				"# Apply a system-upgrade-controller Plan",
				"kubectl apply -f https://raw.githubusercontent.com/rancher/rke2-upgrade/master/server-plan.yaml",
				"kubectl apply -f https://raw.githubusercontent.com/rancher/rke2-upgrade/master/agent-plan.yaml",
			},
			Notes: []string{"Edit each Plan to set --version=" + target + " before applying."},
		}
	case ProviderK3s:
		return UpgradeCommands{
			Provider: c.Provider,
			ControlPlane: []string{
				"# Apply a system-upgrade-controller Plan",
				"kubectl apply -f https://raw.githubusercontent.com/k3s-io/k3s-upgrade/master/server-plan.yaml",
				"kubectl apply -f https://raw.githubusercontent.com/k3s-io/k3s-upgrade/master/agent-plan.yaml",
			},
		}
	case ProviderTalos:
		return UpgradeCommands{
			Provider: c.Provider,
			ControlPlane: []string{
				"talosctl upgrade-k8s --to " + target,
			},
			Notes: []string{"Talos separates OS upgrades (talosctl upgrade) from K8s upgrades (talosctl upgrade-k8s)."},
		}
	case ProviderKubeadm:
		return UpgradeCommands{
			Provider: c.Provider,
			PreReqs: []string{
				"# Confirm the target version is available in your package repo:",
				"#   Debian/Ubuntu: apt-cache madison kubeadm | head",
				"#   RHEL/Rocky:    dnf --showduplicates list kubeadm | tail",
				"sudo kubeadm version",
			},
			ControlPlane: []string{
				"# Pick the path that matches how this cluster was installed:",
				"",
				"# ── Path A: Kubespray (Ansible) — the recommended flow if your cluster came from Kubespray",
				"# From your Kubespray checkout:",
				"ansible-playbook -i inventory/mycluster/hosts.yaml -b -v upgrade-cluster.yml -e kube_version=" + target,
				"# Bumps control plane and workers, drains/uncordons, runs kubeadm under the hood.",
				"",
				"# ── Path B: Other Ansible-driven distros (e.g. RKE-style playbooks) — run your own playbook:",
				"# ansible-playbook upgrade.yml -e k8s_version=" + target,
				"",
				"# ── Path C: Direct kubeadm (vanilla bare-metal, no Ansible)",
				"# 1. FIRST control-plane node — install kubeadm at " + target + ":",
				"#    Debian/Ubuntu: sudo apt-mark unhold kubeadm && sudo apt-get update && sudo apt-get install -y kubeadm=" + bareVersion(target) + ".x-*",
				"#    RHEL/Rocky:    sudo dnf install -y kubeadm-" + bareVersion(target) + ".x --disableexcludes=kubernetes",
				"sudo kubeadm upgrade plan",
				"sudo kubeadm upgrade apply " + target,
				"",
				"# 2. OTHER control-plane nodes (install kubeadm at " + target + " first, same as above):",
				"sudo kubeadm upgrade node",
				"",
				"# 3. On EVERY node (control-plane + worker), refresh kubelet + kubectl, then restart:",
				"sudo systemctl daemon-reload && sudo systemctl restart kubelet",
			},
			NodePools: []string{
				"# Skip this whole section if you used Path A (Kubespray) — the playbook drains and bumps each worker for you.",
				"",
				"# For Path C (direct kubeadm), one worker at a time:",
				"kubectl drain <node> --ignore-daemonsets --delete-emptydir-data",
				"# (install kubelet+kubectl=" + bareVersion(target) + ".x via apt/dnf on the node)",
				"sudo systemctl daemon-reload && sudo systemctl restart kubelet",
				"kubectl uncordon <node>",
			},
			Notes: []string{
				"Detected via kube-system/kubeadm-config ConfigMap. Kubespray, vanilla kubeadm, kops, kind, minikube, and most on-prem installers all produce this artifact.",
				"Skew policy: kubelet may be ≤ N-3 minors behind apiserver from K8s 1.28+.",
				"For HA control planes, run kubeadm sequentially on each CP node — the ConfigMap is updated by the FIRST 'upgrade apply'; subsequent nodes use 'upgrade node'.",
				"Kubespray ref: https://github.com/kubernetes-sigs/kubespray/blob/master/docs/upgrades/upgrades.md",
			},
		}
	case ProviderVCluster:
		return UpgradeCommands{
			Provider: c.Provider,
			Notes: []string{
				"This appears to be a vCluster Tenant Cluster. Use `kubectl upgrade vcluster --target <version>` for the loft.sh upgrade decision tree.",
			},
		}
	}

	// Truly unknown: emit a generic kubeadm-flavoured runbook with a clear
	// caveat. Most bare-metal "unknowns" turn out to be kubeadm under the
	// hood, but we couldn't find the kubeadm-config ConfigMap (RBAC denial,
	// custom installer that wiped it, very old cluster).
	return UpgradeCommands{
		Provider: c.Provider,
		PreReqs: []string{
			"# Provider auto-detection inconclusive. Trying generic kubeadm-style commands.",
			"# If your cluster wasn't installed by kubeadm, consult your distro docs instead.",
			"sudo kubeadm version 2>/dev/null || echo '!! kubeadm not on this host — skip kubeadm path'",
			"kops version 2>/dev/null      || echo '!! kops not on this host — skip kops path'",
		},
		ControlPlane: []string{
			"# OPTION A: kubeadm-managed cluster",
			"sudo kubeadm upgrade plan",
			"sudo kubeadm upgrade apply " + target,
			"",
			"# OPTION B: kops-managed cluster",
			"kops edit cluster   # bump kubernetesVersion to " + target,
			"kops update cluster --yes",
			"kops rolling-update cluster --yes",
			"",
			"# OPTION C: custom installer / Ansible / Terraform",
			"# Bump the version variable in your installer, re-run apply, then:",
			"# kubectl drain each node, install new kubelet+kubectl, restart kubelet, uncordon.",
		},
		NodePools: []string{
			"# After the control plane is upgraded, one node at a time:",
			"kubectl drain <node> --ignore-daemonsets --delete-emptydir-data",
			"# install kubelet+kubectl=" + bareVersion(target) + " via your OS package manager",
			"sudo systemctl daemon-reload && sudo systemctl restart kubelet",
			"kubectl uncordon <node>",
		},
		Notes: []string{
			"Couldn't auto-detect a known provider (no cloud-suffix in gitVersion, no kubeadm-config ConfigMap, no kops-controller, no Rancher / OpenShift / CAPI namespaces).",
			"If you know the provider, the runbook above only suggests the most-common shapes. Adjust to match your setup.",
		},
	}
}

func bareVersion(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	return s
}

func strOrDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
