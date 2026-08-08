# ADR-0012: Skill registry and BYO-agent contract

Status: Accepted (retroactive)

## Context

`AGENTS.md` can carry repo-local skill snippets, but inline snippets do not give
Hive a shareable catalog, version metadata, search, or a stable contract for
third-party agents. Hive also needs custom agents to be declared without binding
them to internal launcher structures.

## Decision

Introduce `skillreg` as a concurrency-safe registry of named, versioned skill
files plus a minimal bring-your-own-agent SDK contract
([skill registry](../../pkg/skillreg/skillreg.go)). A skill is a Markdown body
with optional YAML front matter for name, version, description, and tags. Missing
skill directories, unreadable files, and malformed front matter are skipped with
logs so one bad catalog entry does not prevent Hive from loading.

Resolve requested skills by preferring the curated registry over inline
`AGENTS.md` snippets, while falling back to repo-local skills the registry does
not know about. Version resolution supports latest, exact versions, wildcard,
caret-major, and greater-than-or-equal constraints, and `InjectionText` renders a
single Markdown block for the kick path.

Define the BYO-agent contract as `AgentSpec`: name, backend, model, operating
mode, and default skills ([agent spec](../../pkg/skillreg/agentspec.go)). YAML
agent specs are strict: malformed YAML, missing name/backend/model, or unknown
modes fail before an agent can launch silently.

## Consequences

Skills become portable and discoverable without removing simple repo-local
snippets. Custom agents get a small, stable interface that catalogs and launchers
can share. The trade-off is that registry skills intentionally override inline
snippets, so catalog governance matters; and the current registry helper is a
contract and rendering layer, not a full package manager or deep launcher
integration.
