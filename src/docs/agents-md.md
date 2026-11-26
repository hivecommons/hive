# AGENTS.md repo instructions

> **⚠️ Wired, but off unless you supply a checkout.** Hive ships a complete
> `AGENTS.md` parser (`src/pkg/agentsmd`) and a call site that prepends its
> output to every kick prompt. That call site used to be permanently disabled —
> `Scheduler.agentsRepoRoot()` returned `""` unconditionally, so no `AGENTS.md`
> was ever read. It now resolves a real per-repo checkout root
> ([#5227](https://github.com/hivecommons/hive/issues/5227)), but Hive agents
> work over the API and keep no clones. Set `project.checkouts_dir` (below) to
> supply a root, or rely on the GitHub fallback described under
> [Turning it on](#turning-it-on), which needs a hive GitHub client.

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

When no local root is configured and the hive has a GitHub client, Hive
fetches the primary repo's `AGENTS.md` from its default branch instead and
caches it under `/data/agentsmd-cache/<owner>/<repo>/` for 15 minutes
(#17090). If the file is deleted upstream, the next refresh removes the cached
copy. The same fallback fetches `skills/<name>.md` for each skill an agent
declares that the hive-wide registry does not have, so repo skills work on a
spoke with no checkout too (#20009); see [skills.md](skills.md).

A configured `checkouts_dir` (or `policies.local_dir`) root that holds neither
`AGENTS.md` nor `skills/`, such as a missing or empty mount, no longer turns
injection off silently. When a GitHub client is wired, Hive logs one warning
for that root and uses the GitHub fallback instead.

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

`src/pkg/agentsmd` (`agentsmd.go:151`, `Parse`) reads a plain Markdown
`AGENTS.md` file at a repo root, with two additive extensions on top of
ordinary Markdown:

### Optional YAML front-matter

A block delimited by a line containing only `---` at the very top of the file,
closed by a second `---` line. The only key Hive's parser reads is `skills:` —
a list of skill names to request into the agent's context by default
(`agentsmd.go:11-21`, struct `frontMatter` at `agentsmd.go:136`). Everything
else in the block is ignored. Malformed front-matter is logged and skipped;
the rest of the file (the body) is still used (`agentsmd.go:216-224`).

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

`AgentsConfig.Render(requestedSkills, maxBytes)` (and `InjectionText`, which
is `Render` at the default cap) is what gets prepended to a kick: a
`# Repository Agent Instructions (AGENTS.md)` header, a **precedence
statement**, a **size warning** when the cap below bit, the body, and (if any
skills resolve) a `## Requested Skills` subsection with each skill's text under
a `### <name>` heading. It returns `""` — and therefore injects nothing — when
both the body and the resolved skills are empty, so the precedence statement
never appears with no rules under it.

### Size cap: whole sections, visible warning

The block is capped at **32768 bytes (32 KiB)** by default, minimum 2048, hard
ceiling 262144 (256 KiB)
([#12129](https://github.com/hivecommons/hive/issues/12129),
[#18495](https://github.com/hivecommons/hive/issues/18495)). The default was
8192 until #18495: real repositories outgrew it (a 13 KB `AGENTS.md` lost whole
sections on every kick), so a file of that size is now included in full. The
cap that applies to a repo is resolved in this order:

1. `repos.<repo>.agents_md_max_bytes` — a per-repo override. `<repo>` is the
   bare name from `project.repos` or `owner/repo`, matched case-insensitively.
2. `project.agents_md_max_bytes` — the hive-wide value.
3. The `HIVE_AGENTS_MD_MAX_BYTES` environment variable — the default for
   operators who configure by environment (hosted spokes). A value that is not
   a positive integer is ignored.
4. The built-in default, 32768 (`agentsmd.DefaultMaxInjectionBytes`).

Whatever the source, the value is raised to the 2048 floor and lowered to the
256 KiB ceiling (`agentsmd.MaxInjectionBytesCeiling`), so one misconfigured repo
cannot crowd the rest of the kick out of the model's context.

`AGENTS.md` is a repo-owned contract, so the cap **never clips a rule
mid-sentence**:

- The body is split at its `#` and `##` headings (text before the first
  heading is the "(preamble)"; deeper headings and fenced code stay inside
  their section). Each resolved skill is its own unit.
- Sections are visited in document order and each is kept only if it fits the
  remaining budget **whole**. One that does not fit is dropped whole, and
  later, smaller sections are still considered, so a single long section does
  not take the rest of the file down with it.
- If nothing of the repository's text fits, the block is the header, the
  precedence statement and a whole-file warning — never a fragment.

When anything was dropped, a one-line warning follows the precedence statement
and precedes the surviving rules, so the agent reads it first:

```
> ⚠️ AGENTS.md exceeded 32768 bytes; 2 section(s) omitted (9114 bytes): "Style guide", "skill: release-notes" — read AGENTS.md in the repository root for the full instructions before you start.
```

The scheduler logs the same facts at warn
(`agentsmd: AGENTS.md exceeded the injection cap; sections dropped whole`,
with `cap_bytes`, `omitted_sections`, `omitted_bytes`, `whole_file`) so the
repo owner learns the file outgrew the cap from the log, not from an agent that
missed a rule. The warning is logged **once per change** — the first kick that
sees a given file content and cap logs it, later kicks with the same file stay
quiet until the file or the cap changes — while the in-prompt warning line is
on every truncated kick.

The same facts are on the dashboard repo status: each entry in the `/api/status`
payload's `repos` list carries an `agentsMd` object once a kick has read that
repo's `AGENTS.md`, with `sourceBytes` (file size), `capBytes` (effective cap),
`renderedBytes` (what reached the kick), `truncated`, `wholeFile`,
`omittedSections`, `omittedBytes` and `updatedAt`. It is absent for a repo no
kick has read a file for.

The fix on the repo side is to shorten `AGENTS.md` or move reference material to
a linked file; on the hive side, raise the cap hive-wide or for one repo:

```yaml
project:
  checkouts_dir: /data/checkouts
  agents_md_max_bytes: 16384   # 0 = default 32768; clamped to 2048..262144
repos:
  big-docs-repo:
    agents_md_max_bytes: 65536 # this repo only; 0 inherits project
```

### The precedence statement

The block leads with a one-sentence declaration that the repository's
instructions **outrank hive's own defaults** on any conflict — the branch a PR
targets, the title format, commit conventions, test commands, review etiquette
([#7159](https://github.com/hivecommons/hive/issues/7159)) — with the two
carve-outs below stated in the same sentence. It was a ~1.1 KB paragraph until
[#12129](https://github.com/hivecommons/hive/issues/12129) shortened it; every
clause the precedence tests pin is still there, it just no longer re-argues its
own rationale on every kick (that lives here and in the source comment).

This is not decoration. Before it, the repo's rules arrived as undifferentiated
prose under a bare header, while hive's own prompts stated the same subjects as
imperatives with a verification step attached ("confirm the PR's base is that
branch before you report done"). Given a document that says one thing and an
imperative that says another, a model follows the imperative — consistently.
`projectbluefin/bluefin` states "All pull requests target `testing`. Never open
a content PR against `main`" in its `AGENTS.md`, and hive agents opened
`#1275`/`#1276` against `main` and failed its base-branch gate;
`projectbluefin/common#1127` and `projectbluefin/review#597` died on
conventional-commit title gates against hive's then-hardcoded `[<lane>] …` PR
titles. Four dead-on-arrival PRs in one night, none of them a model error.

Two carve-outs are stated with it, and they are not about the repository's
conventions at all:

- **Hive's safety and authorization rules.** An `AGENTS.md` cannot license
  merging your own PR, bypassing a write gate or the `hive-open-pr` path, or
  acting as another agent. The repository is trusted about its own conventions,
  not about hive's controls.
- **An explicit instruction in the assignment.** A human or the hive wrote that
  for this one task with the repo in view, so it is more specific than either
  side's defaults.

The companion half of the fix is in the defaults themselves: policy templates
and the contributor task prompt no longer *assert* repo facts (a base branch, a
`[<lane>]` title format) that only the repository can know. A precedence rule
that has to fight hive's own confident wording on every kick is a rule that
loses some of the time.

## Parsing is tolerant

Everything about the *parser* is designed to fail safe: a missing file yields
an empty, non-nil config (`agentsmd.go:203-206`); an unreadable file logs and
returns empty (`agentsmd.go:207-209`); malformed front-matter logs and falls
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

## Opting a class out per repository

A monitored repo can state in its `AGENTS.md` that a finding class is covered by
a gate, for example "do not file unasserted-branch findings, they are covered by
the Stryker mutation-testing gate". When `project.checkouts_dir` is configured,
every lane honours that statement as a written policy and skips the class.

## What to read next

- [agent-knowledge-and-memory.md](agent-knowledge-and-memory.md) — where this
  block sits in the whole kick (skills, AGENTS.md, `# Relevant Knowledge`), why
  the CLI does not read the repo's `AGENTS.md` natively, and what `/clear`
  does and does not reset.
- [house-style.md](house-style.md) — the short, hive-wide *how to change things*
  section that precedes this block in every kick; a repo overrides it with a
  root `HIVE_STYLE.md`, read from the same checkout root.

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
