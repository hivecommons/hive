# Ask AI help

Ask AI help is an optional **Ask** panel that answers questions about using the
hub and hive dashboards ("where do I pause an agent?") from the Hive docs, with
the sources it used and an "AI generated, may be wrong" label. It is off by
default everywhere and inert until configured. The design, threat model and
isolation argument are in
[Dashboard Ask AI help](design/dashboard-ask-ai-help.md) (#19177).

The browser only ever talks to its own dashboard. The dashboard's backend
calls the Kapa.ai Chat API server side: there is no third-party script on the
page and no Content-Security-Policy change, on or off.

## What leaves, and what does not

Per question, and only after the person has confirmed a short notice:

- the question text (refused, not sent, when it looks like a password, token or
  key);
- the label of the dashboard section the person is in, from a fixed list of
  section names in `pkg/askai/sections.go` (turn it off with
  `ask.send_section_label: false`);
- the Kapa.ai integration id and, for a follow-up, the vendor thread id, which
  is kept on the server behind an opaque handle.

Never sent: the person's login, email or role, page content, the URL, hive,
repo, agent or customer names, tokens or cookies. Hive keeps no question or
answer text; audit lines carry outcomes and counts only. Each person can
withdraw with **Stop sending my questions**; that cannot recall what Kapa.ai
already holds, and the notice says so.

## Modes

| Where | `ask.mode` | Who calls Kapa.ai | Key on this process |
|---|---|---|---|
| The hub | always `direct` | the hub | yes, from a Secret |
| A hosted hive | `hub` | the hub, through `POST /api/hub/ask/relay` | no |
| A self-hosted hive | `direct` | the hive | yes, from a Secret |

A hosted hive in hub mode authenticates to the relay with the per-hive
heartbeat bearer it already has; no new secret is issued and the key never
leaves the hub. The hub applies a per-hive hourly quota and its own hub-wide
limits, and a hub operator can turn the relay off (`HIVE_ASK_RELAY_HIVES=none`)
or refuse single hives (`HIVE_ASK_RELAY_DISABLED_HIVES`). In this round the
relay serves hosted hives only; a self-hosted hive registered with a hub uses
direct mode with its own key, or stays off.

## Turning it on for a hosted hive

1. The hub operator turns Ask on at the hub ([Ask AI help on the
   hub](hub-deployment.md#ask-ai-help)).
2. The hive owner opens **Settings, Features**, turns on **Ask AI help** and
   sets **Ask mode** to `hub`, or sets in `hive.yaml`:

   ```yaml
   ask:
     enabled: true
     mode: hub
   ```

   `HIVE_ASK_ENABLED` and `HIVE_ASK_MODE` override these.
3. If the hive is not hosted, or has no hub link, Settings says why Ask stays
   off.

## Turning it on for a self-hosted hive

A self-hosted hive (no hub, or registered with a hub, which the relay does not
serve in this round) calls Kapa.ai itself with its own key:

1. In the Kapa project, create an HTTP API integration and an API key.
2. Put the key in a file under `/secrets` or `/data/secrets`, from a Secret
   (Kubernetes) or a read-only bind mount (Docker Compose, Podman). For
   Kubernetes:

   ```sh
   kubectl -n <hive-namespace> create secret generic hive-ask-kapa \
     --from-file=api-key=<path-to-key-file>
   ```

   and mount it at `/secrets/kapa` in the hive container.
3. Set in `hive.yaml` (or the matching `HIVE_ASK_*` env):

   ```yaml
   ask:
     enabled: true
     mode: direct
     kapa:
       project_id: <project id>
       integration_id: <integration id>
       api_key_file: /secrets/kapa/api-key
   ```

   The project id and integration id are configuration, never committed to a
   repository; the config export redacts them.
4. Settings, Features shows **Ask AI help** on, or the reason it stays off.

The hive applies the same limits as the hub to its own traffic; a busy shared
vendor quota shows "Busy, try again in N seconds" and the docs link.

## Using the panel

With Ask on, the dashboard header shows an **Ask** button. It opens a panel
docked to the side (a bottom sheet on a phone). The first time, it shows the
notice and **I understand, continue**; after that the person types a question
(Enter sends, Shift+Enter adds a line) and gets a plain-text answer with a
**Sources** list and the "AI generated, may be wrong" label. The label of the
section in view, or of the open Settings tab, goes with the question. Every
state has a docs link for the section in view. Escape closes the panel and
returns focus to the button; answers and errors are announced to screen
readers. With Ask off the button is not shown and the page loads nothing for
it.

## Configuration keys

| Key | Env | Default | Meaning |
|---|---|---|---|
| `ask.enabled` | `HIVE_ASK_ENABLED` | off | The owner or operator switch. It makes the panel available; each person still consents. |
| `ask.mode` | `HIVE_ASK_MODE` | `off` | `off`, `direct` or `hub`. |
| `ask.kapa.project_id` | `HIVE_ASK_KAPA_PROJECT_ID` | unset | Direct mode only. Never logged or returned by an API; redacted in the config export. |
| `ask.kapa.integration_id` | `HIVE_ASK_KAPA_INTEGRATION_ID` | unset | Direct mode only. Same handling. |
| `ask.kapa.api_key_file` | `HIVE_ASK_KAPA_API_KEY_FILE` | unset | Direct mode only. A file under `/secrets` or `/data/secrets`, mounted from a Secret. |
| `ask.send_section_label` | `HIVE_ASK_SEND_SECTION_LABEL` | on | Send the closed-list section label with the question. |
| `ask.limits.person_per_minute`, `ask.limits.person_per_hour` | `HIVE_ASK_LIMIT_PERSON_PER_MINUTE`, `HIVE_ASK_LIMIT_PERSON_PER_HOUR` | 3, 20 | Per person, with one question in flight. |
| `ask.limits.global_per_minute`, `ask.limits.in_flight`, `ask.limits.daily` | `HIVE_ASK_LIMIT_GLOBAL_PER_MINUTE`, `HIVE_ASK_LIMIT_IN_FLIGHT`, `HIVE_ASK_LIMIT_DAILY` | 4, 2, 500 | Process-wide. The per-minute cap is a fifth of Kapa.ai's 20 per minute per-team limit and is never allowed above half of it. |
| `ask.limits.hive_per_hour` | `HIVE_ASK_LIMIT_HIVE_PER_HOUR` | 60 | Read by the hub: relayed questions per hive per hour. |

If `ask.enabled` is on but a value is missing or the key file is unreadable,
Ask stays off, Settings shows the reason, and one warning line is logged that
names no value.

## Failure and fallback

Every state keeps a plain docs link: not consented, Kapa.ai or the hub
unreachable or busy (`429` backs callers off for its `Retry-After`), the circuit
breaker open after repeated failures, a refused question, or an answer with no
source on the allowlist (`docs.hivecommons.dev` and the docs path of
`hivecommons/hive` on GitHub), which is shown as "No cited answer found in the
docs" rather than as an answer. A hive whose relay is refused never falls back
to a key it does not hold.

## API

Hive dashboard: `GET /api/ask/status`, `PUT` and `DELETE /api/ask/consent`,
`POST /api/ask` (see `dashboard/openapi.json`); every route answers `404` while
Ask is off. Hub: the same routes plus `GET /api/ask/stats` and the relay, in
the [hub API reference](api-reference/hub-saas.md).

## Verifying a rollout

Run these after the Kapa key exists and Ask is switched on, first on the hub,
then on one hosted hive, then on one self-hosted hive. Use a test account that
has consented; nothing here needs a production change beyond the switch.

1. **Hub, off first.** With `HIVE_ASK_ENABLED` unset, `GET /api/ask/status` and
   `GET /ask-panel.js` on the hub answer `404`, the dashboard has no **Ask**
   button, and `kubectl logs` for the hub shows no `ask:` line.
2. **Hub, on.** Set the env and the Secret ([hub
   steps](hub-deployment.md#ask-ai-help)), restart, and check the log for
   `ask: enabled` (and no `ask: requested but staying off`). Open My Hives, click
   **Ask**, confirm the notice, and ask "Where do I add a hosted hive?". Expect an
   answer with at least one source on `docs.hivecommons.dev` and the AI label.
3. **Refusal.** Ask a question containing a fake token such as
   `ghp_` followed by 30 letters. Expect "This looks like it contains a password,
   token or key" and, in the Kapa analytics, no new question.
4. **Withdraw.** Click **Stop sending my questions**, ask again, and expect the
   notice instead of an answer.
5. **Hosted hive.** On a hosted hive, Settings, Features: turn on **Ask AI help**
   and set **Ask mode** to `hub`. Open **Ask**, confirm, and ask "Where do I pause
   an agent?". Expect an answer linking to the dashboard sections or agents
   docs. The hub log shows `audit: ask request` with `hive=<that hive>`; the hive
   log shows `audit: ask request` with `relay=true`. Neither line carries the
   question.
6. **Self-hosted hive.** Configure direct mode as above, restart, and ask the
   same question. The hive log shows `ask: enabled mode=direct`.
7. **Fallback.** On the self-hosted hive point `ask.kapa.api_key_file` at an
   empty file and restart: Settings shows why Ask stays off and the button is
   gone. Restore it.
8. **Off again.** Turn Ask off on the hosted hive and on the hub. The button
   disappears on the next reload, the routes answer `404`, and the hub's
   `GET /api/ask/stats` (while on) showed the counts by outcome for the test.
9. **What left.** In Kapa analytics, the test questions appear with the section
   label prefix (`Dashboard section: ...`) and nothing else: no login, URL or
   hive name.

