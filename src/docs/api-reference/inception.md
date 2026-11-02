# Dashboard REST API reference — Inception

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/inception/start` | Owner only | Inception Start | `pkg/dashboard/api.go:434` |
| `POST` | `/api/inception/scan` | Owner only | Inception Scan | `pkg/dashboard/api.go:435` |
| `GET` | `/api/inception/state` | Dashboard auth/session | Inception State | `pkg/dashboard/api.go:436` |
| `POST` | `/api/inception/questions` | Owner only | Inception Set Questions | `pkg/dashboard/api.go:437` |
| `POST` | `/api/inception/answer` | Owner only | Inception Answer | `pkg/dashboard/api.go:438` |
| `POST` | `/api/inception/facts` | Owner only | Inception Record Facts | `pkg/dashboard/api.go:439` |
| `GET` | `/api/inception/scaffold` | Dashboard auth/session | Inception Scaffold | `pkg/dashboard/api.go:440` |
| `POST` | `/api/inception/approve` | Owner only | Inception Approve | `pkg/dashboard/api.go:441` |
| `POST` | `/api/inception/reset` | Owner only | Inception Reset | `pkg/dashboard/api.go:442` |
| `GET` | `/api/inception/ideation-facts` | Dashboard auth/session | Inception Ideation Facts | `pkg/dashboard/api.go:443` |
| `GET` | `/api/inception/download` | Dashboard auth/session | Inception Download | `pkg/dashboard/api.go:444` |
| `GET` | `/api/inception/has-files` | Dashboard auth/session | Inception Has Files | `pkg/dashboard/api.go:445` |
| `PUT` | `/api/inception/wiki-name` | Owner only | Inception Rename Wiki | `pkg/dashboard/api.go:446` |
| `POST` | `/api/inception/import` | Owner only | Inception Import | `pkg/dashboard/api.go:447` |
