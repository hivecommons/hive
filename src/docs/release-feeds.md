# Release-feed watcher

Kicks an agent as soon as an upstream dependency it tracks publishes a new
version, instead of leaving the agent to find the release on its own timer.
**Off until an agent lists a source.** Tracking issue:
[#18494](https://github.com/hivecommons/hive/issues/18494).

A custom agent that bumps dependencies used to learn about a release only
when its cadence came round, which delayed the bump and spent kicks that found
nothing. The governor already raises signals for PRs, CI and issues; the
release-feed watcher adds one for upstream releases.

## Turning it on

```yaml
governor:
  release_feeds:
    # enabled: false        # explicit off switch; absent = on when any agent lists a source
    interval_s: 900         # default: HIVE_RELEASE_FEED_INTERVAL, else 15m (never below 1m)
    cooldown_s: 3600        # default: HIVE_RELEASE_FEED_COOLDOWN, else 1h
    agents:
      deps-bot:
        cooldown_s: 1800    # optional per-agent override
        sources:
          - kind: github_release     # latest published release
            repo: kubernetes/kubernetes
          - kind: github_tag         # newest tag as GitHub lists it
            repo: psf/requests
          - kind: pypi               # PyPI package JSON API
            package: requests
          - kind: index              # any URL; private indexes use auth_var
            url: https://pkgs.example.com/widgets/latest
            version_field: version   # JSON key (default "version")
            name: widgets            # label in the kick (default: the URL)
            auth_var: WIDGETS_INDEX_TOKEN
```

| Kind | Request | Version read from |
|---|---|---|
| `github_release` | `GET https://api.github.com/repos/<repo>/releases/latest` | `tag_name` |
| `github_tag` | `GET https://api.github.com/repos/<repo>/tags?per_page=1` | first tag's `name` |
| `pypi` | `GET https://pypi.org/pypi/<package>/json` | `info.version` |
| `index` | `GET <url>` | a JSON object's `version_field`, or the first non-empty line of a plain-text body |

`hive.yaml` validation rejects an unknown `kind`, a GitHub `repo` that is not
`owner/repo`, a `pypi` source without `package`, an `index` URL that is not
http(s), a negative interval or cooldown, and an `auth_var` that is not a
valid variable name. The keys are YAML-only; Settings has no control for them
yet.

## What happens on a poll

Every interval the watcher fetches each subscribed feed **once** (a feed two
agents share is one request) and compares the version with what each
subscriber was last told about:

1. **First answer is a baseline.** A newly added feed, or a fresh state file,
   records the current version and kicks nothing.
2. **Nothing changed, no kick.** Requests are conditional: the stored `ETag`
   goes out as `If-None-Match` and `Last-Modified` as `If-Modified-Since`, so
   an unchanged feed usually costs a `304` with no body.
3. **New version: kick within the same poll.** The release joins the agent's
   pending list and, unless the agent is in its [cooldown](#cooldown), the
   agent is kicked at once through the runtime's
   [kick queue](design/crux.md), which delivers when the agent is idle so a
   release never interrupts a running turn. A runtime without the queue
   (`CRUX_MODE=sidecar`) gets a plain kick.

The kick passes the same gates as the webhook CI-failure kick
([runtime-and-quota.md](operator-reference/runtime-and-quota.md)): the agent
must be known, enabled and not paused in config or at runtime, and eligible
for an event-driven kick (no cadence pause, not on-demand, budget not
exhausted). A refused or failed kick keeps the releases pending and they go
out on a later poll once the gate opens.

Kick history records the reason, for example
`new release: requests 2.32.0 -> 2.33.0`, on the governor kick entry
(`reason` in the persisted state and the kick-history API).

State (last-seen versions, validators, pending releases, last kick time) is
kept in `/data/release-feeds.json`, so a release published while the hive was
down still kicks after the restart. An unreadable state file is logged and
replaced: the next answers become baselines.

## The `${NEW_RELEASES}` variable

`${NEW_RELEASES}` is a built-in kick-template variable. It renders the
agent's pending releases, empty when there are none:

```text
NEW UPSTREAM RELEASES (release-feed watcher):
- requests: 2.32.0 -> 2.33.0 (https://pypi.org/project/requests/2.33.0/)
- kubernetes/kubernetes: v1.34.1 -> v1.35.0 (https://github.com/kubernetes/kubernetes/releases/tag/v1.35.0)
```

When the agent's kick template contains `${NEW_RELEASES}`, the release kick
is the agent's full rendered prompt with the block in place. Otherwise the
kick is a short targeted message: a header telling the agent to act on the
releases (and not to poll the feeds itself) followed by the block. Versions
are reduced to one printable line of at most 128 characters, links to http(s)
without credentials, and the message goes through the hive's mention
sanitizer.

## Cooldown

An agent gets at most one release-feed kick per cooldown. Releases that land
inside it wait in the pending list; a feed that moves twice before its kick
is folded into one line that keeps the original old version and the newest
new one (`1.1.0 -> 1.3.0`). Precedence: the agent's `cooldown_s`, then the
block's `cooldown_s`, then `HIVE_RELEASE_FEED_COOLDOWN`, then `1h`.

## Egress and credentials

Polls go through the spoke's upstream-proxy rules (`network.upstream_proxy`,
`HIVE_UPSTREAM_PROXY_*`, or `HTTPS_PROXY`), the same selector the control
plane's other outbound clients use; a locked-down spoke must allow the feed
hosts there.

A private index authenticates with `auth_var`, the name of an existing
variable: the agent's own `variables:` entry wins over a hive-level
`variables.defs` entry, exactly as for its kick prompt. The resolved value is
sent as `Authorization: Bearer <value>`. It is never logged, never written to
the state file, and dropped by Go's HTTP client on a redirect to another host.
If the variable does not resolve, the feed is skipped with a warning naming
the variable (not its value) rather than polled anonymously. An index URL that
carries a credential in its query string is redacted (query replaced by a
short digest) in logs, the state file and the kick block.
