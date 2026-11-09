# Draining CLI auto-memory into draft knowledge (#17033)

Status: **shipped.** The three open questions in #17033 are decided below;
the work is cut into three implementation children, listed in
[Implementation plan](#implementation-plan). Each phase shipped with the
differences recorded in [Phase 1 as shipped](#phase-1-as-shipped),
[Phase 2 as shipped](#phase-2-as-shipped) and
[Phase 3 as shipped](#phase-3-as-shipped).

All citations are against `origin/v5` at `7a929356`. The CLI-memory isolation
and disable work this design builds on (#17031, #17032) is in open PRs
[#17039](https://github.com/hivecommons/hive/pull/17039) and
[#17040](https://github.com/hivecommons/hive/pull/17040). Their symbols are
named below without line numbers because they are not on `v5` yet.

## The problem: a hidden, per-agent store

Every coding CLI Hive drives keeps its own persistent memory across sessions:

| CLI | Store | Written by |
|---|---|---|
| Claude Code | `~/.claude/projects/<project>/memory/MEMORY.md` plus sibling topic files | auto memory |
| Codex | `$CODEX_HOME/memories`, `$CODEX_HOME/memories_v2` | the memories feature |
| Gemini CLI | `~/.gemini/GEMINI.md` (the `save_memory` tool appends to it) | the model |
| Copilot CLI | none on disk — Copilot Memory is stored on GitHub | — |

Hive does not see what is in those stores. A one-off inference written on
Tuesday comes back on every later session, it is not scoped, nobody reviews
it, and until #17039 lands it is shared across agents: v5 bridges `~/.claude`,
`~/.gemini` and `~/.codex` whole into the shared home
(`src/crux/agent/interactive_home.go:68-71`).

From the 2026-11-05 community meeting, quoted in #17033:

> the CLIs' built-in memory "creates its own hidden bead storage … like 60% of
> these should have been beads … is there a create-memory hook where instead
> we could overload that for our CLIs and make it inject a bead instead? I
> pulled them down locally, created them as beads, and turned that feature off
> on the CLI."

#17040 does the "turned that feature off" half. With `agents.cli_memory` it
seeds each CLI's documented disable (`autoMemoryEnabled: false` for Claude
Code, `[features] memories = false` for Codex). With `clear_on_kick` it wipes
the agent's own store before each kick (`clearCLIMemoryLocked`, called from
`deliverKickLocked`). This design does the "created them as beads" half: what
a CLI writes to its memory store becomes a reviewable, scoped,
lifecycle-managed knowledge entry instead of being lost or kept hidden.

## The destination already exists

Nothing new is needed on the read side.

- **Lifecycle states.** Every fact is `draft`, `approved`, `deprecated` or
  `superseded` (`src/pkg/knowledge/lifecycle.go:16-21`). Only `approved`
  passes `lifecycleAllowed` unless a caller opts other states in
  (`src/pkg/knowledge/lifecycle.go:88-98`). The kick primer filters with
  `FilterByLifecycle` (`src/pkg/knowledge/primer.go:144`). See
  [Knowledge lifecycle states](../knowledge-lifecycle.md).
- **Per-agent scope.** `knowledge.agent_scopes`
  (`src/pkg/config/knowledge_config.go:43-46`) binds an agent to layers,
  repos, types, tags and admitted states. It is resolved for the kick primer in
  `primeKnowledge` (`src/pkg/scheduler/kick_priming.go:261-310`). See
  [Scoped knowledge table of contents](../knowledge-toc.md).
- **A write path.** `POST /api/knowledge/create`
  (`src/pkg/dashboard/api.go:389`, handler at
  `src/pkg/dashboard/api_knowledge.go:577-610`) creates a fact from a
  `CreateFactRequest` (`src/pkg/knowledge/api.go:293-300`) and audits it as
  `knowledge_create_fact`. `bd kb` already talks to the dashboard API rather
  than the local bead store ([Beads CLI](../beads-cli.md)).
- **State changes and review.** `PUT /api/knowledge/entry/{id}/state` and
  `hivectl knowledge state` move a local-channel fact between states with an
  audit entry. `UpdateFactRequest` (`src/pkg/knowledge/api.go:335-344`)
  already carries `tags` and `state`.
- **The curator.** `knowledge.curator` (`KnowledgeCurator`,
  `src/pkg/config/knowledge_config.go:269-289`) drives
  `Promoter.AutoPromoteCandidates` (`src/pkg/knowledge/promote.go:127`) on a
  schedule through `PromotionScheduler`
  (`src/pkg/knowledge/curator_schedule.go:31`). It is opt-in and off by
  default. See [Knowledge promotion configuration](../knowledge-curator.md).

Two gaps on the write side remain, and phase 1 closes them:

1. `bd create` has no `knowledge` type (`src/docs/beads-cli.md:11` lists the
   types), and there is no `bd` verb that creates a fact.
2. `CreateFactRequest` has no `state` field, so there is no way to create a
   fact as `draft`. A fact with no `state:` frontmatter loads as `approved`, so
   a drained entry written through today's create path would reach primers
   immediately. That is the reason the field is required, not a nicety.

What is missing is the **drain**: a step that moves what is in each store into
`draft` facts, then empties the store.

## Bead shape

Each memory entry becomes one knowledge fact, created through the same verb
the hook and the manager both use:

```bash
bd create --type knowledge --state draft \
  --tag auto-memory --tag agent:<name> --tag cli:<claude|codex|gemini> \
  --tag memhash:<16 hex> \
  --title "<first line of the entry, ≤ 80 chars>" --body-file <entry>
```

- `bd create --type knowledge` is routed to `POST /api/knowledge/create` (as
  `bd kb` is), not to the local bead store. That keeps facts in one place: the
  vault the curator, the TOC and `hivectl knowledge state` already work on.
- **Layer:** `project`, in the local vault channel, so the existing
  `hivectl knowledge state` path (local channels only) can approve or discard
  it. Type defaults to `pattern`, the existing create default.
- **State:** always `draft`. The create path rejects `--tag auto-memory`
  with any other state, so a caller cannot create an approved entry directly.
- **Tags:** an allow-list, set only by the drain code and never parsed out of
  the entry: `auto-memory`, `agent:<name>`, `cli:<backend>`, `memhash:<hex>`.
  `<name>` must match the agent-name grammar and `<backend>` is one of the
  three CLIs. Anything else is refused.
- **Entry splitting.** Claude Code: one entry per topic file in the memory
  directory, plus one per top-level bullet or `##` section of `MEMORY.md`
  (the index lines that only link to a topic file are dropped). Gemini: one
  entry per bullet under the `## Gemini Added Memories` heading of
  `GEMINI.md`; the rest of the file is operator context and is never touched.
  Codex: one entry per file under `memories/` and `memories_v2/` (the on-disk
  format is not a stable contract, so files are taken whole).
- **Content hash.** `memhash` is the first 16 hex characters of the SHA-256
  of the normalised entry: LF line endings, trailing whitespace stripped,
  runs of blank lines collapsed. The hash is over the body only, never the
  title or the agent.
- **Size cap.** An entry over 8 KiB is stored truncated with a visible
  `[truncated by drain]` marker. The hash covers the full normalised entry.

### Truncating the store

The store is emptied only after every entry from that file has either created
a fact or matched an existing one (see dedup below). Claude topic files are
removed and `MEMORY.md` is truncated to zero bytes. The Gemini memories section
is removed and the rest of `GEMINI.md` is kept. Codex files are removed.

A file whose drain fails part-way is left untouched and retried on the next
drain. That is safe because dedup makes the drain idempotent.

Reads and the truncate follow the same rules as #17040's
`cliMemoryStores`. Only stores private to the agent are drained. The plan
was to `Lstat` every path component and to truncate and remove as the agent
through the same `su-exec` path as `cliMemoryRemover`; as shipped, the drain
runs in the hive process and instead opens every memory file through an
`os.Root` anchored at its store (see "Phase 2 as shipped" below and #17251).
A store that is still shared (a legacy whole-directory
bridge, a UID 0 agent, `HIVE_SHARED_AGENT_HOME=1`) is **not** drained. It
cannot be attributed to one agent, and truncating it would wipe other agents'
memory. It is logged once per agent, exactly like `clear_on_kick`, and is left
to the one-shot legacy migration in phase 3.

## The drain, per CLI

The drain logic lives in one Go package inside the Crux module
(`src/crux/agent/memdrain`, beside `cli_memory.go`). It does not live in
`src/pkg`: the agent manager that calls it is in `src/crux`, and the Crux
outbound ratchet (`src/internal/testutil/crux_outbound_ratchet_test.go`)
forbids new imports from Crux into `src/pkg`. The package talks to the
knowledge store only over the dashboard API, the same way `bd` does, so it
needs no `pkg/knowledge` import. A thin `hive memory-drain` subcommand wraps
it for the Claude hook.

### Claude Code: a `SessionEnd` hook

Claude Code has a hook surface
([hooks reference](https://code.claude.com/docs/en/hooks)), so its drain runs
inside the session's own lifecycle rather than waiting for the next kick:

- **`SessionEnd`** runs `hive memory-drain --cli claude --agent "$HIVE_AGENT"`
  over `$HOME/.claude/projects/*/memory/`. The hook gets the session `cwd` on
  stdin, which narrows the drain to that project's directory.
- **`PostToolUse`** on `Write|Edit|MultiEdit` is optional and off by default.
  Its command exits at once unless the tool's `file_path` is under
  `~/.claude/projects/*/memory/`, so the cost on every other write is one
  process spawn and a path compare. It exists for long-lived sessions that
  rarely end. `SessionEnd` alone is the floor.
- **Seeding.** The hooks are merged into the settings Hive already seeds. For
  interactive agents that is `sharedClaudeSettingsFile`
  (`src/crux/agent/claude_remote_control.go:44-46`), which #17039 keeps as a
  per-agent bridge to one fleet file. For inference agents it is the two files
  `ensureClaudeSettings` writes (`src/crux/agent/manager_homes.go:624`). The
  merge goes through `seedJSONFile` (`src/crux/agent/manager_homes.go:582`),
  so an operator's own `hooks` entries are kept and Hive adds its entry only
  when it is absent. The command reads the agent from `HIVE_AGENT`, so one
  fleet-wide entry serves every agent.
- **Backstop.** A session killed hard does not run `SessionEnd`, so the
  manager also drains Claude stores at kick time (below). Dedup makes the
  double coverage free.

### Codex, Gemini CLI: manager-side, at kick time

Neither CLI documents a hook that fires on a memory write. The manager drains
their stores itself at the same point #17040 clears them: inside
`deliverKickLocked` (`src/crux/agent/manager_kick.go:242`), after
`rotateKickLogOnKickLocked` (`src/crux/agent/manager_kick.go:271`) and
**before** `clearCLIMemoryLocked`.

- **Codex:** `$CODEX_HOME/memories` and `$CODEX_HOME/memories_v2`, the paths
  `cliMemoryStores` already resolves for `codexBackend`.
- **Gemini CLI:** the `## Gemini Added Memories` section of
  `$HOME/.gemini/GEMINI.md`, the file `cliMemoryStores` already resolves.
- **Claude Code:** the hook backstop described above, over the same
  `projects/*/memory` paths.
- **Copilot CLI:** nothing to drain. Copilot Memory is stored on GitHub, not on
  disk, as #17040 records. `~/.copilot/session-state` holds transcripts, not
  memory, and is out of scope. If Copilot CLI ships a local memory store, it
  joins the table above.

Ordering with `clear_on_kick`: drain first, then clear. If the drain fails
(dashboard unreachable, knowledge disabled), the files are **moved** into a
Hive-owned spool (`/data/knowledge/auto-memory-spool/<agent>/`) instead of
being left in place. The clear still runs, so the kick still starts clean, and
the next drain retries the spool first. Neither the drain nor the spool ever
blocks a kick. Both are best-effort and logged, like `clearCLIMemoryLocked`.

### Configuration

One new key next to #17040's block, off by default:

```yaml
agents:
  cli_memory:
    auto_memory: false     # #17040
    clear_on_kick: true    # #17040
    drain: false           # this design: drain stores into draft knowledge
    drain_on_write: false  # Claude PostToolUse hook (needs drain: true)
```

`drain` is useful mainly to operators who turn `auto_memory` back on, and to
the one-shot migration of stores written before #17040. It needs
`knowledge.enabled`. With knowledge off, the drain logs once and does nothing,
and the store is left alone (or cleared, if `clear_on_kick` says so).

## Decisions on the open questions

#17033 left three questions open. They are decided here. The maintainer can
override any of them on #17033 or on the phase that implements it.

### 1. Dedup and merge across agents

**Decision taken by hive:** dedup on the content hash at create time. Before
creating, the drain looks up `memhash:<hex>` across **every** lifecycle state.

- No match: create a new `draft`.
- Match: no new fact is created. If the match lacks this agent's
  `agent:<name>` tag, the drain adds it through `UpdateFactRequest.Tags`. A
  second agent reaching the same inference becomes a second tag on the one
  fact, not a second fact.
- Match in `deprecated` or `superseded`: the tag is still added, but the state
  is **not** reset. A discarded memory that a CLI writes again stays discarded.
  Otherwise the drain would undo the reviewer's work every session.

Semantic merge (two differently-worded entries that say the same thing) is
**not** done by the drain. That is the reviewer's call, made with the existing
`superseded_by` link.

**Rationale.** An exact hash is deterministic, cheap and cannot merge two
different claims by mistake. Hive's knowledge design already prefers
deterministic steps over using one model to verify another
([knowledge system](knowledge-system.md#context)). The `agent:` tag count is
also a useful review signal: an entry three agents wrote on their own is worth
reading first.

### 2. The curator or a bespoke hook

**Decision taken by hive:** the drain is a thin **ingest** step, a hook for
Claude Code and the manager for the rest. It only ever creates `draft` facts
and adds tags. The existing `knowledge.curator` remains the **sole promoter**;
no second curator is built. Approval of a drained draft goes through the paths
that already exist: `hivectl knowledge state <id> approved`,
`PUT /api/knowledge/entry/{id}/state`, or the dashboard knowledge view.

**Rationale.** The curator answers *what is worth promoting*; the drain only
answers *where this text goes*. Running ingest through the curator would tie
memory capture to `knowledge.curator.enabled`, which is off by default. It
would also run on the curator's schedule rather than at session end, and would
mix untrusted input into the only component allowed to raise trust. A
second promoter would split review across two places. So the drain is
ingest-only and the curator stays the only path to `approved`. `knowledge.curator`
today only promotes facts between layers. Phase 3 makes sure it never selects
an `auto-memory` draft.

### 3. Prompt-injection posture

**Decision taken by hive:** drained text is **untrusted, agent-written
content**.

- The body is stored verbatim as the fact body. It is never interpreted, so
  nothing in it can set the title's provenance, the tags, the state, the layer
  or the type. Frontmatter-looking lines in the body are escaped so they
  cannot become frontmatter.
- Tags come only from the allow-list above and are set by the drain code,
  never parsed from the entry.
- No `auto-memory` fact is rendered into any kick, primer, TOC entry an agent
  reads, or public MCP response while it is `draft`. This holds even for an
  agent whose `knowledge.agent_scopes` entry admits `draft` through
  `include_states`. Phase 3 adds that hard exclusion to the primer and the
  agent-identified TOC/entry reads. A human, or the curator's approval path,
  must move the fact to `approved` first. Operator reads (`GET /api/knowledge`,
  the dashboard) still show drafts so they can be reviewed.
- The drain never runs the content and never follows links in it.

**Rationale.** A memory file is written by a model that may have read
attacker-controlled text (an issue body, a PR comment, a fetched page). If the
drain auto-primed that text, a single poisoned session would become an
instruction delivered to every later kick, across agents. That is worse than
the per-agent store it replaces. Keeping it `draft` and unread until a reviewer
approves it is the scoping asked for in #17033: "I don't want a developer agent
working another ticket to pull that information in yet."

## Relation to the knowledge-system design

This resolves open question 5 of [knowledge-system.md](knowledge-system.md)
("should `~/.claude/projects/*/memory/` be migrated into the wiki?"). The
answer is yes: as `draft` knowledge facts, through the drain, after which the
memory file is emptied. The destination is the project layer's local vault
rather than a separate personal wiki, so the existing review and scope tooling
applies.

## Implementation plan

Each phase is one child issue of #17033. Phases 2 and 3 are blocked by phase 1.
Phase 1 depends on #17040 (it reuses `cliMemoryStores` and runs beside
`clearCLIMemoryLocked`). Attribution to one agent depends on #17039 (per-agent
CLI homes).

### Phase 1 — drain library and manager-side drain (Codex, Gemini, Claude backstop)

Child: #17056.

- `CreateFactRequest` gains `state` (`draft` or `approved`, default unchanged).
  `POST /api/knowledge/create` refuses `auto-memory`-tagged requests that are
  not `draft`, and refuses tags outside the allow-list on those requests.
- `bd create --type knowledge [--state] [--tag]…` routes to that endpoint.
  Documented in [Beads CLI](../beads-cli.md).
- `src/crux/agent/memdrain`: read a private store, split, normalise, hash, look
  up `memhash` across all states, create the draft or add the `agent:` tag,
  truncate after success, spool on failure.
- `agents.cli_memory.drain` config key with the settings-parity entry.
- Manager drain in `deliverKickLocked` before `clearCLIMemoryLocked`, for
  Codex, Gemini and (as backstop) Claude Code stores.
- Tests: splitting per CLI, hash stability, dedup-adds-tag, a discarded fact is
  not revived, partial failure leaves the file, symlinked or shared stores are
  skipped, spool and retry, kick never blocked.

#### Phase 1 as shipped

Phase 1 landed in #17086 and the follow-up for #17056. Where the shipped code
differs from the plan above, the shipped code is authoritative:

- **Drain library.** The drain is `src/pkg/knowledge/climemory`, run in the
  hive process and installed on the in-process runtime through
  `crux.CLIMemoryDrainHost`, not an HTTP-only `src/crux/agent/memdrain`. It
  writes into the `bead-synth-wiki` vault the Primer already reads, so the
  Crux module still imports nothing from `src/pkg`.
- **Names.** The config key is `agents.cli_memory.drain_to_knowledge`, and
  the provenance tag is `cli-memory` (with `source: cli-memory` frontmatter)
  rather than `auto-memory`.
- **Dedup.** As designed: a duplicate in any state only gains the draining
  agent's `agent:<name>` tag; its state is never reset.
- **Store emptying.** The drain does not truncate the store; `clear_on_kick`
  does. A file whose facts did not all reach the vault is **copied** to
  `/data/knowledge/auto-memory-spool/<agent>/` before the wipe and retried on
  the next drain, which gives the "partial failure loses nothing" property
  without a second remover running as the agent.
- **Create path.** `CreateFactRequest.state` and the `cli-memory` tag
  allow-list on `POST /api/knowledge/create`, and `bd create --type knowledge`,
  shipped as planned; a draft is written into the local channel named by
  `layer`.

### Phase 2 — Claude Code `SessionEnd` and `PostToolUse` hooks

Child: #17057.

- `hive memory-drain` subcommand wrapping the phase-1 library (reads hook JSON
  on stdin, `HIVE_AGENT` from the environment).
- Seed `hooks.SessionEnd` (and `hooks.PostToolUse`, matcher
  `Write|Edit|MultiEdit`, when `drain_on_write` is on) into the Hive-seeded
  Claude settings through `seedJSONFile`, merge-only, and remove Hive's own
  entry when `drain` is turned off.
- Tests: seed and merge keep operator hooks, the hook exits fast on writes
  outside the memory directory, end-to-end hook → draft fact → empty
  `MEMORY.md`.

#### Phase 2 entry point (#17208)

The hook runs as the agent's UID, so it can neither write the hive-owned
vault nor reach the in-process drainer. It asks the hive instead: the
`hive memory-drain` command (#17209) drops `{"agent", "cwd", "truncate"}` into
`/var/run/hive-metrics/memory-drain-requests`, the same drop-box as the GitHub
relays, and reads `<name>.result.json` back.

- **Auth.** The owner UID of the request file is the caller's identity. The
  runtime (`crux.CLIMemoryDrainNowHost`) refuses a request naming any other
  agent. Every request, refused or not, is audited as
  `cli_memory_drain_requested`.
- **Sources.** Resolved from the agent's own home with
  `cliMemoryDrainSources`, never from the request. `cwd` only narrows them to
  the Claude project slug Claude derives from it.
- **Truncate.** With `truncate`, a drain that reports no error is followed by
  `Drainer.TruncateDrained`, which empties only the files whose every
  `memhash` is in the vault.
- **Drain off.** With `agents.cli_memory.drain_to_knowledge` off for the
  agent, the answer is `ok` with `ran: false` and nothing drained.

#### Phase 2 as shipped

Phase 2 landed in #17208 (entry point above), #17209 and #17210. Where the
shipped code differs from the plan above, the shipped code is authoritative:

- **Command.** `hive memory-drain --cli claude --agent "$HIVE_AGENT"` does not
  wrap the drain library: it queues a request for the entry point above, with
  `truncate` set and the hook's `cwd`, and waits up to 10 seconds
  (`--wait`) for the answer. Every outcome exits 0; a refusal, a timeout or a
  missing request directory is a warning on stderr, and the kick-time drain
  remains the backstop.
- **PostToolUse.** The command exits before queuing anything unless
  `tool_input.file_path` is under `~/.claude/projects/<slug>/memory/` (or
  `$CLAUDE_CONFIG_DIR/projects/<slug>/memory/`). Its request carries
  `on_write`, which the hive answers as a no-op unless the agent has
  `drain_on_write`.
- **Open through the store root (#17251).** The drain never reads or
  truncates a memory file by path. Each `Source` is resolved with
  `EvalSymlinks` and dropped unless it still lies under the agent's own
  resolved home (a slug or `.claude` directory symlinked at another agent's
  store is not drained). The store is then opened as an `os.Root` (the parent
  directory for a single-file store such as `GEMINI.md`), checked with
  `SameFile` against what `Lstat` saw, and every listed file is `Lstat`ed
  through the root — a symlink entry is refused even when its target stays
  inside the root — opened through it, required to be a regular file within
  the size cap and the same inode `Lstat` reported, and `TruncateDrained`
  truncates through that read descriptor. A path swapped for a symlink
  between the listing and the open lands in `kept` and the drain error;
  nothing outside the store is ever read or emptied.
- **Keys.** The new key is `agents.cli_memory.drain_on_write`, fleet-wide and
  per agent, and it has no effect without `drain_to_knowledge`.
- **Seeding.** Not through `seedJSONFile`, which never touches an existing
  top-level key: a separate merge works per `hooks.<event>`. It adds Hive's
  entry only when no matcher group already runs the exact command, keeps
  every operator entry, and when the hook is off removes only Hive's command
  (and a group or event it leaves empty). The settings files are shared, so
  `SessionEnd` is seeded while any agent has `drain_to_knowledge` and
  `PostToolUse` while any agent has `drain_on_write`; the hive's per-agent
  answer keeps every other agent a no-op. Both are merged into the shared
  interactive settings file and into the two files `ensureClaudeSettings`
  writes for inference agents.

### Phase 3 — review flow, draft exclusion, legacy migration, operator docs

Child: #17058.

- Hard-exclude `auto-memory` facts in `draft` from the kick primer,
  agent-identified TOC and entry reads, and the public MCP, even when
  `include_states` admits `draft`. Make sure `Promoter.AutoPromoteCandidates`
  never selects them.
- A dashboard knowledge filter for `auto-memory` drafts that shows the
  `agent:` tags and approve and discard actions (backed by the existing state
  API).
- `hivectl knowledge drain-memory --legacy`: a one-shot, operator-run drain of
  the shared `/data/home/.claude/projects/*/memory` store that #17039 leaves
  behind. Tagged `agent:shared`, never truncated without `--truncate`.
- Operator docs: an `agents.cli_memory.drain` section in the operator
  reference, an "Auto-memory drafts" section in
  [Knowledge lifecycle states](../knowledge-lifecycle.md), and the status line
  in [the design index](README.md) moved to partly shipped or shipped.

#### Phase 3 as shipped

Where the shipped code differs from the plan above, the shipped code is
authoritative. User-facing behaviour is documented in
[Auto-memory drafts](../knowledge-lifecycle.md#auto-memory-drafts).

- **Marker.** The drain shipped with the tag `cli-memory` and
  `source: cli-memory` frontmatter, not `auto-memory`.
  `knowledge.IsAutoMemory` accepts either marker. A fact is excluded when
  its effective state is `draft`.
- **Exclusion.** `FilterByLifecycleFor` (the kick primer and the knowledge
  advisor) never admits an auto-memory draft. `TOCScope.HideAutoMemoryDrafts`
  drops it from every scope `AgentScope` builds and from every `?agent=` TOC,
  entry and effective read. The operator filter `FilterByLifecycle` and
  operator TOC reads still list it. The public MCP was already approved-only.
- **Review in the reserved vault.** Drained entries live in `bead-synth-wiki`,
  which `PUT /api/knowledge/entry/{id}/state` used to refuse outright. It now
  accepts entries whose frontmatter says `source: cli-memory`, because the
  drain never rewrites an existing entry. The dashboard's **Discard** sets
  `deprecated` rather than deleting, so the `memhash` keeps the fact from
  being re-drafted.
- **Auto-promotion.** `AutoPromoteCandidates` skips any page tagged
  `cli-memory`, whatever its status.
- **Legacy drain.** `hivectl knowledge drain-memory --legacy [--truncate]`
  calls the owner-only `POST /api/knowledge/cli-memory/drain-legacy`, which runs
  the phase-1 drainer in the hive process. With `--truncate` it empties only
  the files whose every fact's `memhash` is now in the vault.
- **Config key.** The operator reference documents the shipped key,
  `agents.cli_memory.drain_to_knowledge`. There is no separate
  `agents.cli_memory.drain`.

## References

- #17033 — this design's issue.
- #17031 / [#17039](https://github.com/hivecommons/hive/pull/17039) — per-agent CLI homes.
- #17032 / [#17040](https://github.com/hivecommons/hive/pull/17040) — `agents.cli_memory`: disable auto-memory, clear on kick.
- #11102 — knowledge lifecycle states.
- [Knowledge system design](knowledge-system.md), [knowledge lifecycle](../knowledge-lifecycle.md),
  [scoped knowledge TOC](../knowledge-toc.md), [knowledge curator](../knowledge-curator.md).
