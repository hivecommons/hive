# Design documents

Longer-form design records for work that needed a written argument before it was
built: the problem, the constraint that shaped the answer, and the phases the
work was cut into. They are the reference record for a decision, not operator
documentation — the operator-facing page for a shipped feature lives in
[`src/docs/`](../README.md) and is the one to read first.

Unlike an [ADR](../adr/README.md), which is deliberately short and captures one
decision, a design document here carries the full reasoning and is **not
rewritten as later stages ship**. So each entry below states its status, and
that status is the thing to check before treating a page as current behaviour:

- **Shipped** — the described behaviour is live.
- **Partly shipped** — some phases landed; the page still describes work that
  does not exist yet, and says which.
- **Design only** — accepted plan, no implementation. Read it as intent.
- **Historical** — kept as an accurate record of something already decided;
  details reflect the era they were written in, on purpose.

## Records

- [Hub master secret rotation](master-key-rotation.md) — **partly shipped.** Why
  every signing value on the platform (heartbeat bearer, session cookie, session
  and SSO Ed25519 seeds, impersonate, terminal, invite keys) is a pure function
  of one master with no generation marker, which makes rotation a fleet-wide
  flag day, and the generations design that ends it. The foundational
  generations code and the session-cookie domain have landed
  (`src/pkg/hub/hub_generations*.go`, `hub_cookie_generations.go`); the
  remaining per-domain adoptions listed under "Follow-on PRs" are still pending.
- [Wrapped master delivery to pull-only spokes](master-delivery-wrapped.md) —
  **partly shipped**, and page-level **design only**. Read it after
  master-key-rotation.md: rotation is complete on the hub and delivers nothing to
  the two thirds of the fleet on pull-only clusters, which the hub cannot write
  to by design. Records Option D — the spoke generates a keypair, publishes the
  public half over its own outbound heartbeat, and the hub seals each new master
  to it — and why Options A and B were rejected. The sealing primitive and the
  spoke-side wrapping-key lifecycle have since landed (`src/pkg/hub/wrapkey.go`,
  `wrapkey_store.go`); the page is not updated post hoc as later stages ship, so
  its own header still reads DESIGN ONLY.
- [PR reach telemetry](pr-reach-telemetry.md) — **partly shipped.** The design
  behind attributing merged code to what is actually running, and the anchoring
  rule that governs it: **merged ≠ deployed ≠ running**, so reach must attribute
  to the commit executing in the binary, never to the merge or publish event.
  Phase 1 (version-stamped spans, `hive.component` attribute) and phase 2a
  (per-component reach counters) have landed; phases 2b (#3994) and 2c (#3995)
  are pending, and this is the page to read before implementing them. Its
  `v2/pkg/...` path examples are **deliberately historical** — PRs merged before
  the `v2/` → `src/` rename report those paths from the forge API forever, and
  the component mapper still handles both eras (`src/pkg/reach/mapping.go`).
- [Knowledge system and the ACMM developer journey](knowledge-system.md) —
  **partly shipped.** The layered llm-wiki knowledge base — layers,
  subscriptions, the pre-seeded deployment vault, and the extraction/promotion
  APIs — as the mechanism for making Hive useful from ACMM Level 1 upward. The
  wiki layers, the seed vault, and programmatic extraction are live; the
  `scheduler.RunCurator(schedule)` wiring this document plans is **not
  implemented**, so `knowledge.curator.schedule` is accepted and defaulted but
  read by nothing. See [Knowledge curator](../knowledge-curator.md) for the
  programmatic path that does work today.

- [The agent turn model and where in-process state lives](agent-turn-model.md) —
  **spike / investigation, informational.** Step 1 of RFC #4002 and the only
  step taken: a cited map of how hive drives an agent turn today and which
  per-agent state does not survive a process restart. No decision, no
  prototype, no proposal. Its main finding is that there is no "turn" in the
  code at all — a turn begins when hive types a prompt into a tmux pane and
  ends when the pane matches an idle-prompt marker, so every progress signal
  hive has (turn completion, liveness, stalls, auth failure) is screen-scraped
  rendered terminal text. Read it before steps 2-4, particularly for the fork
  it names: hive does not own the conversation format, so
  "conversation-as-state" means either accepting backend-specific resume
  envelopes or moving off opaque interactive CLIs.

- [Copilot per-repo cost capture at the MITM proxy](copilot-cost-capture.md) —
  **investigation, no decision taken.** Phase 4 of epic #4836, which asked
  whether Copilot token usage can be captured per request with repo context and
  timestamps at the proxy. Its answer is a recommendation **against** building
  phase 4 as scoped: per-request usage is available and already captured
  (`src/pkg/proxy/github_proxy.go:2331`), but per-request repo context is not
  available at the proxy in any form, and the nearest substitute — a "last
  `api.github.com` repo seen" from `ExtractRepo` — is an inference from read
  traffic, weaker than the audited `repo=` events phase 3 already joins against.
  Read it for the one separable finding that is worth doing: the proxy has the
  timestamps and `InferenceSink` discards them, so persisting per-request usage
  with its timestamp would move Copilot from "structurally impossible" to
  phase 3's existing join, in `pkg/tokens` only and off the request path.

- [`hive tui` — a terminal dashboard for Hive](tui.md) — **design only.** The
  accepted plan for the k9s-style terminal client tracked in #4907: scope,
  the fixed architecture decisions, the default 2x2 pane layout, the keybinding
  table, and the golden-file testing convention. Read it before picking up any
  #4907 sub-issue, for two corrections it carries that the sub-issue bodies do
  not: their `v2/...` paths are pre-#3996 spellings and map to `src/pkg/tui/...`,
  and `dashboard/openapi.json` — which the epic names as the contract for every
  client task — publishes 32 of the dashboard's 298 routes and **no write
  operations at all**, so the four action tasks have no spec to build against.

## Adding a document here

Add the page, then add a line above saying what a reader gets from it **and its
status**. A design document that outlives its implementation is normal and fine;
a design document a reader mistakes for current behaviour is not, which is what
the status is for.

Pages in this directory are reached through this index rather than through
[`src/docs/README.md`](../README.md) directly, the same delegation
[`adr/`](../adr/README.md) uses. That is also why the docs-index reminder
workflow does not flag them: it is scoped to the top level of `src/docs/` on
purpose, so a directory with its own README never trips it.
