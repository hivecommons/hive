# Dashboard REST API reference — Auth and external accounts

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` to resync citations.


| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/gh-auth` | Dashboard auth/session | GitHub Auth | `pkg/dashboard/api.go:162` |
| `GET` | `/api/gh-rate-limits` | Dashboard auth/session | GitHub Rate Limits | `pkg/dashboard/api.go:163` |
| `GET` | `/api/gh-user-auth/status` | Public | GitHub User Auth Status | `pkg/dashboard/api.go:164` |
| `POST` | `/api/gh-user-auth/start` | Public | GitHub User Auth Start | `pkg/dashboard/api.go:165` |
| `POST` | `/api/gh-user-auth/poll` | Public | GitHub User Auth Poll | `pkg/dashboard/api.go:166` |
| `POST` | `/api/gh-user-auth/logout` | Public | GitHub User Auth Logout | `pkg/dashboard/api.go:167` |
| `GET` | `/api/gh-user-auth/session` | Public | GitHub User Auth Session | `pkg/dashboard/api.go:168` |
| `GET` | `/api/claude-auth/status` | Dashboard auth/session | Claude Auth Status | `pkg/dashboard/claude_auth.go:54` |
| `POST` | `/api/claude-auth/start` | Dashboard auth/session | Claude Auth Start | `pkg/dashboard/claude_auth.go:55` |
| `POST` | `/api/claude-auth/exchange` | Dashboard auth/session | Claude Auth Exchange | `pkg/dashboard/claude_auth.go:56` |
| `POST` | `/api/claude-auth/logout` | Dashboard auth/session | Claude Auth Logout | `pkg/dashboard/claude_auth.go:57` |
| `GET` | `/api/copilot-auth/status` | Dashboard auth/session | Copilot Auth Status | `pkg/dashboard/copilot_auth.go:85` |
| `POST` | `/api/copilot-auth/start` | Owner only | Copilot Auth Start | `pkg/dashboard/copilot_auth.go:86` |
| `POST` | `/api/copilot-auth/logout` | Owner only | Copilot Auth Logout | `pkg/dashboard/copilot_auth.go:88` |
| `GET` | `/api/openrouter/connect/start` | Dashboard auth/session | Open Router Start | `pkg/dashboard/openrouter.go:45` |
| `GET` | `/api/openrouter/qr` | Dashboard auth/session | Open Router QR | `pkg/dashboard/openrouter.go:46` |
| `GET` | `/api/openrouter/models` | Dashboard auth/session | Open Router Models | `pkg/dashboard/openrouter.go:47` |
| `GET` | `/api/openrouter/credit` | Dashboard auth/session | Open Router Credit | `pkg/dashboard/openrouter.go:48` |
| `GET` | `/openrouter/callback` | Public | Open Router Callback | `pkg/dashboard/openrouter.go:49` |
| `POST` | `/api/github-app/recheck` | Dashboard auth/session | GitHub App Recheck | `pkg/dashboard/server.go:1805` |
| `POST` | `/api/github-app/install-clicked` | Dashboard auth/session | GitHub App Install Clicked | `pkg/dashboard/server.go:1806` |
| `GET` | `/gh-setup` | Public | GitHub App Setup Callback | `pkg/dashboard/server.go:1807` |
| `GET` | `/api/github-owner-identity` | Owner only | The hive owner's GitHub identity for remote repos ([#18197](https://github.com/hivecommons/hive/issues/18197)): `connected`, `login`, `scope`, `connected_at`, `usable`, `reason`, `verified_at`, `scope_required` (`public_repo`), `private_repos_supported` (false) and `file` (the token file path on the spoke). Never the token | `pkg/dashboard/api.go:570` |
| `POST` | `/api/github-owner-identity/start` | Owner only | Start the consent: a GitHub device flow with the spoke's `oauth_client_id` asking for `public_repo`, separate from the scope-free login flow. Returns `user_code`, `verification_uri`, `expires_in`, `interval`, `scope` and the `flow_id` the poll must present. 502 when GitHub refused the device-code request | `pkg/dashboard/api.go:571` |
| `POST` | `/api/github-owner-identity/poll` | Owner only | Poll the consent with `{"flow_id"}`: `status` `pending` / `slow_down` (poll again), `complete` (with `login` and the identity) or `error`. On completion the granted scope must cover `public_repo`, the token must validate and the account must be the logged-in owner's; the identity is then stored on the spoke (`/data/secrets/github-owner-token`, `HIVE_GITHUB_OWNER_TOKEN_FILE` overrides) and the reference-repo policies re-resolve. 400 without a live flow or with the wrong `flow_id`; audited as `github_owner_identity_connected` / `_rejected` | `pkg/dashboard/api.go:572` |
| `DELETE` | `/api/github-owner-identity` | Owner only | Disconnect: removes the token file, drops every write-tier reference repo to read-only on the next pass, returns the previous `login` and a `note` that GitHub-side revocation is done under Settings → Applications on github.com; audited as `github_owner_identity_disconnected` | `pkg/dashboard/api.go:573` |
