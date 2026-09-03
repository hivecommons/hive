# Hive Knowledge System + ACMM Developer Journey

## Context

Hive today targets teams at ACMM Level 3-4 — repos with CI, agents running 24/7. The goal is to make Hive useful starting from Level 1 (someone with an idea and no code), guiding them through the full developer journey to full automation. Along the way, a layered knowledge base (powered by llm-wiki) accumulates learnings, test patterns, and project-specific facts that compound over time — making agents smarter and automation safer at every level.

This design is informed by contrasting MetaSwarm (dsifry/metaswarm) with Hive. MetaSwarm's best idea — a self-improving knowledge base with automated extraction and selective priming — is adopted here. Its weakest idea — using more LLMs to verify LLM output — is replaced by Hive's existing deterministic pipeline approach, with **testing** as the primary trust mechanism instead of adversarial LLM review.

---

## Pre-seeded deployment wiki

The container image includes a small Obsidian-compatible seed vault in `src/deploy/data/wiki/`. Current seed files are `agents.md` and `getting-started.md`; they introduce Hive agent roles, governor modes, beads, and the knowledge layer. Operators can extend these Markdown files in a derived image or mount/clone their own vault.

The image copies `src/deploy/data/` to `/opt/hive/seed-data/`. At startup, `deploy/entrypoint.sh` copies missing seed files from there into `/data/`, so the wiki seed lands at `/data/wiki/` without overwriting existing operator content. The entrypoint also prepares `/data/vaults/hive-wiki`; if `HIVE_WIKI_GIT_URL` is set and the vault is not already a git checkout, it clones that URL there, otherwise the local vault directory is created for the knowledge service to use. The seed files are intended as starter knowledge, not immutable product documentation: edit or replace them with project-specific runbooks, gotchas, and policies that should be available to agent priming.

Fixes #2924.

## 1. Layered Knowledge Architecture

### Implementation: geronimo-iia/llm-wiki (Rust)

Evaluated three implementations. `geronimo-iia/llm-wiki` scored 39/40 — the clear winner:
- 23 MCP tools (vs 12 and 4 for alternatives)
- First-class multi-wiki via `wiki_spaces_create` (no wrapper code needed)
- BM25 search via tantivy with confidence-weighted ranking
- Single Rust binary, zero runtime deps (just needs `git`)
- HTTP transport (shared service, not per-client stdio)
- Auto-commits on ingest, redaction support
- MIT/Apache-2.0 licensed — can fork if needed
- Risk: single maintainer, 1 month old. Mitigated by license + pure Rust.

### Four wiki layers, merged at prime time

Knowledge flows DOWN automatically (community → personal). Knowledge flows UP only with explicit opt-in.

```
┌─────────────────────────────────────────────────┐
│  Community Wiki          PUBLIC / READ-ONLY      │
│  Hosted centrally (e.g., wiki.hive.dev)          │
│  CNCF patterns, Go/React best practices          │
│  Anyone can read, curated contributors write      │
│  "Go operators need envtest"                      │
│  "Helm charts need values.schema.json"            │
└──────────────────┬──────────────────────────────┘
                   │ MCP query (HTTP, cross-wiki)
┌──────────────────▼──────────────────────────────┐
│  Org Wiki                SHARED / TEAM-SCOPED    │
│  Hosted per-org (e.g., wiki.kubestellar.io)      │
│  Or git-backed repo (hivecommons/hive-wiki)      │
│  Org members read+write, outsiders read-only     │
│  "ALWAYS use DCO signing"                        │
│  "isDemoData wiring is MANDATORY"                │
└──────────────────┬──────────────────────────────┘
                   │ MCP query (HTTP or local)
┌──────────────────▼──────────────────────────────┐
│  Project Wiki            SHARED / REPO-SCOPED    │
│  Lives in .hive/wiki/ inside the repo            │
│  Same access as the repo itself                  │
│  "vite.config.ts TS errors are IDE noise"        │
│  "createMockCluster() lives in test/factory"     │
└──────────────────┬──────────────────────────────┘
                   │ MCP query (local)
┌──────────────────▼──────────────────────────────┐
│  Personal Wiki           PRIVATE / LOCAL-ONLY    │
│  Lives on YOUR machine (~/.hive/wiki/)           │
│  Your preferences, corrections, style            │
│  NEVER leaves your machine. NEVER shared.        │
│  Highest precedence — overrides all layers above │
│  "I prefer single bundled PRs"                   │
│  "Don't summarize at the end of responses"       │
└──────────────────┬──────────────────────────────┘
                   │ all layers merged + filtered
┌──────────────────▼──────────────────────────────┐
│  knowledge-primer.sh (pipeline stage)            │
│  Deterministic: queries MCP by file paths,       │
│  filters by relevance, injects into work         │
│  order sent to agent                             │
│  Precedence: personal > project > org > community│
└─────────────────────────────────────────────────┘
```

