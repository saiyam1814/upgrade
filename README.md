# kubectl-upgrade

> The missing pre-flight + watch + verify layer that wraps any Kubernetes upgrade.

`kubectl-upgrade` tells you what will break before you start, generates the exact provider-specific upgrade commands for you to run, watches the upgrade in progress (catching stuck PDBs, stalled CRD migrations, addon dependency cycles), tells you how to unstick it, and verifies success after.

It does not run cloud CLIs itself — it makes them safe to run.

```
$ kubectl upgrade preflight --target v1.34
$ kubectl upgrade run plan   --target v1.34   # emits the eksctl/gcloud/az command for YOU to run
$ kubectl upgrade run watch                    # monitors the in-flight upgrade
$ kubectl upgrade run verify --target v1.34    # post-upgrade smoke
$ kubectl upgrade fleet      --host-target v1.34 --plan   # vCluster Tenant Cluster wave
```

## Why

Every existing tool — `pluto`, `kubent`, `kubepug`, every cloud's "upgrade insights" — checks **only what's in etcd plus a recent audit-log window**. None of them inspect the workload supply chain (Git, Helm, ArgoCD/Flux, third-party CRDs, admission webhooks, addon controllers, PV/PVCs) where the actual time bombs live. And once the upgrade starts, you are on your own.

`kubectl-upgrade` covers the whole flow:

| Stage | What it does | Other tools |
|---|---|---|
| **Pre-flight** | Manifests + Helm releases + live cluster + Git + CRDs + webhooks + PV/PVCs + addons + PDBs + vCluster gates | pluto/kubent stop at API removals |
| **Plan** | Detect provider, emit exact cloud-CLI command | none |
| **Watch** | Monitor in-flight upgrade for stuck patterns | none |
| **Unstick** | Detect + remediate stuck states (PDB block, drain hang, CRD stuck, ...) | none |
| **Verify** | Post-upgrade rescan, server version check, smoke | none |
| **Fleet** | vCluster Tenant Cluster wave orchestration | none |

## Install

### curl (recommended)
Downloads the latest release archive and drops `kubectl-upgrade` on your `$PATH`. Once it's there, `kubectl upgrade` works automatically — kubectl auto-discovers `kubectl-*` binaries; no krew required.
```bash
curl -sSL https://raw.githubusercontent.com/saiyam1814/upgrade/main/install.sh | sh
kubectl upgrade --help
```

### From source
```bash
go install github.com/saiyam1814/upgrade@latest
mv $(go env GOBIN)/upgrade $(go env GOBIN)/kubectl-upgrade
```

