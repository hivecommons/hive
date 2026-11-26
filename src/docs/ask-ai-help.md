# Ask AI help

Ask AI help is an optional **Ask** panel that answers questions about using the
hub and hive dashboards ("where do I pause an agent?") from the Hive docs, with
the sources it used and an "AI generated, may be wrong" label. In Settings and
in the Help menu it is called **Ask Hive**. It is **on by default on a hosted
hive** (hub mode, through the hub relay, no key on the hive) and **off by
default on a self-hosted hive**, which needs its own key. Either way each person
confirms a notice before their first question. The design, threat model and
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

## A hosted hive: on by default

A hosted hive (`hub.hive_type: hosted`, with `hub.url` and `hive_id` set) has
Ask on in hub mode when `hive.yaml` says nothing about `ask.enabled`. Nothing
needs to be configured on the hive; it works once the hub operator has Ask on
at the hub ([Ask AI help on the hub](hub-deployment.md#ask-ai-help)). Settings,
Features shows **Ask Hive** on, with "On by default for this hosted hive".

To turn it off, the hive owner switches **Ask Hive** off in **Settings,
Features**, which saves `ask.enabled: false`, or sets any of:

```yaml
ask:
  enabled: false   # or: mode: off
```

`HIVE_ASK_ENABLED=false` or `HIVE_ASK_MODE=off` does the same and wins over
`hive.yaml`. The hub operator can also refuse a single hive with
`HIVE_ASK_RELAY_DISABLED_HIVES`, or every hive with `HIVE_ASK_RELAY_HIVES=none`.
A hosted hive with no hub link or no hive id gets no default; if Ask is
switched on there, Settings says why it stays off.

## Turning it on for a self-hosted hive

A self-hosted hive is **off by default**: it has no vendor key, and the hub
relay does not serve registered self-hosted hives in this round (#19177). A
self-hosted hive (no hub, or registered with a hub) calls Kapa.ai itself with
its own key:

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
4. Settings, Features shows **Ask Hive** on, or the reason it stays off.

The hive applies the same limits as the hub to its own traffic; a busy shared
vendor quota shows "Busy, try again in N seconds" and the docs link.

## Using the panel

With Ask on, the hive dashboard shows **Ask Hive** in the Help menu, right
after Getting started: in the top bar **? Help** menu and in the sidebar
**Help** group. The header also keeps its **Ask** button, and the hub dashboard
has that button only, since its header has no Help menu. Either opens a panel,
titled **Ask Hive**, docked to the side (a bottom sheet on a phone). Choosing
Ask Hive closes the Help menu (and the navigation drawer on a phone) first; it
works with a tap, a click, or Enter and Space from the keyboard. The first time, it shows the
notice and **I understand, continue**; after that the person types a question
(Enter to send, Shift+Enter for a new line) and gets an answer, with
paragraphs, lists and code blocks laid out but never rendered as HTML, a
**Sources** list and the "AI generated, may be wrong" label. The label of the
section in view, or of the open Settings tab, goes with the question. Every
state has a docs link for the section in view. Escape closes the panel and
returns focus to the **Ask** button; answers and errors are announced to
screen readers. With Ask off, or when the backend says Ask is not on for the
person, neither the button nor the Help menu item is shown, and with Ask off
the page loads nothing for it.

## Configuration keys

| Key | Env | Default | Meaning |
|---|---|---|---|
| `ask.enabled` | `HIVE_ASK_ENABLED` | on for a hosted hive, off otherwise | The owner or operator switch. It makes the panel available; each person still consents. An explicit `false` always wins. |
| `ask.mode` | `HIVE_ASK_MODE` | `hub` for a hosted hive, `off` otherwise | `off`, `direct` or `hub`. `off` keeps Ask off even on a hosted hive. |
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
   button, and `kubectl -n hive-hub logs deploy/hive-hub -c hub` shows no `ask:` line.
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
5. **Hosted hive.** On a hosted hive with no `ask` block, Settings, Features
   shows **Ask Hive** on and **Ask mode** `hub` by default. Open **? Help**, then **Ask Hive**, confirm, and
   ask "Where do I pause an agent?". Expect an answer linking to the dashboard sections or agents
   docs. The hub log shows `audit: ask request` with `hive=<that hive>`; the hive
   log shows `audit: ask request` with `relay=true`. Neither line carries the
   question.
6. **Self-hosted hive.** Configure direct mode as above, restart, and ask the
   same question. The hive log shows `ask: enabled mode=direct`.
7. **Fallback.** On the self-hosted hive point `ask.kapa.api_key_file` at an
   empty file and restart: Settings shows why Ask stays off and the button is
   gone. Restore it.
8. **Off again.** Turn **Ask Hive** off on the hosted hive (this saves
   `ask.enabled: false`) and turn Ask off on the hub. The button
   disappears on the next reload, the routes answer `404`, and the hub's
   `GET /api/ask/stats` (while on) showed the counts by outcome for the test.
9. **What left.** In Kapa analytics, the test questions appear with the section
   label prefix (`Dashboard section: ...`) and nothing else: no login, URL or
   hive name.

