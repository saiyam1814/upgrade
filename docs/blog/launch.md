# I built a kubectl plugin because my Kubernetes upgrades kept breaking in places no tool warned me about

*~1200 words · about 5 min*

Every Kubernetes upgrade I've done in the last year has broken in at least one way that no preflight tool warned me about. Pluto told me my deprecated APIs were fine. Kubent told me my deprecated APIs were fine. The cloud provider's "upgrade insights" told me my deprecated APIs were fine. And then the upgrade started, the drain hit a `PodDisruptionBudget` with `ALLOWED DISRUPTIONS = 0`, and my Tuesday afternoon turned into a Tuesday night.

The pattern repeated enough times that I built one tool to catch them all.

It's called `kubectl-upgrade`, it's open source under Apache 2.0, single static Go binary, and it's at [github.com/saiyam1814/upgrade](https://github.com/saiyam1814/upgrade).

## What's actually missing in 2026

Every existing upgrade tool — pluto, kubent, kubepug, EKS Upgrade Insights, GKE deprecation insights, AKS pre-checks — checks **only what's in etcd plus a recent audit-log window**. EKS = 30 days. AKS = 12 hours. GKE = "calls observed at runtime, not what's in your manifests."

Five things they all miss:

1. **Deprecated APIs in manifests you haven't applied yet** — your Helm charts, your ArgoCD ApplicationSets, your Crossplane compositions
2. **Conversion-webhook certs about to expire** — when this cert dies, every operation on your CRD's resources starts returning 503. Cert-manager itself doesn't proactively monitor this.
3. **Orphan CRDs** — you `helm uninstall`ed an operator three months ago but its CRDs and CRs still exist; their finalizers will deadlock the next namespace delete
4. **PodDisruptionBudgets that will deadlock the drain** — `ALLOWED DISRUPTIONS = 0` is a one-line check no tool runs proactively
5. **Cross-cluster version drift across a fleet** — when you have 50 clusters, "what version is everything on?" is the question every change-window meeting opens with, and nobody surfaces it cleanly

The whole tool exists to close those gaps. Plus the harder one: **once the upgrade starts, you're on your own.** Every existing tool is a pre-flight scanner; nothing actually walks with you through the upgrade and tells you when something's stuck.

## Day-1 flow

```bash
$ curl -sSL https://raw.githubusercontent.com/saiyam1814/upgrade/main/install.sh | sh
```

Once `kubectl-upgrade` is on `$PATH`, kubectl auto-discovers it. No krew install needed; the auto-discovery is built into kubectl.

```bash
# 1. Find what'll break
kubectl upgrade preflight --target v1.34

# 2. Get the runbook for your provider
kubectl upgrade run plan --target v1.34
#    Detects EKS / GKE / AKS / OpenShift / RKE2 / k3s / Talos /
#    kubeadm / Kubespray. Emits the EXACT cloud-CLI command you
#    should run. Never executes — that stays your call.

# 3. (You run the emitted command yourself.)

# 4. While it runs, in another terminal:
kubectl upgrade run watch
#    Catches stuck PDBs, NotReady nodes, helm releases stuck in
#    pending-upgrade, webhooks with failurePolicy=Fail, etc.

# 5. After:
kubectl upgrade run verify --target v1.34
```

There's a TUI (bubbletea) for the visual people:

```bash
$ kubectl upgrade tui --target v1.34
```

It looks like k9s, but for the upgrade flow specifically. Step list on the left, findings on the right. Press `r` on a step to run it.

## The CRD piece — the part nobody else does

CRDs are wild west. Every operator project has its own deprecation policy. There's no central registry.

But there's a piece most operators miss: every CRD already tells you, from inside the cluster.

Look at any CRD's `spec.versions[]`:

```yaml
versions:
- name: v1alpha1
  deprecated: true                       # ← this
  deprecationWarning: "use v1 instead"   # ← this
  served: true
  storage: false
- name: v1
  served: true
  storage: true
```

`deprecated: true` and `deprecationWarning` are first-class fields in `apiextensions.k8s.io/v1`. When a well-behaved operator deprecates a version, they set them. The apiserver even emits warning headers to `kubectl` calls using the deprecated version.

So `kubectl upgrade crds` reads what the cluster already knows — no external feed, no curated list — and flags:

