# Customers: a profile per person and a customer graph

**Status: design only / proposed; the build is in progress as a PR series.**
RFC [#18742](https://github.com/hivecommons/hive/issues/18742). This page is
the design record for the Customers section, the people store behind it, the
read-only graph endpoint, and the goals and satisfaction a hive infers for each
person. Section 13 lists the PR series and what each PR ships; until a PR in
that table has merged, read its row as intent.

The owner's framing: "each customer is the reason we build software." Hive
should be able to say who a piece of work is for, what that person is trying to
get done, and whether they are getting it.

## 1. Goals and non-goals

Goals:

- One profile per person, built from what the hive already holds. No new data
  is collected and no new network calls are made on a normal sweep.
- A clear line between **customers** (dashboard users and people added by hand)
  and **suggested** people (everyone else the hive has seen), so a hive does not
  build a profile of a third party without a human decision.
- Every profile carries **goals** (what the person is trying to get done) and a
  **satisfaction** signal. Hive infers candidates with evidence; a person
  confirms, edits or rejects them.
- A graph of people and artefacts that the dashboard reads through a read-only
  GraphQL endpoint.
- Agents get a bounded customer-context block in the kick prompt, so the work
  is done with the person in view.
- On by default behind one switch, `customers.enabled`.

Non-goals (this series):

- Any outbound path. Nothing is published, written to GitHub, sent to a hub or
  added to a heartbeat.
- An agent-callable query tool. Agents cannot query Hive's stores
  ([security-model.md](../security-model.md) section 4,
  [ADR-0011](../adr/0011-knowledge-system.md)); a query tool is a later RFC.
- A CRM. There are no deals, accounts, pipelines or outbound messaging.
- A numeric satisfaction score. See section 8.
- GraphQL mutations. Writes go through the REST routes, which carry the role
  checks and the audit lines.

## 2. What exists today

Hive has no model of a person. Identity appears as plain strings in separate
places, and nothing joins them:

| Where | Identity |
|---|---|
| Dashboard access list and sessions | dashboard login, role |
| GitHub poll, claims and merged ledgers, review evidence | GitHub login |
| DCO and commit metadata | git author email, noreply email |
| Calendar and scheduled meetings | attendee email, proven calendar email |
| Meetings and recordings | display name, platform participant id |
| Inbox mail | sender address |
| Chat connectors | Discord, Slack and Teams ids |
| Contributor relay | contributor id, GitHub login |
| Audit log | acting dashboard user |

The Contributors section is about trust tiers for people who run work for the
hive. It is not a general person model and this design does not change it; a
contributor is one of several sources of identity for a person.

## 3. Package and wiring

- New package `src/pkg/customers` holds the model, the store, identity
  resolution, tags, goals, satisfaction and tombstones. It imports no dashboard
  code.
- `src/pkg/dashboard` does not import the new package. The dashboard declares a
  service interface, holds it in an `atomic.Pointer`, and `src/cmd/hive`
  installs the implementation. This is the pattern the meeting scheduler uses
  (`src/pkg/dashboard/api_meetings_schedule.go`), and it keeps the dashboard
  import ratchet untouched.
- The GraphQL library is imported only by `src/pkg/customers/gql`.
- One config key, `customers.enabled` (default `true`). With it off, no store
  is opened, the sweep does not run, the routes answer 503 and the section is
  hidden.

## 4. Data model

**Person**

| Field | Notes |
|---|---|
| `id` | Opaque, `p_` plus 12 hex characters from a random source. Never derived from personal data. |
| `name` | Display name. |
| `status` | `customer` or `suggested`. A dismissed suggestion is not kept as a status: its key becomes a tombstone (section 6). |
| `origin` | `hive_user`, `manual` or `accepted`. |
| `identities` | See below. |
| `tags` | See section 7. |
| `goals` | See section 8. |
| `satisfaction` | See section 8. |
| `notes` | Free text, written by people only. |
| `merged_from` | Merge history, so a merge can be undone. |

**Identity** `{kind, value, source, confidence, manual, at, by}`. Kinds:
`hive_user`, `github`, `email`, `git_email`, `discord`, `slack`, `teams`,
`meet`, `zoom`, `name`. Values are normalised per kind (lower-cased logins and
emails; a `name` is kept as written and never used as a key).

**Edge** `{person, predicate, artefact, via, source, at, confidence}`. `via`
is the identity that produced the edge, which makes a split exact: removing an
identity from a person moves exactly the edges that identity produced.
Predicates: authored, assigned, reviewed, merged, claimed, reported,
commented_on, mentioned_in, sent, added, recorded, shared, organised, attended,
spoke_in, chatted_in, invited, decided, cited_in, acted, logged_in. The last
two are stored as one edge per person per day.

**Artefact reference**: an id, never content. Forms: `gh:issue:org/repo#N`,
`gh:pr:org/repo#N`, `inbox:<source>:<id>`, `meeting:<id>`, `event:<id>`,
`fact:<layer>/<slug>`, `bead:<store>:<id>`, `audit:<date>`.

**"Me"** is the person whose `hive_user` identity equals the signed-in
dashboard user. It is created on first read.

## 5. Identity resolution

Automatic links happen only on exact keys and a short list of proven links:

- a proven calendar email (the `mailallow` proof),
- a GitHub sign-in through the device flow, or the hive's configured owner
  identity,
- DCO and GitHub noreply email matching a login,
- a contributor id,
- a companion attestation.

Everything else is a **"Same person?" hint** that a person confirms: a display
name from a meeting or a connector, an email local part that matches a login.
A display name never creates an edge and never causes a merge.

Merge and split are reversible. Every manual decision (link, unlink, merge,
split, "not the same person") is pinned, and automatic resolution reads pins
first, so a sweep cannot undo what a person decided. A `hive_user` identity is
what "me" resolves on, so it is never added, removed or split off by hand, and
two dashboard users are never merged.

## 6. Storage and retention

- **Truth**: `/data/customers/people.json`, written atomically with mode 0600
  in a 0700 directory (the model is `src/pkg/meetings/schedule/store.go`).
- **Index**: `/data/graph/customers.db`, bbolt, holding edges. It is
  rebuildable from the sources and is not the record of any human decision.
  It borrows key idioms from `src/pkg/knowledge/graphstore.go` and does not
  extend that store.
- **Edges are references.** A resolver per artefact kind answers `Exists` and
  `Visible(viewer)`. Edges whose artefact is gone are pruned on each sweep, so
  a profile cannot keep what retention deletes: meeting transcripts, unapproved
  mail and audit entries each keep their own expiry and their edges follow.
- **Visibility follows the source.** A personal-layer recording is visible on a
  profile only to a viewer who can open it in the Inbox.
- **Suggested people** are an identity key, counters and up to five evidence
  references. A suggestion expires with its evidence. Mail awaiting approval
  creates nothing. Dismissing a suggestion deletes it and tombstones its key,
  so the hive keeps no readable trace of a person someone turned down.
- **Tombstones.** Forgetting a person deletes the person, their identities and
  their edges, and stores one salted hash per identity. A sweep checks the
  tombstones before creating a suggestion or a link, so a forgotten person is
  not rebuilt. The salt is per hive and stays in the store; a tombstone holds
  no readable identity. A person adding the same identity back by hand is a
  new human decision and clears the tombstone. A forgotten dashboard user who
  signs in again gets a fresh, empty profile.

## 7. Tags

Defaults: `maintainer`, `contributor`, `end-user`, `stakeholder`. Free-form
tags are allowed.

Each tag keeps `{state, source, rule, at, by}` and a short history; `source`
is `auto` or `manual`. The sweep assigns tags from role, association and
history. A manual value always wins: a tag a person set or removed is not
changed by the sweep until someone chooses "Reset to automatic".

## 8. Goals and satisfaction

Both are first-class fields on a person and both follow the provenance shape
tags use.

**Goal** `{id, text, state, source, evidence, at, by, history}`

- `state`: `suggested`, `confirmed` or `rejected`.
- `source`: `auto` (inferred) or `manual` (written or edited by a person).
- `evidence`: artefact references that led to the suggestion. References only,
  subject to the same pruning and visibility rules as edges.
- A person can have several goals. The profile shows confirmed goals first and
  suggestions beneath them with confirm, edit and reject.

**Satisfaction** `{level, state, source, evidence, at, by, history}`

- `level`: a small ordinal, `unknown`, `at_risk`, `neutral` or `satisfied`.
- There is **no numeric score**. A number would claim a precision the evidence
  does not have. The level is always shown with the evidence that led to it,
  and `unknown` is the honest default.
- `state` and `source` as for goals.

Rules that hold for both:

1. **Inference only writes suggestions.** It never sets a confirmed value.
2. **A confirmed or manual value is never overwritten by inference.** A new
   inference that disagrees is stored as a suggestion beside it.
3. **A rejected suggestion is remembered** (by a hash of its normalised text
   for goals, by level for satisfaction), so the same suggestion is not
   offered again from the same evidence.
4. **History** records each change with who and when, as tag history does.
5. **Nothing inferred is ever published outside the hive.** Inferred goals and
   satisfaction do not appear in GitHub comments, PR bodies, hub payloads,
   exports to third parties or the agent prompt block unless a person has
   confirmed them.
6. **A read-only user can edit their own goals** and confirm or reject
   suggestions about themselves. They cannot see anyone else's.

### Inference

Candidates come from what the person has already put in front of the hive:

| Signal | Goal evidence | Satisfaction evidence |
|---|---|---|
| Issues they opened | What they asked for | Age of open asks, reopen count |
| PR outcomes | What they were trying to land | Merged, closed unmerged, time to merge, review round trips |
| Meeting asks | Action items and asks attributed to them | Follow-through on those items |
| Approved mail | The request in the message | Reply latency, repeat requests |

The inference runs in the hive process through the model route the hive
already uses for in-process inference (the meeting persona's answer path). It
does not start an agent and it gives the model no tools.

- **Inputs are fenced as untrusted.** Evidence text is scanned and wrapped in a
  nonce-tagged untrusted envelope, as Inbox text is.
- **Output is bounded and typed.** The model returns a small fixed structure
  (at most three goals of bounded length, one satisfaction level from the
  ordinal, the evidence ids it used). Anything that does not parse, names
  evidence it was not given or exceeds the bounds is dropped.
- **Inference is budgeted.** A few people per tick, customers only (never
  suggested people), a minimum interval per person, and shed when the hive's
  budget is low. It can be turned off without turning off the section.
- Each suggestion is audited with the person id and the evidence count, not
  the text.

## 9. Ingestion

One optional sweep, `customers`. Steps:

- **A, hive users and proven links (no network):** the access list, proven
  emails, contributor profiles, the owner identity.
- **B, local stores (no network):** uploads, recordings, approved mail, the
  calendar, meetings, connector facts, the claims ledger, the merged ledger,
  review evidence.
- **C, GitHub from the existing poll (no extra calls):** authors, assignees,
  requested reviewers and logins mentioned in bodies, read from the last
  enumeration the hive already made.
- **D, activity:** the audit log, operator actions, share history, meeting
  drafts, sessions, fact authors, and beads through their issue or PR.
- **E, budgeted GitHub search on accept:** mentions and comments for a newly
  accepted person, about two calls per person, one person per tick, shed when
  the API budget is low.

A normal sweep (A to D) makes zero GitHub calls, and a test holds that.

What the first sweep (steps A to C, `customers` in
`src/cmd/hive/sweep_workers.go`, every ten minutes) reads:

| Source | Identity | Edge |
|---|---|---|
| Access list (`dashboard.authorized_users`) | dashboard user; a `github:<login>` entry also proves the GitHub login | none; owner and merger earn `maintainer` |
| Owner's connected GitHub account | GitHub login, when there is exactly one owner | none |
| Proven calendar emails | email of the dashboard user who proved it | none |
| Last GitHub enumeration: issues | author, assignees, logins mentioned in the body | reported, assigned, mentioned_in |
| Last GitHub enumeration: pull requests | author, requested reviewers | authored, review_requested |
| Claims ledger | human, contributor and external holders | claimed |
| Inbox uploads | the dashboard user who added it | added |
| Approved mail | the sender address | sent |
| Calendar events | organiser and attendee addresses, who added it | organised, invited, added |
| Meetings | who sent hive in (dashboard user, address or platform id) | added |

Bots, the hive's own author account, agent claims, mail awaiting approval,
purged meetings and display names produce nothing. Not read yet, and left to
the next sweep PR: recordings, the merged ledger, review evidence, contributor
profiles and connector fact authors, none of which the sweep can reach without
a new accessor.

The sweep is an optional sweep, so it is shed with the other optional sweeps
when the GitHub budget is low, although it spends none of that budget itself.

Step D, in the same sweep: the dashboard audit log gives one `acted` edge per
person per day, and one `logged_in` edge on a day they signed in. Only the user,
the day and the kind of action are read; the detail text of an audit line is
never copied. An audit edge exists while its day is inside the audit log's
retention (90 days) and is shown to the full view only.

"Same person?" hints are raised between a customer and a suggested person when
a suggested address starts like one of the customer's logins (a GitHub login or
the dashboard user name), or a suggested login starts like one of the customer's
addresses. The comparison is on exact, lower-cased keys with any `+tag` removed.
Locals shorter than four characters and role addresses such as `info` or
`noreply` are skipped. A display name is never compared. A hint links nothing:
answering yes makes the suggested identity the customer's own (recorded as
manual, by whom), answering no pins the pair so it is not asked again. A hint
between two customers is answered by merging them, which is the merge and split
UI of a later PR.

