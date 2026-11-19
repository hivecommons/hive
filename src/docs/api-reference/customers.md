# Dashboard REST API reference — Customers

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` when a registration moves.

Customers is a profile per person: identities, tags, goals, a satisfaction level and notes. Design: [customers](../design/customers.md). Three levels of access apply to every route below:

- **Full view** is the verified owner and read-write users. They see every customer and every suggested person.
- **Owner only** adds forget.
- **Self** is any other signed-in, named user. They see only their own profile, addressed as `me`, and may change only its name, identities, tags and goals. Any other id answers 404, exactly as an id that does not exist. The dashboard refuses every other write from a read-only user; `POST /api/customers/me/edit` is the one write it lets through.

The rules are enforced twice: by the handlers and again by the store's service on the viewer it is handed. With `customers.enabled` off the list answers `{"enabled": false}` and every other route answers 503. Nothing these routes return leaves the hive.

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/customers` | Signed-in user | The Customers section: `{enabled, full, owner, me, people, suggestions, default_tags, levels}`. `me` is the viewer's own profile, created on first read. `people` and `suggestions` are empty without the full view | `pkg/dashboard/api_customers.go:178` |
| `POST` | `/api/customers` | Full view | Add a customer by hand: `{name, identities?: [{kind, value}]}`. 409 when an identity belongs to another customer; a `hive_user` identity is refused. Audited as `customer_add` | `pkg/dashboard/api_customers.go:179` |
| `GET` | `/api/customers/{id}` | Signed-in user; self reaches only `me` | One profile, with edge counts and open "Same person?" hints for the full view, and `affiliation_suggestion` (`{value, rule, evidence}`, plus `source`, `from` and `until` when it came from [enrichment](../customers-enrichment.md), never stored) when a likely organization can be offered, and `public_contributions` (`[{project, contributions, period, url, source}]`, read only) when devstats enrichment is on. A viewer without the full view gets no notes, satisfaction, merge history, counts or hints. Opening someone else's profile is audited as `customer_profile_open` | `pkg/dashboard/api_customers.go:180` |
| `POST` | `/api/customers/{id}/edit` | Full view; self only as `/api/customers/me/edit` | One change: `{op, ...}` with `op` one of `name`, `notes`, `profile` (`field` of `real_name`, `organization`, `location` or `country`, `value`; a country is an ISO 3166 alpha-2 code or a country name; `state` of `auto` hands the field back to the automatic source, and a country is then derived again from the location), `affiliation_dismiss` (`value`, a suggested organization the person refused), `identity_add` (`kind`, `value`), `identity_remove` (`key`), `tag` (`tag`, `state` of `on`, `off` or `auto`), `goal_add` (`text`), `goal_edit` (`goal_id`, `text`), `goal_confirm`, `goal_reject` (`goal_id`), `satisfaction_set` (`level`), `satisfaction_confirm`, `satisfaction_reject`. Self may use the name, profile, affiliation_dismiss, identity, tag and goal ops; an identity added by self is an unconfirmed claim that resolves nobody until the full view confirms it with `identity_add`. Audited as `customer_edit` with the person and the op, never the value | `pkg/dashboard/api_customers.go:181` |
| `DELETE` | `/api/customers/{id}` | Owner only | Forget: deletes the customer, their identities and every edge to them, and keeps one salted-hash tombstone per confirmed identity so discovery does not rebuild them. 200 `{forgotten, edges_removed}`. Audited as `customer_forget` | `pkg/dashboard/api_customers.go:182` |
| `POST` | `/api/customers/suggestions/{sid}/accept` | Full view | Turn a suggested person into a customer. With the optional body `{into}` (an existing customer id, or `me`) it attaches the suggestion to that customer instead: the identity becomes one of theirs, pinned as a manual decision, no new person is created, the activity moves onto them on the next sweep, and `unmerge` undoes it with the id in the target's merge history. Audited as `customer_accept`, or `customer_attach` with the target's id and never a name. When `customers.mention_search.enabled` is on and the suggestion is a GitHub login, a budgeted background GitHub search adds `mentioned_in` and `commented_on` edges after the accept (see the design doc, step E) | `pkg/dashboard/api_customers.go:183` |
| `POST` | `/api/customers/suggestions/{sid}/dismiss` | Full view | Delete a suggestion and tombstone its key, so it is not offered again. Audited as `customer_dismiss` | `pkg/dashboard/api_customers.go:184` |
| `POST` | `/api/customers/hints/{hid}/answer` | Full view | Answer a "Same person?" question: `{same}`. `true` makes the suggested identity the customer's own and deletes the suggestion; `false` closes the question for good. A question between two customers is answered by merging them, so `true` on one is 400. Audited as `customer_hint_answer` | `pkg/dashboard/api_customers.go:185` |
| `POST` | `/api/customers/{id}/merge` | Full view | Fold the customer `{from}` into this one: identities, tags, goals and activity move across and the merge is recorded. 409 for two dashboard users. Audited as `customer_merge` | `pkg/dashboard/api_customers.go:186` |
| `POST` | `/api/customers/{id}/unmerge` | Full view | Undo a merge: `{from}` comes back as it stood, with exactly what the merge brought it. Audited as `customer_unmerge` | `pkg/dashboard/api_customers.go:187` |
| `POST` | `/api/customers/{id}/split` | Full view | Move identities `{identities: ["kind:value"], name?}` off this customer onto a new one, with the activity they produced; returns the new customer. Audited as `customer_split` | `pkg/dashboard/api_customers.go:188` |
| `GET` | `/api/customers/{id}/export` | Owner only | Everything held about one customer as a JSON download: the profile, merge history, hints and up to 10,000 activity references. Audited as `customer_export` | `pkg/dashboard/api_customers.go:189` |
| `GET` | `/api/customers/enrichment/settings` | Owner only | Whether the crowd.dev API token used by [customers enrichment](../customers-enrichment.md) is set: `{crowddev_token_set, crowddev_token_env_override}`. The token is never returned. Works whether or not Customers is on. The enrichment switches and limits are Features toggles on `GET /api/config/governor` | `pkg/dashboard/api_customers_enrichment.go:32` |
| `POST` | `/api/customers/enrichment/settings` | Owner only | Seal the crowd.dev API token into `customers.enrichment.crowddev.token_file` through the keystore: `{crowddev_token}`; `""` clears it, an absent field leaves it alone. Audited as `customers_enrichment_settings` with whether the token changed, never its value | `pkg/dashboard/api_customers_enrichment.go:33` |
| `POST` | `/api/graph` | Signed-in user | The customer graph over GraphQL, read-only (no mutations, so a read-only user may call it): `{query, operationName?, variables?}`, answered with a GraphQL `{data, errors}` document. Access follows the viewer exactly as above; see [the customer graph](#the-customer-graph). Audited as `customer_graph_query` with the operation name, node count and error count, never the query text | `pkg/dashboard/api_graph.go:47` |

## The customer graph

`POST /api/graph` answers GraphQL queries over people, the artefacts they touched and the edges between them. An artefact is a reference (`gh:issue:org/repo#12`, `meeting:<id>`, `inbox:email:<id>`, `event:<id>`), never content.

Rules that hold for every query:

- It is **read-only**. There is no `Mutation` and no `Subscription` type. Changes go through the REST routes above, which carry the role checks and the audit lines.
- **Every field that returns a person checks the viewer.** With the full view you can reach every customer. Without it you reach only yourself: `person(id)` answers `null` for anyone else (the same answer as an id that does not exist), `people` lists only you, and an artefact's `people` lists only you.
- **Suggested people are never returned** as a person.
- **An edge is shown only if you could open its artefact where it lives.** A meeting is the owner's to see; Calendar events and Inbox items are for the owner and read-write users.
- `satisfaction` and `notes` are `null` without the full view.

Limits per query: depth 8, 8 KB of query text, pages of at most 100 (25 when `first` is not given), 5,000 people, edges and artefacts in total, and 5 seconds. Introspection is off; this is the schema:

```graphql
type Query {
  me: Person!
  person(id: ID!): Person
  people(tag: String, first: Int, after: String): PersonConnection!
  artefact(ref: String!): Artefact
}

type Person {
  id: ID!
  name: String!
  origin: String!            # hive_user, manual or accepted
  me: Boolean!
  tags: [Tag!]!
  identities: [Identity!]!
  goals: [Goal!]!
  satisfaction: Satisfaction # null without the full view
  notes: String              # null without the full view
  counts: [Count!]!
  edges(predicate: String, first: Int, after: String): EdgeConnection!
}

type PersonConnection { nodes: [Person!]!  next: String }
type Tag { name: String!  on: Boolean!  source: String!  rule: String }
type Identity { kind: String!  value: String!  source: String  manual: Boolean!  claimed: Boolean! }
type Goal { id: ID!  text: String!  state: String!  source: String!  evidence: [String!]! }
type Satisfaction { level: String!  source: String  evidence: [String!]!  suggestedLevel: String  suggestedEvidence: [String!]! }
type Count { predicate: String!  count: Int! }
type Edge { predicate: String!  at: String!  source: String  artefact: Artefact! }
type EdgeConnection { nodes: [Edge!]!  next: String }
type Artefact { ref: String!  kind: String!  people(first: Int): [ArtefactPerson!]! }
type ArtefactPerson { predicate: String!  person: Person! }
```

Your own profile:

```sh
curl -s -X POST "$HIVE/api/graph" -H 'Content-Type: application/json' \
  -d '{"query":"{ me { name tags { name on source } goals { text state } } }"}'
```

Two hops, from a person to the artefacts they touched to everyone else on them:

```graphql
query Around($id: ID!) {
  person(id: $id) {
    name
    edges(first: 20) {
      nodes {
        predicate
        at
        artefact { ref people { predicate person { name } } }
      }
      next
    }
  }
}
```

Page with `after`: pass the `next` value of the previous page; `next` is `null` on the last page.
