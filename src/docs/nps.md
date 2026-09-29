# NPS feedback prompt

The hive dashboard can occasionally ask a signed-in user one question: *how is the hive working for you?* The answer is a 4-point score, optionally with a sentence of free text. Responses go to the hub, where only hub admins can read them. This page covers what is collected, where it goes, who can see it, and how to turn it on or off. Tracking issue: [#9610](https://github.com/hivecommons/hive/issues/9610).

The prompt is modeled on the one KubeStellar Console ships (`useNPSSurvey.ts`), including its timing and backoff, and it avoids the security problems that implementation shipped with (see [Security properties](#security-properties)).

## What the user sees

- A small card in the bottom-left corner with four options: 😠 Not great (1), 😐 Meh (2), 🙂 Good (3), 😍 Love it (4).
- After picking one, an optional free-text box whose prompt depends on the answer (what is not working / what would make it better / what they like most), capped at 500 characters, and a **Send** button.
- A **×** (or Escape) dismisses it. After sending, a short thank-you replaces the card.

The card is shown only when all of these hold:

- NPS is enabled for this hive (see [Turning it on or off](#turning-it-on-or-off)) and the hive has a hub link, or, for a standalone hive, a configured relay (see [Standalone hives: the NPS relay](#standalone-hives-the-nps-relay)).
- The viewer is signed in with write access. Anonymous visitors, public snapshots and read-only users never see it.
- The per-browser timing rules allow it (defaults shown; see [Tuning the timing](#tuning-the-timing)):

| Rule | Default | `hub.nps_timing` key |
|------|---------|----------------------|
| First prompt | not before the 2nd browser session | `min_sessions` |
| Engaged time before prompting, first eligible session | 5 minutes | `second_session_engagement_seconds` |
| Engaged time before prompting, from the 3rd session on | 1 minute | `returning_engagement_seconds` |
| After a response | wait 30 days | `reprompt_days` |
| After a dismissal | wait 7 days | `dismiss_retry_days` |
| After this many dismissals, wait `reprompt_days` instead | 3 | `max_dismissals` |
| Per page load | at most once | - |

"Engaged time" counts only while the tab is visible and the user has used the mouse, keyboard or scroll in the last minute. The timing state lives in the browser's `localStorage` and `sessionStorage` (keys prefixed `hive-nps-`). If storage is unavailable (a private window, blocked site data) the prompt simply does not appear.

## What is collected

For each response, the hub stores:

| Field | Source |
|-------|--------|
| `hive_id` | the spoke's own hive ID |
| `score` | 1-4 |
| `feedback` | the optional free text, trimmed, control characters removed, max 500 characters |
| `timestamp` | set by the hub on receipt |
| `dashboard_version` | the spoke's short build hash |

**No user identity is collected.** The spoke uses the signed-in username only locally, as its rate-limit key, and never sends it. The hub has no field to store one. The hub knows which hive a response came from, not which person. Users should still be told not to put personal data in free text, since whatever they type is stored as written.

The promoter / passive / detractor category is not stored; it is derived from the score every time the data is read (1 = detractor, 2-3 = passive, 4 = promoter).

## Where it goes

1. The browser posts `{score, feedback}` to the spoke's own dashboard: `POST /api/feedback/nps`.
2. The spoke validates it and forwards `{hive_id, score, feedback, dashboard_version}` to the hub's `POST /api/nps/ingest`, authenticated with the same per-hive heartbeat bearer it already uses for `/api/heartbeat` and `/api/task-status`. The response reaches the hub immediately, not on the next heartbeat.
3. The hub stores it in `/data/hub-nps.json` on its data volume, as a rolling window: at most 1000 responses per hive and 10000 in total, oldest dropped first.

A hive with no hub link (a standalone install with no `hub.url`) sends nothing anywhere, even if NPS is enabled, unless its operator also configures the NPS relay; see [Standalone hives: the NPS relay](#standalone-hives-the-nps-relay).

## Standalone hives: the NPS relay

A standalone hive has no hub link to forward over. If its operator opts in, it can send responses to a hivecommons-operated relay instead, and the hivecommons hub pulls them from there into the same admin view. Tracking issue: [#9619](https://github.com/hivecommons/hive/issues/9619).

The relay path is used only when **all** of these hold:

- NPS is enabled (`hub.nps_enabled: true` or `HIVE_NPS_ENABLED=true`). For a standalone hive this is off by default, so nothing is sent until the operator turns it on.
- The hive has no hub link. A hive with a hub link always forwards to its hub and never uses the relay.
- A relay URL is configured (`hub.nps_relay_url` / `HIVE_NPS_RELAY_URL`). It defaults to empty, which disables the relay. It must be `https` (plain `http` is accepted only for a loopback host). The hivecommons relay is `https://docs.hivecommons.dev/api/nps`.

```bash
HIVE_NPS_ENABLED=true
HIVE_NPS_RELAY_URL=https://docs.hivecommons.dev/api/nps
```

That is all. There is no token to request and nothing for a hivecommons maintainer to do per hive.

**Self-registered install keys.** On the first response it forwards, the hive generates an Ed25519 keypair and a random install id (a UUID) and stores both in `/data/secrets/nps_relay_identity.json`, readable only by the hive process (mode `0600`). It then registers the public key with the relay (`POST <relay url>/register` with `{install_id, public_key, hive_version}`), proving it holds the private key by signing the request. Every submission after that is signed: the signature covers the install id, a timestamp, a random nonce and the SHA-256 of the exact body. The relay rejects a bad or missing signature, an unknown install id, a timestamp more than 5 minutes off, and a nonce it has already seen. If the relay answers that it does not know the install (for example after its store was reset), the hive registers the same key again, once, and retries. The private key is never sent anywhere, never logged, and never part of an error message. Deleting the identity file makes the hive start over as a new install.

A self-registered key proves **continuity of one install**, not that the install is a real or trusted hive: anyone can register a key. That is why relay responses are labeled "unverified install" in the hub admin view, and why the relay bounds what one caller can do (below) instead of trusting it.

**What the relay receives** is the same payload the hub path sends: `hive_id`, `score`, `feedback` and `dashboard_version`, with no user identity. The spoke applies the same validation, size caps and per-user / per-hive rate limits as on the hub path. The relay independently caps every body by bytes read, validates the score, the 500-character feedback limit and the hive id, accepts at most one response per client IP and 10 per install per 24 hours, limits registrations per client IP and in total per day, never lets a different key take over a registered install id, and keeps a rolling window of entries. It has **no public read path**.

**How the hub gets them.** A hub configured with the relay URL and a pull secret (`hub.nps_relay_pull_secret` / `HIVE_NPS_RELAY_PULL_SECRET`) pulls pending entries every 15 minutes, in batches of up to 100. Each entry is validated with the same rules as a direct response, stored with `source: relay` and the entry's `install_id`, and then acknowledged so the relay deletes it. The relay's stable entry id is kept as a dedupe key, so a pull that is retried after a failed acknowledgement never duplicates a response. An entry that claims the ID of a hive registered with the hub is discarded, since a hub-linked hive never uses the relay. Relay responses appear in the hub admin NPS card marked "unverified install" with their install id. With no relay URL or no pull secret, the hub does not pull.

## Who can see it

Only **hub admins**. `GET /api/admin/nps` is registered behind the hub's admin gate and is the only read path; a non-admin gets `403` with no data, and the hub dashboard's NPS card sits inside the admin-only section, so non-admins see no trace of it. The card shows:

- the overall NPS score (-100 to 100), promoter / passive / detractor counts and percentages, and the average score;
- a monthly trend;
- a per-hive breakdown;
- the 20 most recent responses, with their free text.

Hive owners do not see their hive's responses in the spoke dashboard.

## Turning it on or off

| Hive type | Default |
|-----------|---------|
| Hosted spoke (provisioned by the hub, `hub.hive_type: hosted`) | **on** |
| Self-hosted, federated, standalone | **off** |

Hosted spokes default on because they already run on the hub operator's infrastructure, so a response does not leave it. Every other install defaults off, following the rule that the hive never sends data off-box without an explicit operator opt-in.

Override with the config key or the environment variable (the variable wins):

```yaml
hub:
  nps_enabled: true   # or false
```

```bash
HIVE_NPS_ENABLED=false   # 1/true/yes/on or 0/false/no/off; anything else is ignored
```

The dashboard asks `GET /api/feedback/nps/status` whether to run the prompt, so a change takes effect on the next page load.

## Tuning the timing

Every timing rule above can be overridden per hive. Unset (or `0`) keeps the default, which is the console's value:

```yaml
hub:
  nps_timing:
    min_sessions: 2                         # 1-100
    second_session_engagement_seconds: 300  # 1-86400
    returning_engagement_seconds: 60        # 1-86400
    reprompt_days: 30                       # 1-365
    dismiss_retry_days: 7                   # 1-365
    max_dismissals: 3                       # 1-100
```

A negative or out-of-range value is rejected when the config loads. The spoke hands the effective values to the dashboard in `GET /api/feedback/nps/status` (only to a viewer who can submit), and the dashboard falls back to its built-in constants for anything missing or malformed. A change takes effect on the next page load; a snooze already stored in a browser keeps the length it had when it was set.

## GA4 funnel events (optional, off by default)

An operator can send three funnel events to a Google Analytics 4 property, to see how often the prompt is shown, answered and dismissed:

| Event | Parameters |
|-------|------------|
| `hive_nps_survey_shown` | none |
| `hive_nps_response` | `score` (1-4), `category`, `feedback_length` (number of characters) |
| `hive_nps_dismissed` | none |

**The free text is never sent to GA4**, only its length. No user identity is added either.

GA4 is off unless the operator sets a measurement ID:

```bash
HIVE_NPS_GA4_MEASUREMENT_ID=G-XXXXXXXXXX   # empty or malformed = off
```

It is an environment variable rather than a `hive.yaml` key because the dashboard document is served by the Node proxy in the combined image, which reads only its environment, and the proxy and the Go server must widen their Content Security Policy from the same source.

When the ID is unset nothing changes: no script is loaded and the CSP is byte-for-byte the same. When it is set:

- `gtag.js` is loaded from Google's tag host only when the card is about to appear, and only for a viewer who has not opted out. Automatic page views, Google signals and ad personalization are turned off, so the three events above are the only hits.
- The CSP adds `https://*.googletagmanager.com` to `script-src` / `script-src-elem`, and the GA4 collection hosts (`https://*.google-analytics.com`, `https://*.analytics.google.com`, `https://*.googletagmanager.com`) to `connect-src`. Nothing else is widened, and `/terminal` is untouched.
- The card says that it sends anonymous usage events and offers **Turn off** / **Turn on**, a per-browser opt-out stored in `localStorage` (`hive-analytics-opt-out-v1`). A browser sending Global Privacy Control or Do Not Track is treated as opted out, and so is a browser whose storage cannot be read. The very first "shown" event for a browser is sent before the viewer can see that notice; GPC or DNT prevents even that one.

## Public issue for detractors (optional, off by default)

A hive can let a detractor turn their feedback into a **public** issue:

```yaml
hub:
  nps_detractor_issues:
    enabled: true
    repo: my-org/my-repo   # owner/name; this hive's App must be installed there
```

When it is on, a user who picks 😠 Not great (score 1) sees an unticked checkbox under the text box: *Also open a public issue in my-org/my-repo with this feedback. Anyone can read it; your name is not included.* Only if they tick it, and after their NPS response has been accepted, the dashboard calls `POST /api/feedback/nps/issue`, and the spoke files the issue through the hive App's existing issue-creation path (the same one agents use, with its duplicate-title reuse and secret scrubbing). The spoke refuses the request unless:

- the user is signed in with write access and NPS is enabled for the hive;
- the score is exactly 1 and `consent` is exactly `true`;
- the feedback has at least 20 characters after trimming (a one-word complaint is not actionable in a public tracker);
- the user has not opened one in the last 24 hours, and the hive has opened fewer than 3 in the last 24 hours.

The issue body quotes the feedback and says it was filed with the user's consent. It carries no username and no hive ID, and every `@mention` in the title and body is neutralized so filing it notifies nobody. No labels are added. A `hive.yaml` with `enabled: true` and no valid `owner/name` repo fails validation; a hive with no forge client never offers the checkbox.

## Security properties

Each of these is covered by tests in `src/pkg/dashboard/nps_test.go`, `nps_relay_test.go`, `nps_ui_test.go`, and `src/pkg/hub/nps_test.go`, `nps_relay_test.go`.

- **No spoofed scores** (console #13664, #13758). The hub accepts a response only with the per-hive bearer derived for the `hive_id` in the body, from a hive in its registry. A bearer for another hive, the retired fleet-wide bearer, or no bearer gets `401`. A hub with no master secret refuses all ingest.
- **Rate limits.** The spoke allows one response per signed-in user and 10 per hive per 24 hours; the hub independently allows 10 per hive per 24 hours, counted from its durable store.
- **No unauthenticated read path** (console #16486). Totals and free text are served only to hub admins.
- **Body caps on bytes read** (console #16666). Both the spoke (4096 bytes) and the hub (4096 bytes) cap the bytes they actually read, so a chunked body or a false `Content-Length` cannot make them buffer more. Free text is capped at 500 characters on both sides.
- **Relay signatures and pull secret** (#9619). The relay accepts a submission only when it is signed by the key registered for its install id, within the timestamp window and with an unused nonce, and serves pending entries only to the hub's pull secret. The spoke's private key never leaves its `0600` identity file and is never logged; the hub sends its pull secret only to the configured `https` relay URL and never logs it. Neither side follows redirects. A hub drops any relay entry that claims a hub-registered hive's ID, and labels the rest "unverified install".
- **No stored XSS** (console #17030). Both the dashboard card and the hub admin card build their DOM with `textContent` only; tests execute the hub renderer against hostile input and fail on any use of `innerHTML`.
- **No free text in analytics.** Tests execute the dashboard's GA4 helpers against a fake browser and fail if any payload carries the text, if anything loads without a measurement ID, or if an opted-out browser sends an event.
- **Detractor issues need consent.** Tests cover the off-by-default switch, the consent flag, the score and length checks, mention neutralization and the rate limits (`src/pkg/dashboard/nps_extras_test.go`).

## Not yet implemented

- **A dedicated relay site.** The relay runs as a function on the docs site. Moving it to its own site is a follow-up.
- **Verified installs.** Self-registered keys are unverified by design. Tying a relay install to a verified identity (for example a GitHub App installation) is a possible follow-up.
