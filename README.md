# homelab-cli

Command-line tools for repeatable homelab operations. The repository currently publishes two binaries:

- `homelab` — repository-oriented operations such as rendering Argo CD Kustomize sources.
- `routerctl` — validation, planning, backup, configuration, and firmware operations for reviewed OpenWrt profiles.

Both binaries are built for Darwin and Linux on `amd64` and `arm64`. GoReleaser publishes separate archives and a shared checksum file for each release.

## Development

The module requires Go 1.26.

```bash
go test ./...
go vet ./...
go build ./cmd/homelab
go build ./cmd/routerctl
```

Validate the release configuration without publishing:

```bash
goreleaser check
goreleaser release --snapshot --clean
```

## Render Kustomize sources

`homelab render` delegates builds to `kustomize build --enable-helm` and uses `yq` to write one generated file per Kubernetes resource. Both executables must be available on `PATH`, or supplied with `--kustomize` and `--yq`.

Render one Kustomization:

```bash
homelab render \
  --path argocd/infrastructure/cilium \
  --output artifacts/infrastructure/cilium
```

Render every direct child of the default `argocd/infrastructure` source root:

```bash
homelab render --all
```

Override the roots when a repository uses another layout:

```bash
homelab render --all \
  --source-root path/to/sources \
  --output-root path/to/artifacts
```

Rendering replaces each target output directory only after Kustomize and resource splitting succeed. It substitutes the supported public identifiers `ARGOCD_GITHUB_REPO`, `ARGOCD_GITHUB_ORG`, `VAULT`, and `ARGOCD_ADMIN_GITHUB_USER`, using their homelab defaults when unset.

## Releases

Releases are deliberately semi-manual. Merging to `main` does not create a tag or release. A human or agent must dispatch the **Release homelab CLI** workflow from `main` and provide the exact confirmation value `release`.

The workflow:

1. Finds the latest `vX.Y.Z` tag.
2. Reads every commit from that tag through `HEAD`.
3. Uses Git Cliff and the Conventional Commits in that range to calculate the next version.
4. Generates the release changelog for the exact range.
5. Runs `go test ./...` and `go vet ./...`.
6. Creates an annotated tag whose message is the changelog.
7. Uses GoReleaser to package and publish `homelab` and `routerctl`.
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
