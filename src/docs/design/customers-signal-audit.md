# Customers: signal audit and using customer data to shape managed repos

**Status: design only.** Issue
[#19866](https://github.com/hivecommons/hive/issues/19866). Part A is an audit
of what the Customers feature collects today, on the hub and on a spoke, and
whether each signal works. Part B proposes privacy-safe ways to use that data
when Hive decides what to work on in the repos it manages. **Nothing in Part B
is implemented.** Read [customers.md](customers.md) and
[customers-hub-consent.md](customers-hub-consent.md) first for the model this
page builds on.

All numbers are counts from the project hive on 2026-11-25. No personal data
appears on this page, and none may be added to it.

## Trust rule

Decided by the operator on 2026-11-25:

- The hub and the project's own hive are **one trust domain**. The project
  hive may use what the hub knows about a person.
- **Other spokes get nothing from the hub** for this purpose. They use only
  the interactions they collected themselves.

The hub-side gate for this landed in
[#19860](https://github.com/hivecommons/hive/pull/19860) (fixes #19854).
Widening it to every hub person and sending hub affiliation is a follow-up
recorded in the review on #19860.

## Part A: signal audit

State of the project hive on 2026-11-25: 20 confirmed customers, 4
suggestions, 149 interaction edges.

Status key: **working** (collected and populated), **off** (built, gated off
by default or on this hive), **broken** (built, but no source feeds it),
**dead** (declared or configurable, never written), **missing** (not built).

### Forge interactions

| Signal | Collected at | Gate (default) | Populated | Used downstream | Status |
|---|---|---|---|---|---|
| Issue reported, assigned, mentioned | `customersGitHubSightings`, `src/cmd/hive/customers_sweep.go:280-286` | `customers.enabled` (on) | yes | interaction count in agent kicks | working |
| PR authored, review requested, maintainer association | `src/cmd/hive/customers_sweep.go:300-305` | on | yes | kick tags and count | working |
| Issue claims | `src/cmd/hive/customers_sweep.go:312` | on | yes | kick count | working |
| Reviews and merges | `PredReviewed`, `PredMerged` declared at `src/pkg/customers/edges.go:20-21`; counted by the tag rules at `src/pkg/customers/ingest.go:229`; never written | n/a | 0 | tag rules always see 0 | dead |
| Comments | `PredCommentedOn` written only by mention search, `src/pkg/customers/mentionsearch.go:231` | `mention_search` (off) | 0 | none | dead by default |
| Reactions, upvotes, stars, forks | not collected | n/a | n/a | n/a | missing |

### Local interactions

| Signal | Collected at | Gate (default) | Populated | Used downstream | Status |
|---|---|---|---|---|---|
| Mail sender, calendar, meetings, uploads | `customersLocalSightings`, `src/cmd/hive/customers_sweep.go:364` | on | yes | stakeholder tag | working |
| Dashboard audit log (acted, logged in) | `customersAuditSightings`, `src/cmd/hive/customers_sweep.go:505` | on | yes | none | working |
| Slack and Discord chat | identity kinds only, `src/pkg/customers/model.go:45-46`; `PredSpokeIn` and `PredChattedIn` at `src/pkg/customers/edges.go:32-33` are never written | n/a | 0 | none | dead |

### Derived and enriched fields

| Signal | Computed at | Gate (default) | Populated | Used downstream | Status |
|---|---|---|---|---|---|
| Auto tags (maintainer, contributor, end-user, stakeholder) | `autoTags`, `src/pkg/customers/ingest.go:235` | on | 25 active tags | kick context, meetings persona | working |
| `hive_user` origin | `src/pkg/customers/ingest.go` sweep | on | 3 | none | working |
| Same-person hints | `DeriveHints`, `src/pkg/customers/hints.go:79` | on | 0 | none | working (nothing to suggest) |
| Mention search | `src/pkg/customers/mentionsearch.go:157` | off | 0 | none | off |
| Forge profile fill (real name, org, location) | `src/pkg/customers/profilefill.go:331` | `profile_fill` (off) | 0 of 20 | none | off on the project hive |
| Country, from location or hub | `DeriveCountry`, `src/pkg/customers/country.go:187`; `deriveAutoCountry`, `src/pkg/customers/store.go:576` | follows its source | 0 of 20 | flag pill and detail line | broken (no source on) |
| Hub import (real name, org, country) | `src/pkg/customers/hub_import.go:439` | `hub_import` (off) | 0; turned on 2026-11-25 | none | was off, see below |
| Enrichment: gitdm, devstats | `runDevstatsStep`, `src/cmd/hive/customers_enrichment.go:282`; `runCustomersEnrichmentPass`, `src/cmd/hive/customers_enrichment.go:363` | `enrichment` (off); on for the project hive since 2026-11-25 | 20 of 20 fetched; 8 with affiliations; 19 with activity | none | working |
| Enrichment: crowd.dev | config and token UI only | off | 0 | none | dead (no provider) |
| Affiliation suggestion | `SuggestAffiliationWith`, `src/pkg/customers/affiliation.go:140`, computed at read | on | from enrichment | none | working |
| Goals and satisfaction inference | `src/pkg/customers/infer.go:267` | `inference` (off) | 0 goals; 1 satisfaction set by hand | confirmed goals go into kicks | off |
| Manual edits (org, notes, tags, goals) | `src/pkg/customers/service.go:294` | on | 1 organization | tags and goals into kicks; notes never | working |
| ADOPTERS files | not parsed by Customers | n/a | n/a | outreach prompt text only | missing |

### Hub side

| Signal | Held in | Gate (default) | Populated | Used downstream | Status |
|---|---|---|---|---|---|
| Hub user country, affiliation, company | `src/pkg/hub/saas.go`, `src/pkg/hub/top_repo.go` | hub | 208 of 278 with a country; 202 with an affiliation | match endpoint (#19860) | working on the hub |
| Hub consents | `src/pkg/hub/customer_consent_store.go:70` | `HIVE_HUB_CUSTOMERS_SHARE_ENABLED` (on) | 0 records | match endpoint | unused |

### Why no country or flag shows on the project hive

Three causes, from the outermost in:

1. `customers.hub_import` was off, so no match request was ever sent. It was
   turned on 2026-11-25.
2. With the import on, the hub answered public fields only for people holding
   a grant on the asking hive. 15 of the 20 customers are hub users and the hub
   knows a country for all 15, but only 3 hold a grant. #19860 does not change
   this; the follow-up noted on #19860 does.
3. The forge profile fill, the other source of a country, is off on the
   project hive.

The UI already renders the flag. It has no data.

## Part B: using customer data to shape the repos

### What exists today

Customer data plays **no part in ranking**. Nothing in governor or selectTask
scoring, triage, issue ranking, the roadmap or advisory reports reads it. The
only uses are informational prompt text:

- `addCustomerContext`, `src/pkg/scheduler/kick_customer_context.go:35`, adds a
  capped "Customer context" block (tags, confirmed goals, interaction count) to
  agent kicks.
- The meetings persona gets people briefs.

### Proposals for the project hive (full hub knowledge, labeled by source)

1. **Adopter-weighted issue ranking.** A bounded boost in selectTask and triage
   scoring for issues filed, commented on or upvoted by confirmed customers
   tagged end-user or stakeholder, or by people whose hub affiliation maps to an
   org in an ADOPTERS file. The weight is a config knob, default off, capped so
   it can never outrank security or regression work. The scorer sees only a
   boolean or a tier per issue, never an identity.
2. **Regression-hits-adopter flag.** When a bug or regression is reported by,
   or mentions, a customer whose affiliation is a known adopter org, add a
   generic `customer-impact` label and raise the priority tier. The advisory
   report shows "regressions affecting adopters: N".
3. **Per-org open-asks view.** In the Customers section, group open issues and
   PRs by organization (from hub affiliation or enrichment, labeled by source):
   count of open asks, oldest age, last response time. It shows which adopters
   are waiting on what, stays inside the hive, and is owner-only.
4. **Advisory report steering.** An aggregate section in the advisory report:
   adopter-reported open issues, median time to first response for customers
   versus everyone else, and countries and orgs as counts only. Optionally, the
   advisor's top-N recommendations lean toward items with customer impact.
5. **ADOPTERS cross-check.** Parse ADOPTERS files in managed repos. Flag
   customers whose org is not listed (an outreach candidate) and listed orgs with
   no active customer (a churn signal). Counts only in any public output.

### Proposals for other spokes (local data only, nothing from the hub)

1. Weight issue ranking by local signals only: an issue whose author or
   commenters are confirmed customers, or carry the end-user or stakeholder tag,
   gets a bounded boost.
2. A per-org view using only locally held organization values: manual entries,
   the spoke's own forge profile fill, and the spoke's own enrichment.
3. A local "customer waiting" age in triage: time since a confirmed customer's
   last unanswered comment.
4. The same advisory aggregates as above, from local data only.

### Prerequisites and gaps to fix first

Each of these is its own PR-sized follow-up, in this order:

1. Write the `reviewed`, `merged` and `commented_on` edges from the existing
   poll, so the tag rules stop counting zero and "commented on" no longer
   depends on mention search.
2. Collect reactions and upvotes on issues, so "upvoted by" can be computed.
3. Decide whether ADOPTERS files are parsed into Customers (proposal 5 and the
   adopter side of proposals 1 and 2 depend on it).
4. Remove or implement the dead pieces: the crowd.dev provider and the Slack and
   Discord chat edges.
5. Turn on `profile_fill` on the project hive if a forge-derived country is
   wanted.
6. The hub-side follow-up from #19860: answer match requests for every hub
   person to the project hive and include hub affiliation.

### Privacy constraints for every proposal

- Ranking and scoring code receives booleans or tiers, never logins, names or
  orgs.
- Public outputs (issue labels, advisory reports, PR text) carry counts and
  generic labels only: no names, no org names, no per-person countries.
- Every boost is a config knob, default off, with a cap.
- Other spokes use only data they collected themselves.
