# Email to Inbox: a per-hive inbound address

**Status: design only / proposed.** No code in this PR. The owner's ask: "enable
email to hive's inbox; we could use a special UID for the email address for each
hive." This page records what the Inbox already is, where a per-hive address
would be minted, how mail would reach a spoke, and the guard set it must ship
with. Read [`inbox-voice-sources.md`](../inbox-voice-sources.md) first for the
shipped Inbox; read [`escalation-surfaces.md`](escalation-surfaces.md) section
"Inbound reply-to-act" for the earlier decision that inbound email is the
weakest-authenticity surface Hive will have and must default off.

## 1. Goals and non-goals

Goals:

- Every hive gets one unique, unguessable inbound address. Mail sent there lands
  as an Inbox item on that hive, keyed `(source=email, id=<Message-ID>)`, and is
  triaged exactly like a Plaud recording today: route to a spek RFC, to dibs, or
  "Knowledge only".
- Works for hosted hives without any per-hive mail infrastructure, and has a
  path for self-hosted spokes with no hub.
- Default off. Nothing listens, no address is shown, until the owner enables it.

Non-goals (this round):

- Sending mail from the hive beyond an optional one-line acknowledgement.
- Reply-to-act verbs (`approve`, `kick ...`), which stay with the escalation
  design. An email never kicks an agent directly; it only becomes an Inbox item.
- Reading an existing mailbox (IMAP/Gmail) as the primary path; see section 3b.
- Mail as an agent conversation channel.

User stories:

1. Forward an email thread ("Fwd: pricing API edge cases") to my hive; later
   that morning file it as an RFC from the Inbox with two clicks.
2. A vendor emails a PDF spec to `<uid>+rfc@inbox.hivecommons.dev`; the subject
   rule files it as a spek RFC automatically, with the PDF text appended.
3. A phone voice-memo app emails its transcript; it lands next to the Plaud
   recordings and goes to dibs as an idea.

## 2. What exists today

The Inbox is source-agnostic by construction:

- `inbox.Source` is `Type() / List() / Fetch()` (`src/pkg/inbox/inbox.go:98-102`);
  `inbox.Item` is the list row and `inbox.Content` the full body with a
  `Provenance` string such as `plaud://recordings/<id>`
  (`src/pkg/inbox/inbox.go:26-49`, `src/pkg/inbox/inbox.go:70-79`).
- `Service.Items` fans out over `inbox.sources` and overlays persisted state
  (`src/pkg/inbox/service.go:125-145`); `Service.Route` queues a delivery
  (`src/pkg/inbox/service.go:185-215`); `Service.Sweep` applies `inbox.rules`
  once per never-seen item (`src/pkg/inbox/service.go:259-279`), driven by
  `inboxAfterSync` after each voice connector sync
  (`src/cmd/hive/boot_inbox.go:111-129`).
- Rules match `source`, `title_prefix` or `tag` (`src/pkg/inbox/rules.go:19-39`);
  the default prefixes are `RFC:` and `Hive:` (`src/pkg/inbox/rules.go:10`).
- Destinations: `SpekDestination` kicks an agent with the enveloped item text
  plus provenance; the agent drafts the spec with spektacular and runs
  `spektacular publish`
  (`src/pkg/inbox/spek.go:52-102`); `DibsDestination` posts a private draft idea
  (`src/pkg/inbox/dibs.go:44-87`).
- State lives in `connector-state/_inbox.json` (`src/cmd/hive/boot_inbox.go:46`,
  `src/pkg/inbox/store.go:19-27`).
- API: `GET /api/inbox`, `POST /api/inbox/route|dismiss|settings`
  (`src/pkg/dashboard/api_inbox.go:24-29`); mutations are owner-only via
  `requireOwnerRole` (`src/pkg/dashboard/api_agent_control.go:519`).
- Config: `InboxConfig{Sources, Rules, Spek, Dibs}` with `token_env` for the
  only secret (`src/pkg/config/inbox_config.go:24-33`,
  `src/pkg/config/inbox_config.go:82-87`).

