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
  **design only**. Read it after master-key-rotation.md: rotation is complete on
  the hub and delivers nothing to the two thirds of the fleet on pull-only
  clusters, which the hub cannot write to by design. Records Option D — the spoke
  generates a keypair, publishes the public half over its own outbound heartbeat,
  and the hub seals each new master to it — and why Options A and B were
  rejected. Earlier unwired wrapkey primitives were removed as dead code in
  [#5697](https://github.com/hivecommons/hive/pull/5697), so no sealing
  primitive or spoke-side wrapping-key lifecycle is currently implemented.
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
  **spike / investigation, steps 1 and 2 complete.** A cited map of how hive
  drives an agent turn today and which state does not survive restart, plus an
  isolated `pkg/turn` prototype: serialized conversation and operation state,
  a re-entrant structured `Step`, atomic scrubbed persistence, and a replay
  test that kills a contribute-shaped turn at every operation boundary. It is
  not wired to the tmux loop or contributor relay, and takes no production
  decision. Read it before steps 3-4, particularly for the unresolved fork:
  backend-specific resume envelopes versus an API-shaped backend hive owns.

- [Evaluating a handoff path for the re-entrant turn model](agent-turn-handoff.md)
  — **spike / investigation, no decision taken.** Step 3 of the same RFC. Its
  finding is that hive has already built handoff's two hard mechanisms twice and
  wired neither: `pkg/convergence/mutation` (#4255) holds an epoch-fenced claim
  ledger and an idempotent operation journal, `pkg/turn` holds a second journal,
  and nothing imports either. No single store has all three properties handoff
  needs — atomic claim, cross-process serialization, corruption-resistant
  persist — and the three partial implementations each hold a different two.
  Recommends **no queue, and not yet**, with ordered prerequisites, and narrows
  the beads-checkpointing challenge to the one variable still undecided.

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

- [`hive tui` — a terminal dashboard for Hive](tui.md) — **shipped (v1).** The
  design record for the k9s-style terminal client tracked in #4907: scope, the
  fixed architecture decisions, the default 2x2 pane layout, the keybinding
  table, and the golden-file testing convention. Every v1 sub-issue has landed;
  [`src/docs/hivectl.md`](../hivectl.md#tui--live-terminal-dashboard) is the
  operator reference for what actually shipped and the one to read first — this
  page is read afterward, for the reasoning and the two corrections it carries
  against the sub-issue bodies: their `v2/...` paths are pre-#3996 spellings
  that map to `src/pkg/tui/...`, and `dashboard/openapi.json` — which the epic
  names as the contract for every client task — originally published 32 of the
  dashboard's 298 routes and no write operations at all, closed by #5023.
- [Agent host confinement on the default (unconfined) launch path](agent-host-confinement.md)
  — **investigation, no decision taken.** #4918: an agent doing correct,
  benign work on an assigned third-party repo ran that repo's own test suite,
  which reached the operator's real bootloader via `rpm-ostree kargs`, stopped
  only by polkit. Maps the default tmux launch path
  (`src/pkg/agent/manager.go`, `bin/agent-launch.sh`) against the three
  deployment modes (hub pod, containerized `contribute-hive`, `contribute-hive
  ... local`) and establishes precisely what #4938's merged host-state denylist
  covers — the reported command family, on the `Bash` tool surface, by bare
  command word — and what it leaves open: absolute-path/wrapper invocation,
  unlisted polkit-reachable actions, non-`Bash` tool surfaces, and plain
  filesystem writes outside any denied command. Surveys confinement options
  (Podman sandbox, bwrap/systemd-run, seccomp, dedicated low-privilege UID, a
  disposable VM) with real costs, and recommends closing the existing Podman
  sandbox's double opt-in gate (`src/pkg/config/config.go`) for
  `contribute-hive` once its CI coverage gap is closed, keeping the denylist as
  the floor elsewhere.

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
