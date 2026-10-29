[API reference](../api-reference.md)

## Beads

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/beads` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:481` |
| `GET` | `/api/beads/{agent}` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:482` |
| `POST` | `/api/beads/{agent}` | Owner only | Beads Create | `pkg/dashboard/api.go:483` |
| `POST` | `/api/beads/reset` | Owner only | Beads Reset | `pkg/dashboard/api.go:484` |
| `POST` | `/api/beads/reset/{agent}` | Owner only | Beads Reset Agent | `pkg/dashboard/api.go:485` |