Does an Inbox source have to be a knowledge connector? No. Two source shapes
exist: `FactSource` reads any connector's facts whose front matter carries
`source`/`source_id` (`src/pkg/inbox/factsource.go:21-26`), and `PlaudSource`
reads the vendor directly, with `PreferDirect` merging the two
(`src/pkg/inbox/plaudsource.go:95-105`). The connector framework
(`src/pkg/knowledge/connector/connector.go:47-58`) is a pull model: `Sync`
emits pages on an interval. Email is push. So the email source is a native
`inbox.Source` over its own on-disk spool, and it optionally writes a Knowledge
fact per message through the same `FactWriter` slug scheme
(`src/pkg/knowledge/connector/factwriter.go:34`) so the Inbox's *open in Knowledge* link keeps working.
`SetSources` already lets a source appear without restart
(`src/pkg/inbox/service.go:70-78`).

Hosted identity: the hub mints `generateHiveID` as
`hosted-<org>-<repo>-<4 chars>` (`src/pkg/hub/saas_provision.go:2020-2031`),
stores the record in `SaaSHive` with its `WebhookRelayKey`
(`src/pkg/hub/saas_provision.go:566-570`, `src/pkg/hub/saas_provision.go:800-807`)
under `saasHivesDir/<id>/meta.json` mode 0600
(`src/pkg/hub/saas_provision.go:2088-2093`), and fronts each spoke with an
nginx `auth-check?hive=<id>` subrequest
(`src/pkg/hub/contribute_ingress_reconcile.go:98-105`). Hub users are GitHub
usernames, not email addresses (`src/pkg/hub/saas.go:140-144`); the spoke's own
allowlist is `dashboard.authorized_users`, also usernames
(`src/pkg/config/config.go:5317`).

Inbound HTTP precedent: `POST /api/webhook/github` fails closed without a
secret, verifies `X-Hub-Signature-256` or, on hosted spokes, the hub's
`X-Hive-Relay-Signature-256` over the raw body
(`src/pkg/dashboard/api_github_webhook.go:50-95`,
`src/pkg/dashboard/webhook_relay.go:28`, `src/pkg/dashboard/webhook_relay.go:108`).
The hub relays byte-for-byte to the managing spoke's `DashboardURL`
(`src/pkg/hub/webhook.go:138`), the per-hive relay key is 32 random bytes
(`src/pkg/hub/webhook_relay_key.go:39-45`) delivered over the heartbeat
response until the spoke reports holding it (`src/pkg/hub/server.go:3394-3402`).
Secrets are generated into files and shown once
(`src/pkg/dashboard/api_github_webhook_setup.go:217`), never kept in config.

## 3. Addressing

**The uid is not the hive id.** `hosted-<org>-<repo>-xxxx` has 4 chars of
entropy and is printed in URLs; an address built from it is guessable. Mint a
separate **inbox uid**: 20 random bytes, base32 lower-case without padding
(32 chars, 160 bits), from `crypto/rand` like `newWebhookRelayKey`. Address
form: `<uid>@<domain>`, e.g. `k7f3...@inbox.hivecommons.dev`.

Where it is minted and stored:

- Hosted: the hub mints it on first enable and stores it as
  `SaaSHive.InboxUID` next to `WebhookRelayKey` in `meta.json`. The uid must be
  findable by the hub (it is the routing key for inbound mail), so the hub is
  authoritative. The spoke receives it in the heartbeat response the same way
  the relay key travels, and keeps it in `connector-state/_inbox_email.json`
  for display only.
- Self-hosted without a hub: the spoke mints the uid itself when the owner
  enables `inbox.email`; the domain is whatever the operator points at it.
- The uid is a routing identifier, not a bearer secret, but it is treated as
  sensitive: masked by default in the dashboard with a copy button, redacted
  from logs and config export (`configExportSecretKeyRE` already redacts
  `token|secret|...`; add `inbox_uid`).

Display: the Inbox header gains an "✉ Email address" chip next to the existing
"⚙ Auto-routing" button (`src/pkg/dashboard/static/index.html:46993`), and the
Knowledge → Integrations card for `email` shows state plus "📥 Open Inbox" like
Plaud does (`src/pkg/dashboard/static/index.html:47223`).

