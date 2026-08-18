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
| `trusted` | auto-promoted after 20 completed tasks with PRs, or granted by an operator | May use higher-trust workflows and, with explicit grants, privileged delegated roles. |
| `merger` | maintainer/owner grant; it is not auto-promoted | May queue **other people's** PRs for Hive's auto-merge-on-green flow. |
| `advisor` | maintainer/owner grant | Advisory/invite tier; it is not ordered above `merger` for role-claim checks. |

The merger tier is deliberately **never allowed to own its own merge**. Queueing a PR as `merger` or `owner` is rejected when the authenticated dashboard user matches the PR author, and the auto-merge sweep re-checks the queued-by user before merging.

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

