# Colony as the customer-data backend

Status: **Accepted; phases 1 to 4 built, P5 cleanup built; the hub default is
`colony`, the spoke default is `shadow` until parity is confirmed on the
project's own hive; `colony-only` not built yet.** P3, the spoke on the colony store, is described in
[Spoke implementation (P3)](#spoke-implementation-p3), and what P5 has done so
far in [P5 status](#p5-status). Tracking issue
[#19851](https://github.com/hivecommons/hive/issues/19851). Phase 1 (hub
Affiliation from colony) and the trust-domain enforcement are built in their
own PR, and every later phase ships behind its own issue. Related records:
[Customers](customers.md) for the spoke-side profile and graph,
[Customers hub consent](customers-hub-consent.md) for the consent-first import
and its 2026-11-19 decision change, [Customers enrichment](../customers-enrichment.md)
for the spoke providers, and [telemetry](../telemetry.md) for the outbound-path
review every flow here falls under.

Line references are to `v5` at `0c4722fe`. They are for orientation, and the
identifier next to each one is the stable reference.

## Summary of the decisions

1. **Colony grows an embedded store package** (`colony/store`, bbolt), which the
   hub and every spoke embed. Colony does not become a service.
2. **One schema** for person, identities, facts (each with source, confidence,
   rank and locality), consent and tombstones. The hub and spokes keep their
   own store file on their own PVC, and nothing is shared between them through
   the store.
3. **Precedence for every field, strongest first:** user-set > admin-set >
   hub-shared labelled > enrichment suggestion. A lower rank never overwrites a
   higher one. An explicit clear is a value at its rank.
4. **The hub and the project's own hives form one trust domain.** The hub
   enforces it with an allowlist of hive IDs. Every other spoke gets nothing
   from the hub about its customers, whatever its own switch says.
5. **Migration:** import, then shadow (dual-write with JSON authoritative),
   then colony-authoritative with JSON still written for rollback, then
   colony-only. Data stays on the PVC it lives on today.
6. **Providers:** hub gitdm is on by default because it sends nothing about
   anyone. Hub devstats is off by default. On the spoke, enrichment works as
   today, plus an auto-accept policy knob that defaults off. crowd.dev stays
   blocked on hivecommons/colony#5.
7. **Every setting is a config knob.** Each one is listed in
   [Settings](#settings).

## What exists today

| Piece | Where | Shape |
|---|---|---|
| colony v0.2.0 | hivecommons/colony | A Go library and a `colony whois` CLI. `resolver` (Provider, Query, Result; each Affiliation carries `Source`, `Confidence`, `MatchedBy`), `fetch` (guarded HTTP client, ETag cache, no redirects, generic User-Agent), `gitdm` (local matching over the public cncf/gitdm files), `devstats` (login-only POST, returns contribution activity, no affiliation). No storage, no service. crowd.dev is blocked on colony#5. |
| Spoke enrichment | `src/cmd/hive/customers_enrichment.go`, `src/pkg/customers/enrichment.go` | An optional sweep, off by default, that writes `/data/customers/enrichment.json`. A gitdm affiliation is a suggestion that the owner accepts or dismisses. |
| Spoke people | `src/pkg/customers/store.go`, `model.go` | `/data/customers/people.json`: people, identities, profile fields (`auto`/`manual`/`hub` with a Rule), tags, goals, satisfaction, suggestions, pins, hints, tombstones. |
| Spoke graph | `src/pkg/customers/edges.go` | bbolt `customers.db`: artefact edges keyed by person id. |
| Spoke hub import | `src/cmd/hive/customers_hub_import.go`, `src/pkg/customers/hub_import.go` | Posts hashed confirmed logins to the hub and stores labelled values as `hub`. |
| Hub CRM | `src/pkg/hub/saas.go:140-319` | One JSON file per user under `/data/saas/users`: FullName, SlackID, Notes, Company (each with a source), Country with CountrySource, and Affiliation/AffiliationSource derived by `top_repo.go`. |
| Hub match | `src/pkg/hub/customer_match.go`, `customer_match_public.go`, `customer_consent_store.go` | `POST /api/customers/match`. Consented real name, plus labelled public country and organization for grant holders. |

## Trust domain

This rule is binding (operator decision, 2026-11-25).

**The hub and the hive's own hive are one trust domain.** The hive's own hive
is the spoke or spokes the project itself runs. Everything the hub knows about
a person may flow to those hives' Customers records, always labelled with its
source. That includes country with its flag, affiliation, colony enrichment
and other derived facts.

**Every other spoke gets nothing from the hub about its customers.** Such a
spoke records its own interactions with its customers, and it can run its own
local enrichment (gitdm locally, devstats and crowd.dev with its owner's
switches). The hub never sends it the hub's knowledge.

### Enforcement

- **The hub enforces the domain by hive identity.**
  - `HIVE_HUB_CUSTOMERS_TRUSTED_HIVES` lists the trusted hive IDs. It is
    comma-separated, empty by default, and read per request.
  - The per-hive heartbeat bearer proves which hive is asking. It is already
    bound to the claimed `hive_id` in `handleCustomerMatch`.
  - A hive that is not on the list gets `{"matches":[]}` before any hash is
    looked at. That is the same answer as for a hive whose customers are all
    unknown, so the answer does not reveal whether the hub knows anyone.
  - The request is audited with `trusted=false`.
- **A spoke-side switch is never enough.** A third-party spoke that turns on
  `customers.hub_import.enabled` gets nothing:
  - its labels header is ignored;
  - a consent record a person made for that hive is ignored;
  - its consent record is never stamped as delivered.
- **Fail closed.** With the list empty, no hive receives anything. An invalid
  entry is dropped, so a typo can only narrow the domain.
- **Tests:** `src/pkg/hub/customer_trust_test.go` covers both cases:
  - the own hive receives labelled values;
  - a third-party hive with its switch on, the labels header and a recorded
    consent receives nothing;
  - an empty list trusts no hive.

### The gap this closed

Before the fix, `POST /api/customers/match` answered **any** hive with a valid
bearer that turned on its import:

- labelled public country and organization for that hive's grant holders,
  without a consent record (#19306);
- the consented real name.

The only gates were the spoke switch and the hub-wide
`HIVE_HUB_CUSTOMERS_SHARE_ENABLED`. That broke the rule above. Issue
[#19854](https://github.com/hivecommons/hive/issues/19854) records the gap, and
the P1 PR closes it.

One follow-up is open. The hub's Profile sharing page still lists every hive
where a person holds a grant, and a consent recorded for a hive outside the
domain now has no effect. The page should list only trusted hives.

Two other hub-to-spoke paths were checked:

- The heartbeat `authorized_users` list carries logins for access control,
  not customer data.
- The hub My Hives access rows (FullName, SlackID) are a hub page shown to a
  hive's co-owners, not a spoke Customers record.

Neither is a customer-data flow to a spoke. Both are noted here so a later
change does not turn either into one.

### Within the domain

Inside the domain the hub may send:

- every fact the colony schema marks `shareable`, each labelled with its rank
  and source;
- affiliation (P4);
- colony enrichment.

It never sends `local_only` facts (see [Privacy](#privacy)), and the real name
and location stay consent-first even inside the domain.

## Architecture

### Options

**(a) An embedded store package in the colony repo.** `colony/store` is a Go
package over bbolt. The hub and each spoke open their own `colony.db` on their
own PVC.

**(b) Colony as a separate service.** A colony server with an API and its own
database, called by the hub and spokes.

### Recommendation: (a)

- **Hive already runs embedded stores.** The spoke graph is bbolt
  (`customers.db`) and people are a JSON file on the PVC. A third embedded
  file is a pattern operators already back up, restore and reason about. A
  service adds a deployment, a database, credentials, network policy, an
  upgrade path and an availability dependency for every spoke.
- **Data locality is the privacy rule.**
  - Private-domain email data must never leave the place it was collected.
  - An embedded store keeps each deployment's facts on its own disk by
    construction.
  - A shared service would be a new place every fact is sent to. That is
    exactly the "send customer data to a hub" proposal that
    [customers.md](customers.md) section 12 reserves for its own review, and
    it would cut across the trust domain above.
- **Push-reported spokes cannot be reached.** Most spokes sit on clusters the
  hub cannot write to. A service would need every spoke to call out to it,
  which is a new outbound path on every hive.
- **One schema, many files.** The value of colony is the shared schema and
  precedence rules, plus the providers writing into them with source and
  confidence intact. A library gives that without shared state.

The cost of (a) is that a schema change ships as a colony release and a hive
bump. That is already how the providers ship. bbolt is single-writer, so the
hub must stay one replica on its PVC, as it already must for the per-user
JSON files.

### Schema (colony/store, hivecommons/colony#6)

All records carry `schema_version` in a `meta` bucket.

| Record | Fields | Notes |
|---|---|---|
| `Person` | `id`, `status`, `created_at`, `updated_at` | The caller may supply the id, so the spoke person ids in `people.json` and the edge index survive migration unchanged. |
| `Identity` | `kind` (`github`, `ghe`, `email`, `git_email`, `slack`, `oidc`), `value`, `verified`, `locality`, `source`, `at` | Every email on a domain that is not free-mail is `local_only`. |
| `Fact` | `field` (`real_name`, `organization`, `affiliation`, `country`, `location`, `notes`, `slack_id`, ...), `value` (empty means an explicit clear), `rank` (`user`, `admin`, `hub_shared`, `enrichment`), `source` (for example `colony:gitdm`, `github_profile`, `hub:person`, `browser_language`), `confidence` (`high`/`medium`/`low` or empty), `locality`, `from`, `until`, `at`, `by`, `suggested` | One record per field per rank. The effective value is the highest rank present. |
| `Consent` | `scope` (a hive id), `fields`, `text_version`, `granted_at`, `withdrawn_at`, `delivered` | Replaces `customer_consent_store.go` on the hub (P4). |
| `Tombstone` | Salted hash of a forgotten identity | It is never re-added. |

`store.SetFact` enforces precedence:

- a lower rank never overwrites a higher one;
- at the same rank, the newer value wins.

`store.Export(Shareable())` drops every `local_only` identity, and every fact
whose source is `local_only` or was derived from a `local_only` identity. It is
the only export a hub-bound or spoke-bound path may call.

## Mapping

### Precedence

| Rank | Meaning on the hub | Meaning on a spoke |
|---|---|---|
| `user` | The person set it on the hub (`*Source == "user"`, `CountrySetByUser`) | A hive user typed it (`SourceManual`) |
| `admin` | A hub admin set it (`*Source == "admin"`) | Not used. A spoke owner editing a profile counts as `user` on that spoke. |
| `hub_shared` | Not used | Came from the trusted hub, labelled (`SourceHub` with its label) |
| `enrichment` | Derived: browser-language country, GitHub footprint, colony providers | Derived: forge profile fill (`SourceAuto` with a profile Rule), colony providers. A provider value is `suggested` until accepted. |

### Hub SaaSUser (P4)

| Today | Colony |
|---|---|
| `FullName` + `FullNameSource` | Fact `real_name` at rank `user`/`admin`; shareable only with consent |
| `Company` + `CompanySource` | Fact `organization` at rank `user`/`admin`; an empty value with a source is an explicit clear |
| `Country` + `CountrySource` / `CountrySetByUser` | Fact `country`: `user`/`admin`, or `enrichment` with source `browser_language` |
| `Affiliation` + `AffiliationSource` (+ `AffiliationConfidence` from P1) | Fact `affiliation` at rank `enrichment`, with source `github:company_field`, `github:contributions`, `colony:gitdm` and so on, and confidence |
| `SlackID`, `Notes` | Facts at rank `admin`, `local_only` |
| `Email` | Identity `email`, `local_only` unless it is on a free-mail domain |
| `CanonicalID`, `LinkedGitHubLogin`, `GitHubUsername` | Identities `oidc` / `github` |
| `customer_consent_store.go` records | `Consent` |
| Hives, expiry, quota, tokens, engagement stats | **Stay in the SaaSUser JSON.** This is account and access data, not customer data. |

### Spoke people.json and edges (P3)

| Today | Colony |
|---|---|
| `Person.ID`, `Status`, `Origin` | `Person` with the same id |
| `Identity` (`Kind`, `Value`, `Source`, `Confidence` float, `Manual`, `Claimed`) | `Identity`: `verified = !Claimed`. Confidence is bucketed: at least 0.9 is high, at least 0.6 is medium, otherwise low. |
| `Profile.RealName/Organization/Location/Country` (`manual`/`hub`/`auto` + Rule) | `Fact` at rank `user` / `hub_shared` (source `hub:<label>`) / `enrichment` (source = Rule) |
| `enrichment.json` gitdm affiliations, devstats activity | `Fact` at rank `enrichment`, `suggested`, with source `colony:gitdm` / `colony:devstats` and colony's confidence |
| `Profile.DismissedAffiliations` | Dismissed suggestions (a dismissed fact is kept so it is not offered again) |
| Tombstones | `Tombstone` |
| Tags, goals, satisfaction, suggestions, pins, hints, merge records | **Stay in hive,** keyed by person id. These are hive CRM features, not identity or provenance. |
| bbolt `customers.db` edges | **Stays in hive, unchanged.** Edges reference person ids, which migration keeps. |

## Migration

The same steps apply on the spoke (P3, `customers.store` /
`HIVE_CUSTOMERS_STORE`) and on the hub (P4, `HIVE_HUB_CUSTOMER_STORE`).

1. **`json`.** The behaviour before the colony store, and the rollback. No
   `colony.db` is created. It was the default until
   [#19972](https://github.com/hivecommons/hive/issues/19972); a value that is
   set but unknown still means `json`.
2. **`shadow`.**
   - On first boot in this mode, read the JSON and write every person into
     `colony.db` in one bbolt transaction. Record the source file's hash and
     the import time in `meta`.
   - Every write goes to both stores, and the JSON stays authoritative for
     reads.
   - A parity check after each sweep logs counts only, never values (people,
     facts per rank, mismatches).
3. **`colony` (the hub default since #19972; the spoke default once parity is
   confirmed on the project's own hive).** Reads come from colony, and the
   JSON is still written, so switching back to `json` loses nothing. A
   deployment upgraded from the `json` default runs step 2's import on its
   first boot, with the same backups (`*.pre-colony` on the spoke,
   `colony.db.bak-*` on the hub, whose JSON files the import never
   rewrites), and falls back to `json` when the import fails.
4. **`colony-only` (P5).**
   - JSON writes stop, and the JSON file is renamed `*.migrated` and kept for a
     documented retention.
   - `colony export --format hive-people-json` (and the hub equivalent) writes
     the old shape for a manual rollback.

**Rollback:**

- From `shadow` or `colony`, set the mode back to `json`.
- From `colony-only`, export, put the file back and set `json`.
- A schema-version mismatch on open (an older binary reading a newer file)
  fails closed to `json` mode and logs it. It never rewrites the file.

**PVCs:** `colony.db` sits next to the file it replaces:

- spoke: `/data/customers/colony.db`
- hub: `/data/saas/colony.db`

hive-backup includes it. No PVC changes size class. The store holds a few
kilobytes per person.

**Forget:** forget erases the person from both stores in every mode and writes
the tombstone to both.

### Spoke implementation (P3)

Built in [#19855](https://github.com/hivecommons/hive/issues/19855) on colony
v0.3.0 (`src/pkg/customers/colony.go`). What it does beyond the steps above:

- **The import** runs through colony's `Migrate` under the name
  `hive-people-json`, in one transaction, with the hashes of `people.json` and
  `enrichment.json` as its fingerprint. The boot checks that colony wrote as
  many people as `people.json` holds and fails closed to `json` otherwise.
  Before the first import, `people.json.pre-colony` and
  `enrichment.json.pre-colony` keep the files as they stood; a later import
  never replaces them.
- **Idempotence.** `colony-sync.json` records the hashes of the JSON files
  `colony.db` was last brought in line with. A boot whose files match does
  nothing. A boot whose files differ (a `json`-mode run in between, or a
  colony write that failed) imports again from the JSON, which is never older
  than `colony.db`: the JSON is always written first.
- **`colony` mode** reads at boot: for each person, what `colony.db` holds
  replaces the JSON copy of the identities colony can hold, the profile
  fields, the dismissed affiliations and the status, and the result is
  written back to `people.json`. Enrichment is read from `colony.db` on every
  request; the display detail only hive keeps (project name, period, link,
  history) still comes from `enrichment.json`.
- **What stays in hive.** Identity kinds colony has no kind for (Discord,
  Teams, Meet and Zoom handles, display names) stay in `people.json`, as do
  the tags, goals, satisfaction, suggestions, pins, hints, merge records and
  the edge index. A dashboard user (`hive_user`) is colony's `oidc` kind, always
  `local_only`. Tombstones written before the import stay in `people.json`
  and keep being enforced there: they are salted hashes under hive's own salt
  and cannot be turned into colony tombstones without the identity. Every
  forget after the import writes both.
- **Locality.** Values a hive user typed, values from the hub and anything not
  derived from the person's public GitHub login are `local_only`. Email
  identities on a domain that is not free-mail are `local_only` (colony
  enforces it). No spoke path exports from `colony.db`.
- **A merge** empties the merged person's colony record and marks it
  `removed`, so the identities can move and an unmerge brings the same id
  back. A forget erases the record.
- **Forget** in shadow and colony mode goes through the mirror; in `json` mode
  it opens a `colony.db` an earlier run left behind, only to forget. Either
  way the file is compacted afterwards, because bbolt keeps freed pages, and
  the person is removed from both `.pre-colony` backups (their tombstones are
  added there).
- **Parity** runs at boot and after each discovery sweep and logs counts only:
  people and identities on each side, facts per rank, people that differ,
  and failed colony writes.
- **Backups.** `colony.db` sits in `/data/customers` with `people.json`. The
  downloadable spoke backup carries neither; a volume-level backup of `/data`
  covers both (see [backup and restore](../backup-restore.md)).

### P5 status

[#19857](https://github.com/hivecommons/hive/issues/19857). Built:

- **Hub consent reads come from colony in `colony` mode.** The match index,
  the match answer and the person's own Profile sharing page read the grant
  (fields, disclosure version, withdrawal) from colony as well as from the
  consent file, and colony can only narrow what the file grants, never widen
  it: a field crosses only when both grant it, a withdrawal on either side
  holds, and an outdated disclosure version on either side stops delivery.
  Writes go to the file first, so a colony copy that fell behind can only
  send less. The
  delivery bookkeeping (first and last delivery, the delivered field sets and
  the rev) has no slot in colony's `Consent`, so it is still read from
  `/data/saas/consents`. Only hive ids the consent file lists are read, because
  colony has no delete and keeps a pruned consent as an emptied record. A
  person whose last sync failed is read from the file alone. Consent writes,
  the delivery stamp and the retention sweep still read and write the file
  first and mirror it, so `json` stays the rollback. In `json` and `shadow`
  mode nothing changes.
- **Settings.** `customers.store` (Customer record store) and
  `customers.enrichment.auto_accept` (Auto-accept gitdm affiliations) have
  Settings controls under Features, Customers. Settings refuses an unknown
  value, and shows when an environment override decides.
- Spoke gitdm defaults on when enrichment is enabled, independently of the store flip.
- **The default flip**
  ([#19972](https://github.com/hivecommons/hive/issues/19972)). The store
  mode defaults to `colony` on the hub (`HIVE_HUB_CUSTOMER_STORE` unset) and
  to `shadow` on the spoke (`customers.store` and `HIVE_CUSTOMERS_STORE`
  unset): JSON stays authoritative and colony is written alongside it. The
  spoke default moves to `colony` once parity is confirmed on the project's
  own hive. Every explicit value, `json` included, keeps working, and a set
  but unknown value still means `json`. The hub default rests on its `shadow` boot sync
  (`json_users=280`, `synced=280`, `failed=0`, `skipped=0`,
  `orphans_held=0`, `colony_people=280`, `parity=true`); the project's own
  hive's `shadow` parity is collected by the operator, and gates the spoke move to `colony`. Upgrading from the
  `json` default goes through the boot import and backups above; the tests
  that hold it are `TestColonyUpgradeFromJSONDefaultStraightToColony` and
  `TestColonyDamagedFileFailsClosed` (spoke store),
  `TestWireCustomersDefaultUpgradeFromJSON` and
  `TestWireCustomersDefaultFallsBackToJSON` (spoke wiring), and
  `TestHubCustomerStoreDefaultUpgradeFromJSON` and
  `TestHubCustomerStoreUnreadableFileFailsClosed` (hub).

Not yet, tracked in
[#19972](https://github.com/hivecommons/hive/issues/19972):

- `colony-only`, the `*.migrated` rename and `colony export`. colony v0.3.0
  has no slot for part of what the JSON holds, so stopping the JSON writes
  as designed would lose data:
  - spoke: the identity metadata hive keeps (`manual`, `by`, the numeric
    confidence), identities colony refuses (one owner per identity), and
    the enrichment display detail (project name, period, link, history,
    when each source answered) live only in `people.json` and
    `enrichment.json`;
  - hub: the delivery bookkeeping lives only in `/data/saas/consents`, and a
    value colony refuses (a long note) is JSON-only.
  Either colony grows those slots, or `colony-only` keeps a hive-local file
  for what colony cannot hold. The duplicated enrichment types in
  `src/pkg/customers/enrichment.go` and the hub's top-repo affiliation cache
  fields stay until then, because `json`, `shadow` and `colony` mode all
  still read them.

## Providers

### Hub (P1, built)

The daily top-repo refresh (`src/pkg/hub/top_repo.go`, `refreshUserTopRepo`)
asks colony after the GitHub footprint (`src/pkg/hub/affiliation_colony.go`).

**gitdm:**

- The hub matches the user's **public github.com login only**, against the
  public files.
- It never matches an email, so nothing derived from an email address,
  private-domain or otherwise, enters the hub Affiliation.
- It never matches a GHE login, because gitdm lists github.com logins.

**devstats:**

- It sends the same login and nothing else.
- It is capped per hour.
- It adds the CNCF all-time contribution count to the admin tooltip, and an
  affiliation if a later colony release returns one.

**Precedence for the Affiliation shown, strongest first:**

1. hub Company set by the user;
2. hub Company set by an admin;
3. the company on the person's own GitHub/GHE profile, which is the person's
   own public statement;
4. `colony:gitdm`;
5. `colony:devstats`;
6. the footprint heuristics (contributed-to org, org membership, top repo
   owner) and the GHE/IdP fallbacks.

1 and 2 are applied when the record is read, so a refresh never overwrites
them. A colony value records `affiliation_source` and
`affiliation_confidence`.

[telemetry.md](../telemetry.md) (section "Hub affiliation from colony", added with P1) lists both hub
paths.

### Spoke (P3)

Enrichment works as today: gitdm matches a confirmed login and confirmed
emails locally, and devstats sends a confirmed login only. P3 adds
`customers.enrichment.auto_accept`:

- **`off` (the default).** Every provider value is a suggestion the owner
  accepts or dismisses.
- **`gitdm_high`.** A gitdm affiliation matched by login (confidence `high`),
  current today, is accepted automatically when the organization field is empty
  or holds an `enrichment` value. It never overwrites `user` or `hub_shared`.
  It is labelled as auto-accepted and stays dismissible.

An email-only gitdm match (`medium`) is never auto-accepted.

### crowd.dev (P6)

It stays blocked on hivecommons/colony#5. Issue
[#19858](https://github.com/hivecommons/hive/issues/19858) is the follow-up
that #19303 promised.

## Defaults

| Setting | Default | Why |
|---|---|---|
| Hub gitdm | **on** | No data about anyone leaves the hub. The download is the same public files for everyone, with ETags. This meets the Linux Foundation telemetry policy as a public download, not collection. |
| Hub devstats | **off** | It is a per-person request to a third party, so it is a new outbound path. The Linux Foundation telemetry policy asks for review and consent before such a path is on by default. In colony v0.2.0 devstats returns activity, not affiliation, and DevStats' own company data comes from gitdm, so turning it on adds no affiliation the hub does not already have. |
| Hub trust allowlist | **empty** | Fail closed. The operator lists the project's own hives. |
| Spoke gitdm | **on when enrichment is enabled** | Sends nothing about anyone; enrichment itself remains off by default. |
| Spoke devstats, crowd.dev | off | Same reasoning as the hub. crowd.dev is inert until colony#5. |
| Spoke auto-accept | `off` | Accepting is a decision an owner makes until the owner chooses a policy. |
| Store mode | hub `colony`, spoke `shadow` | Since #19972: the hub after its clean `shadow` boot sync, the spoke stays on `shadow` until parity is confirmed on the project's own hive, then moves to `colony`. `json` stays valid and is the rollback; a set but unknown value means `json`. `colony-only` is not built yet. |

## Privacy

The binding data-sharing rule:

- Hub country and affiliation may reach spokes, labelled, and derived values
  are fine.
- Public forge data is fine.
- Data from private-domain emails never leaves the place it was collected.
- Name and location are consent-first.

The trust domain narrows "spokes" to the project's own hives.

| Flow | Data | Direction | Rule | Status |
|---|---|---|---|---|
| Hub gitdm snapshot | Public cncf/gitdm files; nothing about a person | GitHub to hub | Public data | P1. On by default. |
| Hub gitdm match | The user's public github.com login, matched on the hub | Local | Public forge identity. Emails are never matched, so nothing derived from a private-domain email is produced. | P1 |
| Hub devstats | Public github.com login | Hub to CNCF DevStats | Public forge identity | P1. Off by default, hourly cap. |
| Hub Affiliation display | Derived affiliation with source and confidence | Hub admin UI only | Derived, labelled | P1. Not sent to any spoke. |
| Hub to own hive: country, organization | Labelled public values | Hub to trusted hive | Allowed: hub country and affiliation, labelled; derived OK | Existing, now limited to the trust domain |
| Hub to own hive: affiliation, colony enrichment | Labelled derived values from public forge data | Hub to trusted hive | Allowed inside the trust domain | P4 |
| Hub to own hive: real name | Person-entered value | Hub to trusted hive | Consent-first | Existing. Consent is still required inside the domain. |
| Hub to own hive: location | None | n/a | Consent-first. Not sent in v1. | Never sent |
| Hub to any other spoke | Nothing | n/a | Trust domain | Enforced by `HIVE_HUB_CUSTOMERS_TRUSTED_HIVES` |
| Hub to any spoke: email, email domain, Slack id, notes | Nothing | n/a | Private-domain email data and admin notes stay on the hub | `local_only` in colony; never in `Shareable()` |
| Hub org-from-email-domain | An organization name for an operator-listed domain | Hub to trusted hive, labelled `email_domain` | Operator-listed organization domains only. The email and domain never cross. | Existing. Limited to the trust domain. |
| Spoke to hub match request | Salted hashes of confirmed logins | Spoke to hub | No values sent | Existing. Unchanged. |
| Spoke gitdm match | Confirmed login and confirmed emails, matched on the spoke | Local | Private-domain email stays on the spoke | Existing |
| Spoke devstats, crowd.dev | Confirmed public GitHub login | Spoke to CNCF / LFX | Public forge identity | Existing, off. crowd.dev blocked. |
| Spoke to hub: any customer fact | Nothing | n/a | customers.md section 12: any such proposal is its own RFC | **Not proposed here** |
| colony.db on disk | Everything the deployment already holds | Local | No new egress. Locality is stored per fact. | P2 to P4 |

## Using customer data to shape a hive's repos

The point of knowing customers is to build the right thing for them. Inside
the privacy rules above, the Customers data a hive already holds can shape the
repos it manages. A separate review is auditing whether today's collection
works. This section is about what to do with it once it does.

### What signal is available, on the spoke, today

- Who opened, commented on, reacted to or is mentioned in each issue and PR:
  the edge index, with predicates `reported`, `commented_on`, `mentioned_in`
  and so on.
- Which of those people are **confirmed customers** and which are suggested.
- Their affiliation (accepted, hub-shared or suggested, each with its source),
  goals, satisfaction level, and tags such as `adopter`.

### What to build with it (proposed follow-ups, each its own issue after this design is approved)

1. **Adopter-weighted issue priority.**
   - Each open issue gets a score from the confirmed customers linked to it:
     count, distinct organizations, satisfaction (an unhappy customer weighs
     more), and whether the issue matches one of their goals.
   - The planner and the scanner read it as one input next to labels and age.
     It is never the only one.
   - The score and its evidence (person ids and organization names, never
     emails) are shown on the issue in the dashboard, not posted to the forge.
2. **"Which issues matter to adopters" view.**
   - A Customers tab section listing the issues with the most distinct adopter
     organizations.
   - Filterable by tag (for example `adopter`) and by goal.
   - Read-only, through the existing GraphQL endpoint.
3. **Roadmap input.**
   - A periodic digest clusters customer goals and the issues linked to them
     into themes, with counts per theme and per organization.
   - The owner reads it when setting milestones.
   - Hive proposes, a person decides, which is the same customer-versus-
     suggested line as everywhere else.
4. **Satisfaction as a regression signal.**
   - A satisfaction drop for a customer linked to a recent merge raises an
     Inbox item pointing at that merge.

### Rules that keep it inside the privacy line

- **Spoke-local.**
  - The scoring runs on the spoke, over the spoke's own data.
  - No score, organization list or person reference leaves the hive.
  - The hub does not aggregate adopter signals across spokes. That would
    turn the hub into the very cross-spoke customer database the trust domain
    rules out.
- **Only confirmed customers count.** Suggested people never weigh an issue.
  The line between a customer and a third party is a human decision.
- **Agents see organizations and counts, not people.** The kick-context block
  ([customers.md](customers.md) section 12) can carry "this issue matters to 3
  adopter organizations". It never carries names, emails or notes. An
  organization derived from a `local_only` identity is shown as "1 adopter"
  with no name.
- **Nothing on the forge.**
  - Priority evidence is never written into an issue comment or label on a
    public repository.
  - A label such as `adopter-priority` may be applied only when it names no
    one.
  - Applying it is an owner switch, off by default.
- **The trust domain applies.**
  - The project's own hive may use hub-shared affiliation as an input, because
    it is in the domain.
  - Any other hive uses only what it recorded and enriched itself.

## Settings

| Key / env | Where | Default | Phase |
|---|---|---|---|
| `HIVE_HUB_AFFILIATION_COLONY_GITDM` | hub | `true` | P1 |
| `HIVE_HUB_AFFILIATION_COLONY_GITDM_REFRESH` | hub | `24h` (at least `1h`) | P1 |
| `HIVE_HUB_AFFILIATION_COLONY_DEVSTATS` | hub | `false` | P1 |
| `HIVE_HUB_AFFILIATION_COLONY_DEVSTATS_HOURLY_LIMIT` | hub | `30` (at most `120`) | P1 |
| `HIVE_HUB_AFFILIATION_COLONY_TIMEOUT` | hub | `2m` (at most `10m`) | P1 |
| `HIVE_HUB_CUSTOMERS_TRUSTED_HIVES` | hub | empty | P1 PR (#19854) |
| `HIVE_HUB_CUSTOMER_STORE` | hub | `colony` (was `json` until #19972) | P4, default P5 |
| `customers.store` / `HIVE_CUSTOMERS_STORE` | spoke | `shadow` (was `json` until #19972; moves to `colony` once parity is confirmed on the project's own hive) | P3, default P5 |
| `customers.enrichment.auto_accept` / `HIVE_CUSTOMERS_ENRICHMENT_AUTO_ACCEPT` | spoke | `off` | P3 |
| Existing `customers.enrichment.*` | spoke | unchanged until P5 | P5 |

## Phases

| Phase | Issue | Depends on | One PR |
|---|---|---|---|
| P1 hub Affiliation via colony gitdm and devstats, plus the trust-domain allowlist | [#19853](https://github.com/hivecommons/hive/issues/19853), [#19854](https://github.com/hivecommons/hive/issues/19854) | nothing | built |
| P2 colony store package | [hivecommons/colony#6](https://github.com/hivecommons/colony/issues/6) | nothing | built (colony v0.3.0) |
| P3 spoke Customers on the colony store, migration, auto-accept | [#19855](https://github.com/hivecommons/hive/issues/19855) | P2 released | built |
| P4 hub CRM on the colony store, migration, labelled affiliation for trusted hives | [#19856](https://github.com/hivecommons/hive/issues/19856) | P2 released | built |
| P5 default-on and cleanup | [#19857](https://github.com/hivecommons/hive/issues/19857), default flip [#19972](https://github.com/hivecommons/hive/issues/19972) | P3, P4 | cleanup built; hub default `colony`, spoke default `shadow` until parity is confirmed on the project's own hive; `colony-only` and `colony export` open in #19972 |
| P6 crowd.dev | [#19858](https://github.com/hivecommons/hive/issues/19858) | colony#5 | hive |

The repo-shaping follow-ups above are filed only after this design is
approved.

## Open questions

1. Should the hub's Profile sharing page hide hives outside the trust domain,
   or show them as "this hive does not receive hub data"? The recommendation
   is to hide them.
2. Should the allowlist move from an env var to the hub's operator settings
   page, with an audit line per change? An env var is enough for P1.
3. Should a confirmed `colony:gitdm` affiliation on the hub feed the labelled
   `organization` in the match answer when the person set none (P4), or only
   a separate `affiliation` field? The recommendation is a separate field, so
   a hive can tell a statement from a derivation.
