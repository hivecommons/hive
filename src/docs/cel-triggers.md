# CEL-based agent triggers

The `triggers:` config key lets an operator declare CEL ([Common Expression
Language](https://github.com/google/cel-spec)) rules that decide when an
agent should be kicked for an incoming event — issue opened, issue labeled,
PR opened, a comment posted, and so on — without writing code.

```yaml
triggers:
  - name: bug-triage
    expr: event.kind == "issue.opened" && hasLabel(event.labels, "bug")
    agent: triager
    priority: 10
```

This is implemented by `pkg/celtrigger`.

## How it composes with existing triggering

`triggers:` is **additive**. It runs *alongside* — not in place of — hive's
built-in label/governor triggering: the governor's eval cycle evaluates the
compiled rule set against each enumerated actionable item and **unions** the
matched agent names into the due-agents set, after its own pause/budget/
on-demand gates already apply. An empty (or absent) `triggers:` list compiles
to a valid engine that never matches anything, so leaving it out is
byte-identical to today's behavior — nothing about default triggering
changes because this feature exists.

## The `TriggerRule` schema

Each entry under `triggers:` is one `TriggerRule` (`pkg/config/config.go`):

| YAML field | Type | Required | Notes |
| --- | --- | --- | --- |
| `name` | string | yes (in practice) | Human-readable identifier for logs/diagnostics. Not enforced non-empty by the schema, but an empty name is rendered as `rule[<index>]` in error messages, so name your rules. |
| `expr` | string | **yes** | The CEL expression. Must compile and must evaluate to a `bool`. An empty expression is a compile-time error. |
| `agent` | string | **yes** | The agent to trigger when `expr` evaluates `true`. A rule with an empty `agent` compiles fine but is silently skipped when collecting matched agents. |
| `priority` | int | no (default `0`) | Orders competing rules when several match the same event — higher sorts first. Ties preserve declaration order. |

Source: `TriggerRule` struct, `pkg/config/config.go` (`Name`, `Expr`, `Agent`,
`Priority` fields with their `yaml:"..."` tags), and `pkg/celtrigger/wire.go`'s
`CompileFromConfig`, which maps each `TriggerRule` 1:1 onto a
`celtrigger.Rule`.

## What the CEL expression binds to

Every rule is evaluated against exactly one variable: **`event`**, a
`celtrigger.NormalizedEvent` — hive's forge-neutral representation of a
source-control event. `event` is the *only* activation exposed to the
expression; there is no other variable, no access to config, no access to
agent state.

The reachable fields, taken from the `cel:"..."` struct tags on
`NormalizedEvent` (`pkg/celtrigger/celtrigger.go`), are:

| CEL field | Go type | Meaning |
| --- | --- | --- |
| `event.kind` | string | Event kind — see the `Kind*` constants below. |
| `event.repo` | string | Fully-qualified repository, e.g. `"org/name"`. |
| `event.labels` | list of string | Labels on the issue/PR. |
| `event.title` | string | Issue/PR title. |
| `event.author` | string | Login/handle of the actor that produced the event. |
| `event.body` | string | Issue/PR/comment body. |
| `event.is_draft` | bool | Whether a PR is a draft. |
| `event.number` | int | Issue/PR number. |
| `event.state` | string | Current state, e.g. `"open"`/`"closed"`. |
| `event.base_branch` | string | Target branch for a PR. |
| `event.head_branch` | string | Source branch for a PR. |
| `event.assignees` | list of string | Currently-assigned logins. |
| `event.comment` | string | Body of the triggering comment (for `comment.created`). |

Field access is type-checked at compile time against this registered native
type — an expression that references a field not in this table (e.g.
`event.does_not_exist`) is rejected at `Compile`, not left to fail silently
at runtime.

`event.kind` takes one of these forge-neutral values (`pkg/celtrigger/celtrigger.go`):

```
issue.opened
issue.labeled
issue.closed
issue.reopened
pr.opened
pr.labeled
pr.ready_for_review
pr.closed
pr.merged
comment.created
```

### Available functions

Besides the standard CEL string/list operators (`in`, `.contains(...)`,
`.startsWith(...)`, `.matches(...)`, `&&`, `||`, `!`, `==`, comprehensions
like `.exists(...)`/`.all(...)`), one custom helper is registered:

- `hasLabel(event.labels, "bug")` — convenience for
  `"bug" in event.labels`; returns `false` (not an error) for non-list or
  non-string arguments.

No other custom functions exist. Do not invent one — an expression calling
an unregistered function is a compile-time error.

### Evaluation cost budget

Each rule's evaluation is bounded (`maxEvalCost = 10_000` in
`pkg/celtrigger/celtrigger.go`) to stop pathological nested comprehensions
(e.g. a triple-nested `.all()` over labels) from burning runtime per event.
A rule that exceeds the budget is treated as **no-match** for that
evaluation (not an error, not a crash) and logs a warning naming the rule
and the budget so you can diagnose why it stopped firing. Realistic
operator rules — field checks, string predicates, a label
`.exists(...)`/`.contains(...)` — comfortably fit inside the budget.

## Fail-closed semantics

This is a safety property, stated plainly:

- **Compile time (config load):** every rule's `expr` is parsed,
  type-checked, and confirmed to return `bool`. If *any* rule in `triggers:`
  is malformed — a syntax error, a reference to a field that doesn't exist,
  a call to an unknown function, an expression that returns something other
  than `bool`, or an empty expression — the **entire config load fails**
  with an error. No partial engine is produced; the bad rule cannot reach a
  running fleet.
- **Runtime (event evaluation):** if a compiled rule still errors while
  evaluating a specific event (including exceeding the cost budget above),
  that is treated as **"no match,"** not a crash and not an error
  propagated to the caller. One misbehaving rule cannot take down
  evaluation for the other rules or for the event pipeline.

In short: a bad rule is caught early and loudly (at config load); a rule
that merely fails to match at runtime does so quietly and safely.

## Worked example

This rule set compiles and matches, verified against
`pkg/celtrigger/wire_test.go`'s `TestCompileFromConfig_ValidAndMatch`:

```yaml
triggers:
  - name: bug
    expr: hasLabel(event.labels, "bug")
    agent: triager
    priority: 10
  - name: pr
    expr: event.kind == "pr.opened"
    agent: reviewer
    priority: 5
```

An `issue.labeled` event carrying the label `bug` matches the `bug` rule and
kicks `triager`. A more selective example, combining a kind check, a
non-draft check, and a target-branch check — verified against
`pkg/celtrigger/celtrigger_test.go`'s `TestMatch_BooleanCombos`:

```yaml
triggers:
  - name: ready-pr
    expr: event.kind == "pr.opened" && !event.is_draft && event.base_branch == "main"
    agent: reviewer
```

This matches a non-draft PR opened against `main`, and does not match a
draft PR against the same branch.

A slash-command-style comment trigger — verified against
`TestMatch_CommentEvent`:

```yaml
triggers:
  - name: slash-cmd
    expr: event.kind == "comment.created" && event.comment.startsWith("/hive")
    agent: cmd
```

## Relationship to hook predicates (`when:`)

[State-triggered hooks](hooks.md) use the same CEL engine and the same
fail-closed posture for their optional `when:` predicate, but bind a
different, unrelated variable (`t`, the transition payload) — see
[Predicates (`when:`)](hooks.md#predicates-when) in that doc. `triggers:`
and hooks' `when:` are two separate surfaces that happen to share an
evaluation engine: `triggers:` decides whether to *kick an agent* for a
normalized source-control event, while hooks' `when:` decides whether to
*fire an action* on a state transition. Don't mix `event.*` fields into a
hook's `when:` or `t.*` fields into a `triggers:` rule — the field sets are
disjoint and referencing the wrong one is a compile error.
