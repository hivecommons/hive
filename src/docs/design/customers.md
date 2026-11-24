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
  added to a heartbeat. The one later exception is the opt-in, consent-first
  hub import in [customers-hub-consent.md](customers-hub-consent.md), off by
  default.
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
| `profile` | Optional `real_name`, `organization`, `location` and `country`, each `{value, source, rule, at, by, login}`. `source` is `auto` (the person's own public forge profile name, company and location, read by the profile fill, `rule` naming the source and `login` the login it was read for; the country derived offline from the location, rule `location`) or `manual` (a person typed it or cleared it; neither the fetch nor the location changes it; `auto` hands it back). `country` is an ISO 3166 alpha-2 code. Also `fetched` (the last profile-fill lookup: `at`, `outcome`, `sources`) and `dismissed_affiliations`, the suggested organizations a person refused (at most 20). Nothing here comes from the hub, except a `real_name`, `organization` or `country` the person entered on the hub and consented to share with this hive, when `customers.hub_import.enabled` is on ([customers-hub-consent.md](customers-hub-consent.md)). |

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

Status: built, opt-in. `customers.inference.enabled` defaults to **off**
(env `HIVE_CUSTOMERS_INFERENCE_ENABLED`), because it sends text about people to a
model, some of whom never signed up. It also needs the Customers section on.

Candidates come from what a customer has already put in front of the hive, read
from state the hive holds and with no GitHub call:

| Signal | Goal evidence | Satisfaction evidence |
|---|---|---|
| Issues they opened (the last enumeration) | What they asked for | Whether it is open or closed |
| Pull requests they authored | What they were trying to land | Merged or not |
| Approved mail from their address | The request in the message | The tone of the reply |

Meeting asks are not read yet: a meeting stores display names, never an
identity, and a name never links a person to anything.

The pass is the `customers_inference` optional sweep, every 30 minutes. It
reads at most three due customers, only customers (never a suggested person), and
leaves a person alone for 24 hours after a read and while their evidence has not
changed. It runs on the model route meetings already uses in process
(`meetllm`, through `betty.MeetLLM`): an OpenAI-compatible endpoint or the
Anthropic API. An agent backend is not used. Calls are charged to a daily usage
ledger of their own with a budget of 40,000 tokens, kept for seven days, and a
spent budget ends the pass.

- **Inputs are untrusted.** Each item is cleaned, cut to 300 bytes, and dropped
  whole if it holds an address, one of this store's ids or anything `ioscan`
  finds. What remains sits in one `untrusted-customer-evidence` fence with a
  random nonce, and the system prompt says it is data and never an instruction.
  The prompt carries no name, address, id, note or standing satisfaction level.
- **Output is bounded and typed.** At most three goals, each a short, safe
  sentence that cites only evidence it was given; one satisfaction level from the
  ordinal with such evidence. A goal citing nothing it was given is dropped, a
  number is never a level, and an unusable reply changes nothing and is tried
  again later.
- **Suggestions only.** Goals go through `SuggestGoals` and the level through
  `SuggestSatisfaction`, which never touch a confirmed or manual value and never
  offer a rejected suggestion again. A person confirms, edits or rejects each on
  the profile.
- **Audited without text.** `customer_inference` records the person id, how many
  goals and whether a satisfaction suggestion was filed.
- Nothing inferred leaves the hive.

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
- **E, budgeted GitHub search on accept (opt-in, off by default):** mentions and
  comments for a newly accepted GitHub login, at most two calls per accept and a
  per-hour budget across accepts, shed when the API budget is low. Runs from the
  accept handler, never from the sweep.

A normal sweep (A to D) makes zero GitHub calls, and a test holds that. Step E is
not part of the sweep.

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

Profile fields are filled by the profile fill (below, #19302), which has its own
switch and budget; step E no longer reads the public profile. A suggested person
has no profile fields until accepted. Forget deletes the fields with the person
and export includes them. The hub's own records of the same people
(name, company, country, notes, inferred affiliation) are not read or copied,
except, when the hub import is on, the name, organization and country a person
entered on the hub and consented to share with this hive. Notes and affiliation
never cross. See [customers-hub-consent.md](customers-hub-consent.md).

Country and likely affiliation add no request of their own. The country is derived
from the location text by a bundled table (country names and aliases, ISO codes, US
states and Canadian provinces, major cities; `pkg/customers/country_data.go`), offline
and deterministically. The last comma-separated piece decides, a bare two-letter code
after a comma reads as a US state first unless a city of another country precedes it
("Berlin, DE"), and "remote", "Earth" or an empty location give no country. It is a
guess about free text: a small city with no state or country, or a location that is
a joke, can be wrong or empty, so it is an automatic field a person overwrites or
clears, and a hand-set or hand-cleared country is never touched. A likely
affiliation is a view-time suggestion, never stored: the GitHub company text tidied
(legal suffix and "@" dropped, a bare handle capitalized), or, while the organization
is empty, the domain of an email identity a person confirmed by hand (never a
free-mail or code-host domain, and the address itself is not shown). It carries its
evidence ("from GitHub company field"), Accept writes it as a by-hand organization,
Dismiss remembers the refusal on the profile. Public GitHub organization membership
is deliberately not used: it is not fetched, and fetching it would be a new request.
Notes are free text up to 4000 bytes, written by the owner and read-write users, never
shown to a read-only user (not even on their own profile), part of the owner's export
and deleted with the person on Forget.

Step E, the budgeted GitHub search on accept, is built and opt-in.
`customers.mention_search.enabled` defaults to **off** (env
`HIVE_CUSTOMERS_MENTION_SEARCH_ENABLED`), because it asks GitHub about a person's
login and spends shared API quota, and the section otherwise adds no outbound
path. It also needs the Customers section on. When it is on and an owner or
read-write user accepts a suggested person (or attaches one to an existing
customer), the hive runs one search for that identity, so the new profile starts
with history beyond what the hive already holds:

- **Accept only.** The call path is the accept handler, never the discovery
  sweep, which is still handed no GitHub client. A test holds both.
- **GitHub logins only.** A suggested email address is never sent to GitHub.
- **Scoped to the hive's own repositories.** The query is `mentions:<login>` and
  then `commenter:<login>`, each limited to the configured project repositories
  (as many as fit GitHub's 256-character query limit). With no repositories there
  is nothing to search and no call is made.
- **Hard per-accept budget.** At most two search requests, one page of 30 results
  each, so at most 60 results kept per accept.
- **Per-hour budget across accepts.** `customers.mention_search.max_requests_per_hour`
  (default 10, at most 60) bounds the requests all accepts spend together in a
  rolling hour. A spent budget skips the search; the profile still gets created.
- **Shared quota.** The requests go through the shared GitHub transport, so they
  count in the request accounting (caller `customers_mention_search`), carry the
  low pacing class, and stand down when the API budget mode is not normal, when
  the core or search remaining is low, or while the secondary-limit caution
  window is engaged.
- **Edges, deduplicated.** Each hit becomes a `mentioned_in` or `commented_on`
  edge on the issue or pull request (source `github_search`). An edge the index
  already holds is left as it is, and a hit seen by both queries is written once
  per predicate.
- **Privacy.** The edge is a reference, as for every other source: no title, body
  or comment text is read into the store. The log line carries the person id and
  counts only, never the login, a query or a token. It runs in the background, so
  the accept does not wait for GitHub, and a failure never undoes the accept.

### Profile fill

The profile fill (#19302) fills a confirmed customer's real name, organization and
location from their own public profile on a forge, and derives the country from the
location. It is opt-in: `customers.profile_fill.enabled` defaults to **off** (env
`HIVE_CUSTOMERS_PROFILE_FILL_ENABLED`, or Settings, Features), because it is a
periodic network lookup about people and the goal above is no new network calls
on a normal sweep. It needs the Customers section on and is independent of
`customers.mention_search`.

- **Sources.** One per forge, behind one interface (`customers.ProfileLookup`):
  the hive's project-wide forge, then Linear. GitHub and GitHub Enterprise go
  through the shared GitHub client (`GET /users/{login}`, and for a company field
  of exactly `@handle`, `GET /orgs/{handle}` for the organization's public display
  name, cached for a day); GitLab through the `pkg/forge` adapter (username search,
  then `GET /users/{id}`); Gitea and Forgejo through theirs (`GET /users/{login}`,
  no company field). When a Linear workspace is connected, one GraphQL query reads
  the workspace member's name by a confirmed email address; Linear has no company
  or location. A forge that cannot be reached (an enterprise server this hive has
  no route to, say) is "no data". The website and avatar a profile names are read
  but never stored or fetched. Organization membership lists are not fetched.
- **Who.** Confirmed customers only (origin `hive_user`, `manual` or
  `accepted`), never a suggestion. A forge is asked about the person's first
  confirmed login identity, else a dashboard user name shaped like a login; Linear
  only about a confirmed email. A bot login is never asked about.
- **Fill rules.** Precedence is: a value set here by hand, then a hub value the
  person entered or consented to share, then the public forge profile, then a hub
  value the hub derived or an admin set (hub rules other than `hub_consent`,
  `hub_person` and `hub_unrecorded`). So a manual or person-entered hub field is
  never touched, and a derived or admin hub value is replaced only by a non-empty
  forge value (a derived hub country by the country of a forge location). An
  empty field is filled. A field an earlier fill wrote from a source that answered this time is
  the fill's own: it is updated, or cleared when the person removed it from their
  profile. Any other value is kept. The first source with a value wins, forge
  before Linear. Each field records `source: auto`, `rule` (the profile source:
  `github_profile`, `ghe_profile`, `gitlab_profile`, `gitea_profile` or
  `linear_profile`), `login` and `at` (fetched); the dashboard tooltip shows them.
  The organization is the company field with a leading `@` dropped, or the GitHub
  organization's public name. The country is `DeriveCountry` over the location:
  an unplaceable or ambiguous location gives no country.
- **When.** A pass runs 2 minutes after boot and then every 15 minutes. It looks
  up at most 20 people, never-fetched first (so existing customers, dashboard
  users included, are backfilled in the first passes), then the stalest; a
  success is refreshed after 7 days, a failure retried after 24 hours. Accepting a
  suggestion fills that person in the background. "Refresh from profiles" on one
  customer runs it now; for everyone it makes every customer due again.
  `profile.fetched` records the last lookup (`at`, `outcome`, `sources`).
- **Budget.** `customers.profile_fill.max_requests_per_hour` (default 60, at most
  300) bounds every lookup together, reserved per lookup before it is made. The
  GitHub source stands down while the API budget mode is not normal, the
  secondary-limit caution window is engaged or the core bucket is low, so agents
  are never starved; its requests carry the low pacing class and the caller label
  `customers_profile_lookup`. A rate-limit refusal from any forge stops the pass
  and backs off 15 minutes, doubling to at most 6 hours, until a lookup answers.
- **Privacy.** Only text the person published on their own profile. The log
  line carries counts and a fixed skip word, never a login, an email or a value.

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

**Merge, split and the graph view.** Merge folds one customer into another and
moves every edge across; the merge is recorded on the target with the source as
it stood and the identities, tags and goals it brought, so unmerge can give back
exactly those. Unmerge moves back only the edges whose `via` identity belonged
to the restored person, so activity the target produced on its own stays put.
Split works the same way in the other direction: the edges that the named
identities produced move with them. Two dashboard users are never merged, and a
`hive_user` identity is never split off. Both are full-view actions and audited
(`customer_merge`, `customer_unmerge`, `customer_split`). Export is owner only
and audited (`customer_export`); it holds the profile, merge history, hints and
at most 10,000 edge references.

The graph view is a static SVG drawn from the same GraphQL endpoint (one query:
a person's edges and, under each, the people on that artefact). It does not
reuse the knowledge graph canvas, which is animated and styled inline; the
customers view is a small radial drawing that stays readable on a phone and
follows the page's design tokens.

## 12. Agents and privacy guard rails

**Agents.** Hive selects from the graph for the agent; the agent does not
query it.

- A **customer context** block in the kick prompt for the people on the kicked
  issue: login, tags, confirmed goal, counts. Capped at 1,024 bytes. No emails
  and no person ids. Scanned and fenced as untrusted. The pattern is the
  maintainer-notes block (`src/pkg/scheduler/kick_maintainer_notes.go`).
- A people retriever for the meeting persona
  (`src/pkg/meetings/betty/retrieve_people.go`).

What crosses into a prompt is built in one place, `customers.RenderKickContext`
and `customers.PeopleBriefs`, and the scheduler and the meetings package see
only the finished text or a small struct, never the customers package:

- Only customers; a suggested person is never included. A login is matched on
  the exact GitHub identity.
- Only a login, the active tags, up to three goals a person confirmed, and a
  count. Never an email, an id, notes or the satisfaction assessment. A goal the
  hive inferred and nobody confirmed stays out.
- A goal is text a person typed, so `SafeGoal` scans it with `ioscan`, removes
  control characters, and drops the whole goal if it holds anything that looks
  like an address or one of this store's ids, or if `ioscan` finds anything.
  What remains sits inside one `<untrusted-customer-data nonce="...">` fence
  with a fresh nonce, and any fence marker in the text is rewritten.
- The kick block is at most 1,024 bytes, people lines first, and a line that
  does not fit is left out rather than cut. The scheduler appends it only when
  it starts with the block header, and asks about at most 20 logins.
- The persona's snippet is a name, tags and confirmed goals, nothing else. The
  persona speaks in a room that can hold people who are not users, so notes and
  the satisfaction assessment have no field to travel in. Betty fences snippets
  as data in its own prompt as well.

Retrieval by name here is not identity resolution: a word in a question is
matched against the words of a customer's name or their login to find a profile
to describe, and nothing is linked or created.

**Personal data of people who never signed up.** A hive sees issue authors,
meeting attendees and mail senders who never created an account on it. The
position:

1. Such a person is a suggestion only (section 6), and a suggestion expires
   with its evidence.
2. A profile needs a human decision: accept, or add by hand. Both are audited.
3. The owner can forget anyone; the tombstone stops a rebuild.
4. Retention follows the source: an edge cannot outlive its artefact.
5. Nothing leaves the hive. No hub sync, nothing written to GitHub, nothing in
   a heartbeat. The one thing a browser fetches for this section is a public GitHub
   picture, described next.

**Avatars.** The one exception to "nothing leaves the hive" is not data the hive
sends. The Customers list shows a GitHub picture next to a person who has a
confirmed GitHub login, so the viewer's browser requests
`https://github.com/<login>.png` (GitHub redirects to its own image host) for
each such person on screen, lazily, with `referrerpolicy="no-referrer"`. GitHub
sees the viewer's IP address and which logins were requested, so it can tell
that someone looking at this dashboard has those GitHub users in their customer
list. It is not sent the hive's address, the page, a token, a name, a tag or
anything else about the person. The hive's server does not fetch, proxy or
store the pictures. A person known only by
email, Slack, Discord, Teams or Meet gets an initial or a source icon, and no
service keyed by an email address or its hash (Gravatar and the like) is ever
asked, because that would hand the address to a third party. To turn pictures
off set `customers.avatars.enabled: false`, set `HIVE_CUSTOMERS_AVATARS_ENABLED=false`,
or switch **Customer avatars** off in Settings, Features; the list then shows
initials and makes no request to github.com. The default is on, because the
dashboard already loads GitHub avatars for contributors and sign-ins.

[telemetry.md](../telemetry.md) records the review of Hive's outbound paths
against the Linux Foundation telemetry policy, which says project software
should not collect telemetry by default without review and prior consent. This
feature adds no outbound path from the hive, which is what allows it to be on by default. The avatar request above is made by the viewer's browser to a site they already use, and has its own switch.
Any later proposal to send customer data to a hub is its own RFC and falls
under that review.

**Enrichment.** [Customers enrichment](../customers-enrichment.md) is the one
reviewed outbound path for this section, and it is off by default with a
switch per source. gitdm downloads public CNCF files and sends nothing about a
person; devstats and crowd.dev send a confirmed customer's public GitHub login
only, and are listed in [telemetry.md](../telemetry.md). Suggested people are
never looked up, an affiliation found this way is only a suggestion, and Forget
erases enrichment.

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
  consent question. Decided in [customers-hub-consent.md](customers-hub-consent.md):
  only on the person's per-hive consent, only values they entered, off by default.
- Does `/data/customers` belong in backups? That moves personal data off the
  hive's volume.
- Is the four-value satisfaction ordinal enough, or is a value above
  `satisfied` wanted?
