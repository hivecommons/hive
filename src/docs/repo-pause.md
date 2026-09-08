# Per-repo agent pause

Quiet **one repository** without stopping the hive.

A hive is one process running a fleet of agents against the repositories listed
in `project.repos`. Before this, an operator could stop one *agent* (everywhere)
or stop *everything* (the fleet breaker). There was no way to say "stop agent
activity on this one repo, leave the rest of the hive running" — so a release
freeze, an incident, or one repo whose CI is red for an unrelated reason cost
the whole fleet, or cost the repo its place in `project.repos`.

Pause is a **run-state**. It is independent of which agents are paused and of
the repo's ACMM level, and it is not an edit to the hive's identity: a paused
repo stays in `project.repos`, keeps its dashboard card, keeps its counts, and
keeps its ACMM evaluation. Only agent activity stops.

## When to use it

- **Release freeze or incident.** A repo is mid-release and should receive no
  agent PRs or issues for a few hours. Every other repo keeps working.
- **Broken CI on one repo.** Agents keep filing the same failure against a repo
  whose CI is red for an unrelated reason, burning inference budget and creating
  issue noise you then have to close.
- **Staged onboarding.** A newly added repo should be declared in config — so it
  appears on the dashboard and gets its ACMM eval — while agents stay off it
  until you are ready.
- **Budget triage.** One repo is disproportionately expensive and you want it
  quiet until the next budget window, without losing the rest of the fleet.

## Pausing and resuming

### From the dashboard

Each card in **REPOSITORIES** carries a **⏸ pause** / **▶ resume** button
(owner role only). Pausing asks for a reason; the card then shows a **⏸ PAUSED**
pill whose tooltip names who paused it, when, and why.

### From the API

```bash
curl -X POST http://localhost:8080/api/repos/pause \
  -H 'Content-Type: application/json' \
  -d '{"repo": "console", "reason": "release freeze until Friday"}'

curl -X POST http://localhost:8080/api/repos/resume \
  -H 'Content-Type: application/json' \
  -d '{"repo": "console"}'

curl http://localhost:8080/api/repos/pauses
```

`pause` and `resume` are owner-only; the listing is read-only and open to any
authenticated role. Both toggles return `changed: false` for a no-op, so a stale
client cannot silently re-pause a repo you were trying to resume — and a
re-pause deliberately leaves the **original** provenance in place rather than
restamping who and when.

They also return `persisted`. The pause takes effect in memory immediately, so
on a deployment whose config file is mounted read-only you can get
`persisted: false` with a `warning`: the repo really is quiet, but the pause
will not survive a restart.

The repo travels in the request **body**, not the path, because a `project.repos`
entry may be an explicit cross-org reference (`laredo/cuga-agent`) whose slash
cannot live in a single path segment.

### From config

```yaml
project:
  org: acme
  repos:
    - console
    - dashboard
  paused_repos:
    - repo: dashboard
      by: bketelsen
      at: 2026-09-07T10:04:00Z
      reason: release freeze until Friday
```

Only `repo` is required; `by`, `at` and `reason` are the provenance the
dashboard and the API fill in for you. A hand-written entry with no `by`/`at` is
a valid state — "unknown", kept distinguishable from an attributed pause.

`repo` may be spelled either way `project.repos` spells it (a bare name or an
explicit `owner/name`), and matching is case-insensitive, so `Console`,
`console` and `acme/console` are the same repository.

The field is optional and additive: absent — the state of every existing config
— means nothing is paused and the hive behaves exactly as before.

## What a pause actually stops

Enforcement is **deterministic**, not a prompt instruction. A prompt-only pause
has already been observed to fail when an agent's model changes, so nothing here
depends on an agent reading the kick and choosing to comply.

| Layer | Behaviour on a paused repo |
| --- | --- |
| MITM GitHub proxy | Every agent **write** (`POST`/`PUT`/`PATCH`/`DELETE`, and `git push`) is answered `403` with a message naming the pause. Whatever mode the agent holds — including `ISSUES_PRS_MERGE`. |
| [`hive-open-pr`](hive-open-pr.md) relay | The request is rejected before any GitHub call and quarantined, with the reason in its result file. |
| [`hive-merge`](hive-merge.md) relay | Same, checked before the optional branch update so the repo receives no write at all. |
| [`hive-open-issue`](hive-open-issue.md) relay | Issue creation (including label setup), issue/PR comments and claim labels are rejected before any GitHub call. Requests are quarantined as `.denied`, with the pause reason in their result files. |
| `hive-review` relay | Approvals, change requests and review comments are rejected before any GitHub call, quarantined as `.denied`, with the pause reason in their result files. |
| Work enumeration | The repo contributes no actionable issues or PRs, so nothing downstream — kicks, claims, advisories, the contribute queue — can hand an agent work on it. |
| Auto-merge sweeps | Both the queued (`lgtm`) and self-authored sweeps skip the repo. |
| Kick text | The repo is absent from `AUTHORIZED REPOS` and from `$HIVE_REPOS`, and is named separately as paused so an agent does not read the shorter list as scope loss. |

