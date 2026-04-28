# Contributing

Thanks for your interest. The fastest way to help is to extend the curated rule data — that's where the long tail of value is.

## Rule data — where to add new entries

| Rule type | File | When to update |
|---|---|---|
| Deprecated APIs | `internal/rules/apis/versions.yaml` (vendored from pluto) | Run `make refresh-rules` to pull the latest snapshot |
| Feature gates / defaults / kubelet / kernel | `internal/rules/featuregates/featuregates.go` | When a new K8s minor lands or a new behavior change is documented |
| Addon ↔ K8s compat | `internal/addons/addons.go` (`matrix` slice) | Each new release of cert-manager / Karpenter / Istio / ArgoCD / Flux / Kyverno / etc. |
| vCluster gates | `internal/vcluster/vcluster.go` (`evaluate` func) | Each vCluster release with new gates |
| Cloud provider commands | `internal/cloud/cloud.go` (`Plan` func) | New providers or CLI changes |
| Stuck-state patterns | `internal/unstick/unstick.go` | New corner cases we encounter in postmortems |
| Volume / CSI migrations | `internal/volumes/volumes.go` (`migrations` slice) | New in-tree → CSI removals |

## Code style

- Standard `gofmt`. CI rejects unformatted code.
- `go vet ./...` must pass.
- New checks add a new test in the relevant package.
- Prefer extending an existing struct over adding a new one.
- Comment the **why**, not the **what**. Identifiers should describe the what.

## Safety changes

Anything that touches the safety contract (`SAFETY.md`) requires:
- A new test that proves the read-only default holds.
- A note in `CHANGELOG.md`.

Safety regressions are blocking.

## Local dev

```bash
make build
make test
./bin/kubectl-upgrade preflight --target v1.34   # against your default kubeconfig
```

For TUI changes:
```bash
go run . tui --target v1.34
```

## Adding a new addon to the compat matrix

1. Find the addon's release notes / compat doc.
2. For each row of (controller version range × supported K8s minor range), append a `Compat` entry to `matrix`.
3. Detect the addon: pick a stable namespace + Deployment name pattern + image regex.
4. Add a brief test if your detection logic is non-trivial.

## Releasing

Tags follow semver: `vMAJOR.MINOR.PATCH`. GoReleaser handles the rest:
```bash
git tag v0.1.0
git push origin v0.1.0
```

The release workflow builds binaries for linux/darwin/windows × amd64/arm64, publishes a GitHub release, and updates the Homebrew tap.

For krew releases: bump version + sha256 in `.krew/upgrade.yaml` and submit to [krew-index](https://github.com/kubernetes-sigs/krew-index).
