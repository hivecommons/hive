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

## Reporter trust and escalation

The hive automatically works issues only from authors whose GitHub `author_association` is one of:

- **OWNER** — an owner of the organization that holds the repository.
- **MEMBER** — a member of that organization.
- **COLLABORATOR** — someone who was explicitly invited and granted access to the repository (see GitHub's docs on [repository collaborators](https://docs.github.com/en/organizations/managing-user-access-to-your-organizations-repositories/managing-outside-collaborators/adding-outside-collaborators-to-repositories-in-your-organization)). This is GitHub's term, not a Hive-specific role.

Issues from anyone else wait until a maintainer adds the `triage/accepted` label. See [Securing your hive](src/docs/securing-your-hive.md) for the full table of associations.

If your issue is being ignored, mention a maintainer from the table above in the issue. If there is no response after about 7 days, raise it on the [hivecommons-dev Google Group](https://hivecommons.dev/join), on [Discord](https://hivecommons.dev/discord), or at the [community meeting](https://hivecommons.dev/agenda).

## Conduct and security

All participants must follow the [Code of Conduct](CODE_OF_CONDUCT.md). Security issues are reported privately through [SECURITY.md](SECURITY.md), not public GitHub issues.
