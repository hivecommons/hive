# How Hive injects knowledge and memory into agents

> Written to answer the 2026-11-05 community-meeting question "how does
> Hive's memory/knowledge injection work today?". It describes `v5` as of
> this page's merge; the [Known gaps](#known-gaps--in-progress) section links
> the work that will change the memory half.

Hive agents receive context from **three distinct layers**, and only one of
them is Hive's own:

| Layer | Who owns it | Where it lives | Hive's role |
|---|---|---|---|
| **Knowledge** — curated facts about the repos and the fleet | Hive (`pkg/knowledge`) | File vaults under `/data/knowledge/…`, optional HTTP wiki layers | Selects a few facts per kick and **pushes** them into the kick prompt; also serves them for **pull** via API/MCP/`hivectl` |
| **Conversation** — the current chat in the CLI pane | The CLI | In the CLI process | Resets it before every kick (`/clear`) |
| **CLI memory** — the CLI's own persistent, cross-session notes (Claude Code auto-memory, Codex memories, Gemini `GEMINI.md`, …) | The CLI | Dot-directories under the agent's `HOME` | **None.** Hive neither feeds, reads, filters nor clears it; it only decides which agents share the directory |

The confusion at the meeting was between layers 2 and 3: "every agent clears
its context before it runs" is true of the conversation and false of CLI
memory.

## 1. Knowledge: the kick primer (push)

```
 knowledge.vaults / git_sources / connectors / bead_synthesizer vault
   (FileStore per source; HTTP wiki layers for personal/project/org/community)
                              │
   registerKnowledgeStores (cmd/hive/boot_knowledge_primer.go)
                              ▼
 ┌───────────────── Scheduler.primeKnowledge (pkg/scheduler/kick_priming.go) ───┐
 │ 1. extractKeywords ← titles + labels of the agent's first                    │
 │    maxIssuesToPrime (=5) queued issues                                       │
 │ 2. scope ← knowledge.agent_scopes[<agent>] (or base agent for a replica)     │
 │ 3. Primer.PrimeScoped (pkg/knowledge/primer.go)                              │
 │      search every layer/store → merge by layer precedence →                  │
 │      FilterByLifecycle (approved only) → scope.Filter →                      │
 │      Fisher-Rao rerank → type priority → cap at max_facts (25) →             │
 │      expandWithGraph (never past the scope) → Context7 hints                 │
 │ 4. PrimedKnowledge.Render (primer_render.go): "# Relevant Knowledge",        │
 │    grouped by type, whole facts dropped past max_bytes (4096),               │
 │    "_… truncated: N facts / M bytes omitted (size cap)._" marker             │
 └──────────────────────────────────────────────────────────────────────────────┘
                              │
   templates.go: skills section + AGENTS.md section + knowledge section
   substituted into the agent's kick template; lane kicks (lane_messages.go)
   append the knowledge section for scanner / quality / architect / <agent>
                              ▼
   tmux: [/clear ⏎]  then the kick text  ⏎        (crux/agent/manager_kick.go)
```

**Where the facts come from.** Everything the primer searches is registered
in `registerKnowledgeStores` (`src/cmd/hive/boot_knowledge_primer.go`):
configured `knowledge.vaults` (personal layer), `knowledge.git_sources`
(layer from config), connector output, the **bead synthesizer vault**
(closed beads from every agent, classified into fact types — the main
organic source on a running hive), plus any HTTP wiki layers in
`knowledge.layers`. The repo's own docs are *not* a source unless you index
them through a git source, connector or code map
([knowledge-curator.md](knowledge-curator.md)).

**How it is scoped.**

- *By the work, not the repo:* the query is keywords from the titles and
  labels of the agent's first five queued issues (`extractKeywords`,
  `isNoiseLabel`). There is no automatic per-repo or per-issue filter; a fact
  tagged with another repo can surface if the keywords match.
- *By agent:* `knowledge.agent_scopes.<agent>` is an allow-list of `layers`,
  `repos`, `types`, `tags` (`KnowledgeAgentScope` in
  `src/pkg/config/knowledge_config.go`). It is applied inside
  `Primer.PrimeScoped` *after* the lifecycle filter and can only narrow; graph
  expansion never pulls in an out-of-scope fact. A replica inherits its base
  agent's entry (`Config.KnowledgeAgentScope`). No entry means unrestricted.
- *By lifecycle:* kicks carry **approved facts only**. The kick primer is built
  with no `IncludeStates` (`newKnowledgePrimer` in `src/cmd/hive/main.go`), so
  `draft`, `deprecated` and `superseded` never reach a kick; `agent_scopes.
  include_states` only widens API/TOC reads that already asked for those
  states ([knowledge-lifecycle.md](knowledge-lifecycle.md)).

