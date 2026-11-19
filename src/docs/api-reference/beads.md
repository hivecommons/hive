# Dashboard REST API reference — Beads

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/beads` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:489` |
| `GET` | `/api/beads/{agent}` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:490` |
| `POST` | `/api/beads/{agent}` | Owner only | Beads Create | `pkg/dashboard/api.go:491` |
| `POST` | `/api/beads/reset` | Owner only | Beads Reset | `pkg/dashboard/api.go:492` |
| `POST` | `/api/beads/reset/{agent}` | Owner only | Beads Reset Agent | `pkg/dashboard/api.go:493` |
