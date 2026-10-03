# ADR-0020: External work sources over a versioned HTTP/JSON contract

Status: Accepted — implemented in #10291 (config block, primary registry,
`external` adapter, tests, and provider docs). Write-back
(`hive.worksource/v2`) and the open questions below are still open.

Tracked in #10174. Implementation in #10291.

## Context

A work source is the Step 01 seam of the governor loop: it lists the actionable
items Hive may dispatch. The seam is the `worksource.WorkSource` interface in
`src/pkg/worksource/worksource.go`:

```go
type WorkSource interface {
	SourceType() string
	ListIssues(ctx context.Context) ([]Issue, error)
}
```

Every primary adapter (GitHub Issues, GitHub Projects, Linear, Jira, Gitea,
GitLab) is compiled into the hive binary and selected by the `switch cfg.Type`
in `worksource.FromConfig` (`src/pkg/worksource/factory.go`). Additive sources
(run stages, Wavefront) use a separate compile-time registry,
`worksource.RegisterAdditive`, and are composed by `AppendAdditive`. A team
whose tracker is not on that list must send a PR to Hive. The
[integration guide](../integration-guide.md) lists this as a known gap, and
[Work source providers](../integrations/work-source-providers.md) says it
directly: "a new primary provider currently requires a PR".

#10174 asks for an external boundary, but only if it keeps four properties that
the in-tree adapters get from being Hive code:

1. **Source-neutral work identity.** `worksource.Ref.Key()`
   (`src/pkg/worksource/key.go`) is the only identity that holds, cooldowns,
   claims, queue order, failure quarantine, and active-work tracking use. It is
   `repo#N` for GitHub-backed items and `repo!externalID` for string-keyed
   items. These keys are stored on disk. A provider that mints a key in the
   wrong shape can take over the identity of another item, or collapse many
   items onto one identity (the `repo#0` bug that `Ref` was written to fix).
2. **Secrets handling.** Credentials are written as whole-value env
   references (`${LINEAR_API_KEY}`). `resolveSecretRef` resolves them when they
   are used. The dashboard overlay stores only the reference, never the value.
3. **Admission behavior.** `workSourceIssuesForCycle`
   (`src/cmd/hive/eval_cycle_seams.go`) fails closed when the config is bad or
   enumeration fails. It returns no issues but still does GitHub PR
   maintenance. It then applies `github.FilterExemptIssues` and
   `IssueFilterConfig.Admits`. Holds, cooldowns, and contributor admission run
   later and key on `Ref.Key()`.
4. **Dashboard terminology.** User-facing copy says **work source**,
   **provider**, **issue**, and **project**, following
   [Work-source terminology](../work-source-terminology.md). A source that is
   not GitHub must never be labelled as GitHub.

This ADR compares the candidate shapes from #10174 and fixes the wire contract
for `SourceType` and `ListIssues`.

## Options considered

| Option | Summary | Verdict |
|---|---|---|
| A. HTTP/JSON shim | Hive calls a provider-run HTTPS endpoint that returns `worksource.Issue`-shaped JSON. | **Chosen.** |
| B. gRPC shim | Same semantics, carried as protobuf over gRPC. | Deferred. |
| C. MCP-style external process | Hive starts a provider binary and talks JSON-RPC over stdio, as with MCP servers. | Rejected for v1. |
| D. Go `plugin` / shared object | The binary loads a `.so` that implements `WorkSource`. | Rejected. |

**A** follows a pattern Hive already has. The Wavefront additive source reads
its graph from `work_source.wavefront.url` using a plain HTTP(S) GET. An HTTP
shim also works with any language, can be tested with `curl`, and puts the
provider's process lifecycle outside Hive. The provider already runs a service
that can reach its tracker, so a small shim next to it costs little.

**B** has the same semantics but needs protobuf toolchains on both sides and a
new gRPC dependency in the hub. It is harder to debug by hand, and the
contract is small enough that schema tooling adds little value. If a later
version needs streaming, gRPC can carry the message shapes below unchanged.

