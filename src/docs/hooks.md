# State-triggered hooks

Hooks let an operator attach behavior to hive's state transitions **declaratively**, in config, instead of patching core. When a named transition durably commits, hive performs a vetted action.

This implements [RFC #4001](https://github.com/kubestellar/hive/issues/4001).

```yaml
# /data/hive.yaml (PVC-backed running config)
hooks:
  - name: review-rejected-notify
    on: review_rejected
    action: notify
    params:
      priority: high
```

That one rule is the shipped default: when a human rejects a review's output, hive sends a notification naming the model that produced it and linking straight to that model's pin control.

## Why this exists

Hive already reacts to state transitions everywhere — the governor's mode changes, agent pause/resume, sweep results, escalation's red-CI reactions, ACMM level changes, the upgrade kill switch. Every one of those reactions is hand-rolled at its call site.

The clearest symptom is [#3836](https://github.com/kubestellar/hive/issues/3836)'s upgrade-pause switch, whose own file header has to warn that the pause must be honoured by every delivery path *or it is a lie*. Hooks are where the next such switch gets **declared** instead of threaded by hand through every call site.

## Security model

Hooks are, by construction, a "run something when state changes" surface. The security posture matters more than the mechanism, so it is worth stating plainly what a hook can and cannot do.

### Registration is operator-only, config-as-code

Hook definitions live in the PVC-backed running config next to everything else. Writing them requires **exactly the same authz as any other config write**, and carries the same layer provenance — each hook shows up in `GET /api/config/provenance` as `hooks.<name>`, so you can always see which layer declared it.

There is **no runtime registration API**. Nothing agent-writable can reach the hook list. This is enforced structurally, not by a permission check that could be mounted on the wrong route: `pkg/hooks` exports no `Register`/`AddHook`/`InstallHook` function at all, and a test asserts that it never grows one. An agent that could register hooks on its own transitions would have an escalation path, so the capability simply does not exist.

### Observe and notify freely; mutate only through audited APIs

A hook may read transition payloads and send notifications without restriction. But **no action writes state directly** — not `hive-state.json`, not the registry, not any ledger. Mutating actions call the same audited entry points the dashboard uses:

| Action | Reaches the system through |
| --- | --- |
| `notify` | the existing ntfy/Slack/Discord fanout (`pkg/notify`) |
| `pause` | the **audited** agent-manager pause — the same call the dashboard's pause button makes |
| `annotate` | the existing lifecycle timeline (`pkg/timeline`) |
| `enqueue-approval` | the tool-approval queue ([#4000](https://github.com/kubestellar/hive/issues/4000)) |

Those four interfaces are the *complete* mutation surface of the feature. A dispatcher with no sinks wired can do nothing at all — there is no fallback path that writes directly.

The `pause` sink deliberately offers **no resume**. A hook may stop work; restarting it is a human decision.

### Why there is no `exec`

The action vocabulary is a **closed, vetted set**: `notify`, `pause`, `annotate`, `enqueue-approval`. There is deliberately no `exec`, `script`, `shell`, or `webhook` action.

Arbitrary code execution on a state transition is a fundamentally different security problem — it needs a sandbox story, a credential-isolation story, and a resource-bounding story, none of which this slice has. Shipping `exec` "just for trusted operators" would mean any config-write escalation becomes remote code execution.

This is enforced three ways so it cannot regress quietly:

1. Validation rejects any action outside the vetted set, naming the allowed set in the error.
2. A test asserts the action set is exactly those four, and that every plausible spelling of code execution (`exec`, `bash`, `eval`, `webhook`, …) is refused.
3. A test parses the package's own imports and fails if `pkg/hooks` ever imports `os/exec`, `syscall`, `plugin`, `net/http/cgi`, or `text/template`. No future action can shell out without that test failing first.

If exec-style hooks are ever wanted, they are a separate RFC with their own sandbox design.

### Fail-closed everywhere

An invalid hook list is **rejected in full** — hive does not skip the bad rule and arm the rest. Skipping would leave you with a hook you believe is armed and which silently never fires, which is the failure mode that makes declarative rule engines untrustworthy.

Rejected at config-load time:

- an unknown transition (the error lists the catalog)
- an unknown action (the error lists the vetted set)
- a `when:` predicate that does not parse, type-check, or return a bool
- a predicate referencing a field that does not exist (so `t.agnet` is a load error, not a rule that never matches)
- a duplicate hook name
- a negative or over-maximum rate limit
- a `notify` with an unknown priority
- a `pause` on a transition that carries no agent, with no explicit `params.agent`

At runtime, a predicate that errors or exceeds its cost budget is treated as **no-match**. A predicate hive cannot evaluate must never be able to trigger a pause.

On a config reload, an invalid hook list logs an error and **keeps the previous hook set armed** rather than disarming working hooks.

## Dispatcher guarantees

**1. Hooks fire post-durable-commit, never inline.** An emitting site calls `Fire` only after the durable write succeeds, and `Fire` hands work to a background goroutine and returns immediately. A hook can never slow, block, or fail the transition that triggered it. (A hook fired before the persist could act on a transition that never durably happened — recent incident history says that is not hypothetical.)

**2. Depth-1 causation guard.** A transition *caused by a hook* does not itself fire hooks.

This matters because `pause` is both an action and a transition, so a hook on `agent_paused` whose action is `pause` feeds itself. Rate limiting alone does not fix that — it only slows the storm, and a slow infinite loop is still an infinite loop that fills the audit log. The guard is checked in `Fire`, the single entry point, so no emitter and no action can bypass it.

Depth 1 rather than a configurable cap is deliberate: a configurable depth invites raising it, and the useful cases are all depth-0 → depth-1. Genuine multi-stage reactions should be expressed as multiple hooks on real transitions.

**3. Per-hook rate limiting.** Every hook has a firings-per-minute ceiling (default 12, maximum 120). There is **no unlimited setting** — a flapping governor mode must not become a notification storm. The window survives config reloads, so a reload loop cannot be used to clear it. Suppressed firings are audited, not dropped silently.

The window is *sliding and half-open*: the guarantee is "at most `limit` firings in any trailing 60 seconds", not "at most `limit` per calendar minute". A firing exactly 60 seconds after an earlier one does not count against it, so a burst at the boundary can produce `limit + 1` firings within a single 60-second span. Lowering a hook's limit takes effect immediately, and capacity returns as the existing firings age out.

**The limit is consumed before `when:` is evaluated.** A transition that reaches the hook costs a slot even if the predicate then declines it — this is what stops a flapping transition from becoming a CEL-evaluation storm. The consequence to plan for: a hook with a *selective predicate on a noisy transition* can exhaust its quota without ever firing. If a hook seems not to fire despite a matching transition, check the audit log for `hook_rate_limited` and raise its `rate_limit_per_minute`.

**4. Failure isolation.** Each hook runs independently behind its own recovered panic boundary and a 30-second timeout — enforced even for sinks whose call takes no context, so a wedged notification endpoint times out as a single failed hook instead of pinning a goroutine. One bad hook affects neither the transition nor any other hook. A hook whose sink is unwired reports a loud error per firing rather than silently doing nothing.

**5. Every firing is audited.** Success, failure, rate-limit suppression, and depth suppression all land in `/data/audit.jsonl` alongside every other privileged action, as `hook_fired`, `hook_failed`, `hook_rate_limited`, and `hook_suppressed_depth`. Each entry names the responsible config rule. Where the transition carries model metadata, so does the audit entry — the audit log alone can answer "which model produced the rejected output".

## Transition catalog

| Transition | Fires when | Payload fields |
| --- | --- | --- |
| `governor_mode_change` | the governor commits a mode change | `from`, `to`, `reason` |
| `agent_paused` | an agent's paused flag is durably set | `agent`, `trigger`, `reason` |
| `agent_resumed` | an agent's paused flag is durably cleared | `agent`, `trigger`, `reason` |
| `sweep_completed` | an auto-merge sweep records its result | `repo`, `reason`, `attrs.merged`, `attrs.skipped` |
| `escalation_red` | escalation observes a red-CI state it reacts to | `repo`, `agent`, `reason`, `attrs.pr` |
| `acmm_level_change` | the ACMM level changes via the audited path | `from`, `to`, `actor` |
| `upgrade_pause` | the #3836 upgrade kill switch flips (`to` is `on`/`off`) | `to`, `actor`, `reason` |
| `review_rejected` | a human rejects a review's output as low quality | `agent`, `repo`, `actor`, `reason`, `model`, `backend`, `pin`, `acmm_level`, `attrs.pr`, `attrs.model_knob_url` |

For `agent_paused`/`agent_resumed`, `trigger` carries the `paused_trigger` provenance, so you can tell an operator pause from a governor pause from the login-detector's.

A hook-driven pause records both halves of the #4055 provenance: `paused_trigger` is `hook:<hook-name>`, and `paused_by` is the same machine identity rather than empty or a fabricated user. A hook pause is therefore never anonymous in the fleet view — "paused, actor unknown" is precisely the state that proved indistinguishable from a malfunction — while still being unmistakably not a person.

### Adding a transition

Three steps, all in `src/pkg/hooks/transition.go` plus the emitting site:

1. Declare a `Transition` constant.
2. Add a `catalogEntry` (name, doc, payload fields it populates).
3. Call `Dispatcher.Fire` at the emitting site, **after** the durable commit.

Nothing else changes — the registry, predicates, and every action work off the generic `Payload`. `installGovernorModeChangeEmitter` in `cmd/hive/hookwire.go` is the worked example: it uses an observer the governor invokes after committing *and* after releasing its mutex, which is how you emit post-commit without holding a lock across third-party work.

**If the transition can also be produced by a hook action, the emitter must carry the causation.** `pause` is the case that exists today: a hook that pauses an agent feeds `agent_paused` straight back into the hooks that triggered it. The pause action passes `cause.Child(hookName, transition)` through the audited pause API, and the `agent_paused` emitter carries that structured cause because the depth-1 guard reads *only* `Payload.Causation`. The `paused_trigger` provenance string (`hook:<name>`) is for humans reading the audit log — do not try to reconstruct the depth by parsing it.

## Action reference

### `notify`

Sends through the configured ntfy/Slack/Discord fanout.

| Param | Meaning |
| --- | --- |
| `title` | override the default title |
| `message` | override the default body |
| `priority` | `high`, `default`, or `low` |

The default body is transition-aware; for `review_rejected` it leads with the producing model and its knob link.

### `pause`

Pauses an agent through the audited pause API.

| Param | Meaning |
| --- | --- |
| `agent` | agent to pause; defaults to the transition's agent |
| `reason` | recorded on the pause; defaults to naming the hook |

### `annotate`

Records an entry on the lifecycle timeline.

| Param | Meaning |
| --- | --- |
| `note` | the annotation text |
| `issue_ref` | issue to attach to; defaults to `attrs.issue_ref` |

Model/backend/pin are carried into the timeline attrs when the transition has them.

### `enqueue-approval`

> **⚠️ NOT YET FUNCTIONAL — does not gate anything today.**
>
> The queue interface is defined and consumed, but the backing queue ships with
> [#4000](https://github.com/kubestellar/hive/issues/4000). Until then the sink
> is nil: an `enqueue-approval` hook **loads and validates cleanly, fires, and
> then fails to enqueue**, recording an unwired-sink error in the audit log.
>
> **The transition it appears to gate is NOT blocked.** A hook written to
> require approval for an ACMM raise does not prevent that raise — it records a
> failure after the fact. Treat any `enqueue-approval` hook as documentation of
> intent, not as an enforcement control, and do not rely on one for a change
> you actually need approved.
>
> Watch for `hook_failed` entries in `/data/audit.jsonl` if you configure one
> anyway.

Places a request on the [#4000](https://github.com/kubestellar/hive/issues/4000) tool-approval queue.

| Param | Meaning |
| --- | --- |
| `kind` | approval category; defaults to the transition name |
| `summary` | the ask shown in the approvals UI |
| `agent`, `repo` | scope; default to the transition's values |

**Status:** see the callout at the top of this section — not yet functional. The wiring target is known: as of [#4057](https://github.com/kubestellar/hive/issues/4057) it is `toolapprove.Inbox`, connected by an adapter in `cmd/hive/hookwire.go` passed via `WithApprovalQueue`. Nothing in `pkg/hooks` changes when it lands.

## Predicates (`when:`)

An optional CEL expression, evaluated against the transition payload bound to `t`. Empty means "always fire". It uses the same engine and the same fail-closed posture as `triggers:` (`pkg/celtrigger`) — for the `triggers:` config key itself (declarative agent triggering on source-control events, a separate surface from hooks that happens to share this engine), see [CEL-based agent triggers](cel-triggers.md).

```yaml
hooks:
  - name: pause-reviewer-on-repeated-rejects
    on: review_rejected
    action: pause
    when: t.agent == "reviewer" && t.pin != ""
    params:
      reason: repeated low-quality output on a stale pin
```

Available fields: `t.transition`, `t.from`, `t.to`, `t.agent`, `t.repo`, `t.actor`, `t.trigger`, `t.reason`, `t.model`, `t.backend`, `t.pin`, `t.acmm_level`, `t.attrs`.

**Use `attr(t.attrs, "key")` to read attrs, not `t.attrs["key"]`.** CEL's native map index *raises* on an absent key, and since a raised error is treated as no-match, `t.attrs["pr"] != ""` would silently never fire for transitions that omit that attr. `attr()` returns `""` for a missing key instead.

```yaml
    when: attr(t.attrs, "pr") != ""
```

Referencing a field that does not exist is a **compile error at config load**, so typos surface immediately rather than becoming a rule that never matches.

## Worked examples

### Notify on a rejected review, with the model knob in the message

The shipped default. Answers the fleet-owner ask: when output is bad, see the tuning knob *in that moment* rather than hunting the admin UI.

```yaml
hooks:
  - name: review-rejected-notify
    on: review_rejected
    action: notify
    params:
      priority: high
```

Produces:

```
[hive-prod] Review rejected: reviewer
kubestellar/hive#4001 review rejected by fleet-owner.
Reason: hallucinated the API surface
Produced by anthropic/claude-opus-4 (pin: 20240229)
Adjust the model pin: https://hive.example.com/agents?agent=reviewer&focus=model
```

### Alert only on the governor entering surge

```yaml
hooks:
  - name: surge-alert
    on: governor_mode_change
    action: notify
    when: t.to == "surge"
    params:
      priority: high
      title: Governor entered surge
```

### Annotate the timeline whenever the upgrade kill switch flips

```yaml
hooks:
  - name: record-upgrade-pause
    on: upgrade_pause
    action: annotate
    params:
      note: upgrade delivery kill switch flipped
```

### Require approval for a large ACMM jump

> **⚠️ This example does not work yet.** `enqueue-approval` has no backing queue
> until [#4000](https://github.com/kubestellar/hive/issues/4000) — the hook
> below validates, loads, and fires, but **does not block the ACMM raise**. It
> is shown as the intended shape, not as a control you can deploy today.

```yaml
hooks:
  - name: gate-high-autonomy
    on: acmm_level_change
    action: enqueue-approval
    when: t.to == "5" || t.to == "6"
    params:
      kind: acmm-raise
      summary: Confirm raising fleet autonomy to L5+
```

### Rate-limit a chatty hook explicitly

```yaml
hooks:
  - name: sweep-summary
    on: sweep_completed
    action: notify
    rate_limit_per_minute: 2
    params:
      priority: low
```

## Operational notes

- **Hot reload.** Hooks recompile on config reload, only when the list actually changed. The registry swaps inside the existing dispatcher so rate-limit windows survive.
- **Default off.** No `hooks:` block means no hooks fire and behavior is byte-identical to before. An empty registry costs one map lookup per transition.
- **Bounded.** At most 100 hooks; each predicate has a bounded CEL cost budget; each action has a 30-second timeout.
- **Diagnosing a hook that is not firing.** Check, in order: hive logs at startup/reload for a rejection (`hooks: rejecting invalid hook config`); the audit log for `hook_rate_limited` or `hook_suppressed_depth`; whether the `when:` predicate matches (a warning is logged when one fails to evaluate); and whether the transition is hook-caused, in which case the depth-1 guard suppressed it by design.
