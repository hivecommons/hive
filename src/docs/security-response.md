# Security Response Process

This page documents **who responds** to a vulnerability report and **how a
report is handled** once it reaches the project. For **where and how to
report** a vulnerability, see [`SECURITY.md`](../../SECURITY.md) — this page
does not duplicate or override that process.

## Who responds

Security response is a duty of the Hive **Maintainer Committee**, the same
body that owns day-to-day project decisions under
[GOVERNANCE.md](../../GOVERNANCE.md) and the upstream
[GOVERNANCE-HIVE.md](https://github.com/kubestellar/kubestellar/blob/main/GOVERNANCE-HIVE.md),
whose Maintainer Committee Duties explicitly include *"Responding to security
compromise reports."* There is no separate security team or roster: the
Maintainer Committee **is** the security response team.

The authoritative membership list is [`OWNERS`](../../OWNERS) — this page
intentionally does not restate it as a second list, because a second list can
drift out of sync with the real one. At the time of writing, `OWNERS` lists:

| Name          | GitHub                                           | Affiliation     |
|---------------|---------------------------------------------------|-----------------|
| Andy Anderson | [@clubanderson](https://github.com/clubanderson) | IBM             |
| James Reilly  | [@hanthor](https://github.com/hanthor)           | Universal Blue  |
| Doug Baggett  | [@Danathar](https://github.com/Danathar)         | independent     |

Any of the three can receive and triage a report; in practice, whoever picks
up a GitHub Security Advisory notification first drives it, and loops in the
others for confirmation, severity, and disclosure timing. There is no
published on-call rotation or single named point of contact beyond "the
Maintainer Committee via GitHub private vulnerability reporting" — see
[Known limits](#known-limits) below.

## How a report is handled

1. **Report received.** Through GitHub private vulnerability reporting (the
   repository's Security tab), or direct maintainer contact if that channel
   is unavailable to the reporter, per `SECURITY.md`.
2. **Acknowledgement.** The project aims to acknowledge within **5 business
   days**, per `SECURITY.md`.
3. **Triage and severity assessment.** A maintainer reproduces or confirms
   the issue and assesses impact and severity. There is no published CVSS
   scoring policy at this time — severity is assessed by maintainer judgment,
   discussed among the Maintainer Committee for anything non-trivial.
4. **Fix development.** Development happens privately (e.g., a GitHub
   private security advisory fork/branch, or an unlisted branch) so the fix
   does not disclose the vulnerability before it ships.
5. **Coordinated disclosure.** The maintainers coordinate a disclosure
   timeline with the reporter, aiming to give affected operators time to
   update before public details are available. `SECURITY.md` asks reporters
   for "a reasonable opportunity to remediate before any public disclosure."
6. **Advisory publication.** A GitHub Security Advisory is published for the
   repository once a fix is available, per GitHub's standard advisory flow.
7. **Credit.** With the reporter's permission, they are credited in the
   published advisory, per `SECURITY.md`.

### The 60-day commitment

The project commits to fixing publicly-known vulnerabilities within **60
days of disclosure**. This commitment was made as part of Hive's
[OpenSSF Best Practices badge](https://www.bestpractices.dev/projects/14261)
application (`vulnerabilities_fixed_60_days` criterion) and is backed by
`govulncheck` running in CI on every PR and weekly to catch known-vulnerable
dependencies. It applies to publicly-known vulnerabilities generally
(including dependency CVEs surfaced by tooling), not only reports that arrive
through private disclosure.

There is no separately published SLA for time-to-fix on a privately-reported
vulnerability beyond this 60-day commitment and the "reasonable opportunity
to remediate" language in `SECURITY.md` — a maintainer-judgment target, not a
contractual one.

## Membership: how someone joins or leaves

Hive does not maintain a separate process for security-response membership.
Membership follows the same maintainer lifecycle documented in
`GOVERNANCE-HIVE.md` and enforced through `OWNERS`:

- **Joining:** nomination by an existing maintainer, lazy consensus of the
  Maintainer Committee, and a pull request adding the person to both
  `OWNERS` and the upstream `GOVERNANCE-HIVE.md` maintainer table. The
  general path onto the committee runs through the
  [Contributor Ladder](https://github.com/kubestellar/community/blob/main/CONTRIBUTOR_LADDER.md)
  (Contributor → Organization Member → Reviewer → Maintainer), reviewed
  monthly per `GOVERNANCE-HIVE.md`.
- **Leaving / removal:** a maintainer may be removed by a 2/3 majority vote
  of the other Maintainer Committee members, or a majority vote of the
  KubeStellar Steering Committee, per `GOVERNANCE-HIVE.md`. The same PR
  mechanism (against `OWNERS` and the upstream table) records the change.

`OWNERS` and `GOVERNANCE-HIVE.md` must agree; a change to one without the
matching change to the other is a governance-drift bug, not a housekeeping
detail (see the header comment in `OWNERS`).

**A non-maintainer security responder is possible but not in use today.** The
Maintainer Committee could decide to add someone as a security responder
specifically — without full maintainer/approver status — if a future need
arose (for example, dedicated security expertise that doesn't map to general
code review). No such role exists at the time of writing, and this document
does not create one; it only notes that the committee retains the option and
would document any such addition here and in `OWNERS` if it happened.

## Diversity, answered honestly

The current three security responders are affiliated with three different
organizations (IBM, Universal Blue, and one who is independent/unaffiliated),
per the roster in `OWNERS`. This is a **property of who happens to hold
maintainer status today, not a policy target the project has set.** Hive has
no formal diversity requirement, quota, or rotation rule governing security
response membership — nomination and advancement follow the Contributor
Ladder on merit and sustained participation, described above, with no
affiliation criterion attached. If the roster's composition changes as
maintainers join or leave, that would not by itself violate any stated
policy, because no such policy exists. This section is here so a reviewer
does not have to guess whether the current spread is a commitment or an
accident — it is the latter, stated plainly.

## Escalation

- **No response from the maintainers.** If a reporter receives no
  acknowledgement within the 5-business-day target, or gets no further
  update after acknowledgement, the documented path is the same one that
  governs unresolved cross-project or Committee-level matters generally:
  escalation to the **KubeStellar Steering Committee**, which Hive defers to
  "on cross-project matters, CNCF compliance, and trademark usage" per
  `GOVERNANCE.md`, and which also holds a majority-vote removal power over
  unresponsive maintainers per `GOVERNANCE-HIVE.md`. There is no dedicated
  security-specific contact address for the Steering Committee published at
  this time; a reporter should raise this through the KubeStellar project's
  general channels (see the
  [KubeStellar community repository](https://github.com/kubestellar/community)).
- **Disagreement over a severity call.** The same escalation applies:
  raise it with the Maintainer Committee first; if unresolved, it follows
  the same path to the KubeStellar Steering Committee as any Committee
  decision the reporter disputes. There is no independent security arbiter
  role today — this document does not invent one.
- **Code of Conduct concerns arising from a report interaction**
  (harassment, bad-faith handling, etc.) go through the existing
  [Code of Conduct](../../CODE_OF_CONDUCT.md) reporting path: the KubeStellar
  Code of Conduct Committee at
  `kubestellar-dev-private@googlegroups.com`, or the CNCF Code of Conduct
  Committee at `conduct@cncf.io` for project-agnostic or multi-project
  incidents. This is a conduct escalation path, not a substitute for
  technical security escalation above.
- **CNCF-level escalation.** Hive is a KubeStellar subproject under CNCF; a
  reporter with concerns that cannot be resolved through the paths above can
  reach the CNCF directly through its published channels (e.g.
  `conduct@cncf.io` for conduct-related matters, or the CNCF TAG-Security
  contacts for security-process concerns). This document does not assert a
  more specific CNCF security escalation contact than what CNCF itself
  publishes, since none specific to Hive exists today.

## Known limits

Stated plainly, not implied away:

- **Three part-time maintainers**, all also carrying general maintenance,
  review, and (for two of the three) other-employer responsibilities.
  Security response competes with that workload; there is no dedicated
  security role.
- **No on-call rotation.** Response depends on one of the three maintainers
  seeing and picking up a report; there is no paging, shift schedule, or
  guaranteed after-hours coverage.
- **No third-party security audit or penetration test** has been performed
  on this project to date (see
  [security-self-assessment.md](security-self-assessment.md), "Security
  issue resolution").
- **No published CVSS scoring policy** and no published time-to-fix SLA
  beyond the 60-day commitment above — severity and fix timelines are
  maintainer judgment calls, discussed among the Committee rather than
  computed against a published rubric.
- **No formal, published incident-response runbook specific to a security
  incident** exists in this repository, as distinct from the operational
  [Hub disaster recovery](https://github.com/hivecommons/hive/blob/v4/docs/HUB_DISASTER_RECOVERY.md)
  runbook, which covers backup/restore and fleet recovery but not
  security-incident roles, communication SLAs, or post-incident review
  specifically.

A three-maintainer, cross-organization Committee with a documented process is
a real improvement over a single-maintainer bus factor, and this document
updates the institutional-risk picture in
[security-self-assessment.md](security-self-assessment.md) accordingly. It is
not, however, equivalent to a dedicated security team with guaranteed
coverage, and this page does not claim otherwise.

## Related documents

- [`SECURITY.md`](../../SECURITY.md) — how and where to report a
  vulnerability, and what a reporter should expect.
- [`GOVERNANCE.md`](../../GOVERNANCE.md) and the upstream
  [GOVERNANCE-HIVE.md](https://github.com/kubestellar/kubestellar/blob/main/GOVERNANCE-HIVE.md)
  — the Maintainer Committee, its duties, and the maintainer lifecycle.
- [`OWNERS`](../../OWNERS) — the authoritative maintainer roster.
- [security-self-assessment.md](security-self-assessment.md) — the CNCF
  TAG-Security self-assessment, including the prior single-maintainer
  response-capacity risk this document updates.
- [security-threat-model.md](security-threat-model.md) and
  [security-model.md](security-model.md) — technical threat model and
  enforcement layers (not who responds to a report, but what is being
  protected).
