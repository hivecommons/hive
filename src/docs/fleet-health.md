# Fleet health verdicts and remediation hints

The hub computes a per-hive **health verdict** for the `/fleet` view and the
my-hives API ([#5577](https://github.com/kubestellar/hive/issues/5577)). Each
hive card shows a color state, a one-line **WHY chip** (the reason), and — for
any non-green verdict that matches a known failure signature — a one-line
**remediation hint**: what to do and where to do it, with a direct link when
the hub can build one.

This page covers hive-level health on the hub. For dashboard URL and
Ingress/Route health probing, see [health-checks.md](health-checks.md).

## Verdict states

| State | Meaning |
| --- | --- |
| `green` | Producing output; no known fault. Green **never** carries a remediation hint. |
| `amber` | Output may still look acceptable, but a named condition needs an operator (all agents paused, consent wedge, missing cadences, channel lag). |
| `red` | The hive cannot produce or deliver work (GitHub App broken, provider limit, budget exhausted, agents down/stuck, stale output). |
| `unknown` | Not reporting (offline) or the spoke is too old to report health. No hint — there is nothing trustworthy to diagnose from. |

## Precedence

The base verdict returns on the **first matching precondition**, so a more
fundamental fault always wins the headline:

1. GitHub App broken
2. Provider limit reached
3. Budget exhausted / budget misconfigured
4. Blocked agents (out of quota, stuck at login, down, idle with work)
5. Generic stale-output red (no recent output)

A budget-exhausted red can never be shadowed by the generic no-output red, and
App-broken beats everything. The detector ambers below only ever demote green
— they never mask a red.

**Exemptions:** L1 (Inception) hives and non-reporting hives are exempt from
output-based verdicts — no output is expected there, so a config gap is not a
health fault.

## Remediation hints

Every hint has an **action** (one-line "do this"), a **surface** (where the
action happens), and, when the hub can resolve one, a direct **link**. The
signature→action table (`src/pkg/hub/remediation.go`):

| Cause | State | Hint | Surface |
| --- | --- | --- | --- |
| `app-broken` | red | Install or repair the GitHub App for this repo | GitHub App settings (link resolved per cluster forge — github.com vs GHE) |
| `login-stuck` | red | Copilot device-flow login on the spoke dashboard | spoke dashboard `/login` |
| `budget-exhausted`, `budget-misconfigured` | red | Raise or reset the budget limit (Settings → Budget) | spoke dashboard settings |
| `error-streak` | red | Pin a working model on the agent card | spoke dashboard agent card |
| `consent-wedge` | amber | Complete the Copilot consent flow on the live pod dashboard | spoke dashboard |
| `no-cadence` | amber | Set cadences on the agent card | spoke dashboard agent card |
| `hold-stale` | amber | Review the needs-human queue | repo PR list (link to the hive's first repo's `/pulls`) |
| `channel-lag` | amber | Spoke lags its channel — check auto-upgrade / force rollout | hub fleet version controls |

Causes without a row here (provider limit, agents down, advisory stale, …)
get **no hint**: a hint is only ever shown when it is specifically true.

## Detector states

Detector signals layer onto the base verdict:

- **error-streak (re-explains a red).** When a *generic* stale-output red is
  showing but an agent has **3 or more consecutive failed model calls**, the
  reason is rewritten to name the agent and the streak — "turns ran, every
  model call died" is a different fix than "no write in 4 days"
  ([#5338](https://github.com/kubestellar/hive/issues/5338)). Precondition
  reds (App, provider, budget, login) keep their reason: they gate the output
  stream upstream of the model calls and already name a more fundamental fix.
  The threshold of three matches the spoke's consecutive-failure convention —
  one failure is noise, three in a row is a pattern.
- **consent-wedge (demotes green).** A named agent is looping on a Copilot
  consent screen; only an operator can complete the flow.
- **no-cadence (demotes green).** Enabled agents exist that will never be
  kicked because no cadence is set.
- **channel-lag (demotes green).** The spoke tracks the `stable` channel, is
  measurably **2 or more commits behind** its channel head, and is not
  currently upgrading. One commit behind is a rollout in flight; two or more
  with no upgrade running means the channel moved and the spoke did not
  follow. Skipped entirely when the channel signals are absent.

Consent-wedge, no-cadence, and channel-lag never touch a red or amber verdict
— a harder fault stays the headline.
