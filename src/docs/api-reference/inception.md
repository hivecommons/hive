# Dashboard REST API reference — Inception

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.

The inception API was retired in favour of [Inception (spek) sessions](sessions.md). For one release every route below answers `410 Gone` with a JSON body (`error`, `replacement: "/api/sessions"`, `docs`); nothing is read or changed. See [Inception](../inception.md).


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/inception/start` | Dashboard auth/session | Retired (was: Inception Start): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:458` |
| `POST` | `/api/inception/scan` | Dashboard auth/session | Retired (was: Inception Scan): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:459` |
| `GET` | `/api/inception/state` | Dashboard auth/session | Retired (was: Inception State): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:460` |
| `POST` | `/api/inception/questions` | Dashboard auth/session | Retired (was: Inception Set Questions): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:461` |
| `POST` | `/api/inception/answer` | Dashboard auth/session | Retired (was: Inception Answer): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:462` |
| `POST` | `/api/inception/facts` | Dashboard auth/session | Retired (was: Inception Record Facts): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:463` |
| `GET` | `/api/inception/scaffold` | Dashboard auth/session | Retired (was: Inception Scaffold): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:464` |
| `POST` | `/api/inception/approve` | Dashboard auth/session | Retired (was: Inception Approve): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:465` |
| `POST` | `/api/inception/reset` | Dashboard auth/session | Retired (was: Inception Reset): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:466` |
| `GET` | `/api/inception/ideation-facts` | Dashboard auth/session | Retired (was: Inception Ideation Facts): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:467` |
| `GET` | `/api/inception/download` | Dashboard auth/session | Retired (was: Inception Download): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:468` |
| `GET` | `/api/inception/has-files` | Dashboard auth/session | Retired (was: Inception Has Files): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:469` |
| `PUT` | `/api/inception/wiki-name` | Dashboard auth/session | Retired (was: Inception Rename Wiki): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:470` |
| `POST` | `/api/inception/import` | Dashboard auth/session | Retired (was: Inception Import): answers `410 Gone` pointing to `/api/sessions` | `pkg/dashboard/api.go:471` |
