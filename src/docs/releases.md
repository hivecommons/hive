# Tagged releases

Hive ships continuously — every merge to `v4` publishes moving image tags
(`v4-latest`, the three channels, an immutable short-SHA tag; see
[release channels](release-channels.md)) with no human step. This page covers
the second, **additive** layer on top of that: immutable, semver-tagged
releases (`v1.2.3`) with a git tag and a GitHub Release, cut automatically —
no human ever pushes a tag or clicks "Draft a release" in the normal path.

## What triggers a release

`.github/workflows/tagged-release.yml` runs after every successful
`Build and Push Docker Image` (`docker.yml`) run on `v4` — a `workflow_run`
trigger, not a tag push, because there is no tag until this workflow decides
to create one. It never runs for `v2`, `mk`, `dd`, or a manual
`workflow_dispatch` build.

It also runs **hourly on a schedule**, as a backstop (#5318). The
`workflow_run` trigger alone can silently lose a release opportunity: a
`docker.yml` run that is *cancelled* never fires `workflow_run` at all, and a
run that fires but finds `v4` already advanced stands down in favour of a
successor that may itself stand down. Standing down is correct — the run's
images were built from the older tree, so tagging would name content those
images do not contain — but nothing used to come back for the abandoned work.
The scheduled pass evaluates `v4`'s **current** tip, whose images have long
since been published, so it retags an existing digest exactly as the normal
path does. It refuses to act unless `docker.yml` has a *successful, completed*
push run for that tip, so a cancelled or in-flight build never produces a tag
with no digest behind it, and it is a no-op whenever `## Unreleased` is empty
— which on a healthy repository is almost always. Deferrals are logged as
warnings so a skipped opportunity is visible rather than silent.

Before anything is decided, `src/scripts/compile-changelog.sh` folds any
[`changelog.d/`](../../changelog.d/README.md) fragment files — the per-PR
entry files that replaced direct `## Unreleased` appends
([#5675](https://github.com/hivecommons/hive/issues/5675)) — into the
`## Unreleased` section of the working tree, grouped under the `###`
subsection their filename prefix names. `src/scripts/derive-release-version.sh`
then decides two things by reading
[`CHANGELOG.md`](../../CHANGELOG.md)'s `## Unreleased` section — nothing else,
no commit-message parsing:

- **Empty `## Unreleased`** (the common case — most merges are docs, tests,
  CI, or refactors) → **no release**. Silent, correct, no tag, no GitHub
  Release, nothing published beyond the continuous images that already went
  out.
- **Non-empty** → a release is cut, with the bump taken from which
  `###` subsection headers are present:
  - `### Security` present → **major**
  - else `### Added` present → **minor**
  - else (`### Changed` / `### Fixed` / `### Deprecated` only) → **patch**

### Why CHANGELOG.md and not the emoji commit-prefix convention

CONTRIBUTING.md asks PR titles to start with an emoji (`🐛 fix`, `✨ feature`,
`📖 docs`, …), which looks like a ready-made conventional-commit signal. It
is **not enforced** — plenty of merged commits on `v4` carry no emoji at all
— so inferring a published, immutable version number from it would mean an
unlabeled fix silently produces the wrong shape of release, or a missing
prefix produces no release at all, with nobody watching each merge to catch
it. `CHANGELOG.md`'s `## Unreleased` section is already the human,
PR-time judgment call for "is this release-worthy" (`CONTRIBUTING.md` and
`.github/workflows/changelog-reminder.yml` already ask for and nudge
entries there), and getting it wrong fails safe: a missed entry means **no**
release fires, never a released version with the wrong number. See the
script's own header comment for the full reasoning.

## The commit convention IS the interface

This is the one thing a contributor needs to know to cause a release, and it
is nothing new — it is the existing changelog convention, now load-bearing:

- Add a `changelog.d/<category>-<pr-or-slug>.md` fragment in your PR (as
  `CONTRIBUTING.md` asks, nudged by the changelog-reminder and
  changelog-fragment-guard checks) if your change is user-visible. The file's
  content is exactly your entry — one `- ` bullet; see
  `changelog.d/README.md`.
- The filename's category prefix picks the subsection your entry is compiled
  into (`added-` → `### Added`, and so on for `changed`/`deprecated`/`fixed`/
  `security`), and the subsections drive the bump exactly as they always
  have. Direct `## Unreleased` edits still compose with fragments during the
  transition (#5675), but every PR editing that one shared heading is what
  made unrelated PRs conflict, so prefer fragments.
- If your PR carries **no** fragment (and no `CHANGELOG.md` edit), it
  contributes to whatever release fires next but does not by itself trigger
  one — and if `Unreleased` and `changelog.d/` are otherwise empty, no
  release fires until someone's entry lands.

You do not choose a version number. You choose a changelog section, and the
version follows from semver rules applied to whatever is sitting in
`Unreleased` when the next merge to `v4` completes its build.

## The escape hatch

Inference will occasionally be wrong — an entry filed under the wrong
heading, or a change a maintainer wants held back. A single HTML-comment
marker anywhere in the `## Unreleased` section overrides the inferred
decision:

```markdown
<!-- release: none -->
```

suppresses a release for this merge even if `Unreleased` has content (the
entries stay queued for the next release that does fire). Or force a specific
bump regardless of which headers are present:

```markdown
<!-- release: major -->
<!-- release: minor -->
<!-- release: patch -->
```

Two conflicting markers in the same section is a hard failure (the workflow
errors loudly) rather than a silent pick — remove all but one.

## What is immutable vs. moving

| Tag | Written by | Moves? |
|---|---|---|
| `v4-latest`, `stable`, `candidate`, `edge` | `docker.yml`, every merge to `v4` | Yes — moving pointers |
| `<7-hex-sha>` | `docker.yml`, every successful build | No — immutable, but not a *release* |
| `v1.2.3` | `tagged-release.yml`, only when a release is cut | No — immutable, and **is** the release |

`tagged-release.yml` never writes `stable`/`candidate`/`edge`. Channel promotion is a
separate, deliberate policy described in
[release-channels.md](release-channels.md); cutting a version tag never
silently couples to it, on purpose — the operator explicitly asked for these
to stay decoupled.

## How a release is actually built

`tagged-release.yml` does **not** rebuild the image. `docker.yml`'s own freshness
guard already proved, for this exact commit, that the pushed digest's
embedded commit hash matches — rebuilding would only reintroduce the risk
that guard exists to eliminate. Instead, `tagged-release.yml` retags the
already-published `<7-hex-sha>` digest as the immutable version tag with
`docker buildx imagetools create`, the same primitive
`src/scripts/publish-image-tags.sh` already uses for the moving tags. The
published `v1.2.3` image is byte-identical to the `<sha>` / `v4-latest` image
`docker.yml` published for that commit.

Concretely, per release:

1. `derive-release-version.sh` decides `release=true` and a version.
2. Refuse if `v<version>` already exists as a git tag (idempotency guard —
   should never trigger in the normal path; see below).
3. `docker buildx imagetools create` retags `hive`, `hive-contributor`, and
   `hive-hub` at `:<7-hex-sha>` as `:v<version>`.
4. `changelog.d/` fragments are compiled into `## Unreleased`
   (`src/scripts/compile-changelog.sh` — the same compile the decide job ran
   working-tree-only, now on the release job's fresh checkout), then the
   whole section is moved into a dated `## YYYY-MM-DD (v<version>)` section
   (the file's own documented convention — see its "How we maintain this
   file"), a new empty `## Unreleased` is left above it, and the consumed
   fragment files are deleted.
4a. Syft generates an SPDX JSON SBOM for each of the three retagged images
   (see "Software bill of materials (SBOM)" below) — this happens before the
   changelog commit, using the version tag written in step 3.
5. That change is committed (`git commit -s`, signed off by the release bot).
5a. Before it can reach `v4`, the commit has to earn the `gate` check that
   branch protection requires (see "Satisfying branch protection" below).
   The commit is pushed to a throwaway `release-gate/v<version>` branch,
   `docker.yml` is dispatched, and the workflow waits for `gate` to succeed
   on that exact SHA. It then mirrors the verified result as a SHA-scoped
   `gate: success` commit status so a release PR can see it.
6. The workflow opens a PR from the scratch branch into `v4` and merges it
   through the SHA-keyed merge API, leaving branch protection fully enforced.
   It deletes the scratch branch, then creates and pushes the `v<version>` tag
   on the commit that landed on `v4`.
7. A GitHub Release is created from the tag, with GitHub's auto-generated
   notes plus an SBOM callout, and the three SBOM files from step 4a attached
   as release assets.

## Satisfying branch protection

`v4`'s only required context is `gate` (`docker.yml`). The release commit is
created inside `tagged-release.yml`, so it has no check when it first exists;
a direct push to `v4` is rejected (`GH006: Required status check "gate" is
expected`, [#5026](https://github.com/hivecommons/hive/issues/5026)). Retrying
does not create the missing evidence, so every attempt fails identically.

The workflow first pushes the commit to `release-gate/v<version>`, dispatches
`docker.yml`, and waits for its `gate` check-run on the exact release SHA.
That verifies the same code path as an ordinary PR gate, but a
`workflow_dispatch` check-run has no pull-request association: its
`pull_requests` list remains empty even if it is dispatched after the release
PR exists. Consequently GitHub's protected-PR rollup omits it and the merge
API still reports `gate` as expected ([#5356](https://github.com/hivecommons/hive/issues/5356)).

After the check-run succeeds, the workflow posts a `gate: success` commit
status on the same SHA using its `GITHUB_TOKEN` and `statuses: write`
permission. A commit status is SHA-scoped rather than check-suite/PR-scoped,
so it appears in the release PR's required-context rollup. This is a mirror,
not a second source of truth: a missing or red docker gate prevents the status
from being posted, a failed status POST prevents the PR from opening, and the
SHA-keyed merge API still asks GitHub to enforce `v4` protection server-side.

**Getting `docker.yml` to actually run on the scratch branch (#5072):**
`docker.yml`'s `push` trigger is `branches: ["**"]` (minus bot branches — see
`.github/release-lines.yml`'s `unpinned` entry for it), which on paper covers
`release-gate/*` too — but the scratch push uses this job's default
`GITHUB_TOKEN`, and GitHub deliberately does not start *other* workflow runs
from a `GITHUB_TOKEN`-authenticated push (recursive-workflow prevention). The
`push` trigger silently never fires, no `gate` check ever attaches to the
commit, and the wait loop times out — every release run failed this way until
#5072. `docker.yml` also has a `workflow_dispatch` trigger, which a
`GITHUB_TOKEN` *can* start via the API (`gh workflow run docker.yml --ref
release-gate/v<version>`), and that run's check-runs attach to the scratch
branch's head SHA exactly as a `push`-triggered run's would — so this step
dispatches it explicitly right after the scratch push, rather than relying on
the `push` trigger.

`workflow_dispatch` on `docker.yml` normally forces a GHCR push regardless of
branch (so a throwaway branch can be published for a hive on demand) — which
would mean every release pushes a real, one-off `release-gate/v<version>`
image and moving tag to GHCR purely to obtain a status check that only needs
the `gate` job (a few seconds) to run. `docker.yml`'s `gate` job carries a
`release-gate/*` exception so that never happens, on any trigger: the scratch
branch name is deliberately not in `docker.yml`'s `LONG_LIVED` set (`v2 v4 mk
dd`) and the exception forces `push=false` for it unconditionally, so this
detour never pushes a GHCR image or moves a channel tag; `gate` runs
regardless of push policy, which is all this needs. The scratch branch is
deleted by the merge step's `trap ... EXIT` once that step starts, whether the
PR merges or fails. A failure during the preceding gate-earning step leaves the
branch in place for diagnosis.

This preserves branch protection exactly as configured — no bypass, no
weakened check, no `enforce_admins` change, and no force push. The workflow
earns the real docker gate on the scratch branch, mirrors that exact-SHA
verdict into the representation the release PR can consume, and lets the
protected merge endpoint make the final decision.

## Software bill of materials (SBOM)

Every tagged release ships a downloadable SBOM per image — `hive`,
`hive-contributor`, `hive-hub` — attached to the GitHub Release as
`hive-v<version>-sbom.spdx.json`, `hive-contributor-v<version>-sbom.spdx.json`,
and `hive-hub-v<version>-sbom.spdx.json`. Format is **SPDX JSON**, generated
by **Syft** (`anchore/sbom-action`), scanning the already-published, just-
retagged `v<version>` image reference on GHCR — not the source tree. A
source-tree scan would report what `go.mod`/`package.json` *ask for*; scanning
the built image reports what the shipped filesystem *actually contains* — the
apk-installed OS packages, the from-source tmux/Node layers, and the exact
resolved Go module versions — which is the more faithful record for anyone
consuming a specific release's security posture.

**Coverage limitation, stated plainly:** `hive` and `hive-hub` are multi-arch
(`linux/amd64`, `linux/arm64`); the SBOM step scans the `linux/amd64`
manifest only, not `linux/arm64` separately. This is a documented limitation,
not a silent gap: a Go binary's cross-compiled machine code differs by
architecture but the *package versions* that produced it — Go module
versions, apk/npm package versions in the same Dockerfile layers — are
identical on both, and an SBOM describes packages and versions, not compiled
bytes. One architecture's manifest is therefore representative of both, and
the GitHub Release notes say so explicitly rather than implying full
multi-arch coverage.

### Why this is a release artifact, not an in-image attestation

`docker.yml` sets `provenance: false` and `sbom: false` on every
`docker/build-push-action` step, on purpose, and
`.github/workflows/image-attestation-guard.yml` +
`src/scripts/check-no-image-attestations.sh` guard those four sites in CI so
a well-meaning revert fails loudly instead of shipping. The reason ([#3760](https://github.com/hivecommons/hive/issues/3760),
documented at length directly above each flag in `docker.yml`): `build-push-action`
attaches provenance/SBOM attestations by *default*, and an attestation can
only be carried by an OCI image **index** — so turning it on changes the
published per-arch digest from a plain image manifest to
`application/vnd.oci.image.index.v1+json`. That index form is exactly what let
a `COPY --from=builder` layer ship an **overlayfs metacopy redirect** for
`/usr/local/bin/hive`, which containerd/k3s and rootless podman present as
**non-executable** at the image path until copy-up — every hive pulling such
an image crash-looped at boot on a bare `EPERM` at `execve`. The image
manifest format is therefore a hard constraint, not a style preference: it
must stay a plain manifest.

The release SBOM generated here never touches that constraint. It runs
**after** `docker.yml` has already published the plain-manifest image
(`tagged-release.yml` retags, never rebuilds — see above), scans the published
digest from the outside with an independent tool, and writes an ordinary JSON
file that is uploaded to the GitHub Release. Nothing about generating it adds
an attestation to the GHCR image, changes its media type, or touches the
manifest `docker.yml` published. If a future change moves SBOM generation
back into `docker.yml`'s `build-push-action` steps (e.g. by flipping
`sbom: true`), it reopens #3760 — that is exactly what the guard workflow
exists to catch.

## Third-party notices (NOTICE)

The repo root carries a generated [`NOTICE`](../../NOTICE) file: one entry per
Go module dependency compiled into `hive`, `hive-hub`, and
`hive-contributor`, with module path, resolved version, license identifier,
and — once CI has produced the authoritative version — the full license text
where the license requires reproduction (Apache-2.0 §4(d), MIT, and BSD all
do). This exists because a CNCF General Technical Review asks how the
project ensures third-party code carries correct and complete attribution;
`go.sum` pins dependency versions but was never, by itself, an assembled
attribution document.

**How it is generated.** `src/scripts/generate-notice.sh` walks the resolved
module graph (not just `go.mod`'s direct requires) using
[`google/go-licenses`](https://github.com/google/go-licenses), pinned to an
exact tagged version and installed via `go install` — the same pattern
`go-security-analysis.yml` already uses for `govulncheck` and `gosec`, for
the same reason: a supply chain one hop shorter than depending on a
marketplace action, and a version that only changes when a maintainer
deliberately bumps it. The script's own header explains why `go-licenses` was
chosen over a hand-rolled `go list -m -json all` walk.

**How freshness is enforced.** The `notice-drift` job in
`go-security-analysis.yml` regenerates `NOTICE` on every push and pull
request touching `src/go.mod`, `src/go.sum`, or the generator script itself
(plus weekly, since an upstream dependency's license text can change without
a version bump), and fails the build if the committed file differs from a
fresh run. A generated file that can silently go stale is worse than no file
at all — it would make a false completeness claim the moment a dependency
changed. There is deliberately no special-case to tolerate an out-of-date
`NOTICE`: the fix is always "run the script, commit the result."

**Current state of the committed file.** `NOTICE` is the authoritative,
`go-licenses`-generated output from the resolved module graph. The generator
uses `go-licenses report` rather than `save`: `save` refuses to emit anything
when the graph contains a license class the tool considers incompatible,
while an attribution inventory must identify every dependency, including a
restrictively licensed one. Inclusion in `NOTICE` records what ships; it is
not an approval or compatibility decision. License-acceptance policy belongs
in a separate gate so it cannot make this inventory incomplete.

**What `NOTICE` does NOT cover.** Go module dependencies only. It does not
cover:

- Base container image OS packages (the `apk`-installed package set) — those
  are covered by the per-image SBOMs above, which scan the built filesystem
  rather than the source tree.
- The Node.js and `tmux` layers built from source in `src/Dockerfile` /
  `src/Dockerfile.hub` — also covered by the SBOMs, not `NOTICE`.
- Anything in `dashboard/` or other non-Go parts of the repo that may carry
  their own dependency manifests (npm, etc.) — out of scope for this file;
  a JS/TS equivalent, if wanted, is a separate follow-up.

`NOTICE` and the per-image SBOMs are therefore complementary, not redundant:
`NOTICE` gives license *text* for Go dependencies (what the SBOM format does
not carry), and the SBOM gives full package inventory (OS + language runtime
layers) that `NOTICE` does not attempt.

**Shipped in releases.** `tagged-release.yml` copies the repo-root `NOTICE` (already
kept fresh by `notice-drift` at every commit that changes dependencies) to
`hive-v<version>-NOTICE` and attaches it to the GitHub Release alongside the
three SBOM files, using the same `gh release create` asset-upload call.

## Idempotency and concurrency

- **Step 5 emptying `Unreleased`** is what makes this safe to chain off
  `docker.yml`: pushing the release commit to `v4` triggers `docker.yml`
  again, which triggers `tagged-release.yml` again — and on that second pass
  `Unreleased` is empty, so `derive-release-version.sh` returns
  `release=false` and the workflow is a no-op. It never chases its own tail.
- `concurrency: { group: tagged-release-v4, cancel-in-progress: false }`
  serializes overlapping runs so two merges landing close together queue
  rather than race two tags for two different commits.
- A re-run after a partial failure (for example, the images got retagged but
  the push failed) is safe: `imagetools create` is itself idempotent per tag,
  and the "refuse to reuse an existing tag" step turns any genuine double-run
  attempt into a loud failure instead of a silent duplicate.
- The version a running binary reports can never disagree with its tag,
  because the version is never a source-committed string to fall out of sync
  — see below.

## The version constant

`cmd/hive/main.go` carries one `var version = "0.0.0-dev"`, overridable at
build time via `-ldflags -X main.version=...`, exactly like the existing
`gitHash`/`gitShort`/`gitBranch` build-time vars. `src/Dockerfile` and
`src/Dockerfile.hub` both accept an optional `VERSION` build-arg; when unset
(every ordinary branch build, including plain `docker.yml` runs) the Go
linker default `0.0.0-dev` ships instead — never an empty string.

`tagged-release.yml` does not need to pass `VERSION` to a rebuild, because it never
rebuilds (see above) — the retagged image was already built by `docker.yml`
carrying whatever `version` that ordinary build embedded. This is deliberate:
today, a tagged-release image and its `<sha>`/`v4-latest` sibling report the
same non-version-specific string (`0.0.0-dev`) even though only one of them
carries a `vX.Y.Z` GHCR tag. **This is a known gap**, not an oversight — see
below.

## What still blocks cutting a real release

This PR wires the machinery; it does not itself cut a release, and one
concrete gap remains before the first automated `v0.x.y` should be trusted
end-to-end:

- **The running binary's `--version` output does not yet say `v1.2.3` for a
  release build.** Because `tagged-release.yml` retags rather than rebuilds (by
  design — see "How a release is actually built"), the image GHCR now calls
  `ghcr.io/hivecommons/hive:v1.2.3` still reports whatever `main.version`
  the original `docker.yml` build embedded, which today is always the
  `0.0.0-dev` fallback since `docker.yml` never passes `VERSION`. Closing
  this cleanly means either (a) teaching `docker.yml` to pass a
  provisional/dev-style `VERSION` on every `v4` build so the embedded string
  at least varies per commit, or (b) accepting that `--version` reports the
  git commit faithfully (`gitShort`/`gitBranch` are always correct) while the
  semver field is aspirational until a release is cut and only the *image
  tag*, not the binary's self-report, is authoritative for a specific
  release. This PR does not resolve that tension — it is a maintainer product
  decision about what `--version` should mean on a non-release build, not a
  wiring gap this automation can silently paper over.
- **No starting version has been chosen.** With zero existing tags, the first
  automated release computes its base as `0.0.0` and bumps from there (so the
  first `## Added` entry ships as `v0.1.0`, not `v1.0.0`). If the project
  wants its first tag to read `v1.0.0`, a maintainer needs to push that tag
  by hand once, after which every future release bumps from it normally —
  this workflow deliberately never invents a major version on its own
  authority.
- **This PR does not create any tag.** No `v*.*.*` tag or GitHub Release
  exists yet as of this PR; the very next content-carrying merge to `v4`
  (after this PR itself merges, assuming its own CHANGELOG entry survives
  under `## Unreleased`) is what would cut the first one.
