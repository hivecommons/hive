# RFC and proposal process

How larger changes to Hive are proposed, decided and tracked in the open. This page describes what the project actually does today; the authority for who decides is [GOVERNANCE.md](../../GOVERNANCE.md#decision-process) and [OWNERS](../../OWNERS).

## When an RFC is needed

Open an RFC when the change is a design question that people should argue about before code exists:

- structural or protocol-level changes, which the [branch table in CONTRIBUTING.md](../../CONTRIBUTING.md#branches) and [ROADMAP.md](../../ROADMAP.md) route through a public RFC issue before they land on a release line;
- scope for a new release line or a new workstream on one;
- behavior changes that affect operators or agents, such as a new trust boundary, a new write path, or a changed default.

A plain issue or pull request is enough for a bug fix, a docs change, a dependency update, or a feature whose shape is not in dispute. If you are unsure, open an issue and a maintainer will say whether it needs an RFC.

## How to file one

1. Open an issue from the **RFC / design proposal** template. It sets the title prefix `📝 RFC: ` and the `proposal` label.
2. Title it `📝 RFC: <short summary>`. Many RFCs also name the target line, for example `📝 RFC: v7 - Hub registration protocol for self-hosted hives`.
3. In the body, cover the problem you hit, the proposal, the open questions you want answered, what is out of scope, and which issues or PRs it relates to.
4. Discuss in the issue. A design document under `docs/rfc-*.md` is added by pull request when an RFC grows past what an issue body can hold (for example [`rfc-17054`](../../docs/rfc-17054-llm-ranked-knowledge-advisor.md)); it refers back to the issue, which stays the place for the decision.

### Older titles and tags

Older RFCs use other prefixes: `[v5 RFC]` (for example #5480, and the `📝 [v5 RFC]` form on #16075) and `[v5] RFC:` (#5691, #5698). They are the same process: [CONTRIBUTING.md](../../CONTRIBUTING.md#branches) and [ROADMAP.md](../../ROADMAP.md#v5--current-stable-line) refer to `[v5 RFC]` issues gating structural changes on `v5`. Some RFCs carry `kind/design` as well as `proposal`. The hive treats an open issue with `proposal` or `kind/design`, or an unlabelled one whose title opens with `RFC:` or `[RFC]` after any leading emoji, and with no live claim, as **Open for discussion** (`stage/018-open-for-discussion`), not as agent work waiting to be picked up; see [stage labels](labels-and-control-signals/stage-labels.md).

## Who decides

The same people who decide everything else here. [GOVERNANCE.md](../../GOVERNANCE.md#decision-process) says proposals are opened as issues or pull requests, discussed in public, and that maintainers seek consensus, with controversial or cross-project decisions escalated to the Hive Commons maintainer committee. The maintainers are listed in [GOVERNANCE.md](../../GOVERNANCE.md#maintainers-and-authority) and [OWNERS](../../OWNERS). There is no separate RFC committee, voting rule or fixed comment period.

In practice a maintainer states the decision in a comment on the RFC issue. Recent examples:

- [#18464](https://github.com/hivecommons/hive/issues/18464): the owner proposed "accept with scope cuts", naming a phased plan and what is deferred, and the author confirmed it as accepted as scoped.
- [#18642](https://github.com/hivecommons/hive/issues/18642): the owner posted decisions on each open question, lifted the hold, and announced the port to `v5`.

## What accepted means

Accepted means a maintainer has recorded, on the RFC issue, that the direction is agreed and what is in scope, with any cuts or phases. It is not a label and not a merge. Nothing is accepted while the open questions are undecided: an RFC such as [#17091](https://github.com/hivecommons/hive/issues/17091) says so in its body.

- **Held:** the `hold` label means no hive agent picks the issue up for implementation until the open questions are settled. [#17091](https://github.com/hivecommons/hive/issues/17091) and [#18464](https://github.com/hivecommons/hive/issues/18464) are held RFCs. A maintainer lifts the hold when the questions are decided, as on [#18642](https://github.com/hivecommons/hive/issues/18642).
- **Approved direction:** some RFCs also carry `approved-direction`, for example [#17054](https://github.com/hivecommons/hive/issues/17054). It is applied by hand and is not applied to every accepted RFC, so read the decision comment, not only the labels.
- **Declined:** the maintainer says so in a comment and closes the issue.

## How accepted RFCs are tracked and closed

- Implementation work is split into child issues and pull requests that link back to the RFC. Use `Refs #N` on a pull request that implements only part of an RFC, so the RFC is not closed early; the hive then shows the RFC as partially addressed and it returns to Open for discussion until the rest lands ([stage labels](labels-and-control-signals/stage-labels.md)). [#17054](https://github.com/hivecommons/hive/issues/17054) is tracked this way, as an epic with child slices.
- Accepted workstreams that become release-line work are listed in [ROADMAP.md](../../ROADMAP.md), with links to the RFC issue or design document.
- The RFC issue is closed when its implementation has landed: [#18642](https://github.com/hivecommons/hive/issues/18642) was closed as completed after its implementing pull request merged. A merged pull request to `v5` does not close issues on its own, so a maintainer or the hive closes the RFC and cites the pull request.

## Where to see them

- [Open RFCs and proposals](https://github.com/hivecommons/hive/issues?q=is%3Aissue+is%3Aopen+label%3Aproposal)
- [All RFCs and proposals, open and closed](https://github.com/hivecommons/hive/issues?q=is%3Aissue+label%3Aproposal)
- [Issues with `RFC` in the title, open and closed](https://github.com/hivecommons/hive/issues?q=is%3Aissue+RFC+in%3Atitle) (catches older RFCs filed without the label)
- [Held RFCs](https://github.com/hivecommons/hive/issues?q=is%3Aissue+is%3Aopen+label%3Aproposal+label%3Ahold)
- [Approved-direction RFCs](https://github.com/hivecommons/hive/issues?q=is%3Aissue+label%3Aapproved-direction)
- RFC design documents: `docs/rfc-*.md`, for example [rfc-5691](../../docs/rfc-5691-constellation.md) and [rfc-6825](../../docs/rfc-6825-capability-aware-contributor-task-assignment.md)
- [Accepted workstreams in the roadmap](../../ROADMAP.md)

There is no single "accepted" label, so the closest searches are closed RFCs with `is:closed` added to the second link, and the roadmap.
