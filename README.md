# homelab-cli

One `homelab` binary is the entrypoint for repeatable homelab operations:

- `homelab argocd render` renders Argo CD Kustomize sources.
- `homelab router …` validates, plans, backs up, configures, and upgrades reviewed OpenWrt profiles.

The binary is built for Darwin and Linux on `amd64` and `arm64`. GoReleaser publishes one archive per target plus checksums.

## Development

The module requires Go 1.26.

```bash
go test ./...
go vet ./...
go build ./cmd/homelab

Validate the release configuration without publishing:

```bash
goreleaser check
goreleaser release --snapshot --clean
```

## Argo CD rendering

`homelab argocd render` discovers native Helm and Kustomize source units and uses `yq` to write one generated file per Kubernetes resource. A Helm unit contains `Chart.yaml` and `values.yaml`; a Kustomize unit contains `kustomization.yaml`. A directory containing both markers, an incomplete Helm unit, or a values-only unit fails closed. `helm`, `kustomize`, and `yq` must be available on `PATH`, or supplied with the corresponding command flags.

Render one source:

```bash
homelab argocd render \
  --path argocd/infrastructure/cilium \
  --output artifacts/infrastructure/cilium
```

Render every source discovered recursively below the default `argocd/` root:

```bash
homelab argocd render --all
```

The source-relative layout is preserved below `artifacts/`, for example `argocd/application/immich` renders to `artifacts/application/immich`. A complete render atomically replaces the output root, removing stale outputs.

In CI, render only source units changed by the current GitHub event:

```bash
homelab argocd render --ci
```

CI mode requires a clean worktree, reads the push or pull-request base from `GITHUB_EVENT_PATH`, and falls back to `HEAD^` for manual runs. Deleting a source removes its matching output directory.

After repository validation, explicitly commit artifact-only changes and push them to the current branch:

```bash
homelab argocd render --commit-and-push
```

The commit step is a no-op when artifacts are current and refuses to commit if any changed path is outside `--output-root`. Add `--fail-on-change` when CI must push generated artifacts and then fail the original source-only run. `--ci --commit-and-push` combines both phases only when no validation step needs to run between them.

Override the roots when a repository uses another layout:

```bash
homelab argocd render --all \
  --source-root path/to/sources \
  --output-root path/to/artifacts
```

Kustomize execution uses `kustomize build --enable-helm`. Helm execution runs `helm dependency build` in an isolated source copy, then `helm template` with the unit's `values.yaml` and CRDs included. Rendering replaces an individual target only after source execution and resource splitting succeed. It substitutes the supported public identifiers `ARGOCD_GITHUB_REPO`, `ARGOCD_GITHUB_ORG`, `VAULT`, and `ARGOCD_ADMIN_GITHUB_USER`, using their homelab defaults when unset.

## Router operations

Router functionality is available only through `homelab router`. Existing router flags and safety gates are retained:

```bash
homelab router validate --profile path/to/profile
homelab router plan --profile path/to/profile --target router.example --host-fingerprint SHA256:...
```

Use `homelab router --help` for the full command set. Mutating commands retain their explicit `--authorize` requirements and pinned-host-key checks.

## Releases

Releases are deliberately semi-manual. Merging to `main` does not create a tag or release. A human or agent must dispatch the **Release homelab CLI** workflow from `main` and provide the exact confirmation value `release`.

The workflow:

1. Finds the latest `vX.Y.Z` tag.
2. Reads every commit from that tag through `HEAD`.
3. Uses Git Cliff and the Conventional Commits in that range to calculate the next version.
4. Generates the release changelog for the exact range.
5. Runs `go test ./...` and `go vet ./...`.
6. Creates an annotated tag whose message is the changelog.
7. Uses GoReleaser to package and publish `homelab`.
8. Uses the same changelog as the non-empty GitHub release body.
9. Verifies all archives, checksums, and release notes before notifying the Hermit package index.

### Trigger from GitHub

1. Open **Actions** → **Release homelab CLI** → **Run workflow**.
2. Select the `main` branch.
3. Select the bump:
   - `auto` — recommended; Conventional Commits determine the SemVer bump.
   - `patch`, `minor`, or `major` — explicit operator override.
4. Leave `recover_tag` empty for a new release.
5. Enter `release` in `confirm`.
6. Run the workflow and inspect its job summary and resulting GitHub release.

### Trigger with GitHub CLI

Create a normal release using automatic SemVer calculation:

```bash
gh workflow run release.yml \
  --repo nandiheath/homelab-cli \
  --ref main \
  -f bump=auto \
  -f confirm=release
```

Inspect the dispatched run:

```bash
gh run list \
  --repo nandiheath/homelab-cli \
  --workflow release.yml \
  --limit 1

gh run watch --repo nandiheath/homelab-cli
```

### Recover a failed publication

Use recovery only when the annotated tag was pushed but its GitHub release was not created. The workflow refuses to overwrite an existing release.

```bash
gh workflow run release.yml \
  --repo nandiheath/homelab-cli \
  --ref main \
  -f bump=auto \
  -f recover_tag=vX.Y.Z \
  -f confirm=release
```

Recovery checks out the existing tag and regenerates its changelog from the preceding release tag through the recovery tag. Do not delete, move, or reuse published semantic tags.

## Commit convention

Use Conventional Commits. Git Cliff groups `feat`, `fix`, `perf`, `refactor`, `docs`, `test`, `build`, `ci`, `chore`, `revert`, and `style` commits in release notes. Breaking changes and `feat` commits drive automatic major and minor bumps; other release-relevant changes produce a patch bump.
