# Dashboard REST API reference — Configuration

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/themes` | Dashboard auth/session | Embedded dashboard/contributor theme catalog with swatches for Settings → Appearance; `?scope=contributor` filters profile themes | `pkg/dashboard/api.go:47` |
| `GET` | `/api/theme.css` | Dashboard auth/session | Effective dashboard theme stylesheet with strong ETag / 304 support; `?theme=<id>` previews an embedded preset; `?scope=contributor` emits contributor token aliases and layers hive custom CSS | `pkg/dashboard/api.go:48` |
| `GET` | `/api/config` | Dashboard auth/session | Config | `pkg/dashboard/api.go:49` |
| `GET` | `/api/config/triggers` | Verified owner only | Ordered Event triggers, opaque `version`/ETag, existing agents and managed repository values/labels; applies on next evaluation without restart | `pkg/dashboard/api_event_triggers.go` |
| `PUT` | `/api/config/triggers` | Verified owner only | Compile-to-bool and durably compare-and-save `{rules:[{name,agent,expr,priority}],preset?:{index,kind,repo}}`; exact `If-Match` required (428 missing, 412 stale), validation 400, persistence failure 500 leaves live rules unchanged. No enabled field or source/webhook installation | `pkg/dashboard/api_event_triggers.go` |
| `GET` | `/api/config/dashboard/theme` | Owner only | Dashboard theme selection, overrides, effective theme, and built-in catalog | `pkg/dashboard/api.go:58` |
| `PUT` | `/api/config/dashboard/theme` | Owner only | Persist `dashboard.theme` and `dashboard.theme_overrides` with validation, CSS/background guardrails, and audit logging | `pkg/dashboard/api.go:59` |
| `GET` | `/api/config/download` | Owner only | Config Download | `pkg/dashboard/api.go:50` |
| `GET` | `/api/config/sla` | Owner only | Delivery SLA targets: `{targets:{start_to_pr_h,pr_to_merge_h,open_to_merge_h}, effective_targets:{...}, defaults:{...}}`, hours, from `sla.targets` in `hive.yaml` with defaults 4/24/72 | `pkg/dashboard/api.go:259` |
| `PUT` | `/api/config/sla` | Owner only | Replace `sla.targets` (`{targets:{start_to_pr_h,pr_to_merge_h,open_to_merge_h}}`; each 0 (default) to 8760, validated before anything is persisted); the Throughput Delivery SLA panel recolours on the next status poll | `pkg/dashboard/api.go:260` |
| `GET` | `/api/config/export` | Owner only | Redacted JSON export of the effective config, layer files, side files, and provenance | `pkg/dashboard/api_config_export.go` |
| `GET` | `/api/config/provenance` | Owner only | Config Provenance | `pkg/dashboard/api.go:53` |
| `GET` | `/api/config/variables` | Dashboard auth/session | Variables List | `pkg/dashboard/api.go:54` |
| `GET` | `/api/config/authorized-users` | Dashboard auth/session | Authorized Users List | `pkg/dashboard/api.go:57` |
| `PUT` | `/api/config/variables/{name}` | Owner only | Variable Upsert | `pkg/dashboard/api.go:60` |
| `DELETE` | `/api/config/variables/{name}` | Owner only | Variable Delete | `pkg/dashboard/api.go:61` |

| `GET` | `/api/config/agent/{name}` | Dashboard auth/session | Agent Config Get | `pkg/dashboard/api.go:194` |
| `PUT` | `/api/config/agent/{name}/general` | Owner only | Agent Config General | `pkg/dashboard/api.go:195` |
| `PUT` | `/api/config/agent/{name}/cadences` | Owner only | Agent Config Cadences | `pkg/dashboard/api.go:196` |
| `PUT` | `/api/config/agent/{name}/models` | Owner only | Agent Config Models | `pkg/dashboard/api.go:197` |
| `PUT` | `/api/config/agent/{name}/pipeline` | Owner only | Agent Config Pipeline | `pkg/dashboard/api.go:198` |
| `PUT` | `/api/config/agent/{name}/hooks` | Owner only | Agent Config Hooks | `pkg/dashboard/api.go:199` |
| `PUT` | `/api/config/agent/{name}/restrictions` | Owner only | Agent Config Restrictions | `pkg/dashboard/api.go:200` |
| `PUT` | `/api/config/agent/{name}/stats` | Owner only | Agent Config Stats | `pkg/dashboard/api.go:201` |
| `GET` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt | `pkg/dashboard/api.go:202` |
| `PUT` | `/api/config/agent/{name}/prompt` | Owner only | Agent Prompt Save | `pkg/dashboard/api.go:203` |
| `GET` | `/api/config/agent/{name}/export` | Dashboard auth/session | Agent Export | `pkg/dashboard/api.go:204` |
| `PUT` | `/api/config/agent/{name}/channels` | Owner only | Agent Config Channels | `pkg/dashboard/api.go:205` |
| `PUT` | `/api/config/agent/{name}/tools` | Owner only | Agent Config Tools | `pkg/dashboard/api.go:206` |
| `PUT` | `/api/config/agent/{name}/connections` | Owner only | Agent Config Connections | `pkg/dashboard/api.go:207` |
| `GET` | `/api/config/agent/{name}/variables` | Dashboard auth/session | Per-agent Variables tab: the agent's own `${VAR}` definitions plus the hive-level ones it inherits (each flagged `overridden` when the agent redefines it) and the seed-only exec/http gate flags. Never returns values — only name, type, scope and a non-secret source hint | `pkg/dashboard/api_variables.go:298` |
| `PUT` | `/api/config/agent/{name}/variables/{var}` | Owner only | Create or update one per-agent variable (`static`/`env` only; `script`/`http` are 403 and seed-only; scope `template` or `both`; secret-looking static values rejected). Persisted on the agent entry; overrides a same-named hive-level variable in this agent's kick prompt | `pkg/dashboard/api_variables.go:299` |
| `DELETE` | `/api/config/agent/{name}/variables/{var}` | Owner only | Delete one per-agent variable (404 for inherited hive-level variables — delete those via `/api/config/variables/{name}`) | `pkg/dashboard/api_variables.go:300` |
| `GET` | `/api/config/stat-sources` | Dashboard auth/session | Stat Sources | `pkg/dashboard/api.go:208` |
| `GET` | `/api/config/governor` | Dashboard auth/session | Governor Config Get | `pkg/dashboard/api.go:210` |
| `PUT` | `/api/config/governor/sensing` | Owner only | Governor Sensing | `pkg/dashboard/api.go:213` |
| `PUT` | `/api/config/governor/thresholds` | Owner only | Governor Thresholds | `pkg/dashboard/api.go:214` |
| `PUT` | `/api/config/governor/labels` | Owner only | Governor Labels | `pkg/dashboard/api.go:221` |
| `PUT` | `/api/config/governor/budget` | Owner only | Governor Budget | `pkg/dashboard/api.go:222` |
| `PUT` | `/api/config/governor/notifications` | Owner only | Governor Notifications | `pkg/dashboard/api.go:226` |
| `PUT` | `/api/config/governor/health` | Owner only | Governor Health | `pkg/dashboard/api.go:227` |
| `PUT` | `/api/config/governor/logging` | Owner only | Governor Logging | `pkg/dashboard/api.go:239` |
| `PUT` | `/api/config/governor/attribution` | Owner only | Governor Attribution | `pkg/dashboard/api.go:240` |
| `PUT` | `/api/config/governor/hub` | Owner only | Governor Hub | `pkg/dashboard/api.go:241` |
| `PUT` | `/api/config/governor/litellm` | Owner only | Governor Lite LLM | `pkg/dashboard/api.go:242` |
| `PUT` | `/api/config/governor/trajectory` | Owner only | Governor Trajectory | `pkg/dashboard/api.go:243` |
| `GET` | `/api/config/governor/backup` | Owner only | Backup Key Status (presence + safe source label; never the key value) | `pkg/dashboard/backup_key.go` |
| `PUT` | `/api/config/governor/backup` | Owner only | Backup Key Set (64-hex AES-256 key; stored 0600, path-only in `hive.yaml`) | `pkg/dashboard/backup_key.go` |
| `DELETE` | `/api/config/governor/backup` | Owner only | Backup Key Clear (backups are refused again) | `pkg/dashboard/backup_key.go` |
| `GET` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Status | `pkg/dashboard/api.go:288` |
| `PUT` | `/api/config/governor/bob` | Owner only | Governor Bob Key | `pkg/dashboard/api.go:289` |
| `DELETE` | `/api/config/governor/bob` | Owner only | Governor Bob Key Clear | `pkg/dashboard/api.go:290` |

| `GET` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Get (`aggregate` or `per_repo`) | `pkg/dashboard/api.go:215` |
| `PUT` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Set (`cadenceScope`/`cadence_scope`: `aggregate`, `per_repo`, or empty for default) | `pkg/dashboard/api.go:216` |
| `POST` | `/api/config/governor/bob/test` | Dashboard auth/session | Governor Bob Key Test | `pkg/dashboard/api.go:292` |
| `POST` | `/api/config/governor/litellm/test` | Dashboard auth/session | Governor Lite LLMTest | `pkg/dashboard/api.go:293` |
| `GET` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways List | `pkg/dashboard/api.go:296` |
| `PUT` | `/api/config/governor/gateways` | Owner only | Governor Gateways Upsert | `pkg/dashboard/api.go:297` |
| `DELETE` | `/api/config/governor/gateways/{name}` | Owner only | Governor Gateways Delete | `pkg/dashboard/api.go:298` |
| `POST` | `/api/config/governor/gateways/{name}/test` | Dashboard auth/session | Governor Gateways Test | `pkg/dashboard/api.go:299` |
| `POST` | `/api/config/governor/gateways/discover` | Owner only | Governor Gateways Discover | `pkg/dashboard/api.go:300` |
| `POST` | `/api/config/governor/agents` | Owner only | Governor Add Agent | `pkg/dashboard/api.go:305` |
| `DELETE` | `/api/config/governor/agents/{name}` | Owner only | Governor Remove Agent | `pkg/dashboard/api.go:306` |
| `PUT` | `/api/config/governor/repos` | Owner only | Governor Repos | `pkg/dashboard/api.go:307` |
| `POST` | `/api/config/governor/repos/check-access` | Dashboard auth/session | Governor Repo Check Access | `pkg/dashboard/api.go:311` |
| `PUT` | `/api/config/github` | Owner only | Config GitHub | `pkg/dashboard/api.go:312` |
| `GET` | `/api/config/github/forge-apps` | Dashboard auth/session | Config GitHub Forge Apps | `pkg/dashboard/api.go:315` |
| `GET` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Get | `pkg/dashboard/api.go:364` |
| `PUT` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Set | `pkg/dashboard/api.go:365` |
| `GET` | `/api/config/backends` | Dashboard auth/session | Backends | `pkg/dashboard/api.go:366` |
| `GET` | `/api/compliance/status` | Merger or owner | Evaluated compliance controls for the frameworks in `compliance.frameworks` (`compliance.Report`, JSON): per control `meets`/`deviates`/`off`/`not_covered` with current vs recommended value per mapped setting, a summary, and the non-certification disclaimer — see [Compliance](../compliance.md) ([#11078](https://github.com/hivecommons/hive/issues/11078)) | `pkg/dashboard/api.go:356` |
| `GET` | `/api/compliance/posture` | Merger or owner | Posture-check catalogue, effective interval / window / history retention, and the latest run (`compliance.PostureRun`: schedule/manual trigger, `triggered_by` identity, and per-check `pass`/`fail`/`skip`/`error`, detail, control ids, evidence refs); `?since=` (RFC 3339, `YYYY-MM-DD` or a look-back such as `168h`) adds every run since then, oldest first, capped at 1000 — see [Compliance](../compliance.md#posture-checks-catalogue) ([#11079](https://github.com/hivecommons/hive/issues/11079)) | `pkg/dashboard/api.go:357` |
| `POST` | `/api/compliance/posture/run` | Merger or owner | Run the posture checks now, attributed to the authenticated user, and record the pass in the history; audited as `compliance_posture_run`; `409` when no framework is selected or a pass is already running ([#11079](https://github.com/hivecommons/hive/issues/11079)) | `pkg/dashboard/api.go:358` |
| `GET` | `/api/compliance/posture/history` | Owner only | Posture history for `?since=`/`?until=` (default the last 30 days) as one series per check: points (`at`, `status`), status counts, `last_fail_at` and the latest result with detail and evidence refs; at most the 1000 most recent runs — see [Compliance](../compliance.md#evidence-exports-and-attestations) ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:359` |
| `GET` | `/api/compliance/export` | Owner only | Evidence download for `?since=`/`?until=`: `kind=controls` (`json`/`md`), `posture` (`json`/`csv`), `audit` (`json`), `config` (redacted effective config + sha256, `json`), `attestations` (`json`/`csv`) or `bundle` (`json`). Every format includes source, period, generator, selected profile versions, attesters, retention and a per-section SHA-256 manifest; audited as `compliance_export` with the SHA-256 of the delivered bytes ([#11081](https://github.com/hivecommons/hive/issues/11081), [#13582](https://github.com/hivecommons/hive/issues/13582)) | `pkg/dashboard/api.go:360` |
| `GET` | `/api/compliance/attestations` | Owner only | Owner attestations, newest first (`?framework=` filters) ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:361` |
| `POST` | `/api/compliance/attestations` | Owner only | Record `{"framework", "reviewed_on", "note"}` for the signed-in owner; audited as `compliance_attestation`; `201` with the attestation, `400` for an unknown framework, a bad or future date, or a note over 2000 characters ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:362` |
| `PUT` | `/api/config/governor/compliance` | Owner only | Settings → Compliance framework picker: `{"frameworks": [...]}` sets `compliance.frameworks` (empty list deselects all, absent key leaves it unchanged; unknown/duplicate ids are a `400` before any change); audited as `config_compliance`; returns the re-evaluated `compliance.Report` ([#11080](https://github.com/hivecommons/hive/issues/11080)) | `pkg/dashboard/api.go:530` |

| `GET` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Get | `pkg/dashboard/api.go:218` |
| `PUT` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Set | `pkg/dashboard/api.go:219` |
| `PUT` | `/api/config/governor/coverage-target` | Owner only | Governor Coverage Target | `pkg/dashboard/api.go:220` |
| `POST` | `/api/config/governor/budget/reset` | Owner only | Governor Budget Reset | `pkg/dashboard/api.go:223` |
| `PUT` | `/api/config/governor/watchdog` | Owner only | Governor Watchdog | `pkg/dashboard/api.go:228` |
| `GET` | `/api/config/escalation` | Owner only | Escalation Config Get | `pkg/dashboard/api.go:231` |
| `PUT` | `/api/config/escalation` | Owner only | Escalation Config Set | `pkg/dashboard/api.go:232` |
| `GET` | `/api/config/write-surface` | Owner only | Lane write allowlist (`write_surface.allowlist`, [#9587](https://github.com/hivecommons/hive/issues/9587)): `allowlist`, `ops`, `warnings` | `pkg/dashboard/api.go:526` |
| `PUT` | `/api/config/write-surface` | Owner only | Replace the lane write allowlist; `{"allowlist": {}}` clears it; unknown operation or bad lane name is a 400 with nothing changed | `pkg/dashboard/api.go:527` |
| `GET` | `/api/config/review` | Dashboard auth/session | Review Config Get | `pkg/dashboard/api.go:235` |
| `PUT` | `/api/config/review` | Owner only | Review Config Set | `pkg/dashboard/api.go:238` |
| `PUT` | `/api/config/governor/features` | Owner only | Governor Features | `pkg/dashboard/api.go:244` |
| `GET` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Get | `pkg/dashboard/api.go:245` |
| `PUT` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Set | `pkg/dashboard/api.go:246` |
| `GET` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Get | `pkg/dashboard/api.go:250` |
| `PUT` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Set | `pkg/dashboard/api.go:251` |
| `GET` | `/api/config/convergence` | Owner only | Convergence Config Get | `pkg/dashboard/api.go:256` |
| `PUT` | `/api/config/convergence` | Owner only | Convergence Config Set | `pkg/dashboard/api.go:258` |
| `GET` | `/api/config/governor/advisory` | Owner only | Governor Advisory Get | `pkg/dashboard/api.go:262` |
| `PUT` | `/api/config/governor/advisory` | Owner only | Governor Advisory Set | `pkg/dashboard/api.go:263` |
| `GET` | `/api/config/governor/replan` | Owner only | Governor Replan Get | `pkg/dashboard/api.go:264` |
| `PUT` | `/api/config/governor/replan` | Owner only | Governor Replan Set | `pkg/dashboard/api.go:265` |
| `GET` | `/api/config/governor/work-source` | Owner only | Governor Work Source Get (Jira secrets/TLS fields reported as set/unset only) | `pkg/dashboard/api.go:275` |
| `PUT` | `/api/config/governor/work-source` | Owner only | Governor Work Source Set (including Jira Data Center TLS settings) | `pkg/dashboard/api.go:276` |
| `PUT` | `/api/config/governor/security` | Owner only | Governor Security | `pkg/dashboard/api.go:277` |
| `GET` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Get | `pkg/dashboard/api.go:278` |
| `PUT` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Set | `pkg/dashboard/api.go:279` |
| `GET` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Get | `pkg/dashboard/api.go:294` |
| `PUT` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Set | `pkg/dashboard/api.go:295` |