Step E, the budgeted GitHub search for mentions and comments on accept, is not
built. The GitHub client has no comment or mention search, so it needs a new
client method and a place in the API budget. It is left for its own PR.

Automatic tags come from role, association and history: `maintainer` from an
owner or merger role or a GitHub author association of owner, member or
collaborator; `contributor` from an authored pull request; `end-user` from a
reported issue with no pull request; `stakeholder` from meeting, calendar or
mail activity with no GitHub activity.

Existence is answered per artefact kind. A meeting exists until it is deleted
or its transcript is purged; a Calendar event while the schedule holds it; an
Inbox upload while its store holds it; a mail item while it exists and is
approved. When a store is turned off nothing can say an artefact is gone, so
its edges are kept. GitHub references have no resolver and are kept.

Because the feature is on by default, the discovery sweep does not merge before
forget, the role checks and retention pruning are merged and tested.

## 10. Access

| Viewer | Can see | Can edit |
|---|---|---|
| Owner | Everyone; suggested people; export; forget | Everyone |
| Read-write | Everyone; suggested people | Everyone, except forget and export |
| Read-only | Only their own "me" | Only their own name, identities (as unconfirmed claims), tags and goals |

- The owner and read-write admission is the one used for playing an Inbox
  recording.
- The read-only rule holds on every path: the list route returns only "me",
  a person route for any other id answers as if the id did not exist, and
  every GraphQL resolver that returns a person checks the viewer. A test walks
  the role matrix over every route and over each GraphQL path.