### Hosting model

| Layer | Where it lives | Who can read | Who can write | Hosting |
|-------|---------------|-------------|--------------|---------|
| **Personal** | `~/.hive/wiki/` on user's machine | Only you | Only you | Local llm-wiki instance (never networked) |
| **Project** | `.hive/wiki/` in repo | Anyone with repo access | Agents + Knowledge Curator | Local llm-wiki instance per worktree |
| **Org** | Dedicated git repo (e.g., `hivecommons/hive-wiki`) | Org members | Promoted facts from project wikis | llm-wiki HTTP service (self-hosted or cloud) |
| **Community** | Public git repo or hosted service | Anyone | Curated contributors (PR-based) | llm-wiki HTTP service at wiki.hive.dev |

### Privacy principles

1. **Personal wiki never syncs** — it stays on your machine, period. No telemetry, no upload, no "anonymous aggregation."
2. **Project wiki inherits repo visibility** — private repo = private wiki. Public repo = public wiki.
3. **Promotion is always explicit** — facts flow up (personal → project → org → community) only when a human or agent explicitly promotes them.
4. **Down-flow is automatic** — community/org/project knowledge primes your agents automatically. You don't have to opt in to receive collective knowledge.
5. **Personal always wins** — if your personal wiki says "use tabs" and the org wiki says "use spaces," your agents use tabs.

### Promotion flow

```
Personal fact
  → user runs: hive promote --to project "this is useful for the team"
    → fact appears in .hive/wiki/ with provenance: "promoted from personal by <user>"

Project fact (seen in 3+ repos across org)
  → knowledge curator proposes promotion
    → org maintainer approves (or auto-promote if confidence > 0.9)
      → fact appears in org wiki

Org fact (universal best practice)
  → maintainer opens PR to community wiki repo
    → community reviewers approve
      → fact available to all Hive users
```

### Why llm-wiki over raw JSONL (MetaSwarm's approach)

| Capability | MetaSwarm JSONL | llm-wiki |
|-----------|----------------|----------|
| Cross-referencing | Manual tags | `[[wikilinks]]` with backlinks |
| Querying | `grep -i` on files | MCP semantic query ("gotchas for CardWrapper.tsx?") |
| Staleness detection | Weekly cron by curator agent | Built-in `lint` finds contradictions, orphans, stale claims |
| Multi-format | JSON only | HTML (humans), JSON-LD (structured), .txt (LLM context) |
| Version control | Flat files in repo | Git-backed with merge/diff/blame |
| Dashboard integration | None | HTML export renderable in Hive dashboard |

### Knowledge types stored

| Type | Example | Primed When |
|------|---------|-------------|
| **Pattern** | "Use mock factories from `test/factories.ts`" | Agent touches test files |
| **Gotcha** | "`.join()` on undefined crashes — guard with `(arr \|\| [])`.join()" | Agent edits hook consumers |
| **Decision** | "State management uses Zustand + TanStack Query" | Agent adds new state |
| **Regression** | "PR #1281: `.join()` on undefined — test in `useSearchIndex.test.ts`" | Agent edits `useSearchIndex.ts` |
| **Test scaffold** | "This project uses vitest + msw + testing-library" | Agent creates any test file |
| **Integration** | "envtest needs 30s timeout on CI, 10s locally" | Agent modifies controller tests |
| **Coverage rule** | "Cards using `useCached*` MUST have isDemoData test cases" | Agent creates a new card |

