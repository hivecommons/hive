# Hive-curated good first issues

Hive can advertise a few of its open issues to new contributors — in
particular contributors who bring their own coding agent (Claude Code, Codex,
Copilot CLI, Bob, …). With `good_first_issue.enabled`, a periodic sweep scores
the open, unclaimed issues in each watched repository, applies GitHub's
canonical **`good first issue`** and **`help wanted`** labels to the few that
qualify, and leaves **one** welcome comment that explains how to land the fix
through this hive's [contributor relay](contributor-relay.md) without waiting
on a human.

The label is **advertising, not a hold**. The issue stays in the hive's own
queue. If a hive agent starts on it first, the sweep's label comes off and the
welcome comment is edited to say so. If a contributor claims it through the
relay first, hive leaves it to them — and counts the conversion.

Default **off**: the sweep writes labels and comments on the operator's behalf.

## Attracting contributors with agents

The point of the label is reach. `good first issue` (exact spelling, colour
`7057ff`) plus `help wanted` are what GitHub's per-repository **Contribute**
page (`github.com/<owner>/<repo>/contribute`) and the good-first-issue
aggregators key on, so a hive-labelled issue shows up where people — and the
people steering agents — look for a first task. The welcome comment then does
the pitch, end to end:

1. **Claim through the relay.** The comment deep-links this hive's relay page
   for this issue: `<public_url>/contribute?issue=<owner>/<repo>%23<n>`. The
   landing page recognises the parameter and greets the visitor with the
   issue they came from. Signing in with GitHub and claiming stamps the issue
   with `hive:claim` (see [Contributor relay](contributor-relay.md)), which
   tells hive's own agents to leave it alone.
2. **Bring your own agent.** The comment says explicitly that the repository
   is maintained with hive and welcomes agent-made contributions, and includes
   a one-line **"paste this to your agent"** prompt carrying everything the
   agent needs: repository, issue URL, relay claim URL, the DCO rule
   (`git commit -s`), the `Fixes #<n>` rule, and the repository's `AGENTS.md`
   when it has one.
