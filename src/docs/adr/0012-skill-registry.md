# ADR-0012: Skill registry and BYO-agent contract

Status: Accepted (back-filled)

> **Operator guide:** [Skill registry](../skills.md) covers the on-disk format
> and the current wiring status. This ADR records the decision; it is not an
> operator guide.

## Context

`AGENTS.md` can carry repo-local skill snippets, but inline snippets do not give
Hive a shareable catalog or version metadata.

## Decision

Introduce `skillreg` as a concurrency-safe registry of named, versioned skill
files ([skill registry](../../pkg/skillreg/skillreg.go)). A skill is a Markdown
body with optional YAML front matter for name, version, description, and tags.
Missing skill directories, unreadable files, and malformed front matter are
skipped with logs so one bad catalog entry does not prevent Hive from loading.

Requested skills are selected by preferring the curated registry over inline
`AGENTS.md` snippets, while falling back to repo-local skills the registry does
not know about. `Get` returns the newest loaded version, `List`/`Search` provide
catalog UX, and `InjectionText` renders a single Markdown block for the kick
path.

The same package defines `AgentSpec`, the minimal BYO-agent contract:
name, backend, model, operating mode, optional launch command, prompt, tools,
and default skills. Agent configs opt into that contract with `agent_spec`,
which points at a YAML file or spec directory.

## Status notes

- Implemented in #6004: `agent_spec` is loaded through `skillreg.LoadAgentSpec`
  on the pkg/agent launch path, and `hivectl agent specs list|search` exposes
  the registry discovery surface.

## Consequences

Rationale not recorded beyond the implementation, linked code, and cited design notes.

Skills become portable without removing simple repo-local snippets. The
trade-off is that registry skills intentionally override inline snippets, so
catalog governance matters. BYO-agent specs add launcher integration without
turning the registry into a package manager: Hive resolves a local declaration
and applies only the fields in the stable contract.
