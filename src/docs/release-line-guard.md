# Release-line carry-forward guard (#4405)

## Why this exists

Nine workflows pin their `push` / `pull_request` triggers to a hardcoded list of
version-branch names. That list is hand-maintained, and it has already gone
stale once. `podman-contract.yml` named `v2` alone, mainline moved to `v4`, and
the workflow stopped running entirely
([#4339](https://github.com/kubestellar/hive/issues/4339)). Its own header
records the lesson:

> a guard that never executes reports green forever, which is the one failure
> mode a guard cannot have.

That fix added `v4` to the list. It did not change the fact that the list is
hand-maintained, so the same failure was simply queued up for the day `v5` is
cut. `docker.yml` is the outlier that does not have the problem — it triggers on
`'**'` with bot branches excluded, deliberately, so a PR's image always builds.

So on the day `v5` branches, without this guard: the Docker image keeps building
on every branch, while both CI workflows, both security contract gates and all
five Podman guards report nothing at all. Not red — *absent*.

## What the guard is

Three files:

| File | Role |
|---|---|
| `.github/release-lines.yml` | Single source of truth: the release lines, and how each workflow relates to them. |
| `src/scripts/check-release-lines.sh` | Asserts every workflow's branch filters against that manifest. |
| `.github/workflows/release-line-guard.yml` | Runs it, on **every** branch. |

The [issue](https://github.com/kubestellar/hive/issues/4405) offered two shapes.
This is shape 1, *assert the set*, and not shape 2, *remove the pins* (trigger
on `v*`). Two reasons:

- `scorecard.yml` triggers on `main`, which is not a release line and does not
  match `v*`. A pattern cannot express it, so a pattern would leave the workflow
  pinned anyway — the guard would still be needed, for a smaller set.
- A `v*` pattern silently widens what runs. Any branch someone happens to name
  `validate-thing` starts running the Podman lanes. Coverage would be correct by
  construction, but nobody would be able to see what it covers.

Asserting the set keeps the trigger blocks saying literally what they run on,
and turns cutting `v5` into one edit that CI enforces.

## Cutting a new release line

1. Add the branch name to `release_lines` in `.github/release-lines.yml`.
2. Add it to the trigger block of every workflow listed under `pinned`.

Doing only step 1 fails the guard, naming each workflow that would not run on
the new branch. That is the demonstration
`src/scripts/test-release-lines-guard.sh` performs in CI, against the real
workflow files:

```
FAIL: v2-ci.yml:5 branches: has [v2,v4], expected [v2,v4,v5] — missing: v5.
      A release line named here but absent from the workflow means that
      workflow does not run on it at all.
```

## What it checks

- **Both YAML spellings.** `branches: [v2, v4]` (v2-ci.yml, v2-tests.yml) and
  the block sequence used by the Podman lanes and the contract gates. A checker
  that understood only block sequences would silently skip the two highest-value
  entries.
- **Every branch filter in a file**, not the first one — `push` and
  `pull_request` blocks drift apart independently.
- **Classification is mandatory.** A workflow with a branch filter that appears
  in neither `pinned` nor `unpinned` fails. The next pinned workflow somebody
  adds is the next #4339.
- **Deliberate wildcards stay deliberate.** `docker.yml` is declared `unpinned`
  and is not flagged — but if its `'**'` is ever narrowed to a fixed list, that
  *is* flagged, and asks for it to be reclassified.
- **Stale manifest entries.** An entry naming a workflow that no longer exists,
  or one that has lost its branch filter, fails. A guard checking a file with
  nothing left to check is green for no reason.

`pinned` values adjust the expected set per workflow: `[main]` is an extra
branch the workflow legitimately allows, `[-v2]` a release line it deliberately
does not cover. An exclusion has to be written down, which is the point — it
turns a silent gap into a line somebody has to justify.

## The guard is not subject to the bug it guards against

`release-line-guard.yml` triggers on `branches: ['**']`, and is itself listed
under `unpinned` in the manifest, so the guard checks that fact about itself on
every run. A branch-pinned guard against branch-pinned workflows would be #4339
one level up.

Its path filter (`.github/**`, the manifest, the two scripts) is safe in a way a
branch filter would not be: this invariant can only be broken by editing a
workflow, the manifest, or the checker.

The workflow runs the self-test *before* the check, because a checker that has
quietly stopped detecting anything would otherwise pass the real check for the
wrong reason.

## Open: should `v2` still be in these lists?

Deliberately **not** answered by this guard — it is a maintainer call about
release-line support, and the issue asks for it to be surfaced rather than
settled. What the guard surfaced on its first run is that the repository is not
currently consistent about it: mainline is `v4`, eight of the nine pinned
workflows still fire on `v2`, and the ninth — `scorecard.yml` — never has. That
gap is recorded as `-v2` in the manifest rather than quietly corrected, because
turning OpenSSF Scorecard on for a release line is a decision, not a side effect
of adding a CI guard.

Either resolution is one edit to `release_lines` plus the workflows, and the
guard proves the edit is complete. See the note at the bottom of
`.github/release-lines.yml`.

## Running it locally

```sh
src/scripts/check-release-lines.sh          # assert the repository is in sync
src/scripts/test-release-lines-guard.sh     # prove the checker still fails when it should
```