3. **Open a PR that says `Fixes #<n>`.** The closing keyword is how the
   [contributor lane](hive-merge.md#contributor-lane-auto-merge) links the PR
   to the claim; without it the PR is an ordinary human PR and waits for a
   maintainer.
4. **Hive merges it.** CI runs, the hive review runs, and a green PR from a
   claimed contributor whose tier is sufficient (`require_tiers` or
   `registry_tiers_sufficient`) merges on its own.

The comment also says that hive may pick the issue up itself if nobody claims
it, and that claiming first prevents that. Note the interplay with the
default actionable priority labels: `good first issue` already ranks a
human-filed issue into tier 0 of kick lists
([labels-and-control-signals.md](labels-and-control-signals.md)), so an
advertised issue is also one hive's own agents are likely to reach soon. The
minimum age, the per-repo cap and the release-on-start rule keep that honest:
hive never advertises something it is about to start, and it withdraws the
advertisement the moment it does start.

## What qualifies

The scorer (`src/pkg/gfi`) runs over the hive's last enumeration — the
actionable issue list, which already excludes hard-suppressed, claimed and
PR-covered items — and never calls GitHub to decide. Gates, in order; the
first failure is the `gfi skip reason=…` logged:

| Gate | Skip reason |
|---|---|
| Pull request wearing the issue shape | `pull-request` |
| Live claimant (`hive:claim`, assignee) | `claimed`, `assigned` |
| An open PR already references the issue | `linked-pr` |
| Hard-suppress label (`needs-human`, `needs-direction`, `needs-decision`, `needs-spec`) | `hard-suppress:<label>` |
| Excluded label (`kind/epic`, `epic`, `meta`, `tracker`, `security`, `blocked`, `hold`, `wontfix`, `duplicate`, `hive/in-progress`, `hive/claimed-by-*`, …) | `excluded-label:<label>` |
| Title starts with `Epic`, `Tracking`, `[meta]`, `RFC`, `Proposal`, … | `title:<prefix>` |
| Younger than `min_age` (default 24h) | `too-fresh` |
| Body shorter than `min_body_chars` (80) or longer than `max_body_chars` (4000) | `body-too-short`, `body-too-long` |
| Mentions more than `max_packages` (2) distinct source directories | `touches-N-packages` |
| Fewer than `min_score` (2) positive signals | `score-N-below-M` |

Positive signals, each worth one point and each quoted in the comment as a
reason ("hive picked this issue because: …"):

- already sized small by a maintainer (`size/XS`, `size/S`, `good-first-issue`, `beginner`, `easy`, …)
- a concrete, reproducible bug report (`bug`, `kind/bug`)
- maintainers asked for help on it (`help wanted`)
- has clear steps or acceptance criteria (two or more headings/bullets/checkboxes, or phrases like "acceptance criteria", "steps to reproduce")
- scoped to one or two packages (paths mentioned in the body)
- short, focused description (body at most half of `max_body_chars`)

Per pass, each repository gets at most `max_per_sweep` (2) new selections, and
never more than `max_per_repo` (5) issues wearing the label at once — counting
issues a maintainer labelled by hand. Within a repository the highest score
wins, oldest first on ties. Issues with the `good first issue` label the hive
did not apply are still given the welcome comment (once) so that their
visitors get the relay pitch too.

## The welcome comment

One comment per issue, found by its first-line marker `<!-- hive:gfi -->`. A
later pass edits it in place when the rendered body changes; it never posts a
second one. The default template (`gfi.DefaultTemplate`, a Go `text/template`
over `gfi.Vars`):

````markdown
<!-- hive:gfi -->
### 🌱 Good first issue — bring your own coding agent

Hi! This repository is maintained with [hive](<docs>/README.md) and welcomes contributions made **with your own AI coding agent** (Claude Code, Codex, Copilot CLI, Bob, …). <hive> picked this issue as a good place to start because:

- <reason 1>
- <reason 2>

**How to land it (no waiting on a human):**

1. **Claim it through the relay:** open <public_url>/contribute?issue=<owner>%2F<repo>%23<n> and sign in with GitHub. Claiming stamps the issue so hive leaves it to you.
2. **Fork or branch**, make the change with your agent, and sign each commit with DCO (`git commit -s`).
3. **Open a PR whose description says `Fixes #<n>`** — that closing keyword is how hive's contributor lane links the PR to your claim.
4. Hive runs CI and **merges green PRs from claimed contributors automatically**.

<details>
<summary>Paste this to your agent</summary>

```text
Fix #<n> in github.com/<owner>/<repo> (<issue url>). First open <claim url> and claim the issue through the hive relay (sign in with GitHub). Work on a fork/branch; keep the change scoped to the issue; sign every commit with DCO (git commit -s); open a PR whose description contains the line 'Fixes #<n>' so the hive's contributor lane can auto-merge it once CI is green. Read and follow <AGENTS.md url> before changing code.
```
</details>

If nobody claims it, hive may pick it up itself — claiming first prevents that. Docs: [contributor relay](<docs>/contributor-relay.md) · [contributor lane auto-merge](<docs>/hive-merge.md#contributor-lane-auto-merge) · [trust tiers](<docs>/contributor-trust-and-roles.md) · [AGENTS.md](<AGENTS.md url>).
````

Template variables: `.Repo`, `.Number`, `.Title`, `.IssueURL`, `.ClaimURL`,
`.RelayURL`, `.Docs`, `.Hive`, `.Reasons` / `.TopReasons` (first two),
`.AgentsMDURL` (empty when the repository has no `AGENTS.md`), `.AgentPrompt`
and `.Marker`. An override set in `good_first_issue.comment_template` must
contain `{{ .Marker }}` (or the literal marker); otherwise the config is
rejected, because without the marker the sweep could not find its own
comment.

### Release when hive takes the issue

Whenever a hive agent's own request shows it working an advertised issue — a
claim, comment, label or PR request through the relay, or an agent claim on
the claims ledger — the `good first issue` label is removed (and `help wanted`
too, when this process added it), and the comment is edited to:

```markdown
<!-- hive:gfi superseded=true -->
~~Good first issue~~ — **superseded**: <hive> started working on this issue itself (claimed-by-hive, <RFC3339>), so the `good first issue` label has been removed. Thanks for looking! Other issues open to new contributors are listed on the relay page: <public_url>/contribute
```

A superseded marker is also how the sweep recognises, after a restart, an
issue it must not re-advertise; `reselect_cooldown` (7d) covers the same
within a process.

## Metrics

Structured logs: `gfi select issue=… score=… reasons=… claim_url=…`,
`gfi skip issue=… reason=…` (debug), `gfi release issue=… reason=claimed-by-hive`,
`gfi converted issue=… contributor=… kind=contributor|human` (a claim through
the relay on a labelled issue), and a per-pass `gfi sweep scanned=… selected=…`.
The same events land on the audit log as `gfi-select`, `gfi-release` and
`gfi-convert`. Since-start counters (selected, released, converted) appear on
**Settings → Features → Good first issue curation** and in the Features GET as
`goodFirstIssueStats`. The dashboard marks labelled issues with a 🌱 pill in
the project card listing.

## Configuration

`good_first_issue:` in `hive.yaml`; the toggle, caps, minimum age and interval
are also on Settings → Features. Everything else is YAML-only.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Turn the sweep on. Requires `dashboard.public_url` or a hub-assigned dashboard URL; otherwise the sweep logs `gfi skip reason=no-public-url` and does nothing. |
| `interval` | `1h` | Sweep cadence (over the last enumeration; costs GitHub calls only for the issues it labels). |
| `max_per_repo` | `5` | Issues wearing the label per repository at once. |
| `max_per_sweep` | `2` | New selections per repository per pass. |
| `min_age` | `24h` | Minimum issue age. |
| `min_body_chars` / `max_body_chars` | `80` / `4000` | Body length bounds. |
| `max_packages` | `2` | Distinct source directories an issue may mention. |
| `min_score` | `2` | Positive signals required. |
| `reselect_cooldown` | `168h` | A released issue stays out this long. |
| `exclude_labels` | built-in list | Replaces the built-in exclusions; hard-suppress labels are always excluded. |
| `repos` | all watched | Restrict to these `owner/repo` slugs. |
| `docs_url` | hive's `src/docs` on `v5` | Base URL the comment links docs under. |
| `comment_template` | `gfi.DefaultTemplate` | Override; must contain `{{ .Marker }}`. |

## Related

- [Contributor relay](contributor-relay.md) — what a contributor sets up and how claiming works.
- [Hive merge — contributor lane auto-merge](hive-merge.md#contributor-lane-auto-merge) — the `Fixes #n`, tier and DCO requirements for the merge.
- [Contributor trust tiers and roles](contributor-trust-and-roles.md).
- [Labels and control signals](labels-and-control-signals.md) — where `good first issue` sits among hive's labels.