Rotation and revocation: `POST /api/inbox/email/rotate` (owner) mints a new uid
and keeps the previous one accepting for a 24 h overlap, mirroring the heartbeat
key rotation overlap on the hub. Disabling `inbox.email` revokes immediately;
mail to a revoked uid is dropped and counted, never bounced (no oracle).

Plus-addressing: `<uid>+<hint>@domain`. The hint becomes a tag on the item
(`Tags: ["rfc"]`), so the existing `tag` rule matcher routes it with no new rule
syntax. Only `[a-z0-9-]{1,32}` hints are accepted; anything else is ignored, not
rejected.

## 4. Delivery options

### (a) Hub-owned inbound domain, provider webhook, hub relays to the spoke (recommended)

Hub operator points MX for `inbox.<hub domain>` at a provider with an inbound
parse webhook (Postmark Inbound, SendGrid Inbound Parse, Mailgun Routes) or at
Cloudflare Email Routing with a Worker that POSTs the raw message. The provider
posts JSON (parsed headers, text/html bodies, attachments, and its own
SPF/DKIM/DMARC verdicts) to `POST /api/hub/inbox/email` on the hub, signed with
the provider's webhook secret (env `HIVE_INBOX_EMAIL_WEBHOOK_SECRET`, or basic
auth in the URL for providers that only support that).

The hub: verifies the provider signature, extracts the uid from the
`To`/`Delivered-To` local part, looks up the hive, drops unknown uids silently,
normalises the message into the item shape in section 6, caps it, and relays it
to `<spoke DashboardURL>/api/inbox/email` signed with the hive's existing relay
key and timestamp headers, exactly like `relayWebhookToSpoke`. The spoke
verifies with `verifyWebhookRelay`, appends to its spool, runs `Sweep`.

Push-reported spokes with no `DashboardURL` (the hub skips them for webhooks
today) get a pull path: the hub keeps up to N pending messages per hive and
the spoke's heartbeat response carries `inbox_email_pending: true`; the spoke
then `GET`s them with its heartbeat bearer. This is the same shape as config
delivery over the heartbeat and needs no inbound port on the spoke.

Trade-offs: one domain and one provider account for the whole fleet; the owner
does nothing but copy an address. The hub sees mail in transit (it already sees
webhook payloads) and must not retain bodies beyond the pending spool. Depends
on a provider; Cloudflare Email Routing is free and needs only a small Worker.

### (b) Per-hive IMAP/Gmail polling (connector style)

A `type: email-imap` knowledge connector polls a mailbox the owner provides,
like Plaud polls Plaud. Fits the connector framework and push-reported spokes,
and is the only option for a hub-less spoke that cannot receive HTTP from the
internet. Costs: the owner must create and secure a mailbox and app password,
the address is theirs (no uid), OAuth for Gmail/Outlook on a hosted hive has
the same redirect problem Plaud has
([`inbox-voice-sources.md`](../inbox-voice-sources.md) "Hosted hives"), and
polling latency is minutes.

### (c) Both

Recommendation: ship (a) first; design the source so the spool is the seam, and
add (b) later as a second writer into the same spool only if a self-hosted
operator asks. A hub-less spoke can also use (a) directly by pointing its own
Cloudflare Email Routing Worker at `<spoke>/api/inbox/email` with a shared
secret from `inbox.email.webhook_secret_env`.

## 5. Security

- **Fail closed.** `inbox.email.enabled: false` default; with no secret
  configured the spoke endpoint answers 503 like the GitHub webhook does.
- **Sender verification.** The hub trusts only the provider's authentication
  results: accept when DKIM passes with the `From` domain aligned, or SPF passes
  and DMARC is `pass`; otherwise drop and audit. This is the rule
  `escalation-surfaces.md` already set (`src/docs/design/escalation-surfaces.md:116-119`).
- **Sender allowlist.** Empty allowlist means nothing is accepted. The default
  set is the verified emails of the hive's authorized users (GitHub
  `user/emails` needs the `user:email` scope; otherwise owner-entered), plus
  `inbox.email.allowed_senders`. Allowlist is matched on the authenticated
  `From`, never `Reply-To`. Open question 1.
- **Unguessable uid.** 160 bits; unknown uid is dropped with no response body
  difference from success at the provider (always 200 to the provider, so the
  provider does not retry or bounce).
