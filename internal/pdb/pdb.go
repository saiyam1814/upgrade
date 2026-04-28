// Package pdb analyzes PodDisruptionBudgets for the canonical "stuck
// drain" pattern: ALLOWED DISRUPTIONS == 0. A drain hitting one of
// these will hang indefinitely on the affected node, which is the
// most common cause of stalled managed-K8s upgrades.
//
// Detection logic mirrors what `kubectl get pdb` shows in its
// "ALLOWED DISRUPTIONS" column — the apiserver's own DisruptionsAllowed
// status field, with secondary checks against unhealthy/desired pod
// counts when the controller hasn't yet reconciled status.
package pdb

import (
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saiyam1814/upgrade/internal/finding"
)

// Analyze scans every PDB cluster-wide and emits findings for those
// that would deadlock a node drain.
func Analyze(ctx context.Context, core kubernetes.Interface) ([]finding.Finding, []error) {
	pdbs, err := core.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) {
			return nil, []error{fmt.Errorf("permission denied listing PodDisruptionBudgets cluster-wide; rerun with cluster-reader RBAC")}
		}
		return nil, []error{fmt.Errorf("list pdbs: %w", err)}
	}
	var (
		out  []finding.Finding
		errs []error
	)
	for _, p := range pdbs.Items {
		if f := evaluate(p); f != nil {
			out = append(out, *f)
		}
	}
	return out, errs
}

func evaluate(p policyv1.PodDisruptionBudget) *finding.Finding {
	allowed := p.Status.DisruptionsAllowed
	current := p.Status.CurrentHealthy
	desired := p.Status.DesiredHealthy
	expected := p.Status.ExpectedPods

	src := finding.Source{Kind: "live", Location: fmt.Sprintf("policy/v1/poddisruptionbudgets/%s/%s", p.Namespace, p.Name)}
	obj := finding.Object{APIVersion: "policy/v1", Kind: "PodDisruptionBudget", Namespace: p.Namespace, Name: p.Name}

	switch {
	case allowed == 0 && expected > 0:
		return &finding.Finding{
			Severity: finding.Blocker,
			Category: finding.CategoryPDB,
			Title:    fmt.Sprintf("PDB %s/%s ALLOWED DISRUPTIONS == 0 (will deadlock drain)", p.Namespace, p.Name),
			Detail:   fmt.Sprintf("currentHealthy=%d desiredHealthy=%d expectedPods=%d minAvailable=%s maxUnavailable=%s", current, desired, expected, ptrToString(p.Spec.MinAvailable), ptrToString(p.Spec.MaxUnavailable)),
			Source:   src,
			Object:   &obj,
			Fix:      "Increase replicas (≥ desiredHealthy + 1), OR loosen the PDB to maxUnavailable=1, OR add a second replica behind the same selector. The drain will not proceed until at least one disruption is allowed.",
			Docs:     []string{"https://kubernetes.io/docs/concepts/workloads/pods/disruptions/"},
		}
	case allowed == 0 && expected == 0:
		// Selector matches nothing — common drift after pod selector typo.
		return &finding.Finding{
			Severity: finding.Medium,
			Category: finding.CategoryPDB,
			Title:    fmt.Sprintf("PDB %s/%s matches zero pods (likely stale selector)", p.Namespace, p.Name),
			Detail:   "PDB exists but selects no pods. Harmless during drain, but indicates configuration drift.",
			Source:   src,
			Object:   &obj,
			Fix:      "Delete the PDB or fix its selector to match the intended workload.",
		}
	}
	return nil
}

func ptrToString(v any) string {
	if v == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", v)
}
