# project policy: the scanner / ci-maintainer lane split

> Example memory file for the scanner agent policy ([scanner.md](scanner.md)).
> The mirror of this rule lives in the ci-maintainer's own policy
> ([ci-maintainer.md](ci-maintainer.md)); the two must always agree.

## The split

One line: **the scanner owns pre-merge; the ci-maintainer (reviewer) owns
post-merge state-of-project.** Every piece of work belongs to exactly one
lane, decided by *what kind of work it is* — never by who noticed it first.

| Work | Owner |
|---|---|
| Issue triage, fix dispatch, worktree fixes | scanner |
| PR review and merge (community + AI-authored, pre-merge) | scanner |
| Addressing Copilot review comments pre-merge | scanner |
| Playwright test failures / flake fixes | scanner (dispatched fix agents) |
| CI workflow health on the default branch (post-merge reds) | ci-maintainer |
| Invariant regressions on main | ci-maintainer |
| GA4 monitoring, error watch, `ga4-error` issues | ci-maintainer |
| Adoption digest (audience / engagement / traffic / geo / trend) | ci-maintainer |
| Unaddressed Copilot comments on already-merged PRs | ci-maintainer |
| UX proposals, workflow offload | ci-maintainer |

## Handing work across the lane

Noticing something in the other lane does not transfer ownership. The scanner
that sees a broken workflow on main does not fix it — it files a bead into
the other lane and moves on:

```bash
bd create --title "<repo>: <what is broken>" --type bug --actor ci-maintainer
bd update <id> --set-metadata lane_transfer=scanner-to-ci-maintainer discovered_at=<iso>
```

The ci-maintainer does the mirror image for pre-merge work it stumbles on
(most commonly Playwright failures, which it is explicitly forbidden to
debug itself).

## Why the split is strict

- **One consolidator per signal.** GA4 anomalies and adoption numbers need
  a single actor with the full timeline (regression framing, PR blame).
  Two agents each filing half the picture produces duplicate issues with
  conflicting baselines — that is exactly what happened before the split.
- **Opposite cost profiles.** Pre-merge work is cheap-parallel (dispatch ten
  worktree agents); post-merge diagnosis is expensive-serial (one context
  window walking main's history). An agent tuned for one is wasteful at the
  other — Playwright debugging inside the ci-maintainer's session burned its
  entire context on flakiness, which is why that prohibition is written into
  its policy in bold.
- **No orphaned work.** "The other agent has it" is only safe when lanes are
  exclusive and transfers are recorded. The `lane_transfer` metadata is the
  audit trail proving the handoff happened, so a dropped bead is a visible
  bug rather than a silent gap.

If either policy file changes the split, change the other in the same commit.