**Size caps** (all drop whole units, never clip, and leave a visible marker):

| Block | Cap | Config |
|---|---|---|
| `# Relevant Knowledge` | 25 facts, 4096 bytes (floor 512) | `knowledge.primer.max_facts`, `knowledge.primer.max_bytes` |
| Repo `AGENTS.md` | 8192 bytes (floor 2048), whole sections | `project.agents_md_max_bytes` (`agentsmd.DefaultMaxInjectionBytes`) |
| Declared skills | 8192 bytes, whole skills | `maxSkillsInjectionBytes` in `kick_priming.go` (fixed) |

**Switches.** `knowledge.enabled` gates the primer only — the bead synthesizer
still writes facts when it is off, they just reach no kick. The dashboard
knowledge toggle registers/unregisters the primer live
(`knowledgePrimerControl`). `knowledge.enabled: false` or an empty keyword set
means the kick simply has no knowledge section.

## 2. Knowledge: pull access

Agents (and external tools) can fetch what the primer did not push:
`GET /api/knowledge/toc` and `/api/knowledge/entry/{id}` (pass `agent=` to
apply that agent's scope), `GET /api/knowledge/effective?agent=` to see what
a given agent is allowed to see and why entries are excluded, the public
`POST /mcp/knowledge` MCP (`knowledge_toc`, `knowledge_get`), `hivectl
knowledge search|list`, and `bd kb search` inside the pod. See
[knowledge-toc.md](knowledge-toc.md) and
[public-knowledge-mcp.md](public-knowledge-mcp.md).

## 3. AGENTS.md / CLAUDE.md: injected by Hive, not read natively

Each agent's CLI starts in `/data/agents/<name>` (`crux.DefaultWorkDir`),
not inside a repository checkout, so the CLIs' native instruction-file
discovery (`AGENTS.md` for Codex/Copilot, `CLAUDE.md` for Claude Code,
`GEMINI.md`) finds **nothing from the monitored repo** at launch. Repo
instructions reach agents only through the scheduler: `primeAgentsMd` parses
`<project.checkouts_dir>/<repo>/AGENTS.md` for the agent's *primary* repo and
prepends the rendered block to the knowledge section (`templates.go`). Without
`project.checkouts_dir` (or a matching `policies.local_dir`) the block is
empty — see [agents-md.md](agents-md.md). Skills declared on the agent are
prepended the same way ([skills.md](skills.md)); the hive-wide house style
precedes both ([house-style.md](house-style.md)).

Two instruction files *are* read natively: a user-level
`~/.claude/CLAUDE.md` / `~/.gemini/GEMINI.md` if one exists — and because
those directories are fleet-shared today (next section), such a file applies
to every Claude or Gemini agent on the hive.

## 4. CLI memory and conversation: what Hive does and does not touch

**Conversation.** `clear_on_kick` defaults to `true` (`ClearOnKick`,
`src/pkg/config/config.go`). `deliverKickLocked` in
`src/crux/agent/manager_kick.go` types `/clear` ⏎ before the kick text for
every TUI backend (agy headless starts a new conversation instead). The one
exception is a PR follow-up delivered with `SendResumeKick`
(`pr_session_resume.go`), which deliberately continues the authoring
conversation. `/clear` empties the chat; it does not touch anything on disk.

**Home layout** (`interactive_home.go`, `manager_env.go`, `manager_homes.go`):

- Per-UID agents get `HOME=/data/home/agents/<name>` (`AgentHome`,
  `interactiveHomePath`) with per-agent `XDG_DATA_HOME`/`XDG_STATE_HOME` under
  `~/.local` (`setupAgentXDGDirs`). `/data/home` is the persistent volume.
- `interactiveHomeBridgeDirs` symlinks `.claude`, `.config`, `.codex`, `.bob`,
  `.gemini` and `.cache` from every per-agent home to the **same** directories
  under `/data/home`, so one login authenticates the fleet. Anything a CLI
  keeps *inside* those directories is therefore fleet-shared and persistent.
- Real per-agent directories with one narrow bridge: `~/.copilot` (only
  `config.json` shared — `setupCopilotHome`, #9444), `~/.local`,
  `.claude.json`, `.npm`, `.gitconfig` (#9478). Codex gets
  `CODEX_HOME=/data/home/.codex-<name>` with only `auth.json` symlinked to the
  shared login (`setupCodexHome`, `codexHomePath`).
- `HIVE_SHARED_AGENT_HOME=1` forces the legacy single shared home for all.
- Inference (API-routed) agents use throwaway homes under
  `/tmp/.claude-inference-home-<name>` that vanish with the pod.
- Contributor containers stage only `.credentials.json` and `settings.json`
  into the container (`CLAUDE_STAGE_ALLOWLIST` in the `justfile`, #7836);
  transcripts, history and memory stay on the host. This is the one place
  memory is isolated by construction.

**No memory handling.** Nothing in Hive sets a CLI's "disable auto-memory"
option (`inferenceSettingsSeed` seeds permissions/onboarding/remote-control
keys only), prunes a `memory/` directory, or clears memory on kick, `/clear`
or restart. What each CLI remembers, it remembers across kicks and pod
restarts, and — for the bridged directories — across agents.

### What persists across what

Columns: a cadence **kick**; an explicit **`/clear`**; a **pod restart**; a
**new agent** added to the same hive. ✓ = survives / is visible, ✗ = gone /
not visible.

| | Kick | `/clear` | Pod restart | New agent in same hive |
|---|---|---|---|---|
| **Knowledge** (`# Relevant Knowledge`, AGENTS.md, skills) | re-selected for *this* kick's issues | n/a — re-injected next kick | ✓ vaults on `/data` | ✓ same stores, filtered by its own `agent_scopes` |
| **Conversation** — all CLIs | ✗ (`/clear` first, unless `clear_on_kick: false` or a resume kick) | ✗ | ✗ (CLI relaunched) | ✗ |
| **Claude Code memory** (`~/.claude/projects/<cwd-slug>/memory/`, `~/.claude/CLAUDE.md`, `history.jsonl`) | ✓ | ✓ | ✓ (`/data/home/.claude` is shared & persistent) | ✓ readable: the tree is fleet-shared; auto-memory is keyed by cwd `/data/agents/<name>` so files are per-agent by path, but `CLAUDE.md`/history are one copy |
| **Codex memory** (`$CODEX_HOME`) | ✓ | ✓ | ✓ (`/data/home/.codex-<name>`) | ✗ per-agent dir; only `auth.json` shared |
| **Copilot CLI memory/sessions** (`~/.copilot`) | ✓ | ✓ | ✓ | ✗ per-agent dir (#9444); only `config.json` shared |
| **Gemini memory** (`~/.gemini/GEMINI.md`, state) | ✓ | ✓ | ✓ | ✓ whole directory shared |
| **Bob memory/settings** (`~/.bob`) | ✓ | ✓ | ✓ | ✓ whole directory shared |

The dashboard's cost accounting reads the shared Claude tree
(`data.claude_sessions_dir`, default `/data/home/.claude/projects`, and
attributes sessions by the `-data-agents-<name>` project slug —
`src/pkg/tokens/claude_scanner.go`), which is why that tree is shared on
purpose today.

## Known gaps / in progress

Do not treat the following as current behaviour; follow the links.

- [#17031](https://github.com/hivecommons/hive/issues/17031) — Claude/Gemini/Bob
  persistent memory is shared across every agent on a hive. Fix in flight:
  [PR #17039](https://github.com/hivecommons/hive/pull/17039) makes `.claude`,
  `.gemini` and `.bob` real per-agent directories with only credential/settings
  entries bridged (the `setupCopilotHome` pattern) and teaches the token
  collector to scan `/data/home/agents/*/.claude/projects`. Until it merges,
  the table above stands.
- [#17032](https://github.com/hivecommons/hive/issues/17032) — disable CLI
  auto-memory by default and add a `clear_memory_on_kick` option alongside
  `clear_on_kick`.
- [#17033](https://github.com/hivecommons/hive/issues/17033) — drain CLI
  auto-memory into beads/knowledge at session end instead of a hidden
  `MEMORY.md` (revives open question 5 of
  [design/knowledge-system.md](design/knowledge-system.md)).
- Repo `AGENTS.md` injection still requires `project.checkouts_dir`; nearest-
  file (`ParseNearest`) targeting is deferred ([agents-md.md](agents-md.md)).

Closed history worth knowing: #7836 (contributor `~/.claude` staging), #9444
(Copilot session-state isolation), #9478 (per-agent `.gitconfig`), #11201
(`knowledge.agent_scopes`), #12124/#12129 (primer and AGENTS.md size caps),
#10524 (knowledge TOC, lifecycle, suggest).

## Related

[design/knowledge-system.md](design/knowledge-system.md) ·
[knowledge-curator.md](knowledge-curator.md) ·
[knowledge-lifecycle.md](knowledge-lifecycle.md) ·
[knowledge-toc.md](knowledge-toc.md) · [agents-md.md](agents-md.md) ·
[skills.md](skills.md) · [agent-configuration.md](agent-configuration.md) ·
[contributor-relay.md](contributor-relay.md)