**C** would make the hub start and supervise arbitrary provider binaries. The
child would inherit the hub's environment, which on a hub includes forge and
dashboard credentials. Hive would also own restart, zombie, and resource-limit
behavior for that child. MCP tool calls are built for models to invoke, not as
a typed enumeration contract. A provider that already has an MCP server can put
a thin HTTP adapter in front of it.

**D** breaks whenever the Go toolchain or module graph changes. It does not
work on every platform Hive ships for, and it runs third-party code inside the
hub process with full privileges.

## Decision

Add a primary work source of type `external`. It implements `WorkSource` by
calling a provider-operated HTTP/JSON endpoint. It is read-only. Identity,
secrets, and admission stay under Hive's control.

### Configuration

```yaml
governor:
  work_source:
    type: external
    external:
      name: acme                      # stable SourceType; ^[a-z][a-z0-9_]{1,31}$
      display_name: Acme Tracker      # dashboard label; defaults to name
      base_url: https://acme-shim.internal:8443
      auth_token: ${ACME_WORKSOURCE_TOKEN}   # must be an env reference
      ca_bundle: ${ACME_WORKSOURCE_CA}       # optional, env reference (PEM)
      repos: [your-org/app, your-org/platform]  # required allow-list
      hold_labels: [hold, blocked]    # applied by Hive as well as the provider
      timeout_seconds: 30             # whole ListIssues budget; default 30
```

`config.ExternalSourceConfig` sits next to the other blocks in
`src/pkg/config/work_sources.go`. Its `Validate()` enforces:

- `name` matches `^[a-z][a-z0-9_]{1,31}$` and is not a reserved source type:
  `github`, `github_projects`, `linear`, `jira`, `gitea`, `gitlab`, `run`,
  `wavefront`, `external`.
- `base_url` uses `https://`. Plain `http://` is allowed only for loopback
  hosts (`localhost`, `127.0.0.0/8`, `::1`), so a sidecar shim works.
- `auth_token` and `ca_bundle` are **whole-value env references**. A literal
  value fails validation. Linear and Jira accept literals; this adapter does
  not, so a resolved provider token never reaches the dashboard overlay on
  disk.
- `repos` is non-empty and each entry is `owner/name`.

### Registry and selection

`FromConfig`'s `switch` becomes a primary-builder registry, built like
`RegisterAdditive`:

```go
// PrimaryDeps carries what built-in adapters need today from FromConfig's
// parameters; external providers use only Logger.
type PrimaryDeps struct {
	GitHub      *github.Client
	GitHubToken string
	GitHubOrg   string
	Logger      *slog.Logger
}

type PrimaryBuilder func(cfg config.WorkSourceConfig, deps PrimaryDeps) (WorkSource, error)

// RegisterPrimary panics on a duplicate name, like RegisterAdditive.
func RegisterPrimary(name string, build PrimaryBuilder)
```

The built-in types register themselves under their current names, and `""`
still maps to `github`. `external` is registered the same way. An unknown
`type` is still a config error, so `workSourceIssuesForCycle` keeps failing
closed. `FromConfig` keeps its signature and builds `PrimaryDeps` from its
parameters. Callers (`cmd/hive/eval_cycle_seams.go`,
`pkg/dashboard/design_spektacular.go`) do not change.

Only one external provider can be the primary at a time. That matches today's
one-primary model. Using an external provider as an additive source is an open
question.

### Wire contract `hive.worksource/v1`

Every request carries `Authorization: Bearer <resolved auth_token>`,
`Accept: application/json`, and `Hive-Worksource-Contract: hive.worksource/v1`.
Hive does not follow redirects to a different host, so the bearer token is
never forwarded.

**`GET {base_url}/v1/source`.** Hive calls this once at construction and on
the dashboard "test connection" action. It never calls it per cycle.

```json
{"contract": "hive.worksource/v1", "source_type": "acme", "display_name": "Acme Tracker"}
```

`source_type` must equal the configured `name`. A mismatch is a config error.
`SourceType()` returns the configured `name` and makes no network call.

