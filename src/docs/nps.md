# NPS feedback prompt

The hive dashboard can occasionally ask a signed-in user one question: *how is the hive working for you?* The answer is a 4-point score, optionally with a sentence of free text. Responses go to the hub, where only hub admins can read them. This page covers what is collected, where it goes, who can see it, and how to turn it on or off. Tracking issue: [#9610](https://github.com/hivecommons/hive/issues/9610).

The prompt is modeled on the one KubeStellar Console ships (`useNPSSurvey.ts`), including its timing and backoff, and it avoids the security problems that implementation shipped with (see [Security properties](#security-properties)).

## What the user sees

- A small card in the bottom-left corner with four options: 😠 Not great (1), 😐 Meh (2), 🙂 Good (3), 😍 Love it (4).
- After picking one, an optional free-text box whose prompt depends on the answer (what is not working / what would make it better / what they like most), capped at 500 characters, and a **Send** button.
- A **×** (or Escape) dismisses it. After sending, a short thank-you replaces the card.

The card is shown only when all of these hold:

- NPS is enabled for this hive (see [Turning it on or off](#turning-it-on-or-off)) and the hive has a hub link.
- The viewer is signed in with write access. Anonymous visitors, public snapshots and read-only users never see it.
- The per-browser timing rules allow it:

| Rule | Value |
|------|-------|
| First prompt | not before the 2nd browser session |
| Engaged time before prompting | 5 minutes in the 2nd session, 1 minute from the 3rd session on |
| After a response | wait 30 days |
| After a dismissal | wait 7 days |
| After 3 dismissals | wait 30 days |
| Per page load | at most once |

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

A hive with no hub link (a standalone install with no `hub.url`) sends nothing anywhere, even if NPS is enabled. A relay for standalone hives is planned separately; see [Not yet implemented](#not-yet-implemented).

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

## Security properties

Each of these is covered by tests in `src/pkg/dashboard/nps_test.go`, `nps_ui_test.go`, and `src/pkg/hub/nps_test.go`.

- **No spoofed scores** (console #13664, #13758). The hub accepts a response only with the per-hive bearer derived for the `hive_id` in the body, from a hive in its registry. A bearer for another hive, the retired fleet-wide bearer, or no bearer gets `401`. A hub with no master secret refuses all ingest.
- **Rate limits.** The spoke allows one response per signed-in user and 10 per hive per 24 hours; the hub independently allows 10 per hive per 24 hours, counted from its durable store.
- **No unauthenticated read path** (console #16486). Totals and free text are served only to hub admins.
- **Body caps on bytes read** (console #16666). Both the spoke (4096 bytes) and the hub (4096 bytes) cap the bytes they actually read, so a chunked body or a false `Content-Length` cannot make them buffer more. Free text is capped at 500 characters on both sides.
- **No stored XSS** (console #17030). Both the dashboard card and the hub admin card build their DOM with `textContent` only; tests execute the hub renderer against hostile input and fail on any use of `innerHTML`.

## Not yet implemented

- **Relay for standalone hives.** Hives with no hub link, which default off, have no path to the hub yet. The plan is a hivecommons-operated relay (per-IP rate limit, signed per-install token), with the hub merging its responses. Until then, enabling NPS on such a hive has no effect.
- **Optional public issue for detractors.** The console can open a GitHub issue from a detractor's feedback with explicit consent. The hive has no existing feedback-to-issue path to reuse, so this is deferred. When built, it will need an explicit consent checkbox, a minimum text length, and the mention sanitizer.
- **GA4 events.** The v6 dashboard has no GA4 wiring, so the `hive_nps_survey_shown` / `hive_nps_response` / `hive_nps_dismissed` events are deferred. When added, they carry only score, category and feedback length, never the free text.
