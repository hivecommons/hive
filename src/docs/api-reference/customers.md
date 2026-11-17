# Dashboard REST API reference — Customers

Part of the [Dashboard REST API reference](../api-reference.md); see that page for how auth levels and `pkg/<file>:<line>` source citations are read and checked. Edit this file for routes in this area and run `src/scripts/check-api-reference-citations.sh --fix` when a registration moves.

Customers is a profile per person: identities, tags, goals, a satisfaction level and notes. Design: [customers](../design/customers.md). Three levels of access apply to every route below:

- **Full view** is the verified owner and read-write users. They see every customer and every suggested person.
- **Owner only** adds forget.
- **Self** is any other signed-in, named user. They see only their own profile, addressed as `me`, and may change only its name, identities, tags and goals. Any other id answers 404, exactly as an id that does not exist. The dashboard refuses every other write from a read-only user; `POST /api/customers/me/edit` is the one write it lets through.

The rules are enforced twice: by the handlers and again by the store's service on the viewer it is handed. With `customers.enabled` off the list answers `{"enabled": false}` and every other route answers 503. Nothing these routes return leaves the hive.

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/customers` | Signed-in user | The Customers section: `{enabled, full, owner, me, people, suggestions, default_tags, levels}`. `me` is the viewer's own profile, created on first read. `people` and `suggestions` are empty without the full view | `pkg/dashboard/api_customers.go:128` |
| `POST` | `/api/customers` | Full view | Add a customer by hand: `{name, identities?: [{kind, value}]}`. 409 when an identity belongs to another customer; a `hive_user` identity is refused. Audited as `customer_add` | `pkg/dashboard/api_customers.go:129` |
| `GET` | `/api/customers/{id}` | Signed-in user; self reaches only `me` | One profile, with edge counts and open "Same person?" hints for the full view. A viewer without the full view gets no notes, satisfaction, merge history, counts or hints. Opening someone else's profile is audited as `customer_profile_open` | `pkg/dashboard/api_customers.go:130` |
| `POST` | `/api/customers/{id}/edit` | Full view; self only as `/api/customers/me/edit` | One change: `{op, ...}` with `op` one of `name`, `notes`, `identity_add` (`kind`, `value`), `identity_remove` (`key`), `tag` (`tag`, `state` of `on`, `off` or `auto`), `goal_add` (`text`), `goal_edit` (`goal_id`, `text`), `goal_confirm`, `goal_reject` (`goal_id`), `satisfaction_set` (`level`), `satisfaction_confirm`, `satisfaction_reject`. Self may use the name, identity, tag and goal ops; an identity added by self is an unconfirmed claim that resolves nobody until the full view confirms it with `identity_add`. Audited as `customer_edit` with the person and the op, never the value | `pkg/dashboard/api_customers.go:131` |
| `DELETE` | `/api/customers/{id}` | Owner only | Forget: deletes the customer, their identities and every edge to them, and keeps one salted-hash tombstone per confirmed identity so discovery does not rebuild them. 200 `{forgotten, edges_removed}`. Audited as `customer_forget` | `pkg/dashboard/api_customers.go:132` |
| `POST` | `/api/customers/suggestions/{sid}/accept` | Full view | Turn a suggested person into a customer. Audited as `customer_accept` | `pkg/dashboard/api_customers.go:133` |
| `POST` | `/api/customers/suggestions/{sid}/dismiss` | Full view | Delete a suggestion and tombstone its key, so it is not offered again. Audited as `customer_dismiss` | `pkg/dashboard/api_customers.go:134` |