**`GET {base_url}/v1/issues?cursor=<opaque>`.** Hive calls this for each
`ListIssues`, following `next_cursor` until it is empty.

```json
{
  "contract": "hive.worksource/v1",
  "items": [{
    "repo": "your-org/app",
    "external_id": "ACME-123",
    "title": "Retry webhook delivery",
    "body": "…",
    "author": "jdoe",
    "labels": ["bug"],
    "assignees": [],
    "is_tracker": false,
    "priority": "high",
    "state": "Todo",
    "created_at": "2026-09-30T12:00:00Z",
    "updated_at": "2026-10-01T08:00:00Z",
    "url": "https://acme.example/issues/ACME-123",
    "depends_on": [{"repo": "your-org/app", "external_id": "ACME-100", "resolved": false}]
  }],
  "next_cursor": ""
}
```

The item fields use the same names as the JSON tags on `worksource.Issue`.
`depends_on` needs its own wire shape because `Dependency.Ref` has the tag
`json:"-"`. These fields are **not** accepted from the wire: `source_type`,
`number`, `stage`. Hive sets `SourceType = name`, `Number = 0`, and
`Stage = ""`. If an item contains any of them with a non-zero value, Hive
rejects the item.

Limits (constants in the adapter, raised only by a contract revision):
50 pages, 5,000 items, and 8 MiB response body per page. The whole call is
bounded by `timeout_seconds`. Exceeding any limit fails the call.

### Identity and attribution rules

Hive validates each item **before** it builds a `Ref`:

1. `repo` must be in `repos`. A provider cannot send agents to clone or open
   change requests against a repository the operator did not list.
2. `external_id` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. With no `!`,
   `#`, `:`, `/`, or whitespace allowed, `Ref.Key()` always produces the
   `repo!externalID` form. The key can never parse as a GitHub `repo#N` key
   (`ParseKey`), and `isRunStageExternalID` can never read it as a run-stage
   key. External items therefore cannot take the identity of GitHub-backed
   work, run stages, or Wavefront nodes (`<graph>:<node>`).
3. `Number` is always `0`, so `Ref.IsGitHubIssue()` is false. The
   GitHub-only observers (PR-claim ledger, bead dependency gate) skip these
   items, as they skip Linear and Jira items today.
4. Two items with the same `Ref.Key()` in one listing fail the **whole** call.
   Keeping either one would be a guess about which item holds and claims
   should apply to.
5. A `depends_on` entry must pass rules 1 and 2. If it does not, Hive keeps
   the item and drops the edge, logging the field and value. Hive never
   invents a resolved dependency.
6. `url` must be `https` (or loopback `http`) or empty. `author` and
   `assignees` are shown as provider-native handles and are never mapped to
   GitHub logins.

An item that fails rule 1, 2, or 6 (or carries a rejected wire field) is dropped. Hive logs
the `name`, the offending field, and the `external_id` if it is safe to print.
A per-source counter of dropped items is shown on the dashboard. Dropping an
item withholds it: an item that fails validation is never dispatched.

Because the key is persisted, a provider must keep `external_id` stable for
the life of an item. If the provider renumbers items, every hold and cooldown
for them is lost. The provider docs must state this in bold.

### Secrets

- Hive resolves `auth_token` and `ca_bundle` with `resolveSecretRef` at
  `FromConfig` time and keeps them only in memory. The saved config and the
  dashboard overlay (`PUT /api/config/governor/work-source`) store only the
  reference. `GET` returns the reference string, never the value.
- The provider receives only its own bearer token. It never receives the
  GitHub App installation token, `HIVE_GITHUB_TOKEN`, dashboard tokens, or
  minted agent credentials (ADR-0007).
- Error messages and logs report the env variable name, never the value. HTTP
  error bodies from the provider are cut to 512 bytes and passed through the
  existing log redaction before they are logged.

### Admission

The boundary adds **no** admission authority. The wire has no field that
admits an item, bypasses a hold, or sets priority above the normalized enum.

