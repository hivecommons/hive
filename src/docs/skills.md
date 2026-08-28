# Skill registry (`/data/skills/`)

> **⚠️ Loaded and counted, but NOT yet delivered to agents.**
>
> The registry is read at startup and its skill count appears on the dashboard.
> **Nothing injects those skills into an agent's context.** The source says so
> directly: *"The skills registry (pkg/skillreg) is not yet wired into the
> runtime"* (`src/pkg/dashboard/status_builder.go:31-32`).
>
> Populating `/data/skills/` today gives you a number on a dashboard and
> nothing else. It does not change how any agent behaves. Treat this page as
> the file-format contract to author against, not as a feature you can deploy
> to influence agents.

The registry is the intended home for reusable, named instructions — domain
knowledge, repo conventions, review patterns — that agents could request by
name instead of having them pasted into every prompt. See
[ADR-0012](adr/0012-skill-registry.md) for why it exists.

For knowledge that **does** reach agents today, use the
[knowledge vault](knowledge-curator.md) instead.

## Where files go

`/data/skills/`, inside the container — that is, in the mounted `/data`
volume. The path is `dataVolumePath + "/skills"`
(`src/pkg/dashboard/status_builder.go:36`).

The directory is **optional**. An absent directory reports "not configured"
rather than erroring, so nothing breaks if you never create it.

## File format

One skill per `*.md` file. Optional YAML front matter between `---` lines sets
the metadata; everything after it is the skill body.

```markdown
---
name: go-error-wrapping
version: 1.2.0
description: How this project wraps and inspects errors.
tags: [go, conventions]
---

Wrap with %w so callers can errors.Is/As. Never wrap a sentinel you expect a
caller to compare by identity.
```

| Field | Required | Default |
| --- | :---: | --- |
| `name` | no | the filename |
| `version` | no | `0.0.0` (`DefaultVersion`) |
| `description` | no | empty |
| `tags` | no | none — free-form labels used by `Search` |

`version` is dotted numeric (`major.minor.patch`); **missing parts read as
zero**, so `1.2` is `1.2.0`.

A file with no front matter is still a valid skill: the filename becomes the
name and the whole file becomes the body.

## Malformed files are tolerated, not rejected

`Registry.Load` skips a file it cannot read or parse and continues
(`src/pkg/skillreg/skillreg.go:191`). It returns the count it *did* load.

This matters for an operator: **a typo in one skill's front matter does not
fail startup and does not produce a loud error** — that skill is simply absent,
and the dashboard count is one lower than you expected. If the count does not
match your file count, look for a malformed file rather than assuming the
feature is broken.

## What exists in the package today

| Function | What it does |
| --- | --- |
| `NewRegistry()` | empty registry |
| `Registry.Load(dir, logger)` | reads `*.md` from a directory, returns the count loaded |
| `Registry.Add(skill)` | adds one skill |
| `ParseAgentSpec` / `LoadAgentSpec` | parse an agent spec that can name `DefaultSkills` |

The only non-test consumer in the tree is
`src/pkg/dashboard/status_builder.go:466`, which loads the directory to report
a count.

## Open questions

These are **not** settled in the code, and this page will not guess:

- **Precedence over inline `AGENTS.md` snippets.** A comment in
  `agentspec.go:53` refers to resolving a requested skill "against a Registry
  via `ResolveRequested`", but **no such function exists** in the package. There
  is no implemented resolution path, so there is no precedence behaviour to
  document yet.
- **How a skill will reach an agent** — injection at kick time, on request, or
  something else — is undecided in the tree.
- **Whether `version` will be used for selection.** It is parsed and stored, but
  nothing consumes it.

## Related

- [ADR-0012: skill registry](adr/0012-skill-registry.md) — the architecture
  decision. ADRs are decision records, not operator guides.
- [Knowledge curator](knowledge-curator.md) — the mechanism that **does**
  deliver knowledge to agents today.
