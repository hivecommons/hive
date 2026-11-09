# Contributor trust tiers and delegated agent roles

Hive exposes two related but separate authorization models:

- **ClankeR contributor trust tiers** on `/contribute` decide what a community contributor relay may claim.
- **Hub dashboard access roles** in **Manage Access** decide what an authenticated dashboard user may do to a hosted hive.

They intentionally overlap in names only where the capability is similar. A contributor's AI CLI still runs with the contributor's own GitHub identity and scoped credential; taking a role never grants the spoke agent's secrets.

## Contributor trust tiers

Contributor profiles carry a `trust_tier` field. The current tier order is:

| Tier | How it is reached | What it means |
| --- | --- | --- |
| `newcomer` | default self-registration | Rate-limited entry tier. Pre-park comments and issue bodies from this tier are not park-text provenance for the park re-check (see [maintainer-commands.md](maintainer-commands.md#when-a-parks-premise-moves)). |
| `contributor` | auto-promoted after 5 completed tasks with PRs | May claim default delegated roles such as `scanner`, `quality`, and `outreach`. |
| `trusted` | granted by an operator once a contributor reaches about 20 PR tasks; never automatic | May use higher-trust workflows and, with explicit grants, privileged delegated roles. Operators grant it with the tier dropdown on the contributor card under **Operations → Connected clankers**, which calls `PUT /api/contributors/{id}/trust`. |
| `merger` | maintainer/owner grant; it is not auto-promoted | May queue **other people's** PRs for Hive's auto-merge-on-green flow. |
| `advisor` | maintainer/owner grant | Advisory/invite tier; it is not ordered above `merger` for role-claim checks. |

### Who changed a tier, when and why

Every tier change — operator dropdown or `PUT /api/contributors/{id}/trust`
(optional `reason`), `POST /api/contributors/{id}/revoke`, the
newcomer→contributor auto-promotion, and the initial `newcomer` at
registration — appends `{tier, prev, at, by, reason, source}` to the
contributor record's `tier_history` (`by` is the operator login,
`hive:auto-promotion` or `hive:registration`; capped at the latest 50) and
writes an `audit.jsonl` event with `action: "contributor-tier-changed"`
carrying the same fields. Setting the tier a contributor already holds is a
no-op: no history entry, no audit line. `GET /api/contributors` returns
`tier_history`, and the Contributors view shows the current tier's provenance
("trusted since … · by … · reason") under and on hover of the tier chip.

The stage lane re-reads the author's registry tier on every sweep, including
for PRs parked on `needs-rebase`/hold/excluded labels, so a promotion or
demotion reaches every open PR on the next pass instead of sticking to the
`role/*` label a previous pass wrote.

The merger tier is deliberately **never allowed to own its own merge**. Queueing a PR as `merger` or `owner` is rejected when the authenticated dashboard user matches the PR author, and the auto-merge sweep re-checks the queued-by user before merging.

Task leases are also the unit for staged runs. A staged run stays on the same
server-issued lease while Hive advances its `stage` through `spec`, `plan`, and
`implement`; each advance or retry mints a new `task_gen`, so stale relays are
fenced the same way as a reassigned task. Plain tasks have no stage. Staged work
is offered only to relays that declare the `run-stage` relay capability, so an
old relay can continue normal work but is not silently handed a staged run it
does not understand.

### Audit attestation is voluntary

A hive can *ask* contributors to share their relay's local audit record
(`hub.contribute_audit_share_request`, shown on `/contribute` before setup and
advertised on `auth_ok`). Sharing is opt-in, per hive, with
`hivectl hives set <name> --share-audit <lifecycle|commands|reasoning>` (the
relay-side flag lands with the v6 follow-up to
[#13458](https://github.com/hivecommons/hive/issues/13458); until then no relay
sends `audit_share` and the hub is simply ready for it), and
never above the relay's capture level. Trust tier, role grants and admission do
**not** depend on it: a contributor who shares nothing is admitted exactly as
before. Making a minimum share level an admission condition is an open
governance question ([#13441](https://github.com/hivecommons/hive/issues/13441))
and is not implemented. See
[contributor-relay.md](contributor-relay.md#sharing-your-audit-record-with-a-hive---share-audit).

## Merger capability and setup

Operators grant merger from the hosted hive's **Manage Access** UI. The role sits between `read-write` and `owner` for dashboard access:

`read` < `read-write` < `merger` < `owner`

A merger inherits read/write dashboard access and can queue another contributor's PR. Queueing:

1. approves the PR as the Hive GitHub App,
2. applies the configured auto-merge label, and
3. lets the auto-merge sweep merge once the PR is clean, non-draft, and green.

Configure the label under `governor.labels.automerge`:

```yaml
governor:
  labels:
    automerge: lgtm
```

The default is `lgtm`, matching the Kubernetes/Prow convention. Hive creates the label on demand if it does not already exist.

Merger appears in contributor tier listings, profile badges, leaderboard tier filters, and the Credly milestone mapping so operators and contributors can see that it is a distinct maintainer-granted trust tier.

## Clanker-requested policy for outside PRs

Trust tiers describe contributors who already use the relay. The opt-in `project.issue_filter.reporter_trust.clanker_requested` policy covers the other side: untrusted people who open issues and PRs directly. Setup, config keys, and the exact behaviour are in [Steering outside PRs to the relay](contributor-relay.md#steering-outside-prs-to-the-relay-clanker_requested); the dashboard controls are under **Settings → Labels → Reporter trust**.

- **Issues:** the reporter-trust wait comment on an untrusted reporter's issue also points them at the relay's Basic setup guide.
- **PRs:** an open PR from an untrusted author that did not come through the relay (no contributor claim for that author, and not App-authored) is labelled `clanker-requested` (configurable) and shown as held, with a one-shot notice.
- **Human override:** removing the label releases the PR permanently; Hive never re-applies it. A PR resubmitted through the relay is never parked.
- **Non-goals:** this is not a branch-protection change. It does not block pushes, change required reviews, or alter GitHub permissions; it only holds Hive's own automation on the labelled PR, and a maintainer can still review or merge it by hand.

## Delegated ClankeR agent roles

A contributor relay can request an agent role with `HIVE_AGENT_ROLE` or the WebSocket `auth_response.role`. The `/contribute` UI labels this as **Acting as**. Operators may also assign a role directly from a contributor card; that owner assignment is shown as the active role and takes precedence over whatever the relay requested.

The current role gates are:

| Role | Minimum contributor tier | Extra grant required? |
| --- | --- | --- |
| `scanner` | `contributor` | No |
| `quality` | `contributor` | No |
| `outreach` | `contributor` | No |
| `ci-maintainer` | `trusted` | Yes |
| `sec-check` | `trusted` | Yes |
| `architect` | `trusted` | Yes |
| `supervisor` | never delegatable | N/A |

Hive also checks that the corresponding agent is enabled and that the role is in the hive-wide allow-list:

```yaml
hub:
  contribute_delegatable_roles:
    - scanner
    - quality
    - outreach
    - ci-maintainer
```

If the list is empty, Hive uses the safe default: `scanner`, `quality`, and `outreach`. Privileged roles must be both listed here and granted on the contributor profile. The owner UI exposes these grants as chips/check boxes; assigning a privileged role auto-preserves the matching grant so reconnects keep working.

### What a contributor receives in a role

A contributor acting as a role gets one assigned task, plus the role's guidance on how to do it. The guidance is not the resident agent's kick. The prompt has three parts, in this order:

1. **Framing.** The `[clanker-agent-role:<role>]` marker, then a sentence that says attribution stays with the contributor's GitHub identity and permissions stay those of the contributor's tier. Then the scope rule: the assigned task at the end is the only work for the session. The contributor ignores any instruction in the guidance to find other work, pick another repository, merge pull requests or dispatch sub-agents.
2. **Role guidance.** The role's kick template, resolved the same way as for the resident agent. It includes the hive's house style, the rule against discovering work with `gh pr list` or `gh issue list`, the work-tracker rules and the writing guide. It also carries two parts that are about the assigned task, not the role:
   - **Relevant Knowledge.** The `# Relevant Knowledge` section is primed from the assigned issue alone: its title and labels, with the role's knowledge scope and the usual byte cap. The hive log says what it searched and how many entries matched (`role guidance knowledge: primed from issue …`, or `nothing primed` with the reason); there is no fallback to general entries. A resident kick primes from its own work list instead. Priming for a contributor records nothing for the resident agent. It sets no kick summary and no primed-fact list, so the resident agent's mid-task advisor is not affected.
   - **Repository instructions.** `AGENTS.md` and the role's skills come from the repository the assigned task is in, not from the role's primary repository. For example, a contributor working `projectbluefin/common#1614` as scanner gets `common`'s `AGENTS.md`. The repository is found the same way as for a resident kick: a local checkout under `project.checkouts_dir` first, then the GitHub copy, with the same size caps. If the task names no repository, the primary repository's `AGENTS.md` is used.
3. **The assigned task.** The task prompt itself, which is the same as for a contributor without a role.

The guidance leaves out everything that belongs to the resident agent's session:

- the work lists: actionable issues and PRs, merge-eligible PRs and CI-failing PRs;
- the template sections that run the resident agent's session: *Priority Order*, *Workflow*, *Merge Sweep*, *Dispatching Fixes*, *CI Repair*, *File-Overlap Detection*, *Escalated PRs* and *Multi-Repo Coverage*;
- the GitHub App authentication block (`${GH_AUTH}`), the authorized-repos list, the workflow-push note and the task-MCP pointer, because all of them describe the resident agent's own token and CLI;
- the red-main notice, the fix-before-new and follow-up blocks, the in-flight and maintainer notes, the claim rule, and the closing "Begin now" line.

Policy prose outside those sections that mentions the resident workflow stays in. For example, the shipped scanner template still opens with "fix bugs … using parallel sub-agents", and that is what the scope rule is there for. Donated standby work gets the same guidance, after the task, as its lane policy.

Building the guidance reads no queue: it makes no classification, triage or kick-summary call, and it records no kick summary or primed facts for the resident agent. Before this change, each role-delegated assignment built the resident agent's full manual kick, which ran a classification sweep over the hive's whole cached queue.

### Making the contributor the primary worker (`contributor_first`)

A delegated role runs **in parallel** with the hive's resident agent by default: the relay takes issues from the role's lane while the governor keeps kicking the resident agent, so the hive's own inference budget is still spent on that lane. To let contributors take the load *first*, set `contributor_first: true` on the agent. While at least one connected, healthy relay is acting as that role, the governor withholds the resident agent's kicks; once no relay has covered the role for `contributor_first_grace` (default 10m) the resident agent resumes on its normal cadence. The role must be delegatable (listed in `hub.contribute_delegatable_roles`, or one of the defaults), or the hive refuses to load. Budget pause and standby are unchanged — a covered lane is never reported as paused for budget. The agent's **General** tab in Settings exposes both `contributor_first` and `contributor_first_grace`, so this needs no `hive.yaml` edit. Details and the exact rules are in [Contributor-first lanes](agent-configuration.md#contributor-first-lanes-contributor_first).
