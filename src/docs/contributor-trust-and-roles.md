# Contributor trust tiers and delegated agent roles

Hive exposes two related but separate authorization models:

- **ClankeR contributor trust tiers** on `/contribute` decide what a community contributor relay may claim.
- **Hub dashboard access roles** in **Manage Access** decide what an authenticated dashboard user may do to a hosted hive.

They intentionally overlap in names only where the capability is similar. A contributor's AI CLI still runs with the contributor's own GitHub identity and scoped credential; taking a role never grants the spoke agent's secrets.

## Contributor trust tiers

Contributor profiles carry a `trust_tier` field. The current tier order is:

| Tier | How it is reached | What it means |
| --- | --- | --- |
| `newcomer` | default self-registration | Rate-limited entry tier. |
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
