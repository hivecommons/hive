# Security response process

This page says who handles a security bug report and what happens next. To
send a report, see [SECURITY.md](../../SECURITY.md).

## Who responds

The Hive Maintainer Committee is the security response team. There is no
separate security team. The committee is listed in
[`OWNERS`](../../OWNERS) and in the organization
[MAINTAINERS.md](https://github.com/hivecommons/.github/blob/main/MAINTAINERS.md):

| Name | GitHub | Affiliation |
|---|---|---|
| Andy Anderson | [@clubanderson](https://github.com/clubanderson) | IBM |
| James Reilly | [@hanthor](https://github.com/hanthor) | Universal Blue |
| Doug Baggett | [@Danathar](https://github.com/Danathar) | independent |
| Nic Jackson | [@nicholasjackson](https://github.com/nicholasjackson) | IBM |
| Kelly Abuelsaad | [@kellyaa](https://github.com/kellyaa) | IBM |

The Chief Maintainer (now @clubanderson) leads security response. They are
the default contact for the CNCF. Any maintainer can take a report. The
first one to see the GitHub Security Advisory alert owns it. They bring in
the others to agree on how bad it is and when to fix it. There is no on-call
rota.

## How a report is handled

1. **Received** through GitHub private vulnerability reporting. If that is
   down, contact a maintainer directly.
2. **Acknowledged** within 5 business days.
3. **Triaged.** A maintainer reproduces the bug and judges how bad it is.
   There is no formal scoring policy. The committee discusses anything that
   is not trivial.
4. **Fixed in private**, on a private advisory fork or an unlisted branch.
5. **Disclosed with the reporter.** Operators get time to update before
   details go public.
6. **Published** as a GitHub Security Advisory when the fix is out.
7. **Credited** to the reporter in the advisory, if they agree.

Hive fixes public security bugs within 60 days of disclosure. This includes
bugs in dependencies (code Hive imports) found by `govulncheck`. That tool
runs on every pull request and once a week. This is the
`vulnerabilities_fixed_60_days` rule of the
[OpenSSF Best Practices badge](https://www.bestpractices.dev/projects/14261).
There is no published time-to-fix target for private reports.

## Joining and leaving

Security responders are maintainers. Membership follows the
[Hive Commons contributor ladder](https://github.com/hivecommons/.github/blob/main/MAINTAINERS.md).
An existing maintainer nominates a new one. The committee has seven days to
object. If nobody objects, the person is in. If anyone objects, it goes to a
simple-majority vote. One pull request updates `OWNERS`, the organization
maintainer table and the [GOVERNANCE.md](../../GOVERNANCE.md) table together.
Maintainers who step down become emeritus (retired, with no access).

## Escalation

* **No response.** If nobody replies within 5 business days, mention a
  maintainer from the table above in the advisory. Or ask on the
  [hivecommons-dev Google Group](https://hivecommons.dev/join) or
  [Discord](https://hivecommons.dev/discord). Do not share bug details there.
* **Disagreement on how bad it is.** Raise it with the committee. If that
  fails, the Hive Commons maintainer committee decides, as set out in
  [GOVERNANCE.md](../../GOVERNANCE.md).
* **Conduct concerns** from a report go to `conduct@hivecommons.dev` or
  `conduct@cncf.io` under the [Code of Conduct](../../CODE_OF_CONDUCT.md).

## Known limits

* Five part-time maintainers with other jobs. No dedicated security role
  and no on-call rota.
* No outside security audit or penetration test (paid attack test) yet.
* No formal scoring policy. No time-to-fix target beyond the 60-day rule.
* No security-incident runbook (step-by-step guide). The
  [hub disaster recovery](../../docs/HUB_DISASTER_RECOVERY.md) runbook covers
  backup, restore and fleet recovery only.

## Related documents

* [SECURITY.md](../../SECURITY.md): how to report.
* [GOVERNANCE.md](../../GOVERNANCE.md) and the organization
  [MAINTAINERS.md](https://github.com/hivecommons/.github/blob/main/MAINTAINERS.md):
  the committee and the maintainer lifecycle.
* [security-threat-model.md](security-threat-model.md) and
  [security-model.md](security-model.md): what is protected and how.