- An identity a read-only user adds to their own profile is an **unconfirmed
  claim**. It is shown on their profile and does nothing else: it is not a
  key, resolves nobody and attracts no edges, whether or not the identity
  belongs to someone else, so the answer tells the claimant nothing about
  other profiles. An owner or read-write user confirms it; if it belongs to
  another person, confirming raises a "Same person?" hint instead of linking.
  A trusted source (section 5) proving the same identity also upgrades it.
- A read-only user's view of their own profile leaves out the maintainers'
  working material: notes, the satisfaction assessment, merge history, edge
  counts and hints (a hint names another profile).
- A read-only user must be a named, signed-in user. An anonymous viewer has
  no profile.
- Writes go through one route for the read-only role,
  `POST /api/customers/me/edit`, which the dashboard's read-only write gate
  lets through by exact method and path. The handlers apply the rules and the
  store's service applies them again on the viewer it is handed, so the two
  cannot drift apart silently.

## 11. GraphQL

`POST /api/graph`, read-only.

- Schema: `me`, `person(id)`, `people(filter, first, after)`, `artefact(ref)`;
  `Person.edges(predicate, first, after)`; `Artefact.people`.
- No mutations and no subscriptions.
- Limits: depth 8, 8 KB query, page size 100, 5 seconds, 5,000 nodes per
  query.