1. **Deprecated CRD versions in use** — read `spec.versions[].deprecated`, count CRs of that type, severity scales with count
2. **Conversion-webhook cert expiry** — decode `spec.conversion.webhook.clientConfig.caBundle`, parse the X.509, compute days-to-expiry. < 0 or < 7 days = BLOCKER, < 30 = HIGH
3. **Orphan CRDs** — owning controller (helm release or `app.kubernetes.io/managed-by`) is gone but CRs still exist

That third one is the silent killer. The controller is gone, but its CRs hold finalizers. The next time someone deletes that namespace, it sits in `Terminating` forever. I've seen this take down a dev cluster for a full afternoon while everyone tried to figure out which finalizer to surgically remove. Now it's a finding before you start the upgrade.

The cert-expiry check is the one I was most excited to ship. cert-manager does most of the auto-rotation, but if cert-manager itself is unhealthy at rotation time, the cert silently expires. When it does, every read/write of every CR of that type returns TLS errors. We literally just decode the base64, parse `NotAfter`, compute the delta. ~50 lines of Go. Nobody else does it because it's tedious, not because it's hard.

## At fleet scale

If you're running ten or fifty clusters, the single most useful command is:

```bash
$ kubectl upgrade fleet drift --all-contexts --target v1.34
```

It fans out across every kubeconfig context in parallel, RBAC-scoped per context (each cluster only sees what its identity is allowed). Output is the one number every change-window meeting starts with:

```
K8s version distribution:
  v1.31  ████████████████████████████  42 (84%)
  v1.30  ███                            5 (10%)
  v1.29  ██                             3 ( 6%)

Top issues across the fleet:
   18 cluster(s)  BLOCKER: policy/v1beta1/PodDisruptionBudget removed in v1.25
   12 cluster(s)  HIGH: cgroup v1 deprecated in kubelet
    7 cluster(s)  HIGH: cert-manager v1.13 (incompatible target)
```

Plus, since I work on vCluster at loft.sh: there's `kubectl upgrade fleet --host-target v1.34 --plan`. It walks every vCluster Tenant Cluster on a host, runs each through a (vCluster band → host K8s window) compatibility matrix, and tells you which tenants must be bumped *before* the host upgrade.

## What's intentionally NOT in here

Some things I deliberately skipped:

- **Cloud-CLI execution.** The tool emits the exact `aws eks update-cluster-version`, `gcloud container clusters upgrade`, `az aks upgrade`, or `kubeadm upgrade apply` command for your provider. It never runs them. The blast radius of a control-plane upgrade is your IAM policy + your billing — that should stay yours, not ours.
- **Telemetry.** None. The binary makes no network calls beyond your kube-apiserver.
- **A SaaS dashboard.** This is a CLI. There's a TUI. There's no web UI. There's no account.
- **Auto-fixes.** With one exception (`unstick --auto-fix --execute` will uncordon a node for you), every fix is emitted as a command for you to run. Read-only is the default; mutations need `--execute` AND a per-action `[y/N]`.

## The roadmap

v0.1.3 is what I described above. v0.2 (issues [#1-#8](https://github.com/saiyam1814/upgrade/issues?q=label%3Aenhancement+is%3Aopen)) adds:

- Storage-version migration check (silent data loss when an old CRD version is removed before all CRs are migrated)
- Removed-in-target K8s version cross-reference for CRDs
- Admission-webhook cert expiry (same code path as conversion webhooks)
- API-service health (aggregated APIs like metrics-server)
- Operator compat matrix expansion to top 30 operators
- Legacy schema-marker advisory

v0.3 has the bigger lifts: synthetic ConversionReview prober, schema diff vs. operator chart.

## Try it

```bash
curl -sSL https://raw.githubusercontent.com/saiyam1814/upgrade/main/install.sh | sh
kubectl upgrade preflight --target v1.34
```

Honest feedback I want most: where does it surface a finding that wasn't real? Where does it MISS a finding that was? File at [github.com/saiyam1814/upgrade/issues](https://github.com/saiyam1814/upgrade/issues) — there's a feedback template that takes ~30 seconds.

If you've broken your K8s upgrade in a way no tool warned you about, that's a v0.2 finding waiting to be added. Tell me about it.

— Saiyam