---

## 2. ACMM Developer Journey

Hive guides (not does) the work at lower levels, progressively increasing autonomy as the project matures.

### Level 1 — Inception (No repo, no code)

**What Hive does**: Guides the user through project scaffolding.

- User describes what they want to build
- Hive queries **community wiki** for scaffolding patterns, CI templates, project structure
- Guides user through: repo creation → initial structure → first commit → basic CI
- **Testing starts here**: Hive guides user to write acceptance criteria as test stubs
  - `test("should reconcile CRD when namespace is created")` with `TODO` body
  - These define the contract — what "done" looks like — before any production code
  - 0% coverage, 100% intent
- **Knowledge**: Community patterns only (no project-specific facts yet)
- **Agents**: None running. Hive is interactive/advisory.

### Level 2 — Advisory (Code exists, PRs are manual)

**What Hive does**: Acts as reviewer/advisor, not autonomous.

- Hive reviews PRs on request (not auto-scanning)
- Flags PRs that add code without tests: "this handler has no test — want me to suggest one?"
- **Testing**: Tests alongside features, no hard gate
  - Coverage trending up (tracked, not enforced)
  - Test stubs from Level 1 get filled in as features land
  - Community wiki primes test patterns: "Go operators need envtest", "React: use msw for API mocking"
- **Knowledge accumulation begins**:
  - Knowledge Curator watches merged PRs, extracts gotchas and patterns
  - **Project wiki** starts: "this repo uses controller-runtime v2", "webhook tests need envtest"
  - User corrections ("no, we use X not Y") become high-confidence facts
