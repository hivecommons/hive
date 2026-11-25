# Self-hosted hub deployment

Hive can run either as a spoke dashboard or as the hub. The same Go binary serves both roles; `HIVE_MODE=hub` selects the hub server in `cmd/hive/main.go`.

Use a self-hosted hub when you need a private registry, private SaaS provisioning, air-gapped control, or a hub URL other than `https://hive.hivecommons.dev`. If you only need a hosted spoke, use the hosted Hive Hub instead.

## What `Dockerfile.hub` builds

`src/Dockerfile.hub` builds only `./cmd/hive`, installs `kubectl`, creates `/data` and `/etc/hive`, sets `HIVE_MODE=hub`, and runs:

```sh
hive --config /etc/hive/hive.yaml
```

The hub listen port is still controlled by `HIVE_HUB_PORT` in the binary and defaults to `3001`. The Dockerfile exposes port `80`, so set `HIVE_HUB_PORT=80` if your container platform expects the process to bind that exposed port.

## Required storage

Use a persistent volume mounted at `/data`. The production/manual-provisioning docs describe the live hub as namespace `hive-hub`, PVC `hive-hub-data-rwx`, mounted at `/data`.

The hub stores live SaaS state under `/data`:

| Path | Purpose |
|---|---|
| `/data/saas/hives/<id>/meta.json` | Per-hive SaaS record used by auth, upgrades, assignments, and dashboard links. |
| `/data/hub-registry.json` | Fleet registry populated by spoke heartbeats. |
| `/data/saas/hub-secret.key` | Optional fallback heartbeat bearer secret when `HIVE_HUB_SECRET` is not set on the hub. |
| `/data/saas/clusters.json` | Cluster definitions used by automated provisioning. |

Use ReadWriteMany storage for hosted spoke PVCs. The provisioning template in `pkg/hub/saas_provision.go` requests `ReadWriteMany` and its rolling update strategy relies on both old and new pods being able to mount `/data` during a surge.

## Minimal hub environment

