# Dashboard REST API reference — Configuration

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `PUT` | `/api/config/triggers` | Verified owner only | Compile-to-bool and durably compare-and-save `{rules:[{name,agent,expr,priority}],preset?:{index,kind,repo}}` (preset `kind` is `issue.opened`, `pr.opened` or `ci.failed`; the server writes `event.kind == <kind> && event.repo == <repo>` into rule `index`); exact `If-Match` required (428 missing, 412 stale), validation 400, persistence failure 500 leaves live rules unchanged. No enabled field or source/webhook installation | `pkg/dashboard/api_event_triggers.go` |
| `GET` | `/api/themes` | Dashboard auth/session | Embedded dashboard/contributor theme catalog with swatches for Settings → Appearance; `?scope=contributor` filters profile themes | `pkg/dashboard/api.go:49` |
| `GET` | `/api/theme.css` | Dashboard auth/session | Effective dashboard theme stylesheet with strong ETag / 304 support; `?theme=<id>` previews an embedded preset; `?scope=contributor` emits contributor token aliases and layers hive custom CSS | `pkg/dashboard/api.go:50` |
| `GET` | `/api/config` | Dashboard auth/session | Config | `pkg/dashboard/api.go:51` |
| `GET` | `/api/config/triggers` | Verified owner only | Ordered Event triggers, opaque `version`/ETag, existing agents and managed repository values/labels; applies on next evaluation without restart | `pkg/dashboard/api_event_triggers.go` |
| `GET` | `/api/config/dashboard/theme` | Owner only | Dashboard theme selection, overrides, effective theme, and built-in catalog | `pkg/dashboard/api.go:60` |
| `PUT` | `/api/config/dashboard/theme` | Owner only | Persist `dashboard.theme` and `dashboard.theme_overrides` with validation, CSS/background guardrails, and audit logging | `pkg/dashboard/api.go:61` |
| `GET` | `/api/config/download` | Owner only | Config Download | `pkg/dashboard/api.go:52` |
| `GET` | `/api/config/sla` | Owner only | Delivery SLA targets: `{targets:{start_to_pr_h,pr_to_merge_h,open_to_merge_h}, effective_targets:{...}, defaults:{...}}`, hours, from `sla.targets` in `hive.yaml` with defaults 4/24/72 | `pkg/dashboard/api.go:279` |
| `PUT` | `/api/config/sla` | Owner only | Replace `sla.targets` (`{targets:{start_to_pr_h,pr_to_merge_h,open_to_merge_h}}`; each 0 (default) to 8760, validated before anything is persisted); the Throughput Delivery SLA panel recolours on the next status poll | `pkg/dashboard/api.go:280` |
| `GET` | `/api/config/export` | Owner only | Redacted JSON export of the effective config, layer files, side files, and provenance | `pkg/dashboard/api_config_export.go` |
| `GET` | `/api/config/provenance` | Owner only | Config Provenance | `pkg/dashboard/api.go:55` |
| `GET` | `/api/config/variables` | Dashboard auth/session | Variables List | `pkg/dashboard/api.go:56` |
| `GET` | `/api/config/authorized-users` | Dashboard auth/session | Authorized Users List | `pkg/dashboard/api.go:59` |
| `PUT` | `/api/config/variables/{name}` | Owner only | Variable Upsert | `pkg/dashboard/api.go:62` |
| `DELETE` | `/api/config/variables/{name}` | Owner only | Variable Delete | `pkg/dashboard/api.go:63` |

