# ADR-0013: CEL triggers over normalized forge events

Status: Accepted (retroactive)

## Context

Hive's built-in label and governor triggers cover common flows, but operators
need repo-specific trigger policy without hard-coding GitHub webhook details or
forking the governor. The trigger language must be expressive enough for labels,
branches, authors, draft status, and comments, while failing safely when an
operator writes a bad rule.

## Decision

Add CEL-based declarative triggers over a forge-neutral `NormalizedEvent`
([CEL trigger engine](../../pkg/celtrigger/celtrigger.go)). Forge adapters map
native events onto stable kinds such as `issue.opened`, `pr.labeled`,
`pr.ready_for_review`, and `comment.created`; CEL expressions see only the
`event` object with normalized fields such as repo, labels, title, author, body,
state, branches, assignees, and comment.

Compile every rule before use and require a boolean expression. Unknown fields,
parse errors, type errors, empty expressions, or non-boolean results reject the
engine at config load. Runtime evaluation errors are treated as no match. Matched
rules are returned in descending priority, and `MatchAgents` de-duplicates agent
names before the governor unions them with existing built-in triggers
([config wiring](../../pkg/celtrigger/wire.go)).

## Consequences

Operators can add declarative, forge-neutral trigger policy without changing Go
code, and bad rules fail closed before they can crash a running fleet. The same
normalized event model keeps the path open for GitLab or other forge adapters.
The trade-off is that CEL only sees fields Hive normalizes; new trigger needs may
require extending `NormalizedEvent`, and a malformed rule currently blocks the
whole configured CEL engine rather than being partially ignored.
