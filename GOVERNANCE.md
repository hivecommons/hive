# Hive Project Governance

Hive is a CNCF Sandbox project in the Hive Commons organization. The historical KubeStellar governance file now points here and to the Hive Commons organization governance records.

## Maintainers and authority

The Hive maintainer committee owns day-to-day decisions for this repository: issue triage, PR review, merge decisions, release readiness, security response coordination, and project-specific policy changes. Current maintainer ownership is reflected in [OWNERS](OWNERS) and the Hive Commons organization [MAINTAINERS.md](https://github.com/hivecommons/.github/blob/main/MAINTAINERS.md).

Current maintainers:

| GitHub | Name | Affiliation |
| ------ | ---- | ----------- |
| @clubanderson | Andy Anderson | IBM |
| @hanthor | James Reilly | Universal Blue |
| @Danathar | Doug Baggett | independent |
| @nicholasjackson | Nic Jackson | IBM |
| @kellyaa | Kelly Abuelsaad | IBM |

Routine changes use lazy consensus through GitHub review. Maintainers may merge when CI and review expectations are satisfied, DCO sign-off is present, and no unresolved objection remains.

## Decision process

1. Proposals are opened as GitHub issues or pull requests.
2. Discussion happens in public on the issue or PR whenever possible.
3. Maintainers seek consensus. For routine changes, no objection after review is treated as approval.
4. Controversial or cross-project decisions may be escalated to the Hive Commons maintainer committee.

## Contributor ladder

Hive follows the [Hive Commons Contributor Ladder](https://github.com/hivecommons/.github/blob/main/MAINTAINERS.md), maintained in the org-level `.github` repository: **Contributor → Organization Member → Reviewer → Maintainer**. Contributors earn additional responsibility through sustained, high-quality participation. Requests or nominations for expanded responsibility should be raised with maintainers.

**Where "collaborator" fits.** GitHub *collaborator* is not a rung on this ladder. It is a GitHub repository-access grant, and it is one of the author associations the hive trusts (see [Reporter trust and escalation](#reporter-trust-and-escalation)). Policy for earning it:

- A maintainer may grant repository collaborator access to a Contributor with a sustained record of accepted issues or PRs.
- There is no self-service path. To request it, ask a maintainer in an issue or on [Discord](https://hivecommons.dev/discord).
- Organization Members (ladder level 2) are trusted automatically, so joining the organization is the normal path and makes collaborator access unnecessary.

See [How to become a trusted reporter](#how-to-become-a-trusted-reporter) for the steps.

## Reporter trust and escalation

The hive automatically works issues only from authors whose GitHub `author_association` is one of:

- **OWNER** — an owner of the organization that holds the repository.
- **MEMBER** — a member of that organization.
- **COLLABORATOR** — someone who was explicitly invited and granted access to the repository (see GitHub's docs on [repository collaborators](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-outside-collaborators/adding-outside-collaborators-to-repositories-in-your-organization)). This is GitHub's term, not a Hive-specific role.

Issues from anyone else wait until a maintainer adds the `triage/accepted` label. See [Securing your hive](src/docs/securing-your-hive.md) for the full table of associations.

If your issue is being ignored, mention a maintainer from the table above in the issue. If there is no response after about 7 days, raise it on the [hivecommons-dev Google Group](https://hivecommons.dev/join), on [Discord](https://hivecommons.dev/discord), or at the [community meeting](https://hivecommons.dev/agenda).

### How to become a trusted reporter

"Contributor" and "trusted reporter" are different things. Everyone who files an issue or PR is already a *Contributor* (ladder level 1 in the [org MAINTAINERS.md](https://github.com/hivecommons/.github/blob/main/MAINTAINERS.md)); the hive's "only works issues from OWNER, MEMBER, COLLABORATOR" message is about GitHub's `author_association`, not the ladder.

- **You do not need to become a member for a single issue to be worked.** A maintainer adds `triage/accepted` to that issue.
- **MEMBER** is Organization Member (ladder level 2): 5+ merged or accepted contributions (PRs, substantive issue triage, or doc changes) over at least 2 months, sponsored by 1 Maintainer, 2FA enabled, and a clean DCO history. Open an issue titled `Org membership request: @handle` listing the qualifying contributions; the sponsor approves, and any Maintainer adds you within 7 days absent objections.
- **COLLABORATOR** is repository access granted directly by a maintainer to a Contributor with a sustained record of accepted issues or PRs. It is not a ladder rung (see [Contributor ladder](#contributor-ladder)). There is no self-service path; ask a maintainer in an issue or on Discord. Most people should pursue MEMBER instead, which is trusted automatically.

## Conduct and security

All participants must follow the [Code of Conduct](CODE_OF_CONDUCT.md). Security issues are reported privately through [SECURITY.md](SECURITY.md), not public GitHub issues.
