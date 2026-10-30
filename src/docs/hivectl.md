# hivectl

`hivectl` is the non-interactive command-line client for a running Hive
dashboard API. The `hive` binary runs the service; `hivectl` inspects and
operates it.

Choose the task before choosing credentials:

| I want to… | Start here | What you need |
| --- | --- | --- |
| Check or operate a running Hive | [First commands](#first-commands), then [command reference](#commands) | Its dashboard URL and an accepted credential. Reads do not grant permission to write. |
| Watch agents in a terminal | [Launch and endpoint selection](#launch-and-endpoint-selection) | A real terminal; the TUI uses `HIVE_DASHBOARD_URL`, not `--server`. |
| Contribute my worker to Hives | [Contributor profiles](#hives--named-profiles-for-the-hives-you-contribute-to) | Local contributor setup; profiles are separate from dashboard login. |
| Enroll a repository in hosted lite mode | [`enroll`](#enroll--spoke-based-lite-repo-enrollment) | Local `gh` authentication and repository access. |
| Export or analyze evidence locally | [`evidence`](#evidence--export-stored-bundles) and [benchmarks](#evidence--benchmark) | The inputs named by that command; these are not agent-control procedures. |
| Develop or record the TUI | [Seeing it run](#seeing-it-run) | A source checkout and the developer tools listed there. |

A **spoke** is a Hive connected to a coordinating **hub**. These deployment names do not choose your permissions: the dashboard's authentication policy does. See the [glossary](dashboard-glossary.md#terms) for other terms.

## Quick start

### Getting the binary

On a host installed with `bin/hive-podman-setup.sh` there is nothing to do:
the installer extracts `hivectl` from the Hive image onto the host —
`~/.local/bin/hivectl` rootless, `/usr/local/bin/hivectl` rootful — and
refreshes it on every re-run, so it always matches the running hive. This is
the supported route on image-based hosts (Fedora Silverblue/Bluefin, RHEL
image mode), which have no Go toolchain and a read-only `/usr` (#5646).

The same extraction works by hand against any pulled Hive image. The image
carries the binary as cargo at `/usr/local/share/hive/hivectl` (deliberately
off the container's PATH — it is a client of the dashboard API, not part of
the runtime; run it on the host, never inside the container):

```bash
podman create --name hivectl-extract ghcr.io/hivecommons/hive:stable
podman cp hivectl-extract:/usr/local/share/hive/hivectl ~/.local/bin/hivectl
podman rm hivectl-extract
```

Contributors working from a checkout usually do not need a Go toolchain: `just contribute-tui` and `just contribute-hives` call `bin/hivectl-bootstrap.sh`, which resolves `HIVECTL`, `./bin/hivectl`, user/system installs, and `PATH` in that order. If none exists, it resolves an image from the checkout: a reachable release tag image such as `ghcr.io/hivecommons/hive:v5.4.0` wins when `git describe --tags --abbrev=0` names a release tag; otherwise it falls back to the branch channel (`v5` → `stable`, `v6` → `edge`, `v4` → `stable`). It extracts the binary into `./bin/hivectl`, records the image digest plus `hivectl version` in `./bin/.hivectl.source`, and refreshes when the digest changes. Before running a staged binary, the bootstrap refuses if that binary's commit predates checkout-local `src/pkg/hivectl` changes, naming the checkout version, image/binary version, and the escape hatches. Override the image with `HIVECTL_BOOTSTRAP_IMAGE=...`; the skew check still runs and the error notes the override. An explicit `HIVECTL=/path/to/hivectl` bypasses image and skew checks because the user chose the binary.

To build from source manually, run from the repository root:

```bash
(cd src && go build -o ../bin/hivectl ./cmd/hivectl)
```

### First commands

Ask the operator which authentication lane the target dashboard accepts. The shared token is not a substitute for a personal session.

| Dashboard deployment | Authenticate for API commands |
| --- | --- |
| Self-hosted with `dashboard.auth_token`, no `authorized_users` | Export `HIVE_DASHBOARD_TOKEN` with the server's token. See [token setup and rotation](env-vars.md#generating-and-rotating-hive_dashboard_token). |
| Direct-route spoke with an `authorized_users` allowlist, or hub-hosted Hive | Run `hivectl --server URL login`, approve its device-flow prompt, then use that same URL for commands. Login proves identity; the live allowlist decides your role. |

For a shared-token Hive:

```bash
# Point at a server (default: http://127.0.0.1:3001) and provide the token
# via an environment variable, never as a flag value. The value must match the
# server's dashboard token — see "Generating and rotating HIVE_DASHBOARD_TOKEN"
# in env-vars.md (generate with: openssl rand -hex 32).
export HIVE_DASHBOARD_TOKEN="..."
hivectl system status

# On a Podman-standalone host, talk to the gateway's published port — the
# installer prints the exact URL at the end of its run:
hivectl --server http://127.0.0.1:3001 system status

```

For a session-based Hive:

```bash
hivectl --server https://hive.example.com login
hivectl --server https://hive.example.com system status
```

Success prints the Hive's runtime status. Next, inspect `hivectl agent list` or open the [TUI](#launch-and-endpoint-selection). If access fails, check the URL and credential lane first; exit 3 means authentication or authorization failed, while exit 4 means a connection or timeout failure. A successful read does not prove permission for an owner-only write.

From a source checkout the binary is `bin/hivectl` instead. Repository enrollment is a separate task: see [`enroll`](#enroll--spoke-based-lite-repo-enrollment).

## Running against a local Hive

Read-only and operational commands (`system`, `agent list/get/logs/kick/...`,
`bead`, `governor get`, `observe`) work against a plain local Hive. A few
commands need extra setup that a hand-run instance may lack:

- **knowledge writes** (`create/update/delete/import`) require the knowledge
  layer to have a wiki `url` in `hive.yaml` — Hive does not bundle llm-wiki, so
  a `path`-only layer returns "no configured endpoint".
- **config writes** (`agent prompt/model/backend/pipeline set`, `governor
  *-set`) write to the server's `/data/policies` dir, which must be writable
  (e.g. a container or Linux run); some take effect only after reload/restart.
- **auth**: set `dashboard.auth_token` (or `HIVE_DASHBOARD_TOKEN`) and pass it
  via `--token-env`. Bearer auth is rejected on direct-route spokes
  (`authorized_users` set with `hub_proxied: false`), which require a per-user
  session instead.

## Global options

| Flag | Default | Description |
| --- | --- | --- |
| `--server` | `http://127.0.0.1:3001` | Dashboard API base URL |
| `--token-env` | `HIVE_DASHBOARD_TOKEN` | Env var holding the auth token |
| `--timeout` | `30s` | Per-request (or stream) timeout |
| `-o, --output` | `table` | Output format: `table`, `json`, `yaml`, `jsonl` |

Results go to stdout, errors to stderr. Exit codes:

| Code | Meaning |
| ---: | --- |
| 0 | Success |
| 1 | Local/unclassified failure |
| 2 | Invalid arguments or missing `--yes` |
| 3 | Authentication/authorization failure |
| 4 | Connection or timeout failure |
| 5 | Hive API/server-side error |

## Commands

Use these groups as reference after connecting successfully. Before changing an agent, inspect its current state; after a write, read it back. Server-side role and policy checks still apply.

### system — runtime state

Start with `status` for a summary, then `health` if something looks wrong. Use JSON for scripts; `snapshot` gives a broader diagnostic view. These commands inspect the service rather than start or restart it.

```bash
hivectl system status -o json
hivectl system health
hivectl system version
hivectl system snapshot
hivectl system events --follow --timeout 2m -o jsonl   # SSE stream as JSONL
```

`events` is a stream, so `--follow` is required and output must be `jsonl`.

### agent — manage and operate agents

List agents to get an exact name, then inspect that agent and its logs. `kick` asks it to work; pause, resume, restart and delete are operational changes, not log filters. The `pause|resume|restart` line below denotes three alternatives: choose one word, rather than pasting the `|` characters into a shell.

```bash
hivectl agent list
hivectl agent get quality -o yaml
hivectl agent logs quality --lines 200
hivectl agent kick quality --prompt "Review the work list"
hivectl agent pause|resume|restart quality
hivectl agent export quality -o yaml            # portable AgentDefinition
hivectl agent import --file agent.yaml --preview # validate without applying
hivectl agent delete ux-discovery --yes
```

Configure an agent (prompt template, model/backend, pipeline toggles):

```bash
hivectl agent prompt get quality --raw
hivectl agent prompt set quality --file quality.md
hivectl agent model-set quality claude-sonnet-4-6
hivectl agent backend-set quality claude
hivectl agent jev-mode-set scanner assist                  # Jev typed-decision tool: off | assist
hivectl agent pipeline-set quality --file pipeline.yaml   # map of step: bool
```

### knowledge — inspect and maintain knowledge

Search or list entries before changing them, then get the exact layer and ID. `stats` and `health` help diagnose an empty or unavailable store. Writes need the layer's configured wiki endpoint; deletion needs `--yes`, and lifecycle-state changes are owner-only as described below.

The `knowledge` group is also available under the alias `wiki`. Suggestions are a separate local, PR-based workflow; they do not immediately publish knowledge.

```bash
hivectl knowledge search "user journey" --limit 20
hivectl knowledge list --limit 200 --offset 0 -o json  # first page across all layers
hivectl knowledge list --limit 200 --offset 200 -o json # next page
hivectl knowledge list --layer project        # layer: personal|project|org|community
hivectl knowledge get project create-project
hivectl knowledge stats
hivectl knowledge health                      # knowledge engine health check
hivectl knowledge graph --root create-project --depth 3 -o json
hivectl knowledge export > hive-knowledge.md

hivectl knowledge create --file fact.yaml     # title + body required
hivectl knowledge update project create-project --file update.yaml
hivectl knowledge import --file ux-wiki.md --format markdown --layer project
hivectl knowledge delete project create-project --yes
```

Without `--layer`, `knowledge list` returns one page: `--limit` defaults to 50
(1–200), and `--offset` defaults to 0. The response includes `total`, `limit`,
`offset`, and `has_more`; while `has_more` is true, request the next page by
increasing `--offset` by `--limit`. `--type` applies before pagination.
With `--layer`, the command still lists the entire layer; combining `--layer`
with `--limit` or `--offset` is an error.

`graph` reads the knowledge graph (`--root` optional; `--depth` defaults to 2
and must be greater than zero). `import` posts Markdown or structured content
from `--file` or `--stdin` (`--format` defaults to `markdown`, `--layer` to
`project`) and, like the other knowledge writes, needs the layer's wiki `url`
configured — see [Running against a local Hive](#running-against-a-local-hive).

`state` changes an entry's lifecycle state (`draft`, `approved`, `deprecated`
or `superseded`) in the local channel that holds it, via
`PUT /api/knowledge/entry/{id}/state`. It is owner-only, and the change is
recorded in the audit log together with `--reason`. `superseded` requires
`--superseded-by`, which names the replacement entry in the same channel. See
[Knowledge lifecycle states](knowledge-lifecycle.md#changing-state-over-the-api).

```bash
hivectl knowledge state old-relay deprecated --reason "relay removed"
hivectl knowledge state old-relay superseded --superseded-by relay-v2 --reason "relay rewritten"
```

`suggest` lets an agent propose a knowledge change without changing what other
agents see. It runs locally against a repository checkout (no dashboard call):
it writes the change under `.hive/wiki/` (`--dir`, `--repo-root`) and prints
the branch, PR title and PR body to open a pull request with. The suggestion
becomes approved knowledge only when a human merges that PR; closing the PR
rejects it.

```bash
hivectl knowledge suggest --title "Retry relay uploads" --body-file note.md \
  --source https://github.com/acme/app/pull/12 --reason "learned while fixing #12" \
  --type gotcha --repo acme/app --layer project --tags relay,ops --pr-body-file pr.md
hivectl knowledge suggest --action replace --target old-relay --title "Relay v2" --stdin \
  --source acme/app#40 --reason "relay rewritten"
hivectl knowledge suggest --action deprecate --target old-relay --source acme/app#41 --reason "relay removed"
```

`--action` is `add` (default), `update` (rewrite `--target` in place),
`replace` (new entry; `--target` is marked `status: superseded` with
`superseded_by`, and the new entry records `supersedes`) or `deprecate`
(`--target` is marked `status: deprecated`). `--source` (PR/issue URL or
`owner/repo#N`) and `--reason` are always required. `--status` is the proposed
lifecycle status once merged (`draft`, `approved` — the default — or
`deprecated`). Existing entries with a similar title are written to the
entry's `related` front matter and listed in the PR body as possible
duplicates, and every entry carries a `dedupe_key`.

### bead — work items

A **bead** is a record in Hive's work ledger. It may track an issue, a review/advisory finding or another task; creating one does not itself open a GitHub issue or publish an approved knowledge entry. Use `knowledge` for knowledge entries.

First list the target agent's beads. Create a bead only when you want to add work to that store; `--priority 2` means medium priority. The supported priorities are 0 critical, 1 high, 2 medium, 3 low and 4 minor.

```bash
hivectl bead list --agent quality -o json
hivectl bead create --agent quality --title "Review UX journey" --type advisory --priority 2
hivectl bead reset --agent quality --reason "replace stale findings" --yes
```

**Reset closes work; it does not delete the ledger.** An owner may reset one agent's store with `--agent`, or every initialized agent store by omitting it. In those stores, open, in-progress and blocked beads become closed with the reason and timestamps; already closed records stay closed. The response reports closed counts. List the beads again to check the result, especially after an all-store reset: individual store failures are logged server-side, so a response is not a guarantee every store was reset. This does not close linked GitHub issues or change knowledge entries.

### governor — scheduler configuration

Read `governor get` before applying configuration. The `*-set` commands write the supplied configuration; agent add/remove changes the configured fleet. The server needs a writable policy directory, and some writes need a reload or restart as described [above](#running-against-a-local-hive).

```bash
hivectl governor get -o yaml
hivectl governor thresholds-set --file thresholds.yaml
hivectl governor budget-set --file budget.yaml
hivectl governor repos-set --file repos.yaml
hivectl governor agent-add ux-discovery --backend copilot --model claude-sonnet-4-6
hivectl governor agent-remove ux-discovery --yes
hivectl governor hotspots --repo hivecommons/hive   # merge hotspots: files concurrent PRs keep touching (GET /api/hotspots)
```

### claim / unclaim / claims — issue claims

```bash
hivectl claim hivecommons/hive#8380              # "mine" — contributors and agents back off
hivectl claim hivecommons/hive#8380 --until 6h   # hold longer than the hive default (4h)
hivectl claim hivecommons/hive#8380              # run again to renew
hivectl takeover hivecommons/hive#8380           # = claim --force: take over another person's claim
hivectl unclaim hivecommons/hive#8380            # release yours (or a lower-ranked holder's)
hivectl unclaim hivecommons/hive#8380 --force    # owner override: release regardless of holder
hivectl claims -o json                           # every live claim on this hive
```

A claim records who is working an issue *right now* ([#8380](https://github.com/hivecommons/hive/issues/8380)).
Holders are ranked **human > agent > contributor > external**: a human's claim takes an issue over from a hive
agent or a contributor agent and the displaced holder is told to stop — a relay session is
yanked off the item and handed different work immediately; an agent or an external bot sees the takeover
comment and the `preempted:<login>` label on the issue. At the same rank the command warns (`held`, exit 3
with the holder named) unless `--force`. A lower rank can never displace a higher one. Claims lapse on their
own (default 4h for a person, 2h for an agent kick, 30m for a relay task — configurable under `claims:` in
`hive.yaml`) and a relay's claim is released the moment its task finishes or is revoked. The hive mirrors each
transition onto the GitHub issue as a `🔒`/`🔁`/`🔓` comment with a machine-readable `<!-- hive:claim … -->`
marker and a `claimed` label, so a human, a script or another hive can see the hold without asking the API.

### review — review evidence

Use this to retrieve the evidence behind a PR review. Fetching needs **owner or merger** access to the target Hive; verifying a saved bundle needs no Hive session. Start with the newest bundle, then select an exact head if you are investigating an older decision.

```bash
hivectl review evidence hivecommons/hive#11061                  # newest bundle as JSON on stdout
hivectl review evidence hivecommons/hive#11061 --head 1a2b3c4   # a specific head (unique 7+ char prefix)
hivectl review evidence hivecommons/hive#11061 -o bundle.json   # save the bundle
hivectl review evidence hivecommons/hive#11061 --zip -o ev.zip  # bundle + verdict reports + review links
hivectl review evidence verify bundle.json --pubkey <hex>       # offline: hash + Ed25519 signature
hivectl review evidence verify ev.zip                           # uses the public key carried in the zip
```

`review evidence` reads `GET /api/review/evidence`. A bundle removed by retention
fails with `evidence expired`, not a generic not-found.

`verify` recomputes the canonical
hash, then checks the signature with `--pubkey` (hex, base64, or a file holding either) or, when omitted, the public
key embedded in the file (a zip manifest's `public_key`). Unsigned bundles, hash mismatches and bad signatures exit
non-zero. See [Review evidence bundle](review-evidence.md).

### evidence — export stored bundles

Use this for a local report from evidence files you can read. No dashboard session or network is needed. By default it emits one JSONL row per PR; choose CSV for a spreadsheet, and check stderr for skipped invalid bundles before trusting totals.

```bash
hivectl evidence export                                            # every PR, JSON Lines on stdout
hivectl evidence export --since 168h --format csv --out ev.csv     # last 7 days as CSV
hivectl evidence export --since 2026-10-01T00:00:00Z --repo hivecommons/hive
hivectl evidence export --root ./evidence --format csv | column -ts,
```

#### Export storage and folding reference

`evidence export` reads the schema v1 bundles under `--root` (default `/data/evidence`, layout
`<org>/<repo>/<pr>/<sha>.json`) directly from the filesystem — no hive session, no network — and emits one row per
pull request, folding every bundle of the PR (verdicts from the newest bundle; outcome, links and open time from
whichever bundle recorded them) exactly as `GET /api/evidence/summary` does, so the numbers reconcile with the
dashboard Evidence charts.

#### Export filters and columns

`--since` takes an RFC3339 timestamp or a Go duration (`72h`) and keeps PRs opened at or after it; `--repo` narrows
to one `owner/name` (case-insensitive); `--format` is `jsonl`
(default) or `csv`; `--out FILE` writes a file instead of stdout. Rows are ordered by repo then PR number. Bundles that
do not parse or are not schema v1 are skipped and counted on stderr, never fabricated.

Columns: `repo`, `pr`, `head_sha`, `bundle_id`, `author_login`, `author_kind`, `author_model`, `acmm_level`,
`perspectives`, `reviewer_models`, `verdicts` (per perspective: perspective, verdict, model, backend, confidence),
`ci_conclusion`, `ci_captured_at`, `outcome` (`merged`, `closed` when a backfilled bundle records a close without merge, else `open`),
`merged_at`, `revert_of`, `reverted_by`, `followup_of`, `followed_up_by`, `shadow_review_would_have_held`,
`shadow_intent_would_have_blocked` (empty/`null` when the gate was not evaluated), `sentinel_hits`, `signed`,
`created_at`, `updated_at`, `opened_at`. In CSV, lists join with `;` and each verdict is `perspective:verdict:model:backend:confidence`.
Reporting only: nothing here reads into or alters merge eligibility. See
[Review evidence bundle](review-evidence.md#exporting).

### evidence — benchmark

Use a benchmark when you want to compare PRs reviewed by Hive with those merged without it. You need a readable local evidence tree or a saved report for `--from-json`; the command does not log in to a Hive or send telemetry. It writes report and image files. `--readme FILE` supports legacy marked documents, not the hand-written root README.

```bash
hivectl evidence benchmark --month 2026-10 --out-dir src/docs/benchmarks
hivectl evidence benchmark --root ./evidence --month 2026-10 --spend cost.json --hub hub.json --source "hive-oke snapshot"
hivectl evidence benchmark --from-json src/docs/benchmarks/outcomes-2026-10.json --out-dir src/docs/benchmarks
```

#### Benchmark measures and inputs

`evidence benchmark` folds the schema v1 bundles under `--root` per PR and compares the pull requests Hive's review
swarm touched (**mediated**: at least one verdict or posted review on any head) with those merged without it
(**unmediated**): revert rate and change-failure rate over merges observed ≥ 14 days, median time-to-merge, CI failure
at merged head, the share of mediated merges that carried a `changes_requested` verdict before merging, and — when
`--spend` names a saved `GET /api/cost` response — cost per merged PR. Every rate carries its n and a 95 % Wilson
interval; n < 10 is `not reported`.

`--month YYYY-MM` bounds the merge time (default all), `--now` fixes the
observation time for maturation, `--hub` adds a saved `GET /api/hub/evidence` response as fleet context, `--source`
names the data in the methodology section.

#### Benchmark outputs and re-rendering

It writes `outcomes-YYYY-MM.{md,json}`,
`assets/outcomes-YYYY-MM-{bars,trend}.svg` and the legacy plain-language cards
`assets/outcomes-YYYY-MM-card-<name>-{dark,light}.{svg,png}` under `--out-dir` (the PNG is drawn in-process with an
embedded font), and `--readme FILE` rewrites a legacy document's
`<!-- outcomes:start -->…<!-- outcomes:end -->` block from the same report.

The legacy block carries only the cards
and flat sentences, no intervals: `--readme-min-base N` (default 200) is the smallest base a production-count card
may rest on, separate from the n ≥ 10 report threshold; `--stories FILE` supplies curated caught → fixed → merged
examples and `--red-team FILE` the prompt-injection evaluation counts (see [benchmarks/](benchmarks/README.md) for
both formats). The root README uses a separate renderer:
`node docs/readme/render-charts.mjs`. See [README graphics](../../docs/readme/README.md).

`--from-json FILE` skips the evidence tree and re-renders every output from a saved report, for
redrawing committed assets after a layout change. Local filesystem only; no hive session, no telemetry.
`src/scripts/outcome-benchmark.sh` wraps it (`--rerender` = `--from-json` on the newest committed report); see [benchmarks/](benchmarks/README.md).

### evidence — backfill bundles from GitHub history

Use backfill to reconstruct reporting records from GitHub history, not to manufacture review verdicts. You need GitHub API access through `$GITHUB_TOKEN`, `$GH_TOKEN` or `gh auth token`, plus a writable local output root for a real run. Start with `--dry-run`: it counts without writing. Then inspect the summary before choosing to write or refresh records.

```bash
hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --dry-run          # counts only, writes nothing
hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --root ./evidence   # write schema v1 bundles
hivectl evidence backfill --repo kubestellar/console --since 2026-03-01 --until 2026-05-01 --acmm-level 3
hivectl evidence backfill --repo hivecommons/hive --since 2026-06-01 --flag hive_on_hive
hivectl evidence backfill --repo ublue-os/bluefin --since 2025-01-01 --refresh-fields merge.opened_at  # fill a field on old bundles
```

#### Backfill provenance and limits

`evidence backfill` reconstructs evidence bundles from GitHub pull-request history for PRs created in
`[--since, --until)` and writes them under `--root` (default: the hive's `/data/evidence`) at
`<owner>/<repo>/<pr>/<head_sha>.json`. It talks to the GitHub API directly (token from `$GITHUB_TOKEN`,
`$GH_TOKEN` or `gh auth token`; `--github-api` for GHES), not to a hive. Only what history records is written —
author with a documented kind heuristic (`--agent-login` names logins to count as agents), merged / closed / open
with timestamps, merger, and revert / follow-up links under the live recorder's rules — and every bundle is marked
`backfilled: true`; verdicts, CI, sentinel and shadow fields are left absent, never fabricated. Pre-registration
exclusions are stamped as `backfill.flags` (`--flag placeholder_hive|hive_on_hive|under_min_merged`; `hive_on_hive`
and `under_min_merged` are added automatically), never applied by dropping PRs. Bundles that already exist are
skipped, so re-running is safe; `--skip-mergers` saves one request per merged PR; `--signing-key-file` signs the
output.

#### Refresh an earlier backfill

`--refresh-fields <list>` additionally fills the named fields (currently `merge.opened_at`) on bundles an
earlier backfill already wrote, only where absent and only from the list page (no per-PR requests, ~1 call per 100
PRs); nothing else on those bundles changes, bundles that already have the field are not rewritten, and live
(non-backfilled) bundles are never touched. The summary reports `to_refresh` / `refreshed`. Record-only: nothing here reads into or alters merge eligibility. See
[Review evidence bundle, Backfilled bundles](review-evidence.md#backfilled-bundles) and the run plan in
[`research/backfill.md`](research/backfill.md).

#### Backfill issue reporting

`hivectl evidence backfill-issues --repo <owner/repo> --since <date> [--until <date>] [--root <dir>] [--agent-login <login>] [--dry-run]`
records GitHub issues opened and closed in the range as ISO-week files at
`<root>/<owner>/<repo>/issues/<year>-W<week>.json`. GitHub's issues endpoint also
contains pull requests; these are excluded. The command scans older issues too,
so a close in the range is counted even if the issue opened earlier. Each file
includes zero-count weeks, event times for exact custom summary windows, and
coverage bounds. Re-run it to refresh recent weeks. The files are reporting data
for operators and downstream readers. The dashboard work-balance card reads the
series for weeks before collector coverage and keeps the collector-maintained
rollup authoritative from its first covered Monday. For a collector starting
midweek, set the exclusive `--until` to the following Monday to cover the whole
boundary week; a shorter backfill retains audit fallback there. Neither path
affects merge eligibility.

### observe — read-only metrics

Use these commands to investigate usage and recent decisions without changing the Hive. For a cost jump, start with `tokens`; for who changed something, use `audit`. `trends` accepts a named range or hours, not both.

```bash
hivectl observe tokens -o json
hivectl observe audit
hivectl observe history
hivectl observe timeline
hivectl observe trends --range week        # or --hours 12 (1-720); not both
```

### login / logout — hold a per-user session from the terminal

```bash
hivectl login                                    # against the default --server
hivectl --server https://hive.example.com login  # against a specific hive
hivectl logout
```

A hive dashboard identifies callers two ways, and they are not
interchangeable: self-hosted hives accept the shared bearer token
(`HIVE_DASHBOARD_TOKEN`), while hub-hosted hives and spokes with an
`authorized_users` allowlist accept only a **per-user session** — the
`hive_session` cookie the GitHub device-flow login mints, resolved on every
request against the live allowlist. `hivectl login` runs that device flow from
the terminal ([#5651](https://github.com/hivecommons/hive/issues/5651)): it
prints a one-time code and `https://github.com/login/device`, waits for you to
approve there, and caches the minted session so every subcommand — and
`hivectl tui` — presents it automatically.

Details worth knowing:

- **The login proves identity only.** It requests no OAuth scope at all, and
  your role (owner or viewer) is re-resolved by the hive on every request —
  caching a session caches who you are, never what you may do.
- **The cache** lives at `$XDG_CONFIG_HOME/hive/sessions.json` (default
  `~/.config/hive/sessions.json`), owner-only (0600), keyed by dashboard URL
  so one operator can hold sessions for several hives. The credential is never
  printed.
- **Precedence:** an explicitly exported `HIVE_DASHBOARD_COOKIE` always wins
  over the cache. The token lane is independent — both credentials are
  presented when both exist, and the hive honours whichever lane its
  deployment implements.
- **A hive runs one device flow at a time.** If another operator's login
  starts while yours is pending, yours is replaced and `hivectl login` says
  so — just run it again.
- **On an allowlist spoke, an unauthorized GitHub account is refused** with
  the server's own explanation naming the account; nothing is cached.
- **Sessions expire** (30 days server-side). When a cached session stops
  working, hivectl says to run `hivectl login` again instead of showing a bare
  401.
- `hivectl logout` ends the session server-side via the existing endpoint and
  removes the cached credential; the cache is cleared even when the hive is
  unreachable.

### tui — live terminal dashboard

```bash
hivectl tui
```

A full-screen, keyboard-driven view of the fleet — agents, the governor,
token/cost spend, and recent activity — over the same dashboard API the
non-interactive subcommands above use. It is not a second Hive runtime, just
another client of the API: same auth token, same endpoints, same SSE stream
the web dashboard consumes. Requires a real terminal.

This is the v1 delivery of the `hive tui` epic
([#4907](https://github.com/hivecommons/hive/issues/4907)); the design
rationale and the fixed architecture decisions behind it are recorded in
[`src/docs/design/tui.md`](design/tui.md).

#### Launch and endpoint selection

`hivectl tui` does **not** honour the root `--server` / `--token-env` flags the
other `hivectl` commands read: it builds its own client directly from three
environment variables, checked once at startup:

| Variable | Default | Meaning |
|---|---|---|
| `HIVE_DASHBOARD_URL` | `http://localhost:3001` | Dashboard API base URL |
| `HIVE_DASHBOARD_TOKEN` | *(empty)* | Shared dashboard token — the same variable `--token-env` defaults to |
| `HIVE_DASHBOARD_COOKIE` | *(empty)* | Session cookie header, for hives that do not accept the shared token — see [Credentials](#credentials) |

If you already have `HIVE_DASHBOARD_TOKEN` exported for the commands above,
`hivectl tui` picks it up for free — the token variable name is shared on
purpose. The base URL default differs by one detail from `--server`'s
(`http://localhost:3001` vs. `http://127.0.0.1:3001`): both resolve to the
same loopback dashboard, but set `HIVE_DASHBOARD_URL` explicitly if you also
pass `--server` to other commands against a non-default host, since the TUI
will not pick that flag up.

For contributor profile switching without the dashboard panes, use
`hivectl tui --hives`. That mode opens directly to the Hives overlay and does
not build a dashboard API client.

#### Credentials

The dashboard has two credential lanes and they are **not** interchangeable.
Which one your hive accepts is a property of how it was deployed, not a
preference:

| Your hive | What it accepts | Set |
|---|---|---|
| Self-hosted, `dashboard.auth_token` set, no `authorized_users` | Shared bearer token | `HIVE_DASHBOARD_TOKEN` |
| Spoke with an `authorized_users` allowlist (direct-route) | Per-user session **only** | `hivectl --server URL login`; cached session, or explicit `HIVE_DASHBOARD_COOKIE` |
| Hub-hosted | The hub's per-user session | `hivectl --server URL login`; cached session, or explicit `HIVE_DASHBOARD_COOKIE` |

The shared token grants unscoped owner and carries no per-user identity, which
is exactly why a spoke with an allowlist **disables** it — accepting it would
let any holder act as an owner and defeat the per-hive allowlist. Those hives
identify callers by the `hive_session` cookie the GitHub device-flow login
mints, resolved on every request against the live allowlist.

Terminal attach never appends the shared token to `/terminal` URLs. When a
shared-token hive needs a browser/WebSocket terminal open, the client first
posts to `/api/terminal/handoff` with the normal Authorization header and uses
the returned short-lived single-use `code` in the terminal URL.

`HIVE_DASHBOARD_COOKIE` is a **cookie header value**, not a bare session id —
the same string a browser would send:

```bash
export HIVE_DASHBOARD_COOKIE='hive_session=8f3c…'
hivectl tui
```

Several cookies are joined with `; `, which is what a hub-hosted hive needs
when its per-hive terminal assertion rides alongside the hub session. Taking a
header value rather than a named id is what lets one variable serve every
cookie-based deployment without the TUI having to know which kind of hive it
is talking to.

Normally, use [`hivectl login`](#login--logout--hold-a-per-user-session-from-the-terminal) rather than copying a browser cookie. The TUI loads the cached session for the URL in `HIVE_DASHBOARD_URL`; an explicitly exported `HIVE_DASHBOARD_COOKIE` takes precedence. Remove an obsolete override if a fresh login appears to have no effect.

For example, authenticate once, then launch against the same dashboard:

```bash
hivectl --server https://hive.example.com login
HIVE_DASHBOARD_URL=https://hive.example.com hivectl tui
```

Manual cookie headers remain supported for deployments that need additional cookies. Keep them private. The terminal login is implemented by [#5651](https://github.com/hivecommons/hive/issues/5651).

Both variables may be set at once, and both are sent. That is not redundancy:
a shared-token hive ignores the cookie and an allowlist hive ignores the token,
so setting both is how one exported environment works against every hive you
operate.

**If the credentials are rejected, `hivectl tui` refuses to start** rather than
opening a screen of empty panes. A `401` is a standing answer — no keypress in
the TUI can fix it — so it prints which variables to set and exits, leaving
your scrollback intact. Every other failure still opens normally: a hive that
is merely *down* is one of the main reasons to open the TUI, and the panes fill
themselves when it returns. A `403` also opens: that is a working session whose
role is too narrow for some reads, which the panes already handle individually.

#### The four panes

A responsive grid — Agents, Governor, Tokens and Events — refreshed from two
independent loops with different cadences. At 120×40 and larger it uses the
classic 2×2 layout; on wide-but-short terminals it shows the focused pane and
the next pane side by side; on narrow terminals it gives the focused pane the
whole grid area. Press `z` (or `enter`) to zoom the focused pane, and `z` or
`esc` to restore the grid.

| Pane | Source | Cadence |
|---|---|---|
| **Agents** | `GET /api/agents`, joined with live per-agent state pushed over SSE | reconciliation loop |
| **Governor** | `GET /api/status` (live mode/queue), `GET /api/config/governor` (eval interval) | reconciliation loop |
| **Tokens** | `GET /api/tokens` (counts, required) + `GET /api/cost` (estimate, optional) | activity loop, fixed |
| **Events** | `GET /api/audit` (newest-first operator/governor activity) | activity loop, fixed |

The **reconciliation loop** is what the SSE connection status affects: every
5 seconds while the stream is down or has not proven itself yet, stretching to
every 60 seconds once an event has actually been received on it (the stream
is doing the work at that point; the poll is just a reconciler catching a
roster change the stream does not announce, or a stream that stalled without
dropping the connection).

The **activity loop** — Tokens and Events — polls every 5 seconds
**unconditionally**, whether or not the SSE stream is connected. Nothing on
the stream carries token counts, estimated cost, or audit rows, so there is
no push event for those panes to wait on; tying them to the reconciliation
timer would make a *healthy* connection the reason they went stale. (This is
what `tui T32` / [#5421](https://github.com/hivecommons/hive/issues/5421)
fixed — earlier builds hung all seven reads off one timer, so a connected
stream paradoxically made the Tokens and Events panes twelve times staler.)

Both loops fetch once immediately on startup, so the frame fills in before
either interval elapses. A failed fetch never blanks a pane: the previous
successful frame stays on screen until the next successful read replaces it.
One field is the exception — a failed `/api/cost` read *clears* the cached
estimate rather than holding it, because a dollar figure attached to token
counts that have since moved by a fresh `/api/tokens` read is worse than an
honest `—`. Every write action below (pause/resume, model apply, kick,
ACMM apply, returning from a tmux attach) also triggers an immediate full
refresh of both loops, so the frame does not wait out an interval to show the
effect of the operator's own action.

`/api/audit` is the one poll-shaped read that needs owner (read-write) access;
a read-only token gets a 403 there like any other forbidden read, and it
travels the same swallowed-error path as a network failure — the Events pane
simply keeps its last successful snapshot rather than showing an error.

#### Keybindings

| Key | Action | Scope |
|---|---|---|
| `tab` / `shift+tab` | Cycle pane focus forward / backward | global |
| `z` / `enter` | Zoom the focused pane; `z` or `esc` restores the grid | global |
| `j` / `k`, `↓` / `↑` | Move the row selection | Agents, Events panes |
| `p` | Pause or resume the selected agent (opens a y/n confirm) | Agents pane |
| `m` | Open the model picker for the selected agent | Agents pane |
| `K` | Kick the selected agent now | Agents pane |
| `A` | Open the ACMM level overlay | global |
| `a` | Attach to the selected agent's tmux session (local, or via the dashboard's terminal proxy) | Agents pane |
| `H` | Open the Hives overlay — your contributor profiles, with `enter` to switch | global |
| `?` | Toggle the help overlay (lists this table; dismisses on any key) | global |
| `q` / `ctrl+c` | Quit | global |

Case is meaningful and deliberate: `K` (kick), `A` (ACMM) and `H` (hives) are
the actions with the widest blast radius bound to a bare key, so each sits on
the shifted member of a pair whose lowercase twin (`k` navigate, `a` attach,
`h` unbound) is pressed constantly during normal use — a missed shift never
fires the bigger action by accident. Actions apply to the selection in the
**focused** pane only; pressing `p`/`m`/`K`/`a` while a pane other than Agents
is focused is a no-op, never a guess at "the current agent". `A` and `H` are
the exceptions and are global: an ACMM level is a property of the whole hive,
and the hives list is not a property of this hive at all.

**Every overlay below is modal**: while one is open it consumes *every* key,
including `q`, `tab`, and the other action letters — closing or resolving the
overlay is the only way those reach the frame underneath again. The ACMM
overlay is the one exception to "letters are bindings": once its typed
confirmation is open, ordinary letters (including `p`, `a`, `A`, `K`) are
literal text being typed into the confirmation phrase, not actions.

#### Pause / resume

`p` on a selected agent opens a confirm dialog (`y` confirm, `n` / `esc`
cancel) rather than acting immediately. While the request is in flight the
dialog shows "Working…" and swallows further input; on failure it shows the
error in place and offers retry (`y`) or cancel. A 403 renders as `Pause
failed: owner access required` (or `Resume failed: …`) rather than the raw
HTTP error, since a forbidden pause/resume always means the same thing:
the token is not the hive owner's.

#### Model apply

`m` opens a picker for the selected agent's backend, fetching its model
catalogue asynchronously. The catalogue can carry two independent
qualifications, both shown as a note in the overlay when present:

- **Fallback** — endpoint discovery found nothing and the server substituted
  its static alias list; these ids are unverified guesses, not a confirmed
  catalogue.
- **Partial** — some of the backend's endpoints answered and others did not;
  a model's *absence* from the list proves nothing, since it may be served
  only by the endpoint that failed to answer.

`enter` applies the highlighted model. **Applying restarts the agent's
session** — the overlay states this before every apply — so the picker is not
a preview; choosing a model takes effect and interrupts in-flight work.
Applying is refused while a previous apply is still pending, so repeated
`enter` presses cannot queue a second restart. A 403 here renders as `Model
change failed: owner access required`.

#### Kick

`K` queues an immediate run for the selected agent and reports the outcome in
the footer rather than the pane: `kick queued for <agent>` on success, or
`kick already in flight for <agent> (request deduplicated)` if one was already
queued. A 403 renders as `Kick failed: owner access required`. Only one local
kick request is in flight at a time; `K` is a no-op while the previous one is
still pending.

#### ACMM apply

`A` opens the ACMM level overlay from anywhere — the level is a property of
the whole hive, not the focused pane's selection. It fetches the pack list
(the level definitions the server has configured) asynchronously; a 403 here
renders as `ACMM packs unavailable: owner access required`.

Selecting a level and pressing `enter` does **not** apply it directly — it
begins a **typed confirmation**: the overlay asks for the exact phrase
`APPLY L<n>`, and only an exact match arms the apply on the next `enter`.
Anything else typed is a no-op; `esc` during confirmation backs out to the
list rather than closing the overlay outright, so a mistyped phrase costs one
key. Selecting the level already in force skips confirmation entirely and
shows a "nothing to apply" message instead, since there is no write to
protect against.

On success the overlay **stays open**, now showing the reconciliation
receipt, until the operator dismisses it with `enter` or `esc` — it does not
flash past into a footer line, and a second `enter` cannot re-apply. On a
partial failure (the level persisted server-side but the fleet has not yet
reconciled to it — a 500 after the write took effect), the overlay still
shows an apply error, but the app also triggers an immediate refresh anyway,
because the panes underneath may already describe a hive that has moved.

#### Attach: local tmux, or the dashboard's terminal proxy

`a` on a selected agent attaches to that agent's tmux session. Where the
session actually is decides how ([#5644](https://github.com/hivecommons/hive/issues/5644)):

- **Local fast path.** When `HIVE_DASHBOARD_URL` is loopback (or unset) and a
  local `tmux has-session -t hive-<agent>` succeeds, the terminal is handed to
  `tmux attach` directly — the original, zero-network behavior.
- **Remote attach.** When the dashboard is not loopback, or the local session
  does not exist — the recommended Podman install, where the sessions live
  inside the container under other UIDs — the TUI dials the dashboard's own
  `/terminal` websocket instead: the same authenticated reverse proxy the web
  dashboard's "▶ terminal" links use, in front of the container's ttyd.
  Nothing new is exposed for this; ttyd's port stays loopback-only inside the
  container, and the attach carries the same credentials as every other TUI
  request (`HIVE_DASHBOARD_TOKEN` and/or `HIVE_DASHBOARD_COOKIE` — see
  [Credentials](#credentials)), plus ttyd's own basic-auth credential derived
  the way the container derives it (`hive:<token>`, overridable with
  `HIVE_TTYD_CREDENTIAL` if the deployment overrode it too).

The fallback is never silent: a remote attach prints one line before the
session's first byte saying which hive it went through and which session it
attached to, so there is no ambiguity about what you are typing into. That
line never contains the token.

Both paths preflight before suspending the TUI, so a missing `tmux` binary, a
session that does not exist anywhere, an unreachable dashboard, or a refused
authorization surfaces as a footer message (`Attach failed: …`) instead of a
redraw flicker — a 403 reads `owner access required` like the other actions,
a 401 names both credential variables. Inside a remote session every
keystroke, `ctrl+c` included, belongs to the agent's terminal; you leave with
tmux's own detach (`prefix + d`, i.e. `Ctrl-b d` by default). Returning from
the session (however it ends, on either path) restores the TUI and
immediately refreshes the fleet, since the agent's state may have moved while
attached. If the connection drops mid-session rather than closing cleanly,
the footer says so — your last keystrokes may not have arrived.

#### Hives: switching the hive you contribute to

`H` opens the **Hives** overlay from anywhere. It is the one view in the TUI
that is not a dashboard client: it lists the hives *this machine* can lend a
CLI to — the named profiles in `~/.config/hive/profiles.yml` — rather than
anything the hive on screen knows about, which is why it is global rather than
addressed at a pane. The rows are the ones
[`hivectl hives list`](#hives--named-profiles-for-the-hives-you-contribute-to)
prints, in the same order (active first, marked `*`), because the overlay calls
the same `pkg/hivectl` functions the CLI does — there is no second
implementation of the profiles file here.

| Key | Effect |
|---|---|
| `j` / `k`, `↓` / `↑` | Move the cursor |
| `enter` | Make the selected hive active — the same effect as `hivectl hives use` |
| `[` / `]` | Move the selected hive up or down in The Commons rank |
| `s` | Cycle the relay routing strategy: `ranked` → `spread` → `neediest` |
| `e` | Enable/disable new work for the selected hive, retaining its credentials |
| `a` | Add a hive: a two-field form (name, hub URL), then the registration POST |
| `d` | Remove the selected hive — asks you to type its name, as the CLI does |
| `r` | Rename the selected hive |
| `esc` | Close the overlay, or back out of an open form |

The **REACHABLE** column is the same probe `hivectl hives list --check` runs,
issued per distinct hub once the list is on screen. Nothing waits on it: the
rows render first and each hub's answer replaces `checking…` with `yes` or `no`
as it lands, so one dead hive costs the others nothing and cannot hold up the
overlay.

Opening the overlay migrates a legacy positional `contributor.env` exactly as
the first `hivectl hives` command would, and leaves that file byte-identical
until something actually changes. Every mutation then writes `profiles.yml`
first and regenerates `contributor.env` from it, so the relay keeps reading the
variables it always has, plus `HIVE_COMMONS_STRATEGY`. **`enter`, `[`/`]` and
`s` can move a running relay without a restart** — like the CLI, they rewrite
the projection and then signal the relay recorded in `contributor-relay.pid`
(or its docker/podman container) to reload it. Work already in flight stays with
the hub that assigned it; the next solicitation uses the new rank/strategy. If
no relay is running, the next relay start uses the saved Commons settings.

Two safety properties carry over from the CLI, unchanged:

- **Removal is gated on typing the hive's name**, because it discards a
  registration token the hub cannot reprint. Anything else typed is a no-op.
- **Registration tokens are never rendered.** The overlay's row type has no
  field for one; what reaches the frame is the name, hub, contributor id,
  session label and probe result.

Like every other overlay it is modal — and more strictly, because its add and
rename screens are **text fields**: while one is composing, ordinary letters
(including `q`, `p`, `a` and `K`) are characters being typed, not bindings.

#### Connection status and the poll fallback

The header's `ws:` field is `connected` only once an event has actually been
**received** on the SSE stream — not merely dialled, since a successful
subscribe hands back its channels before the request is even confirmed.
Everything else — before the first event, during a reconnect's backoff, and
right after a drop — reads `not connected`, because all three mean the same
thing to an operator: the numbers on screen are coming from the fallback poll,
not the stream. `hive:` (identity) and `governor:` (mode) never blank on a
drop; only `ws:` changes, so a flapping connection does not make the header
look like the hive itself went away.

On a drop, reconnection backs off starting at 1 second and doubling to a
30-second cap, and the reconciliation loop's cadence drops back from 60s to 5s
immediately along with an out-of-cycle fetch, so the fallback's first data
does not wait out a whole interval. The activity loop (Tokens, Events) is
unaffected by any of this — see [The four panes](#the-four-panes) above.

#### Terminal size, help, resize, and quit

The grid needs at least **60×20** to stay readable; below that the frame is
replaced by a single centred `terminal too small (need at least 60x20)`
message rather than shrinking the panes into unreadable slivers. Resizing is
automatic — the layout re-derives itself from the terminal size on every
render, with no keybinding involved. `?` toggles a help overlay listing every
binding above; it dismisses on any keypress. Colors are chosen for contrast
against both light and dark terminal backgrounds, so the focused-pane border
and header stay legible either way. `q` or `ctrl+c` quits **from the top level
only** — while any overlay is open, `q` is swallowed like every other key, so
an operator dismisses the overlay first (see the modal rule above).

#### v1 boundaries

- **Login happens before launch, not inside a pane.** Run `hivectl login`
  against the target URL; the TUI uses its cached session unless
  `HIVE_DASHBOARD_COOKIE` is explicitly set. Shared-token deployments still
  need `HIVE_DASHBOARD_TOKEN`. See [Credentials](#credentials) and
  [#5651](https://github.com/hivecommons/hive/issues/5651).
- **No terminal emulation in-frame.** Remote attach
  ([#5644](https://github.com/hivecommons/hive/issues/5644)) suspends the TUI
  and streams the ttyd websocket through your own terminal, exactly as the
  local path suspends into `tmux attach`; the session is not re-rendered
  inside a pane.
- **Feature parity with the web dashboard's operator loop, not visual
  parity.** The TUI does not attempt to look like the web dashboard.

#### Seeing it run

Everything above is recorded as a VHS tape at
[`src/docs/design/tui.tape`](design/tui.tape) — launch, the four populated
panes, focus and navigation, the help overlay, and each write action in its
safe state. Render it with [VHS](https://github.com/charmbracelet/vhs)
(`brew install vhs`) and a Go toolchain; it needs no Docker, no network and no
cluster:

```bash
cd src
go build -o bin/hivectl ./cmd/hivectl
go run ./pkg/tui/testdata/demohive &     # loopback fixture; kill %1 to stop
vhs docs/design/tui.tape                 # writes docs/design/_out/tui.gif
```

The tape records against `src/pkg/tui/testdata/demohive`, a single-file,
stdlib-only, loopback-only fixture that serves fixed literals for the routes
the TUI reads — so the recording never captures a real hive's identity, agent
names or spend, and renders identically every time. The GIF is a build product
and is gitignored; the tape is the committed source of truth.

The tape deliberately does **not** complete a model apply or an ACMM apply (it
drives each to its confirmation and cancels), and omits local tmux attach and
SSE degradation, both of which need state the fixture cannot produce
deterministically. Each omission is explained in a comment in the tape itself.

Track any further work under the `hive tui` epic
([#4907](https://github.com/hivecommons/hive/issues/4907)).

### enroll — spoke-based lite repo enrollment

```bash
hivectl enroll OWNER/REPO
hivectl enroll OWNER/REPO --acmm-level 1
hivectl enroll OWNER/REPO --installation-id 123456
```

`enroll` verifies local `gh` authentication and repository access, then asks the
hub to place the repo on a spoke. Existing owned spokes receive the repo through
the heartbeat project-config callback; otherwise the hub provisions a hosted
lite spoke. Lite mode is advisory-only and capped at ACMM L2. It writes no
repository PAT or long-lived secret.

### contribute — registration token recovery and operator controls

`hivectl contribute login [profile]` rotates and saves a registration token using
GitHub identity proof; omitting the profile uses the active saved hive. Operator
commands `hivectl contribute tokens list`, `tokens revoke <contributor>`, and
`tokens revoke --all --yes` inspect ages and invalidate credentials without changing
trust tiers. See [token expiry and incident response](contributor-token-policy.md)
for policy settings, API equivalents, migration and relay restart instructions.

### hives — named profiles for the hives you contribute to

`hives` manages your local contributor profiles: one saved identity and credential per Hive. It does not use a dashboard API client, so `--server` and `--token-env` do not select a profile. Discovery and registration can still contact a hub.

#### First use: inspect, choose and check

On a new contributor machine, complete `just contribute-setup <backend>` first. `hives add` performs only registration; it does not replace GitHub login or the backend CLI preflight.

```bash
hivectl hives list
hivectl hives list --check -o json
hivectl hives use acme
hivectl hives repos acme
```

Use the exact saved name from `list` in place of `acme`. The active Hive is marked `*`. `use` saves the choice and signals a managed running relay; the current task keeps its original Hive and lease, and the next solicitation uses the new choice. If no managed relay is found, the change applies on next start. A saved change or reload signal is not remote acknowledgement.

| Your task | Next step |
| --- | --- |
| Add one Hive | `hivectl hives add acme --hub URL`; for an identity already registered elsewhere, use the [recovery tasks](#contributor-tasks-and-recovery). |
| Pause new work without losing credentials | `hivectl hives disable acme`; later `enable acme`. Selecting a disabled profile does not re-enable it. |
| Limit repositories | [Editing repository selections](#editing-repository-selections); saved consent is not proof of runtime enforcement on old relays. |
| Review changes visually | `hivectl tui --hives` or `hivectl hives web`; stage changes, review the diff, then Apply. |
| Inspect without writes or discovery | [Contribution tree](#inspecting-the-contribution-tree-read-only). |
| Track or enroll a hub's advertised Hives | [Tracking hubs](#tracking-hubs), then the [adoption/refresh reference](#hub-adoption-and-refresh-reference). |
| Understand files, projections or adapter APIs | [Storage and adapter reference](#contributor-storage-and-adapter-reference). |

#### Contributor command reference

The commands below are independent examples, not a script to run from top to bottom. In particular, reissue rotates a credential and remove discards one.

```bash
hivectl hives list                                            # active hive marked *
hivectl hives list --check -o json                            # also probe each hub
hivectl hives add acme --hub wss://acme.hive.hivecommons.dev/contribute
hivectl hives reissue acme                                    # rotate one saved profile
hivectl hives reissue acme --hub wss://acme.hive.hivecommons.dev/contribute
hivectl hives use acme
hivectl hives export acme --out acme.hive-profile
hivectl hives import acme.hive-profile --name acme-laptop
hivectl hives session acme --label review
hivectl hives move acme up                                    # rank order
hivectl hives strategy spread                                 # ranked|spread|neediest
hivectl hives disable acme                                    # pause new work, keep credentials
hivectl hives enable acme                                     # resume without registering again
hivectl hives web                                             # loopback web UI for The Commons
hivectl hives rename acme acme-prod
hivectl hives remove acme                                     # confirm, or --yes
hivectl hives hub add commons --url https://hub.example.org   # track a hub
hivectl hives hub show commons                                # advertised Hives, onboarded or not
hivectl hives hub adopt commons --follow current --yes         # review and enroll its Hives
hivectl hives hub follow commons --mode current-and-future-repos # future enrollment policy only
hivectl hives hub refresh commons --yes                        # refresh additions/removals
hivectl hives hub remove commons                              # keep enrolled Hive profiles
```

#### Hub adoption and refresh reference

`hivectl hives hub show NAME` is read-only discovery from `GET HUB/api/hives`.
`hub adopt NAME` reviews every advertised Hive, reuses matching profiles by
normalized HTTP origin, and registers only new Hives after confirmation. Use
`--hive URL` more than once to narrow the review; a narrowed adoption merges
the selected Hives into `hubs[].reviewed` and keeps every Hive reviewed by an
earlier run, so a later `hub refresh` never re-enrolls a Hive you reviewed and
then removed with `hives remove`. `--follow off` leaves a new
profile without a repository selection; `--follow current` snapshots
`GET /api/contribute/repositories`; `--follow current-and-future-repos` also
records permission to follow future repository additions. Without `--yes`,
enrollment requires a terminal and an explicit `y`/`yes`; EOF, a pipe, or any
other answer saves nothing and makes no registration request.

`hub follow NAME --mode MODE` changes only the saved policy. It never changes
current repository membership. `hub refresh [NAME]` compares the current
advertisement with `hubs[].reviewed`, reports newly advertised and
`no longer advertised` Hives, and retains removed profiles. Follow policies
other than `off` enroll new Hives automatically; `off` requires confirmation.
Successful registrations are committed even when another Hive is unavailable
or its registration fails, so a later adopt or refresh retries only what is
still missing.

Discovery/onboarding helpers resolve Hive identity by the HTTP origin of its
hub URL: `wss` maps to `https`, `ws` to `http`, hosts are lower-case, default
ports are omitted, and paths do not distinguish Hives. Overlapping discovery
paths reuse the saved endpoint, contributor ID, token and personal consent.
Unrelated origins require separate credentials. Deliberate legacy profiles
with different contributor IDs at the same origin remain separate; onboarding
must ask which identity to use instead of silently merging them.

### Inspecting the contribution tree (read-only)

```bash
hivectl hives tree
hivectl hives tree -o json
hivectl hives tree -o yaml
```

Reads saved `profiles.yml` without migration, network requests, registration,
projection writes, or relay signals. Subscriptions currently appear under
**Direct Hives**: wiring persisted hub discovery into this command follows the
hub-entry work in #13174 / #13189. The shared `BuildContributionTree` read model
already accepts hub and repository discovery snapshots for TUI/Web adapters;
shared hub appearances reference one subscription and rollups count it once.
Separate profiles at one origin are never merged.

Selection, follow policy, readiness, and activity are separate. Without current
discovery the tree says **Membership incomplete**, never **All current**.
Absent repository selections are **Unrestricted — legacy**, not selected empty
snapshots. Explicitly empty consent remains off. Cross-profile repository
exclusions list their source subscription names and do not narrow legacy
profiles. Disabled subscriptions retain saved selections and follow policy.
Runtime readiness is unknown (or disabled); activity is unavailable, not zero.
No credentials or contributor identities are included in JSON/YAML output.
Selection source IDs are selection-level provenance, not repository ownership.
Profile-name keys identify the current snapshot only, not durable history.

### Editing repository selections

`hivectl hives repos <hive>` displays saved repository consent and each entry's
source (reviewed membership, snapshot, preset, or personal override). It never
prints registration tokens. JSON/YAML output includes excluded entries with
`selected: false` and distinguishes legacy unrestricted consent from an empty
selection.

```bash
hivectl hives repos acme
hivectl hives repos acme --select all-current  # review today's eligible membership
hivectl hives repos acme --exclude org/repo
hivectl hives repos acme --include other/project
hivectl hives repos acme --toggle-exclude org/repo
hivectl hives repos acme --clear org/repo      # clear both personal overrides
hivectl hives repos acme --deselect-current    # clear current consent, keep follow policy
hivectl hives repos acme --select none         # select nothing and stop following
hivectl hives repos acme --follow-new-repos     # allow additions on supported refresh
hivectl hives repos acme --follow-new-repos=false # stop following, keep current consent
hivectl hives repos acme --reset --yes         # restore legacy unrestricted consent
```

#### First use: restrict a legacy profile

1. Run `hivectl hives repos acme` to inspect saved consent.
2. Review today's eligible membership with `hivectl hives repos acme --select all-current`.
3. If needed, exclude a repository with `hivectl hives repos acme --exclude org/repo`.
4. Read the selection again and check the relay receipt. The scope should say `N repos` or `none`, not legacy `all`.

An unreviewed “all except one” wildcard is not supported. `all-current` must first turn legacy unrestricted consent into an explicit snapshot. Discovery errors, including an older Hive missing the endpoint, leave the saved selection unchanged.

#### Selection-operation reference

Use only one selection-edit operation per invocation:

| Operation | Membership effect | What it preserves |
| --- | --- | --- |
| `--select all-current` | Snapshots public `GET /api/contribute/repositories`; does not follow future additions. | Personal additions and exclusions; exclusions always win, even over an explicit inclusion. |
| `--select none` | Clears membership/additions and turns following off. | Exclusions, so later snapshots do not silently restore excluded work. |
| `--deselect-current` | Clears reviewed membership (including an all-current snapshot) and personal additions. No discovery or registration. | Accepted follow policy, exclusions, sources and preset review history. A later approved refresh may select repositories again. Refuses legacy unrestricted profiles. |
| `--follow-new-repos` / `--follow-new-repos=false` | Changes refresh permission only; neither fetches nor adds membership. Stopping following removes no current consent. | Current membership and exclusions; neither converts legacy unrestricted profiles. |
| `--reset --yes` | Drops the whole selection and restores **legacy unrestricted** consent. This is not a “select nothing” action. | The profile's identity/credential, not its repository restriction. |

Following uses the applicable preset/discovery refresh source, not a background poller or wildcard. To stop current work selection *and* following, choose `--select none`; to leave accepted following in place, use `--deselect-current` on an already scoped profile.

#### Repository IDs, receipts and compatibility

Repository arguments accept `owner/name` (defaults: `--forge github --host
github.com`) or canonical `forge/host/owner/name` IDs. Bare names are refused;
IDs are normalized to lower case. GitLab nested namespaces are supported.
`hives list` and the Hives overlay show the saved effective scope as `all`,
`N repos`, or `none`. In the overlay, **x** opens a repository exclusion toggle;
enter an ID (or GitHub `owner/name`) and press enter. This is the same operation
as `--toggle-exclude`, preserving personal additions when an exclusion is lifted.
Initialize snapshots or reset consent using the CLI.

Every selection write uses `ProfileStore.Commit` and then the existing relay
signal path. Adding/removing a Hive also signals the relay, including from the
TUI. If no relay is running, the receipt says the change applies on next start.
An already executing task keeps its original Hive credentials and lease.

**Compatibility:** these commands edit local consent and its generated
projection, not operator admission policy. Runtime scope negotiation,
fail-closed enforcement, and remote `unsupported` telemetry are companion work
in #12805 and #12806. A relay that does not understand `HIVE_REPO_SCOPE` cannot
enforce a saved repository selection; do not rely on this editor alone as an
execution boundary. Upgrade the Hive and relay together before relying on live
repository filtering. The scope column describes saved consent, not an
acknowledgement from the remote Hive.

### Contributor tasks and recovery

#### The same list is in the TUI

`just contribute-tui` (or `hivectl tui --hives`) opens the
[Hives overlay](#hives-switching-the-hive-you-contribute-to): the same rows,
with `enter` to switch, `[`/`]` to rank, `s` to cycle the strategy, `e` to enable/disable and
`a`/`d`/`r` to add, remove and rename. It calls these same functions, so
either surface leaves the files in the same state.

#### The contribution tree is in the TUI too

In the Hives overlay, `t` opens the
same Hub → Hive → Repository tree as `hivectl hives web`, with the same staged
edits, diff and Saved / Relay reload / Acknowledged receipt. `space` selects,
`x` excludes or clears an exclusion, `f` follows, `e` enables, `R` reviews and
restricts a legacy profile, `/` searches (keeping ancestry), `A` opens the bounded
change review, `D` discards and `ctrl+r` refreshes without losing staged edits. Hub
and Hive actions show affected counts before staging. In review, `↑`/`↓` or
`j`/`k` scroll wrapped rows; `PageUp`/`PageDown` page and `Home`/`End` jump.
Full target identities wrap without truncation. `Enter` explicitly applies;
`Esc` returns with edits kept. The review pins the title, scope and key hints,
scrolls only the diff, adapts to resize and pauses Apply
below 40 columns or 12 rows until its confirmation context fits again.
Nothing is written until Apply; a review whose profiles changed elsewhere must
be discarded and reviewed again, and leaving or quitting with unapplied edits asks first.

#### Disable instead of removing a hive to pause it

`hivectl hives disable <name>`
(or `just contribute-hives disable <name>`) preserves its URL, registration token,
contributor ID and rank. `enable <name>` restores eligibility without registration
or token rotation. Existing profiles default to enabled. Disabled profiles remain
visible in the CLI/TUI but are excluded from new work under **every** strategy;
selecting one as active does not re-enable it. Disable every other hive to focus
exclusively on one, even when it has no jobs. Changes signal the running relay
to reload immediately, without interrupting work on its original hive. With all
profiles disabled, the relay reports why it is idle and waits for an enable.
The generated `HIVE_HUB_DISABLED` boolean list follows the same profile ordering
as the hub/token/contributor-ID lists; disabled entries are not removed from
any credential list. Upgrade the relay together with `hivectl`: older relays
do not understand this local routing control.

#### The Commons strategies choose only between tasks

`ranked` (the default)
asks the highest-ranked hive first and falls through only when it reports no
work; `spread` uses rank-weighted rotation with occasional mixing so lower
ranked hives still receive some of your daily allotment; `neediest` polls
each hub's `/api/contribute/status` and prefers queued actionable work,
lightly boosted by idle contributor capacity when the hub reports
`active_contributors` and `total_registered`. The existing contributor quota
guard still runs before an offered task is accepted, so a local
daily/subscription cap can hold the relay regardless of which hive The
Commons picked.

#### The Commons web UI is local-only

`hivectl hives web` prints a
tokenized `http://127.0.0.1:<port>/` URL, serves only on loopback, and backs
the page with the same `profiles.yml` / generated `contributor.env` store as
the CLI and TUI. Its Hub → Hive → Repository tree reads saved consent without
migrating legacy profiles. **Refresh membership** discovers public membership
without registering or enrolling anything; failed discovery remains incomplete.
Selection, exclusions, follow rules, enable/disable and **Review and restrict**
are staged through the shared contribution editor. **Apply** commits the reviewed
diff; **Discard** drops unsaved edits. Shared branches and cross-Hive exclusions
are named in the diff. An empty explicit selection stays idle; legacy scope
remains labelled **Unrestricted — legacy** until explicitly restricted.
**Saved**, **Relay reload**, and **Acknowledged** are separate receipt fields.
A partial projection failure requires retrying Apply; a successful reload signal
is not acknowledgement. Readiness and activity remain **unavailable** until
evidence is integrated. Existing subscription, rank and strategy controls save
separately after an explicit Apply; pending tree edits must be applied or
discarded first.
Arrow keys expand/collapse and move through the tree, Space stages selection,
and Tab reaches search, row actions, Apply and Discard. Search keeps ancestry;
refreshing a staged review preserves edits and focus. Back, in-page navigation
and **Leave page** use a styled discard dialog. Browser toolbar reload and tab
close cannot show a custom dialog and abandon unsaved browser reviews; there is
no native confirmation prompt. Reviews are process-local, bounded to 64 tabs,
and abandoned reviews can expire after eight hours.
The API and HTML never return registration tokens, provider credentials or
GitHub tokens. API calls require the local token header and valid loopback
Host/same-origin requests. The printed URL carries a process-lifetime local UI
token and should not be shared; the page removes it from browser history.
No third-party assets are loaded.

#### Concurrent edits are never silently lost

The CLI, the TUI and the web
UI all commit `profiles.yml` under a shared owner-only lock file
(`profiles.yml.lock`) and only if the file is still the revision they loaded.
If another surface changed it in the meantime, nothing is written: the CLI
and TUI ask you to re-run or retry, and the web UI answers `409` asking you
to reload and review the current hives.

#### `use`, `move` and `strategy` switch a running relay

They regenerate the
projection, then signal the relay advertised in `contributor-relay.pid` (or
its recorded docker/podman container) to reload `contributor.env`. The task
currently in flight finishes on the hub that assigned it; the next
solicitation goes to the newly selected hive. If no relay is running, the next
relay start uses the saved Commons settings.

#### Last seen is local relay state

`hives list` reads
`~/.config/hive/hubs-seen.json`, written by the relay after `auth_ok` and
successful heartbeats at most once a minute. A `-` means this machine has not
seen that hub since the feature landed or since the file was removed.

#### `add` is the registration half of `contribute-setup` only

It POSTs to
`<hub>/api/contribute/register` and appends the result; it does not run the
`gh` login or the backend CLI preflight, so a first-time machine still wants
`just contribute-setup <backend>`. No bearer credential is sent to the hub.

#### Already registered elsewhere?

The register endpoint is unauthenticated,
so the hub will never hand an existing contributor's token back. On a
profile-store machine, rotate and save only that one profile with
`hivectl hives reissue acme --hub <url>` (or omit `--hub` when `acme`
already exists locally). If you want to keep the old machine working, use
`hivectl hives export` on the holding machine and `hivectl hives import` on
the new one. Add the credential you already hold instead only when you have
the token and contributor id in hand:
`printf '%s' "$TOKEN" | hivectl hives add acme --hub <url> --token-stdin --contributor-id <id>`,
`just contribute-move` is unsafe with profiles because it rewrites
`contributor.env` behind `profiles.yml`; the next profile-store command
regenerates `contributor.env` from `profiles.yml`.

#### `reissue` rotates exactly one hive token

It calls the hub's
authenticated `/api/contribute/reissue-token` endpoint using your local `gh`
token, replaces only the named profile's registration token in
`profiles.yml`, and regenerates `contributor.env` through the profile store.
If the hub says this GitHub account is not registered, run
`hivectl hives add <name> --hub <url>` instead.

#### Moving a profile to another machine:

`hivectl hives export <name> --out
<file>` writes one passphrase-encrypted bundle. On the other machine,
`hivectl hives import <file> [--name <new-name>]` decrypts, validates and
appends it. The registration token is never printed unless you already hold
it and explicitly feed it to `add --token-stdin`.

#### Two sessions on one hive:

`hivectl hives session <name> --label <label>
[--name <new-name>]` copies a profile and sets `session`, so the projection
carries `HIVE_SESSION=<label>` only when that copied profile is active.

#### Registration tokens are never printed

, in any `-o` format.

#### `remove` discards a token the hub cannot reprint

, so it asks you to type
the hive name unless `--yes` is given.

#### Staged contribution edits and Apply receipts

In the TUI and Commons web tree, stage changes first, review the bounded diff, then choose **Apply**. Opening, editing, reviewing or discarding an unsaved review does not write profiles or signal the relay. The integrated surfaces use the same staged editor, so their receipts have the same meaning.

Repository selection, exclusions, following, and subscription enablement are
separate operations:

- **Select current / Deselect current** affects the entire named Hive or hub
  branch, regardless of search filters. Selection retains saved targets absent
  from discovery and the approved refresh-source mode. Deselect current preserves
  follow policy and exclusions; **Select none and stop following** remains the distinct CLI
  `--select none` operation. Inclusion requires complete discovery. Saved
  selections remain individually editable during an outage.
- **Exclude / Clear exclusion** changes only the personal veto. Clearing one
  preserves saved personal inclusions and does not approve an unselected target.
  Parent selection and enable/disable operations never erase exclusions.
- **Hive on/off** retains credentials, saved membership, and following. **Hub
  off** disables its shared subscriptions and turns off that hub's future-Hive
  enrollment. Re-enabling the hub does not resume future enrollment implicitly.
- **Review and restrict** explicitly converts a legacy unrestricted profile to
  the complete discovered snapshot, with following off. Failed discovery leaves
  it unchanged. Ordinary repository and branch-selection actions cannot convert
  legacy scope; disabling a legacy subscription does not convert it either.

The reviewed diff names selected/deselected subscription–repository pairs and counts them
once per subscription, even when the subscription appears under multiple hubs.
It separately records exclusions, follow policies, enablement, legacy
conversions, and the linked hub appearances. Selection changes masked by a veto
remain visible as saved-membership changes. Exclusion effects list the remaining
veto sources, every scoped subscription with affected saved membership
(including disabled ones), and enabled legacy unrestricted profiles that can
still receive the repository. This reflects the existing cross-profile exclusion
union; it does not change its semantics.

Apply returns three independent status fields, plus the saved scope generation:

| Field | Meaning |
| --- | --- |
| `Saved` | `profiles-and-projection-saved`, `profiles-saved/projection-failed`, `not-saved`, or `unchanged` for an empty diff. |
| `RelayReload` | `notified`, `pending-failed`, `no-managed-relay-found — saved for next start`, or `not-requested` when no complete save occurred. |
| `Acknowledged` | `pending` after a complete save; otherwise `unknown`. Notification alone never proves adoption. |

An empty diff writes and signals nothing. A projection failure reports that
profiles were already saved, keeps the reviewed diff, and allows an explicit
Apply retry against that saved revision. Further edits and Discard are refused
until projection repair completes: Discard cannot undo a save that happened.
A concurrent edit marks the diff stale and requires
reloading profiles and reviewing a new session; neither retry nor Discard can
silently merge or approve the old edits. Successful persistence consumes the diff
even when notification fails; the saved selection governs the next relay start.

No managed PID record is not proof that no relay exists. Until matching relay
status evidence is available, show **Change saved — relay adoption pending**;
do not claim that leasing stopped or an in-flight task was cancelled. Diffs and
receipts contain no registration tokens, provider/GitHub credentials, raw relay
command output, or credential-bearing endpoint queries.

#### Contribution presets

A contribution preset is a plain, credential-free document a project publishes
at a public HTTPS URL (or ships as a local file) to recommend Hives and
repositories to contribute to. It is a recommendation only: it grants nothing,
and each Hive's operator admission gates (disabled repos, holds, trust tiers,
model filters, quota) stay authoritative. It is unrelated to the encrypted
`hives export` bundle, which carries a registration token.

```yaml
version: 1                                   # integer; any other major is refused
name: Bluefin ecosystem
description: Hives Bluefin depends on        # optional, display only
revision: "2026-10-17"                       # optional, opaque publisher revision
hives:
  - url: https://bluefin-hive.example
    description: Why we recommend this Hive  # optional
    repositories: all-current                # or a list:
  - url: https://dependency-hive.example
    repositories:
      - repo: dependency-org/component-a     # full owner/name (GitLab may nest groups)
        forge: github                        # optional: github (default), gitlab, gitea
        host: github.com                     # optional: github.com / gitlab.com; required for gitea
        description: optional
hubs:                                        # optional
  - url: https://hub.example
    hives: all-current                       # or a list of Hive URLs advertised by the hub
```

`all-current` selects the current membership only; there is no `all` or
`follow` keyword, because following future additions is a personal setting,
not something a publisher can switch on. Repositories are identified by
`{forge, host, owner/name}` compared case-insensitively, and duplicates are
collapsed.

`hivectl` loads a preset from an `https://` URL or a local path and writes
nothing. It refuses:

- any scheme other than `https://` (including `file://`, `ws://`, `wss://`);
  `http://` only for loopback hosts, and URLs with embedded credentials;
- redirects that leave HTTPS; fetches send no credentials or cookies and time
  out after 30 seconds;
- documents larger than 256 KiB or with more than 1024 entries, multiple YAML
  documents, and YAML aliases;
- an unknown or missing `version`;
- any unknown field. Fields that carry credentials, shell commands, local
  execution settings or permission changes (`token`, `registration_token`,
  `*_api_key`, `secret`, `password`, `env`, `command`, `script`, `hooks`,
  `backend`, `model`, `session`, `permissions`, `allowed_tools`, …) are named
  in the error;
- repository entries that are not a full `owner/name` (display names are never
  matched) or that name an unknown forge.

##### Adopting and refreshing a preset

```bash
hivectl hives preset adopt https://bluefin.example/hive-preset.yml   # review, then confirm
hivectl hives preset adopt ./preset.yml --hive acme --follow-new-repos --yes
hivectl hives preset adopt ./preset.yml --onboard  # review destinations, then enroll missing Hives
hivectl hives preset refresh                     # re-fetch recorded presets and review changes
hivectl hives preset refresh --yes               # accept new recommendations
hivectl hives preset refresh --accept-removals   # also drop what the preset no longer recommends
```

Without `--onboard`, `adopt` matches each preset Hive to an existing profile by normalized origin. It
never registers or copies credentials: a Hive without a profile is reported as
`not onboarded, skipped` (onboard it with `hives add` and adopt again). For each
matched Hive it shows the review diff against your saved selection:

- `+` recommendations that become selected once you confirm (`--yes` skips the
  prompt). `all-current` snapshots the Hive's currently eligible repositories
  through the same discovery as `hives repos --select all-current`.
- `x` recommendations withheld by a personal exclusion. Exclusions always win
  and are never cleared by a preset.
- `-?` saved repositories the preset does not recommend. `adopt` keeps them.

Confirming records the accepted membership, the preset as a selection source,
and its provenance (`presets:` in `profiles.yml`: id, source, reviewed revision
and time) through the same save, `contributor.env` projection and relay reload
as `hives repos`. The reviewed revision is the publisher's `revision` plus a
digest of the document. Re-adopting a revision with nothing new is a no-op and
does not advance the scope generation. A recommendation of no repositories
saves an explicit empty selection, which idles that Hive; it never becomes
unrestricted. Without `--onboard`, hub entries are listed but not adopted.

**One-flow enrollment.** `adopt --onboard` also works before any profiles exist.
It fetches preset hubs' public `GET /api/hives` registries and snapshots the
selected advertised Hives (`hives: all-current` or an explicit list). It does
not recursively discover hubs. Duplicate origins share one profile and one
registration; an explicit direct Hive recommendation takes precedence over a
hub's whole-Hive recommendation. Existing credentials, disabled state, personal
exclusions, backend/provider authentication and quota settings are preserved.

Before registration, it shows the preset source, hub sources, effective Hive
URLs, repository diff and follow policy. Confirmation is mandatory (`--yes`
cannot be combined with `--onboard`); declining or reaching EOF without approval
leaves both profile files and registration state unchanged, including legacy
configuration. Browse/discovery requests may already have occurred. Missing
profiles receive unique generated names and reuse `hives add`'s registration
flow, using the current `gh` login or `--github-user LOGIN`. New profiles use
WebSocket contributor URLs; saved credentials never go to the preset or hub
publisher. Discovery refuses redirects and malformed/oversized hub responses.

An unavailable Hive or one without repository-scope discovery is not enrolled.
Registration failures are reported as pending and cause a nonzero exit; ready
profiles and accepted scope are saved, so retrying the same command does not
repeat successful registrations. Failed targets are not projected into the
relay; rerun `adopt --onboard` to resolve them. An existing remote registration
that returns no token still requires the supported `hives reissue` or import
flow—onboarding never retrieves credentials from a directory hub.

Hub membership is a current snapshot, not permission to enroll future Hives.
`refresh` can review repository changes at already-adopted hub members, but
never enrolls newly advertised Hives. Run `adopt --onboard` again to review those.
`--follow-new-repos` affects only reviewed Hive repository selections, not hub
membership. The command reloads an existing relay once after saving; it does
not start another relay or launch contribution work itself.

`refresh` re-fetches every recorded preset (or only `--hive <name>`) and diffs
it against the saved selection:

- **Additions** are applied automatically only at Hives that follow new
  repositories; elsewhere they are shown as `+?` and need confirmation (`--yes`
  or answering the prompt).
- **Removals** are shown as `-?` and kept unless you confirm them at the prompt
  or pass `--accept-removals`. `--yes` alone never removes anything.
- **Outages**: if the preset cannot be fetched or a Hive's discovery fails, that
  Hive is reported as `discovery unavailable`, its saved selection is left
  untouched, nothing is broadened, and the command exits non-zero.
- **Hubs**: for every recorded preset that lists hubs, including presets adopted
  without `--onboard`, `refresh` re-reads each hub's public `GET /api/hives`
  registry to review repository changes at hub members you already have
  profiles for. This is a network call to the hub with no credentials attached.
  An unreachable or malformed hub registry is reported as
  `hub discovery unavailable`; the refresh is then incomplete, the reviewed
  revision does not advance, and the command exits non-zero. Hub members
  without a profile are never enrolled by `refresh`.

The reviewed revision only advances when every Hive of the preset was reviewed
and nothing is left pending.

**Follow policy.** `--follow-new-repos` on `adopt` (off by default; pass
`--follow-new-repos=false` to turn it off) stores `follow_new_repositories` on
each adopted Hive's selection. It lets `refresh` include that Hive's new
recommendations without a prompt. Personal exclusions still take precedence,
and removals still need confirmation.

#### Tracking hubs

A hub runs a federation registry (`GET /api/hives`) that lists the Hives
associated with it. `hivectl hives hub` records hubs in the `hubs:` key of
`profiles.yml` and reads that registry, so you can see which Hives a hub
advertises without editing YAML:

```bash
hivectl hives hub add commons --url https://hub.example.org
hivectl hives hub list                 # -o json|yaml for scripts
hivectl hives hub show commons         # each advertised Hive, onboarded or not
hivectl hives hub remove commons       # stop tracking; Hive profiles are kept
```

**Adding or showing a tracked hub is discovery only.** The hub entry stores no credentials; those operations do not call `/api/contribute/register` or change profile membership. To enroll advertised Hives, use `hub adopt`; use `hub refresh` later to apply the saved follow policy to newly advertised Hives. You can also onboard one explicitly with `hivectl hives add`.

- `add` rejects a name already in use (case-insensitively) and a URL whose
  normalized origin is already tracked under another name. New hubs start with
  `follow: off`. `hub follow --mode current` or `current-and-future-repos` saves permission for automatic enrollment on a later `hub refresh`; it does not itself fetch or enroll anything. With `off`, refresh still needs confirmation (or `--yes`). These are command-driven refreshes, not a background subscription.
- `show` keys each advertised Hive by the normalized origin of its `hub_url`
  (the same identity rule as above) and marks it `onboarded as <profile>` when a
  saved profile has that origin, otherwise `not onboarded`. Entries without a
  usable `hub_url` are skipped, and a repeated origin is listed once.
- Discovery sends no token, does not follow redirects, and caps the response
  size. A redirect, a non-200 status, an unreachable hub, or a malformed body
  reports the hub as **unavailable** and exits non-zero. It is never shown as
  a hub with no Hives.
- A `profiles.yml` without `hubs:` loads and saves exactly as before.

See the [adoption and refresh reference](#hub-adoption-and-refresh-reference) for repository snapshots, partial failures and how previously reviewed Hives are retained.

### Contributor storage and adapter reference

These are serialized formats and implementation contracts, not setup steps. Use the contributor tasks above to change consent; do not edit the generated environment file behind the profile store.

#### Profile schema and identity

Profiles live in
`~/.config/hive/profiles.yml` (mode 0600), one named entry per hive:

```yaml
version: 1
active: acme
commons_strategy: ranked
profiles:
    - name: acme
      hub: wss://acme.hive.hivecommons.dev/contribute
      contributor_id: contrib_a1b2
      registration_token: <secret>
      session: review          # optional: HIVE_SESSION for this profile
      added_at: 2026-09-21T10:00:00Z
      selection:               # optional: absent preserves legacy unrestricted scope
        mode: selected         # selected | all-current
        repositories: [github/github.com/acme/component]
        include: [github/github.com/acme/personal-addition]
        exclude: [github/github.com/acme/excluded]
        membership_snapshot: [] # accepted membership for all-current
        follow_new_repositories: false
        sources: [ecosystem]
presets:                       # optional local review history, not an export
    - id: ecosystem
      source: https://ecosystem.example/hive-preset.yaml
      reviewed_revision: "2026-10-17"
      accepted_at: 2026-10-17T10:00:00Z
hubs:                          # optional: tracked hubs, no credentials
    - name: commons
      url: https://hub.example.org
      follow: "off"            # off | current | current-and-future-repos
      reviewed: []              # normalized Hive origins from the last review
```

Repository identities are lower-case `<forge>/<host>/<owner>/<name>` (forge:
`github`, `gitlab`, or `gitea`; nested GitLab namespaces are preserved).
`selected` uses `repositories`; `all-current` uses the saved
`membership_snapshot`, not live discovery. Personal `include` entries are added,
then canonical `exclude` entries win across scoped appearances. An explicit
empty selection means **no repositories**, not unrestricted. Future following
is off by default; recording it does not itself discover or accept new members.
Selection and review history stay in the local store, not profile bundles or
printed profile exports. Schema version remains 1.

#### Environment projection and legacy migration

`~/.config/hive/contributor.env` becomes a **generated projection** of that
file. The relay, `src/compose-contributor.yaml` and `just contribute-k8s` keep
reading the same `HIVE_HUB` / `HIVE_REGISTRATION_TOKEN` / `CONTRIBUTOR_ID`
lists they always have; the active/top-ranked hive is written first in each,
which is the hub the relay solicits from at startup (`activeHubIndex` 0 in
`bin/contributor-relay.js`). `HIVE_COMMONS_STRATEGY` is written alongside the
lists so the long-running relay can choose its next hive between tasks. Keys the
projection does not own —
`CONTRIBUTOR_USERNAME`, `AGENT_BACKEND`, `HIVE_LITELLM_ENDPOINT` — are carried
across rather than dropped, and the previous file is kept at
`contributor.env.bak`.

The projection also includes an aligned `HIVE_REPO_SCOPE` with one entry per
hub, in `HIVE_HUB` order and separated by `,`: `*` for a legacy profile,
otherwise that hub's sorted canonical repository IDs joined by `;`, with an
empty entry for no repositories. The whole value is wrapped in a literal pair
of single quotes so the separators survive `source`; the relay's env-file
parser strips them. For example, a legacy hub followed by a scoped hub with two
repositories projects as:

```sh
HIVE_REPO_SCOPE='*,github/github.com/hivecommons/hive;github/github.com/hivecommons/promptargs'
```

Readers must split on `,` first (aligned with `HIVE_HUB`) and only then on `;`
within one entry; splitting the whole value on `;` would merge hubs.
`HIVE_REPO_SCOPE_VERSION` is persisted as `repo_scope_version` and incremented
on every profile-store commit (including retries after projection failure).
These fields provide local state for the repository-scope protocol; they do
**not** by themselves enforce filtering on older relays/Hives. Scoped execution
requires the companion relay/Hive acknowledgement and admission work in
[#12570](https://github.com/hivecommons/hive/issues/12570). Operator admission,
trust and quota gates remain authoritative.

The first `hivectl hives` command (except the strictly read-only `hives tree`)
on a machine that still has the old positional `contributor.env` migrates it: entries are named after their hub
host, and the first hub in the legacy list stays active, so a running relay is
unaffected. Migration alone does not rewrite `contributor.env` — the
projection is written only when a command actually changes your hives. A
legacy file whose three lists disagree in length is **refused** rather than
guessed at, because pairing a token with the wrong hub points a live
credential at the wrong hive; fix the file (or re-run
`just contribute-setup`) and try again.

#### Staging adapter contract

`pkg/hivectl.NewStagedEdits` is the shared staging layer for the contribution
tree surfaces. It freezes the loaded profile revision and the hub/repository
discovery snapshots used by `BuildContributionTree`. Opening a session, editing
its tree, reviewing `Diff()`, and discarding unsaved edits perform no writes or
relay signals. The integrated TUI and Web tree adapters use this layer's diff and receipt.

`Diff()` counts subscription–repository pairs once per subscription and exposes cross-profile exclusion effects from `ProfileSet.RepositoryScopes()`. Apply uses `ProfileStore.CommitExpecting`, then `SignalRunningRelay`. A concurrent write yields `ErrStaleRevision`; the surfaces must require a fresh review rather than silently merge that stale diff. See [Apply receipts](#staged-contribution-edits-and-apply-receipts) for the operator-visible outcomes.

## Input and safety

- Write commands take `--file <path>` or `--stdin` (JSON or YAML).
- Destructive actions (`agent delete`, `knowledge delete`, `bead reset`,
  `governor agent-remove`) require `--yes`.
- Request bodies are capped at 1 MiB, measured on the JSON-encoded payload;
  oversized input is rejected before anything is sent.
- Tokens are read from an environment variable, never passed as a flag value.