- **Caps.** 1 MiB body text, 10 MiB total per message, 10 attachments, 60
  messages per hive per hour and 10 per sender per hour, 200 pending per hive on
  the hub. Over-cap mail is dropped and audited.
- **Attachments.** Accept `text/*`, `application/pdf`, `.md`, `.csv`, `.json`,
  images; reject archives and executables. Stored under
  `connector-state/email/<message-id-hash>/` on the spoke, never on the hub past
  relay. PDF and text are extracted to markdown and appended to the body under
  `## Attachments`; images are kept but not inlined into prompts. No scanner
  dependency in phase 1; type and size are the only gate.
- **Prompt-injection posture.** The body is untrusted data, same as a Plaud
  transcript: it reaches a prompt only through `Content.Text()` into the spek
  stdin or the agent kick prompt (`src/pkg/inbox/spek.go:76`), fenced in the
  nonce-tagged `<untrusted-inbox-data>` envelope (`src/pkg/inbox/envelope.go:40`), and the spoke
  passes it through `ioscan.EnforceInput` (`src/pkg/ioscan/enforce.go:24`)
  before `Fetch` returns. Headers are never trusted for anything but routing;
  HTML is converted to markdown with scripts and tracking pixels stripped.
- **Audit.** `inbox_email_accepted`, `inbox_email_dropped` (reason: auth,
  allowlist, cap, unknown_uid, type), `inbox_email_uid_rotated`, through
  `AuditLog.LogRecord` (`src/pkg/dashboard/audit.go:248`) with sender domain
  and message-id hash, never the body.
- **No secrets in config.** `webhook_secret_env` and the relay key only; the uid
  is stored by the hub, not in YAML.
- **Abuse and spam.** The provider's spam score is honoured (drop above the
  provider's default threshold). Auto-replies and bulk mail (`Auto-Submitted`,
  `Precedence: bulk`, `List-Id`) are dropped. Bounces (`mailer-daemon`, DSN
  content type) are dropped. No unsubscribe handling because the hive sends
  nothing unsolicited.

## 6. Data model

Item (list row): `Source: "email"`, `Connector: "email"`, `ID: <Message-ID>`
(angle brackets stripped; falls back to SHA-256 of raw headers when absent),
`Title: <Subject>` with `Re:`/`Fwd:` prefixes preserved so `RFC:` rules still
see their prefix, `Excerpt` from the first 240 runes of text, `RecordedAt:
Date` header, `Tags` from the plus-hint plus `attachments` when any.

Content (Fetch): `Title: Subject`, `Summary` empty, `Transcript` = text body
(markdown from HTML when no text part), `Markdown` = full rendered message
(`# Subject`, `From`, `Date`, body, `## Attachments`), `Provenance:
mailto:<From>?message-id=<id>`. Dedupe: the spool is keyed by Message-ID; a
duplicate delivery (provider retry, both relay and pull) is a no-op. Threading:
`In-Reply-To`/`References` are stored on the spool record; the list shows a
"↩ in thread" marker and `Fetch` of a reply prepends a one-line link to the
parent item when it is in the spool. No thread collapsing in phase 1.

Spool: `connector-state/email/<message-id-hash>.json` with headers, bodies,
attachment list, provider verdicts and a `received_at`; route state continues
to live in `_inbox.json` under key `email/<id>`. Retention: 90 days or the
owner's dismiss, same as nothing today prunes Plaud items.

## 7. Routing

Email items feed the existing destinations unchanged: `spek_rfc` gets subject
as title and the rendered body as transcript; `dibs` gets the same via
`DibsBody` (`src/pkg/inbox/dibs.go:90-113`). Suggested defaults when email is
enabled (owner can edit under ⚙ Auto-routing):

```yaml
inbox:
  rules:
    - source: email
      title_prefix: "RFC:"
      destination: spek_rfc
    - source: email
      tag: rfc
      destination: spek_rfc
    - source: email
      tag: idea
      destination: dibs
```

Reply: optional and off by default (`inbox.email.ack: false`). When on, and
only when the hub has an outbound mail sink (the escalation SMTP sink), the
hive replies once per filed item with the RFC or idea URL. No reply on drop.

