[API reference](../api-reference.md)

## Snapshots and style

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/style` | Public | Sanitized custom dashboard CSS | `pkg/dashboard/api.go:45` |
| `GET` | `/branding/custom.css` | Dashboard auth/session | Operator branding stylesheet override, read per request (see [branding](../branding.md)) | `pkg/dashboard/server.go:1668` |
| `GET` | `/api/snapshot/frame-ancestors` | Public | Snapshot framing allowlist | `pkg/dashboard/api.go:78` |
| `GET` | `/api/snapshot` | Public | Snapshot data | `pkg/dashboard/api.go:79` |
| `GET` | `/snapshot` | Public | Public read-only snapshot page | `pkg/dashboard/api.go:80` |

