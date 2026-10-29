# Dashboard REST API reference — Hub SaaS

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/saas/my-hives` | Hub auth | My Hives. Each row carries `quadrant` and the payload carries `fleet_quadrant` ([#12458](https://github.com/hivecommons/hive/issues/12458)): `axes[]` of `{axis, score, scored, reason?, delta, nudge?, criteria[], measured, unmeasured, summary?, n?}` where each `criteria[]` entry is `{name, label, measured, value, display?, score, weight, how, good, reason?, n?}`; every criterion maps to 0–100 on a fixed named curve, an axis is the weighted mean of its *measured* criteria only (an unmeasured criterion is listed with `reason` and excluded, never scored 0), `composite` averages the scored axes, and `fleet_quadrant` is the per-axis **median** across hives in view (`aggregate: "median"`, `n` hives) with each criterion's median value. `delta` is the hive minus the fleet median. Shape is additive over the earlier `axes[]{axis,score,scored,reason,delta,nudge}`/`composite`/`scored_axes` payload. See [Fleet quadrant](../fleet-health.md#fleet-quadrant) | `pkg/hub/saas.go:371`, `pkg/hub/quadrant.go` |
| `GET` | `/api/hub/widget` | Hub auth | iPhone fleet widget ([#12148](https://github.com/hivecommons/hive/issues/12148)): every hive the caller can see (hub admins: all; others: hives they own or hold a role on; pool placeholders excluded), sorted by `act_issues + act_prs` descending. Per hive `id`, `project`, `url`, `online`, `acmm_level`, `issues`, `prs` (all open, from the heartbeat; 0 from spokes that do not report them), `act_issues`, `act_prs`, `agents_running`, `agents_total`, `last_heartbeat`; `totals` (`hives`, `online`, `need_you`, the same counts summed) and `history` (fleet `act_issues`/`act_prs` per 15-minute bucket, `t` in Unix seconds, at most 48 points). See [iOS widget](../ios-widget.md#fleet-widget-hub-mode) | `pkg/hub/saas.go:372` |
| `GET` | `/api/saas/usage` | Hub auth | Usage | `pkg/hub/saas.go:381` |
| `POST` | `/api/saas/hives` | Hub auth | Create Hive | `pkg/hub/saas.go:394` |
| `GET` | `/api/saas/hives/{id}/status` | Hub auth | Hive Status | `pkg/hub/saas.go:395` |
| `POST` | `/api/saas/hives/{id}/pin-digest` | Hub auth (owner) | Pin Digest - hold the spoke at an immutable image digest (`{"digest":"sha256:..."}` or `{"sha":"<short git SHA>"}`, optional `reason`) | `pkg/hub/digest_pin.go` |
| `POST` | `/api/saas/hives/{id}/unpin-digest` | Hub auth (owner) | Unpin Digest - lift the pin and return to the tracked channel or branch tag | `pkg/hub/digest_pin.go` |
| `GET` | `/api/saas/hives/{id}/digest-pin` | Hub auth (owner) | Digest Pin - the pin with provenance and whether the spoke reports running it | `pkg/hub/digest_pin.go` |
| `GET` | `/api/saas/hives/{id}/open` | Hub handler-specific | Open Hive | `pkg/hub/saas.go:400` |
| `DELETE` | `/api/saas/hives/{id}` | Hub auth | Delete Hive | `pkg/hub/saas.go:401` |
| `POST` | `/api/saas/hives/{id}/upgrade` | Hub auth | Upgrade Hive - arms heartbeat delivery (`200 {"status":"upgrading"}`); a spoke already at its reachable target is a no-op `200 {"status":"current","current","target","channel","message"}`, not an error | `pkg/hub/saas.go:402` |
| `POST` | `/api/saas/hives/{id}/switch-branch` | Hub auth | Switch Branch | `pkg/hub/saas.go:403` |
| `PUT` | `/api/saas/hives/{id}/visibility` | Hub auth | Toggle Visibility | `pkg/hub/saas.go:410` |
| `PUT` | `/api/saas/hives/{id}/auto-upgrade` | Hub auth | Toggle Auto Upgrade | `pkg/hub/saas.go:411` |
| `PUT` | `/api/saas/hives/{id}/name` | Hub auth | Rename Hive | `pkg/hub/saas.go:415` |
| `POST` | `/api/saas/hives/{id}/forge` | Hub auth | Switch Forge | `pkg/hub/saas.go:420` |
| `POST` | `/api/saas/hives/{id}/reset-app` | Hub auth | Reset App | `pkg/hub/saas.go:421` |
| `POST` | `/api/saas/hives/{id}/restart-spoke` | Hub auth | Restart Spoke | `pkg/hub/saas.go:426` |
| `POST` | `/api/saas/hives/{id}/breaker` | Hub auth | Pause / Resume Hive (fleet breaker) | `pkg/hub/saas.go:429` |
| `GET` | `/api/saas/hive-config/{hiveID}` | Hub auth | Proxy Hive Config | `pkg/hub/saas.go:430` |
| `GET` | `/api/saas/latest-sha` | Hub handler-specific | Latest SHA | `pkg/hub/saas.go:431` |
| `GET` | `/api/hub/release/stable-promotion` | Public | Stable Promotion State | `pkg/hub/saas.go:433` |
| `POST` | `/api/saas/hub/upgrade` | Hub handler-specific | Hub Self Upgrade | `pkg/hub/saas.go:434` |
| `PUT` | `/api/saas/hub/auto-upgrade` | Hub handler-specific | Hub Auto Upgrade | `pkg/hub/saas.go:435` |
| `PUT` | `/api/hub/release/stable-promotion` | Hub admin | Stable Promotion Toggle (audit-logged) | `pkg/hub/saas.go:436` |
| `GET` | `/api/saas/auth-check` | Hub handler-specific | Saa SAuth Check | `pkg/hub/saas.go:441` |
| `POST` | `/api/saas/user-token` | Hub auth | User Token | `pkg/hub/saas.go:452` |
| `GET` | `/api/saas/hives/{id}/access` | Hub auth | Access List | `pkg/hub/saas.go:453` |
| `GET` | `/api/saas/grantable-users` | Hub auth | Grantable Users | `pkg/hub/saas.go:454` |
| `POST` | `/api/saas/hives/{id}/access` | Hub auth | Access Add | `pkg/hub/saas.go:455` |
| `DELETE` | `/api/saas/hives/{id}/access/{username}` | Hub auth | Access Remove | `pkg/hub/saas.go:456` |
| `POST` | `/api/saas/hives/{id}/request-access` | Hub auth | Request Access | `pkg/hub/saas.go:457` |
| `GET` | `/api/saas/hives/{id}/requests` | Hub auth | Get Requests | `pkg/hub/saas.go:458` |
| `GET` | `/api/saas/hives/{id}/timeline` | Hub auth | Hive Timeline | `pkg/hub/saas.go:459` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/approve` | Hub auth | Approve Request | `pkg/hub/saas.go:461` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/deny` | Hub auth | Deny Request | `pkg/hub/saas.go:462` |
| `PUT` | `/api/saas/hives/{id}/approve-access/{username}` | Hub auth | Approve Access | `pkg/hub/saas.go:463` |
| `DELETE` | `/api/saas/hives/{id}/deny-access/{username}` | Hub auth | Deny Access | `pkg/hub/saas.go:464` |
| `GET` | `/api/saas/access-status` | Hub handler-specific | Access Status | `pkg/hub/saas.go:465` |
| `POST` | `/api/saas/request-provision` | Hub auth | Request Provision | `pkg/hub/saas.go:473` |
| `PUT` | `/api/saas/approve-provision/{username}` | Hub handler-specific | Approve Provision | `pkg/hub/saas.go:474` |
| `DELETE` | `/api/saas/deny-provision/{username}` | Hub handler-specific | Deny Provision | `pkg/hub/saas.go:475` |
| `GET` | `/api/saas/admin/available-placeholders` | Hub handler-specific | Available Placeholders | `pkg/hub/saas.go:476` |
| `GET` | `/api/saas/admin/users` | Hub handler-specific | Admin Users | `pkg/hub/saas.go:479` |

`GET /api/saas/admin/users` includes cached `affiliation`, `affiliation_source`, `profile_bio`, `profile_location`, `public_activity`, `top_repo`, and `top_repo_url` fields for the Hub Admin — Users **AFFILIATION** column. The cache identifies who a signup appears affiliated with from public GitHub/GHE footprint only, ranked as profile company, contributed-to repo owner/org (excluding the personal namespace), public org membership, then top-repo owner/independent fallback; bio/location are supporting tooltip context, and `public_activity` buckets recent public events as `quiet`, `active`, or `prolific`. Provider API refreshes are asynchronous and daily, so the table never blocks on GitHub/GHE and never requires Hive activity.

| `PUT` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Update User | `pkg/hub/saas.go:502` |
| `DELETE` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Delete User | `pkg/hub/saas.go:503` |
| `POST` | `/api/saas/admin/impersonate/exit` | Hub handler-specific | Impersonate Exit | `pkg/hub/saas.go:509` |
| `POST` | `/api/saas/admin/impersonate/{username}` | Hub handler-specific | Impersonate Start | `pkg/hub/saas.go:510` |
| `GET` | `/api/saas/impersonation-status` | Hub auth | Impersonation Status | `pkg/hub/saas.go:511` |
| `POST` | `/api/saas/hives/{id}/assign` | Hub auth | Assign Hive | `pkg/hub/saas.go:512` |
| `POST` | `/api/saas/hives/{id}/reset-assignment` | Hub handler-specific | Reset Assignment | `pkg/hub/saas.go:516` |
| `GET` | `/api/saas/cluster-health` | Hub handler-specific | Cluster Health | `pkg/hub/saas.go:517` |
| `POST` | `/api/saas/admin/alert-ack` | Hub handler-specific | Alert Ack | `pkg/hub/saas.go:530` |
| `GET` | `/api/saas/admin/cluster-app-keys` | Hub handler-specific | Get Cluster App Keys | `pkg/hub/saas.go:534` |
| `PUT` | `/api/saas/admin/cluster-app-keys/{clusterID}` | Hub handler-specific | Put Cluster App Key | `pkg/hub/saas.go:535` |
| `POST` | `/api/saas/admin/hub-banner` | Hub handler-specific | Send Hub Banner | `pkg/hub/saas.go:536` |
| `DELETE` | `/api/saas/admin/hub-banner` | Hub handler-specific | Clear Hub Banner | `pkg/hub/saas.go:537` |
| `GET` | `/api/saas/admin/hub-banner` | Hub handler-specific | Get Hub Banner | `pkg/hub/saas.go:538` |
| `POST` | `/api/saas/slack/user/{username}` | Hub auth | Slack Message User | `pkg/hub/saas.go:545` |
| `POST` | `/api/saas/hives/{id}/slack` | Hub auth | Slack Message Hive Owner | `pkg/hub/saas.go:546` |
| `POST` | `/api/saas/admin/slack/broadcast` | Hub handler-specific | Slack Broadcast | `pkg/hub/saas.go:547` |
| `POST` | `/api/saas/admin/journey-snooze` | Hub handler-specific | Journey Snooze | `pkg/hub/saas.go:548` |
| `GET` | `/api/saas/admin/journey-status` | Hub handler-specific | Journey Status | `pkg/hub/saas.go:549` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:391` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:392` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:393` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:425` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:416` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:439` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:440` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:447` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:451` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:460` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:477` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:478` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:487` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:492` |
| `GET` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Get | `pkg/hub/saas.go:493` |
| `PUT` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Update + hot reload | `pkg/hub/saas.go:494` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:500` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:501` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:527` |
| `POST` | `/api/saas/hives/bulk` | Hub auth | Bulk Hive Action | `pkg/hub/saas_bulk.go:89` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:391` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:392` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:393` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:425` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:416` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:439` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:440` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:447` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:451` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:460` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:477` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:478` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:487` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:492` |
| `GET` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Get | `pkg/hub/saas.go:493` |
| `PUT` | `/api/saas/admin/notifications` | Hub admin | Hub Notifications Update + hot reload | `pkg/hub/saas.go:494` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:500` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:501` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:527` |
