# Dashboard REST API reference — Snapshots and style

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/style` | Public | Sanitized custom dashboard CSS | `pkg/dashboard/api.go:47` |
| `GET` | `/branding/custom.css` | Dashboard auth/session | Operator branding stylesheet override, read per request (see [branding](../branding.md)) | `pkg/dashboard/server.go:1841` |
| `GET` | `/api/snapshot/frame-ancestors` | Public | Snapshot framing allowlist | `pkg/dashboard/api.go:87` |
| `GET` | `/api/snapshot` | Public | Snapshot data | `pkg/dashboard/api.go:88` |
| `GET` | `/snapshot` | Public | Public read-only snapshot page | `pkg/dashboard/api.go:89` |
