# Trajectory-Review Lane

The **trajectory-review lane** is a safety layer (on by default) that periodically reads
each running agent's recent transcript and asks a second model whether the
*sequence* of actions is still working toward the agent's assigned intent. If
the trajectory has diverged — individually-innocuous steps assembling toward an
outcome the intent does not authorize — the lane pauses the agent (or alerts)
and records an audit entry.

It targets **trajectory-level goal drift**: the failure mode where no single
action trips an allow/deny check, but the arc of the session heads somewhere it
should not (escaping a sandbox, evading a scanner, splitting a credential to
avoid detection, escalating privileges, or adopting a goal taken from repo
content instead of the operator's instruction). Per-action gating cannot see
this; only looking at the direction of the whole sequence can.

## How it works

The lane runs off the governor tick, after the eval cycle (so each agent's
intent — its last kick — is current), gated by its own cadence:

1. Snapshot every **running, non-paused, non-exempt** agent, with its assigned
   intent (last kick message) and a tail of its live tmux transcript.
2. For each, send `{intent, transcript-tail}` to the reviewer model via the
   hive's existing LiteLLM endpoint and get back a compact verdict:
   `{divergent, confidence, reason}`.
3. On a divergent verdict, apply the configured response: **pause** the agent
   and raise an error alert + notification, or **alert** only.

The reviewer is instructed to judge *direction*, not any single line, and to
default to non-divergent when unsure — so ordinary engineering work (reading
code, running tests, opening a PR that matches the intent) is never paused on a
low-signal read.

### Fail-open by design

A reviewer outage (endpoint down, timeout, unparsable reply) yields a
non-divergent verdict: a broken reviewer must never pause a working fleet. Each
review has its own 30-second timeout so the lane cannot stall the governor.

### Cost control

Only a bounded transcript tail is sent (default 120 lines, hard-capped at
24 000 chars), at `temperature=0` with a 200-token cap, on a cadence you set
(default every 600 s, floored by the governor eval interval). A cheap reviewer
model is appropriate — this is a divergence classifier, not a coding model.

## Configuration

**Dashboard:** Governor Config → **General** tab has a **Trajectory Review**
toggle (on by default) with an info tooltip explaining the check. Toggling it
persists immediately and takes effect on the next hive restart. The review
model, cadence, and exemptions are set in `hive.yaml` (below).

**hive.yaml** — under `governor.trajectory`:

```yaml
governor:
  litellm:
    endpoint: https://litellm.example.com
    api_key_env: HIVE_LITELLM_API_KEY
    default_model: claude-haiku-4-5
  trajectory:
    enabled: true                 # ON by default; set false to disable
    interval_s: 600               # review cadence (floored by eval_interval_s)
    model: claude-haiku-4-5       # reviewer model; empty → litellm.default_model
    transcript_lines: 120         # trailing lines sent per agent
    on_divergence: pause          # "pause" (default) or "alert"
    exempt_agents:                # never reviewed
      - quality                   # advisory-only agent that opens no PRs
```

| Field | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Turn the lane on/off. Also on the Governor → General tab. No reviews run (zero cost) until a LiteLLM reviewer endpoint is configured. |
| `interval_s` | `600` | Seconds between reviews; the governor eval interval is the floor. |
| `model` | `litellm.default_model` | Reviewer model id sent to LiteLLM. |
| `transcript_lines` | `120` | Trailing transcript lines sent per agent. |
| `on_divergence` | `pause` | `pause` stops the agent + alerts; `alert` only notifies. |
| `exempt_agents` | `[]` | Agents never reviewed. |

The reviewer uses the hive's existing `governor.litellm` endpoint and key
(same resolution as inference), so no new secret is introduced.

## Backend and model coverage

The lane reads each agent's **tmux transcript**, which every CLI backend writes
to — so it works identically for `claude`, `copilot`, `gemini`, `goose`,
`bob`, and the self-hosted inference backends. It does not depend on a
provider-specific trace format.

Two honest limits:

- **It reads the rendered transcript, not hidden chain-of-thought.** It judges
  what the agent *does and says in its session*, which is where drift becomes
  observable — but a model that reasons silently and acts minimally gives it
  less to see. This is why the lane is one layer among several (short
  re-anchored rollouts, scoped per-task credentials, PR-gated writes), not the
  only defense.
- **The reviewer is itself a model.** It can miss or false-positive. Keep the
  reviewer model at least as capable as a small frontier model, start in
  `alert` mode to calibrate on a given hive's agents, then switch to `pause`.

## Relationship to other safeguards

The lane is the semantic-oversight layer. It complements hive's structural
mitigations rather than replacing them: agents are re-kicked with fresh intent
every governor cycle (no hour-one directive to forget), write only via
CI-gated PRs, run under per-task GitHub tokens and per-tier rate limits, and
are already pausable/auditable. Trajectory review adds the missing "what
outcome is this sequence working toward?" check on top.
