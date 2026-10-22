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

## Review status and how to read control IDs

The shipped profiles were checked against the published framework
numbering and titles in [#13578](https://github.com/hivecommons/hive/issues/13578):
SOC 2 uses the 2017 Trust Services Criteria (CC6.1–CC9.2), FedRAMP uses
NIST SP 800-53 Rev 5 identifiers, and ISO 27001 uses the **2022** Annex A
numbering (A.5.x–A.8.x, not the 2013 A.9–A.18 numbers). Control text on this
page is a paraphrase for orientation, never the framework's own wording; read
the cited publication for the authoritative requirement.

- Each profile is a **subset** of its framework, not a full checklist. A
  control missing from a profile is not "passed": it is simply not mapped.
  Passing every row here says nothing about the controls that are absent.
- `ELIG-1` is a Hive-specific row, not a NIST control identifier. It marks
  the US-persons/location eligibility conditions that FedRAMP assessors ask
  about under AC-2 and AC-3.
- The mapping of a setting to a control is a plausibility judgement by the
  Hive maintainers. It has not been reviewed by a person with audit
  experience; have your own control owners and auditor confirm it before
  relying on it.
- Hive is not certified or assessed under any of these frameworks. Exports are
  configuration evidence for your own control owners, not an audit opinion.

## Turning it on

```yaml
compliance:
  frameworks: [soc2-type2]     # profile ids; default none
  posture_checks:
    interval: 1h               # 5m..168h; unset = 1h
    window_days: 30            # look-back of the merged-PR checks; 1..90, unset = 30
    history_days: 365          # posture-history retention; 30..1095, unset = 365
```

Validation rejects unknown, malformed or duplicate framework ids and an
interval outside 5 minutes–7 days (or a window / history retention outside
the ranges above), so a typo cannot leave you believing a
profile is being evaluated when it is not. Shipped profile ids:
`soc2-type2`, `fedramp-moderate` and `iso27001-annex-a`.

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

The framework and controls panels ship in
[#11080](https://github.com/hivecommons/hive/issues/11080); posture history,
evidence exports and attestations ship in
[#11081](https://github.com/hivecommons/hive/issues/11081) — see
[Using the Compliance tab](#using-the-compliance-tab). The tab has four
panels:

1. **Framework profile** — select one or more frameworks (SOC 2 Type II,
   FedRAMP Moderate, ISO 27001 Annex A; or none). The selection is
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
| CC1.1 — Commitment to integrity and ethical values | Risk management | _not covered by Hive_ | — | — | Integrity standards, conduct reviews and remediation are organisational governance and personnel processes; no Hive setting can evidence them. |
| CC1.2 — Independent oversight of internal control | Risk management | _not covered by Hive_ | — | — | Board independence, expertise and oversight are organisational governance responsibilities outside the system Hive can observe. |
| CC1.3 — Structures, reporting lines, authorities and responsibilities | Segregation of duties | _not covered by Hive_ | — | — | Hive roles cover access to Hive, not the entity's governance structures, reporting lines or assignment of control responsibilities. |
| CC1.4 — Commitment to attract, develop and retain competent people | Risk management | _not covered by Hive_ | — | — | Recruitment, training, competency assessment and succession planning are organisational personnel processes outside Hive. |
| CC1.5 — Accountability for internal control responsibilities | Risk management | _not covered by Hive_ | — | — | Performance measures, incentives, discipline and accountability are organisational processes; Hive cannot evidence that they operate. |
| CC2.1 — Relevant, quality information supports internal control | Data handling | _not covered by Hive_ | — | — | Hive produces operational records, but cannot evidence the entity's information requirements, asset inventory, classification or data flows. |
| CC2.2 — Internal communication of control information | Risk management | _not covered by Hive_ | — | — | Internal communications, awareness training and confidential reporting channels are operated by the organisation, not by Hive settings. |
| CC2.3 — Communication with external parties | Risk management | _not covered by Hive_ | — | — | Communications with customers, regulators, auditors and other external parties are organisational processes that Hive cannot evidence. |
| CC3.1 — Clear objectives support risk identification and assessment | Risk management | _not covered by Hive_ | — | — | Defining business, reporting, compliance and system objectives and their risk tolerances is the operator's risk-management process. |
| CC3.2 — Identification and analysis of risks | Risk management | _not covered by Hive_ | — | — | Hive can expose technical findings but cannot evidence an entity-wide risk assessment, risk significance decisions or accepted residual risk. |
| CC3.3 — Consideration of fraud risk | Risk management | _not covered by Hive_ | — | — | Fraud-risk assessment, including incentives, pressures and rationalisations, is an organisational process outside Hive. |
| CC3.4 — Identification and assessment of significant change | Risk management | _not covered by Hive_ | — | — | Hive reviews repository changes but cannot evidence the entity's broader assessment of regulatory, business, leadership, vendor and technology change. |
| CC4.1 — Ongoing and separate evaluations of internal control | Logging & monitoring | `sentinel.enabled` | `true` | `equals` | Sentinel supplies ongoing security evaluation and required independent review supplies a separate evaluation of proposed repository changes. These are evidence inputs, not an entity-wide internal-audit programme. |
| CC4.1 — Ongoing and separate evaluations of internal control | Logging & monitoring | `review.require_approval` | `true` | `equals` |  |
| CC4.2 — Evaluation and communication of control deficiencies | Logging & monitoring | _not covered by Hive_ | — | — | Hive can report findings, but cannot evidence management's evaluation, communication and tracking of control deficiencies to remediation. |
| CC5.1 — Control activities mitigate risks | Risk management | _not covered by Hive_ | — | — | Selecting entity-wide controls from assessed risks and deciding acceptable residual risk are organisational responsibilities outside Hive. |
| CC5.2 — General control activities over technology | Risk management | _not covered by Hive_ | — | — | Hive exposes controls over its own workflows but cannot evidence the entity's complete technology infrastructure, security-management and acquisition controls. |
| CC5.3 — Deployment of controls through policies and procedures | Risk management | _not covered by Hive_ | — | — | Establishing, assigning, performing and periodically reassessing organisation-wide policies and procedures is outside Hive's evidence. |
| CC6.1 — Logical access security over protected information assets | Access control | `dashboard.authorized_users` | `2` | `max_owners` |  |
| CC6.1 — Logical access security over protected information assets | Access control | `env.HIVE_PROXY_INJECT_GH_AUTH` | `true` | `equals` |  |
| CC6.2 — Registration and authorization of users before access is granted | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` | On a hub-proxied hive the hub's Manage Access screen is the registration point and delivers this list; on a standalone spoke the list itself is the gate. Either way an empty list means no named-user allowlist exists. |
| CC6.3 — Role-based access, least privilege and segregation of duties | Segregation of duties | `dashboard.authorized_users` | `1` | `min_mergers` |  |
| CC6.3 — Role-based access, least privilege and segregation of duties | Segregation of duties | `auto_merge.trusted_authors.enabled` | `false` | `equals` |  |
| CC6.3 — Role-based access, least privilege and segregation of duties | Segregation of duties | `auto_merge.trusted_authors.require_github_permission` | `true` | `equals` |  |
| CC6.4 — Physical access to facilities and protected information assets | Access control | _not covered by Hive_ | — | — | Hive is software running on infrastructure the operator chooses. Physical security belongs to the operator's hosting provider and facilities controls; no Hive setting can evidence it. |
| CC6.5 — Secure disposal of protected data and software | Data handling | _not covered by Hive_ | — | — | Media sanitisation and disposal are performed by the operator and its infrastructure providers; Hive cannot observe or evidence them. |
| CC6.6 — Protection against threats from outside system boundaries | Access control | `agent_sandbox.enabled` | `true` | `equals` |  |
| CC6.7 — Restricted and protected transmission and removal of information | Data handling | `env.HIVE_PROXY_INJECT_GH_AUTH` | `true` | `equals` | Proxy credential injection keeps the GitHub credential out of agent processes, while the sandbox restricts the processes that may receive it. Operators remain responsible for transport and endpoint controls outside Hive. |
| CC6.7 — Restricted and protected transmission and removal of information | Data handling | `agent_sandbox.enabled` | `true` | `equals` |  |
| CC6.8 — Prevention and detection of unauthorized or malicious software | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| CC6.8 — Prevention and detection of unauthorized or malicious software | Vulnerability management | `sentinel.disabled_behaviors` | `none` | `empty` |  |
| CC7.1 — Detection of configuration changes that introduce vulnerabilities | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| CC7.1 — Detection of configuration changes that introduce vulnerabilities | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| CC7.2 — Monitoring of system components for anomalies | Logging & monitoring | `audit.retention_days` | `365` | `at_least` | Audit retention is currently fixed in code (90 days, size-rotated); the configurable audit.retention_days floor is tracked by the Compliance epic (hivecommons/hive#11077), so this control reports a deviation until it lands. Delegated relay work is covered by the relay_* rows in the audit log and the relay_audit_coverage posture check. |
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
| A1.1 — Capacity monitoring and management | Logging & monitoring | _not covered by Hive_ | — | — | Infrastructure capacity measurement, forecasting and scaling belong to the operator's hosting and service-management controls. |
| A1.2 — Environmental protection, backup and recovery infrastructure | Risk management | `governor.backup.key_file` | `non-empty` | `non_empty` | A resolvable backup encryption key enables Hive's fail-closed encrypted backup path. The value is the effective key source the backup path uses (governor key_file, the default Secret or PVC mount, or the HIVE_BACKUP_KEY env fallback), not the raw key_file setting. It does not evidence backup scheduling, off-site storage, environmental protection or successful restoration; operators must supply that evidence separately. |
| A1.3 — Testing of recovery procedures | Risk management | _not covered by Hive_ | — | — | Hive provides backup and restore tooling but cannot evidence that the operator maintains or tests a business-continuity and recovery plan. |
| C1.1 — Identification and retention of confidential information | Data handling | _not covered by Hive_ | — | — | Information classification, confidentiality commitments and retention schedules are defined and operated by the organisation, not Hive. |
| C1.2 — Disposal of confidential information | Data handling | _not covered by Hive_ | — | — | Hive cannot determine which operator data is confidential or evidence its secure destruction across repositories, hosts and service providers. |
| PI1.1 — Quality information about processing objectives | Data handling | _not covered by Hive_ | — | — | Defining service-processing requirements, specifications and data semantics is the operator's product and service-management process. |
| PI1.2 — Complete and accurate system inputs | Data handling | _not covered by Hive_ | — | — | Hive cannot evidence completeness and accuracy controls for every input to the operator's in-scope products, services and reports. |
| PI1.3 — Controlled system processing | Data handling | _not covered by Hive_ | — | — | Processing specifications, error correction and complete processing records depend on the operator's in-scope systems, not Hive settings. |
| PI1.4 — Complete, accurate and timely output | Data handling | _not covered by Hive_ | — | — | Hive cannot establish or evaluate the output specifications, intended recipients and delivery controls of the operator's services. |
| PI1.5 — Complete, accurate and timely storage | Data handling | _not covered by Hive_ | — | — | Storage specifications, archival controls and complete storage-activity records belong to the operator's in-scope systems. |
| P1.1 — Notice of privacy practices | Data handling | _not covered by Hive_ | — | — | The organisation determines whether it is a data controller or processor and owns its privacy notice; Hive cannot evidence that notice. |
| P2.1 — Privacy choices and consent | Data handling | _not covered by Hive_ | — | — | Consent collection, withdrawal and purpose records are features of the operator's products and privacy programme, not Hive. |
| P3.1 — Collection consistent with privacy objectives | Data handling | _not covered by Hive_ | — | — | Hive cannot classify all personal information collected by the operator or determine whether each collection is necessary, fair and lawful. |
| P3.2 — Explicit consent before collection when required | Data handling | _not covered by Hive_ | — | — | Required consent and its timing depend on the operator's products, jurisdictions and privacy programme; Hive does not administer them. |
| P4.1 — Use limited to identified privacy purposes | Data handling | _not covered by Hive_ | — | — | Hive cannot determine the purposes for which the operator processes personal information or evidence purpose limitation. |
| P4.2 — Retention consistent with privacy objectives | Data handling | _not covered by Hive_ | — | — | Personal-information retention schedules and legal exceptions are organisational controls outside Hive. |
| P4.3 — Secure disposal of personal information | Data handling | _not covered by Hive_ | — | — | Hive cannot identify all personal information held by the operator or evidence its deletion across in-scope systems and service providers. |
| P5.1 — Data-subject access to personal information | Access control | _not covered by Hive_ | — | — | Data-subject authentication and access-request fulfilment are operated by the organisation's privacy programme, not Hive. |
| P5.2 — Correction of personal information | Data handling | _not covered by Hive_ | — | — | Hive cannot process data-subject correction requests or propagate corrections through the operator's in-scope systems and third parties. |
| P6.1 — Consented disclosure to third parties | Data handling | _not covered by Hive_ | — | — | The operator controls personal-information disclosures, purposes, recipients and consent; Hive cannot evidence those decisions. |
| P6.2 — Records of authorized disclosures | Logging & monitoring | _not covered by Hive_ | — | — | Hive's audit log covers Hive actions, not every authorized disclosure of personal information by the operator's in-scope systems. |
| P6.3 — Records of unauthorized disclosures | Logging & monitoring | _not covered by Hive_ | — | — | Hive cannot evidence a complete breach and unauthorized-disclosure register for the operator's systems and third parties. |
| P6.4 — Third-party privacy commitments and compliance | Risk management | _not covered by Hive_ | — | — | Contracting with and assessing vendors is the operator's privacy and vendor-management process. |
| P6.5 — Third-party notification of unauthorized disclosures | Incident response | _not covered by Hive_ | — | — | Vendor breach-notification commitments and the response to their reports are organisational and contractual controls outside Hive. |
| P6.6 — Breach notification | Incident response | _not covered by Hive_ | — | — | Determining notification obligations and issuing legally sufficient notices are responsibilities of the operator's privacy programme. |
| P6.7 — Accounting of personal information and disclosures | Data handling | _not covered by Hive_ | — | — | Hive cannot inventory all personal information held about a data subject or all disclosures made by the operator and its third parties. |
| P7.1 — Quality of personal information | Data handling | _not covered by Hive_ | — | — | Data-quality requirements and correction processes belong to the operator's in-scope products and privacy programme. |
| P8.1 — Privacy inquiries, disputes and compliance monitoring | Logging & monitoring | _not covered by Hive_ | — | — | Hive cannot evidence the operator's privacy complaint process, compliance reviews, remediation or disciplinary actions. |
<!-- END GENERATED: compliance-profile soc2-type2 -->

A step-by-step SOC 2 operator guide, written for auditors and control
owners, is [SOC 2 control mapping](soc2-control-mapping.md); it maps auditor
questions to the fields of a PR's [review evidence bundle](review-evidence.md).
This page is the reference for what the registry evaluates.

## FedRAMP Moderate mapping

A **subset** of the NIST SP 800-53 Rev 5 Moderate baseline: the families AC,
AU, CM, IA, RA, SA and SI. Control text is a short paraphrase, not NIST's
wording; the authoritative text is NIST SP 800-53 Rev 5 and the FedRAMP
baseline. Relevant controls Hive cannot evidence (for example AC-17, AU-9,
CM-2, SC-7, SC-8, SC-28, CP-9, SR-*, PE-3 and PS-3) are listed `not_covered`
so silence is not read as coverage, as are external service providers (SA-9)
and location/citizenship eligibility (ELIG-1, a non-NIST identifier). IA-2 is informational:
authentication happens at GitHub or the hub, Hive only holds the allowlist.
Selecting this profile does not make a hive FedRAMP authorized, and Hive is
not a FedRAMP authorization boundary. Generated from
`src/pkg/compliance/profiles/fedramp-moderate.yaml`; do not edit by hand.

<!-- BEGIN GENERATED: compliance-profile fedramp-moderate -->
| Control | Domain | Hive setting | Recommended | Evaluator | Notes |
|---|---|---|---|---|---|
| AC-2 — Account management | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` | The dashboard allowlist is the account roster; periodic review of it is the operator's process. |
| AC-2 — Account management | Access control | `dashboard.authorized_users` | `2` | `max_owners` |  |
| ELIG-1 — Personnel eligibility by location or citizenship (not a NIST control) | Access control | _not covered by Hive_ | — | — | Hive cannot verify where a person is or their citizenship. Enforce eligibility at the identity provider and the hosting region; Hive only records who is on the allowlist. |
| AC-3 — Access enforcement | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` |  |
| AC-3 — Access enforcement | Access control | `auto_merge.trusted_authors.require_github_permission` | `true` | `equals` |  |
| AC-5 — Separation of duties | Segregation of duties | `dashboard.authorized_users` | `1` | `min_mergers` |  |
| AC-5 — Separation of duties | Segregation of duties | `auto_merge.self_authored` | `false` | `equals` |  |
| AC-5 — Separation of duties | Segregation of duties | `review.require_approval` | `true` | `equals` |  |
| AC-6 — Least privilege | Access control | `auto_merge.trusted_authors.enabled` | `false` | `equals` |  |
| AC-6 — Least privilege | Access control | `agent_sandbox.enabled` | `true` | `equals` |  |
| AC-6 — Least privilege | Access control | `env.HIVE_PROXY_INJECT_GH_AUTH` | `true` | `equals` |  |
| AU-2 — Event logging | Logging & monitoring | `review.post_comments` | `true` | `equals` | Delegated relay work is covered by the relay_* rows in the audit log and the relay_audit_coverage posture check. |
| AU-2 — Event logging | Logging & monitoring | `audit.retention_days` | `365` | `at_least` |  |
| AU-3 — Content of audit records | Logging & monitoring | `review.post_comments` | `true` | `equals` | Review verdicts are published as PR comments with the evidence; the audit log record format is fixed in code. |
| AU-6 — Audit record review and reporting | Logging & monitoring | `sentinel.enabled` | `true` | `equals` |  |
| AU-6 — Audit record review and reporting | Logging & monitoring | `escalation.disabled` | `false` | `equals` |  |
| AU-11 — Audit record retention | Logging & monitoring | `audit.retention_days` | `365` | `at_least` | Retention is fixed in code at 90 days today; the configurable floor is tracked by hivecommons/hive#11077, so this reports a deviation until it lands. |
| AU-12 — Audit record generation | Logging & monitoring | `sentinel.enabled` | `true` | `equals` |  |
| AU-12 — Audit record generation | Logging & monitoring | `review.post_comments` | `true` | `equals` |  |
| CM-3 — Configuration change control | Change management | `review.require_approval` | `true` | `equals` | Whether an automated review satisfies approval is the organisation's change-management policy; these values describe the conservative posture. |
| CM-3 — Configuration change control | Change management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| CM-3 — Configuration change control | Change management | `auto_merge.human_merge_paths` | `non-empty` | `non_empty` |  |
| CM-3 — Configuration change control | Change management | `auto_merge.self_authored` | `false` | `equals` |  |
| CM-5 — Access restrictions for change | Change management | `auto_merge.human_merge_paths` | `non-empty` | `non_empty` |  |
| CM-5 — Access restrictions for change | Change management | `auto_merge.trusted_authors.require_github_permission` | `true` | `equals` |  |
| CM-5 — Access restrictions for change | Change management | `dashboard.authorized_users` | `1` | `min_mergers` |  |
| CM-6 — Configuration settings | Change management | `sentinel.enabled` | `true` | `equals` |  |
| CM-6 — Configuration settings | Change management | `sentinel.disabled_behaviors` | `none` | `empty` |  |
| IA-2 — Identification and authentication (organizational users) | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` | Informational. Authentication is performed by GitHub OAuth or the hub; Hive only holds the allowlist of named users, so a non-empty list is the most it can evidence. |
| IA-5 — Authenticator management | Access control | `env.HIVE_PROXY_INJECT_GH_AUTH` | `true` | `equals` | Hive keeps the real GitHub credential out of agent processes; issuing, rotating and revoking credentials belongs to the operator and GitHub. |
| RA-5 — Vulnerability monitoring and scanning | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| RA-5 — Vulnerability monitoring and scanning | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| SA-11 — Developer testing and evaluation | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| SA-11 — Developer testing and evaluation | Vulnerability management | `review.require_approval` | `true` | `equals` |  |
| SA-9 — External system services | Risk management | _not covered by Hive_ | — | — | Model providers, GitHub and the host are the operator's external providers; assessing them is the operator's vendor-management process. |
| SI-4 — System monitoring | Logging & monitoring | `sentinel.enabled` | `true` | `equals` |  |
| SI-4 — System monitoring | Logging & monitoring | `escalation.disabled` | `false` | `equals` |  |
| SI-4 — System monitoring | Logging & monitoring | `tool_approval.enabled` | `true` | `equals` |  |
| SI-7 — Software, firmware and information integrity | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| SI-7 — Software, firmware and information integrity | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| SI-7 — Software, firmware and information integrity | Vulnerability management | `agent_sandbox.enabled` | `true` | `equals` |  |
| PE-3 — Physical access control | Access control | _not covered by Hive_ | — | — | Hive is software on infrastructure the operator chooses; physical security belongs to the hosting provider. |
| PS-3 — Personnel screening | Access control | _not covered by Hive_ | — | — | Hiring and background screening are HR processes; no Hive setting can evidence them. |
| AC-8 — System use notification | Access control | _not covered by Hive_ | — | — | The dashboard does not present a configurable use banner; the notice belongs to the operator's identity provider or reverse proxy. |
| AC-17 — Remote access | Access control | _not covered by Hive_ | — | — | Remote access paths (VPN, bastion, reverse proxy) are operator infrastructure; Hive only exposes its dashboard allowlist. |
| AU-8 — Time stamps | Logging & monitoring | _not covered by Hive_ | — | — | Clock synchronization is a property of the host; no Hive setting evidences it. |
| AU-9 — Protection of audit information | Logging & monitoring | _not covered by Hive_ | — | — | Protection of the audit log files depends on host file permissions and backup controls outside Hive's settings. |
| CM-2 — Baseline configuration | Change management | _not covered by Hive_ | — | — | Baselines of the hosting environment are the operator's; Hive settings are not a full system baseline. |
| CM-7 — Least functionality | Change management | _not covered by Hive_ | — | — | Host and container hardening are outside what Hive settings can evidence. |
| CM-8 — System component inventory | Change management | _not covered by Hive_ | — | — | Hive does not maintain an authoritative inventory of host components. |
| CP-9 — System backup | Risk management | _not covered by Hive_ | — | — | Backup of hive state is an operator process; no Hive setting evidences it. |
| IA-8 — Identification and authentication (non-organizational users) | Access control | _not covered by Hive_ | — | — | Authentication of outside contributors is performed by GitHub and the operator's identity provider, not Hive. |
| IR-4 — Incident handling | Incident response | _not covered by Hive_ | — | — | Incident handling is an organisational process; Hive escalation is a signal, not an incident-response program. |
| IR-6 — Incident reporting | Incident response | _not covered by Hive_ | — | — | Reporting obligations and channels are the operator's. |
| SC-7 — Boundary protection | Access control | _not covered by Hive_ | — | — | Network boundaries are operator infrastructure outside Hive's settings. |
| SC-8 — Transmission confidentiality and integrity | Access control | _not covered by Hive_ | — | — | Transport encryption is terminated by operator infrastructure; Hive settings do not evidence it. |
| SC-28 — Protection of information at rest | Access control | _not covered by Hive_ | — | — | Disk and volume encryption belong to the host and storage provider. |
| SI-2 — Flaw remediation | Vulnerability management | _not covered by Hive_ | — | — | Patching of the host and dependencies is an operator process; Hive settings do not evidence it. |
| SI-3 — Malicious code protection | Vulnerability management | _not covered by Hive_ | — | — | Endpoint and malware protection are host-level controls outside Hive. |
| SR-2 — Supply chain risk management plan | Risk management | _not covered by Hive_ | — | — | Supply chain risk planning is an organisational document; Hive cannot evidence it. |
| SR-3 — Supply chain controls and processes | Risk management | _not covered by Hive_ | — | — | Supplier selection and controls are organisational processes. |
| SR-11 — Component authenticity | Risk management | _not covered by Hive_ | — | — | Hive settings do not verify component provenance; use the operator's artifact signing and provenance checks. |
<!-- END GENERATED: compliance-profile fedramp-moderate -->

## ISO 27001 Annex A mapping

Selected controls of ISO/IEC 27001:2022 Annex A, covering policies and roles
(5), people (6), physical (7) and technological controls (8: access, secure
development, logging, change). Control text is a short paraphrase, not ISO's
wording; the authoritative text is the standard. Policy, supplier, incident
programme, continuity, screening and physical controls are `not_covered`.
Selecting this profile does not make a hive ISO 27001 certified. Generated
from `src/pkg/compliance/profiles/iso27001-annex-a.yaml`; do not edit by hand.

<!-- BEGIN GENERATED: compliance-profile iso27001-annex-a -->
| Control | Domain | Hive setting | Recommended | Evaluator | Notes |
|---|---|---|---|---|---|
| A.5.1 — Policies for information security | Risk management | _not covered by Hive_ | — | — | Policies are organisational documents; Hive cannot evidence that they exist or were approved. |
| A.5.2 — Information security roles and responsibilities | Segregation of duties | `dashboard.authorized_users` | `non-empty` | `non_empty` |  |
| A.5.2 — Information security roles and responsibilities | Segregation of duties | `dashboard.authorized_users` | `1` | `min_mergers` |  |
| A.5.3 — Segregation of duties | Segregation of duties | `auto_merge.self_authored` | `false` | `equals` |  |
| A.5.3 — Segregation of duties | Segregation of duties | `review.require_approval` | `true` | `equals` |  |
| A.5.3 — Segregation of duties | Segregation of duties | `auto_merge.trusted_authors.enabled` | `false` | `equals` |  |
| A.5.15 — Access control | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` |  |
| A.5.15 — Access control | Access control | `dashboard.authorized_users` | `2` | `max_owners` |  |
| A.5.16 — Identity management | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` | Hive holds the allowlist of named users; identity lifecycle is run at the identity provider. |
| A.5.18 — Access rights | Access control | `dashboard.authorized_users` | `non-empty` | `non_empty` |  |
| A.5.18 — Access rights | Access control | `auto_merge.trusted_authors.require_github_permission` | `true` | `equals` |  |
| A.5.19 — Information security in supplier relationships | Risk management | _not covered by Hive_ | — | — | Model providers, GitHub and hosting are the operator's suppliers; supplier assessment is the operator's process. |
| A.5.24 — Incident management planning and preparation | Incident response | _not covered by Hive_ | — | — | The incident process is the operator's. Hive supplies tools it can use (pause, hold labels, sentinel alerts, audit log) but cannot evidence the process. |
| A.5.25 — Assessment and decision on information security events | Incident response | `escalation.disabled` | `false` | `equals` |  |
| A.5.25 — Assessment and decision on information security events | Incident response | `sentinel.enabled` | `true` | `equals` |  |
| A.5.30 — ICT readiness for business continuity | Incident response | _not covered by Hive_ | — | — | Backup, restore and disaster recovery are run by the operator; no Hive setting evidences a tested recovery. |
| A.6.1 — Screening | Access control | _not covered by Hive_ | — | — | Screening and other HR controls are the organisation's; Hive cannot verify who a person is or where they are, including any geographic or citizenship eligibility rule. |
| A.7.2 — Physical entry | Access control | _not covered by Hive_ | — | — | Physical security belongs to the operator's hosting provider and facilities. |
| A.8.2 — Privileged access rights | Access control | `dashboard.authorized_users` | `2` | `max_owners` |  |
| A.8.2 — Privileged access rights | Access control | `auto_merge.trusted_authors.enabled` | `false` | `equals` |  |
| A.8.4 — Access to source code | Access control | `env.HIVE_PROXY_INJECT_GH_AUTH` | `true` | `equals` |  |
| A.8.4 — Access to source code | Access control | `agent_sandbox.enabled` | `true` | `equals` |  |
| A.8.4 — Access to source code | Access control | `auto_merge.human_merge_paths` | `non-empty` | `non_empty` |  |
| A.8.7 — Protection against malware | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| A.8.7 — Protection against malware | Vulnerability management | `sentinel.disabled_behaviors` | `none` | `empty` |  |
| A.8.8 — Management of technical vulnerabilities | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| A.8.8 — Management of technical vulnerabilities | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| A.8.9 — Configuration management | Change management | `sentinel.enabled` | `true` | `equals` |  |
| A.8.9 — Configuration management | Change management | `acmm_level` | `5` | `at_most` |  |
| A.8.15 — Logging | Logging & monitoring | `audit.retention_days` | `365` | `at_least` | Audit retention is fixed at 90 days in code today; the configurable floor is tracked by hivecommons/hive#11077, so this reports a deviation until it lands. Delegated relay work is covered by the relay_* rows in the audit log and the relay_audit_coverage posture check. |
| A.8.15 — Logging | Logging & monitoring | `review.post_comments` | `true` | `equals` |  |
| A.8.16 — Monitoring activities | Logging & monitoring | `sentinel.enabled` | `true` | `equals` |  |
| A.8.16 — Monitoring activities | Logging & monitoring | `escalation.disabled` | `false` | `equals` |  |
| A.8.16 — Monitoring activities | Logging & monitoring | `tool_approval.enabled` | `true` | `equals` |  |
| A.8.25 — Secure development life cycle | Change management | `review.require_approval` | `true` | `equals` |  |
| A.8.25 — Secure development life cycle | Change management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| A.8.28 — Secure coding | Vulnerability management | `review.require_approval` | `true` | `equals` |  |
| A.8.28 — Secure coding | Vulnerability management | `sentinel.enabled` | `true` | `equals` |  |
| A.8.29 — Security testing in development and acceptance | Vulnerability management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| A.8.31 — Separation of development, test and production environments | Change management | `agent_sandbox.enabled` | `true` | `equals` |  |
| A.8.32 — Change management | Change management | `review.require_approval` | `true` | `equals` | Whether an automated review satisfies approval is the organisation's change-management policy; these values describe the conservative posture. |
| A.8.32 — Change management | Change management | `auto_merge.required_checks` | `non-empty` | `non_empty` |  |
| A.8.32 — Change management | Change management | `auto_merge.human_merge_paths` | `non-empty` | `non_empty` |  |
| A.8.32 — Change management | Change management | `auto_merge.self_authored` | `false` | `equals` |  |
| A.5.23 — Information security for use of cloud services | Risk management | _not covered by Hive_ | — | — | Cloud supplier governance is an organisational process. |
| A.5.28 — Collection of evidence | Incident response | _not covered by Hive_ | — | — | Forensic evidence handling is an organisational process. |
| A.8.3 — Information access restriction | Access control | _not covered by Hive_ | — | — | Data-level restriction inside the operator's systems is outside Hive's settings. |
| A.8.5 — Secure authentication | Access control | _not covered by Hive_ | — | — | Authentication is performed by GitHub and the operator's identity provider. |
| A.8.12 — Data leakage prevention | Access control | _not covered by Hive_ | — | — | Hive settings do not evidence data-loss prevention tooling. |
| A.8.13 — Information backup | Risk management | _not covered by Hive_ | — | — | Backup of hive state is an operator process. |
| A.8.20 — Networks security | Access control | _not covered by Hive_ | — | — | Network security is operator infrastructure. |
| A.8.21 — Security of network services | Access control | _not covered by Hive_ | — | — | Network service agreements are the operator's. |
| A.8.22 — Segregation of networks | Access control | _not covered by Hive_ | — | — | Network segregation is operator infrastructure. |
| A.8.23 — Web filtering | Access control | _not covered by Hive_ | — | — | Egress filtering is operator infrastructure. |
| A.8.24 — Use of cryptography | Access control | _not covered by Hive_ | — | — | Cryptographic key and transport management are outside Hive's settings. |
| A.8.26 — Application security requirements | Change management | _not covered by Hive_ | — | — | Requirements definition is an organisational process. |
| A.8.30 — Outsourced development | Change management | _not covered by Hive_ | — | — | Supplier oversight is an organisational process. |
<!-- END GENERATED: compliance-profile iso27001-annex-a -->

## Posture checks catalogue

Posture checks are continuous checks over what the hive actually *did*, not
just how it is configured
([#11079](https://github.com/hivecommons/hive/issues/11079)). While at least
one framework is selected, the dashboard runs every check once a minute after
start and then every `compliance.posture_checks.interval`; with no framework
selected the runner stays idle. Each check records a result with `status`
`pass`, `fail`, `skip` (the evidence is unavailable — for example no GitHub
client or no repos configured — so nothing was asserted) or `error` (the
evidence fetch failed), a short `detail`, the profile `control_ids` it
evidences and `evidence_refs` (PR URLs, config file paths, doc links) an
auditor can follow. Details never echo a secret value.

| Check id | Passes when | Controls (SOC 2) | Evidence source |
|---|---|---|---|
| `non_author_review` | Every PR merged in the last `window_days` had an approving, change-requesting or commenting review from someone other than its author | CC8.1, CC6.3 | GitHub merged-PR search and reviews |
| `owner_not_auto_merge_author` | No dashboard owner (`dashboard.authorized_users` with the owner role) authored a PR that a bot merged in the window | CC6.3 | GitHub merged-PR search (merger is a bot) |
| `audit_retention` | The audit log's retention (`audit.retention_days`, today the built-in 90 days) is at least the strictest floor of the selected profiles (365 days for SOC 2 CC7.2) | CC7.2 | [Audit log](audit-log.md#rotation-and-retention) |
| `agent_confinement` | Every enabled agent runs at tier T2 or better: T1 = credential-free sandbox, T2 = proxy-injected GitHub credential (`HIVE_PROXY_INJECT_GH_AUTH`), T3 = unconfined. The floor becomes `agent_backends.min_confinement_tier` under [#11077](https://github.com/hivecommons/hive/issues/11077) | CC6.6, CC6.1 | Agent and sandbox config, process env |
| `sentinel_enabled` | `sentinel.enabled` is on with no behaviour disabled and the sentinel label exists on every repo | CC6.8, CC7.1 | `sentinel` config, repo labels |
| `no_secrets_in_config` | No secret-looking value in `hive.yaml`, the dashboard overlay, the runtime config or the per-agent overlays (the log scrubber's token patterns, PEM blocks, and literal values under credential-named keys such as `*_token`; `$ENV` references and paths are fine) | CC6.1 | Config file scan (file and line only) |
| `dashboard_auth` | The dashboard is hub-proxied, enforces per-user login (`dashboard.authorized_users`) or requires the shared auth token | CC6.1, CC6.2 | `dashboard` config |
| `relay_audit_coverage` | At least one `relay_*` row was written to the audit log in the last 24h, or no relay connected in that window; fails only when relays connected but no `relay_*` row was written | CC7.2 | [Audit log](audit-log.md) (`/data/audit.jsonl`), hub relay connections |
| `hold_labels_exist` | The hive's hold label and the needs-human label exist on every repo, so a human can stop automation | CC8.1, CC7.3 | Repo labels |

Results are appended to `/data/compliance-posture.jsonl`, so the history
survives restarts and upgrades; runs older than
`compliance.posture_checks.history_days` (and beyond a hard cap of 9000 runs)
are pruned. When a check goes from `pass` to `fail` the hive writes a
`compliance_posture_failed` [audit log](audit-log.md) entry naming the check,
its controls and the detail; manual runs are audited as
`compliance_posture_run`.

- `GET /api/compliance/posture` (merger or owner) returns the catalogue, the
  effective `interval`, `window_days` and `history_days`, and the `latest`
  run. Add `?since=` (an RFC 3339 time, a `YYYY-MM-DD` date or a look-back
  such as `168h`) for every run since then, oldest first (at most 1000, with
  `truncated: true` when more matched).
- `POST /api/compliance/posture/run` (merger or owner) runs one pass now and
  returns it; `409` when no framework is selected or a pass is already
  running.

A passing posture check is evidence that the hive behaved as configured over
the window; it is not an attestation that your organisation meets the
control, and it is not a certification.

## Evidence exports and attestations

Exports and attestations shipped in
[#11081](https://github.com/hivecommons/hive/issues/11081). Every endpoint
below is owner only (verified owner session), because the evidence names
owners, PRs and config locations.

**Exports.** `GET /api/compliance/export?kind=&format=&since=&until=`
returns inline text (`Content-Disposition: inline`, never cached) for the
range `[since, until)` so dashboards can show exports without triggering
browser download permission prompts. Add `download=1` for an explicit
attachment outside the dashboard. `since` takes an RFC 3339 time, a `YYYY-MM-DD` date
or a look-back such as `720h`; `until` takes an RFC 3339 time or a
`YYYY-MM-DD` date meaning the end of that day. Without them the range is
the last 30 days. Every export is written to the
[audit log](audit-log.md) as `compliance_export` with its kind, format and
range.

| `kind` | Formats | Contents |
|---|---|---|
| `controls` | `json` (default), `md` | The control-mapping report: the evaluated status above, stamped with the hive id, generation time and each profile's name and version |
| `posture` | `json` (default), `csv` | Every posture run in the range, plus per-check series in JSON. The CSV has one row per check result |
| `audit` | `json` | The audit log entries in the range, from the current file and rotated backups. Rotation is size-triggered, so `covered_from` reports the oldest entry actually found. If it is later than `since`, the log no longer holds the start of the range |
| `config` | `json` | The effective config with secrets redacted (the same redaction as the config export) and the `sha256` of exactly the `effective` bytes. Use the hash to tie a report to one configuration |
| `attestations` | `json` (default), `csv` | Attestations recorded in the range |
| `bundle` | `json` | All of the above in one file |

JSON exports carry a `meta` object (`kind`, `disclaimer`, `hive_id`,
`generated_at`, `since`, `until`). CSV cells that start with `=`, `+`, `-`
or `@` are prefixed with `'` so a spreadsheet does not run them as formulas.
PDF output is not offered. Bulk review-evidence bundles depend on the
per-PR bundles from #11058 and will join the exports when that lands.

**Posture history.** `GET /api/compliance/posture/history?since=&until=`
returns the posture history for the range as one series per check:
`points` (`at`, `status`), status counts, `last_fail_at` and the latest
result with its detail and evidence refs. It covers at most the 1000 most
recent runs, with `truncated: true` when more matched. The Compliance tab
draws each series as a sparkline.

**Attestation workflow.** An owner reviews the Controls panel for a
framework and records "reviewed <framework> on <date>" with an optional
note. `POST /api/compliance/attestations` takes
`{"framework", "reviewed_on", "note"}`. The framework must be a shipped
profile id. `reviewed_on` must be a `YYYY-MM-DD` date that is not in the
future. The note can be at most 2000 characters. The attesting login comes
from the session, never from the body. Each attestation is written to the
audit log as `compliance_attestation`, so it is part of the same record the
exports draw from. A copy goes to `/data/compliance-attestations.jsonl`,
where it outlives audit-log rotation (the newest 5000 are kept).
`GET /api/compliance/attestations` (`?framework=` filters) lists them newest
first. An attestation records that a person looked. It is not a pass/fail
verdict and does not change any status.

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
- **Organisational, physical and infrastructure controls.** The SOC 2 profile
  lists all 61 Trust Services Criteria. It marks criteria `not_covered` when
  evidence must come from the operator or its providers: governance and HR,
  entity-wide risk and communication processes, physical security, capacity,
  recovery testing, data life-cycle and processing-integrity controls, and the
  privacy programme. The generated table above gives the criterion-specific
  reason instead of allowing an omitted criterion to look covered. FedRAMP
  ELIG-1, SA-9, PE-3 and PS-3 and ISO 27001 A.5.1, A.5.19, A.5.24, A.5.30,
  A.6.1 and A.7.2 are likewise explicit.

## Related

- [Security model](security-model.md) — the layered controls these mappings
  point at.
- [Audit log](audit-log.md) — the record exports and attestations draw from.
- [General technical review](general-technical-review.md) — the project's
  statement that it holds no certification.
- [ACMM policy matrix](acmm-policy-matrix.md) — what each autonomy level
  allows.

## Using the Compliance tab

Open **Settings → Compliance** (merger or owner; read and read-write users
see a notice instead). A banner at the top repeats that Hive is not
certified — the tab maps your configuration to control requirements, nothing
more.

**Framework profile.** One checkbox per shipped profile, with its version,
control count and a one-line description. Checking or unchecking stages
`compliance.frameworks`; the footer **Save & close** writes it through
`PUT /api/config/governor/compliance` (owner only; unknown or duplicate ids
are rejected before anything changes, and the change is audited as
`config_compliance`). Unchecking every box turns evaluation off.

**Controls.** Every control of the saved selection, grouped by domain. Each
row shows the control id and title (hover the **i** for the requirement text
and citation), the implementing setting(s), current → recommended value, and
a `meets` / `deviates` / `off` / `not covered` pill. For each setting:

- a boolean the owning section can write (`review.require_approval`,
  `auto_merge.self_authored`, `auto_merge.trusted_authors.*`,
  `sentinel.enabled`, `agent_sandbox.enabled`, `escalation.disabled`) gets an
  inline toggle;
- any other setting with a Settings home (authorized users, required checks,
  Sentinel behaviours) gets **Open setting**, which switches to the owning
  tab without discarding staged edits;
- `env.*` settings, built-in behaviour and hive.yaml-only keys are labelled
  as such and are read-only here.

**Apply recommended** (per domain) opens a confirmation listing every
boolean it will change, `setting: current → recommended`, plus the deviating
settings it cannot change for you. Confirming stages those values.

Toggles and Apply recommended only **stage** changes, marked `staged`, in the
owning section (Features, Security or Health). Nothing is written until
**Save & close**, which saves each section through its own owner-only
endpoint — exactly as if you had edited it on that tab. Statuses are
re-evaluated from the saved config the next time the tab opens. Non-owners
see the same panels read-only.

**Posture checks** (owners only). One row per check: its control ids, a
sparkline of up to the last 60 results in the selected range (7, 30, 90 or
365 days), pass/fail/skip/error counts, the last result and when it ran, the
failing detail, the last failure time and links to the evidence (PRs, config
files, docs). **Run checks now** runs one pass through
`POST /api/compliance/posture/run` and refreshes the panel.

**Evidence & attestations** (owners only). Pick an optional date range and
open any export in the table above. Each button renders the export in an
in-app viewer with copy support; it does not force a browser download. To
attest, choose a framework, the review date (default today) and a note, then **Record attestation**. A confirmation dialog shows
what will be written to the audit log under your login. Recorded
attestations are listed below the form, newest first.
