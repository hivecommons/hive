# Skill registry (`/data/skills/`)

The registry is the home for reusable, named instructions — repo conventions,
review patterns, "how we do X here" — that an agent loads by name instead of
having them pasted into every prompt. See
[ADR-0012](adr/0012-skill-registry.md) for why it exists.

Skills **are delivered to agents today** — the scheduler resolves and injects
them on every kick (`primeSkills`). Delivery is opt-in: skills reach an agent
**only when that agent declares them**. Dropping files in `/data/skills/` makes
them *available*; it does not change any agent's behaviour until that agent's
config names them (see
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
| `tags` | no | none — free-form metadata |

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

At each kick the scheduler loads `/data/skills/` and, when a checkout is
configured for the primary repo, parses that repo's `AGENTS.md` and adjacent
`skills/` directory. It resolves each declared name against the hive-wide
registry first and falls back to the repo-local definition when the registry
has no match, then prepends the rendered block to the agent's `${KNOWLEDGE}`
section (`src/pkg/scheduler/scheduler.go`, `primeSkills`). Loading happens **per
kick, not once at startup**, so editing either source takes effect on the next
kick — no hive restart required.

The fallback needs a checkout root from `project.checkouts_dir` (or the guarded
`policies.local_dir` fallback described in [agents-md.md](agents-md.md)). Without
one, registry-backed skills continue to work exactly as before.

Only declared skills are injected. A skill sitting in the directory that no
agent names is never sent to anyone.

## What happens when something is wrong

Every failure mode degrades the kick rather than blocking the agent:

| Situation | Result |
| --- | --- |
| agent declares no `skills:` | nothing injected |
| `/data/skills/` absent | repo-local matches still inject when a checkout is configured; otherwise nothing |
| repo checkout absent | registry matches still inject; repo-local fallback is unavailable |
| declared name matches neither source | that name skipped; the others still inject |
| *every* declared name unknown | nothing injected, logged at `warn` |
| malformed front matter in a file | that file skipped by `Load`, logged |

Each injection logs at `info` with the agent, registry directory, repo root, and
how many skills were injected, so "did this agent actually get its skills" is
answerable from the hive log.

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
the **highest version wins** for an agent skill reference.

## Discovering skills

The same registry used at kick time is exposed through hivectl:

```bash
hivectl agent specs list
hivectl agent specs search testing --limit 10
```

Both commands read `/data/skills/` from the local filesystem and return the
skill name, version, description, source, and tags. Use `--dir` to inspect a
different local registry, and use `--output json` or `--output yaml` for
scripts.

## BYO-agent specs

ADR-0012 also defines a small bring-your-own-agent spec that a configured agent
can reference with `agent_spec`:

```yaml
agents:
  reviewer:
    agent_spec: /data/agent-specs/reviewer
```

The reference may point to a YAML file or to a directory containing
`agent.yaml`, `agent.yml`, `agent-spec.yaml`, `agent-spec.yml`, `spec.yaml`, or
`spec.yml`. Hive loads it with `skillreg.LoadAgentSpec` when the agent launches
and applies its backend, model, mode, launch command, prompt, tools, and default
skills. See [Agent configuration](agent-configuration.md#byo-agent-specs) for
the full example.

## Package surface

| Function | What it does |
| --- | --- |
| `NewRegistry()` | empty registry |
| `Registry.Load(dir, logger)` | reads `*.md` from a directory, returns the count loaded |
| `Registry.Add(skill)` | adds one skill |
| `Registry.Get(name)` | looks up the newest loaded version for a name |
| `Registry.Resolve(name, constraint)` | looks up a version by exact, wildcard, caret, or `>=` constraint |
| `Registry.List()` | lists loaded skills for catalog UX |
| `Registry.Search(term)` | searches names, descriptions, and tags |
| `LoadAgentSpec(path)` | loads a BYO-agent YAML file or spec directory |
| `Registry.ResolveRequested(cfg, names)` | resolves names, preferring registry skills over an `AGENTS.md` inline fallback |
| `InjectionText(skills)` | renders resolved skills as the Markdown block injected into a kick |

## Related

- [ADR-0012: skill registry](adr/0012-skill-registry.md) — the architecture
  decision. ADRs are decision records, not operator guides.
- [Knowledge curator](knowledge-curator.md) — the complementary mechanism for
  *factual* per-issue knowledge. Both it and the skill registry deliver into the
  same `${KNOWLEDGE}` block of the kick.
