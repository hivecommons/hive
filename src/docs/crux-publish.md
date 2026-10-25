# Publishing Crux: `hivecommons/crux` is a synced copy of `src/crux`

Hive is the source of truth for the Crux agent runtime. The nested Go module
at `src/crux` (module `github.com/hivecommons/hive/crux`, RFC
[#13536](https://github.com/hivecommons/hive/issues/13536), tracker
[#14138](https://github.com/hivecommons/hive/issues/14138)) is developed,
reviewed and tested here. The public repository
[hivecommons/crux](https://github.com/hivecommons/crux) is a **published,
module-path-rewritten copy** of that module, so that

```sh
go install github.com/hivecommons/crux/cmd/crux@latest
brew install hivecommons/hive/crux
docker run --rm ghcr.io/hivecommons/crux --help
```

work without a consumer pulling hive. Nobody edits Go code in
`hivecommons/crux`; its own CI refuses such pull requests and points back
here.

## Mechanism

| Piece | Where | What it does |
|---|---|---|
| `src/scripts/crux-publish.sh` | hive | Builds the publish tree: clears a `hivecommons/crux` checkout except its hand-maintained scaffolding, copies `src/crux` in, rewrites `github.com/hivecommons/hive/crux` → `github.com/hivecommons/crux` in `go.mod` and every import, writes `.hive-sync` (hive sha, ref, date), then proves the tree builds with `GOWORK=off go build ./... && go vet ./...`. A failing build aborts the sync; nothing is committed. |
| `.github/workflows/crux-sync.yml` | hive | Runs the script on every push to `v5` touching `src/crux/**`, on tags `crux/v*`, and on `workflow_dispatch`; commits the result on top of `hivecommons/crux` `main` (fast-forward, one commit per sync) and, for a `crux/vX.Y.Z` tag, pushes tag `vX.Y.Z` to `hivecommons/crux` at the synced commit. |
| `.github/workflows/ci.yml` | hivecommons/crux | Build, vet, test (`-race`), lint, and a `crux serve` smoke on linux + macOS. |
| `.github/workflows/sync-guard.yml` | hivecommons/crux | Fails any pull request from a non-sync actor that touches `*.go`, `go.mod` or `go.sum`, with a message pointing at `src/crux` here. |
| `.github/workflows/release.yml` + `.goreleaser.yaml` | hivecommons/crux | On tag `v*`: darwin/linux × amd64/arm64 binaries, checksums, cosign-signed, GitHub release, multi-arch `ghcr.io/hivecommons/crux` image. |
| `Formula/crux.rb` | hivecommons/homebrew-hive | Hand-maintained formula over the release tarballs. |

### Why a plain copy, not `git subtree split`

`git subtree split --prefix=src/crux` walks hive's whole history on every run
(minutes today and growing), yields commits whose imports do not compile
(the module path is rewritten *after* the split), and buys nothing: the
commit that lands in `hivecommons/crux` names the exact hive sha it came
from in its message and in `.hive-sync`. The published history is linear
and fast-forward only; `git log` there is a list of hive shas.

### What is preserved in `hivecommons/crux`

`.git`, `.github/`, `.goreleaser.yaml`, `Dockerfile`, `Dockerfile.goreleaser`, `.dockerignore`,
`LICENSE`, `README.md`, `.gitignore`, `.golangci.yml`. Everything else is
replaced on each sync. The module's own `src/crux/README.md` lands as
`docs/MODULE.md`; the repository `README.md` (positioning, install, quick
start) is hand-maintained there.

### Why the nested module publishes standalone

`src/crux/go.mod` requires nothing from the root module and the root
`go.work` is not part of the copy, so the rewritten tree is a complete module
on its own. The root module's `replace github.com/hivecommons/hive/crux =>
./crux` is what lets hive consume it in-tree; the publish side never sees it.
If a change makes `src/crux` import the root module, the sync's build guard
fails and the change must be undone here (that is the Stage 2 ratchet
working as intended).

## Credentials

Pushing to `hivecommons/crux` needs a token with **Contents: write** there.
The workflow uses the repository secret `CRUX_SYNC_TOKEN` when set and falls
back to `TOPUP_PUSH_TOKEN` (the fine-grained PAT `v5-topup` and
`cli-pin-bump` already use). Without a usable token the run fails at the
push step with the secret to provision in its summary, and uploads the
publish tree as the `crux-publish-tree` artifact.

## Manual sync

Same script, same result:

```sh
git clone https://github.com/hivecommons/crux crux-publish
src/scripts/crux-publish.sh . crux-publish
cd crux-publish && git add -A && git commit -s -m "🌱 sync: hive@$(git -C .. rev-parse --short HEAD) (src/crux)" && git push
```

## Releasing

Tag hive: `git tag crux/v0.2.0 <v5-sha> && git push origin crux/v0.2.0`. The
sync workflow publishes that commit and pushes `v0.2.0` to
`hivecommons/crux`; its release workflow does the rest. Then bump
`Formula/crux.rb` in `hivecommons/homebrew-hive` (Prow: `/lgtm` `/approve`).