- `ListIssues` errors (transport failure, non-2xx, contract mismatch, bad
  JSON, a limit exceeded, a duplicate key) go to the existing fail-closed
  branch in `workSourceIssuesForCycle`. The cycle lists no issues, PR
  maintenance continues, and the error is logged with
  `source=<name>`.
- `hold_labels` are applied inside the adapter, the same way Jira does it,
  even though the provider is also expected to filter. The governor still
  applies `github.FilterExemptIssues` and `IssueFilterConfig.Admits`
  afterwards.
- Operator holds, queue order, cooldowns, quarantine, and contributor
  admission key on `Ref.Key()` and do not change.
- `priority` outside `urgent|high|medium|low|none` becomes `""`.

### Write-back

v1 is read-only. The external source implements none of `LabelMutator`,
`Commenter`, or `StatusTransitioner`. Design mode already treats a missing
interface as "unsupported" and shows that message. A later contract revision
(`hive.worksource/v2`) may add `POST /v1/items/{external_id}/labels`,
`…/comments`, and `…/transitions`, each behind the matching optional
interface.

### Dashboard terminology

- The source badge and Settings → Work source show `display_name` (falling
  back to `name`), using the words **work source**, **provider**, and
  **issue**. Copy never says "GitHub issue" for external items.
- In the Settings type picker the option is labelled **External provider
  (HTTP)**. The `repos` field is labelled **Source repositories**, as for
  Linear and Jira.
- Item links use `url`. Short references use `Ref.Display()`, which shows
  `external_id` and never `#0`.

## Consequences

- A new tracker can be connected without a Hive PR by running a small HTTPS
  shim. Hive stays the only authority over identity, admission, and
  credentials.
- Validation rules fix the shape of external keys before they are persisted.
  Loosening them later is compatible. Tightening them would drop stored holds.
- Providers must run and secure a service. Hive does not supervise it. If the
  service is unreachable, the work source lists nothing, but GitHub PR
  maintenance continues.
- External providers cannot write back to the tracker until v2.

## Acceptance criteria for #10291

1. `config.ExternalSourceConfig` with `Validate()` covering every rule in
   *Configuration*, plus round-trip through `PUT/GET
   /api/config/governor/work-source`. The overlay on disk holds only `${VAR}`
   references.
2. `RegisterPrimary` / `PrimaryBuilder` / `PrimaryDeps` in
   `pkg/worksource/factory.go`. Built-ins are registered with no behavior
   change, and the existing `factory*_test.go` pass unmodified.
3. `externalSource` implementing `WorkSource` with the wire contract, limits,
   redirect policy, and validation rules above.
4. Table tests using `httptest.Server` for:
   - happy path and pagination;
   - every rejected-item rule;
   - duplicate key, which fails the whole call;
   - each limit;
   - non-2xx response and `contract` mismatch;
   - `source_type` mismatch on `/v1/source`;
   - wire `number`/`stage`/`source_type`, which reject the item;
   - redirect to a foreign host, where the token must not be sent;
   - env reference unset, where the error must name the variable but not the
     value.
5. An `eval_cycle_seams` test showing a failing external source fails closed
   and still lists PRs.
6. A key test showing every valid external item produces `repo!id`,
   round-trips through `ParseKey`, and has `IsRunStage() == false` and
   `IsGitHubIssue() == false`.
7. Docs: an *External provider* section in
   [Work source providers](../integrations/work-source-providers.md) with the
   wire contract and the stable-ID warning. Update the #10174 line in the
   integration guide's known gaps.

## Open questions

- Should an external provider also be allowed as an **additive** source
  through `RegisterAdditive`, alongside GitHub Issues? The identity rules
  already prevent key collisions. The open issue is how the dashboard shows
  two sources at once.
- Should Hive drop only the invalid items, as proposed, or fail the whole call
  when more than some fraction of items are invalid?
- Mutual TLS (`client_cert`/`client_key`, as for Jira Data Center) in v1, or
  later?
- How should `hive.worksource/v2` write-back express idempotency so a retried
  comment or transition is not applied twice?
