# Compliance controls

> **Hive is not certified for SOC 2, FedRAMP, ISO 27001 or any other
> framework, and makes no claim to be.** This page and the Compliance surface
> it describes help *your* control owners configure, verify and evidence
> *your* controls. Whether a Hive setting satisfies a control is decided by
> your organisation's policy and your auditor, not by Hive. This is the same
> position as the [general technical review](general-technical-review.md) and
> the [outreach policy](../../docs/outreach-antispam.md).

Hive's controls that matter to an auditor already exist but are spread
across many settings: owner/merger roles, the audit log, human-merge paths,
review gates, ACMM levels, the sentinel, sandbox confinement and credential
injection. The compliance registry (`src/pkg/compliance`, tracked by the
epic [#11077](https://github.com/hivecommons/hive/issues/11077)) presents
them as *controls*: it says which framework requirement each setting is
evidence for, and whether the hive is currently in the posture a framework
profile recommends.

## What this is, and what it is not

It **is**:

- A **mapping** from framework controls (for example SOC 2 CC8.1) to the
  Hive settings that implement them, shipped as data in
  `src/pkg/compliance/profiles/*.yaml`.
- A **read-only evaluation** of the live config against each mapping's
  recommended value: `meets`, `deviates`, `off`, or `not_covered`.
- A list of controls Hive **cannot** evidence, marked `not_covered` with the
  reason, so nobody mistakes silence for coverage.

It is **not**:

- A certification, attestation or audit opinion.
- A second source of truth. Every mapping points at a setting that lives in
  its own section (`review:`, `auto_merge:`, `sentinel:`, …). Changing a
  posture means changing that setting. The registry never writes config.
- A replacement for GitHub branch protection, your identity provider, or
  your organisation's incident-response, vendor-management and HR controls.

## Turning it on

```yaml
compliance:
  frameworks: [soc2-type2]     # profile ids; default none
  posture_checks:
    interval: 1h               # 5m..168h; unset = 1h
```

Validation rejects unknown, malformed or duplicate framework ids and an
interval outside 5 minutes–7 days, so a typo cannot leave you believing a
profile is being evaluated when it is not. Shipped profile ids:
`soc2-type2`. FedRAMP Moderate and ISO 27001 Annex A profiles are follow-ups
under [#11077](https://github.com/hivecommons/hive/issues/11077).

## Reading the status

`GET /api/compliance/status` (merger or owner; see the
[API reference](api-reference.md)) returns one entry per control of every
selected framework:

| Status | Meaning |
|---|---|
| `meets` | Every mapped setting matches the profile's recommendation. |
| `deviates` | At least one mapped setting is on but differs from the recommendation, or the settings disagree. |
| `off` | Every mapped setting is switched off or unset. |
| `not_covered` | The profile marks this control as outside what any Hive setting can evidence; the `rationale` says why. |

Each covered control lists its settings with `setting_path`, `current`,
`recommended`, `evaluator` and a per-setting status. A `builtin: true`
setting is behaviour fixed in code today (for example audit retention); it
is reported so the gap is visible, and becomes an operator setting under the
epic. The response always carries the non-certification `disclaimer`, a
`summary` of counts, and the shipped profiles under `available`.

Setting paths are YAML key paths of `hive.yaml` (a test walks the config
structs to prove each one exists), plus two documented namespaces:

- `env.<NAME>` — a process environment variable, for example
  `env.HIVE_PROXY_INJECT_GH_AUTH` (proxy-side GitHub credential injection;
  see the [security model](security-model.md)).
- `audit.retention_days` — currently the audit log's fixed 90-day
  maximum age (see [audit log](audit-log.md)); a configurable floor is
  planned.

## The Settings → Compliance tab

The tab itself ships in [#11080](https://github.com/hivecommons/hive/issues/11080);
until then the status API above is the interface. As designed, it has four
panels:

1. **Framework profile** — select one or more frameworks (SOC 2 Type II
   today; FedRAMP Moderate and ISO 27001 later; or none). The selection is
   `compliance.frameworks`.
2. **Controls** — grouped by domain (Access control, Segregation of duties,
   Change management, Logging & monitoring, Vulnerability management,
   Incident response, Risk management, Data handling). Each row shows the
   control id, the Hive setting(s) that implement it, current and
   recommended value, status, and a link to the section that owns the
   setting. Owner-only writes go through that section's existing endpoint.
3. **Posture checks** — continuous checks the hive runs against itself, with
   pass/fail history (see the catalogue below).
4. **Evidence & attestations** — exports and owner attestations (see below).

## SOC 2 Type II mapping

Control text in the profile is a short paraphrase for orientation, not the
AICPA's wording; the authoritative criteria are the AICPA 2017 Trust
Services Criteria. The table below is generated from
`src/pkg/compliance/profiles/soc2-type2.yaml` — do not edit it by hand.
Regenerate with:

```sh
python3 src/scripts/render-compliance-tables.py          # rewrite in place
python3 src/scripts/render-compliance-tables.py --check  # verify only
```

A Go test in `pkg/compliance` fails CI when the committed table and the
profile disagree.

<!-- BEGIN GENERATED: compliance-profile soc2-type2 -->
| Control | Domain | Hive setting | Recommended | Evaluator | Notes |
|---|---|---|---|---|---|
| CC6.1 — Logical access security over protected information assets | Access control | `dashboard.authorized_users` | `2` | `max_owners` |  |
| CC6.1 — Logical access security over protected information assets | Access control | `env.HIVE_PROXY_INJECT_GH_AUTH` | `true` | `equals` |  |
| CC6.2 — Registration and authorization of users before access is granted | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` | On a hub-proxied hive the hub's Manage Access screen is the registration point and delivers this list; on a standalone spoke the list itself is the gate. Either way an empty list means no named-user allowlist exists. |
| CC6.3 — Role-based access, least privilege and segregation of duties | Segregation of duties | `dashboard.authorized_users` | `1` | `min_mergers` |  |
| CC6.3 — Role-based access, least privilege and segregation of duties | Segregation of duties | `auto_merge.trusted_authors.enabled` | `false` | `equals` |  |
| CC6.3 — Role-based access, least privilege and segregation of duties | Segregation of duties | `auto_merge.trusted_authors.require_github_permission` | `true` | `equals` |  |
| CC6.4 — Physical access to facilities and protected information assets | Access control | _not covered by Hive_ | — | — | Hive is software running on infrastructure the operator chooses. Physical security belongs to the operator's hosting provider and facilities controls; no Hive setting can evidence it. |
| CC6.6 — Protection against threats from outside system boundaries | Access control | `agent_sandbox.enabled` | `true` | `equals` |  |
| CC6.8 — Prevention and detection of unauthorized or malicious software | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| CC6.8 — Prevention and detection of unauthorized or malicious software | Vulnerability management | `sentinel.disabled_behaviors` | `none` | `empty` |  |
| CC7.1 — Detection of configuration changes that introduce vulnerabilities | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| CC7.1 — Detection of configuration changes that introduce vulnerabilities | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| CC7.2 — Monitoring of system components for anomalies | Logging & monitoring | `audit.retention_days` | `365` | `at_least` | Audit retention is currently fixed in code (90 days, size-rotated); the configurable audit.retention_days floor is tracked by the Compliance epic (hivecommons/hive#11077), so this control reports a deviation until it lands. |
| CC7.2 — Monitoring of system components for anomalies | Logging & monitoring | `review.post_comments` | `true` | `equals` |  |
| CC7.3 — Evaluation of security events | Incident response | `escalation.disabled` | `false` | `equals` |  |
| CC7.3 — Evaluation of security events | Incident response | `sentinel.enabled` | `true` | `equals` |  |
| CC7.4 — Incident response | Incident response | _not covered by Hive_ | — | — | The incident-response programme is the operator's. Hive supplies tools such a programme can use (pause, hold labels, sentinel alerts, the audit log) but no setting can evidence that a programme exists or was followed. |
| CC7.5 — Recovery from identified security incidents | Incident response | _not covered by Hive_ | — | — | Backup, restore and disaster recovery are run by the operator (see the hub disaster-recovery guide); Hive has no setting that evidences a tested recovery. |
| CC8.1 — Authorization, testing and approval of changes | Change management | `review.require_approval` | `true` | `equals` | Whether an automated review satisfies "approved" is a decision for the organisation's own change-management policy, not for Hive. These recommendations describe the conservative posture in which every merge has an independent approval, CI evidence and a human on sensitive paths. |
| CC8.1 — Authorization, testing and approval of changes | Change management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| CC8.1 — Authorization, testing and approval of changes | Change management | `auto_merge.human_merge_paths` | `non-empty` | `non_empty` |  |
| CC8.1 — Authorization, testing and approval of changes | Change management | `auto_merge.self_authored` | `false` | `equals` |  |
| CC8.1 — Authorization, testing and approval of changes | Change management | `acmm_level` | `5` | `at_most` |  |
| CC9.1 — Mitigation of risks from potential business disruptions | Risk management | `tool_approval.enabled` | `true` | `equals` |  |
| CC9.1 — Mitigation of risks from potential business disruptions | Risk management | `acmm_level` | `5` | `at_most` |  |
| CC9.2 — Vendor and business partner risk | Risk management | _not covered by Hive_ | — | — | Model providers, GitHub and the hosting platform are the operator's vendors. Assessing them is the operator's vendor-management process; Hive can only list which backends and gateways are configured. |
<!-- END GENERATED: compliance-profile soc2-type2 -->

A step-by-step SOC 2 operator guide, written for auditors and control
owners, is being added as `src/docs/soc2-control-mapping.md` in
[#11063](https://github.com/hivecommons/hive/issues/11063); this page is the
reference for what the registry evaluates.

## Posture checks catalogue

Posture checks are continuous checks over what the hive actually *did*, not
just how it is configured. The runner and its history store land in
[#11079](https://github.com/hivecommons/hive/issues/11079); its cadence is
`compliance.posture_checks.interval`. The designed catalogue:

| Check | Passes when | Evidence source |
|---|---|---|
| Non-author review | Every merge in the window had a review from someone other than the author | Audit log merge entries, review evidence bundles ([#11058](https://github.com/hivecommons/hive/issues/11058)) |
| Owner does not self-merge | No owner also authored a PR that auto-merged | Audit log merge entries joined with PR authors |
| Audit retention | Audit retention is at least the profile's floor (365 days for SOC 2) | Audit log configuration |
| Agent confinement | Every agent backend runs confined (sandbox on, T1/T2 tier) | `agent_sandbox`, agent backend config |
| Sentinel enabled | The sentinel sweep is on with no behaviours disabled | `sentinel` config, sentinel audit entries |
| No credentials in config | No literal tokens or keys in `hive.yaml` or overlays; secrets come from env or mounted Secrets | Config scan |
| Human-merge paths honoured | No PR touching `auto_merge.human_merge_paths` was merged by automation | Audit log merge entries |

## Evidence exports and attestations

Exports and attestations land in
[#11081](https://github.com/hivecommons/hive/issues/11081). As designed:

- **Control-mapping report** — the evaluated status above, as Markdown, JSON
  or PDF, stamped with the hive id, the generation time and the profile
  version.
- **Posture history** — check results for a date range.
- **Audit log slice** — the [audit log](audit-log.md) entries for a date
  range. Note that rotation is size-triggered, so a report must state the
  window it actually covered.
- **Review evidence bundles** — the per-PR bundles from #11058, in bulk.
- **Config snapshot** — the effective config with secrets redacted, plus its
  hash, so an auditor can tie a report to an exact configuration.

**Attestation workflow.** An owner reviews the Controls panel for a
framework and records "reviewed on <date> by <login>". The attestation is
written to the audit log like every other owner action, so it is part of the
same record the exports draw from. An attestation records that
a person looked; it is not a pass/fail verdict and does not change any
status.

## Your organisation's policy must authorise automated review

Hive can show that every merge had an independent review, that CI was green,
and that sensitive paths needed a person. It cannot decide whether an
**automated** reviewer counts as the approval your change-management policy
requires. That is your organisation's decision, written into its own policy
and accepted by its auditor.

- If your policy requires a human approval on every change, configure for
  it: `review.require_approval: true`, `auto_merge.self_authored: false`,
  `auto_merge.trusted_authors.enabled: false`, an ACMM level of 5 or lower
  (L5 is holdgated: agents open PRs, people merge), and human-merge paths for
  anything sensitive. The SOC 2 profile's recommendations describe this
  conservative posture.
- If your policy explicitly authorises automated review for some classes of
  change, record that authorisation in the policy itself, keep the review
  evidence (comments on the PR via `review.post_comments: true`, the audit
  log, #11058 bundles), and expect the Controls panel to show `deviates` for
  the settings you deliberately relaxed. A deviation you have authorised is
  still a deviation from the profile; the profile does not know your policy.

## Gaps Hive does not cover

Some requirements cannot be met by any Hive setting, and the profiles say so
rather than implying coverage:

- **Personnel eligibility (geography and citizenship).** Some frameworks and
  contracts (for example some US public-sector or export-controlled work)
  restrict
  who may access systems or data by location or citizenship. Hive cannot
  verify where an operator, contributor or model provider is, or who they
  are. Restrict access at your identity provider and choose model backends
  and hosting regions that meet the requirement.
- **Legal responsibility for agent actions.** An agent is not a person and
  cannot be accountable. Every change an agent makes is the responsibility of
  the people and organisation operating the hive. Name accountable owners
  (`dashboard.authorized_users`) and keep a human on the merge decision
  wherever your policy requires accountability.
- **Physical, HR, vendor, incident-response and recovery controls.** These
  belong to your hosting provider and your organisation's programmes. The
  SOC 2 profile marks CC6.4, CC7.4, CC7.5 and CC9.2 `not_covered` for this
  reason.

## Related

- [Security model](security-model.md) — the layered controls these mappings
  point at.
- [Audit log](audit-log.md) — the record exports and attestations draw from.
- [General technical review](general-technical-review.md) — the project's
  statement that it holds no certification.
- [ACMM policy matrix](acmm-policy-matrix.md) — what each autonomy level
  allows.
