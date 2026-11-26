# Customers enrichment: public affiliation and activity

Customers enrichment adds public facts about **confirmed customers** from
CNCF-published sources: who a person works for, with dates, and their public
contributions to CNCF projects. Hive uses the
[hivecommons/colony](https://github.com/hivecommons/colony) resolver for the
lookups.

**Everything here is off by default.** With every switch off a hive behaves
exactly as it does without the feature: no file is downloaded, no lookup is
made and nothing extra is stored or shown.

## Sources

| Source | What it adds | What leaves the hive | Switch |
|---|---|---|---|
| gitdm | Affiliation (organization, from, until) from the curated [cncf/gitdm](https://github.com/cncf/gitdm) files | Only a download of the public gitdm files from GitHub, the same files for every hive. Matching runs on the hive; no login, email or anything else about a person is sent. | `customers.enrichment.gitdm.enabled` |
| devstats | Public contribution activity per CNCF project, with links to devstats | The person's GitHub login, never an email, sent to CNCF devstats | `customers.enrichment.devstats.enabled` |
| crowd.dev (LFX Insights) | Affiliation and activity from crowd.dev | The person's GitHub login, never an email, sent to the crowd.dev API with your token | `customers.enrichment.crowddev.enabled` |

Rollout status: the gitdm and devstats sources are live. crowd.dev is not:
LFX Insights has no public API it can use today (hivecommons/colony#5), so its
switch and token field are accepted but make no request.

## How gitdm works

The `customers_enrichment` optional sweep runs every 30 minutes. With the gitdm
switch on it:

1. Downloads the public gitdm files again when the snapshot is older than
   `customers.enrichment.gitdm.refresh_interval` (default 24h). The download
   uses ETags, so an unchanged file is not sent again, and it is cached in
   `/data/customers/colony-cache`.
2. Looks up, on the hive, each confirmed customer whose last lookup is older
   than the snapshot, by their confirmed GitHub login and confirmed email
   addresses. At most 200 people per pass; the rest wait for the next one.
3. Stores the affiliations found, with their dates, in
   `/data/customers/enrichment.json`, and writes one audit line
   (`customer_enrichment`, person id and provider) when a person's facts
   change.

## How devstats works

With the devstats switch on, the same sweep looks up each confirmed customer
who has a confirmed GitHub login and whose activity is older than 24 hours.
Each lookup sends that login, and nothing else, to `devstats.cncf.io` (a POST
to its public API). Lookups are capped by
`customers.enrichment.devstats.hourly_limit` (default 30 an hour across all
people); people over the limit wait for a later pass. The answer, total CNCF
contributions with a link to the devstats dashboard, appears read-only as
**Public contributions** in the person's details, with an external link.
A person known only by email is never looked up.

With a switch off a pass only tidies: it deletes stored gitdm facts and the
facts of people who are gone, and makes no request. Like every optional sweep
it stands down when the hive's API budget asks optional work to wait.

## How results are used

- **Affiliation is a suggestion unless an owner picks a policy.** A gitdm or
  crowd.dev affiliation is offered in the person's details with the existing
  Accept and Dismiss buttons and a chip naming the source and the dates. A value
  set by hand and a value the person shared from the hub always win. With the
  auto-accept policy (below) left at its default, `off`, accepting is always a
  decision an owner makes.
- **Auto-accept (`customers.enrichment.auto_accept`, default `off`).** With
  `gitdm_high`, a gitdm affiliation matched by GitHub login (confidence high)
  and current today fills the organization when it is empty or holds an earlier
  auto-accepted value. It never replaces a value a person set or cleared, a
  value from the hub, or the company on the person's own public forge profile,
  which is their own statement. An email-only match (confidence medium) is never
  accepted. The field is labelled "auto-accepted from cncf/gitdm" and has a
  Dismiss button; dismissing clears it and the value is not accepted again.
  Each auto-accept writes a `customer_enrichment` audit line with the person id
  and provider `gitdm_auto_accept`.
- **Activity is read-only.** devstats activity appears as a "Public
  contributions" block in the person's details, with external links.
- **Only confirmed customers are looked up.** A suggested person is never
  enriched, and a lookup needs a confirmed GitHub login (gitdm also matches a
  confirmed email locally, on the hive).
- **Forget erases enrichment.** Forgetting a person deletes everything found for
  them, and the tombstone stops it from being added again.
- **Audit lines carry ids, not values.** Each change enrichment makes to a
  person's facts writes a `customer_enrichment` audit line with the person id
  and the provider, and each change to the crowd.dev token writes one saying
  only that the token changed. Never the facts found or the token.

Enrichment results are kept in `/data/customers/enrichment.json` (mode 0600),
keyed by person id with the source and fetch time of each fact. The file can be
deleted at any time; it is rebuilt from the sources. With `customers.store`
`shadow` or `colony` they are also written to `/data/customers/colony.db` as
suggested enrichment facts: the current gitdm affiliation (source
`colony:gitdm`, with its confidence and the identity it matched) and one
`activity:<project>` count per devstats project (source `colony:devstats`). See
[the colony store](design/colony-customer-data-backend.md#spoke-implementation-p3).

## Settings

Settings, Features, Customers holds every knob, under **Public affiliation and
activity (enrichment)** and its **Enrichment sources** group. Each switch needs
the one above it on, and the whole block needs Customers on.

| hive.yaml key | Default | Environment override | Meaning |
|---|---|---|---|
| `customers.enrichment.enabled` | `false` | `HIVE_CUSTOMERS_ENRICHMENT_ENABLED` | Master switch |
| `customers.enrichment.gitdm.enabled` | `false` | `HIVE_CUSTOMERS_ENRICHMENT_GITDM_ENABLED` | gitdm source |
| `customers.enrichment.gitdm.refresh_interval` | `24h` | `HIVE_CUSTOMERS_ENRICHMENT_GITDM_REFRESH_INTERVAL` | How often the public gitdm files are downloaded again (a Go duration, at least `1h`) |
| `customers.enrichment.devstats.enabled` | `false` | `HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_ENABLED` | devstats source |
| `customers.enrichment.devstats.hourly_limit` | `30` | `HIVE_CUSTOMERS_ENRICHMENT_DEVSTATS_HOURLY_LIMIT` | devstats lookups all people may make in one hour (at most `120`) |
| `customers.enrichment.crowddev.enabled` | `false` | `HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_ENABLED` | crowd.dev source |
| `customers.enrichment.auto_accept` | `off` | `HIVE_CUSTOMERS_ENRICHMENT_AUTO_ACCEPT` | Auto-accept policy: `off` or `gitdm_high`. Off whenever gitdm is off; an unknown value means `off`. Set in hive.yaml or the environment; Settings does not show it yet |
| `customers.enrichment.crowddev.token_file` | `/data/secrets/customers_crowddev_token` | `HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_TOKEN_FILE` | Keystore-sealed crowd.dev API token. `HIVE_CUSTOMERS_ENRICHMENT_CROWDDEV_TOKEN` supplies the token directly and wins over the file |

The crowd.dev token is never stored in hive.yaml. Settings saves it through the
owner-only `POST /api/customers/enrichment/settings`, which seals it into the
token file and only ever reports whether a token is set.

```yaml
customers:
  enrichment:
    enabled: true
    gitdm:
      enabled: true
      refresh_interval: 24h
```

## Privacy

The Customers section's rule is that nothing it stores leaves the hive
([design, section 12](design/customers.md#12-agents-and-privacy-guard-rails)).
Enrichment is the reviewed exception, and only when an owner turns it on:

- gitdm sends nothing about anyone. It downloads public files.
- devstats and crowd.dev send a confirmed customer's public GitHub login, and
  nothing else, to a CNCF or Linux Foundation service. Both are listed in
  [telemetry.md](telemetry.md) with their switches.
- No request carries the hive's identity, a user name or a token other than the
  crowd.dev API token the owner supplied. Requests use a generic User-Agent and
  follow no redirects.
