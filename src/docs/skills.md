# Skill registry (`/data/skills/`)

The registry is the home for reusable, named instructions — repo conventions,
review patterns, "how we do X here" — that an agent loads by name instead of
having them pasted into every prompt. See
[ADR-0012](adr/0012-skill-registry.md) for why it exists.

Skills reach agents **only when an agent declares them**. Dropping files in
`/data/skills/` makes them *available*; it does not change any agent's
behaviour until that agent's config names them (see
[Declaring skills on an agent](#declaring-skills-on-an-agent)). An agent with
no `skills:` list is unaffected no matter what the directory contains.

Skills are for *procedural* instructions the operator writes and versions. For
*factual* project knowledge retrieved per issue, use the
[knowledge vault](knowledge-curator.md) — the two are complementary and both
land in the same `${KNOWLEDGE}` block of the kick.

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

## Declaring skills on an agent

An agent opts in by naming skills in its config. The name is the skill's
`name` (which defaults to the filename without `.md`), not the file path:

```yaml
agents:
  reviewer:
    skills:
      - go-error-wrapping
      - review-checklist
```

At each kick the scheduler loads `/data/skills/`, resolves the declared names,
and prepends the rendered block to the agent's `${KNOWLEDGE}` section
(`src/pkg/scheduler/scheduler.go`, `primeSkills`). Loading happens **per kick,
not once at startup**, so editing a skill file takes effect on the next kick —
no hive restart required.

Only declared skills are injected. A skill sitting in the directory that no
agent names is never sent to anyone.

## What happens when something is wrong

Every failure mode degrades the kick rather than blocking the agent:

| Situation | Result |
| --- | --- |
| agent declares no `skills:` | nothing injected |
| `/data/skills/` absent | nothing injected |
| declared name matches no skill | that name skipped; the others still inject |
| *every* declared name unknown | nothing injected, logged at `warn` |
| malformed front matter in a file | that file skipped by `Load`, logged |

Each injection logs at `info` with the agent, the directory, and how many
skills were injected, so "did this agent actually get its skills" is answerable
from the hive log.

## Size cap

A single kick may carry at most **8 KiB** of skill bodies
(`maxSkillsInjectionBytes`). The kick prompt shares a context budget with the
knowledge primer and the issue/PR lists, so an unbounded skill file could
otherwise crowd out the actual work queue.

Skills are kept in declaration order until the next one would exceed the cap.
A skill that does not fit is **dropped whole, never truncated** — an agent
never receives half an instruction — and dropped names are logged at `warn`.
Smaller skills declared after an oversized one are still considered, so one
large file does not silently suppress everything behind it.

## Versions

When several files declare the same `name` with different `version` values,
the **highest version wins** for a plain name reference. `Registry.Resolve`
additionally understands `^1.2.0` (highest sharing that major) and `>=1.2.0`
constraints; agent config uses plain names today, which resolve to the highest
version.

## Package surface

| Function | What it does |
| --- | --- |
| `NewRegistry()` | empty registry |
| `Registry.Load(dir, logger)` | reads `*.md` from a directory, returns the count loaded |
| `Registry.Add(skill)` | adds one skill |
| `Registry.Get` / `Resolve` / `Search` / `List` | look skills up by name, constraint, or term |
| `Registry.ResolveRequested(cfg, names)` | resolves names, preferring registry skills over an `AGENTS.md` inline fallback |
| `InjectionText(skills)` | renders resolved skills as the Markdown block injected into a kick |
| `ParseAgentSpec` / `LoadAgentSpec` | parse a BYO-agent spec that can name `DefaultSkills` |

## Still not wired

Two paths named in the package remain unconnected, and this page will not
imply otherwise:

- **`AGENTS.md` inline skills.** `ResolveRequested` accepts an
  `agentsmd.AgentsConfig` so a repo's inline snippets can act as a fallback
  under registry skills, but the scheduler passes `nil` for it. That path needs
  a per-repo checkout, which hive agents do not have — they work over the
  GitHub API — and `agentsRepoRoot()` returns `""` accordingly
  ([#5227](https://github.com/kubestellar/hive/issues/5227)). Until that lands,
  only registry files in `/data/skills/` are resolvable.
- **`AgentSpec.DefaultSkills`.** The BYO-agent contract can declare default
  skills, but no launcher consumes them yet; use the agent `skills:` config
  above.

## Related

- [ADR-0012: skill registry](adr/0012-skill-registry.md) — the architecture
  decision. ADRs are decision records, not operator guides.
- [Knowledge curator](knowledge-curator.md) — the mechanism that **does**
  deliver knowledge to agents today.