| `GET` | `/api/config/agent/{name}` | Dashboard auth/session | Agent Config Get; `general` carries the API proxy's GitHub API hourly cap: `githubApiHourlyCap` (this agent's `agents.<name>.github_api_hourly_cap`, `null` = inherit), `githubApiHourlyCapEffective` (the cap the proxy applies now; 0 = no cap), `githubApiHourlyCapFleet` (the fleet default) and `githubApiHourlyCapBase` (the base agent of a replica or review head, else empty) | `pkg/dashboard/api.go:208` |
| `PUT` | `/api/config/agent/{name}/general` | Owner only | Agent Config General; `githubApiHourlyCap` sets `agents.<name>.github_api_hourly_cap` (a whole number, 0 = no cap, `null` = inherit the fleet default), applies to the proxy without a restart and to the agent's replicas and review heads; refused (400) on a replica or review head. Audited as `config_agent_general` with the old and new cap | `pkg/dashboard/api.go:209` |
| `PUT` | `/api/config/agent/{name}/cadences` | Owner only | Agent Config Cadences; each mode sent becomes operator-owned (`governor.cadence_owners`). Wrapped by the admin MCP `agent.cadences` operation | `pkg/dashboard/api.go:210` |
| `PUT` | `/api/config/agent/{name}/models` | Owner only | Agent Config Models | `pkg/dashboard/api.go:211` |
| `PUT` | `/api/config/agent/{name}/pipeline` | Owner only | Agent Config Pipeline | `pkg/dashboard/api.go:212` |
| `PUT` | `/api/config/agent/{name}/hooks` | Owner only | Agent Config Hooks | `pkg/dashboard/api.go:213` |
| `PUT` | `/api/config/agent/{name}/restrictions` | Owner only | Agent Config Restrictions | `pkg/dashboard/api.go:214` |
| `PUT` | `/api/config/agent/{name}/stats` | Owner only | Agent Config Stats | `pkg/dashboard/api.go:215` |
| `GET` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt | `pkg/dashboard/api.go:216` |
| `PUT` | `/api/config/agent/{name}/prompt` | Owner only | Agent Prompt Save | `pkg/dashboard/api.go:217` |
| `GET` | `/api/config/agent/{name}/export` | Dashboard auth/session | Agent Export | `pkg/dashboard/api.go:218` |
| `PUT` | `/api/config/agent/{name}/channels` | Owner only | Agent Config Channels | `pkg/dashboard/api.go:219` |
| `PUT` | `/api/config/agent/{name}/tools` | Owner only | Agent Config Tools | `pkg/dashboard/api.go:220` |
| `PUT` | `/api/config/agent/{name}/connections` | Owner only | Agent Config Connections | `pkg/dashboard/api.go:221` |
| `GET` | `/api/config/agent/{name}/variables` | Dashboard auth/session | Per-agent Variables tab: the agent's own `${VAR}` definitions plus the hive-level ones it inherits (each flagged `overridden` when the agent redefines it) and the seed-only exec/http gate flags. Never returns values — only name, type, scope and a non-secret source hint | `pkg/dashboard/api_variables.go:298` |
| `PUT` | `/api/config/agent/{name}/variables/{var}` | Owner only | Create or update one per-agent variable (`static`/`env` only; `script`/`http` are 403 and seed-only; scope `template` or `both`; secret-looking static values rejected). Persisted on the agent entry; overrides a same-named hive-level variable in this agent's kick prompt | `pkg/dashboard/api_variables.go:299` |
| `DELETE` | `/api/config/agent/{name}/variables/{var}` | Owner only | Delete one per-agent variable (404 for inherited hive-level variables — delete those via `/api/config/variables/{name}`) | `pkg/dashboard/api_variables.go:300` |
| `GET` | `/api/config/stat-sources` | Dashboard auth/session | Stat Sources | `pkg/dashboard/api.go:222` |
| `GET` | `/api/config/governor` | Dashboard auth/session | Governor Config Get | `pkg/dashboard/api.go:224` |
| `PATCH` | `/api/config/governor` | Owner only | Update `merge_train_stall_warn_s`; omitted values stay unchanged, 0 disables warnings; returns the effective interval | `pkg/dashboard/api_governor_merge_train.go` |
| `PUT` | `/api/config/governor/sensing` | Owner only | Governor Sensing | `pkg/dashboard/api.go:228` |
| `PUT` | `/api/config/governor/thresholds` | Owner only | Governor Thresholds | `pkg/dashboard/api.go:229` |
| `PUT` | `/api/config/governor/labels` | Owner only | Governor Labels | `pkg/dashboard/api.go:236` |
| `PUT` | `/api/config/governor/budget` | Owner only | Governor Budget | `pkg/dashboard/api.go:237` |
| `PUT` | `/api/config/governor/notifications` | Owner only | Governor Notifications | `pkg/dashboard/api.go:241` |
| `PUT` | `/api/config/governor/health` | Owner only | Governor Health | `pkg/dashboard/api.go:242` |
| `PUT` | `/api/config/governor/logging` | Owner only | Governor Logging | `pkg/dashboard/api.go:258` |
| `PUT` | `/api/config/governor/attribution` | Owner only | Governor Attribution | `pkg/dashboard/api.go:259` |
| `PUT` | `/api/config/governor/hub` | Owner only | Governor Hub | `pkg/dashboard/api.go:260` |
| `PUT` | `/api/config/governor/litellm` | Owner only | Governor Lite LLM | `pkg/dashboard/api.go:261` |
| `PUT` | `/api/config/governor/trajectory` | Owner only | Governor Trajectory | `pkg/dashboard/api.go:262` |
| `GET` | `/api/config/governor/backup` | Owner only | Backup Key Status (presence + safe source label; never the key value) | `pkg/dashboard/backup_key.go` |
| `PUT` | `/api/config/governor/backup` | Owner only | Backup Key Set (64-hex AES-256 key; stored 0600, path-only in `hive.yaml`) | `pkg/dashboard/backup_key.go` |
| `DELETE` | `/api/config/governor/backup` | Owner only | Backup Key Clear (backups are refused again) | `pkg/dashboard/backup_key.go` |
| `GET` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Status | `pkg/dashboard/api.go:309` |
| `PUT` | `/api/config/governor/bob` | Owner only | Governor Bob Key | `pkg/dashboard/api.go:310` |
| `DELETE` | `/api/config/governor/bob` | Owner only | Governor Bob Key Clear | `pkg/dashboard/api.go:311` |

