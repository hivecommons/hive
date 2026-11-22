# Dashboard Ask AI help: an optional, isolated "Ask" panel for the hub and hive dashboards (#19177)

Status: **Proposed; design only.** Spec for issue
[#19177](https://github.com/hivecommons/hive/issues/19177). The operator asked
for the feature and fixed the constraints in the issue (off by default and
inert, no third-party script on an authenticated origin without an isolation
argument, no private data to the third party, credentials only from
configuration or a Secret, hub and hive and self-hosted, a docs-link fallback,
cited and labelled answers). This page chooses an isolation model, records what
leaves the deployment, and cuts the work into phases. No code or behaviour
changes with this record. Every phase ships behind its own issue and is off by
default. Related records: [Customers: hub consent](customers-hub-consent.md) for
the per-person consent pattern this follows,
[session taint](session-taint.md) for the "what may leave" framing,
[telemetry](../telemetry.md) for the outbound-path review this falls under, and
[hosted hub](../hosted-hub.md) for how hosted hives sit behind the hub.

Scope: a help panel that answers "where do I find or do X in the dashboard" with
a short answer, links to the right section, and cited docs. Out of scope: any
action taken on the person's behalf, answers about live hive state (the existing
Hive Chat panel does that, see [What exists today](#what-exists-today)), the
public docs site's own widget (a separate, unauthenticated surface handled in
`hivecommons/docs`), and the Slack and Discord bots Kapa also offers.

Line references are to `v5` at `fc061b567` and give orientation. The identifier
next to each one is the stable reference. A statement marked **(unverified)** was
not confirmed against code or vendor documentation and is listed again under
[Verification notes](#verification-notes). Vendor statements cite the public
Kapa documentation at the time of writing (see [References](#references)).

## Summary of the decisions

1. **Recommended isolation model: (a), a server-side call.** The browser shows a
   plain chat panel written by us and talks only to our own backend. Our backend
   (the hub for the hub dashboard, the hive process for a hive dashboard) calls
   Kapa's Chat API with a key held in a Secret. No third-party code runs on the
   authenticated origin and the Content-Security-Policy does not change, on or
   off.
2. **A sandboxed frame (b) is the fallback, not the plan.** It is workable, but
   Kapa documents no hosted chat surface to frame, so we would have to host and
   operate a separate-origin page ourselves. Revisit only if (a) proves
   inadequate.
3. **The public docs widget on the authenticated origin (c) is ruled out.** The
   isolation argument does not hold: the script runs with the page's full
   privileges, it cannot be given a CSP nonce, it needs a second third party for
   bot protection, and it sends the page URL by default. See
   [Option (c)](#option-c-the-public-docs-widget-on-the-authenticated-origin).
4. **The question text is the only default payload.** Plus our own fixed
   vocabulary label for the dashboard section the person is in (justified below)
   and Kapa's opaque thread id for follow-ups. No page content, URL, repo name,
   hive name, login, email, token, cookie or user identifier is sent.
5. **Consent is per person, and the switch is per hive (or per hub).** The owner
   or hub operator switch is required and never sufficient; each person
   confirms a plain disclosure before their first question and can withdraw.
6. **Off by default and inert.** Unconfigured means no route, no panel, no
   request, no CSP change, no consent prompt. Configured but unreachable means a
   plain link to the docs.
7. **Hosted hives use the hub as the relay.** The Kapa key never leaves the hub;
   a hive authenticates to the relay with the per-hive bearer it already has.
   A self-hosted hive with no hub runs direct mode with its own key, or stays off.
8. **Answers are cited or they are not shown as answers.** Every answer carries
   at least one allowlisted source link and an "AI generated, may be wrong" label.
9. **Audit lines carry outcomes and counts, never text.** Nothing the person
   types is stored on our side in v1.

## What exists today

Every row was read in code.

| Thing | Where | What it does | Gap for this design |
|---|---|---|---|
| Hive dashboard CSP | `securityHeaders` (`src/pkg/dashboard/server.go:2003`), policy string at `src/pkg/dashboard/server.go:2100` | `default-src 'self'`, `connect-src 'self' ws: wss:`, no `frame-src` (so it falls back to `default-src`), `script-src-attr 'none'`. | A server-side relay needs no change. Any third-party origin would need one. |
| Inline-script hashes | `baseScriptSrcElem` (`src/pkg/dashboard/csp_script_src.go:52`), `ApplyDocumentScriptSrcElem` (`src/pkg/dashboard/webstatic/csp_script_src.go:111`) | `script-src-elem` is `'self'` plus sha256 hashes of the exact inline scripts served. Hashes, not nonces, by design. | A vendor script that cannot take a nonce can only be admitted by adding its host to this list. |
| Node proxy CSP | `src/proxy/server.js:985-996` | Sets its own CSP for the routes it serves: `script-src 'self' https://cdn.redoc.ly`, `connect-src 'self' https: ws: wss:`. | `connect-src` already allows any `https:` host here, so CSP is not what would contain a hostile script's network access. Which document is fronted by this and by the Go policy is not traced **(unverified)**. |
| Hub dashboard document | `dashboardHTMLETag` (`src/pkg/hub/saas_pages.go:18`) serves the embedded dashboard | The hub activity page sets its own strict CSP (`activityPageCSP`, `src/pkg/hub/activity_page.go:32`). No hub code sets a CSP for the main dashboard document. | Whether the proxy or ingress supplies one for it is not traced **(unverified)**. The relay design does not depend on it. |
| Where header ownership lives | `src/deploy/nginx.conf:96` | States that CSP is owned by the Go dashboard and the Node proxy, not nginx. | Any CSP change in option (b) or (c) would have to land in both emitters. |
| Existing help links | `dashboardDocsHref` (`src/pkg/dashboard/static-src/js/sections.js:192`), `settingHelpMark` (`src/pkg/dashboard/static-src/js/sections.js:219`) | Every section "?" mark and Settings help mark links to a page under `src/docs/` on GitHub for the running release line. | Answers should link to the same targets, and the docs-link fallback reuses this one function. |
| Hive Chat | `ChatGroundedAnswersConfig` (`src/pkg/config/chat.go:21`) | The existing chat panel answers questions about hive state with a model the operator configured, over a snapshot of the hive. | That is state, with private data, to the operator's own model. Ask AI is docs help to a third party and must never receive that snapshot, so it is a separate panel. |
| Settings switch pattern | `CustomersAvatarsConfig` (`src/pkg/config/customers.go:40`), env `EnvCustomersAvatarsEnabled` (`src/pkg/config/customers.go:33`), the Features toggle at `src/pkg/dashboard/static-src/js/config-features.js:893` | A config key, an env override and a Settings, Features toggle with a help mark. Avatars is the one existing switch that makes the viewer's browser contact another site. | The pattern to follow for the new switch. |
| Feature switch by env | `EnvSessionsEnabled` (`src/pkg/config/dashboard_sessions.go:17`) | Env wins over `hive.yaml` for a feature gate. | Same precedence. |
| Secret files | `APIKeyFileWarnings` (`src/pkg/config/api_key_file_validation.go:14`) | Key files must live under the allowed secret directories; an out-of-root path is never read. | The Kapa key file follows the same rule. |
| Hub to hive channel | `handleHeartbeat` (`src/pkg/hub/server.go:1906`), `verifyHeartbeatBearer` (`src/pkg/hub/hub_keys.go:165`), `derivePerHiveKey` (`src/pkg/hub/hub_keys.go:39`) | A per-hive bearer bound to the hive id, derived from a hub secret. | Reused as the relay credential, so no new secret is issued to hives. |
| Hub identity of a hive's user | `proxyAuthHeader` (`src/pkg/dashboard/server.go:89`) | A hub-proxied hive request carries the hub-authenticated user and a proof header. | The hive already knows who is asking, with no Kapa-specific identity. |
| Hub session | `getAuthUser` (`src/pkg/hub/saas_auth.go:688`), `requireAuth` (`src/pkg/hub/saas_auth.go:22`) | The hub resolves the signed-in person from the session. | The relay route is behind this. |
| Per-user limiter precedent | `npsRateLimiter` (`src/pkg/dashboard/nps.go:93`) | A small per-user in-memory limiter. | A pattern for the per-person limit; the relay needs per-hive and global limits too. |
| Secret-shape matcher | `FindSecret` (`src/crux/logscrub/handler.go:157`) over `secretPatterns` (`src/crux/logscrub/handler.go:48`) | Returns a category for a credential-shaped string, never the value. | Reused to refuse a question that contains a pasted credential, rather than writing a new matcher. |
| Audit lines | `audit: admin updated user` (`src/pkg/hub/saas_users_admin.go:618`) | One structured line per sensitive action, with names and no values. | The shape for the Ask audit lines. |
| Docs publishing | authored in `src/docs/`, published at `docs.hivecommons.dev` from `hivecommons/docs` through a curated list | A page is on the site only if it is in the sync list and the nav map (read from a local clone of `hivecommons/docs`). | Decides what Kapa can ingest. See [Docs coverage](#docs-coverage-in-kapa). |

Three findings shape the design:

1. **The dashboards are same-origin, authenticated, and CSP does not contain a
   hostile script's network access.** The proxy already allows `connect-src`
   `https:`. So the only reliable containment is to keep third-party code off
   the origin, not to rely on a CSP allowlist.
2. **There is no network path to a third party today that carries a person's
   free text.** The one existing switch that contacts another site (avatars)
   sends only a GitHub login the viewer is already looking at.
3. **Help links today point at GitHub, not at the docs site.** The citation
   allowlist and the docs ingestion plan both have to account for that.

## Threat model and what is protected

What we protect: the authenticated session and everything the dashboard shows
(agents, repos, tokens-adjacent state, people), and a person's control over what
they typed leaving the deployment.

| Actor | Risk | In scope |
|---|---|---|
| Third-party script on the dashboard origin | Reads the DOM, calls our `/api/` as the person, reads storage, exfiltrates to any `https:` host. | Yes. This is the reason to rule out (c). |
| Vendor service | Receives question text and keeps it; its project admins can read it in analytics. | Partly. We minimise and disclose; we cannot control the vendor. |
| Person pasting something sensitive into the box | A token or an internal name reaches the vendor. | Yes. Credential-shaped input is refused; the notice says not to paste private data. |
| Prompt injection through question text or retrieved docs | The answer contains a hostile link, markup or an instruction to act. | Yes. Answers render as text with an allowlisted link set; the panel can take no action. |
| Abuser or runaway client | Burns the shared vendor quota, starving the docs site and other hives. | Yes. Limits below. |
| Another hive on the hub relay | Uses another hive's quota or thread. | Yes. The bearer is bound to its hive id and threads are bound to it. |
| Malicious hub operator or vendor insider | Reads what they are entitled to read. | No. |

## The options compared

All three call the same vendor. They differ in who runs code where and who
talks to the vendor.

### Option (a): server-side call, plain chat panel

The panel is our own markup and script served from `'self'`. It posts the
question to our own route (`POST /api/ask`). The backend validates, applies
limits, redacts, calls Kapa's Chat API (`POST
https://api.kapa.ai/query/v1/projects/{project_id}/chat/`, key in the
`X-API-KEY` header, `integration_id` and `query` in the body), filters the
sources, and returns JSON.

- **Isolation:** no third-party code in the page, no vendor origin in the CSP,
  no vendor cookie, no CAPTCHA provider. The browser never learns the key, the
  project id or the vendor host. The vendor docs say to keep the key server-side
  and proxy through authenticated routes for authenticated users, which is this.
- **What leaves:** exactly what the backend chooses to put in the request body.
  We control every field and can prove it with a test against a fake server.
- **Cost:** we write and maintain the panel, the sanitiser, the limits and the
  fallback. Streaming is possible later by relaying the vendor's streamed
  endpoint; v1 is one request, one JSON answer.
- **Environments:** works wherever the process can reach the vendor over
  HTTPS. Fails cleanly (docs link) where it cannot.

### Option (b): sandboxed iframe

Kapa documents no hosted chat page or frame mode **(unverified, absence of
evidence in the integrations index and widget page)**. So (b) means we host our
own small page that loads the vendor widget or SDK, and embed it with
`<iframe sandbox="allow-scripts">` (no `allow-same-origin`, so it gets an opaque
origin and cannot read the parent or send its cookies).

- **Isolation:** good if, and only if, the page is on a different registrable
  origin from every dashboard and the sandbox flags are right. Served from a
  hub path it would share the hub origin and the argument fails.
- **Costs and risks:** a new deployable to host and keep alive (and a
  single point of failure for every hive); `frame-src` for that origin added to
  both CSP emitters when on; the widget still sends the page URL it sees (the
  frame's, which is harmless) and still brings the bot-protection provider, a second third party; the vendor's "enabled domains" allowlist would hold our frame origin,
  so anyone could embed that page and spend our quota; in-app navigation links
  need a `postMessage` bridge, which is a new trust boundary; self-hosted and
  air-gapped hives would have to reach our hosted page.
- **Verdict:** a defensible second choice. More moving parts than (a) for the
  same data exposure, so not recommended first.

### Option (c): the public docs widget on the authenticated origin

The script tag the vendor documents for websites, loaded straight into the
dashboard pages.

- **Why it fails the isolation argument:**
  - A script from another origin runs with the page's authority. It can read
    every pixel of text on the page, call our same-origin API as the signed-in
    person (including any write route that person may use), and read what the
    page stores. A vendor-side compromise or a malicious update of
    `widget.kapa.ai/kapa-widget.bundle.js` becomes a full session compromise of
    the hub or a hive.
  - It cannot be pinned. We would load a moving, vendor-controlled bundle; the
    dashboard's CSP model is built on hashes of exact bytes
    (`ApplyDocumentScriptSrcElem`), and the vendor states the widget "does not
    support CSP nonces". We would have to allow the host in `script-src-elem`,
    which trusts everything that host serves.
  - It needs a CSP widening in both emitters: `script-src` hosts for the widget
    and, by default, Google's bot-protection hosts; `connect-src` hosts for the
    vendor proxy; `frame-src` for the bot-protection challenge; `style-src`
    `'unsafe-inline'` (already present). That adds a second third party
    (the bot-protection provider) beyond the one we chose.
  - It sets a first-party cookie (`kapa_web_id`) on the dashboard origin and
    can attach an email or custom id to conversations through page settings.
    The SDK's request shape carries an `originUrl`, so the page URL (which
    includes hive and repo names on this dashboard) is a default field.
  - The vendor's own guidance for bot protection on an unauthenticated widget
    is CAPTCHA plus rate limiting and domain checks, "not full security". Our
    surface is authenticated, so we can do better ourselves, as in (a).
- **Verdict:** ruled out. The operator's note in the issue allowed it only if the
  isolation argument held; it does not.

### A fourth shape considered: browser talks to the vendor with a server-minted token

The vendor's agent SDK lets our server mint a one-hour session token (key stays
on the server) and the browser then talks to the vendor directly. It removes the
key from the browser but keeps the browser-to-vendor request (so the person's IP
and the vendor's CORS and CSP needs appear), pulls a third-party React SDK into
a dashboard that is plain JavaScript, and loses our single choke point for limits
and audit. Not recommended for v1.

### Comparison

| Concern | (a) Server call | (b) Sandboxed frame | (c) Widget on origin |
|---|---|---|---|
| Third-party code on the authenticated origin | None | None if cross-origin and sandboxed | Yes |
| CSP change when on | None | `frame-src` in two emitters | `script-src-elem`, `connect-src`, `frame-src` in two emitters |
| Second third party (bot protection) | No | Yes, inside the frame | Yes |
| What we control in the payload | Every field | Only the frame's inputs | Little; vendor script decides |
| Where limits and audit live | In our backend | Mostly at the vendor | Mostly at the vendor |
| New infrastructure | None | A separate-origin host | None |
| Works air-gapped or self-hosted | Direct mode with own key, or off | Needs our hosted page | Needs vendor reachable from the browser |
| Build cost | Highest (panel, sanitiser, limits) | Medium | Lowest |

**Recommendation: (a).** It is the only option where the answer to "what can the
third party see" is a short list we write and test, and the only one with zero
change to the security posture of the page when switched on.

## What leaves the deployment

Per request, from our backend to the vendor, and nothing else:

| Field | Value | Why it is justified |
|---|---|---|
| `query` | The person's question text after the checks below, capped at a named length. | The product. |
| Section label | One value from a closed list of our own dashboard section ids (for example "Settings, Features"), prepended as plain context text. | Lets "where is this button" resolve to the right section. It comes from a vocabulary we ship, never from data on screen. The operator may drop it (see [Open questions](#open-questions)). |
| `integration_id` | The configured integration's identifier. | The vendor requires it to attribute the conversation; it is an identifier, not an authenticator. |
| Vendor thread id | The opaque id the vendor returned for the previous turn of this conversation. | Follow-up questions. We keep it server-side, bound to the person (and hive), and expire it. |
| Source IP | Our backend's egress address. | Unavoidable. It is the deployment's address, not the person's. |

Never sent: page content, DOM text, the URL or route, `originUrl`, hive, repo,
agent or customer names, the person's login, email, display name or role,
session or dashboard tokens, cookies, file uploads, any Hive Chat snapshot, and
any `user` identification object (the vendor accepts an email, a client id and
company or name metadata; we send none of it).

Before sending, the backend refuses (does not silently edit) a question that
matches the credential-shape matcher (`FindSecret`) and tells the person why. It
does not try to detect names or other personal data; the notice says not to
include them. The vendor can additionally mask personal data in questions
(phone numbers, names, email, card numbers, IBAN, IP addresses) if its project
setting is switched on; it is off by default at the vendor, so the Kapa-side
checklist in [Docs coverage](#docs-coverage-in-kapa) requires turning it on.

What the vendor does with it: it stores conversations and sources for the
duration of the service, deletes them within 60 days of contract end, and says
it has data-processing agreements and training opt-outs with its model
providers. Project administrators can read the questions in its analytics. The
consent text says all of this in the person's terms: "Your question, and the
section you are in, are sent to Kapa.ai, an AI service. The maintainers of this
project can read it in their analytics. Do not include secrets or private
details." We cannot recall what the vendor already holds, and the notice says so.

What comes back and what we do with it: the answer text and the sources. The
backend (1) drops any source whose URL is not on the allowlist (the docs site
host and the GitHub docs path under `hivecommons/hive`), (2) returns the answer as
text, never as HTML, with at most a small closed set of formatting, (3) shows
links only from the allowlisted sources and from a fixed map of in-app section
ids, and (4) treats an answer with no surviving source as "no cited answer",
which becomes the docs-link fallback rather than uncited AI text. The shape of
the vendor's non-streamed response (field names for sources) is not shown in the
public pages read; the streamed callbacks expose sources with a title, a URL and
a type. Phase 1 confirms the exact shape against the API reference before
coding **(unverified)**.

## Consent and the switch

Two gates, both required, the same spirit as the
[hub consent design](customers-hub-consent.md):

1. **The switch (who may offer it).** On the hub, the hub operator. On a hive,
   the hive owner. Off by default everywhere. The owner or operator switch is
   never sufficient: it makes the panel available, it does not consent for
   anyone.
2. **Per-person consent (who may use it).** Before a person's first question,
   the panel shows the disclosure above and a single confirm. It is recorded per
   person, with a disclosure `text_version`; a material change to the text asks
   again. A person can withdraw at any time in the panel; withdrawal takes
   effect on the next request. Declining leaves the panel as a docs link.

Why per person, not per hive: the data is what that person typed, and the
reader of the notice must be the person who decides. The per-hive control is the
owner switch, and a hub admin can additionally disable the relay for a single
hive.

Where the record lives: a small file per person, separate from any other user
record, written atomically with owner-only permissions, keyed by the same stem
the user's record uses so a user deletion finds it. On the hub it lives with the
hub's data; on a hive it lives under the hive's data directory keyed by the
dashboard identity. The hub relay does not hold hive persons' consent and does
not need to: the consent is taken on the dashboard the person is reading the
notice on, and the hive's relay request is only sent after that. The hub
enforces the parts it can: the per-hive allow switch and the quotas.

## Configuration

Off by default. A new top-level `ask:` block, with env overrides (env wins over
`hive.yaml`, as with the other feature switches), and a Settings, Features
toggle with a help mark that names what is sent. Names are proposals.

| Key | Env | Default | Meaning |
|---|---|---|---|
| `ask.enabled` | `HIVE_ASK_ENABLED` | `false` | The switch. Off: no route, no panel, no consent prompt, no request, no CSP change. |
| `ask.mode` | `HIVE_ASK_MODE` | `off` | `off`, `direct` (this process calls the vendor with its own key), or `hub` (a hive calls the hub relay). The hub itself is always `direct`. |
| `ask.kapa.project_id` | `HIVE_ASK_KAPA_PROJECT_ID` | unset | The vendor project. Configuration, never in the repo. |
| `ask.kapa.integration_id` | `HIVE_ASK_KAPA_INTEGRATION_ID` | unset | The vendor integration identifier. Configuration, never in the repo, never logged. |
| `ask.kapa.api_key_file` | `HIVE_ASK_KAPA_API_KEY_FILE` | unset | Path to the key, under the allowed secret directory, mounted from a Secret. No inline key and no env var holding the key. |
| `ask.send_section_label` | `HIVE_ASK_SEND_SECTION_LABEL` | `true` | Whether the closed-vocabulary section label is included. |
| `ask.limits.*` | `HIVE_ASK_LIMIT_*` | see [Limits](#rate-and-cost-limits) | Named, tunable. |
| `ask.relay.hives` | hub only | `hosted` | Which hives may use the hub relay: `none`, `hosted`, or `registered`. A hub admin can also disable one hive. |

If `enabled` is true but a required value is missing or the key file is unreadable,
the feature stays off, the Settings toggle shows why, and one warning line is
logged without values (the same fail-quiet approach the other key-file settings
take at boot). The key, the project id and the integration id are never
returned by any API, never put in a response, never logged, and the config
export path redacts them like other credentials.

## Failure and fallback

The panel always has a way out: a plain link to the docs page for the section
the person is in (`dashboardDocsHref`), and the docs home.

| Condition | Behaviour |
|---|---|
| Feature off or unconfigured | No panel. The existing "?" help marks are unchanged. |
| Not consented or withdrawn | The panel shows the notice and the docs link. No request. |
| Vendor unreachable, timeout (named constant, proposal 30 s), 5xx | "Ask is unavailable right now", the docs link, nothing retried automatically. |
| Vendor rate limit (HTTP 429) or our own limit | "Busy, try again in N seconds" and the docs link. |
| Repeated failures | A circuit breaker opens for a cool-down and the panel shows the docs link without calling out. One audit line when it opens and closes. |
| Credential-shaped input | The question is refused with the reason; nothing is sent. |
| Answer with no allowlisted source | Shown as "No cited answer found in the docs" with the docs link, not as an answer. |
| Hive cannot reach the hub relay | Same as unreachable; the hive never falls back to a key it does not hold. |
| Hub relay disabled for this hive | The hive shows the docs link. |

The fallback is exercised by turning the feature off or by pointing it at a
closed port; neither may change the rest of the dashboard.

## Rate and cost limits

The vendor's chat limit is 20 requests per minute per team, shared across every
project and integration the team has, including the docs site's own widget and
any bots, and it can be raised by contacting the vendor. A dashboard that sends
traffic can starve the docs site. So the relay budget is a deliberate fraction of
that, and the numbers below are proposals, each a named constant overridable by
env, to be set after the vendor limit is raised or confirmed:

- Per person: a small burst and an hourly cap, one request in flight at a time.
- Per hive (relay): an hourly cap, so one hive cannot spend the hub's budget.
- Hub-wide: a per-minute cap well under the vendor's per-team limit, a bounded
  number in flight, and a daily budget. When the daily budget is spent the panel
  shows the docs link until it resets, and a hub operator sees a counter.
- Input and output caps: a maximum question length and a maximum answer length.
- Circuit breaker as above.

Abuse handling: only signed-in, consented people reach the route; a blocked
account is refused as for any other route; the hub operator has a kill switch
(the switch itself) and a per-hive disable; repeated limit hits from one person
produce one audit line per window, not one per request. Cost visibility: a
counter of requests by outcome and a daily total on the hub operator view, so a
spike is visible before the vendor limit is. No per-person cost data is kept
beyond the hourly counter in memory.

## Audit logging without content

One structured line per event, in the style of the existing audit lines, with
names and counts and never text:

- Request: person id (the same stem other audit lines use), hive id for relay
  requests, outcome (`ok`, `no_cited_answer`, `refused_secret_shape`,
  `rate_limited`, `breaker_open`, `upstream_error`, `not_consented`), the
  question length, the number of sources kept, and latency.
- Consent: grant, withdraw and text version, per person.
- Operations: the switch changing, the breaker opening and closing, the daily
  budget being spent.

Not logged, and not stored in v1: the question text, the answer text, the
sources, the thread id, the key, the project id and the integration id. The
conversation lives in the browser's memory for the open panel and in the vendor's
thread; closing the panel drops it. If a later phase wants history it needs its
own decision.

## Accessibility

- The panel is a region with an accessible name, opened and closed by a real
  button reachable by Tab, with a visible focus state and an Escape key close
  that returns focus to the opener.
- The input has a label; the send action works with Enter; a long answer does not
  trap focus.
- New answers and errors are announced through a polite live region; the busy
  state is announced and not only shown as a spinner.
- The "AI generated, may be wrong" label and the source list are real text in the
  reading order, not colour or icon alone. Source links say where they go.
- Contrast and sizing use the dashboard's existing tokens and respect the
  reduced-motion and dark-mode settings; the panel works at phone width and does
  not cover dashboard data (the earlier floating-bubble complaint, issue
  [#7160](https://github.com/hivecommons/hive/issues/7160), is the reason it docks
  rather than floats).
- The consent notice is plain language, keyboard operable, and available without
  asking a question.

## Docs coverage in Kapa

Use the published Hive operator docs on `docs.hivecommons.dev` as the source
of record. The [dashboard UI map](../dashboard-ui-map.md) is generated from
the UI sources so its headings and navigation labels can be indexed without
maintaining another copy. Include it alongside the
[spoke dashboard](../dashboard.md), [settings](../settings.md),
[hosted hub](../hosted-hub.md), and [hub deployment](../hub-deployment.md)
guides. On v5, `dashboardDocsHref` sends `?` links to the docs site for
published pages and falls back to GitHub for the rest.

### Project-owner checklist before enabling Ask

Keep Ask off on public deployments until this checklist passes. Work in
the Kapa console and the deployment's secret store; do not put keys,
project IDs, integration IDs, or console exports in the repository.

- [ ] **Limit ingestion to published operator help.** Configure a
  [Website Crawl source](https://docs.kapa.ai/knowledge-sources/connectors/web-crawling/control-which-pages-are-crawled)
  for the dashboard integration. Use the canonical HTTPS URLs from the
  live Hive docs navigation as start URLs and **Include URLs** entries
  (one anchored Regex entry per approved page, for example
  `^https://docs\.hivecommons\.dev/docs/hive/dashboard/dashboard-ui-map/?$`).
  Include the dashboard, UI map, glossary, sections and per-section pages,
  Settings and its help targets, getting started, agent configuration, env
  reference, telemetry, and the hosted, self-hosted registration, hub
  deployment and fleet guides. Don't allow the whole site, `/docs/hive/`,
  or a broad reference prefix: those can also contain architecture records
  and runbooks. Keep `src/docs/design/`, `src/docs/adr/`, and runbooks out
  by approving URLs against their source paths, not just their published
  slugs. Do not add GitHub issues, pull requests, or chat sources.
- [ ] **Verify the effective corpus.** Run **Preview all**; check Included,
  Excluded and parsed page content before publishing. Confirm that Ask
  cannot inherit excluded material from other sources (including Global
  sources); check the integration's
  [source groups](https://docs.kapa.ai/knowledge-sources/source-groups)
  or use an isolated project if existing sources cannot be separated.
  Confirm the same in **Deployed** after publishing. This keeps excluded
  documents out of Ask without removing them from the public docs site.
- [ ] **Mask questions.** Enable
  [user-message PII masking](https://docs.kapa.ai/security/pii-masking#enabling-user-message-pii-masking)
  on the project. Verify with synthetic data only; masking does not
  replace the disclosure or authorize sending private data.
- [ ] **Constrain answers.** Associate a
  [customization](https://docs.kapa.ai/customizing/use#associating-a-customization-with-an-integration)
  with the dashboard HTTP API integration. Limit answers to using and
  navigating the Hive dashboards; require a cited doc, say when the docs
  do not answer, and refuse off-topic questions and requests to override
  those instructions. Review enabled domains wherever the integration
  exposes that setting; keep only the exact domains in use, with no wildcards.
  Do not enable a browser widget or send an origin URL to satisfy that list.
- [ ] **Protect the shared quota.** Ask [Kapa support](https://support.kapa.ai)
  to raise the [per-team chat limit](https://docs.kapa.ai/integrations/chat-api#rate-limits).
  Confirm the effective limit before choosing a hub cap that leaves room
  for the public docs widget and bots.
- [ ] **Verify on a hub before rollout.** Use a test deployment running the
  phase 2 panel ([#19181](https://github.com/hivecommons/hive/issues/19181)).
  Sign in and consent. Ask “where do I add a repo?” and “how do I pause an
  agent?” in separate conversations. The first should distinguish adding a
  monitored repo ([Settings → Repos](../settings.md#repos)) from
  [requesting a new hosted hive](../hosted-hub.md#2-request-a-hosted-hive-for-a-repo).
  The second should point to the
  [spoke agent card](../dashboard.md#pausing-an-agent), not the hub's fleet
  controls. Open the citations to verify they reach the actual instructions.
  Check off-topic refusal and that withdrawal stops the next request.
- [ ] **Close the feedback loop.** Review
  [Coverage Gaps](https://docs.kapa.ai/analytics/coverage-gaps) and
  [Top Questions](https://docs.kapa.ai/analytics/top-questions) for the
  dashboard integration. Fix unclear docs or UI labels using paraphrases
  rather than copying question text. Recheck the indexed pages and rerun
  the navigation questions after publishing fixes.

### Status of the docs coverage work (#19182)

Re-checked against `hivecommons/docs` main on 2026-11-18:

- **Sync list.** The main dashboard page (`dashboard.md`), the Settings page
  (`settings.md`), hosted hub onboarding (`hosted-hub.md`), hub deployment
  (`hub-deployment.md`) and the telemetry page (`telemetry.md`) were on the docs
  repo's deliberate-omission list, so the site and Kapa had none of them. The
  docs repo change that follows this issue moves them onto the site.
- **UI map.** [Dashboard UI map](../dashboard-ui-map.md) lists the sidebar
  entries, the Settings tabs and the per-agent tabs with their on-screen labels,
  how to sign in to each kind of dashboard, and where common actions live. Its
  tables are generated by `src/pkg/dashboard/uimap/render-dashboard-ui-map.py` and checked in
  CI. [Dashboard settings explained](../settings.md) now has an entry for every
  Settings tab.
- **Help links.** On the v5 line, `dashboardDocsHref` opens docs.hivecommons.dev
  for pages the site publishes and GitHub for the rest; other lines stay on
  GitHub because the site follows v5 only.
- **Ingestion.** The vendor's website source does offer per-source "Exclude
  URLs" (substring or regex) and "Exclude selectors" (skip a page that contains
  a CSS or XPath selector), which settles the earlier unverified note. ADRs share
  a URL prefix and can be excluded by URL; design records are mixed into the
  site's Reference section, so the docs site marks them with an element the
  vendor can exclude by selector. Adding those two exclusions is a source edit
  for the project owner.
- **Vendor-side checklist.** Done: PII masking for questions is on (all six
  built-in entity types); a website widget integration exists for
  `https://docs.hivecommons.dev` only; a customization scopes answers to Hive
  Commons help and politely refuses off-topic questions, and the widget uses it.
  Requested from the vendor: a higher per-team chat limit, GitHub Discussions as
  a source, and a corrected name for the issues source. The Ask-specific
  telemetry and consent wording is published in the
  [telemetry page](../telemetry.md) and the dashboard docs. Still to do, all
  outside this repo: the two ingestion exclusions above and a regular look at
  coverage gaps and top questions.

## Phases

Hub first, then hive. Each phase is its own issue, is off by default, and is
blocked by the one before. No phase changes the CSP.

| Phase | Issue | Scope |
|---|---|---|
| 1 | [#19180](https://github.com/hivecommons/hive/issues/19180) | **Hub backend:** the `ask:` config and env, secret loading, the vendor client, the per-person consent record and routes, credential-shape refusal, source filtering, limits, circuit breaker, audit lines, `POST /api/ask` on the hub. Tests against a fake vendor server. No panel. |
| 2 | [#19181](https://github.com/hivecommons/hive/issues/19181) | **Hub panel:** the chat panel on the hub dashboard (consent notice, answer with sources, "AI generated" label, accessibility, the docs-link fallback), the hub operator switch with its help mark. |
| 3 | [#19182](https://github.com/hivecommons/hive/issues/19182) | **Docs coverage:** sync-list and nav-map gaps, the generated UI map page, ingestion exclusions, the vendor-side checklist, the docs and telemetry page updates. |
| 4 | [#19183](https://github.com/hivecommons/hive/issues/19183) | **Hive backend:** `ask.mode` `hub` and `direct`, the hive route, the per-person consent record on the hive, the hub relay endpoint with the per-hive bearer, per-hive quota and per-hive disable, the Settings, Features toggle. |
| 5 | [#19184](https://github.com/hivecommons/hive/issues/19184) | **Hive panel and live verification:** the panel on the hive dashboard (reusing phase 2's component), self-hosted direct-mode docs, and a live check on a hosted hive, a self-hosted hive and the hub. |

Phase 1 is useful on its own for the hub operator to try against the real project
behind the switch. Phase 3 can run beside phase 2 but is chained after it so nothing goes
live before the docs are ready.

## Test strategy

- **Payload:** a fake vendor server asserts the request body has only the fields
  in [What leaves](#what-leaves-the-deployment); a test seeds a page with a
  known secret, repo name, hive name and login and asserts none appear.
- **Off means inert:** with the switch off the route is absent, the panel is
  absent, a fake vendor sees zero requests, and the CSP header is byte-identical
  to the baseline (a tripwire test, like the existing CSP tripwires).
- **Credentials:** the key, project id and integration id never appear in any
  API response, log line, config export or error string.
- **Consent:** no request without a current consent; withdrawal and a text change
  block the next request; consent for one person does not cover another.
- **Sources and rendering:** an answer with no allowlisted source becomes the
  fallback; a hostile link, HTML or script in an answer renders as inert text.
- **Limits and breaker:** per-person, per-hive and global caps hold; a 429 and a
  timeout both produce the fallback; the breaker opens and closes.
- **Relay:** a bearer for hive A cannot use hive B's quota or thread; a disabled
  hive gets the fallback.
- **Accessibility:** keyboard-only open, ask, read, close; the live region
  announces answers and errors.
- **Live:** one hosted hive, one self-hosted hive and the hub, answer a real
  navigation question with a correct link, then turn the feature off and see the
  panel vanish.

## Risks

- **The vendor sees every question.** Minimised, disclosed and consented, but
  not avoidable. Project administrators can read them.
- **Shared vendor quota.** The docs widget and any bots share the per-team limit,
  so we may be limited by someone else's traffic until it is raised.
- **Wrong or stale answers.** The label, the citations and the no-source fallback
  reduce the harm; the quality is only as good as the docs.
- **Pasted private data.** Refused when credential-shaped; otherwise the notice is
  the only guard.
- **Maintenance of our own panel.** The cost of option (a).
- **Hosted hives share one relay and one key.** A single point of failure and one
  quota to protect; accepted because it keeps the key on the hub.
- **Vendor shape drift.** The chat response shape was not fully shown in the
  public pages; phase 1 pins it with a contract test.

## Open questions

The recommended defaults are stated; the operator may overrule any.

1. **Send the section label?** Recommended yes (closed vocabulary, no data).
   Dropping it costs navigation accuracy only.
2. **Should the relay be on for self-hosted hives registered with a hub**, or
   hosted only? Recommended hosted only at first (`ask.relay.hives: hosted`).
3. **Per-person consent versus a single hive-owner consent.** Recommended per
   person, as the issue suggests, with the owner switch as the per-hive gate.
4. **Who funds the vendor quota** if usage grows past the program's limits.
5. **Should anything be stored** (history, thumbs-up or down) in v1? Recommended
   no; the vendor's feedback calls are a later decision.
6. **Streaming answers**: not in v1; revisit if latency is poor.
7. **Move help links to the docs site** so answers and "?" marks share one host.

## Exit criteria

#19177 closes when this page is merged and phases 1 to 5 exist as issues linked
to it and to each other (done: the table above, each blocked by the one before). Implementation then proceeds through those issues.

## Verification notes

Verified by reading code on `v5` at `fc061b567`: the hive dashboard CSP string and
its owner function, the hash-based script allowlist, the Node proxy CSP, the
header-ownership note in the nginx config, the help-link builder and help mark,
the existing Chat panel config, the Customers avatars switch, the Sessions env
override, the key-file validation, the heartbeat bearer and its derivation, the
proxy-proof header, the hub session helpers, the per-user limiter, the
credential-shape matcher and the audit line style. Vendor statements were read
from the public Kapa documentation pages listed in References; no account, key or
integration was used or touched.

Not verified, and the reason:

- **Which CSP applies to the hub's main dashboard document** and whether the
  Node proxy or ingress adds one. Not traced. The recommended design does not
  depend on it.
- **The vendor's non-streamed chat response shape** (source field names), the
  streamed endpoint path and events, and whether the chat endpoint returns a
  thread id in the body. The public pages read do not show them.
- **Whether the vendor documents a hosted or frame mode.** None was found; the
  claim is an absence in the pages read.
- **Deletion of a single thread through the chat API.** Not described in the
  pages read.
- **A required "powered by" attribution for the chat API.** Documented for the
  chat SDK; not found for the API. The panel shows it anyway, which is also the
  disclosure.
- **Live Ask responses and the deployed Kapa corpus.** Verify with the
  [project-owner checklist](#project-owner-checklist-before-enabling-ask).
- **Every number** (limits, timeouts, caps) is a proposal, not a measurement.

## References

- Issue [#19177](https://github.com/hivecommons/hive/issues/19177).
- [Customers: hub consent](customers-hub-consent.md),
  [session taint](session-taint.md),
  [authorization decision point](authorization-decision-point.md),
  [telemetry](../telemetry.md), [hosted hub](../hosted-hub.md),
  [dashboard sections explained](../dashboard-sections.md).
- Kapa documentation: [integrations](https://docs.kapa.ai/integrations/),
  [website widget](https://docs.kapa.ai/integrations/website-widget),
  [integrations FAQ (CSP, CORS, deploying behind a login)](https://docs.kapa.ai/integrations/faq),
  [Chat API](https://docs.kapa.ai/integrations/chat-api),
  [API reference](https://docs.kapa.ai/api/reference),
  [HTTP API and limits](https://docs.kapa.ai/retrieval/http-api/),
  [Chat SDK](https://docs.kapa.ai/dev/sdk/),
  [agent authentication](https://docs.kapa.ai/dev/agent/authentication),
  [PII masking](https://docs.kapa.ai/security/pii-masking),
  [certifications and retention](https://docs.kapa.ai/security/certifications),
  [customizing](https://docs.kapa.ai/customizing),
  [data ingestion](https://docs.kapa.ai/knowledge-sources/data-ingestion).