The proxy and relay gates are both required, and neither is redundant. The proxy
hard-denies direct `POST /pulls` and `PUT /pulls/{n}/merge` for *every* agent
mode precisely so those operations route through the hive — and a request the
hive fulfils never traverses the proxy. Enforcing pause only at the proxy would
have left agents able to open and merge PRs, file issues, post comments, claim
issues and submit reviews on a paused repo.

Denied relay requests stay quarantined after resume; they are not replayed.
Submit a fresh request once the repository is resumed. Requests to other,
unpaused repositories continue normally.

### What a pause does **not** stop

- **Reads.** Pause stops the hive acting on a repo, not looking at one. `GET`,
  `HEAD`, `OPTIONS` and `git fetch`/`git clone` all still work, so an agent can
  still explain why it did nothing this session, and a reviewer agent working
  elsewhere can still read the paused repo's issues.
- **The hive's own control plane.** App-token minting, heartbeats and other
  hive-originated calls are exempt. Stopping them would take the whole spoke
  down to quiet one repo.
- **Your own activity.** Pause governs agents. Humans push, review and merge on
  a paused repo exactly as before.
- **GraphQL writes whose target repo is only in the request body.** Repo
  extraction reads the request *path*, so `POST /graphql` is not attributed to a
  repository — the same blind spot the existing repo-scope filter has. GraphQL
  mutations remain gated by ACMM mode. Close the gap for a hard freeze by
  pausing the relevant agents as well.
- **The ACMM level.** Pause and level answer different questions. No level means
  "off" — L1 still schedules agents and brainstorm is advisory at every level —
  so lowering a repo's autonomy cannot silence it.

## Provenance, and why it is not optional

An unexplained pause is its own support burden: a week later nobody can tell a
deliberate freeze from a malfunction. That was the lesson of #4041/#4042, and
#4055 answered it for agent pauses by recording who/when/why on the pause
itself. Repo pauses carry the same record, and it shows up in three places:

- the **⏸ PAUSED** pill's tooltip on the dashboard card,
- `GET /api/repos/pauses`,
- the hive log at boot, one line per paused repo, so an operator reading the log
  after a restart can see the hive came back up still holding a pause.

Pauses persist across restarts and upgrades — the same guarantee
`AgentConfig.Paused` had to earn — and the read-modify-write happens under the
same lock as every other config save, so a pause cannot be lost to a concurrent
write of an older snapshot.

A `paused_repos` entry naming a repo that is **not** in `project.repos` is inert
and is reported at boot as a warning rather than a fatal error. The entry is
kept: re-adding the repo restores its pause instead of silently un-pausing it.
The dashboard refuses to *create* such an entry — pausing a repo the hive does
not watch would report success while no enforcement point had ever heard of it —
but it will happily *clear* one, so a pause left behind by a removed repo does
not need a hand edit.

## Relationship to the alternatives

- **Removing the repo from `project.repos`** — the old workaround. It loses the
  repo from the dashboard and the ACMM eval, drops it from the GitHub client's
  scope wholesale, carries no provenance, is easy to forget to reverse, and
  reads as a permanent decision.
- **Pausing every agent** — stops the whole hive to quiet one repo.
- **A second hive per repo** — duplicates the process, dashboard, tmux sessions
  and fixed-cadence spend, and fragments the governor's view.
- **`hub.disabled_repos`** — per-repo, but read only by the contributor relay:
  it controls which repos are offered to *external contributors* and does not
  stop the hive's own agents.

## What to read next

- [`hive-open-pr`](hive-open-pr.md), [`hive-merge`](hive-merge.md) and
  [`hive-open-issue`](hive-open-issue.md) — relay usage. These and `hive-review`
  enforce the pause alongside the proxy.
- [ACMM policy matrix](acmm-policy-matrix.md) — the autonomy tiers pause is
  orthogonal to.
- [Audit log format](audit-log.md) — `repo_pause` / `repo_resume` entries.
