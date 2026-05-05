package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/saiyam1814/upgrade/internal/crds"
	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/recommend"
	"github.com/saiyam1814/upgrade/internal/report"
	"github.com/saiyam1814/upgrade/internal/sources/live"
)

type crdsOpts struct {
	kubeconfig  string
	contextName string
	namespace   string
	format      string
	failOn      string
	explain     bool
}

func newCRDsCmd() *cobra.Command {
	o := &crdsOpts{}
	cmd := &cobra.Command{
		Use:   "crds",
		Short: "CRD-specific upgrade safety: deprecated versions, webhook cert expiry, orphan CRDs",
		Long: `crds runs three checks no other tool covers today:

  1. Deprecated CRD versions in use
     Reads spec.versions[].deprecated on every CRD; flags CRs still using
     a deprecated version.

  2. Conversion-webhook cert expiry
     Decodes spec.conversion.webhook.clientConfig.caBundle, parses the
     X.509 cert, computes days-to-expiry. When that cert expires, every
     CR op of that type starts returning 503.

  3. Orphan CRDs
     CRDs whose owning controller is gone but CRs still exist — the
     #1 cause of stuck Terminating namespaces during upgrade cleanup.

All three are read-only and zero-maintenance — they read what the cluster
already knows. Use --explain for the full decision tree.`,
		Example: `  # Audit every CRD on the cluster
  kubectl upgrade crds

  # Limit to a single namespace
  kubectl upgrade crds --namespace argocd

  # CI gate — fail on BLOCKER (expired webhook cert, etc.)
  kubectl upgrade crds --fail-on blocker

  # Decision tree
  kubectl upgrade crds --explain`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.explain {
				fmt.Println(crds.Explain())
				return nil
			}
			return runCRDs(cmd.Context(), o)
		},
	}
	cmd.Flags().StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig")
	cmd.Flags().StringVar(&o.contextName, "context", "", "Kubeconfig context name")
	cmd.Flags().StringVarP(&o.namespace, "namespace", "n", "", "Limit CR-existence checks to one namespace")
	cmd.Flags().StringVar(&o.format, "format", "human", "Output format: human|json|md|sarif")
	cmd.Flags().StringVar(&o.failOn, "fail-on", "blocker", "Exit non-zero on findings ≥ blocker|high|medium|low|none")
	cmd.Flags().BoolVar(&o.explain, "explain", false, "Print the full decision tree instead of running it")
	return cmd
}

func runCRDs(ctx context.Context, o *crdsOpts) error {
	format, err := report.ParseFormat(o.format)
	if err != nil {
		return err
	}
	client, err := live.Connect(o.kubeconfig, o.contextName)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	v, _ := client.ServerVersion()

	findings, errs := crds.Analyze(ctx, client.RESTConfig(), client.Core, client.Dyn, crds.Options{
		Namespace: o.namespace,
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
	emitRecommendation(format, recommend.Context{
		Command:  "crds",
		Findings: findings,
	})
	return failOnExit(findings, o.failOn)
}
