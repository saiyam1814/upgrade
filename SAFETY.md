# Safety contract

`kubectl-upgrade` is built to be safe to run in production clusters. This document is the contract that governs what the tool will and will not do.

## Tier 1 — read-only (default for every command)

Every `kubectl-upgrade` invocation is read-only unless you explicitly opt into mutation. The default tier:

- Lists Kubernetes resources via the Kubernetes API.
- Reads Helm release secrets.
- Reads cluster events.
- Renders findings to stdout / a file.
- Emits cloud-CLI commands as **text** for you to run.

It does **not**:

- Apply, patch, delete, or create any Kubernetes resource.
- Run any cloud CLI (`aws`, `gcloud`, `az`, `eksctl`, `kubeadm`, `talosctl`, `vcluster`).
- Take backups (Velero, etcd snapshot, vCluster snapshot).
- Drain, cordon, or uncordon nodes.
- Pause GitOps reconciliation.
- Modify webhooks, CRDs, or admission policies.

You can run any default command on a production cluster with no risk.

## Tier 2 — mutating (`--execute` + per-action confirm)

A small number of commands have an `--execute` flag that switches the command into mutation mode. **`--execute` is never the default.** Each mutating action additionally requires a per-action `[y/N]` confirmation unless you explicitly pass `--yes`.

Currently `--execute` is supported only by:

- `kubectl upgrade unstick --auto-fix --execute` — applies the **safe** class of fixes (uncordon nodes only). Every other "fix" is emitted as a command for you to run.

Any future command that mutates the cluster must:

1. Default to read-only.
2. Require an explicit `--execute` flag.
3. Confirm each mutation interactively (`[y/N]`) unless `--yes` is passed.
4. Refuse to prompt and refuse to mutate when stdin is not a TTY *and* `--yes` was not passed.
5. Log the exact action to stderr with timestamp before executing.

## What we will never do

- Run a cloud CLI for you. The blast radius of `aws eks update-cluster-version` is your IAM policy + your billing — it is yours, not ours. We always emit the exact command for you to copy/paste.
- Force-delete a stuck Pod (`--force --grace-period=0`). The risk of double-attached volumes / orphaned data is too high. We surface the command; you run it.
- Modify a webhook's `failurePolicy` or `sideEffects`. The security implications are workload-specific.
- Remove namespace finalizers. Permanent data loss risk.
- Skip pre-flight when an `--execute` step is requested. Pre-flight runs implicitly first.
- Auto-bump operators (cert-manager, Istio, ArgoCD, etc.). The blast radius of an operator upgrade is independent of the K8s upgrade.
- Touch etcd directly.

## Sources of authoritative truth

- Deprecated APIs: vendored snapshot of [`FairwindsOps/pluto`](https://github.com/FairwindsOps/pluto)'s `versions.yaml` (Apache 2.0). Refresh via `make refresh-rules` before each release.
- Feature gates / defaults / kubelet flags / kernel: hand-curated from upstream Kubernetes release notes.
- Addon ↔ K8s compat: hand-curated from each project's release notes.
- vCluster gates: loft.sh docs + release notes.

These are static at build time and do not phone home. The binary makes no network calls except to the Kubernetes API server you point it at.

## Telemetry

There is none. The binary does not phone home, does not collect usage data, and does not contact a remote endpoint.

## Reporting a safety bug

A safety bug is anything that causes `kubectl-upgrade` to mutate cluster state without an explicit `--execute` flag, or that causes the read-only tier to fail in a way that crashes a production cluster. Report via GitHub Issues with the `safety` label, or directly to the repo owner if the bug is exploitable.
