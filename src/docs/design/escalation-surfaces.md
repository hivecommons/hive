# Escalation surfaces: email and push, the terminus of decision routing

Status: **Partly shipped on `v6`** — Track 6 of the epic
[#7563](https://github.com/hivecommons/hive/issues/7563). The outbound half
landed in [#7618](https://github.com/hivecommons/hive/pull/7618)
(tracker [#7613](https://github.com/hivecommons/hive/issues/7613)):
`src/pkg/escalate` carries a severity-routed dispatcher, an SMTP email sink
with the daily digest, and ntfy / Pushover / PagerDuty push sinks. The
**inbound reply-to-act path is not built** — no IMAP poller, no signed reply
parsing, no sender allowlist — as this design intended, inbound last and off
by default. Nothing here changes v4 or v5 behaviour: `pkg/escalate` exists on
the `v6` branch only. Builds on the chat spine (`pkg/chat`, merged on v6) and
the HUMAN DECISION NEEDED routing work (#7515/#7536).

Code references were checked against `v4` at `15d99a45` (and `v6` for
`pkg/chat`). Line numbers drift; the named symbols are the stable handles.

---

## Problem

When a review verdict says *a human must decide*, Hive can currently say so in
three places: the dashboard, a GitHub label (`review.human_decision_label`,
an operator-configured existing repo label — per-hive, no default), and — as
of the v6 chat work — a Discord/Slack channel. All three assume the human is
already looking. The escalation cases that matter are precisely the ones
where nobody is looking:

- a `requires_human` verdict sits in the queue over a weekend;
- a long-running run has waited on a human checkpoint past
  `runs.wait_timeout_seconds`;
- the governor halts on budget exhaustion at 03:00;
- a spoke's App token loses a permission and every kick starts failing.

Chat is a *presence* surface. Escalation needs *interrupt* surfaces: email
(slow, rich, universal) and push/on-call (fast, terse, paging semantics).
They are the logical terminus of #7515's routing: a decision that would wait
in a queue instead lands in front of a person.

## Shape: escalation is fan-out, not a new pipeline

The spine's notifier (`pkg/chat` `Service.diffAgents` / `diffGovernor` on the
dashboard SSE feed) already turns dashboard state transitions into
human-readable lines. Escalation surfaces subscribe to the **same events**,
filtered by severity, through a small `pkg/escalate` fan-out:

```go
type Event struct {
        Severity  Severity  // info | decision | page
        Title     string    // "HUMAN DECISION NEEDED: hive/xyz#123"
        Body      string    // pre-scrubbed, pre-composed by the producer
        Link      string    // dashboard or GitHub deep link
}

type Sink interface {
        Name() string
        Deliver(ctx context.Context, ev Event) error
}
```

Producers: the review publisher (verdict `requires_human`), the governor
(mode transitions to halted/paused), agent failure streaks — each already has
a well-defined site where the GitHub label / chat message is emitted today.
The producer composes and **scrubs** the text once (same canary/secret scrub
as every outbound surface); sinks never re-compose.

Severity is the routing key, set by the producer:

| Severity | Meaning | Email | Push |
|---|---|---|---|
| `info` | digest material | daily digest only | never |
| `decision` | a human must act, not urgently | immediate mail | configurable |
| `page` | operations are stopped | immediate mail | always |

## Surface 1: email

### Outbound (phase 1)

Plain SMTP submission — `net/smtp` with STARTTLS/implicit-TLS, no third-party
mail dependency. Works from pull-only clusters (outbound 465/587 only).

```yaml
email:
  enabled: false
  smtp:
    host: smtp.example.com
    port: 587
    username: hive@example.com
    password: ${HIVE_SMTP_PASSWORD}
  from: hive@example.com
  to: [ops@example.com]        # escalations
  digest:
    to: [team@example.com]      # may differ from escalation recipients
    at: "08:00"                 # local hive time, one mail/day, skipped if empty
```

- **Escalation mail** (`decision`/`page`): one event per mail, subject is
  `Event.Title`, body is `Event.Body` + `Event.Link`. Immediate, no batching —
  batching defeats the purpose.
- **Digest mail** (`info` + a summary of the day's `decision` events and
  their outcomes): one per day at the configured time. This is the "what did
  the hive do" mail an operator reads with coffee, assembled from the same
  audit events the dashboard timeline shows.
- Plain text first; HTML alternative part later if anyone asks. Every mail
  ends with a provenance footer (hive name, spoke, version) so multi-hive
  operators can filter.

### Inbound reply-to-act (phase 2, explicitly later)

Mirrors the Linear inbound model (`pkg/linearagent`: guards → ack → kick,
decoupled): an IMAP poller (outbound-only, fits pull-only clusters) watches a
dedicated mailbox; a reply to an escalation mail can carry exactly the verbs
the mail offered (`approve`, `deny`, `kick <agent>: <prompt>`).

The guard set is the mention-trigger set (#7483) transplanted, fail closed:

- **sender allowlist** — exact addresses, empty = inbound disabled entirely;
- **authenticity** — require DKIM-aligned `From` at minimum; the config
  documents that an operator who cannot guarantee SPF/DKIM on their inbound
  path must leave inbound disabled. Address spoofing is trivial; an
  allowlist without authenticity is decoration;
- **thread binding** — the reply must reference (In-Reply-To / subject token)
  a mail Hive actually sent, single-use, expiring;
- **ioscan** — body passes `ioscan.EnforceInput`
  (`src/pkg/ioscan/enforce.go:24`) before any text reaches a kick;
- **no oracle** — unauthorized mail is dropped and audited, never answered;
- **rate caps** — per-sender and global, same shape as mention triggers.

Inbound email is the weakest-authenticity surface Hive will have; it ships
last, defaults off, and its docs say why.

## Surface 2: push / on-call

Outbound-only, three providers behind one tiny sink each — chosen because
together they cover hobbyist → team → enterprise without an SDK:

| Provider | Transport | Auth | Fit |
|---|---|---|---|
| **ntfy** | `POST https://ntfy.sh/<topic>` (or self-hosted) | optional token | self-hosters; zero account |
| **Pushover** | `POST /1/messages.json` | app+user token | solo operator's phone |
| **PagerDuty** | Events API v2 `enqueue` | routing key | teams with real on-call |

```yaml
push:
  enabled: false
  min_severity: page          # decision | page
  ntfy:      { url: "", token: "" }
  pushover:  { app_token: "", user_key: "" }
  pagerduty: { routing_key: "" }
```

All three are a single HTTPS POST with a JSON/form body — `pkg/escalate`
implements them directly (~50 lines each), no dependencies. `page` events map
to PagerDuty `critical` / Pushover `priority=1` / ntfy `priority=high`.
Delivery failures are logged and audited but never block the producer: an
escalation sink that is down must not stall a review pipeline. No retries
beyond one immediate re-attempt; the mail sink is the durable fallback.

## The guard invariant, restated

Per the epic, no surface grows its own authz:

- **Outbound** carries only text the producer already scrubbed; sinks add
  nothing and cannot widen what is said.
- **Inbound** (email phase 2 only) routes through the same machinery as every
  other trigger: allowlist fail-closed, `ioscan.EnforceInput`, kicks via the
  same `SendKickWithSource` path (`source=email`) the mention trigger uses,
  audited symmetrically (`agent_email_kicked` / `agent_email_declined`).
  A mail can never reach an agent or capability the mode ladder would deny.
- Secrets (SMTP password, provider tokens) come from env expansion, never
  committed — same rule as `slack.app_token`.

## Phasing

1. **PR A — `pkg/escalate` + severity plumbing**: Event/Sink, producer wiring
   at the `requires_human` publish site and governor transitions, config +
   validation. A `chat` sink adapter so Discord/Slack get `page` events even
   when the status diff would have coalesced them.
2. **PR B — email outbound**: SMTP sink + daily digest assembler.
3. **PR C — push**: ntfy, Pushover, PagerDuty sinks.
4. **PR D (later, separate review) — email inbound**: IMAP poller + guard
   set. Ships only with the full mention-trigger guard parity.

Each PR meets the 90% per-package coverage floor (aim 92%+); SMTP/IMAP/HTTPS
sinks are tested against local fakes (`net/smtp` test server, httptest).

## What this deliberately does not do

- No webhook *receiver* for provider callbacks (PagerDuty ack-back etc.) —
  outbound-only keeps the pull-only story intact.
- No templating language for mail bodies; producers compose, full stop.
- No SMS provider (Twilio) — Pushover/ntfy cover the phone story without
  per-message billing; revisit only on demand.
- No per-user notification preferences; recipients are operator config,
  not profiles. The dashboard remains the richest surface — these are
  interrupts, not a second dashboard.
