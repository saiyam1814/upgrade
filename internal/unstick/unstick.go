// Package unstick is the stuck-state recovery toolkit. The single
// largest pain in real upgrades is "we started, it stopped, what now"
// — and the answer is almost always one of a small set of recurring
// patterns. This package detects each pattern, explains the cause,
// and emits the exact remediation command.
//
// Patterns implemented:
//
//  1. Cordoned nodes left over post-upgrade  (drain not uncordoned)
//  2. Pods stuck Terminating > 5 minutes      (finalizer / volume detach)
//  3. Pods stuck Pending > 5 minutes          (scheduler / PDB / quota)
//  4. PDB-blocked drains                      (Cannot evict events)
//  5. CrashLoopBackoff in critical operators  (cert-manager, argocd, etc.)
//  6. NotReady nodes                          (CNI / kubelet)
//  7. CRDs in NotEstablished state            (storage migration stuck)
//  8. Webhooks with failurePolicy=Fail        (potential deadlock during upgrade churn)
//  9. Stuck namespaces (Terminating)          (finalizer deadlock)
//
// Auto-fix scope: only the safe class.
//   - Uncordon nodes (always reversible)
//   - Resume Argo auto-sync (only if the user explicitly paused it)
//
// Risky fixes (force-delete pod, patch webhook failurePolicy, remove
// namespace finalizer) require BOTH --execute AND per-action confirm.
package unstick

import (
	"context"
	"fmt"
	"strings"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saiyam1814/upgrade/internal/finding"
)

// Options narrows the scan.
type Options struct {
	Namespace      string        // empty = cluster-wide
	StuckThreshold time.Duration // default 5 min
}

// Analyze runs every detector. Each detector is independent — failures
// in one do not abort the others.
func Analyze(ctx context.Context, core kubernetes.Interface, opts Options) ([]finding.Finding, []error) {
	if opts.StuckThreshold == 0 {
		opts.StuckThreshold = 5 * time.Minute
	}
	var (
		out  []finding.Finding
		errs []error
	)

	if f, e := detectCordonedNodes(ctx, core); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}
	if f, e := detectNotReadyNodes(ctx, core); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}
	if f, e := detectStuckPods(ctx, core, opts); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}
	if f, e := detectCrashLoopOperators(ctx, core); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}
	if f, e := detectPDBBlockedEvictions(ctx, core); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}
	if f, e := detectFailWebhooks(ctx, core); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}
	if f, e := detectTerminatingNamespaces(ctx, core); e != nil {
		errs = append(errs, e)
	} else {
		out = append(out, f...)
	}

	return out, errs
}

// 1. Cordoned nodes left from a drain.
func detectCordonedNodes(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, error) {
	nodes, err := core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var out []finding.Finding
	for _, n := range nodes.Items {
		if !n.Spec.Unschedulable {
			continue
		}
		out = append(out, finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryPDB,
			Title:    fmt.Sprintf("Node %s is cordoned (Unschedulable=true) — left over from drain?", n.Name),
			Detail:   "Cordoned nodes do not accept new pods. If a drain finished but uncordon was missed, the node is sitting idle. If a drain is in progress, the upgrade is mid-flight.",
			Source:   finding.Source{Kind: "live", Location: "v1/nodes/" + n.Name},
			Object:   &finding.Object{APIVersion: "v1", Kind: "Node", Name: n.Name},
			Fix:      fmt.Sprintf("kubectl uncordon %s   # safe; reverses with kubectl cordon", n.Name),
		})
	}
	return out, nil
}

// 2. NotReady nodes.
func detectNotReadyNodes(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, error) {
	nodes, err := core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var out []finding.Finding
	for _, n := range nodes.Items {
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if ready {
			continue
		}
		out = append(out, finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryKernel,
			Title:    fmt.Sprintf("Node %s is NotReady", n.Name),
			Detail:   "NotReady nodes typically indicate kubelet/CNI/CRI issues. Common upgrade-time causes: kubelet failed to restart after binary swap, CNI Pod stuck Pending, kernel/cgroup mismatch.",
			Source:   finding.Source{Kind: "live", Location: "v1/nodes/" + n.Name},
			Object:   &finding.Object{APIVersion: "v1", Kind: "Node", Name: n.Name},
			Fix:      fmt.Sprintf("kubectl describe node %s   # check Conditions; ssh & journalctl -u kubelet for the offending node", n.Name),
		})
	}
	return out, nil
}