## 8. Config, API, dashboard, hub, migration

Config (`inbox.email`, spoke side):

```yaml
inbox:
  email:
    enabled: false                 # default off
    webhook_secret_env: ""         # hub-less spokes only; hosted use the relay key
    allowed_senders: []            # extra addresses; authorized users are implicit
    max_message_bytes: 10485760
    ack: false
```

API (spoke, names follow `/api/inbox/*` in
[`api-reference/knowledge.md`](../api-reference/knowledge.md)):

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `POST` | `/api/inbox/email` | relay signature or webhook secret | Deliver one message |
| `GET` | `/api/inbox/email` | session | Address (masked), enabled, counts |
| `POST` | `/api/inbox/email/rotate` | owner | New uid, 24 h overlap |
| `POST` | `/api/inbox/email/settings` | owner | `enabled`, `allowed_senders`, `ack` |

API (hub): `POST /api/hub/inbox/email` (provider webhook), `GET
/api/hub/inbox/email/pending` (heartbeat bearer, push-reported spokes),
`POST /api/saas/hives/{id}/inbox-email/enable|rotate` (owner).

Dashboard: address chip in the Inbox header, `email` row in the sources strip of
`GET /api/inbox` (`InboxSourceStatus`, `src/pkg/dashboard/api_inbox.go:44-52`),
Integrations card, Settings → Inbox section for allowlist and ack.

Hub: `SaaSHive.InboxUID`, uid index, provider webhook handler, relay and pending
spool, heartbeat field. Migration: none; everything is additive and off.

## 9. Phased plan

1. **PR 1, spoke source and spool (~600 lines + tests).** `pkg/inbox/email.go`:
   `EmailSource`, spool, normaliser (RFC 5322 parse, HTML to markdown, caps),
   `source: email` in `DefaultInboxSources` gated on `inbox.email.enabled`,
   config and `ValidateInbox` additions. Tests: golden messages (plain, HTML,
   multipart with PDF, missing Message-ID, duplicate), rule matching on
   subject and plus-tag.
2. **PR 2, spoke endpoint and guards (~400 lines).** `POST /api/inbox/email`
   with relay-signature and shared-secret verification reused from
   `webhook_relay.go`, allowlist and provider-verdict gate, ioscan, audit
   events, rate caps, `GET /api/inbox/email`. Tests: fail-closed 503, bad
   signature 401, allowlist drop audited, cap drop.
3. **PR 3, hub (~500 lines).** `InboxUID` mint and store, provider webhook
   handler with Postmark and Cloudflare Worker payload adapters, relay to spoke,
   pending spool and heartbeat flag, owner enable/rotate endpoints. Tests:
   unknown uid is a 200 no-op, relay signed with the right hive's key,
   pending drained once.
4. **PR 4, dashboard (~300 lines).** Address chip, Integrations card,
   settings, rotate flow.
5. **PR 5, docs.** `inbox-voice-sources.md` rename to "Inbox sources", API
   reference rows, security-model control inventory line, this page's status.

Live test on a hosted hive after PR 3: send from an authorized user's address
with `RFC:` subject and confirm the RFC files; send from a non-allowlisted
address and confirm a drop audit with no reply; send a 15 MiB attachment and
confirm the cap; rotate and confirm the old address stops after the overlap.

## 10. Open questions for the owner

1. Allowlist source: GitHub verified emails (needs the `user:email` OAuth scope
   on the hub app and a re-consent for existing users) or owner-entered only?
2. Domain: `inbox.hivecommons.dev` operated by the hub, or let each hub
   operator configure `hub.inbox_email_domain`? Both are supported by the
   design; the first needs MX and a provider account.
3. Provider: Cloudflare Email Routing plus Worker (free, no bodies stored by a
   third party beyond transit) versus Postmark/Mailgun (richer parse, spam
   score, paid)? The adapter seam keeps both open; pick one for PR 3.
4. Should the uid be shown to all hive users or only the owner?
5. Hub retention of pending mail for push-reported spokes: 200 messages / 7
   days acceptable?
6. Is an acknowledgement reply wanted at all, given it requires outbound mail
   the hub does not send today?