### Direct download
Grab a tarball/zip from [Releases](https://github.com/saiyam1814/upgrade/releases) and place `kubectl-upgrade` on `$PATH`.

### Coming soon
- **krew** — manifest at `.krew/upgrade.yaml` is ready; PR to `kubernetes-sigs/krew-index` not yet submitted.
- **Homebrew** — formula scaffolded; needs the tap repo + token before re-enabling in GoReleaser.

## The day-1 production flow

```bash
# 1. Find every workload-side time bomb
kubectl upgrade preflight --target v1.34

# 2. Get the exact cloud-CLI command for your provider
kubectl upgrade run plan --target v1.34

# 3. (You manually run the emitted commands.)
#    In a separate terminal, watch for stuck states:
kubectl upgrade run watch

# 4. After the upgrade finishes, verify
kubectl upgrade run verify --target v1.34

# Got stuck? This is the recovery toolkit:
kubectl upgrade unstick

# Have vClusters? Check which tenants are compatible with the
# upcoming host K8s bump BEFORE you bump the host:
kubectl upgrade fleet --host-target v1.34 --plan
```

## Fleet — host × vCluster compat

When you bump a Control Plane Cluster's K8s minor (e.g. EKS 1.32→1.34),
not every vCluster version on it will tolerate the new host. `fleet`
runs a per-tenant check and tells you which tenants need to be bumped
**before** the host upgrade and which are safe.

```bash
kubectl upgrade fleet --host-target v1.34 --plan
```

Output for each tenant — three states:

- ✓ INFO — current vCluster supports the new host K8s. Safe.
- ⚠ MEDIUM — at the upper edge of the support window. Plan a tenant bump after the host.
- ✗ BLOCKER — tenant's vCluster version does NOT support the new host. Bump the tenant first; the runbook tells you the minimum vCluster version to bump to.

The compat table is hand-curated from upstream vCluster release notes; PRs welcome to extend it as new releases land.

## Visual mode

```bash
kubectl upgrade tui --target v1.34
```

A bubbletea dashboard with steps on the left and findings/commands on the right. Like `k9s` for upgrades.

## Commands

| Command | Purpose |
|---|---|
| `preflight` | Aggregate pre-flight: scan + simulate + addons + pdb + volumes + vcluster |
| `run plan` | Emit cloud-CLI commands for control plane + nodes |
| `run watch` | Monitor in-flight upgrade for stuck states |
| `run verify` | Post-upgrade verification |
| `fleet` | vCluster Tenant Cluster wave |
| `scan` | Deprecated APIs in manifests / Helm releases / live cluster |
| `simulate` | Forward sim: feature gates, default flips, kubelet, kernel |
| `pdb` | Drain-deadlock detector |
| `addons` | cert-manager / Istio / Karpenter / ArgoCD / ... compat |
| `volumes` | PV / PVC / CSI / StorageClass safety |
| `vcluster` | Per-Tenant-Cluster decision tree |
| `unstick` | Stuck-state recovery toolkit |
| `plan` | Chained one-minor-at-a-time path |
| `report` | Combined report (Markdown / JSON / SARIF) |
| `tui` | Interactive visual upgrade dashboard |

Run `kubectl upgrade <cmd> --help` for examples and full options.

## Safety

`kubectl-upgrade` is built to be safe to run in production. **Every command is read-only by default.** Mutating actions (currently only `unstick --auto-fix`) require both `--execute` AND a per-action `[y/N]` confirmation.

We never:
- Run cloud CLIs (`aws`, `gcloud`, `az`) for you. We emit the command; you run it.
- Force-delete stuck Pods, modify webhook `failurePolicy`, or remove namespace finalizers.
- Take backups, drain nodes, or pause GitOps reconciliation.
- Phone home. The binary makes no network calls beyond the Kubernetes API server you point it at.

See [SAFETY.md](./SAFETY.md) for the full safety contract.

## Provider coverage

`run plan` knows how to talk about (no execution, just emits commands):

| Provider | Control plane | Node pools |
|---|---|---|
| AWS EKS / EKS Auto | `aws eks update-cluster-version` | `aws eks update-nodegroup-version` |
| GKE / GKE Autopilot | `gcloud container clusters upgrade` | `gcloud container clusters upgrade --node-pool` |
| AKS | `az aks upgrade` | `az aks nodepool upgrade` |
| OpenShift / ROSA | `oc adm upgrade` | (auto via Machine Config Operator) |
| RKE2 / k3s | system-upgrade-controller Plan | system-upgrade-controller Plan |
| Talos | `talosctl upgrade-k8s` | (separate `talosctl upgrade`) |
| kubeadm | `kubeadm upgrade plan` + `apply` | `kubectl drain` + apt + restart kubelet |
| Cluster API | (provider-specific) | (provider-specific) |
| vCluster | `kubectl upgrade vcluster` (per-tenant) | (n/a) |

## Rule data

| Rule type | Source | Coverage |
|---|---|---|
| Deprecated APIs | Vendored snapshot of [`FairwindsOps/pluto`](https://github.com/FairwindsOps/pluto) `versions.yaml` (Apache 2.0) | k8s, cert-manager, istio, prom-operator, several others |
| Feature gates / defaults / kubelet / kernel | Hand-curated from upstream Kubernetes release notes | 1.25 → 1.36 |
| Addon ↔ K8s compat | Hand-curated from each project's release notes | cert-manager, Karpenter, Istio, ArgoCD, Flux, prom-operator, Kyverno, ingress-nginx |
| vCluster decision tree | upstream vCluster docs + release notes | v0.20 → v0.34 |

PRs to extend coverage are welcome.

## Output formats

```bash
kubectl upgrade preflight --target v1.34 --format human      # default, colorized
kubectl upgrade preflight --target v1.34 --format md         # Markdown for PRs
kubectl upgrade preflight --target v1.34 --format json       # CI / scripts
kubectl upgrade preflight --target v1.34 --format sarif      # GitHub code scanning
```

CI gate:
```bash
kubectl upgrade preflight --target v1.34 --fail-on blocker   # exit 2 on any BLOCKER
```

## Build

```bash
make build        # ./bin/kubectl-upgrade
make test
make smoke        # all the headless-safe commands
make refresh-rules # bump pluto's versions.yaml
```

## License

Apache 2.0. The bundled `versions.yaml` snapshot from [`FairwindsOps/pluto`](https://github.com/FairwindsOps/pluto) is also Apache 2.0.
