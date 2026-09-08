# Fleet drift signals: the per-hive deviation badges on My Hives

Issue [#6089](https://github.com/hivecommons/hive/issues/6089). Every My Hives
request recomputes a **drift report** for each hive server-side
(`src/pkg/hub/drift.go`) and ships it on the same payload the table already
renders. Each signal is a hub-observed fact — a comparison of the hive against
the rest of the fleet, or against itself a minute ago — that would otherwise be
silent: a spoke on a branch nobody else runs, a GitHub App that was never
installed, an upgrade wedged since last week.

This page is the operator reference for those signals: what each kind means,
its severity, **who is expected to fix it** (hive owner vs hub operator), and
why a signal you expect may be deliberately suppressed.

Drift signals are distinct from the per-hive health **verdict** on `/fleet`
(see [fleet-health.md](fleet-health.md)): the verdict judges whether the hive
is producing output; drift signals judge whether the hive's row can be trusted
and whether its configuration matches the fleet.

## Severities

| Severity | Meaning |
| --- | --- |
| `info` | Worth knowing, not acting on today (e.g. a couple of commits behind the fleet). |
| `warn` | A real misconfiguration that degrades the hive but has not stopped it (e.g. ACMM unset, no agents). |
| `critical` | The hive does not work as configured (e.g. offline when it should be up, App required but not installed, pinned to an immutable tag). |

A hive's row shows its **worst** signal; the fleet-exceptions summary groups
by kind and sorts by urgency. Kind strings are stable identifiers — the
frontend uses them as filter keys, so they are listed here verbatim.

## Signal reference

### Signals that apply to every hive (claimed or placeholder)

| Kind | Severity | Meaning | Who fixes it |
| --- | --- | --- | --- |
| `heartbeat-stale` | critical | No heartbeat for longer than the online threshold (5 min, the same bound that clears the Online dot, so the badge and the dot never disagree). Only fires once the hive has beaten at least once — a slot that has never reported is being provisioned, not drifting. | Owner/operator: check the spoke pod. |
| `upgrade-stuck` | warn / critical | An upgrade is recorded in flight with no start stamp (warn), or has exceeded the 10-minute stuck limit (critical) — the same bound the hub's upgrade state machine uses, so the badge and the row's upgrade spinner agree on when to give up. | Operator: inspect the spoke's rollout. |
| `upgrading` | info | An upgrade is **actively** in progress: real start stamp, within the limit, no recorded failure. Status, not alarm — it exists so mid-upgrade hives don't pollute the version-drift breakdown during an upgrade wave. While it fires, the fleet-relative signals below are suppressed. | Nobody — wait for it to land. |
| `pinned-image` | critical | The spoke's own Deployment image is a digest pin, SHA tag, or release tag — not a rolling `*-latest` tag — so it can **never** receive a rolling upgrade. Requires positive evidence of a pin: an empty or unparseable image ref stays silent rather than paging a human about eleven healthy rolling hives. | Operator: repoint the Deployment at a rolling tag. |
| `health-degraded` | warn / critical | The spoke reports health `degraded` (warn) or `critical` (critical), naming up to three failing checks (the rest summarized). Placeholders run a real spoke process and can genuinely be degraded, so this is not claimed-only. | Owner: see the named checks; [troubleshooting.md](troubleshooting.md). |
| `duplicate-spoke` | critical | Two spoke instances are alternating reports as this one hive — every registry field flips with whichever reported last, poisoning every other signal on the row. The reason names the culprit pods. **Restart Spoke** sheds the stale instance. | Owner/operator: use Restart Spoke. |
| `status-flipping` | warn | Reported status keeps alternating between two values on a heartbeat cadence — usually two instances too old to report pod names (indistinguishable from each other), or an oscillating auth check. Suppressed when `duplicate-spoke` already fired, since that signal carries the same fact plus the culprit names. | Owner: Restart Spoke, or upgrade the spoke so instances identify themselves. |

### Claimed-hive-only signals

A placeholder (unassigned pool slot) legitimately has no GitHub App, no agents,
and ACMM 0 — flagging those would bury the real signals under pool noise, so
none of the following fire on placeholders.

| Kind | Severity | Meaning | Who fixes it |
| --- | --- | --- | --- |
| `app-id-placeholder` | critical | The hive is authenticating as the placeholder App ID sentinel — not a real GitHub App, so no installation ID or private key can ever make it work. Deliberately **not** gated on the spoke's own "App required" verdict: the app_id is a raw number the spoke reports regardless, so this fires even on spokes too old or wedged to classify their failure. | **Hub operator** must assign the cluster's real `github_app_id`. The owner cannot fix this and should not be asked to. |
| `app-creds-operator` | critical | The App private key was never delivered to the spoke, or does not match the App it authenticates as (GitHub rejects its JWT). Kept distinct from `app-missing`/`app-perm-issue` so operator work is never filed as an owner-facing adoption problem. | **Hub operator** only. |
| `app-perm-issue` | critical | The GitHub App is installed but its permissions are insufficient; the reason names the missing permission. | Owner: update the App installation's permissions. |
| `app-missing` | critical | The hive requires a GitHub App but none is installed — agents cannot act on the repo. Suppressed when the app_id is the placeholder sentinel, because "App not installed" is exactly the misdiagnosis `app-id-placeholder` exists to end. | Owner: install the App ([github-app-setup.md](github-app-setup.md)). |
| `identity-split` | critical | The hive's GitHub identity components disagree about which forge they name — most importantly a GHE app_id/app_slug with an empty or public api_url, which authenticates as nothing and fails every token request with `404 Integration not found`. The App is real and the credentials are right; they are pointed at the wrong forge. | **Hub operator**: a push moved one component of the identity without the others. |
| `version-absent` | critical | Heartbeats keep arriving but the last 3 beats carried an empty `git_hash`. The hub compares that hash against the branch target to decide on an upgrade, so it is sending this hive **no upgrade instruction at all** — the row cannot be trusted and the hive silently stops upgrading while still counting as online. Skipped for offline hives (silence is already flagged) and placeholders. | Owner: check the spoke for stats-collection timeouts, then restart it. |
| `acmm-unset` | warn | ACMM level is 0 on a claimed hive — the governor has no maturity target to work toward. | Owner: set the ACMM level. |
| `no-agents` | warn | No agents are running on a claimed hive — it holds a slot but does no work. | Owner: configure agents ([agent-configuration.md](agent-configuration.md)). |
| `agent-restart-storm` | critical | At least one reported agent restarted `HIVE_HUB_AGENT_RESTART_PROBLEM_THRESHOLD` times (default 5) in the last 24h or since the hub-side reset marker. Appended from the fleet rollup rather than `computeDrift`; owners/admins can click **reset** beside the chip on `/fleet` while troubleshooting. | Owner: see [fleet-health.md](fleet-health.md). |

### Fleet-relative signals

These compare a hive against the **fleet norm**: the modal git branch across
online, non-placeholder hives, and — scoped within that branch — the modal
short SHA. The norm is *derived, never hardcoded*, so the model stays correct
when the fleet moves to a new branch without anyone editing code.

| Kind | Severity | Meaning | Who fixes it |
| --- | --- | --- | --- |
| `branch-mismatch` | warn | Running a branch different from the one most of the fleet's online hives run. | Operator: repoint or upgrade the hive. |
| `version-behind` | info | On the norm branch but running a SHA that differs from the branch's latest build (preferred target) or, when the hub has not resolved a latest for the branch, from the fleet's modal SHA. | Usually nobody — the next upgrade wave resolves it. |

Fleet-relative signals are **skipped entirely** when:

- fewer than **3** eligible hives exist — "the majority" is not meaningful in a
  two-hive fleet where flagging either would be arbitrary;
- the hive is **offline** (its last-reported version is not evidence of current
  state) or a **placeholder** (it tracks the pool image, not the fleet);
- the hive is **actively upgrading** — mid-upgrade drift is the expected state
  the upgrade exists to resolve, and flagging it would file "being fixed" under
  "needs attention". The moment the upgrade lands, fails, or exceeds the stuck
  limit, any remaining drift is flagged again.

Ties in the modal computation break on the lexically smaller value, so the norm
is deterministic — it cannot flicker between two equally popular branches and
flag half the fleet on alternating page loads.

## First-seen timestamps

Each signal carries a `firstSeen` timestamp: when the hub first observed this
(hive, kind) pair. It stays stable across recomputes while the signal persists
— the hover can say "since 3:42 PM" instead of drifting to "now" on every beat
— and resets only after the signal clears, so a recurrence is dated to its
recurrence. It is keyed by **kind, not reason**, because a reason legitimately
mutates while the condition persists ("No heartbeat for 3 min" becomes "… for
4 min").

The tracker is **in-memory only**: a hub restart re-baselines every "since" to
the first recompute after boot. That is an accepted trade — the timestamp is a
triage aid ("did this start before or after my change?"), not an audit record.

## Where the signals come from (for the curious)

- `src/pkg/hub/drift.go` — kinds, severities, fleet norm, `computeDrift`, and
  the first-seen tracker.
- `src/pkg/hub/version_absent.go` — the beats-to-confirm counter behind
  `version-absent`.
- `src/pkg/hub/saas.go` — appends `agent-restart-storm` from the fleet rollup.

Reasons are complete human-readable sentences rendered verbatim by the UI, so
the specifics (which branch, which check, how long) live in the reason string
itself rather than a lookup table.
