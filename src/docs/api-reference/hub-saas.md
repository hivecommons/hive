# Dashboard REST API reference — Hub SaaS

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/saas/my-hives` | Hub auth | My Hives. Each row carries `quadrant` and the payload carries `fleet_quadrant` ([#12458](https://github.com/hivecommons/hive/issues/12458)): `axes[]` of `{axis, score, scored, reason?, delta, nudge?, criteria[], measured, unmeasured, summary?, n?}` where each `criteria[]` entry is `{name, label, measured, value, display?, score, weight, how, good, reason?, n?}`; every criterion maps to 0–100 on a fixed named curve, an axis is the weighted mean of its *measured* criteria only (an unmeasured criterion is listed with `reason` and excluded, never scored 0), `composite` averages the scored axes, and `fleet_quadrant` is the per-axis **median** across hives in view (`aggregate: "median"`, `n` hives) with each criterion's median value. `delta` is the hive minus the fleet median. Shape is additive over the earlier `axes[]{axis,score,scored,reason,delta,nudge}`/`composite`/`scored_axes` payload. See [Fleet quadrant](../fleet-health.md#fleet-quadrant) | `pkg/hub/saas.go:369`, `pkg/hub/quadrant.go` |
| `GET` | `/api/hub/widget` | Hub auth | iPhone fleet widget ([#12148](https://github.com/hivecommons/hive/issues/12148)): every hive the caller can see (hub admins: all; others: hives they own or hold a role on; pool placeholders excluded), sorted by `act_issues + act_prs` descending. Per hive `id`, `project`, `url`, `online`, `acmm_level`, `issues`, `prs` (all open, from the heartbeat; 0 from spokes that do not report them), `act_issues`, `act_prs`, `agents_running`, `agents_total`, `last_heartbeat`; `totals` (`hives`, `online`, `need_you`, the same counts summed) and `history` (fleet `act_issues`/`act_prs` per 15-minute bucket, `t` in Unix seconds, at most 48 points). See [iOS widget](../ios-widget.md#fleet-widget-hub-mode) | `pkg/hub/saas.go:370` |
| `GET` | `/api/saas/usage` | Hub auth | Usage | `pkg/hub/saas.go:379` |
| `POST` | `/api/saas/hives` | Hub auth | Create Hive | `pkg/hub/saas.go:389` |
| `GET` | `/api/saas/hives/{id}/status` | Hub auth | Hive Status | `pkg/hub/saas.go:390` |
| `POST` | `/api/saas/hives/{id}/pin-digest` | Hub auth (owner or admin) | Pin Digest - hold the spoke at an immutable image digest (`{"digest":"sha256:..."}` or `{"sha":"<short git SHA>"}`, optional `reason`) | `pkg/hub/digest_pin.go` |
| `POST` | `/api/saas/hives/{id}/unpin-digest` | Hub auth (owner or admin) | Unpin Digest - lift the pin and return to the tracked channel or branch tag | `pkg/hub/digest_pin.go` |
| `GET` | `/api/saas/hives/{id}/digest-pin` | Hub auth (owner or admin) | Digest Pin - the pin with provenance and whether the spoke reports running it | `pkg/hub/digest_pin.go` |
| `GET` | `/api/saas/hives/{id}/open` | Hub handler-specific | Open Hive | `pkg/hub/saas.go:395` |
| `DELETE` | `/api/saas/hives/{id}` | Hub auth (owner or admin) | Delete Hive | `pkg/hub/saas.go:396` |
| `POST` | `/api/saas/hives/{id}/upgrade` | Hub auth | Upgrade Hive - arms heartbeat delivery (`200 {"status":"upgrading"}`); a spoke already at its reachable target is a no-op `200 {"status":"current","current","target","channel","message"}`, not an error | `pkg/hub/saas.go:397` |
| `POST` | `/api/saas/hives/{id}/switch-branch` | Hub auth | Switch Branch | `pkg/hub/saas.go:398` |
| `PUT` | `/api/saas/hives/{id}/visibility` | Hub auth | Toggle Visibility | `pkg/hub/saas.go:405` |
| `PUT` | `/api/saas/hives/{id}/auto-upgrade` | Hub auth | Toggle Auto Upgrade | `pkg/hub/saas.go:406` |
| `PUT` | `/api/saas/hives/{id}/name` | Hub auth | Rename Hive | `pkg/hub/saas.go:410` |
| `POST` | `/api/saas/hives/{id}/forge` | Hub auth | Switch Forge | `pkg/hub/saas.go:415` |
| `POST` | `/api/saas/hives/{id}/reset-app` | Hub auth | Reset App | `pkg/hub/saas.go:416` |
| `POST` | `/api/saas/hives/{id}/restart-spoke` | Hub auth | Restart Spoke | `pkg/hub/saas.go:421` |
| `POST` | `/api/saas/hives/{id}/breaker` | Hub auth | Pause / Resume Hive (fleet breaker) | `pkg/hub/saas.go:424` |
| `GET` | `/api/saas/hive-config/{hiveID}` | Hub auth | Proxy Hive Config | `pkg/hub/saas.go:425` |
| `GET` | `/api/saas/latest-sha` | Hub handler-specific | Latest SHA | `pkg/hub/saas.go:426` |
| `GET` | `/api/hub/release/stable-promotion` | Public | Stable Promotion State | `pkg/hub/saas.go:428` |
| `POST` | `/api/saas/hub/upgrade` | Hub handler-specific | Hub Self Upgrade | `pkg/hub/saas.go:429` |
| `PUT` | `/api/saas/hub/auto-upgrade` | Hub handler-specific | Hub Auto Upgrade | `pkg/hub/saas.go:430` |
| `PUT` | `/api/hub/release/stable-promotion` | Hub admin | Stable Promotion Toggle (audit-logged) | `pkg/hub/saas.go:431` |
| `GET` | `/api/saas/auth-check` | Hub handler-specific | Saa SAuth Check | `pkg/hub/saas.go:436` |
| `POST` | `/api/saas/user-token` | Hub auth | User Token | `pkg/hub/saas.go:447` |
| `GET` | `/api/saas/hives/{id}/access` | Hub auth | Access List | `pkg/hub/saas.go:448` |
| `GET` | `/api/saas/grantable-users` | Hub auth | Grantable Users | `pkg/hub/saas.go:449` |
| `POST` | `/api/saas/hives/{id}/access` | Hub auth | Access Add | `pkg/hub/saas.go:450` |
| `DELETE` | `/api/saas/hives/{id}/access/{username}` | Hub auth | Access Remove | `pkg/hub/saas.go:451` |
| `POST` | `/api/saas/hives/{id}/request-access` | Hub auth | Request Access | `pkg/hub/saas.go:452` |
| `GET` | `/api/saas/hives/{id}/requests` | Hub auth | Get Requests | `pkg/hub/saas.go:453` |
| `GET` | `/api/saas/hives/{id}/timeline` | Hub auth | Hive Timeline | `pkg/hub/saas.go:454` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/approve` | Hub auth | Approve Request | `pkg/hub/saas.go:456` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/deny` | Hub auth | Deny Request | `pkg/hub/saas.go:457` |
| `PUT` | `/api/saas/hives/{id}/approve-access/{username}` | Hub auth | Approve Access | `pkg/hub/saas.go:458` |
| `DELETE` | `/api/saas/hives/{id}/deny-access/{username}` | Hub auth | Deny Access | `pkg/hub/saas.go:459` |
| `GET` | `/api/saas/access-status` | Hub handler-specific | Access Status | `pkg/hub/saas.go:460` |
| `POST` | `/api/saas/request-provision` | Hub auth | Request Provision | `pkg/hub/saas.go:468` |
| `PUT` | `/api/saas/approve-provision/{username}` | Hub handler-specific | Approve Provision | `pkg/hub/saas.go:469` |
| `DELETE` | `/api/saas/deny-provision/{username}` | Hub handler-specific | Deny Provision | `pkg/hub/saas.go:470` |
| `GET` | `/api/saas/admin/available-placeholders` | Hub handler-specific | Available Placeholders | `pkg/hub/saas.go:471` |
| `GET` | `/api/saas/admin/users` | Hub handler-specific | Admin Users | `pkg/hub/saas.go:474` |

`GET /api/saas/admin/users` includes cached `affiliation`, `affiliation_source`, `affiliation_confidence` (only for a `colony:gitdm` or `colony:devstats` source), `cncf_contributions` (only while the hub devstats provider is on), `profile_bio`, `profile_location`, `public_activity`, `top_repo`, and `top_repo_url` fields for the Hub Admin Users **AFFILIATION** column. The cache identifies who a signup appears affiliated with from public GitHub/GHE footprint only, ranked as profile company, contributed-to repo owner/org (excluding the personal namespace), public org membership, then top-repo owner/independent fallback; bio/location are supporting tooltip context, and `public_activity` buckets recent public events as `quiet`, `active`, or `prolific`. Provider API refreshes are asynchronous and daily, so the table never blocks on GitHub/GHE and never requires Hive activity. Colony affiliation ([Hub affiliation from colony](../telemetry.md#hub-affiliation-from-colony)) ranks below an organization the user or an admin set on the hub (`affiliation_source` `user` or `admin`) and the GitHub/GHE profile company, and above the footprint heuristics.

| `PUT` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Update User | `pkg/hub/saas.go:497` |
| `DELETE` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Delete User | `pkg/hub/saas.go:498` |
| `POST` | `/api/saas/admin/impersonate/exit` | Hub handler-specific | Impersonate Exit | `pkg/hub/saas.go:504` |
| `POST` | `/api/saas/admin/impersonate/{username}` | Hub handler-specific | Impersonate Start | `pkg/hub/saas.go:505` |
| `GET` | `/api/saas/impersonation-status` | Hub auth | Impersonation Status | `pkg/hub/saas.go:506` |
| `POST` | `/api/saas/hives/{id}/assign` | Hub auth | Assign Hive | `pkg/hub/saas.go:507` |
| `POST` | `/api/saas/hives/{id}/reset-assignment` | Hub handler-specific | Reset Assignment | `pkg/hub/saas.go:511` |
| `GET` | `/api/saas/cluster-health` | Hub handler-specific | Cluster Health | `pkg/hub/saas.go:512` |
| `POST` | `/api/saas/admin/alert-ack` | Hub handler-specific | Alert Ack | `pkg/hub/saas.go:525` |
| `GET` | `/api/saas/admin/cluster-app-keys` | Hub handler-specific | Get Cluster App Keys | `pkg/hub/saas.go:529` |
| `PUT` | `/api/saas/admin/cluster-app-keys/{clusterID}` | Hub handler-specific | Put Cluster App Key | `pkg/hub/saas.go:530` |
| `POST` | `/api/saas/admin/hub-banner` | Hub handler-specific | Send Hub Banner | `pkg/hub/saas.go:531` |
| `DELETE` | `/api/saas/admin/hub-banner` | Hub handler-specific | Clear Hub Banner | `pkg/hub/saas.go:532` |
| `GET` | `/api/saas/admin/hub-banner` | Hub handler-specific | Get Hub Banner | `pkg/hub/saas.go:533` |
| `POST` | `/api/saas/slack/user/{username}` | Hub auth | Slack Message User | `pkg/hub/saas.go:540` |
| `POST` | `/api/saas/hives/{id}/slack` | Hub auth | Slack Message Hive Owner | `pkg/hub/saas.go:541` |
| `POST` | `/api/saas/admin/slack/broadcast` | Hub handler-specific | Slack Broadcast | `pkg/hub/saas.go:542` |
| `POST` | `/api/saas/admin/journey-snooze` | Hub handler-specific | Journey Snooze | `pkg/hub/saas.go:543` |
| `GET` | `/api/saas/admin/journey-status` | Hub handler-specific | Journey Status | `pkg/hub/saas.go:544` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:386` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:387` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:388` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:420` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:411` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:434` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:435` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:442` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:446` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:455` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:472` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:473` |
| `GET` | `/api/saas/admin/profile-share/counts` | Hub admin | Customers profile sharing: consent counts per hive (`hive_id`, `name`, `consents`, `awaiting_reconfirmation`). No person, field choice or value. `404` while `HIVE_HUB_CUSTOMERS_SHARE_ENABLED` is off | `pkg/hub/customer_consent.go` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:482` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:487` |
| `GET` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Get | `pkg/hub/saas.go:488` |
| `PUT` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Update + hot reload | `pkg/hub/saas.go:489` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:495` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:496` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:522` |
| `POST` | `/api/saas/hives/bulk` | Hub auth | Bulk Hive Action | `pkg/hub/saas_bulk.go:89` |
| `GET` | `/api/saas/build-contains` | Hub auth | Does each hive's running build contain a PR or commit ([#18915](https://github.com/hivecommons/hive/issues/18915)): `?q=<PR number or commit SHA>` returns `{query:{kind,pr?,commit?}, commit?, reason?, hives:[{id,name?,commit?,channel?,contains,reason?}]}` for the hives the caller can see in My Hives. `contains` is `yes`, `no` or `unknown`, computed from the hive's already-reported commit and repository ancestry: `yes` when the PR's merge commit (or the SHA) is the running commit or an ancestor, `no` when ancestry resolved and it is not (an unmerged PR is `no`), `unknown` (with `reason`) when the commit cannot be resolved or the lookup failed. Never a guess. `400` when `q` is neither a PR number nor a SHA. See [Build visibility](../release-channels.md) | `pkg/hub/build_contains.go:225` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:386` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:387` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:388` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:420` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:411` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:434` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:435` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:442` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:446` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:455` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:472` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:473` |
| `GET` | `/api/saas/admin/profile-share/counts` | Hub admin | Customers profile sharing: consent counts per hive (`hive_id`, `name`, `consents`, `awaiting_reconfirmation`). No person, field choice or value. `404` while `HIVE_HUB_CUSTOMERS_SHARE_ENABLED` is off | `pkg/hub/customer_consent.go` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:482` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:487` |
| `GET` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Get | `pkg/hub/saas.go:488` |
| `PUT` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Update + hot reload | `pkg/hub/saas.go:489` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:495` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:496` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:522` |
| `GET` | `/api/ask/status` | Hub auth | Browser session only (no bearer, no impersonation). Ask AI help status for the signed-in person: `enabled`, `text_version`, `disclosure`, `consented`, `docs_url`, `ai_label`. Registered only when `HIVE_ASK_ENABLED` is on and fully configured; otherwise `404`. Never returns the vendor key, project id or integration id. See [Ask AI help](../hub-deployment.md#ask-ai-help) | `pkg/hub/ask.go:131` |
| `PUT` | `/api/ask/consent` | Hub auth | Browser session only. Record the person's consent to the Ask disclosure: body `{text_version}`; `409` when the page showed an older text; `204` on success. One audit line, no text | `pkg/hub/ask.go:132` |
| `DELETE` | `/api/ask/consent` | Hub auth | Browser session only. Withdraw Ask consent; takes effect on the next request; `204` | `pkg/hub/ask.go:133` |
| `POST` | `/api/ask` | Hub auth | Browser session only. Ask a docs-help question: body `{question, section?, conversation?}` (`section` is an id from the closed list in `pkg/askai/sections.go`; `conversation` is the opaque handle from the previous answer). Always `200` with `{outcome, answer?, sources[]?, uncertain?, conversation?, retry_after_seconds?, message?, ai_generated, docs_url}`; `outcome` is one of `ok`, `no_cited_answer`, `refused_secret_shape`, `rate_limited`, `breaker_open`, `upstream_error`, `not_consented`, `invalid_question`. Only the question and the section label are sent to Kapa.ai | `pkg/hub/ask.go:134` |
| `GET` | `/api/ask/stats` | Hub admin | Ask cost view: counts by outcome, today's total against the daily budget, breaker state, effective limits. No per-person data | `pkg/hub/ask.go:135` |
| `GET` | `/ask-panel.js` | Hub handler-specific | Ask AI help panel script (`pkg/askai/web/ask-panel.js`), served only when Ask is on; `404` otherwise | `pkg/hub/ask.go:136` |
| `GET` | `/ask-panel.css` | Hub handler-specific | Ask AI help panel stylesheet, served only when Ask is on; `404` otherwise | `pkg/hub/ask.go:137` |
| `POST` | `/api/hub/ask/relay` | Per-hive heartbeat bearer | Ask AI help relay for hosted hives ([#19183](https://github.com/hivecommons/hive/issues/19183)): bearer plus `X-Hive-ID`; body `{question, section?, conversation?}` (no person field); answers the same result shape as `POST /api/ask` with `relay_disabled` for a hive that is not hosted, is listed in `HIVE_ASK_RELAY_DISABLED_HIVES`, or when `HIVE_ASK_RELAY_HIVES=none`. Charged against the hive's hourly quota and the hub-wide limits; conversation handles are bound to the hive. Registered only when Ask is on at the hub | `pkg/hub/ask.go` |
