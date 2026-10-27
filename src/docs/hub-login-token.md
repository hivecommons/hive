# Hub login token

The Hub's browser sign-in is a GitHub OAuth authorization-code flow. This page
records what the resulting **user access token** is used for, how long it lives,
and what the empty-scope authorize request does and does not guarantee. It is an
audit, not a behavior change: token storage, retention and refresh are
unchanged.

## Flow

1. `startProviderLogin` redirects to GitHub with `scope=` empty, the exact
   `redirect_uri` and a `state` nonce (`src/pkg/hub/oauth.go:604`).
2. The callback exchanges the code at `defaultGHTokenURL`
   (`src/pkg/hub/oauth.go:47`) in `exchangeGitHubLogin`
   (`src/pkg/hub/oauth.go:934`). `client_id`, `client_secret`, `code` and
   `redirect_uri` travel in a form-encoded POST body, never in the URL
   (`src/pkg/hub/oauth.go:944`). `redirect_uri` is `oauthRedirectURI()`
   (`src/pkg/hub/saas_auth.go:371`), the same value as the authorize request.
3. The token is encrypted with `encryptToken` (`src/pkg/hub/saas_crypto.go:34`)
   and stored on the user record (`src/pkg/hub/oauth.go:850`).

## Consumers of the stored token

| Consumer | Location | Needs the token for |
|---|---|---|
| `handleUserToken`, `POST /api/saas/user-token` | `src/pkg/hub/saas_auth.go:740`, route `src/pkg/hub/saas.go:449` | Returns the caller's own token (or any user's, for a Hub admin) after checking hive membership; the audit log records each issue. |
| `decryptStoredGitHubToken` | `src/pkg/hub/top_repo.go:217`, used at `src/pkg/hub/top_repo.go:180` | Top-repo profile lookups for GitHub-provider users; falls back to the Hub's own token when absent. |
| `CreateHiveRequest.GitHubToken` | `src/pkg/hub/saas_provision.go:919` | A caller-supplied PAT-style token for hosted-hive provisioning. It is a request field, not read from the stored record; a client may populate it from the user-token endpoint. |

The admin user listing clears `EncryptedToken` before responding
(`src/pkg/hub/saas_users_admin.go:331`), so the ciphertext is never exposed
there.

## Lifetime and refresh

GitHub OAuth-app user tokens do not expire by default and no refresh token is
issued. The Hub overwrites the stored token on each successful login
(`src/pkg/hub/oauth.go:850`); there is no refresh job and no revocation on
logout. A token stops working only if the user revokes the app authorization or
GitHub invalidates it.

## Empty scope is not a revocation

The authorize request sends an empty `scope` so the Hub asks for identity only.
GitHub documents that a scope-less request can still return scopes the user
previously granted to the same OAuth app. Do not claim that an empty scope
revokes earlier grants; the stored token may carry broader scopes than this flow
requests.

## Related credentials and flows

- The GitHub App installation credential is separate from the human login token
  and remains the source for repo writes and advisory posting.
- Direct-spoke device flow (`src/pkg/dashboard/api_auth.go`) and hosted-spoke SSO
  are unchanged by this flow.
- Whether to offer a browser redirect to direct-route spokes, and how they would
  hold a confidential client secret, is **undecided**.
