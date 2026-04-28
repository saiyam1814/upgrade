# Changelog

All notable changes to `kubectl-upgrade` are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-04-28

Initial public release.

### Added
- `preflight` — aggregated workload-side pre-flight (scan + simulate + addons + pdb + volumes + vcluster)
- `run plan` — provider detection + emitted cloud-CLI commands (EKS, GKE, AKS, OpenShift, RKE2, k3s, Talos, kubeadm, vCluster)
- `run watch` — in-flight stuck-state monitor
- `run verify` — post-upgrade verification (server version + rescan + smoke)
- `fleet` — vCluster Tenant Cluster wave orchestration
- `scan` — multi-source deprecated-API scanner (manifests + Helm releases + live cluster)
- `simulate` — forward simulator for feature gates, default flips, kubelet flags, kernel reqs (1.25 → 1.36)
- `pdb` — drain-deadlock detector (`ALLOWED DISRUPTIONS == 0`)
- `addons` — cert-manager / Karpenter / Istio / ArgoCD / Flux / prom-operator / Kyverno / ingress-nginx compat matrix
- `volumes` — PV/PVC/CSI/StorageClass safety with in-tree → CSI migration awareness
- `vcluster` — Tenant Cluster decision tree (distro, etcd, topology, version path)
- `unstick` — stuck-state recovery (cordoned nodes, NotReady, stuck Pods, CrashLoopBackoff operators, PDB-blocked evictions, failurePolicy=Fail webhooks, terminating namespaces)
- `plan` — chained one-minor-at-a-time path
- `report` — combined report (human / JSON / Markdown / SARIF)
- `tui` — bubbletea-based visual upgrade dashboard
- Read-only by default; `--execute` + per-action confirm gating for any mutation
- Vendored deprecation rules from `FairwindsOps/pluto` (`make refresh-rules` to bump)

### Safety
- All commands read-only by default
- `unstick --auto-fix` only safe for `uncordon node` mutation; everything else emits commands
- No network egress beyond the Kubernetes API server
- No telemetry
