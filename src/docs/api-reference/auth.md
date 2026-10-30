# Dashboard REST API reference — Auth and external accounts

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/gh-auth` | Dashboard auth/session | GitHub Auth | `pkg/dashboard/api.go:175` |
| `GET` | `/api/gh-rate-limits` | Dashboard auth/session | GitHub Rate Limits | `pkg/dashboard/api.go:176` |
| `GET` | `/api/gh-user-auth/status` | Public | GitHub User Auth Status | `pkg/dashboard/api.go:177` |
| `POST` | `/api/gh-user-auth/start` | Public | GitHub User Auth Start | `pkg/dashboard/api.go:178` |
| `POST` | `/api/gh-user-auth/poll` | Public | GitHub User Auth Poll | `pkg/dashboard/api.go:179` |
| `POST` | `/api/gh-user-auth/logout` | Public | GitHub User Auth Logout | `pkg/dashboard/api.go:180` |
| `GET` | `/api/gh-user-auth/session` | Public | GitHub User Auth Session | `pkg/dashboard/api.go:181` |
| `GET` | `/api/claude-auth/status` | Dashboard auth/session | Claude Auth Status | `pkg/dashboard/claude_auth.go:54` |
| `POST` | `/api/claude-auth/start` | Dashboard auth/session | Claude Auth Start | `pkg/dashboard/claude_auth.go:55` |
| `POST` | `/api/claude-auth/exchange` | Dashboard auth/session | Claude Auth Exchange | `pkg/dashboard/claude_auth.go:56` |
| `POST` | `/api/claude-auth/logout` | Dashboard auth/session | Claude Auth Logout | `pkg/dashboard/claude_auth.go:57` |
| `GET` | `/api/copilot-auth/status` | Dashboard auth/session | Copilot Auth Status | `pkg/dashboard/copilot_auth.go:84` |
| `POST` | `/api/copilot-auth/start` | Owner only | Copilot Auth Start | `pkg/dashboard/copilot_auth.go:85` |
| `POST` | `/api/copilot-auth/logout` | Owner only | Copilot Auth Logout | `pkg/dashboard/copilot_auth.go:87` |
| `GET` | `/api/openrouter/connect/start` | Dashboard auth/session | Open Router Start | `pkg/dashboard/openrouter.go:45` |
| `GET` | `/api/openrouter/qr` | Dashboard auth/session | Open Router QR | `pkg/dashboard/openrouter.go:46` |
| `GET` | `/api/openrouter/models` | Dashboard auth/session | Open Router Models | `pkg/dashboard/openrouter.go:47` |
| `GET` | `/api/openrouter/credit` | Dashboard auth/session | Open Router Credit | `pkg/dashboard/openrouter.go:48` |
| `GET` | `/openrouter/callback` | Public | Open Router Callback | `pkg/dashboard/openrouter.go:49` |
| `POST` | `/api/github-app/recheck` | Dashboard auth/session | GitHub App Recheck | `pkg/dashboard/server.go:1626` |
| `POST` | `/api/github-app/install-clicked` | Dashboard auth/session | GitHub App Install Clicked | `pkg/dashboard/server.go:1627` |
| `GET` | `/gh-setup` | Public | GitHub App Setup Callback | `pkg/dashboard/server.go:1628` |
