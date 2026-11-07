# Repository knowledge entries (`src/docs/knowledge/`)

`AGENTS.md` carries tone and working style only. Facts about this repository —
module layout, CI guards, the exact changelog exemptions, gotchas — live as
discrete, scoped **knowledge entries** under `src/docs/knowledge/`, one topic
per file, so they are surfaced when a task needs them instead of being paid for
on every kick (proposed by @gregoryhunt at the 2026-11-05 community meeting,
hivecommons/hive#17043).

How a kick receives knowledge end to end (primer, AGENTS.md block, skills,
memory) is described in `agent-knowledge-and-memory.md`; this page is only the
entry format and how the directory reaches the primer.

## Entry format

An entry is an Obsidian-compatible Markdown file with YAML frontmatter — the
same shape `pkg/knowledge`'s `FileStore` already indexes for vaults, git sources
and the bead-synth wiki, plus an additive `scope:` block:

```markdown
---
title: Editing src/scripts/ requires re-pinning TRUSTED_SCRIPTS_TREE
type: gotcha
layer: project
state: approved
tags: [repo:hivecommons/hive, ci, governance, scripts]
source: AGENTS.md (moved 2026-11-07, hivecommons/hive#17047)
scope:
  paths: [src/scripts/**, .github/scripts/integration-governance.cjs]
  labels: []
  lanes: []
---

A PR that edits anything under `src/scripts/` must re-pin …
```

| Key | Meaning |
|---|---|
| `title` | One line; this is what the agent sees in bold in `# Relevant Knowledge`. |
| `type` | `gotcha`, `pattern`, `reference`, `decision`, `regression`, … — drives the primer's `knowledge.primer.priority` ordering and the section heading. |
| `layer` | `project` for repository facts. |
| `state` | Lifecycle (`approved`, `draft`, `deprecated`, `superseded`); only `approved` is primed by default. See [knowledge-lifecycle.md](knowledge-lifecycle.md). |
| `tags` | Inline list. Always include `repo:<org>/<name>` — that is how `knowledge.agent_scopes.repos` recognises the entry's repository. |
| `source` | Where the fact came from, so a reader can trace a moved `AGENTS.md` line to exactly one entry. |
| `author` | Who wrote the entry — an agent name (`developer`) or a login (alias `owner`). A `draft` reaches a kick only for the agent that matches this and has opted into drafts; see [knowledge-toc.md](knowledge-toc.md#per-agent-scopes). |
| `scope` | **Where the fact applies.** `paths` are repo-relative globs (`**` supported), `labels` are issue labels, `lanes` are base agent/lane names. An empty list means "no constraint on that dimension". |

Rules of thumb:

- One topic per file; the slug is the filename. Keep the body to what an agent
  needs to act — the primer drops whole entries that do not fit the kick cap
  (`knowledge.primer.max_bytes`, default 4096 bytes).
- Do not put `file.go:NN` citations in an entry; the docs citation guard would
  then track them and they would drift.
- Inline lists only (`[a, b]`). The frontmatter parser is line-oriented and
  block lists (`- a`) are not supported inside `scope:`.
- Use `state: deprecated` or `superseded_by:` instead of deleting a fact that
  stopped being true; the lifecycle filter hides it and the history stays.

## Scope semantics

An entry **without** a `scope:` block behaves exactly as every other knowledge
fact: it is selected by keyword relevance (BM25 + Fisher-Rao over the issue
titles and labels in the kick) and by the agent's `knowledge.agent_scopes`.

An entry **with** a `scope:` block is additionally gated: it is surfaced only
when at least one declared dimension matches the kick — the kick's lane is in
`lanes`, one of the primed issues carries a label in `labels`, or a file the
agent is working on matches a `paths` glob. Dimensions are OR-ed, so an entry
scoped to `paths: [src/scripts/**]` and `labels: [area/ci]` shows up for either
trigger. At kick time Hive knows the lane and the issue labels; file paths are
known only to callers that have them (the mid-task advisor, tracked
separately), so a `paths`-only entry is a lookup target for an agent reading
this directory from its checkout until that lands. The gate is implemented by
hivecommons/hive#17045.

## How the directory reaches the primer

The primer reads vaults and git sources, not the repository checkout, so the
directory has to be registered once in `hive.yaml`:

```yaml
knowledge:
  enabled: true
  git_sources:
    - name: hive-repo-knowledge
      url: https://github.com/hivecommons/hive
      branch: v5
      subpath: src/docs/knowledge
      layer: project
```

or, where the hive already has a checkout of the repo, as a vault pointing at
`<checkout>/src/docs/knowledge`. Either way the files are indexed by
`FileStore`, merged with the other layers by precedence, scoped by
`knowledge.agent_scopes`, and rendered into the kick's `# Relevant Knowledge`
block under the usual cap. Nothing in the entries is specific to Hive's
injection path: a CLI agent working from a plain checkout reads the same files
directly, picking the entries whose `scope.paths` match what it is about to
touch.

## Entries moved out of AGENTS.md

| Entry | Was | Applies when |
|---|---|---|
| `go-workspace-two-modules.md` | "src/ is a two-module Go workspace …" | editing Go under `src/` |
| `ci-is-the-verdict.md` | rationale behind "CI runs the tests, you do not" | any `src/` change, test-failure issues |
| `agent-pane-test-suite-hazard.md` | tmux / `/proc` / SIGKILL rationale, #9416 | `src/crux/agent/`, `src/pkg/dashboard/` |
| `changelog-fragment-exemptions.md` | the exact exemption set, `no-changelog`, `src/changelog.d/` trap | any `src/` or `changelog.d/` change |
| `docs-citation-guard.md` | `check-docs-citations.py --fix` how-to and what triggers the guard | `src/docs/`, hub/dashboard `server.go` and `static/` |
| `trusted-scripts-tree-repin.md` | `TRUSTED_SCRIPTS_TREE` re-pin | `src/scripts/`, `integration-governance.cjs` |

The rules themselves (do not run the suite locally, never run it in an agent
pane, fragment required, `--fix` first) stay in `AGENTS.md`; only the facts and
reasons moved.
