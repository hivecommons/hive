# State-Triggered Hooks

Hive provides a declarative, state-triggered **Hooks** extension point (`pkg/hooks`). Hooks allow operators to attach custom actions (notifications, scripts, pacing adjustments, audit records) to well-defined lifecycle and governor state transitions without modifying core engine code.

---

## 1. Overview

Today, Hive emits rich events across agent lifecycles, governor evaluation loops, and PR sweeps. State-triggered hooks provide a single, auditable, and testable mechanism to define **"what happens when X occurs"**.

Key characteristics:
- **Opt-in & Safe by Default**: Hooks are disabled unless explicitly configured (`hooks.enabled: true`).
- **Secret-Scrubbed**: Script and command outputs are automatically filtered through `logscrub` before being logged or surfaced.
- **Durable Audit Trail**: Every hook trigger and action result is recorded in the Audit Log under action `hook_fired`.
- **Side-Effect Bounded**: External scripts run with strict context timeouts to prevent blocking governor cycles or agent turns.

---

## 2. Named State Transitions

The following named transitions can be targeted by hook rules:

| Transition Name | Trigger Condition |
| :--- | :--- |
| `on_agent_started` | An agent session/process successfully starts or relaunches |
| `on_agent_stopped` | An agent session is stopped |
| `on_agent_paused` | An agent is paused by operator, circuit breaker, trajectory drift, or tool approval gate |
| `on_agent_resumed` | An agent is resumed from a paused state |
| `on_agent_failed` | An agent process crashes or fails launch validation |
| `on_agent_kicked` | An agent receives an execution prompt / task |
| `on_acmm_change` | The active ACMM maturity level changes (e.g. L3 → L4) |
| `on_sweep_merge` | A pull request is merged by the auto-merge sweep |
| `on_stall_detected` | The governor watchdog detects an inactive/stalled agent |
| `on_pr_opened` | An agent opens a pull request |
| `on_issue_opened` | An agent creates an issue |
| `on_turn_start` | An agent initiates a re-entrant turn |
| `on_turn_complete` | An agent completes a re-entrant turn |
| `on_tool_approval_required` | A side-effectful tool halts pending operator approval (#4000) |
| `on_agent_*` | Wildcard prefix matching all agent lifecycle transitions |
| `*` | Catch-all matching all state transitions |

---

## 3. Configuration & Syntax

Hooks are declared under the `hooks:` block in `hive.yaml`:

```yaml
hooks:
  enabled: true
  rules:
    # Send a notification when any agent is paused
    - name: notify-on-agent-pause
      on: on_agent_paused
      action: notify
      notify:
        title: "Agent Paused: {{.Agent}}"
        message: "Agent {{.Agent}} paused at ACMM L{{.ACMMLevel}} because {{.Reason}} (trigger: {{.Trigger}})"
        webhook_url: "https://discord.com/api/webhooks/..."

    # Run a cleanup or deployment script after a PR is auto-merged
    - name: post-merge-sync
      on: on_sweep_merge
      action: script
      script:
        command: "./scripts/on-merge.sh"
        args: ["{{.Metadata.repo}}", "{{.Metadata.pr_number}}"]
        timeout_s: 30

    # Backoff agent pacing if a stall is detected
    - name: throttle-on-stall
      on: on_stall_detected
      action: pacing
      pacing:
        multiplier: 2.0
        cadence: "slow"

    # Explicit audit entry on ACMM level promotion/demotion
    - name: audit-acmm-transition
      on: on_acmm_change
      action: audit
      audit:
        message: "ACMM maturity level transitioned to L{{.ACMMLevel}} (reason: {{.Reason}})"
```

---

## 4. Supported Actions

### 1. `notify`
Dispatches formatted notifications to Discord webhooks, Slack channels, or the built-in notification hub.
- `title`: Notification title (supports Go templating).
- `message`: Notification body (supports Go templating).
- `channel`: Target channel identifier (optional).
- `webhook_url`: Dedicated webhook URL (optional, falls back to default alert sink).

### 2. `script`
Executes a localized shell script or binary command in an isolated child process.
- `command`: Command or executable path.
- `args`: Arguments list (supports Go templating).
- `timeout_s`: Max execution time in seconds (defaults to 30s).
- *Output*: Standard output and error are captured, passed through `logscrub` to scrub any credential tokens, and saved in the audit log.

### 3. `pacing`
Directly adjusts governor evaluation cadences and agent dispatch intervals.
- `agent`: Target agent name (defaults to triggering agent).
- `multiplier`: Cadence scaling factor (e.g. `2.0` doubles the wait duration).
- `cadence`: Named cadence override (`fast`, `normal`, `slow`).

### 4. `audit`
Emits an explicit, searchable record to Hive's durable Audit Log without external side effects.
- `message`: Audit log detail text.

---

## 5. Event Context & Templating

Hook fields support standard Go text templating (`{{.Field}}`). The following context variables are available:

- `{{.Transition}}`: Name of the transition event (e.g. `on_agent_paused`).
- `{{.Agent}}`: Responsible agent name (e.g. `architect`, `quality`).
- `{{.ACMMLevel}}`: Current ACMM maturity level (integer `1`–`6`).
- `{{.Reason}}`: Explanatory reason string.
- `{{.Trigger}}`: Originator of the event (`operator`, `governor`, `trajectory-review`, `breaker`).
- `{{.Metadata.<key>}}`: Custom dictionary fields (e.g. `{{.Metadata.repo}}`, `{{.Metadata.pr_number}}`).
- `{{.Timestamp}}`: UTC timestamp of the event.