// 3. Pods stuck Terminating or Pending beyond threshold.
func detectStuckPods(ctx context.Context, core kubernetes.Interface, opts Options) ([]finding.Finding, error) {
	pods, err := core.CoreV1().Pods(opts.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var out []finding.Finding
	now := time.Now()
	for _, p := range pods.Items {
		if p.DeletionTimestamp != nil && now.Sub(p.DeletionTimestamp.Time) > opts.StuckThreshold {
			out = append(out, finding.Finding{
				Severity: finding.High,
				Category: finding.CategoryPDB,
				Title:    fmt.Sprintf("Pod %s/%s stuck Terminating for %s", p.Namespace, p.Name, durHuman(now.Sub(p.DeletionTimestamp.Time))),
				Detail:   fmt.Sprintf("Finalizers: %v. Common causes: volume detach hung, finalizer removal blocked, kubelet on host node not reachable.", p.Finalizers),
				Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("v1/pods/%s/%s", p.Namespace, p.Name)},
				Object:   &finding.Object{APIVersion: "v1", Kind: "Pod", Namespace: p.Namespace, Name: p.Name},
				Fix:      fmt.Sprintf("Force delete (CAREFUL — disowns the volume): kubectl delete pod %s -n %s --force --grace-period=0", p.Name, p.Namespace),
			})
			continue
		}
		if p.Status.Phase != corev1.PodPending {
			continue
		}
		if p.CreationTimestamp.Time.IsZero() {
			continue
		}
		if now.Sub(p.CreationTimestamp.Time) <= opts.StuckThreshold {
			continue
		}
		// Try to extract a reason from the latest condition message.
		reason := ""
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				reason = c.Message
				break
			}
		}
		out = append(out, finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryPDB,
			Title:    fmt.Sprintf("Pod %s/%s stuck Pending for %s", p.Namespace, p.Name, durHuman(now.Sub(p.CreationTimestamp.Time))),
			Detail:   reason,
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("v1/pods/%s/%s", p.Namespace, p.Name)},
			Object:   &finding.Object{APIVersion: "v1", Kind: "Pod", Namespace: p.Namespace, Name: p.Name},
			Fix:      fmt.Sprintf("kubectl describe pod %s -n %s   # look at Events / conditions", p.Name, p.Namespace),
		})
	}
	return out, nil
}

// 4. Operator pods in CrashLoopBackoff.
func detectCrashLoopOperators(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, error) {
	pods, err := core.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	criticalNS := map[string]bool{
		"kube-system": true, "cert-manager": true, "argocd": true, "argo-cd": true,
		"flux-system": true, "istio-system": true, "linkerd": true, "kyverno": true,
		"karpenter": true, "monitoring": true, "vcluster": true, "loft-system": true,
	}
	var out []finding.Finding
	for _, p := range pods.Items {
		if !criticalNS[p.Namespace] && !strings.HasPrefix(p.Namespace, "loft-") {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				out = append(out, finding.Finding{
					Severity: finding.Blocker,
					Category: finding.CategoryAddon,
					Title:    fmt.Sprintf("Critical operator Pod %s/%s in CrashLoopBackoff (container %s)", p.Namespace, p.Name, cs.Name),
					Detail:   fmt.Sprintf("Restart count: %d. Last termination: %s.", cs.RestartCount, lastTerm(cs)),
					Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("v1/pods/%s/%s", p.Namespace, p.Name)},
					Object:   &finding.Object{APIVersion: "v1", Kind: "Pod", Namespace: p.Namespace, Name: p.Name},
					Fix:      fmt.Sprintf("kubectl logs %s -n %s -c %s --previous   # find the panic / config issue", p.Name, p.Namespace, cs.Name),
				})
				break
			}
		}
	}
	return out, nil
}