| `GET` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Get (`aggregate` or `per_repo`) | `pkg/dashboard/api.go:230` |
| `PUT` | `/api/config/governor/cadence-scope` | Owner only | Governor Cadence Scope Set (`cadenceScope`/`cadence_scope`: `aggregate`, `per_repo`, or empty for default) | `pkg/dashboard/api.go:231` |
| `POST` | `/api/config/governor/bob/test` | Dashboard auth/session | Governor Bob Key Test | `pkg/dashboard/api.go:313` |
| `POST` | `/api/config/governor/litellm/test` | Dashboard auth/session | Governor Lite LLMTest | `pkg/dashboard/api.go:314` |
| `GET` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways List | `pkg/dashboard/api.go:317` |
| `PUT` | `/api/config/governor/gateways` | Owner only | Governor Gateways Upsert | `pkg/dashboard/api.go:318` |
| `DELETE` | `/api/config/governor/gateways/{name}` | Owner only | Governor Gateways Delete | `pkg/dashboard/api.go:319` |
| `POST` | `/api/config/governor/gateways/{name}/test` | Dashboard auth/session | Governor Gateways Test | `pkg/dashboard/api.go:320` |
| `POST` | `/api/config/governor/gateways/discover` | Owner only | Governor Gateways Discover | `pkg/dashboard/api.go:321` |
| `POST` | `/api/config/governor/agents` | Owner only | Governor Add Agent | `pkg/dashboard/api.go:326` |
| `DELETE` | `/api/config/governor/agents/{name}` | Owner only | Governor Remove Agent | `pkg/dashboard/api.go:327` |
| `PUT` | `/api/config/governor/repos` | Owner only | Governor Repos | `pkg/dashboard/api.go:328` |
| `POST` | `/api/config/governor/repos/check-access` | Dashboard auth/session | Governor Repo Check Access | `pkg/dashboard/api.go:332` |
| `PUT` | `/api/config/github` | Owner only | Config GitHub | `pkg/dashboard/api.go:333` |
| `GET` | `/api/config/github/forge-apps` | Dashboard auth/session | Config GitHub Forge Apps | `pkg/dashboard/api.go:336` |
| `GET` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Get | `pkg/dashboard/api.go:396` |
| `PUT` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Set | `pkg/dashboard/api.go:397` |
| `GET` | `/api/config/backends` | Dashboard auth/session | Backends | `pkg/dashboard/api.go:398` |
| `GET` | `/api/compliance/status` | Merger or owner | Evaluated compliance controls for the frameworks in `compliance.frameworks` (`compliance.Report`, JSON): per control `meets`/`deviates`/`off`/`not_covered` with current vs recommended value per mapped setting, a summary, and the non-certification disclaimer — see [Compliance](../compliance.md) ([#11078](https://github.com/hivecommons/hive/issues/11078)) | `pkg/dashboard/api.go:388` |
| `GET` | `/api/compliance/posture` | Merger or owner | Posture-check catalogue, effective interval / window / history retention, and the latest run (`compliance.PostureRun`: schedule/manual trigger, `triggered_by` identity, and per-check `pass`/`fail`/`skip`/`error`, detail, control ids, evidence refs); `?since=` (RFC 3339, `YYYY-MM-DD` or a look-back such as `168h`) adds every run since then, oldest first, capped at 1000 — see [Compliance](../compliance.md#posture-checks-catalogue) ([#11079](https://github.com/hivecommons/hive/issues/11079)) | `pkg/dashboard/api.go:389` |
| `POST` | `/api/compliance/posture/run` | Merger or owner | Run the posture checks now, attributed to the authenticated user, and record the pass in the history; audited as `compliance_posture_run`; `409` when no framework is selected or a pass is already running ([#11079](https://github.com/hivecommons/hive/issues/11079)) | `pkg/dashboard/api.go:390` |
| `GET` | `/api/compliance/posture/history` | Owner only | Posture history for `?since=`/`?until=` (default the last 30 days) as one series per check: points (`at`, `status`), status counts, `last_fail_at` and the latest result with detail and evidence refs; at most the 1000 most recent runs — see [Compliance](../compliance.md#evidence-exports-and-attestations) ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:391` |
| `GET` | `/api/compliance/export` | Owner only | Evidence download for `?since=`/`?until=`: `kind=controls` (`json`/`md`), `posture` (`json`/`csv`), `audit` (`json`), `config` (redacted effective config + sha256, `json`), `attestations` (`json`/`csv`) or `bundle` (`json`). Every format includes source, period, generator, selected profile versions, attesters, retention and a per-section SHA-256 manifest; audited as `compliance_export` with the SHA-256 of the delivered bytes ([#11081](https://github.com/hivecommons/hive/issues/11081), [#13582](https://github.com/hivecommons/hive/issues/13582)) | `pkg/dashboard/api.go:392` |
| `GET` | `/api/compliance/attestations` | Owner only | Owner attestations, newest first (`?framework=` filters) ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:393` |
| `POST` | `/api/compliance/attestations` | Owner only | Record `{"framework", "reviewed_on", "note"}` for the signed-in owner; audited as `compliance_attestation`; `201` with the attestation, `400` for an unknown framework, a bad or future date, or a note over 2000 characters ([#11081](https://github.com/hivecommons/hive/issues/11081)) | `pkg/dashboard/api.go:394` |
| `PUT` | `/api/config/governor/compliance` | Owner only | Settings → Compliance framework picker: `{"frameworks": [...]}` sets `compliance.frameworks` (empty list deselects all, absent key leaves it unchanged; unknown/duplicate ids are a `400` before any change); audited as `config_compliance`; returns the re-evaluated `compliance.Report` ([#11080](https://github.com/hivecommons/hive/issues/11080)) | `pkg/dashboard/api.go:573` |

| `GET` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Get | `pkg/dashboard/api.go:233` |
| `PUT` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Set | `pkg/dashboard/api.go:234` |
| `PUT` | `/api/config/governor/coverage-target` | Owner only | Governor Coverage Target | `pkg/dashboard/api.go:235` |
| `POST` | `/api/config/governor/budget/reset` | Owner only | Governor Budget Reset | `pkg/dashboard/api.go:238` |
| `PUT` | `/api/config/governor/watchdog` | Owner only | Governor Watchdog | `pkg/dashboard/api.go:243` |
| `GET` | `/api/config/escalation` | Owner only | Escalation Config Get | `pkg/dashboard/api.go:246` |
| `PUT` | `/api/config/escalation` | Owner only | Escalation Config Set | `pkg/dashboard/api.go:247` |
| `GET` | `/api/config/write-surface` | Owner only | Lane write allowlist (`write_surface.allowlist`, [#9587](https://github.com/hivecommons/hive/issues/9587)): `allowlist`, `ops`, `warnings` | `pkg/dashboard/api.go:569` |
| `PUT` | `/api/config/write-surface` | Owner only | Replace the lane write allowlist; `{"allowlist": {}}` clears it; unknown operation or bad lane name is a 400 with nothing changed | `pkg/dashboard/api.go:570` |
| `GET` | `/api/config/review` | Dashboard auth/session | Review Config Get | `pkg/dashboard/api.go:254` |
| `PUT` | `/api/config/review` | Owner only | Review Config Set | `pkg/dashboard/api.go:257` |
| `PUT` | `/api/config/governor/features` | Owner only | Governor Features | `pkg/dashboard/api.go:263` |
| `GET` | `/api/config/governor/general-advanced` | Owner only | Settings → General governor settings: `eval_interval_s`, the loop pacing knobs with `loop_defaults`, `explain_mode`, `attribution_trailer`, `api_budget_pacing` (`governor.api_budget_pacing`) and `api_budget_pacing_state` `{enabled, active, factor, effective_interval_s, burn_per_min, reason}`, what the loop is doing with it | `pkg/dashboard/api.go:264` |
| `PUT` | `/api/config/governor/general-advanced` | Owner only | Change those settings; every field is optional and an absent one is left alone. `api_budget_pacing` (boolean) turns GitHub API budget pacing on or off from the next eval cycle. Returns the GET payload | `pkg/dashboard/api.go:265` |
| `GET` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Get | `pkg/dashboard/api.go:269` |
| `PUT` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Set | `pkg/dashboard/api.go:270` |
| `PATCH` | `/api/config/auto-merge` | Owner only | Partially update auto-merge settings, including nested `merge_train`; omitted fields stay unchanged; returns effective settings and bot-author metadata | `pkg/dashboard/api_config_automerge.go` |
| `GET` | `/api/config/convergence` | Owner only | Convergence Config Get | `pkg/dashboard/api.go:276` |
| `PUT` | `/api/config/convergence` | Owner only | Convergence Config Set | `pkg/dashboard/api.go:278` |
| `GET` | `/api/config/governor/advisory` | Owner only | Governor Advisory Get | `pkg/dashboard/api.go:282` |
| `PUT` | `/api/config/governor/advisory` | Owner only | Governor Advisory Set | `pkg/dashboard/api.go:283` |
| `GET` | `/api/config/governor/replan` | Owner only | Governor Replan Get | `pkg/dashboard/api.go:284` |
| `PUT` | `/api/config/governor/replan` | Owner only | Governor Replan Set | `pkg/dashboard/api.go:285` |
| `GET` | `/api/config/decisions/delegate` | Owner only | "Let hive decide" settings (`decisions.delegate`) with defaults resolved: `enabled` (unset = on), `enabled_set`, `repos` (`{owner/repo: {enabled}}`), `llm` (`{enabled, model, model_masked}`; unset = off), `llm_reviewer_ready`, `llm_reviewer_path` (`http`, `claude-cli` or `none`, picked by `governor.reviewer_backend`), `llm_reviewer_reason` (why the decider turn cannot run; empty when ready) and `llm_model_effective` (the decider turn's model; empty on `claude-cli` means the CLI's default; the endpoint and its key are never returned), `effect_timeout`, `effect_timeout_effective`, `project_repos`. A stored `llm.model` that looks like a key is masked | `pkg/dashboard/api_config_decisions_delegate.go` |
| `PUT` | `/api/config/decisions/delegate` | Owner only | Change `decisions.delegate`: `enabled`, `repos` (the full override map when sent; an entry with `enabled: null` is dropped; a new key must be in `project.repos`), `llm.enabled`, `llm.model` (a model name; a key-like value is refused) and `effect_timeout`. Unknown fields and invalid values are 400 with nothing changed. Saved, audited as `config_decisions_delegate` with the actor and each change before and after, and applied without a restart. Starts no decision and merges nothing | `pkg/dashboard/api_config_decisions_delegate.go` |
| `GET` | `/api/config/governor/work-source` | Owner only | Governor Work Source Get (Jira secrets/TLS fields reported as set/unset only) | `pkg/dashboard/api.go:295` |
| `PUT` | `/api/config/governor/work-source` | Owner only | Governor Work Source Set (including Jira Data Center TLS settings) | `pkg/dashboard/api.go:296` |
| `PUT` | `/api/config/governor/security` | Owner only | Governor Security; `sentinel.repoSensitivePaths` (`{owner/repo: [globs] or null}`) sets or clears single repositories' own Sentinel list (`project.repo_policies[].sentinel_sensitive_paths`; `[]` flags no path, `null` returns to the hive-wide list) and leaves repositories it does not name, and the hive-wide `sensitivePaths`, unchanged; a blank or malformed glob or a repository outside `project.repos` is a 400 with nothing changed, and a change is audited per repository | `pkg/dashboard/api.go:297` |
| `GET` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Get | `pkg/dashboard/api.go:299` |
| `PUT` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Set | `pkg/dashboard/api.go:300` |
| `GET` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Get | `pkg/dashboard/api.go:315` |
| `PUT` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Set | `pkg/dashboard/api.go:316` |
| `GET` | `/api/config/convergence` | Owner only | Convergence Config Get | `pkg/dashboard/api.go:276` |
| `PUT` | `/api/config/convergence` | Owner only | Convergence Config Set | `pkg/dashboard/api.go:278` |
| `GET` | `/api/config/governor/advisory` | Owner only | Governor Advisory Get | `pkg/dashboard/api.go:282` |
| `PUT` | `/api/config/governor/advisory` | Owner only | Governor Advisory Set | `pkg/dashboard/api.go:283` |
| `GET` | `/api/config/governor/replan` | Owner only | Governor Replan Get | `pkg/dashboard/api.go:284` |
| `PUT` | `/api/config/governor/replan` | Owner only | Governor Replan Set | `pkg/dashboard/api.go:285` |
| `GET` | `/api/config/governor/work-source` | Owner only | Governor Work Source Get (Jira secrets/TLS fields reported as set/unset only) | `pkg/dashboard/api.go:295` |
| `PUT` | `/api/config/governor/work-source` | Owner only | Governor Work Source Set (including Jira Data Center TLS settings) | `pkg/dashboard/api.go:296` |
| `PUT` | `/api/config/governor/security` | Owner only | Governor Security | `pkg/dashboard/api.go:297` |
| `GET` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Get | `pkg/dashboard/api.go:299` |
| `PUT` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Set | `pkg/dashboard/api.go:300` |
| `GET` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Get | `pkg/dashboard/api.go:315` |
| `PUT` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Set | `pkg/dashboard/api.go:316` |
| `PUT` | `/api/workflows/enabled` | Owner only | Turn model-based stage derivation (`workflow.enabled`, [#17620](https://github.com/hivecommons/hive/issues/17620)) on or off from the dashboard, for hosted hives with no `hive.yaml` access. Body `{enabled: bool}`; persists `hive.yaml`; the stage engine and tracker read the live flag so it applies on the next derivation pass without a restart. Turning on also provisions every bound repository's model labels now (`warnings[]` lists repositories that could not be provisioned; the next sweep retries). Returns `{enabled, changed, bindings[], warnings?[]}`. Audited `workflow_enabled` / `workflow_disabled`; `400` on a non-boolean body | `pkg/dashboard/api_workflows.go:28` |