| Variable | Required | Notes |
|---|---:|---|
| `HIVE_MODE=hub` | Yes | Selects hub mode. |
| `HIVE_HUB_PORT` | No | Defaults to `3001`. Set to `80` for the published hub image contract if needed. |
| `HIVE_CONFIG` or `--config` | No | Defaults to `/etc/hive/hive.yaml`; `Dockerfile.hub` passes `--config /etc/hive/hive.yaml`. |
| `HIVE_HUB_SECRET` | Strongly recommended | Master secret from which heartbeat bearers and per-hive keys derive. If unset, the hub reads `/data/saas/hub-secret.key` when present. The hub additionally maintains a rotation generation set alongside it — see [security-model.md](security-model.md#9-platform-and-supply-chain); rotate via the admin endpoint, never by swapping the master in place. |
| `HIVE_HUB_OAUTH_CLIENT_ID` / `HIVE_HUB_OAUTH_CLIENT_SECRET` | Enables **GitHub** login | These two enable the GitHub provider specifically. Human login is multi-provider: Google, IBMid, Red Hat, Microsoft Entra ID, and a generic OIDC slot are each enabled independently via `HIVE_HUB_OIDC_<PROVIDER>_CLIENT_ID` (+ `_CLIENT_SECRET`, `_ISSUER`, `_SCOPES`, `_DISPLAY` — see [env-vars.md](env-vars.md#hub-login-providers)). With two or more providers configured the hub shows a provider picker at `/login`; with exactly one it redirects straight into it; with none it logs OAuth disabled and registers no login routes. A provider with a client id but a missing/invalid issuer is silently skipped (check the `hub login enabled providers=` startup log line). |
| `HIVE_GITHUB_TOKEN` or GitHub App config | Required for operations that call GitHub | Same GitHub auth rules as a spoke. |
| `HIVE_HUB_SLACK_BOT_TOKEN`, `HIVE_NTFY_SERVER`, `HIVE_NTFY_TOPIC` | No | Optional hub notifications. |
| `HIVE_BACKUP_*`, `OCI_*`, `HIVE_KUBECONFIG_DIR` | No | Required only for disaster-recovery backup and OCI/FSS provisioning flows. See `backup-restore.md`. |

## Hub config shape

The hub still reads `hive.yaml`. The hub-specific code mainly uses the `hub` block, GitHub credentials, dashboard/auth settings, and any provisioning-related data under `/data/saas`. A minimal seed keeps the normal project and GitHub validation satisfied:

```yaml
project:
  org: your-org
  repos: [your-repo]
  primary_repo: your-repo

github:
  token: ${HIVE_GITHUB_TOKEN}

dashboard:
  auth_token: ${HIVE_DASHBOARD_TOKEN}

hub:
  enabled: true
  url: https://hive.example.com
```

Keep secret values in Kubernetes Secrets or environment variables; do not write token values directly into `hive.yaml`.

## Registering spokes to your hub

A spoke points at the hub with its config `hub.url` or the `HIVE_HUB_URL` override. Protected hubs require the same heartbeat secret on every spoke:

```yaml
hub:
  enabled: true
  url: https://hive.example.com
```

```yaml
env:
  - name: HIVE_HUB_URL
    value: https://hive.example.com
  - name: HIVE_HUB_SECRET
    valueFrom:
      secretKeyRef:
        name: hive-secrets
        key: HIVE_HUB_SECRET
```

Without `HIVE_HUB_SECRET`, a protected hub returns `401 unauthorized`, the spoke logs `hub heartbeat rejected status=401`, and the hub shows the hive offline. This matches the gotcha in `manual-provisioning.md`.

### Customer profile sharing

`HIVE_HUB_CUSTOMERS_SHARE_ENABLED` is off by default. Set it to `1`, `true`,
`yes`, or `on` in the hub environment to enable **Profile sharing** in Fleet
and on hive cards where the signed-in person holds a grant. The hub dashboard's
operator settings show the effective state. With the switch off, the page and
its API return `404`; stored choices are not deleted.

People can enter or clear their real name, organization, and country, then
confirm or withdraw their own selections separately for each hive. A real name
is shared only with that consent, and only if the person entered it
themselves. Country and organization are treated as public and are sent,
labeled with their source, to the project hives named in
`HIVE_HUB_CUSTOMERS_PROJECT_HIVES` (by default the project's own hive),
without a consent record or a grant ([#19306](https://github.com/hivecommons/hive/issues/19306)). A
material disclosure change requires reconfirmation: disclosure version 2, which
replaced the version that said nothing was sent yet, suspends every earlier
consent until the person confirms again.

A hive asks for these values only when its owner turns on
`customers.hub_import.enabled` (see
[Customers hub import](telemetry.md#customers-hub-import)). The hive posts
salted hashes of its confirmed customers' GitHub logins to
`POST /api/customers/match` with its per-hive bearer. The hub answers only a
hive listed in `HIVE_HUB_CUSTOMERS_PROJECT_HIVES`: the hub and that hive are
one trust domain. Every other hive gets `403` and no data, and records only
its own interactions. For a listed hive the hub answers people with an
effective consent for that hive (real name) and every hub person it asks
about (labeled public country and organization; the organization may come
from the person, an admin, a listed email domain in
`HIVE_HUB_CUSTOMERS_ORG_DOMAINS`, or the hub's own affiliation lookup); an
unknown or blocked person gets the same empty answer. A withdrawal, a lost grant, a
blocked account or a deleted hub user makes the next answer tell the hive to
clear what it was sent.

What each role sees:

- **The person**, on the Profile sharing page: per hive, what was last sent and
  when, which fields were sent at any time, and when the first delivery was.
- **A hub admin**, in the dashboard's Scale Controls, under **Customers profile
  sharing**: the number of consents per hive, and how many await
  reconfirmation. No person, field choice or value is shown, and no admin route
  can grant, change or read a consent. The same counts are at
  `GET /api/saas/admin/profile-share/counts`.
- **The hive owner and anyone with Customers access on that hive**: the values
  the hive received, shown as "from the hub". They can export them, and the hub
  cannot recall an exported copy.

Audit lines carry names of fields, never values: `audit: customer consent
grant`, `change` and `withdrawal` per consent change, and `audit: customer match`
per request with the hive id and counts.

### Ask AI help

Ask AI help is an optional docs-help backend for the hub dashboard (design:
[Dashboard Ask AI help](design/dashboard-ask-ai-help.md)). On the hub it is
off by default. Hosted hives default to Ask on in hub mode (**Ask Hive** in
their Settings and Help menu), so turning it on here serves every hosted hive
whose owner has not switched it off; `HIVE_ASK_RELAY_DISABLED_HIVES` refuses
single hives and `HIVE_ASK_RELAY_HIVES=none` refuses all of them. With it off,
or switched on but missing a value, the hub registers no
Ask route, reads no key and sends nothing. The hub calls the Kapa.ai Chat API
itself, server side; the browser only ever talks to the hub, and no
third-party script or Content-Security-Policy change is involved.

To wire it once a Kapa API key exists:

1. In the Kapa project, create an HTTP API integration and an API key. Keep
   both out of the repository.
2. Store the key in a Kubernetes Secret and mount it under one of the allowed
   secret dirs (`/secrets` or `/data/secrets`), for example:

   ```sh
   kubectl -n <hub-namespace> create secret generic hive-ask-kapa \
     --from-file=api-key=<path-to-key-file>
   ```

   and add a volume plus `volumeMounts: [{name: hive-ask-kapa, mountPath: /secrets/kapa, readOnly: true}]`
   to the hub Deployment.
3. Set the hub environment:

   ```yaml
   env:
     - name: HIVE_ASK_ENABLED
       value: "true"
     - name: HIVE_ASK_KAPA_PROJECT_ID
       value: <project id>
     - name: HIVE_ASK_KAPA_INTEGRATION_ID
       value: <integration id>
     - name: HIVE_ASK_KAPA_API_KEY_FILE
       value: /secrets/kapa/api-key
   ```

   The project id and integration id may also come from the same Secret
   through `valueFrom.secretKeyRef`. There is no env var that holds the key
   itself.
4. Restart the hub. It logs `ask: enabled` with the effective limits, or one
   `ask: requested but staying off` line with the reason. Neither line, nor any
   API response, carries the key, the project id or the integration id.

What leaves the hub per question: the question text (refused, not sent, when
it looks like a password, token or key), the label of the dashboard section
from a closed list of ids in `pkg/askai/sections.go` (set
`HIVE_ASK_SEND_SECTION_LABEL=false` to drop it), the integration id and, for a
follow-up, the vendor's thread id. No login, email, page content, URL, hive or
repo name is sent. Each person confirms a disclosure before their first
question and can withdraw; the record lives under `/data/saas/ask-consents/`
and is deleted with the user. Answers without an allowlisted source
(`docs.hivecommons.dev` or the GitHub docs path under `hivecommons/hive`) are
shown as "no cited answer" with a docs link.

Limits (env overrides in [Environment variables](env-vars.md)): per person 3 a
minute, 20 an hour and one in flight; hub-wide 4 a minute (a fifth of the
vendor's 20 a minute per-team limit, and never more than half of it), 2 in
flight and 500 a day. A vendor `429` backs every caller off for its
`Retry-After`; three upstream failures in a row open a circuit breaker for five
minutes. Audit lines (`audit: ask request`, `audit: ask consent grant`,
`audit: ask consent withdraw`, `audit: ask breaker opened`, `audit: ask daily
budget spent`) carry outcomes and counts, never text. Hub admins can read the
counters at `GET /api/ask/stats`.

### Ask AI help

Ask AI help is an optional docs-help backend for the hub dashboard (design:
[Dashboard Ask AI help](design/dashboard-ask-ai-help.md)). On the hub it is
off by default. Hosted hives default to Ask on in hub mode (**Ask Hive** in
their Settings and Help menu), so turning it on here serves every hosted hive
whose owner has not switched it off; `HIVE_ASK_RELAY_DISABLED_HIVES` refuses
single hives and `HIVE_ASK_RELAY_HIVES=none` refuses all of them. With it off,
or switched on but missing a value, the hub registers no
Ask route, reads no key and sends nothing. The hub calls the Kapa.ai Chat API
itself, server side; the browser only ever talks to the hub, and no
third-party script or Content-Security-Policy change is involved.

To wire it once a Kapa API key exists:

1. In the Kapa project, create an HTTP API integration and an API key. Keep
   both out of the repository.
2. Store the key in a Kubernetes Secret and mount it under one of the allowed
   secret dirs (`/secrets` or `/data/secrets`), for example:

   ```sh
   kubectl -n <hub-namespace> create secret generic hive-ask-kapa \
     --from-file=api-key=<path-to-key-file>
   ```

   and add a volume plus `volumeMounts: [{name: hive-ask-kapa, mountPath: /secrets/kapa, readOnly: true}]`
   to the hub Deployment.
3. Set the hub environment:

   ```yaml
   env:
     - name: HIVE_ASK_ENABLED
       value: "true"
     - name: HIVE_ASK_KAPA_PROJECT_ID
       value: <project id>
     - name: HIVE_ASK_KAPA_INTEGRATION_ID
       value: <integration id>
     - name: HIVE_ASK_KAPA_API_KEY_FILE
       value: /secrets/kapa/api-key
   ```

   The project id and integration id may also come from the same Secret
   through `valueFrom.secretKeyRef`. There is no env var that holds the key
   itself.
4. Restart the hub. It logs `ask: enabled` with the effective limits, or one
   `ask: requested but staying off` line with the reason. Neither line, nor any
   API response, carries the key, the project id or the integration id.

With it on, the hub dashboard shows an **Ask** button in the header. It opens a
panel docked to the side (a bottom sheet on a phone) that first shows the
disclosure and a single confirm; after that the person can ask, read the answer
with its **Sources** and the "AI generated, may be wrong" label, and withdraw
with **Stop sending my questions**. Every state keeps a plain docs link: no
consent, Kapa.ai unreachable or busy, or no cited answer. The panel is our own
same-origin script and stylesheet (`/ask-panel.js`, `/ask-panel.css`); with Ask
off the hub serves neither and the dashboard document is unchanged. Hub
operators see the effective switch under **Scale controls**.

What leaves the hub per question: the question text (refused, not sent, when
it looks like a password, token or key), the label of the dashboard section
from a closed list of ids in `pkg/askai/sections.go` (set
`HIVE_ASK_SEND_SECTION_LABEL=false` to drop it), the integration id and, for a
follow-up, the vendor's thread id. No login, email, page content, URL, hive or
repo name is sent. Each person confirms a disclosure before their first
question and can withdraw; the record lives under `/data/saas/ask-consents/`
and is deleted with the user. Answers without an allowlisted source
(`docs.hivecommons.dev` or the GitHub docs path under `hivecommons/hive`) are
shown as "no cited answer" with a docs link.

Hosted hives ask through the hub: with Ask on, the hub also serves
`POST /api/hub/ask/relay`, authenticated with each hive's per-hive heartbeat
bearer, so the key stays on the hub. It serves hosted hives only in this round
(`HIVE_ASK_RELAY_HIVES`, default `hosted`; `none` turns the relay off;
`registered` is accepted and treated as `hosted` until self-hosted hives are
approved), refuses hives listed in `HIVE_ASK_RELAY_DISABLED_HIVES`, and charges
each hive at most 60 relayed questions an hour (`HIVE_ASK_LIMIT_HIVE_PER_HOUR`)
inside the hub-wide limits. See [Ask AI help](ask-ai-help.md) for the hive
side.

Limits (env overrides in [Environment variables](env-vars.md)): per person 3 a
minute, 20 an hour and one in flight; hub-wide 4 a minute (a fifth of the
vendor's 20 a minute per-team limit, and never more than half of it), 2 in
flight and 500 a day. A vendor `429` backs every caller off for its
`Retry-After`; three upstream failures in a row open a circuit breaker for five
minutes. Audit lines (`audit: ask request`, `audit: ask consent grant`,
`audit: ask consent withdraw`, `audit: ask breaker opened`, `audit: ask daily
budget spent`) carry outcomes and counts, never text. Hub admins can read the
counters at `GET /api/ask/stats`.

### Reserved `byo-` hive IDs (credential fence)

Hive IDs that start with `byo-` are reserved for hives that their owners run and register themselves ([#18060](https://github.com/hivecommons/hive/issues/18060)). A hub lists such a hive as directory metadata only, so its heartbeat response never carries hub-held credentials or management actions, whatever key the spoke authenticated with. The hub withholds these fields: the GitHub App config (including cluster, secondary and webhook relay keys), the pending gateway, the claimed project config, the cluster ID, the authorized-user list, visibility, upgrade and branch-switch instructions, the upgrade announcement and policy, spoke restarts, agent-restart resets, fleet-breaker instructions, and self-experiment flags. Display-only data such as admin banners still flows. When the fence withholds a field, the hub logs `heartbeat: credential fence withheld fields from self-service hive` with the field names, never the values. Do not give an operator-provisioned hive a `byo-` ID.

### Self-service registration

The fence above is always on. The registration flow that fills the `byo-` namespace is opt-in. With it enabled, a signed-in hub user lists a hive they run on their own infrastructure without an operator issuing anything. The owner-facing steps are in [Register a self-hosted hive on a hub](register-self-hosted-hive.md); this section is for the operator running the hub.

#### Enable it

| Variable | Default | What it controls |
|---|---|---|
| `HIVE_HUB_SELFSERVICE_REGISTRATION` | off | Turns on `/api/hub/selfservice/*` and the **Register your own hive** form on the dashboard. Accepts `1/true/yes/on`. Read per request, so no restart is needed. While off, every route except `GET /api/hub/selfservice/status` answers `404`, and the dashboard button sends users to the self-hosting guide instead of the form. |
| `HIVE_HUB_SELFSERVICE_MAX_PER_USER` | `5` | How many `byo-` hives one GitHub login may hold at once. The next registration answers `429` until the user removes one. |
| `HIVE_HUB_SELFSERVICE_MAX_REPOS` | `20` | How many repos one registration may list ([#18642](https://github.com/hivecommons/hive/issues/18642)). Each repo is one read-only GitHub ownership check, so values above `500` are clamped to `500`. `POST /api/hub/selfservice/hives` answers `400` over the cap; `POST /api/hub/v1/registrations` answers `413` with `limit: max_repos`. Advertised to clients by `GET /api/hub/v1` as `limits.max_repos`. Read per request. |
| `HIVE_HUB_PROOF_GITHUB_USER_TOKEN` | off | Offers the `github-user-token` ownership proof on `POST /api/hub/v1/registrations` and `PATCH /api/hub/v1/registrations/{id}` ([#19238](https://github.com/hivecommons/hive/issues/19238)). See [Proving ownership with your own token](#proving-ownership-with-your-own-token). Accepts `1/true/yes/on`. Read per request. Only meaningful while `HIVE_HUB_SELFSERVICE_REGISTRATION` is on. |
| `HIVE_HUB_SELFSERVICE_KEY_GRACE` | off | Key rotation with a grace window ([#19235](https://github.com/hivecommons/hive/issues/19235)); see [Rotation](#rotation). Accepts `1/true/yes/on`. Read per request. |
| `HIVE_HUB_SELFSERVICE_KEY_GRACE_WINDOW` | `1h` | How long the old key keeps working after a rotation while `HIVE_HUB_SELFSERVICE_KEY_GRACE` is on. Clamped to `168h`. Read per request. |
| `HIVE_HUB_SELFSERVICE_EXPIRY` | `24h` | How long a registered hive may go without an accepted heartbeat before the sweep removes it. Counted from the last accepted heartbeat, or from registration while there has been none. |
| `HIVE_HUB_SELFSERVICE_SWEEP_INTERVAL` | `10m` | How often the expiry sweep runs. Read at startup; restart after changing. |

All of them are listed with the other hub variables in [env-vars.md](env-vars.md).

#### What a registration does

1. The user is signed in through the hub's GitHub OAuth, so the hub holds their encrypted GitHub token. Without a stored token or a GitHub login the request is refused.
2. The user supplies a short name, an org, up to 20 `owner/name` repos and an `https` dashboard URL. The hub builds the ID as `byo-<github login>--<name>`; the double hyphen keeps IDs unambiguous, since logins may contain `-` but never `--`, and a name may not start with `-`. Nothing the user sends can produce an ID outside the `byo-` prefix, and a name that would borrow another namespace (such as `hosted-`) is refused.
3. For every repo the hub calls GitHub with the user's own token and requires `permissions.admin`. `maintain` and `write` are not enough. One failing repo refuses the whole request with `403` naming that repo, and nothing is stored.
4. The hub checks the per-user cap and that the ID is not already taken, in the registry or by a hub-provisioned hive.
5. The hub mints a random heartbeat key, stores only its hash in `/data/hub-selfservice.json`, and returns the raw key in the `201` response together with Compose, Podman and Kubernetes snippets. The raw key appears nowhere else: not in the list route, not in a log line, not on disk.

The registry entry becomes a `self-hosted` hive in Active Hives and the fleet views once a heartbeat is accepted. The type is assigned by the hub; the spoke cannot claim it.

#### What a `byo-` key can and cannot do

- It authenticates heartbeats for exactly one hive ID, the one it was minted for. Presenting it with any other ID fails.
- A `byo-` ID is verified only against its stored hash. The master-derived paths are never tried for a `byo-` ID, so `HIVE_HUB_SECRET` and keys derived from it never authenticate a self-service hive, and a self-service key never authenticates a `hosted-` or operator-provisioned ID.
- The owner never receives `HIVE_HUB_SECRET`. The hub never consults it in the self-service routes and never returns it.
- Everything the fence withholds still applies: the hub sends the hive no GitHub App config, gateway, cluster ID, authorized users, upgrade or branch-switch instructions, restarts or fleet-breaker actions. Listing is directory metadata only.

#### Editing a registration

`PATCH /api/hub/v1/registrations/{id}` changes `repos` and `dashboard_url` in place, so adding a repo no longer means deleting the registration and registering again (which revoked the key and took the spoke offline). The heartbeat key is not touched: the spoke keeps heartbeating with the key it has.

- **Who:** the owner, signed in to the hub. A registration key cannot edit its own record, and a hub admin who does not own the hive gets `404`.
- **`If-Match` is required.** `GET /api/hub/v1/registrations/{id}` returns the `ETag`; send it back on the `PATCH`. A missing header is `428`, a stale one is `412` with the current `ETag`, so two operators editing at once cannot silently overwrite each other. Heartbeats do not change the `ETag`.
- **`repos`:** a full list replaces the repos, or `{"add": [...], "remove": [...]}` changes them. Only the repos an edit adds are checked against GitHub (the same read-only `permissions.admin` check as registration, with the owner's own stored token); one failing repo refuses the whole edit and nothing is applied. Kept repos keep their proof time, and removed repos lose theirs. At least one repo must remain, and `HIVE_HUB_SELFSERVICE_MAX_REPOS` applies (`413 {limit: "max_repos"}`).
- **`dashboard_url`:** an `https` URL to a public address, or `""` to clear it.
- **Not editable:** `org`, the owner, the ID and the keys. To change the `org`, delete the registration and register it again.
- **No GitHub writes.** The edit changes the hub's own registry only.

The route is part of the v1 registration protocol and follows the same switch as the rest of it, `HIVE_HUB_SELFSERVICE_REGISTRATION`; discovery (`GET /api/hub/v1`) lists it under `operations` with `if_match: "required"` and its fields under `patch_fields`, so a hub that does not offer it does not advertise it. The v6 `/api/hub/selfservice/*` routes are unchanged.

#### Proving ownership with your own token

The hub signs users in with no GitHub scope, so when an org restricts OAuth apps GitHub hides the admin role from the stored token and registration says `not an admin` for repos the user does administer. With `HIVE_HUB_PROOF_GITHUB_USER_TOKEN` on, a v1 create or `PATCH` can prove ownership with a token sent on that request only:

```
X-Hive-Proof-Method: github-user-token
X-Hive-Proof-Token: <a GitHub token>
```

- **Scopes:** none for public repos; the classic `repo` scope for private repos. A fine-grained token needs read access (Metadata) to every repo in the request. Discovery lists this under `proof_method_details`.
- **Whose token:** the token must belong to the signed-in GitHub account (`GET /user`); a token made by another account is refused.
- **Never kept:** the token is used for the read-only `GET /user` and `GET /repos/{owner}/{repo}` calls and then dropped. It is not stored, logged or echoed, and the stored sign-in token stays scopeless.
- **Clear refusals:** a failed proof answers `403 {error, proof_method, reason, repo, required_scopes, granted_scopes}`, where `reason` is one of `token_rejected`, `token_user_mismatch`, `sso_not_authorized`, `missing_scope`, `repo_not_visible`, `admin_role_not_visible` (GitHub showed the repo but not the role) or `not_admin`. `granted_scopes` appears only when GitHub reported the token's scopes.
- **Off means off:** while the flag is off, asking for the method is `400` listing the methods the hub offers; it never falls back to the stored token. Without `X-Hive-Proof-Method` the hub proves with the stored token as before (`hub-session-token`). The v6 `/api/hub/selfservice/*` routes ignore both headers.

#### Heartbeat feature negotiation

A spoke may send `protocol: {"version": 1, "features": [...]}` in its heartbeat ([#19237](https://github.com/hivecommons/hive/issues/19237)). With `HIVE_HUB_HEARTBEAT_PROTOCOL` on (`1/true/yes/on`, default off, read per request), the hub answers with `protocol: {"version", "accepted", "ignored"}`: the version it speaks for this beat, the requested features it honours, and the requested features it does not. The features a v1 hub accepts are `activity_digest`, `activity_events` and `host_aliases`, all of which it already serves to every spoke; negotiation changes nothing else in the response.

- **No offer, or the flag off:** the response is exactly what it was before, with no `protocol` field.
- **Unknown version** (below `1`): the beat is still accepted, every requested feature is listed under `ignored`, and `unsupported_version` echoes what the spoke sent. A version above the hub's is answered with the hub's version, so a newer spoke can fall back to it.
- **Bounds:** at most 32 features are evaluated per beat. Names must be lowercase letters, digits, `_`, `-` or `.`, start with a letter and be at most 64 bytes; anything else is not echoed back.

#### Rotation

The owner can rotate from the **Self-hosted hives** list or with `POST /api/hub/selfservice/hives/{id}/rotate`. The hub mints a new key and keeps the previous hash live for a 10 minute overlap so a running spoke can pick up the new value without a gap. After the overlap only the new key is accepted. Only the owner can rotate; a hub admin who does not own the hive gets `404`, the same answer as for a missing hive.

With `HIVE_HUB_SELFSERVICE_KEY_GRACE` on ([#19235](https://github.com/hivecommons/hive/issues/19235)), the overlap is the configurable grace window `HIVE_HUB_SELFSERVICE_KEY_GRACE_WINDOW` (default `1h`) instead of 10 minutes, on both the v6 route and `POST /api/hub/v1/registrations/{id}/keys`. Rotating again while a window is open keeps the old key, the one the spoke is still running, and replaces the pending key: the key from the earlier rotation stops working at once, and the window restarts. So rotating twice by mistake never cuts off the running spoke. The `overlap` in the response and `limits.key_rotation_overlap` in discovery report the window in effect. Existing registrations need no change.

#### Expiry sweep

A background sweep runs every `HIVE_HUB_SELFSERVICE_SWEEP_INTERVAL` and removes any `byo-` hive whose last accepted heartbeat is older than `HIVE_HUB_SELFSERVICE_EXPIRY`, or that never heartbeated within that window after registration. The key is revoked and the registry entry is removed in the same pass. The owner registers again when the hive is back; there is no grace list.

#### Revoking as admin

A hub admin removes any self-service hive with `DELETE /api/hub/selfservice/hives/{id}` or from the dashboard. The key stops authenticating immediately and the listing disappears. The owner's own **Remove** button does the same for their hives. The audit line records who did it and whether it was the owner or an admin. If you need to stop all registrations at once, unset `HIVE_HUB_SELFSERVICE_REGISTRATION`; existing entries keep heartbeating until they expire or are removed.

#### Audit events

Every decision writes one structured log line, documented in [audit-log.md](audit-log.md):

| Event | When |
|---|---|
| `selfservice_hive_registered` | A registration passed every check and a key was minted. |
| `selfservice_hive_refused` | A registration was turned away; `reason` is one of `invalid_name`, `reserved_prefix`, `duplicate_id`, `invalid_repos`, `invalid_dashboard_url`, `invalid_update` (a `PATCH` body that was refused), `per_user_cap`, `no_github_token`, `no_github_login`, `not_repo_admin`, `ownership_check_error`. |
| `selfservice_hive_updated` | The owner edited the repos or dashboard URL with `PATCH`; the line counts repos added and removed and carries no key. |
| `selfservice_hive_rotated` | The owner rotated the key; `overlap` is how long the old key keeps working, `grace` whether the grace window is on, `replaced_pending` whether a pending key inside an open window was replaced, `old_key_expires_at` when the old key stops working. No key or hash. |
| `selfservice_hive_revoked` | The owner or an admin removed the hive; `by_admin` tells which. |
| `selfservice_hive_expired` | The sweep removed a hive; `last_heartbeat` is RFC 3339 or `never`. |

No audit line ever carries a key or a hash.

## Hosted spoke provisioning model

The hub's SaaS provisioner creates or records:

1. Namespace, RBAC, Secret, ConfigMap, Service, route/ingress, Deployment, and an RWX `hive-data` PVC.
2. A mounted `/data` volume where the spoke writes runtime config and state.
3. `HIVE_ID`, `POD_NAMESPACE`, `HIVE_LEVEL`, `HIVE_HUB_URL`, `HIVE_HUB_SECRET`, optional `HIVE_AUTHORIZED_USERS`, and inference endpoint env vars in the spoke Deployment.
4. `/data/saas/hives/<id>/meta.json` on the hub PVC. Heartbeats can be accepted without this record, but hub-driven upgrades and dashboard links need it.

For push-reported clusters where the hub cannot run `kubectl`, follow the manifest-level workflow in `manual-provisioning.md`; this guide intentionally keeps the hub setup consistent with that battle-tested path.

### Push-reported cluster entries

`/data/saas/clusters.json` also accepts per-cluster network overrides for hives it provisions: `upstream_proxy` (`{url, hosts[], no_proxy[]}`, a CONNECT forward proxy, injected as `HIVE_UPSTREAM_PROXY_*` env) and `host_aliases` (`[{ip, hostnames[]}]`, rendered as Pod `hostAliases` for a GHE reachable only via a TLS reverse proxy on a fixed IP; existing hives are converged by a rate-limited reconcile sweep and, on every cluster including push-reported ones, by each spoke from its heartbeat response). See [network-requirements.md](network-requirements.md).

`/data/saas/clusters.json` keeps the historical compatibility key `pull_only`. Set `pull_only: true` when the hub cannot reach that cluster's Kubernetes API and the spokes must dial out to the hub instead. Operator-facing surfaces call these clusters **push-reported** because their node stats, dashboard token proofs, and liveness arrive in spoke heartbeats pushed from the cluster.

A push-reported entry still needs its stable identity (for example `id`, `name`, `domain`, app/forge fields), but it does not need a usable `kubeconfig_path` for hub-side reads. If node stats are absent, upgrade the spokes to a build that sends heartbeat cluster health and then check the error reason shown in the Clusters panel.

Each spoke must also know its hub registry cluster ID. Hosted provisioning writes
both `hub.cluster_id` and `HIVE_CLUSTER_ID`; manual or older hosted manifests
must set one of them to the cluster entry ID (for example `vllm-d`). Without
that identity, the heartbeat still proves liveness but deliberately omits
`cluster_health`, because the spoke cannot know which cluster row it is
authoritative for.

Heartbeat node health needs read-only cluster-scoped RBAC for:

- core `nodes` `get,list` (capacity, allocatable, readiness, GPU labels)
- `metrics.k8s.io` `nodes` `list` (live CPU/memory usage from metrics-server)

Do not grant `nodes/proxy` to a tenant ServiceAccount: it reaches every kubelet's `/exec`, `/attach` and `/pods` endpoints through the API server. The spoke's kubelet `stats/summary` disk-usage read is best-effort; without it the Node card shows disk capacity but no live usage percentage.

When `metrics.k8s.io` is unavailable or forbidden, spokes still report node count, vCPU, memory and disk capacity from the core Node API. The hub marks only the live-usage data partial and carries the precise `node_health_error` reason, such as `metrics API failed: ... HTTP 403` or a missing metrics API, instead of the generic "check RBAC and metrics-server" note.

Tenant roles deliberately omit `nodes/proxy` (which permits cross-tenant kubelet exec) and cluster-wide `pods list` (which exposes other tenants' pod specs). Push-reported tenant health therefore has no live disk percentage, pod/hive counts or request-based remaining hive capacity; the collector reports partial health (`pods API failed: ... HTTP 403`) while retaining node capacity and available CPU/memory metrics. Do not restore those grants to suppress the partial-health warning.

On hub startup and every 15 minutes, the SHA poller narrows existing registered tenants' `hive-node-health-reader-*` roles on kubectl-reachable clusters. It does not restart pods, create missing roles, or touch shared operator roles. Failed reads/patches are logged and retried. For push-reported clusters the hub cannot perform this migration: a cluster administrator must replace each existing tenant role's rules with the safe node/NodeMetrics rules in [Manual provisioning](manual-provisioning.md). Apply the same correction to orphaned roles no longer represented in the hub registry. Deploy the hub fix as well as the provisioning template fix; upgrading tenant pods alone does not revoke previously issued RBAC grants.

## API surface to know

The hub registers heartbeat, registry, SaaS, contributor, OAuth, webhook, backup, and callback-related routes from the Go hub packages. Operators normally interact through the dashboard, but the important external contracts are:

- `POST /api/heartbeat` — spoke heartbeat, bearer-authenticated by `HIVE_HUB_SECRET` when configured.
- `POST /api/saas/hives` — hub-admin SaaS hive creation path.
- `/api/saas/auth-check` — ingress auth check used by hub-proxied spoke dashboards.
- `/contribute` and `/api/contribute/*` — contributor relay registration and WebSocket flow.
- `/api/registry` and leaderboard/fleet views — public or authenticated hub registry views.

See `api-reference.md` and the dashboard OpenAPI file for route details, and `manual-provisioning.md` for known failure modes.

### Search engine indexing

Only the hub's public landing pages (`/`, `/learn`, `/get-started`, `/reading`, `/contribute`) are meant to be indexed; everything else is an API, an auth gate, or an operator console, and a crawler walking it just collects 401/403s (the "Blocked due to unauthorized request" errors in Google Search Console). The hub enforces that in three layers, all in `src/pkg/hub/robots.go`:

- `GET /robots.txt` is generated per request host. On the configured public host (`HIVE_HUB_PUBLIC_URL`, default `hive.hivecommons.dev`, plus `localhost`) it allows `/` and emits one `Disallow:` line per gated route group (`/api/`, `/metrics`, `/openapi.json`, `/login`, `/logout`, `/auth/`, `/oauth`, `/openrouter/`, `/dashboard`, `/access-denied`, `/error/`, `/fleet`, `/my-hives`, `/gh-setup`, `/ws`, `/static/`) followed by the `Sitemap:` pointer; `sitemap.xml` lists public pages only. On any other host — a hosted tenant, an internal Service name, a preview hostname — it answers `User-agent: *` / `Disallow: /`. A CI test derives the auth-gated routes from the mux registrations and fails if one is not covered by a `Disallow` line.
- Every response under a disallowed group, every 401 or 403, and every redirect into `/login` carries `X-Robots-Tag: noindex, nofollow`; the login pages also carry `<meta name="robots" content="noindex, nofollow">`.
- Hosted spoke dashboards (`<id>.hive.hivecommons.dev`) are never indexable: the hub's ingress auth-check lets `/robots.txt` through unauthenticated and the spoke serves `Disallow: /` for it, and every spoke response — including the 401 login page — carries `X-Robots-Tag: noindex, nofollow` (`src/pkg/dashboard/robots.go`).

Verify from outside with `curl -A Googlebot https://hive.hivecommons.dev/robots.txt` and `curl -sI https://hive.hivecommons.dev/api/hub/stats | grep -i x-robots`.

## Running several spokes under one hub

Splitting one large hive into several subject-scoped spokes stops agents competing over unrelated work. It also surfaces things that are fleet-wide in reality but per-hive in the implementation — provider quota, the shared GitHub App rate limit, agent credentials, and repo ownership ([#5691](https://github.com/hivecommons/hive/issues/5691) is the RFC tracking those).

The first of those the hub now checks for you is **repo ownership**.

### Repo-ownership overlap

Two spokes managing the same repo puts two fleets on one backlog, opening competing PRs on it — the exact thing a split exists to prevent. Nothing stopped that, even though the hub receives every spoke's repo list on every heartbeat.

The hub now compares those lists and reports any repo claimed by more than one spoke:

- `GET /api/registry` gains a `repoOverlaps` array. It is **omitted entirely** when the fleet is clean, so its presence is the signal:

  ```json
  {
    "hives": [ ... ],
    "repoOverlaps": [
      {
        "host": "github.com",
        "repo": "tuna-os/docs",
        "hives": [
          { "id": "h-hive", "name": "hive" },
          { "id": "h-reef", "name": "reef" }
        ]
      }
    ]
  }
  ```

- A **warning** fleet alert (`repo-overlap`) is raised on *both* hives in the pair, naming the other spoke and the shared repos. Either operator can act, and alerting on only one would read as that hive being at fault rather than the pair being ambiguous.

Matching is deliberate about what counts as the same repo, because two spokes are configured separately and so are the pair most likely to spell one repo differently:

| Compared | Rule |
|---|---|
| `docs`, `tuna-os/docs`, `https://github.com/tuna-os/docs.git` | the same repo — bare, owner-qualified and URL forms all resolve to `owner/repo` |
| `Tuna-OS/Docs` vs `tuna-os/docs` | the same repo — GitHub owner/repo names are case-insensitive |
| `acme/ui` on `github.com` vs on `github.ibm.com` | **different** repos — the GitHub host is part of the identity |
| a spoke listing its own primary repo in `repos[]` | one claim, not a conflict with itself |
| a repo with no owner (no `org` recorded) | ignored — inventing an owner would invent conflicts between unrelated repos that share a name |

This is **read-side only**: it is derived per request from data the registry already holds, adds nothing to the heartbeat protocol, and is not persisted. It reports and does not refuse — an overlap is a warning, not a bar. A repo two subject areas legitimately share (a docs repo is the common case) is real, so the alert is acknowledgeable like any other; whether overlap should ever *block* assignment, and how such a repo would be allowlisted, is still open on the RFC.

## Verifying the hub Deployment

The hub's own container is named `hub` and it is the only container in the Deployment. Always scope container-level kubectl commands with `-c hub` (for example `kubectl -n hive-hub logs deploy/hive-hub -c hub`); `kubectl set env` without `-c hub` writes to every container. `HIVE_HUB_OAUTH_CLIENT_SECRET` and `HIVE_NPS_RELAY_PULL_SECRET` must come from a Secret via `valueFrom.secretKeyRef`, never a literal `value:`. Check a live or rendered Deployment with:

```sh
kubectl -n hive-hub get deploy hive-hub -o yaml | python3 src/deploy/check-hub-manifest.py -
```

Cluster-side cleanup that cannot be done from the repo: remove any stray `hive-hub` container from the running Deployment, move the two secrets into a Secret, and rotate both values. The check also rejects a secret env fed from a ConfigMap or field and looks at initContainers.

The hub also checks itself: before each self-upgrade `set image`, it reads its own Deployment's container names and logs `hub deployment has unexpected containers` (with `unexpected=<names>`) when anything besides `hub` is present. The warning never blocks the upgrade.