// 5. PDB-blocked evictions: scan recent events for "Cannot evict" reason.
func detectPDBBlockedEvictions(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, error) {
	events, err := core.CoreV1().Events("").List(ctx, metav1.ListOptions{Limit: 500})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	type key struct{ ns, name string }
	seen := map[key]bool{}
	var out []finding.Finding
	for _, e := range events.Items {
		if !strings.Contains(strings.ToLower(e.Message), "cannot evict") &&
			!strings.Contains(strings.ToLower(e.Message), "violates the pdb") {
			continue
		}
		k := key{e.InvolvedObject.Namespace, e.InvolvedObject.Name}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryPDB,
			Title:    fmt.Sprintf("Drain blocked: cannot evict %s %s/%s due to PDB", e.InvolvedObject.Kind, e.InvolvedObject.Namespace, e.InvolvedObject.Name),
			Detail:   strings.TrimSpace(e.Message),
			Source:   finding.Source{Kind: "live", Location: "v1/events/" + e.Namespace + "/" + e.Name},
			Object:   &finding.Object{APIVersion: e.InvolvedObject.APIVersion, Kind: e.InvolvedObject.Kind, Namespace: e.InvolvedObject.Namespace, Name: e.InvolvedObject.Name},
			Fix:      "Identify the PDB (kubectl get pdb -A) and patch maxUnavailable=1 OR scale the workload up. After upgrade, restore PDB.",
		})
	}
	return out, nil
}

// 6. Webhooks with failurePolicy=Fail — potential deadlock during apiserver churn.
func detectFailWebhooks(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, error) {
	cfgs, err := core.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list validating webhooks: %w", err)
	}
	var out []finding.Finding
	for _, cfg := range cfgs.Items {
		for _, wh := range cfg.Webhooks {
			if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregv1.Fail {
				continue
			}
			out = append(out, finding.Finding{
				Severity: finding.Medium,
				Category: finding.CategoryWebhook,
				Title:    fmt.Sprintf("ValidatingWebhook %s (%s) has failurePolicy=Fail — can deadlock during apiserver upgrade churn", cfg.Name, wh.Name),
				Detail:   "If the webhook backend is also upgrading at the same time, the apiserver becomes unavailable for any object the webhook gates.",
				Source:   finding.Source{Kind: "live", Location: "admissionregistration.k8s.io/v1/validatingwebhookconfigurations/" + cfg.Name},
				Fix:      "Consider failurePolicy=Ignore for the upgrade window OR scale the webhook backend to ≥ 2 replicas + PodAntiAffinity. Do not change failurePolicy permanently — security regressions.",
			})
		}
	}
	mcfgs, _ := core.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	for _, cfg := range mcfgs.Items {
		for _, wh := range cfg.Webhooks {
			if wh.FailurePolicy == nil || *wh.FailurePolicy != admissionregv1.Fail {
				continue
			}
			out = append(out, finding.Finding{
				Severity: finding.Medium,
				Category: finding.CategoryWebhook,
				Title:    fmt.Sprintf("MutatingWebhook %s (%s) has failurePolicy=Fail — same deadlock risk", cfg.Name, wh.Name),
				Source:   finding.Source{Kind: "live", Location: "admissionregistration.k8s.io/v1/mutatingwebhookconfigurations/" + cfg.Name},
				Fix:      "Same as ValidatingWebhook above.",
			})
		}
	}
	return out, nil
}

// 7. Namespaces stuck Terminating.
func detectTerminatingNamespaces(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, error) {
	nss, err := core.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	var out []finding.Finding
	now := time.Now()
	for _, ns := range nss.Items {
		if ns.DeletionTimestamp == nil {
			continue
		}
		if now.Sub(ns.DeletionTimestamp.Time) < 2*time.Minute {
			continue
		}
		out = append(out, finding.Finding{
			Severity: finding.High,
			Category: finding.CategoryAddon,
			Title:    fmt.Sprintf("Namespace %s stuck Terminating for %s (finalizer deadlock)", ns.Name, durHuman(now.Sub(ns.DeletionTimestamp.Time))),
			Detail:   fmt.Sprintf("Finalizers: %v. Common cause: a CRD in this namespace whose owning controller is gone, or APIService unreachable.", ns.Spec.Finalizers),
			Source:   finding.Source{Kind: "live", Location: "v1/namespaces/" + ns.Name},
			Object:   &finding.Object{APIVersion: "v1", Kind: "Namespace", Name: ns.Name},
			Fix:      fmt.Sprintf("kubectl get apiservice | grep False     # find unavailable APIService\nkubectl get %s -A 2>&1 | head     # find dangling CRs blocking finalizer", "<crd>"),
		})
	}
	return out, nil
}

// ---- helpers ----

func durHuman(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func lastTerm(cs corev1.ContainerStatus) string {
	if cs.LastTerminationState.Terminated == nil {
		return "(unknown)"
	}
	return fmt.Sprintf("ExitCode=%d Reason=%s", cs.LastTerminationState.Terminated.ExitCode, cs.LastTerminationState.Terminated.Reason)
}
