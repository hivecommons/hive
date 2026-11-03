# Securing your hive: a first-time operator's guide

Last reviewed: 2026-10-23

You have connected a hive to a repository. Before you pick a level and walk
away, you need one answer: **who can make Hive do what in my repo?** The
mechanisms are documented in [security-model.md](security-model.md), the
[ACMM policy matrix](acmm-policy-matrix.md),
[agent-configuration.md](agent-configuration.md#reporter-trust-who-filed-it-not-only-what-it-is-labelled)
and [contributor-trust-and-roles.md](contributor-trust-and-roles.md). This
page explains what the settings mean for a stranger filing an issue against
your repo tomorrow morning.

**Two terms used below:** a **PR** is a pull request proposing a change.
**CI** is continuous integration: automated tests and checks run on that change.
A green check is useful only if it tests the behavior you need to protect.

## The 60-second version

Three settings decide who can make Hive act, and how far:

1. **ACMM level** (`acmm_level`, 1–6). The big dial. It decides which agents
   run and whether each one can only observe, file issues, open held pull
   requests, or open and merge pull requests.
2. **Reporter trust** (`project.issue_filter.reporter_trust`, off by
   default). Decides whether Hive treats issues from maintainers differently
   from issues from strangers, based on who GitHub says filed them.
3. **GitHub App install scope and repo allowlist.** The repositories you
   installed the App on, or listed in `project.repos`, are the only ones a
   hive can touch. The network proxy enforces this.

A fourth fact you do not configure: GitHub's `author_association` on every
issue and pull request (`OWNER`, `MEMBER`, `COLLABORATOR`, `CONTRIBUTOR`,
`FIRST_TIME_CONTRIBUTOR`, `FIRST_TIMER`, `NONE`).

## Pick a starting posture

Each posture assumes the repo allowlist covers only repos you want Hive to
touch.

### Cautious: L4 or L5, reporter trust off

A stranger files an issue. An agent may act on it, but every pull request it
produces is held for a human. At L5 every agent pull request gets the `hold`
label. At L4 only the `quality`, `ci-maintainer` and `sec-check` agents can
open pull requests, and those are held; the other agents only observe or file
issues. Known contributors get the same treatment, because reporter trust is
off and Hive looks only at labels and gates.

**Who merges:** a human, by reviewing and releasing held pull requests.

### Trusted team: L6, reporter trust on, default trusted set

```yaml
project:
  issue_filter:
    reporter_trust:
      enabled: true
```

Issues from `OWNER`, `MEMBER` or `COLLABORATOR` authors are worked as before,
and at L6 the resulting pull request merges on green CI. Issues from anyone
else wait until a maintainer adds `triage/accepted`. Once accepted, an agent
may work the issue and the pull request may merge; the acceptance label
counts as sign-off. A pull request linked to an unaccepted issue from an
untrusted reporter is held at every level, L6 included. Accepting the issue
later does not release an existing hold.

**Who merges:** agents, for team-filed or accepted work. Unaccepted requests
from strangers need human sign-off.

### Open community: L6, reporter trust on, also trusting CONTRIBUTOR

```yaml
project:
  issue_filter:
    reporter_trust:
      enabled: true
      trusted_associations: [OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR]
```

Anyone with at least one merged commit in the repo is trusted like a team
member. First-time strangers still need `triage/accepted`.

**Who merges:** agents, for anyone with history in the repo or an accepted
issue.

## The reporter-trust matrix

Both tables assume reporter trust is on with the default boxes, the triage
label is `triage/accepted`, and the hive is at L6.

| | No `triage/accepted` | Has `triage/accepted` |
|---|---|---|
| **Filed by a trusted person** | Worked. Hive's pull request merges on green CI. | Worked. The label changes nothing. |
| **Filed by anyone else** | Not worked. | Worked. Hive's pull request may merge on green CI. |

"Trusted" means the author's association is one of the checked boxes, or
their login is under **Always-trusted logins**.

| Filed by | How someone gets here | Default | Worked without label? | Worked with label? |
|---|---|---|---|---|
| `OWNER` | Owns the repo or org | trusted | Yes | Yes |
| `MEMBER` | Member of the org that owns the repo | trusted | Yes | Yes |
| `COLLABORATOR` | Invited to the repo | trusted | Yes | Yes |
| Always-trusted login | You added their login on the Labels tab | trusted | Yes | Yes |
| `CONTRIBUTOR` | Has committed to this repo before | not trusted | No | Yes |
| `FIRST_TIME_CONTRIBUTOR` | Has commits elsewhere on GitHub, none here | not trusted | No | Yes |
| `FIRST_TIMER` | Has never committed on GitHub | not trusted | No | Yes |
| `NONE` | No relationship to the repo | not trusted | No | Yes |

GitHub, not Hive, assigns the association. `CONTRIBUTOR`,
`FIRST_TIME_CONTRIBUTOR` and `FIRST_TIMER` come from commit history alone and
none is trusted by default, so **commit history alone never makes anyone
trusted** unless you check `CONTRIBUTOR`. The trusted rows change only when
someone is invited or joins the org. If a bot or team sync grants access
automatically, whoever it grants becomes trusted too; in that case uncheck
that box and trust people by login instead.

### Things that catch people out

* **`MEMBER` means every member of the owning org**, including people with
  no access to this repo. If that is too broad, uncheck it and list logins.
* **`COLLABORATOR` says nothing about access level.** A read-only
  collaborator and an admin look the same to Hive.
* **Hive checks that `triage/accepted` is present, not who added it.**
  Anyone with Triage access or higher can add it.
* **The hold follows the issue author, not the pull-request author.** A pull
  request linked to any issue from an untrusted reporter (`Closes #N`,
  `Refs #N`, or issues the agent declared) is held unless that issue is
  accepted.
* **The hold covers only pull requests Hive opens.** A pull request a person
  opens goes through the repo's normal branch protection.
* **Issues filed by bots or by Hive itself skip reporter trust.** A separate
  self-authorization hold handles those, off by default at L6.
* **Trusted does not mean unstoppable.** `require_labels`, existing holds and
  the repo's `auto_merge` setting still apply.

## Why did Hive merge this without my `/lgtm`?

Real case: [#9758](https://github.com/hivecommons/hive/issues/9758) →
[#9762](https://github.com/hivecommons/hive/pull/9762). A `CONTRIBUTOR` filed
a small documentation bug. An agent opened a fix, and it merged 79 minutes
later with no `/lgtm`, no `/approve` and no `hold` label.

1. **Admission.** Reporter trust was either off, or on with `CONTRIBUTOR` in
   the trusted set. Only the trusted-team default would have required
   `triage/accepted` first.
2. **No level hold.** The repo was at a level where that agent's pull
   requests are not held.
3. **No Prow gate.** Prow's `tide` merge queue needs `lgtm` and `approved`
   labels from a human reviewer, and the App cannot review its own pull
   request. Hive's self-merge sweep merges the App's own green pull requests
   directly over the REST API, bypassing `tide`.
4. **Green CI.** The sweep merges only clean, non-draft, green pull requests.

The behaviour matched the configuration.

## Common questions

**How do I stop all auto-merges right now?** Drop below L6. Merge
permission is not granted below L6 at the token and proxy level. To hold a
single item, apply the `hive-pause/<hive-id>` label to that issue or pull
request.

**What changes when I turn reporter trust on?** Nothing for issues from
`OWNER`, `MEMBER` or `COLLABORATOR` authors. Issues from anyone else stop
being worked until triaged, and pull requests tracing to unaccepted
untrusted issues start being held, even at L6.

**Which levels can open and merge pull requests?** L1–L2: none. L3: only
`quality`, held. L4: `quality`, `ci-maintainer` and `sec-check`, held. L5:
every hold-gated agent, all held. L6: every full-mode agent can open and
merge on green CI, except cross-repo `outreach` (a destination not managed by
this hive), whose pull requests stay held, and
`reviewer` and `adjudicator`, which never merge. The per-agent table is the
[ACMM policy matrix](acmm-policy-matrix.md).

**How do I trust one outside person?** Add their login to `trusted_logins`.
It is checked before association and overrides it.

```yaml
project:
  issue_filter:
    reporter_trust:
      trusted_logins: [external-maintainer]
```

## Checklist before going to L6

- [ ] You have run at L3–L5 long enough that the pull requests you reviewed
      matched your judgment. There is no calendar rule; the rule is that you
      are not surprised.
- [ ] Your test suite is strong enough that green CI means safe to ship. L6
      has no mandatory human review by default. Configure the review gate
      and branch rules deliberately; required tests remain a core backstop.
- [ ] You chose your reporter-trust posture before enabling L6. An L6 hive
      with reporter trust off merges a stranger's request as soon as CI is
      green.
- [ ] If you accept public issues, reporter trust is on with the
      associations you intend.
- [ ] You know the emergency brake: drop the level, or apply
      `hive-pause/<hive-id>` to one item.
- [ ] You expect cross-repo `outreach` PRs to stay held at every level.
      Outreach into a hive-managed repo follows that repo's normal level gates.

## Glossary

* **hive**: one running instance of Hive: governor, agents, dashboard.
* **hub / spoke**: a hub registers and manages many spokes; a spoke is one
  hive governing one or a few repositories.
* **ACMM level**: the six-step autonomy dial (AI Codebase Maturity Model).
* **policy mode**: what one agent may do at a level: advisory (observe),
  measured (file issues), hold-gated (open held pull requests), full (merge).
* **reporter trust**: the opt-in filter that treats issues differently by
  who filed them.
* **author association**: GitHub's label for an author's relationship to the
  repo.
* **`hold`**: the label a level or the reporter-trust check applies to a
  pull request so a human must release it.
* **`hive-pause/<hive-id>`**: the manual label that stops Hive working one
  item.
* **token tier**: the GitHub permissions an agent's token carries, set by
  its mode.
* **DCO sign-off**: the `Signed-off-by` trailer required on every commit.

## Where to go next

* [ACMM policy matrix](acmm-policy-matrix.md): the per-level, per-agent
  table.
* [security-model.md](security-model.md): every control and where it lives.
* [Getting started](getting-started.md): first deployment.
