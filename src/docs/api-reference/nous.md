# Dashboard REST API reference — Nous / strategy lab

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/nous/status` | Dashboard auth/session | Nous Status — includes `experimentLoop` (`dir`, `lastWriteAt`, `running`, `idleAfterSeconds`): whether the external experiment loop, the only producer of proposals, has written under `/var/run/nous/governor` within 24 evaluation cycles; ready cards then carry `experimentLoopIdle` ([#12541](https://github.com/hivecommons/hive/issues/12541)) | `pkg/dashboard/api.go:463` |
| `GET` | `/api/nous/ledger` | Dashboard auth/session | Nous Ledger | `pkg/dashboard/api.go:464` |
| `GET` | `/api/nous/principles` | Dashboard auth/session | Nous Principles | `pkg/dashboard/api.go:465` |
| `POST` | `/api/nous/approve` | Owner only | Nous Approve | `pkg/dashboard/api.go:466` |
| `POST` | `/api/nous/abort` | Owner only | Nous Abort | `pkg/dashboard/api.go:467` |
| `PUT` | `/api/nous/mode` | Owner only | Nous Mode | `pkg/dashboard/api.go:468` |
| `PUT` | `/api/nous/scope` | Owner only | Nous Scope | `pkg/dashboard/api.go:469` |
| `GET` | `/api/nous/phase` | Dashboard auth/session | Nous Phase | `pkg/dashboard/api.go:470` |
| `PUT` | `/api/nous/gate-decision` | Owner only | Nous Gate Decision | `pkg/dashboard/api.go:471` |
| `GET` | `/api/nous/gate-pending` | Dashboard auth/session | Nous Gate Pending | `pkg/dashboard/api.go:472` |
| `POST` | `/api/nous/gate-respond` | Owner only | Nous Gate Respond | `pkg/dashboard/api.go:473` |
| `GET` | `/api/nous/gate-response` | Dashboard auth/session | Nous Gate Response | `pkg/dashboard/api.go:474` |
| `GET` | `/api/nous/config` | Dashboard auth/session | Nous Config Get | `pkg/dashboard/api.go:475` |
| `PUT` | `/api/nous/config/goals` | Owner only | Nous Config Goals — legacy; the dashboard no longer exposes the research question in the ⚙ dialog, use `PATCH /api/nous/campaigns/{id}` | `pkg/dashboard/api.go:476` |
| `PUT` | `/api/nous/config/repos` | Owner only | Nous Config Repos — the allow-list of repos experiments may target (empty = any) | `pkg/dashboard/api.go:477` |
| `PUT` | `/api/nous/config/output` | Owner only | Nous Config Output | `pkg/dashboard/api.go:478` |
| `PUT` | `/api/nous/config/fast-fail` | Owner only | Nous Config Fast Fail | `pkg/dashboard/api.go:479` |
| `PUT` | `/api/nous/config/schedule` | Owner only | Nous Config Schedule | `pkg/dashboard/api.go:480` |
| `PUT` | `/api/nous/config/controllables` | Owner only | Nous Config Controllables | `pkg/dashboard/api.go:481` |
| `PUT` | `/api/nous/config/principles` | Owner only | Nous Config Principles | `pkg/dashboard/api.go:482` |
| `DELETE` | `/api/nous/principles/{id}` | Owner only | Nous Delete Principle | `pkg/dashboard/api.go:483` |
| `POST` | `/api/nous/campaign/start` | Owner only | Nous Campaign Start — archives the active campaign and starts a new experiment whose baseline restarts at 0 ([#12501](https://github.com/hivecommons/hive/issues/12501)) | `pkg/dashboard/api.go:484` |
| `GET` | `/api/nous/campaigns` | Any | Nous Campaigns List — every experiment with its per-campaign baseline, scope, results and stage ([#12532](https://github.com/hivecommons/hive/issues/12532)) | `pkg/dashboard/api.go:485` |
| `POST` | `/api/nous/campaigns` | Owner only | Nous Campaigns Create — starts an experiment alongside the running ones; 409 `scope_conflict` when a running experiment shares a repo target or both are hive-wide ([#12532](https://github.com/hivecommons/hive/issues/12532)) | `pkg/dashboard/api.go:486` |
| `PATCH` | `/api/nous/campaigns/{id}` | Owner only | Nous Campaign Patch — pause / resume / archive, or edit question, hypothesis and scope. Hypothesis/tag edits save in place (`status: "updated"`); a changed question or repo set needs `restart: true` (else 409 `restart_required`) and archives the campaign as version N while creating version N+1 with `restartedFrom` (`status: "restarted", campaign, archived`); resume, scope widening and restarts re-run the conflict guard ([#12532](https://github.com/hivecommons/hive/issues/12532)) | `pkg/dashboard/api.go:487` |
