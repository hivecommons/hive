# AGENTS.md repo instructions

> **⚠️ Parsed and tested, but NOT wired into kicks.** Hive ships a complete
> `AGENTS.md` parser (`src/pkg/agentsmd`) and a call site that would prepend its
> output to every kick prompt — but the call site is permanently disabled. The
> single function that supplies it a repo checkout path,
> `Scheduler.agentsRepoRoot()`, unconditionally returns `""`
> (`src/pkg/scheduler/scheduler.go:1385-1390`), and `primeAgentsMd` treats an
> empty root as "nothing to inject" (`scheduler.go:1394-1396`). **An `AGENTS.md`
> file you add to a monitored repo today is never read by Hive and has no
> effect on any agent's kick prompt.** This page documents the file format Hive
> already understands, for when the wiring lands — not a feature you can rely
> on to change agent behavior now.

## Why it doesn't run today

Hive agents work over the GitHub API — they do not keep a local git checkout of
the repos they monitor. `agentsRepoRoot()` has no local path to hand back, so
returning `""` is the deliberate, conservative choice rather than a bug. The
comment on the function says so directly:

> "Hive agents operate over GitHub rather than local clones, so this is
> usually empty today; the hook exists so that when a checkout root becomes
> available (e.g. a git source's `LocalDir`) the AGENTS.md convention is
> honored without further wiring."

The one call site, in `Scheduler.buildKickForAgent`-adjacent code, is guarded
and additive — it only prepends when `primeAgentsMd` returns non-empty text —
and carries an explicit `TODO(agentsmd)` marking the missing piece: threading a
per-repo checkout root through, and preferring `agentsmd.ParseNearest` for
closest-wins nested files once file-level targeting exists
(`scheduler.go:242-248`).

This is the same shape as two other recently-documented gaps in Hive:
[`enqueue-approval`](https://github.com/kubestellar/hive/issues/4911) and the
[skill registry](skills.md) (`pkg/skillreg`) — a complete, tested package that
nothing in the runtime calls yet.

## What Hive's parser supports (the format, once wired)

`src/pkg/agentsmd` (`agentsmd.go:119`, `Parse`) reads a plain Markdown
`AGENTS.md` file at a repo root, with two additive extensions on top of
ordinary Markdown:

### Optional YAML front-matter

A block delimited by a line containing only `---` at the very top of the file,
closed by a second `---` line. The only key Hive's parser reads is `skills:` —
a list of skill names to request into the agent's context by default
(`agentsmd.go:11-21`, struct `frontMatter` at `agentsmd.go:104`). Everything
else in the block is ignored. Malformed front-matter is logged and skipped;
the rest of the file (the body) is still used (`agentsmd.go:184-189`).

```markdown
---
skills:
  - go-testing
  - pr-etiquette
---

# Repo conventions

...
```

### Inline skill sections

Any section whose heading is `## Skill: <name>` defines a reusable, named
instruction snippet. The section body runs from that heading to the next
`## ` heading, or end of file (`agentsmd.go:236-277`). Skills may also be
defined as standalone files at `skills/<name>.md` relative to the repo root;
when a standalone file and an inline section define the same name, **the
inline section wins** (`agentsmd.go:202-207`).

```markdown
## Skill: go-testing

Always run `go test ./...` before committing. Table-driven tests preferred.
```

### The body

Whatever is left after front-matter and `## Skill:` sections are stripped out
is the "body" — general instructions that would always apply
(`agentsmd.go:30-31`, `agentsmd.go:198-200`).

### Closest-wins nested `AGENTS.md`

`ParseNearest(repoRoot, relPath, logger)` (`agentsmd.go:127`) walks from a
target file's directory up to the repo root and uses the first `AGENTS.md` it
finds; the root file is the required baseline nested files override for their
own subtree. This exists in the package today but has no caller — the
scheduler's `TODO(agentsmd)` names it as the intended future entry point once
file-level targeting exists; the disabled call in `scheduler.go` currently
only ever calls the flat `Parse`, and even that call never runs because its
root argument is always `""`.

### Rendered injection text (if it ran)

`AgentsConfig.InjectionText(requestedSkills)` (`agentsmd.go:330`) is what would
be prepended to a kick: a `# Repository Agent Instructions (AGENTS.md)`
header, the body, and (if any skills resolve) a `## Requested Skills`
subsection with each skill's text under a `### <name>` heading. It returns
`""` — and therefore injects nothing — when both the body and the resolved
skills are empty.

## Parsing is tolerant, wiring is not the risk

Everything about the *parser* is designed to fail safe: a missing file yields
an empty, non-nil config (`agentsmd.go:171-174`); an unreadable file logs and
returns empty (`agentsmd.go:175-178`); malformed front-matter logs and falls
back to using the body alone. None of that tolerance is why the feature is
inert today — the parser is simply never invoked, because `agentsRepoRoot()`
never has anything to hand it.

## Connection to the skill registry

The `skills:` front-matter key names skills by the same kind of identifier the
[skill registry](skills.md) (`/data/skills/`, `pkg/skillreg`) would resolve —
but the two systems are **separately unwired** from each other and from the
runtime:

- `pkg/agentsmd` can resolve a skill name to text from an inline `## Skill:`
  section or a `skills/<name>.md` file **in the same repo** — this is
  self-contained and does not consult the registry at all.
- The skill registry is a **different, hive-wide** store, loaded and counted
  on the dashboard but — per its own page — not delivered into any agent's
  context either.

Neither pipeline reaches a live kick prompt today. Don't read the shared
`skills:` vocabulary as evidence that authoring one wires up the other; they
are independent, parallel gaps.

## What to read next

- **[Skill registry](skills.md)** — the sibling not-yet-wired feature: file
  format, where it's loaded from, and what "loaded and counted" actually means
  today.
- **[Agent configuration](agent-configuration.md)** — the `definition_source`
  and `channels`/`tools`/`connections` fields that *are* live.
- **[ADR-0012: Skill registry](adr/0012-skill-registry.md)** — the design
  rationale that also describes `AGENTS.md` inline snippets.
- **[ClankeR contributor relay](contributor-relay.md)** — the per-backend
  instruction file table (`AGENTS.md`, `CLAUDE.md`, `.goosehints`, etc.) for
  CLI-side instruction conventions unrelated to this scheduler-side injection
  gap.