- **Agents**: Scanner in advisory mode (suggests, doesn't act). No governor.

### Level 3 — Quality-Gated (Automated builds, tests running)

**What Hive does**: Scanner and reviewer active, governor in QUIET/IDLE mode.

- Scanner triages issues, creates fix PRs for simple bugs
- Reviewer monitors CI health, nightly tests, coverage trends
- **Testing becomes a gate**:
  - Coverage thresholds enforced in CI (configurable, e.g., 70% lines)
  - Thresholds ratchet up as project matures (never down)
  - Scanner auto-creates issues when coverage drops below threshold
  - Nightly integration tests catch service drift
- **Knowledge is now rich**:
  - Org wiki contributes cross-repo patterns: "all kubestellar repos need Netlify preview deploys"
  - Project wiki has dozens of facts from merged PRs
  - Regression patterns accumulate: "changing webhook port breaks envtest — update `testenv.Environment.WebhookInstallOptions`"
- **Agents**: Scanner + reviewer. Governor in QUIET/IDLE. Architect opportunistic.

### Level 4 — Security-Aware (5 agents, 24/7, self-healing)

**What Hive does**: Governor scales dynamically, all agents active.

- Full Hive deployment: scanner, reviewer, architect, outreach, supervisor
- Governor adjusts cadences and models by queue depth (SURGE/BUSY/QUIET/IDLE)
- **Testing is what makes autonomy safe**:
  - Full TDD: every bug fix reproduces the bug in a test first (red), then fixes (green)
  - `merge-gate.sh` requires green CI — deterministic, not LLM-verified
  - Regression tests accumulate automatically from every bug fix
  - Coverage thresholds are high and enforced (e.g., 90%+ lines)
  - **The test suite is the trust mechanism** — not "an LLM reviewed another LLM's code"
- **Knowledge compounds**:
  - All three wiki layers active, merged at prime time
  - Knowledge Curator extracts from every merged PR across all repos
  - Lessons from this project feed back into org layer (and optionally community)
  - Agents get smarter without any human intervention
- **Agents**: All 5. Governor active. Auto-deploy. Dashboard monitoring.

---

## 3. Testing Progression Detail

| Level | Test Approach | Coverage Gate | Who Writes Tests | Trust Model |
|-------|--------------|---------------|-----------------|-------------|
| **1 — Inception** | Acceptance criteria as test stubs (`TODO` bodies) | None | User (guided by Hive) | Human judgment |
| **2 — Advisory** | Tests alongside features, no hard gate | Tracked, not enforced | User + Hive suggests | Human review + test results |
| **3 — Quality-Gated** | Coverage threshold in CI, nightly integration | 70%+ (ratcheting) | User + scanner fixes include tests | CI pass = trust |
| **4 — Security-Aware** | Full TDD, regression on every bug fix | 90%+ enforced | Agents (scanner, architect) | Green test suite = trust |

### The feedback loop

```
Bug found
  → Test written to reproduce (red)
    → Fix applied (green)
      → Knowledge Curator extracts pattern
        → Wiki updated: "guard .join() against undefined in hook consumers"
          → Next agent touching that area gets primed with the gotcha
            → Writes the guard AND the test proactively
```

### Test knowledge in the wiki

The wiki stores test patterns alongside code patterns:

- **Test scaffolding**: "This project uses vitest + msw + testing-library" → primed when agent creates any test file
- **Mock patterns**: "Use `createMockCluster()` from `test/factories.ts`, not inline mocks" → primed when agent touches test files
- **Regression markers**: "PR #1281 crashed because `.join()` on undefined — regression test in `useSearchIndex.test.ts`" → primed when agent edits `useSearchIndex.ts`
- **Integration gotchas**: "envtest needs 30s timeout on CI, 10s locally" → primed when agent modifies controller tests
- **Coverage rules**: "Cards using `useCached*` hooks MUST have isDemoData test cases" → primed when agent creates a new card component

### Adaptive test strictness

Just as the governor adapts model selection by queue depth, it adapts test requirements by project maturity:

```
ACMM Level 1-2: testMode = "suggest"
  → Hive suggests tests, doesn't block without them

ACMM Level 3:   testMode = "gate"
  → CI blocks merges below coverage threshold
  → Scanner creates issues for coverage drops

ACMM Level 4:   testMode = "tdd"
  → Agents must write failing test before fix (red-green)
  → Merge gate requires green suite + coverage threshold
  → Regression test required for every bug fix
```

---

## 4. Architecture Integration

Hive is a Go binary with structured packages under `src/pkg/`. The knowledge system adds a new `pkg/knowledge/` package and extends existing packages.

### New package: `src/pkg/knowledge/`

```
src/pkg/knowledge/
  client.go          — llm-wiki MCP HTTP client (query, search, ingest, lint, spaces)
  primer.go          — Selective fact retrieval by file paths, keywords, work type
  curator.go         — Extract facts from merged PRs, ingest into wiki
  promote.go         — Promote facts between layers (project→org, etc.)
  types.go           — Fact, Layer, WikiConfig, ConfidenceScore, Precedence types
  client_test.go
  primer_test.go
  curator_test.go
  promote_test.go
```

### How it integrates with existing packages

| Package | Integration |
|------------|-------------|
| `pkg/config/` | Add `Knowledge` section to `hive.yaml` (layers, wiki paths, curator schedule, primer config) |
| `pkg/snapshot/` | Add `Knowledge []Fact` field to snapshot state — primed facts per work item |
| `pkg/classify/` | After classification, call `knowledge.Primer.Prime(item)` to enrich with relevant facts |
| `pkg/governor/` | Add test maturity level to governor decisions; adapt agent directives by ACMM level |
| `pkg/scheduler/` | Add curator schedule (daily or on-merge trigger) |
| `pkg/dashboard/` | Add `/api/knowledge/*` endpoints proxying to llm-wiki MCP; Knowledge tab in frontend |
| `pkg/agent/` | Include primed facts in agent work orders |

### Pipeline flow

The pipeline is Go code, not shell scripts:

```
snapshot.Build()
  → classify.Classify(items)          // existing: complexity tier, lane, model rec
    → knowledge.Prime(items, layers)  // NEW: query wiki by file paths, merge layers
      → scheduler.Dispatch(items)     // existing: send to agents with enriched snapshot
```

The primer runs deterministically — Go HTTP client queries llm-wiki, filters by file paths, no LLM in the loop.

### Curator flow (separate goroutine)

```
scheduler.RunCurator(schedule)
  → github.FetchMergedPRs(since)     // existing GitHub client
    → knowledge.Extract(prs)          // NEW: parse diffs, review comments, CI results
      → knowledge.Ingest(facts, wiki) // NEW: call llm-wiki MCP ingest endpoint
```

The curator uses an LLM (via llm-wiki's synthesis layer) for knowledge extraction only — not for enforcement or gating.

### Config (hive.yaml)

```yaml
knowledge:
  enabled: true
  engine: "llm-wiki"  # geronimo-iia/llm-wiki (Rust)
  layers:
    - type: personal
      path: "~/.hive/wiki"
      shared: false          # NEVER syncs, NEVER leaves machine
      precedence: 1          # highest — overrides all others
    - type: project
      path: ".hive/wiki"
      shared: true           # inherits repo visibility
      precedence: 2
    - type: org
      url: "https://wiki.kubestellar.io/mcp"  # or local git repo
      shared: true
      precedence: 3
    - type: community
      url: "https://wiki.hive.dev/mcp"         # public, read-only
      shared: true
      precedence: 4          # lowest — overridden by all others
  curator:
    schedule: "daily"
    extract_from: ["pr_comments", "ci_failures", "review_comments"]
    auto_promote_threshold: 0.9  # confidence to auto-promote project→org
  primer:
    max_facts: 25
    priority: ["regression", "gotcha", "test_scaffold", "pattern", "decision"]
    merge_strategy: "precedence"  # personal > project > org > community
```

---

## 5. Implementation Plan

### Phase 1: llm-wiki integration (project-level only)

**Files to create/modify in `src/`**:

1. **`src/pkg/knowledge/types.go`** — Core types
   - `Fact`, `Layer`, `LayerType`, `WikiConfig`, `ConfidenceScore`
   - `PrimerConfig`, `CuratorConfig`, `PromoteRequest`

2. **`src/pkg/knowledge/client.go`** — llm-wiki MCP HTTP client
   - `Search(query, filters)`, `Read(slug)`, `Ingest(facts)`, `Lint()`, `Stats()`
   - `SpacesList()`, `SpacesCreate(name, path)`
   - Connects to llm-wiki HTTP endpoint (configurable per layer)
   - Falls back gracefully if wiki unavailable

3. **`src/pkg/knowledge/primer.go`** — Selective fact priming
   - `Prime(items []WorkItem, layers []Layer) []EnrichedItem`
   - Queries each layer by file paths, keywords, work type
   - Merges results with precedence (personal > project > org > community)
   - Filters to top N facts by priority order

4. **`src/pkg/knowledge/curator.go`** — Knowledge extraction
   - `Extract(prs []PullRequest) []Fact`
   - `Ingest(facts []Fact, targetLayer Layer)`
   - Parses PR diffs, review comments, CI failures
   - Calls llm-wiki `wiki_ingest` to add facts

5. **`src/pkg/config/`** — Add `Knowledge` field to config struct, parse from `hive.yaml`

6. **`src/pkg/snapshot/state.go`** — Add `Knowledge []Fact` to snapshot state

7. **`src/pkg/dashboard/server.go`** — Add knowledge API endpoints (proxying to llm-wiki MCP)

8. **Tests** — `client_test.go`, `primer_test.go`, `curator_test.go` with mock llm-wiki server

### Phase 2: Testing progression engine

9. **`src/pkg/knowledge/maturity.go`** — ACMM test maturity detector
   - `DetectMaturity(repo) Level` — checks: has tests? has CI? coverage config? TDD markers?
   - Returns Level 1-4

10. **`src/pkg/governor/governor.go`** — Add test maturity to governor logic
    - Governor reads maturity level from snapshot
    - Adjusts agent directives: suggest (L1-2) → gate (L3) → tdd (L4)

11. **`src/policies/`** — Template variables for test mode per maturity level

### Phase 3: Org and community layers

12. **`src/pkg/knowledge/promote.go`** — Fact promotion between layers
    - `Promote(fact, fromLayer, toLayer)` — copies with provenance
    - Auto-promote when confidence > threshold AND seen in 3+ repos

13. **Org wiki deployment** — Docker Compose service or standalone llm-wiki HTTP instance

14. **Community wiki** — Hosted service (future, separate infrastructure)

### Phase 4: Dashboard Knowledge UI

The Hive dashboard gets a new **Knowledge tab** — the primary UI for users to browse, search, and manage their wikis.

#### Layout

```
┌──────────────────────────────────────────────────────────────────┐
│  Hive Dashboard                                                   │
│  [Agents] [Governor] [Health] [Knowledge] [Settings]              │
├──────────────────────────────────────────────────────────────────┤
│                                                                   │
│  Layer Switcher              │  Page View                         │
│  ┌─────────────────────┐     │  ┌──────────────────────────────┐  │
│  │ 🔒 Personal    (12) │     │  │ guard-join-undefined         │  │
│  │ 📁 Project     (47) │     │  │                              │  │
│  │ 🏢 Org         (89) │     │  │ Type: gotcha                 │  │
│  │ 🌍 Community  (340) │     │  │ Confidence: ████████░░ 0.95  │  │
│  └─────────────────────┘     │  │ Status: verified             │  │
│                              │  │ Layer: project               │  │
│  Search: [________________]  │  │                              │  │
│                              │  │ `.join()` on undefined       │  │
│  Filter: [All types    ▼]    │  │ crashes. Always use          │  │
│                              │  │ `(arr || []).join(',')`.     │  │
│  Facts (sorted by relevance) │  │                              │  │
│  ┌─────────────────────┐     │  │ ── Sources ──────────────    │  │
│  │ ● guard-join-undef… │     │  │ PR #1281 (2026-03-15)       │  │
│  │   gotcha · 0.95     │     │  │ PR #1455 (2026-04-02)       │  │
│  │   project · 14 uses │     │  │                              │  │
│  ├─────────────────────┤     │  │ ── Related ──────────────    │  │
│  │ ○ isDemoData-wiring │     │  │ [[isDemoData-wiring]]        │  │
│  │   pattern · 0.92    │     │  │ [[hook-undefined-guard]]     │  │
│  │   project · 9 uses  │     │  │                              │  │
│  ├─────────────────────┤     │  │ ── Override Status ────────  │  │
│  │ ○ dco-signing       │     │  │ No personal override         │  │
│  │   decision · 1.00   │     │  │                              │  │
│  │   org · 31 uses     │     │  │ Last primed: 2 days ago      │  │
│  └─────────────────────┘     │  │ Usage count: 14              │  │
│                              │  │                              │  │
│  Health                      │  │ [Promote to Org] [Archive]   │  │
│  ⚠️ 3 stale (>90 days)      │  │ [Override (Personal)]        │  │
│  ⚠️ 1 orphaned              │  │ [View in Graph]              │  │
│  ✅ 0 contradictions         │  │                              │  │
└──────────────────────────────┴──────────────────────────────────┘
```

#### Features

**11a. Layer switcher**
- Toggle between Personal / Project / Org / Community
- Lock icon on Personal (visual reminder: private)
- Fact counts per layer
- "All layers" view merges with precedence indicators

**11b. Unified search**
- BM25 search across all subscribed layers via llm-wiki MCP `wiki_search`
- Results tagged with source layer and confidence
- Filter by type (pattern/gotcha/regression/test_scaffold/decision/integration/coverage_rule)
- Filter by status (draft/verified/stale/archived)

**11c. Fact detail view**
- Full markdown content rendered
- Confidence bar (visual 0.0-1.0)
- Source provenance: which PRs, which review comments, when extracted
- Wikilinks to related facts (clickable, navigate within dashboard)
- Override indicator: "Your personal wiki overrides this org fact" with diff view
- Usage stats: how many times agents were primed with this fact, when last used
- Actions: Promote (to higher layer), Archive, Override (create personal copy), View in Graph

**11d. Graph view**
- Toggle from list view to wikilink graph (Mermaid rendered via llm-wiki `wiki_graph`)
- Nodes colored by layer (blue=personal, green=project, orange=org, purple=community)
- Node size by usage count
- Click node to navigate to fact detail
- Filter graph by type, layer, or search term

**11e. Health panel (sidebar)**
- Stale facts: unreferenced >90 days (click to review batch)
- Orphaned pages: no inbound wikilinks (suggest linking or archiving)
- Contradictions: conflicting facts detected by `wiki_lint` (click to resolve)
- Coverage trend: graph of project test coverage over time
- Test maturity: current ACMM level indicator (1-4) with criteria checklist

**11f. Subscription management** (under Settings tab)
- List of subscribed org/community wiki URLs
- Add/remove wiki endpoints
- Per-project layer toggle: enable/disable specific layers
- Personal wiki path (defaults to `~/.hive/wiki/`)

#### API endpoints (src/pkg/dashboard/server.go)

```
GET  /api/knowledge                    # List facts across layers (paginated, filterable)
GET  /api/knowledge/:layer             # List facts in specific layer
GET  /api/knowledge/:layer/:slug       # Read single fact with metadata
GET  /api/knowledge/search?q=&type=    # BM25 search across subscribed layers
GET  /api/knowledge/graph?root=&depth= # Wikilink graph (Mermaid/JSON)
GET  /api/knowledge/health             # Stale/orphan/contradiction counts
GET  /api/knowledge/stats              # Aggregate stats (counts by type, layer, confidence)
POST /api/knowledge/promote            # Promote fact to higher layer
POST /api/knowledge/archive            # Archive a fact
POST /api/knowledge/override           # Create personal override of a fact
GET  /api/knowledge/subscriptions      # List subscribed wiki endpoints
POST /api/knowledge/subscriptions      # Add/remove subscription
```

All endpoints proxy to llm-wiki MCP tools. Dashboard auth (bearer token) applies to mutations.

---

## 6. Verification

### Phase 1 verification
- Set up llm-wiki MCP server locally against a test project
- Run `knowledge-primer.sh` and confirm it produces enriched `actionable.json` with relevant facts
- Run `knowledge-curator.sh` against recent merged PRs and confirm facts are extracted and ingested
- Verify pipeline runs end-to-end: enumerate → classify → prime → kick
- Verify agents receive knowledge in their work orders

### Phase 2 verification
- Create test projects at each maturity level (1-4)
- Run `test-maturity.sh` and confirm correct level detection
- Verify governor adjusts test directives by level
- Verify scanner behavior changes: suggests (L2) vs gates (L3) vs TDD (L4)

### Phase 3 verification
- Stand up org wiki, push facts from project curator
- Verify `knowledge-primer.sh` merges layers correctly (personal > project > org > community)
- Verify org-level facts appear in agent work orders
- Verify personal overrides take precedence in merged results

### Phase 4 verification
- Dashboard Knowledge tab renders with all four layers
- Search returns results across layers with correct precedence
- Fact detail view shows provenance, wikilinks, usage stats
- Promote/Archive/Override actions work end-to-end
- Graph view renders wikilink connections across layers
- Health panel shows stale/orphan/contradiction counts from `wiki_lint`
- Subscription management adds/removes wiki endpoints

---

## 7. Resolved Decisions

1. **llm-wiki implementation** — `geronimo-iia/llm-wiki` (Rust). 39/40 score. First-class multi-wiki, BM25 search, git-backed, HTTP transport. Single binary, zero deps.
2. **Knowledge layers** — Four layers (personal/project/org/community), not three. Personal wiki is private, local-only, highest precedence.
3. **Privacy model** — Down-flow automatic, up-flow explicit. Personal never shared. Project inherits repo visibility.

## 8. Open Questions

1. **Rust 1.95 requirement** — Our machine has 1.87. Need to install via rustup or use Homebrew nightly. Or use the prebuilt binary / Homebrew tap (`brew install geronimo-iia/tap/llm-wiki`).
2. **Community wiki seeding** — What initial content? CNCF TAG guidelines? Extracted from top 100 CNCF project READMEs? Start empty and let it grow?
3. **Knowledge curation approval** — Auto-extracted facts go to project wiki at confidence < 0.9. Auto-promote to org wiki at confidence >= 0.9. Human approval for community wiki (PR-based).
4. **Cross-project fact promotion** — Automatic when seen in 3+ repos AND confidence > 0.9. Otherwise manual.
5. **Personal wiki bootstrapping** — Should `hive init` create a personal wiki from Claude Code's existing memory files? (e.g., migrate `~/.claude/projects/*/memory/` → `~/.hive/wiki/`)