- Suggested people are never returned as a person.
- Each query is audited (viewer, operation name, node count), not its text.

Library: `github.com/graph-gophers/graphql-go` v1.11.1 (BSD-2-Clause),
schema-first, imported only by `src/pkg/customers/gql`. The decision and the
evidence:

- `go mod graph` shows it requires `opentracing-go`, `go.opentelemetry.io/otel`
  and `otel/trace`, plus their indirect modules. Hive already requires the
  OpenTelemetry modules at the same version (v1.47.0), and `opentracing-go` is
  imported only by the library's optional `trace/opentracing` package, which
  Hive does not import. Adding the library added one line to `go.mod` and its
  own two lines to `go.sum`; no other module entered the build.
- It has the limits this endpoint needs built in: `MaxDepth`,
  `MaxQueryLength`, `MaxSelections` and an introspection switch.
- The fallback, `github.com/graphql-go/graphql` with a hand-written depth and
  size check, was not needed.

Introspection is off: the standard introspection query is deeper than the
depth limit anyway, and the schema is published in
[the API reference](../api-reference/customers.md#the-customer-graph). The
node limit is enforced by the resolvers, which charge every person, edge and
artefact they materialise to a per-query budget. Access is not decided in the
GraphQL layer at all: every resolver calls the customers service with the
viewer of the request, the same service the REST routes use.

## 12. Agents and privacy guard rails

**Agents.** Hive selects from the graph for the agent; the agent does not
query it.

- A **customer context** block in the kick prompt for the people on the kicked
  issue: login, tags, confirmed goal, counts. Capped at 1,024 bytes. No emails
  and no person ids. Scanned and fenced as untrusted. The pattern is the
  maintainer-notes block (`src/pkg/scheduler/kick_maintainer_notes.go`).
- A people retriever for the meeting persona
  (`src/pkg/meetings/betty/retrieve.go`).

**Personal data of people who never signed up.** A hive sees issue authors,
meeting attendees and mail senders who never created an account on it. The
position:

1. Such a person is a suggestion only (section 6), and a suggestion expires
   with its evidence.
2. A profile needs a human decision: accept, or add by hand. Both are audited.
3. The owner can forget anyone; the tombstone stops a rebuild.
4. Retention follows the source: an edge cannot outlive its artefact.
5. Nothing leaves the hive. No hub sync, nothing written to GitHub, nothing in
   a heartbeat.

[telemetry.md](../telemetry.md) records the review of Hive's outbound paths
against the Linux Foundation telemetry policy, which says project software
should not collect telemetry by default without review and prior consent. This
feature adds no outbound path, which is what allows it to be on by default.
Any later proposal to send customer data to a hub is its own RFC and falls
under that review.

**Audit.** Profile open, export, forget, merge, split, accept, suggestion
confirm and reject, and graph queries each write an audit line.

**Security model.** [security-model.md](../security-model.md) gains the
customers store and a paragraph on third-party personal data in the PR that
adds the agent context block.

## 13. PR series

| PR | Scope |
|---|---|
| 0 | RFC issue and this page |
| 1 | Core package: model (goals and satisfaction with provenance), store, identity resolution, tags, tombstones; `customers.enabled` |
| 2 | REST API, wiring, hive users, "me" with self-edit for any role, forget |
| 3 | Section UI: list, profile, tags, goals and satisfaction |
| 4 | Sweep steps A to C, automatic tags, retention pruning |
| 5 | GraphQL endpoint; the timeline uses it |
| 6 | Sweep steps D and E, "Same person?" hints |
| 7 | Agent context block, persona retriever, security-model text |
| 8 | Graph view, merge and split UI, export |
| 9 | Goal and satisfaction inference |

PRs 0 to 5 are the first usable slice.

## 14. Verification

Automated:

- the identity resolution table; names never link;
- merge and split round trip; pins block an automatic link;
- a tombstone blocks rediscovery;
- the role matrix on every route (read-only reaches only itself and edits only
  itself);
- zero GitHub calls on a normal sweep;
- a purged artefact removes its edges;
- GraphQL: read-only cannot reach another person by any path; the limits
  reject over-deep and over-size queries;
- goals and satisfaction: inference never overwrites a confirmed or manual
  value; a rejected suggestion is not offered again;
- the prompt block honours its cap and carries no email or id.

On a live hive:

- sign in as owner, read-write and read-only; each sees their own "me"; the
  read-only user sees and edits nothing else, in the UI and by calling routes
  directly;
- add a customer, tag them, set a goal, forget them; check the audit log;
- confirm an issue author and a calendar attendee appear under Suggested;
  accept one and watch the profile fill;
- run a `me` query and a two-hop traversal against `POST /api/graph`; repeat
  as the read-only user;
- let a meeting purge and confirm its edges go;
- kick an issue from a tagged customer and read the prompt; ask the meeting
  persona about a customer by name.

## 15. Open questions

- Should a hub send a spoke the email and linked GitHub login it already holds
  for its users? It fills most blanks at once, but it is a hub change and a
  consent question.
- Does `/data/customers` belong in backups? That moves personal data off the
  hive's volume.
- Is the four-value satisfaction ordinal enough, or is a value above
  `satisfied` wanted?
