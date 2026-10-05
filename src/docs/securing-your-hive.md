# Securing your hive: a first-time operator's guide

You've just connected a hive to a repository. Before you pick a level and walk
away, you need a plain answer to one question: **who can make Hive do what in
my repo?** The facts are correct and complete elsewhere — in
[security-model.md](security-model.md), the [ACMM policy
matrix](acmm-policy-matrix.md), [security-threat-model.md](security-threat-model.md),
[agent-configuration.md](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled),
and [contributor-trust-and-roles.md](contributor-trust-and-roles.md) — but
they are mechanism-oriented, and a first deployment shouldn't require reading
all five before you understand what you just turned on.

This guide is decision-oriented. It doesn't repeat the tables in those pages;
it links to them and explains what the settings *mean* for a stranger filing
an issue against your repo tomorrow morning.

## The 60-second version

Three settings decide who can make Hive act, and how far:

1. **ACMM level** (`acmm_level`, 1–6). This is the big dial. It decides which
   agents run, and whether each one can only observe, file issues, open
   held pull requests, or open-and-merge pull requests. See the [ACMM policy
   matrix](acmm-policy-matrix.md) for the full per-level, per-agent table.
2. **Reporter trust** (`project.issue_filter.reporter_trust`, off by
   default). Decides whether Hive tells *maintainer* issues from *stranger*
   issues apart — not just by label, but by who GitHub says filed them. See
   [agent-configuration.md § Reporter
   trust](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled).
