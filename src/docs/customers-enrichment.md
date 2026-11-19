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

Rollout status: this release adds the switches, the Settings home and this
page. The gitdm source, the devstats activity block and the crowd.dev source
each land in their own later release; until a source ships, turning its switch
on does nothing.

## How results are used

- **Affiliation is only ever a suggestion.** A gitdm or crowd.dev affiliation is
  offered in the person's details with the existing Accept and Dismiss buttons
  and a chip naming the source and the dates. Nothing overwrites an
  organization: a value set by hand and a value the person shared from the hub
  always win, and accepting is a decision an owner makes.
- **Activity is read-only.** devstats activity appears as a "Public
  contributions" block in the person's details, with external links.
- **Only confirmed customers are looked up.** A suggested person is never
  enriched, and a lookup needs a confirmed GitHub login (gitdm also matches a
  confirmed email locally, on the hive).
- **Forget erases enrichment.** Forgetting a person deletes everything found for
  them, and the tombstone stops it from being added again.
- **Audit lines carry ids, not values.** Each lookup and each change to the
  crowd.dev token writes an audit line with the person id and the provider (or
  that the token changed), never the facts found or the token.

Enrichment results are kept in `/data/customers/enrichment.json` (mode 0600),
keyed by person id with the source and fetch time of each fact. The file can be
deleted at any time; it is rebuilt from the sources.

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
