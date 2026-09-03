# AGENTS.md repo instructions

> **⚠️ Wired, but off unless you supply a checkout.** Hive ships a complete
> `AGENTS.md` parser (`src/pkg/agentsmd`) and a call site that prepends its
> output to every kick prompt. That call site used to be permanently disabled —
> `Scheduler.agentsRepoRoot()` returned `""` unconditionally, so no `AGENTS.md`
> was ever read. It now resolves a real per-repo checkout root
> ([#5227](https://github.com/hivecommons/hive/issues/5227)), but Hive agents
> work over the API and keep no clones, so **there is still no root unless you
> configure one**. Set `project.checkouts_dir` (below). Without it, an
> `AGENTS.md` you add to a monitored repo has no effect on any agent's prompt —
> the same as before, and still the default.

## Turning it on

Hive agents work over the GitHub API — they do not keep a local git checkout of
the repos they monitor. So the file has to already be on the hive host, and you
tell Hive where:

```yaml
project:
  org: your-org
  repos: [repo-one, repo-two]
  primary_repo: repo-one
  checkouts_dir: /data/checkouts   # repo-one is at /data/checkouts/repo-one
```

Each repo is looked up at `<checkouts_dir>/<repo name>` — the bare name from
`repos`, without the org — so a multi-repo hive gets each repo's own file and
never another repo's. The kick reads the **primary repo's** `AGENTS.md`, since
that is the repo the kick's instructions are about.

How you populate that directory is yours: a mounted volume, a sidecar that
clones and pulls, an NFS export. Hive only reads it.

One other source is used when it happens to be the same repo:
`policies.local_dir`, the checkout of `policies.repo`, is a genuine checkout
root that Hive already reads policy files from. If `policies.repo` names the
repo being asked about, it supplies the root. It is ignored otherwise, so a
config repo's `AGENTS.md` never leaks into work on an unrelated repo.
`checkouts_dir` wins when both apply.

Everything stays fail-open: an absent directory, a missing `AGENTS.md`, or a
blank one all yield no injection and never fail a kick. When nothing is
injected, the scheduler logs at debug which root came up empty — so a
wired-but-empty repo is now distinguishable from an unconfigured hive, which it
was not before.

### Still deferred

The call site keeps a `TODO(agentsmd)`, narrowed to its second half: preferring
`agentsmd.ParseNearest` for closest-wins nested `AGENTS.md`. That needs
file-level targeting — nothing on the kick path knows which *file* an agent will
touch — so `Parse` (repo root only) is what runs.

## What Hive's parser supports

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
own subtree. This still has no caller: the scheduler's `TODO(agentsmd)` names
it as the intended entry point once file-level targeting exists, and nothing on
the kick path knows which file an agent will touch. The live call uses the flat
`Parse` against the repo root.

### Rendered injection text

`AgentsConfig.InjectionText(requestedSkills)` (`agentsmd.go:330`) is what gets
prepended to a kick: a `# Repository Agent Instructions (AGENTS.md)`
header, the body, and (if any skills resolve) a `## Requested Skills`
subsection with each skill's text under a `### <name>` heading. It returns
`""` — and therefore injects nothing — when both the body and the resolved
skills are empty.

## Parsing is tolerant

Everything about the *parser* is designed to fail safe: a missing file yields
an empty, non-nil config (`agentsmd.go:171-174`); an unreadable file logs and
returns empty (`agentsmd.go:175-178`); malformed front-matter logs and falls
back to using the body alone. Combined with the guarded call site — which
prepends only non-empty text — a bad `AGENTS.md` degrades to no injection
rather than a failed kick.

## Connection to the skill registry

The `skills:` front-matter key names skills by the same kind of identifier the
[skill registry](skills.md) (`/data/skills/`, `pkg/skillreg`) resolves. There
are two request paths with different precedence:

- `pkg/agentsmd` resolves a skill name to text from an inline `## Skill:`
  section or a `skills/<name>.md` file **in the same repo** — self-contained,
  and it does not consult the registry at all.
- An agent's own `skills:` config is resolved by `pkg/skillreg`: it checks the
  **hive-wide** `/data/skills/` store first, then falls back to the primary
  repo's inline or adjacent definition when that repo has a configured checkout.
  Registry definitions therefore retain precedence when both sources use the
  same name, while a missing checkout does not affect registry-only operation.

Both reach a live kick prompt. A repo's front-matter request stays self-contained
inside `pkg/agentsmd`; registry precedence applies to names requested by the
agent config through `pkg/skillreg.ResolveRequested`.

## What to read next

- **[Skill registry](skills.md)** — the sibling injection path: file format,
  where it's loaded from, and how an agent declares which skills it wants.
- **[Agent configuration](agent-configuration.md)** — the `definition_source`
  and `channels`/`tools`/`connections` fields that *are* live.
- **[ADR-0012: Skill registry](adr/0012-skill-registry.md)** — the design
  rationale that also describes `AGENTS.md` inline snippets.
- **[ClankeR contributor relay](contributor-relay.md)** — the per-backend
  instruction file table (`AGENTS.md`, `CLAUDE.md`, `.goosehints`, etc.) for
  CLI-side instruction conventions unrelated to this scheduler-side injection
  gap.