3. **GitHub App install scope / repo allowlist.** Whichever repositories you
   installed the Forge App on (or listed in `project.repos`) are the only
   ones a hive can touch at all, enforced twice: once at the network proxy and
   once as a prompt-level reminder. See [security-model.md § Layer
   5](security-model.md#layer-5--github-blast-radius-controls).

Everything below is these three settings interacting with a fourth fact you
don't configure: GitHub's own `author_association` on every issue and PR
(`OWNER`, `MEMBER`, `COLLABORATOR`, `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`,
`FIRST_TIMER`, `NONE`) — see the [glossary](#glossary) below.

## Pick a starting posture

Every posture below assumes the repo allowlist only covers repos you actually
want Hive touching — that part doesn't change with level or reporter trust.

### Cautious — L4/L5, reporter trust off

**A stranger files an issue:** an agent may open an issue about it (at L4,
only quality/sec-check/ci-maintainer may open PRs; at L5, every agent may).
Either way, any resulting pull request is held: at L5 the level gate labels
**every** agent PR `hold`, no exceptions. At L4, only the agents whose mode is
`holdgated` (quality, ci-maintainer, sec-check) can open a PR at all, and
those are held the same way; the rest stay `measured` (issues only) or
`advisory`. Don't describe L4 as "L5 but smaller" — it's a genuinely mixed
roster, not a uniformly held one. See the [L3–L5 agent/mode
tables](acmm-policy-matrix.md#l4--security-aware-adaptive-7-agents).

**A known contributor (`COLLABORATOR`/`MEMBER`) files an issue:** same
treatment — reporter trust is off, so Hive doesn't look at who asked, only at
what labels and gates apply. The PR is still held the same way.

**Who merges:** a human, always. Merge permission "simply is not granted below
L6" — the token tier and proxy rules refuse it regardless of level-hold
labels (see [security-model.md § Layer
5](security-model.md#layer-5--github-blast-radius-controls)).

### Trusted team — L6, reporter trust on, trusting OWNER/MEMBER/COLLABORATOR

This is the reporter-trust default set — you don't have to list anything to
get it:

```yaml
project:
  issue_filter:
    reporter_trust:
      enabled: true
```

**Anyone outside the trusted set (`CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`,
`FIRST_TIMER`, `NONE`) files an issue:** not actionable until someone adds
`triage/accepted` (the default `untrusted_require_labels`).
Once triaged, an agent may work it — but the resulting PR still gets `hold`
at every level, L6 included, because the reporter-trust merge-side check
re-evaluates who the rationale traces to; triaging the issue does not
un-hold the PR. See [agent-configuration.md §
Admission/Merge](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled).

**A team member (`MEMBER`/`COLLABORATOR`/`OWNER`) files an issue:** admitted
and worked exactly as before reporter trust existed — no triage label needed,
and at L6 the resulting PR merges on green CI with no level hold (unless some
other hold applies).

**Who merges:** agents, autonomously, for team-filed work. A human, always,
for anything traceable to an untrusted reporter.

### Open community — L6, reporter trust on, also trusting CONTRIBUTOR

```yaml
project:
  issue_filter:
    reporter_trust:
      enabled: true
      trusted_associations: [OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR]
```

**A `CONTRIBUTOR` (someone with at least one merged commit, ever) files an
issue:** admitted like a team member's — no triage label required, and a
resulting PR can merge unattended on green CI.

**Anyone else** (`FIRST_TIME_CONTRIBUTOR`, `FIRST_TIMER`, `NONE`, or an
association GitHub didn't report): requires `triage/accepted` for admission,
and any resulting PR is still held for a human, exactly as in the trusted-team
posture above.

**Who merges:** agents, for anyone with standing history in the repo. A human
for first-time strangers, always — this posture widens *whose* work gets
autonomy, not *whether* an unvetted stranger's work can merge unattended.

## Who gets worked, who gets merged: the reporter-trust matrix

The trusted-team posture above, as two tables. Both assume reporter trust is
on with the default boxes (`OWNER`, `MEMBER`, `COLLABORATOR` checked;
everything else unchecked), the triage label is `triage/accepted`, the repo's
reporter-trust hold has not been switched off on the Repos tab, and the hive
is at L6.

### The short version

Two questions decide what happens to an issue: was it filed by someone
trusted, and does it carry `triage/accepted`?

| | **No `triage/accepted`** | **Has `triage/accepted`** |
|---|---|---|
| **Filed by a trusted person** | ✅ Worked. Hive's PR merges on its own on green CI. | ✅ Worked. Hive's PR merges on its own on green CI (the label changes nothing). |
| **Filed by anyone else** | ⛔ Not worked. | ✅ Worked. ✋ Hive's PR is held until a person removes `hold`. |

"Trusted" means the reporter's GitHub association is one of the checked boxes,
or their login is under **Always-trusted logins**.

### Who counts as what

GitHub, not Hive, decides each issue author's association.

| Filed by | How someone ends up here | Default box | Worked without `triage/accepted`? | Worked with `triage/accepted`? | Hive's PR merges on its own at L6? |
|---|---|---|---|---|---|
| `OWNER` | Owns the repo or org | ✅ | Yes | Yes | Yes, on green CI |
| `MEMBER` | Is a member of the org that owns the repo | ✅ | Yes | Yes | Yes, on green CI |
| `COLLABORATOR` | Was given access to the repo (invited) | ✅ | Yes | Yes | Yes, on green CI |
| Always-trusted login | You added their login on the Labels tab | n/a | Yes | Yes | Yes, on green CI |
| `CONTRIBUTOR` | **Automatic:** has previously committed to this repo (one typo fix is enough) | ☐ | No | Yes | **No.** Held until a person removes `hold` |
| `FIRST_TIME_CONTRIBUTOR` | **Automatic:** has commits elsewhere on GitHub, none here | ☐ | No | Yes | **No.** Held until a person removes `hold` |
| `FIRST_TIMER` | **Automatic:** has never committed anywhere on GitHub | ☐ | No | Yes | **No.** Held until a person removes `hold` |
| `NONE` | No relationship to the repo | ☐ | No | Yes | **No.** Held until a person removes `hold` |

### What changes automatically, and what doesn't

`FIRST_TIMER`, `FIRST_TIME_CONTRIBUTOR` and `CONTRIBUTOR` come from commit
history alone, and GitHub changes them by itself: a reporter's first commit to
this repo makes them `CONTRIBUTOR`, whichever of the other two they were
before. (`NONE` just means no relationship to the repo.) None of these rows is
checked by default, so **commit history alone never makes anyone trusted**.
The open-community posture above checks `CONTRIBUTOR`. That's the one setting
where a single merged PR does make someone trusted.

The checked rows change only when someone is given repo access or org
membership. That usually means a person sending an invite. If your org or repo
grants either automatically (a team sync, an onboarding bot, or a workflow
that invites contributors after their first merge), whoever it grants becomes
trusted too. In that case, uncheck the box that automation feeds and trust
people by login instead.

**Always-trusted logins** trusts one person by name without giving them any
repo access, which inviting them as a collaborator would.

### Things that catch people out

- **`MEMBER` means every member of the owning org,** including people with no
  access to this repo. In a large org that's a lot of people. If it's too
  broad, uncheck `MEMBER` and list the people you trust by login.
- **`COLLABORATOR` doesn't say how much access someone has.** GitHub reports
  the association without the permission level, so Hive can't tell a
  read-only collaborator from an admin. Checking the box trusts everyone you
  have added as a collaborator.
- **Hive checks that `triage/accepted` is present, not who added it.** Anyone
  with Triage access or higher on the repo can add it. Triage only lets the
  work start; the hold is what stops the merge.
- **The hold depends on who filed the issue, not on who did the work.** It
  doesn't matter which agent or contributor wrote the PR. If the PR is linked
  to any issue from an untrusted reporter (`Closes #N`, `Refs #N`, or the
  issues the agent declared when it asked Hive to open the PR), it's held.
- **The hold only covers PRs Hive opens.** A PR a person opens by hand goes
  through the repo's normal branch protection and review rules.
- **Issues filed by bots or by Hive itself skip reporter trust.** A different
  safeguard handles them on the PR side: the `#5117` self-authorization hold,
  which is off by default at L6.
- **Trusted doesn't mean unstoppable.** `require_labels`, existing holds, and
  a repo's `auto_merge` setting still apply. "Worked" and "merges on its own"
  in the tables only mean reporter trust isn't what stops it.

## Walk-through: why did Hive merge this without my `/lgtm`?

Real case, [#9758](https://github.com/hivecommons/hive/issues/9758) →
[#9762](https://github.com/hivecommons/hive/pull/9762). A contributor
(`author_association: CONTRIBUTOR`) filed #9758, a small, well-scoped
documentation/text bug (wrong kernel module names in a remediation message).
The hive's scanner/quality lane picked it up, opened #9762 fixing it, and the
PR merged about 79 minutes after it was requested — with **no** prow `/lgtm`
or `/approve`, and no `hold` label at any point.

What allowed each step, in order:

1. **Admission.** The reporter's association was `CONTRIBUTOR`. Whatever this
   repo's reporter-trust configuration was at the time, #9758 was admitted:
   either reporter trust was off (association never gates admission), or it
   was on with `CONTRIBUTOR` explicitly added to `trusted_associations` — the
   open-community posture above. The one configuration that would have
   required a maintainer to add `triage/accepted` first is reporter trust on
   *with the trusted-team default* (`OWNER`/`MEMBER`/`COLLABORATOR` only,
   `CONTRIBUTOR` excluded) — that did not happen here, since #9758 was worked
   with no triage label.
2. **No level-hold on the PR.** The repo runs at an ACMM level where this
   agent's PRs are not held-gated, so #9762 never carried the level-hold
   `hold` label the L3–L5 packs would apply.
3. **No prow gate.** Prow bot activity on the PR (`dco-signoff: yes`,
   `size/XS`) is CI plumbing, not a merge gate for Hive's own merges — Prow's
   `tide` merge queue requires `lgtm`+`approved` labels that only a *human
   reviewer* can apply, and the Forge App can never review its own PR. Hive's
   self-merge sweep exists specifically because of that: it merges the App's
   own green PRs directly over the REST API, bypassing tide entirely. See
   [operator-reference.md § App self-merge
   sweep](operator-reference.md#app-self-merge-sweep-auto_merge).
4. **Green CI.** The self-merge sweep only merges PRs that are clean,
   non-draft, and green — #9762's CI passed, so the sweep merged it on its
   next pass.

The behavior was correct for that repo's configuration, but as the issue that
prompted this guide notes, confirming it took a maintainer several docs plus
the GitHub timeline. That's the gap this page closes.

## Q&A for first deployments

**Can an untrusted reporter get code merged?**
No, not unattended. Their issue waits for `triage/accepted` (or your
configured label) if reporter trust is on. Once admitted (or if reporter
trust is off entirely), a resulting PR is still held for a human at **every**
ACMM level, including L6 — reporter trust's merge-side check runs
independently of the level gate. "Untrusted" means *outside the trusted
associations/logins*, not "no merge history": `OWNER`, `MEMBER`, and
`COLLABORATOR` are trusted by default even for an account with zero merged
commits, because GitHub itself vouches for the relationship (org membership
or repo collaborator access), which a merge count does not measure.

**Does prow `/lgtm` / `/approve` / tide gate Hive's own merges?**
No. Tide's `lgtm`+`approved` requirement governs the **human queue** path
(`governor.labels.automerge`, default label `lgtm` — a merger/owner applies
it and Hive squash-merges once CI is green). It structurally cannot gate a
PR the Forge App itself opened, because the App can't review its own work;
that's exactly why the separate self-merge sweep exists, and it bypasses
tide by merging directly over the REST API. See
[contributor-trust-and-roles.md](contributor-trust-and-roles.md) and
[operator-reference.md § App self-merge
sweep](operator-reference.md#app-self-merge-sweep-auto_merge).

**What does `CONTRIBUTOR` actually mean? Why is it off by default?**
GitHub reports `CONTRIBUTOR` when the account has at least one commit merged
into the repository's default branch, at any point in its history — one
merged typo fix qualifies forever. It's excluded from the default trusted set
deliberately: a single past contribution doesn't make someone a maintainer,
so `DefaultTrustedAssociations` is `OWNER`, `MEMBER`, `COLLABORATOR` only
(`src/pkg/config/reporter_trust.go`). Add `CONTRIBUTOR` explicitly (the
open-community posture above) if your project wants to extend
unattended-merge trust to anyone with contribution history.

**Is an org `MEMBER` or `COLLABORATOR` trusted even with no merges?**
Yes. `ReporterTrustConfig.Trusted()` checks the reporter's
`author_association` (or an explicit login match) — it never looks at merge
or PR history. The association alone is enough.

**What exactly does `triage/accepted` unlock, and what does it not unlock?**
It unlocks **admission**: an agent may now work the issue (open a PR about
it, comment, classify it) the same as any other actionable issue. It does
**not** unlock unattended merge. If the resulting PR's rationale traces back
to that untrusted-reporter issue, the reporter-trust merge-side check still
applies `hold` at every level — a human still has to remove that label.

**How do I stop all auto-merges right now?**
Drop below L6 — merge permission is not granted below L6 at the token/proxy
level, so no config error or race can produce an unattended merge. For a
single in-flight item instead of the whole hive, apply the dashboard's
`hive-pause/<hive-id>` label to that issue or PR (see [Hive Labels and
Control Signals](labels-and-control-signals.md)); it is a manual hold
distinct from the level-hold `hold` label and from the provenance-only
`hive/<hive-id>` label.

**What changes for existing hives when I turn reporter trust on?**
Nothing immediately for maintainer-filed issues — `OWNER`/`MEMBER`/
`COLLABORATOR` issues flow exactly as before. Issues from anyone else stop
being actionable until triaged, and once reporter-trust is enabled its PR-side
hold defaults on too (`github.reporter_trust_hold` follows
`reporter_trust.enabled` unless you set it explicitly), so any PR tracing to
an untrusted-reporter issue starts getting held for review even at L6. It is
opt-in for exactly this reason — an existing hive that takes issues from the
public changes nothing until you flip the switch.

**Which levels can open PRs, and which can merge?**
See the [ACMM policy matrix](acmm-policy-matrix.md) for the authoritative,
per-agent table. In summary: L1–L2 agents never open PRs (advisory only); at
L3 only `quality` can (held); at L4 `quality`, `ci-maintainer`, and
`sec-check` can (held); at L5 every non-paused holdgated agent can, and every
PR it opens is held — `reviewer` is `converse` (comments and reviews, never
opens a PR) and `adjudicator` is `issues+prs`; at L6 every non-paused full-mode
agent can open **and merge** on green CI, except `outreach` PRs, which stay
held at every level `outreach` exists (L6 only), and `reviewer`/`adjudicator`,
which never merge at any level.

**How do I trust one specific outside person without trusting a whole
association?**
Add their login to `trusted_logins` — it's checked before association and
overrides it, so an external maintainer whose GitHub association reports as
`NONE` (no org membership, no collaborator grant) can still be treated as
trusted by name:

```yaml
project:
  issue_filter:
    reporter_trust:
      trusted_logins: [external-maintainer]
```

## Checklist before going to L6

- [ ] You've spent real weeks at L3–L5 and the PRs you've reviewed matched
      your judgment consistently — see [Getting Started § Trust >
      Level](getting-started.md#trust--level-always). There's no calendar
      requirement; the requirement is that you're not surprised.
- [ ] Your test suite is strong enough that green CI genuinely means "safe to
      ship" — L6 has no human PR gate for non-outreach agent work, so tests
      are the only backstop left.
- [ ] You've decided your reporter-trust posture (off, trusted-team, or
      open-community above) *before* enabling L6, not after — an L6 hive with
      reporter trust off will merge a total stranger's request the moment CI
      is green.
- [ ] If you take public issues at all, reporter trust is on, with the
      association set you actually intend (does this project want
      `CONTRIBUTOR` trusted, or not?).
- [ ] You know how to pull the emergency brake: drop the level, or apply
      `hive-pause/<hive-id>` to one item — see the Q&A above.
- [ ] `outreach`'s PRs are still held at every level including L6 by design;
      don't expect that to change without a config option, because there
      isn't one.

## Glossary

- **hive** — one running instance of Hive: the governor, agents, dashboard,
  and state for one deployment. Every hive is a **spoke**. See
  [architecture.md § 8](architecture.md#8-hub--spoke).
- **hub** — the one hosted instance (`hive.hivecommons.dev`, or your own
  self-hosted hub) that registers, provisions, and observes many spokes. Same
  container image as a spoke; `HIVE_MODE=hub` selects the role. See
  [architecture.md § 8](architecture.md#8-hub--spoke).
- **spoke** — a hive, from the hub's point of view: it pushes heartbeats and
  receives callbacks (config, banners, branch switches, authorized users).
- **ACMM, ACMM level (L1–L6)** — the maturity model that maps a single dial to
  a per-agent roster and set of policy modes, from advisory-only (L1–L2)
  through fully autonomous merge-on-green (L6). See the [ACMM policy
  matrix](acmm-policy-matrix.md).
- **policy mode: advisory, measured, holdgated, full** — the four per-agent
  capability tiers. Advisory observes only; measured can file issues; holdgated
  can open PRs but every PR gets `hold`; full can open and merge PRs on green
  CI. See [acmm-policy-matrix.md § Policy
  Modes](acmm-policy-matrix.md#policy-modes).
- **agent / lane** — a configured AI worker (`scanner`, `quality`, `sec-check`,
  …) with its own mode, cadence, model, and kick template. "Lane" is the same
  concept viewed as a stream of work. See
  [agent-configuration.md](agent-configuration.md).
- **reporter trust** — the opt-in gate (`project.issue_filter.reporter_trust`)
  that admits or holds work based on *who filed the issue*, using GitHub's
  `author_association`, separately from any label. See
  [agent-configuration.md § Reporter
  trust](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled).
- **GitHub author association** — GitHub's own classification of an issue or
  PR author's relationship to the repo: `OWNER`, `MEMBER`, `COLLABORATOR`,
  `CONTRIBUTOR` (at least one merged commit, ever), `FIRST_TIME_CONTRIBUTOR`,
  `FIRST_TIMER`, `NONE`. Hive treats an unknown/missing association as
  untrusted.
- **always-trusted logins** — `project.issue_filter.reporter_trust.trusted_logins`,
  an explicit login list trusted regardless of association — for an external
  maintainer GitHub doesn't otherwise vouch for.
- **triage label (`triage/accepted`)** — the default label
  (`untrusted_require_labels`) that admits an untrusted reporter's issue for
  agent work. Hive checks that it is present, not who added it, so anyone with
  Triage access or higher on the repo can admit an issue. Does not by itself
  remove a PR-side reporter-trust hold.
- **`hold`** — the literal label multiple gates apply (level gate,
  reporter-trust hold, `#5117` self-authorization hold, SHA-hold, holdguard).
  Any label containing the substring `hold` is treated as a hard hold by
  enumeration and merge sweeps. See [Hive Labels and Control
  Signals](labels-and-control-signals.md).
- **`hive-pause/<hive-id>`** — the dashboard's exact, hive-scoped manual hold
  label; deliberately avoids the substring `hold` so it reads distinctly in
  the UI, but is enforced identically by the same hold predicate.
- **`hive/<hive-id>`** — provenance/migration marker only; it is *not* a hold
  label except as a temporary failed-migration fallback.
- **token tier** — the per-agent GitHub App installation token scope
  (`advisor`, `newcomer`, `contributor`, `trusted`) matched to the agent's
  policy mode; advisory tiers get read-only tokens that cannot even create
  issues, and only trusted tiers get contents/PR write. See
  [security-model.md § Layer
  5](security-model.md#layer-5--github-blast-radius-controls).
- **GitHub App install scope** — the set of repositories you installed the
  Forge App on; a hive can never act outside it, GitHub-side.
- **repo allowlist** — the hive-side configured repo list
  (`project.repos`), enforced twice: hard, at the network policy proxy, and
  again as a prompt-level `AUTHORIZED REPOS` constraint in every kick.
- **prow, tide, `/lgtm`, `/approve`** — the Kubernetes/Prow CI bot suite some
  repos run alongside Hive. `tide` merges PRs that collect `lgtm`+`approved`
  labels from human reviewers; it cannot gate a PR the Forge App opened,
  because the App can't review its own work, so Hive's self-merge sweep
  merges the App's own green PRs directly, bypassing tide. Human-queued
  auto-merge (`governor.labels.automerge`, default `lgtm`) is a distinct,
  human-decision-gated path. See [operator-reference.md § App self-merge
  sweep](operator-reference.md#app-self-merge-sweep-auto_merge).
- **DCO sign-off** — the Developer Certificate of Origin trailer
  (`Signed-off-by:`) every commit must carry, added by `git commit -s`; agent
  policies require it, and pairs with a repo-side DCO check. See
  [CONTRIBUTING.md § DCO sign-off](../../CONTRIBUTING.md#dco-sign-off).
- **ClankeR, contributor trust tier (`newcomer`, `contributor`, `trusted`,
  `merger`, `advisor`)** — a *different* trust model from reporter trust: it
  governs what a community member's own ClankeR relay may claim from
  `/contribute` (starting rate-limited, auto-promoted after 5 completed
  tasks, then operator-granted upward). It never changes what a hive's own
  agents may do to GitHub issues/PRs filed by anyone. See
  [contributor-trust-and-roles.md](contributor-trust-and-roles.md).
- **governor** — the queue-depth scheduler: each eval cycle it enumerates
  actionable work, applies deterministic filters, and decides which agent to
  kick. See [architecture.md § 3](architecture.md#3-the-governor-loop--from-queue-depth-to-a-kick).
- **pack** — one of the six built-in ACMM configurations (`level-1.yaml` …
  `level-6.yaml`) that pairs a curated agent roster with governor cadences and
  a merge policy for that level. See [agent-configuration.md § ACMM levels:
  agent rosters as packs](agent-configuration.md#acmm-levels-agent-rosters-as-packs).
- **beads** — Hive's internal work ledger (`bd` CLI): findings, tasks, and
  decisions recorded as records with status, priority, and actor, independent
  of GitHub issues. See [beads-cli.md](beads-cli.md).
- **ioscan** — the untrusted-input scanner that redacts prompt injection,
  secrets, and hidden-instruction text out of GitHub issue/PR/comment content
  before it reaches an agent kick; `fail_mode` defaults to `closed` (block)
  at ACMM L5–L6 and `open` (redact and continue) below that. See
  [ioscan.md](ioscan.md).

## Where to go next

- [Security model](security-model.md) — the full seven-layer control map.
- [ACMM policy matrix](acmm-policy-matrix.md) — the authoritative per-level,
  per-agent capability table.
- [Security threat model](security-threat-model.md) — attacker-oriented view:
  assets, trust boundaries, threat actors, residual risks.
- [Agent configuration § Reporter
  trust](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled) —
  the full config reference for the reporter-trust gate.
- [Contributor trust tiers and delegated agent
  roles](contributor-trust-and-roles.md) — ClankeR's separate trust model.
- [Hive Labels and Control Signals](labels-and-control-signals.md) — every
  label and non-label control, what applies it, and what clears it.
