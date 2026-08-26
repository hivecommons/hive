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
