package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/sources/live"
	"github.com/saiyam1814/upgrade/internal/ui"
	"github.com/saiyam1814/upgrade/internal/unstick"
)

type unstickOpts struct {
	kubeconfig  string
	contextName string
	namespace   string
	stuckMin    int
	format      string
	autoFix     bool
	execute     bool
	yes         bool
}

func newUnstickCmd() *cobra.Command {
	o := &unstickOpts{}
	cmd := &cobra.Command{
		Use:   "unstick",
		Short: "Detect and remediate a stuck Kubernetes upgrade",
		Long: `unstick walks the cluster for the canonical "stuck upgrade" patterns:

  - Cordoned nodes left over post-drain
  - NotReady nodes (CNI / kubelet)
  - Pods stuck Terminating or Pending past --stuck-min minutes
  - Operator Pods in CrashLoopBackoff
  - PDB-blocked evictions in recent events
  - Webhooks with failurePolicy=Fail (deadlock risk)
  - Namespaces stuck Terminating (finalizer deadlock)

By default this is read-only — it tells you what's stuck and the
exact command to run. Pass --auto-fix --execute to apply the SAFE
class of remediations automatically (e.g., uncordon nodes). All
risky fixes still require per-action confirmation.`,
		Example: `  kubectl upgrade unstick                        # report only
  kubectl upgrade unstick --auto-fix --execute   # apply safe fixes
  kubectl upgrade unstick --namespace argocd     # narrow scope
  kubectl upgrade unstick --stuck-min 1          # be more aggressive`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnstick(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "Limit to a namespace")
	cmd.Flags().IntVar(&o.stuckMin, "stuck-min", 5, "Threshold in minutes for 'stuck'")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().BoolVar(&o.autoFix, "auto-fix", false, "Apply safe automatic fixes (uncordon nodes only)")
	cmd.Flags().BoolVar(&o.execute, "execute", false, "Required with --auto-fix; mutates the cluster")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "Skip per-action confirmation prompts")
	return cmd
}

func runUnstick(ctx context.Context, o *unstickOpts) error {
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	v, _ := client.ServerVersion()
	findings, errs := unstick.Analyze(ctx, client.Core, unstick.Options{
		Namespace:      o.namespace,
		StuckThreshold: time.Duration(o.stuckMin) * time.Minute,
	})
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	finding.Sort(findings)
	header := report.Header{
		Tool:          "kubectl-upgrade",
		ToolVersion:   version,
		Source:        "live",
		SourceVersion: v,
	}
	if err := report.Render(os.Stdout, header, findings, format); err != nil {
		return err
	}

	if !o.autoFix {
		return nil
	}
	if !o.execute {
		ui.Warn(os.Stderr, "--auto-fix is read-only without --execute. Re-run with --execute to apply.")
		return nil
	}
	return runAutoFixes(ctx, client, findings, o.yes)
}

// runAutoFixes applies only the SAFE class of remediations.
//   - Uncordon nodes (always reversible)
//
// All other fixes are emitted as commands; we never mutate without an
// explicit per-action prompt.
func runAutoFixes(ctx context.Context, client *live.Client, findings []finding.Finding, yes bool) error {
	for _, f := range findings {
		if f.Object == nil || f.Object.Kind != "Node" {
			continue
		}
		if !strings.Contains(strings.ToLower(f.Title), "cordoned") {
			continue
		}
		ok := yes
		if !ok {
			ok = ui.Confirm(fmt.Sprintf("Uncordon node %s? [y/N]", f.Object.Name))
		}
		if !ok {
			ui.Info(os.Stderr, "skipped: "+f.Object.Name)
			continue
		}
		if err := uncordonNode(ctx, client, f.Object.Name); err != nil {
			ui.Err(os.Stderr, fmt.Sprintf("uncordon %s failed: %v", f.Object.Name, err))
			continue
		}
		ui.OK(os.Stderr, "uncordoned: "+f.Object.Name)
	}
	return nil
}

func uncordonNode(ctx context.Context, c *live.Client, name string) error {
	patch := []byte(`{"spec":{"unschedulable":false}}`)
	_, err := c.Core.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}
