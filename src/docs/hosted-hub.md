# Hosted Hive Hub onboarding

The hosted Hive Hub at <https://hive.hivecommons.dev> is the no-cluster path for
trying Hive. The hub hosts the OAuth-protected onboarding pages, lists public
hives, and provisions spoke dashboards for approved requests. The legacy
`https://hive.kubestellar.io` hostname redirects during the cutover; use
`https://hive.hivecommons.dev` for new bookmarks and configuration.

This guide follows the current hub and dashboard UI:

- hub routes such as `/login`, `/get-started`, `/dashboard`, `/my-hives`, and
  `/fleet` are registered in `src/pkg/hub/server.go`, `src/pkg/hub/saas.go`, and
  `src/pkg/hub/oauth.go`;
- the request wizard labels are **Login**, **Your Repo**, and **Request** in
  `src/pkg/hub/static/get-started.html`;
- the spoke dashboard settings tabs are rendered by
  `src/pkg/dashboard/static/index.html` as **Forge App**, **Model Gateways**,
  **Budget**, **Repos**, and the per-agent **General** tab.

## 1. Sign in

1. Open <https://hive.hivecommons.dev>.
2. Click **Sign in** or open <https://hive.hivecommons.dev/login>.
3. Complete the GitHub login. The hosted hub uses your GitHub identity to know
   who is requesting the hive; the wizard says it requests no account
   permissions and stores no password.
4. After login, open **My Hives** from the header or go to
   <https://hive.hivecommons.dev/dashboard>. The same dashboard is also aliased
   at `/my-hives`.

## 2. Request a hosted hive for a repo

1. Click **Get Started** or **Request a Hive**, or open
   <https://hive.hivecommons.dev/get-started>.
2. In **Your Repo**, paste the repository URL the hive should manage. Accepted
   examples in the UI include `github.com/my-org/my-repo` and
   `https://github.ibm.com/my-org/my-repo`. GitHub.com and GitHub Enterprise are
   supported; GitLab/Gitea/Forgejo URLs are rejected by the wizard today.
3. In **Request**, pick the starting ACMM level. The wizard currently offers
   L1–L3 and marks **L3 — Quality-Gated (Measured)** as the recommended start.
   L4–L6 appear as **Available after provisioning** because higher automation is
   advanced from the spoke dashboard after the hive is running.
4. Add optional contact details, review the summary, and click **Request a
   Hive**.
5. Wait for an admin to approve and provision the request. When the hive is
   ready it appears under **My Hives** with its dashboard URL.

## 3. Finish first-run setup on the spoke dashboard

Open the hive dashboard from **My Hives**. The dashboard's welcome checklist and
settings tabs guide the rest of setup.

### Install or authorize the GitHub App

The hosted wizard does not ask you to install the GitHub App before
provisioning. After provisioning, the spoke dashboard prompts you with the
right install link for the GitHub host your repo lives on.

Use either:

- the welcome checklist action **Install GitHub App**; or
- **Settings → Forge App**, where the **Active config** card shows the App ID,
  App slug, API URL, Base URL, Installation ID, key fingerprint, and **Auth
  state**. If installation is missing, click **Install GitHub App on
  `<host>`**, finish the GitHub install/authorization flow, then click
  **Re-check**.

When adding more repos later, use **Settings → Repos**. The add flow checks
GitHub App access and, when needed, offers **Install/authorize the App for
`<org>`** followed by **Re-check & add**.

### Configure model gateways and keys

Open **Settings → Model Gateways** when an agent uses OpenRouter, LiteLLM, vLLM,
llm-d, watsonx, or another OpenAI-compatible endpoint.

- Click **+ Add gateway** for a manual gateway. The form has **Preset**,
  **Name**, **Endpoint URL**, **API Key**, optional **Key name**, and **Default
  Model** fields. Keys are stored privately on the hive and are not written into
  `hive.yaml`, logs, or config downloads.
- Click **⚡ Fund this hive with OpenRouter** to generate an OpenRouter
  authorization QR/link. A scoped OpenRouter key is stored as the `openrouter`
  gateway when authorization completes.
- Use **Test** on a saved gateway. Success reads `OK — N models available`.
  Failures are shown as `Connection failed: ...` and classify common causes such
  as an auth rejection (`HTTP 401`), DNS/connectivity, `5xx`, or budget/rate
  limiting (`HTTP 429`).

### Log in Copilot-backed agents

Agents that use `copilot` need a device-flow login on the spoke. The welcome
checklist calls this **Log in once per method** and explains that one Copilot
login covers every Copilot agent. Use the checklist **Show me** action or open an
agent terminal with the cyan **▶ terminal** button and complete `/login` in the
CLI when prompted. The login modal shows a device code, **Copy**, **Open GitHub
→**, and a polling status until authorization finishes.

### Confirm budget and ACMM settings

- Open the maturity-level dialog from the amber ACMM badge to confirm the hive's
  ACMM level. The hosted request starts at the selected level, but higher levels
  should be earned as CI, coverage, and operator trust improve.
- Open **Settings → Budget** to set **Total Tokens**, **Period (days)**, and
  **Critical Percent**. The UI notes that an exhausted budget suppresses most
  kicks; per-agent exemptions are available on the same tab.

### Set your communication persona

Hive records a per-user communication persona on the hosted hub user record,
keyed by the same identity used for OAuth. The proposal is intentionally scoped:
persona data describes how summaries should be phrased, not what agents may do.
It contains `persona.depth` (`outcomes` or `technical`),
`persona.summary_length` (`short`, `standard`, or `detailed`), and free-text
notes. It is stored separately from hive ACMM level and agent mode settings.

In the chat spine, run `!persona setup` to answer the three onboarding
questions. Use `!persona show` to inspect the current record and
`!persona set <key> <value>` to edit one field. Run summaries use the author's
persona for their default depth; `!runs <key> more` always expands to the
technical view without changing the persona or any autonomy setting.

#### Persona learning (default off)

Hive can propose persona changes from observed behaviour, so depth converges
toward what each person actually reads without a settings change. It is off
unless the operator sets `persona.learning.enabled: true` in the hive config or
turns on **Persona learning** under Settings -> Features. Learning never
changes a persona on its own and never touches ACMM, agent mode, or any other
autonomy setting: the persona record and the autonomy configuration share no
key, and the adjustment code in `pkg/persona` is stdlib-only, both enforced by
tests.

Three explicit signals are counted per user on the hub persona record (counts
only, never transcripts):

- `expanded`: the user asked `!runs <key> more` after seeing a summary.
- `skipped`: the user approved or rejected a run without expanding it.
- `re-asked`: the user asked `!runs <key>` again for a summary they had
  already seen and not expanded.

Adjustment rule: when the same-direction count reaches
`persona.learning.threshold` (default 5) inside
`persona.learning.window_days` (default 7), Hive proposes ONE step, never more
per window: expansions and re-asks move `persona.depth` from `outcomes` to
`technical`, then `persona.summary_length` up one notch; skips move `depth`
from `technical` to `outcomes`, then `summary_length` down one notch. A record
at the top or bottom of the ladder gets no suggestion, so a second week of
expansions cannot push past `technical`/`detailed`. Counters reset when a
suggestion is made or the window rolls over.

The user must confirm every change:

- `!persona suggestions` lists pending proposals with their evidence, for
  example `set depth from outcomes to technical (evidence: 5 expansions in 7
  days)`.
- `!persona accept <n>` applies one proposal and writes a `persona_adjusted`
  audit entry naming the user, the key, the old and new values, and the
  evidence.
- `!persona reject` dismisses the pending proposals; nothing changes.
- `!persona show` lists the last adjustment and its evidence.
- `!persona undo` reverts the last adjustment and pins the persona;
  `!persona pin` pins without reverting. A pinned persona collects no signals
  and receives no suggestions until `!persona unpin`.

Hub operators can also inspect the read-only **Persona profile** section on the
hub dashboard when persona learning is enabled. The section is hidden while
`persona.learning.enabled` is off (and the hub mirror flag is off) or for
non-operators. It lists each stored persona's current traits, signal counters
as weights, pending suggestions with evidence counts, accepted/declined
history, and the user-record last-updated time. This reconciles the original
#8363 "automatic adjustment" request with the safer shipped design: Hive learns
from behaviour, but adjustment is deliberately a confirmed suggestion because a
silent persona mutation would be hard to notice, hard to attribute, and too
easy to confuse with an autonomy change.

Users without a persona record are never counted; run `!persona setup` first.

## 4. Read the Fleet page

Open <https://hive.hivecommons.dev/fleet>. The page title is **Fleet health** and
the table columns are **Hive**, **ACMM**, **Mode**, **Version**, **Divergence
(expects · running · able)**, and **Output (90d)**.

Key signals:

- The left dot summarizes the hive as problem, warning, ok, unknown, offline, or
  parked.
- The **WHY** chip and remediation line explain non-green rows when the hub has a
  specific fix.
- **Divergence (expects · running · able)** compares how many agents the governor
  expects, how many are running, and how many can actually work. A **PROBLEM**
  chip means the governor expects agents to be on but they are blocked.
- Agent restart storms are also surfaced as problem chips when a reported agent
  reaches `HIVE_HUB_AGENT_RESTART_PROBLEM_THRESHOLD` restarts in 24 hours
  (default `5`). Owners/admins can click **reset** beside the chip to start
  counting from the reset point while troubleshooting recurrence.
- Capability chips show green/amber/red/gray for able/partial/blocked/unknown,
  with ticks for issue, PR, and merge capability.
- Filters at the top let you narrow by **problems only**, **advisory output**,
  **budget**, and **gh app** health.
- Per-hive drift badges (`heartbeat-stale`, `duplicate-spoke`, `identity-split`,
  `version-absent`, and more) flag rows whose state deviates from the fleet or
  cannot be trusted. Each kind, its severity, and who is expected to fix it
  (hive owner vs hub operator) are listed in the
  [fleet drift signals reference](fleet-drift-signals.md).

## Common problems and fixes

| Symptom | Likely cause | Fix |
|---|---|---|
| Gateway test says `Connection failed` or the fleet diagnosis says `inference auth` | The gateway key is stale, rejected (`HTTP 401`), DNS/connectivity failed, or the provider returned a budget/rate-limit response. | Open the spoke dashboard, go to **Settings → Model Gateways**, edit the affected gateway, update **Endpoint URL** or **API Key**, and click **Test**. |
| Copilot agents show `stuck at login` or fleet remediation says **Copilot device-flow login on the spoke dashboard** | The Copilot CLI has no usable device-flow login in the spoke. | Open the hive dashboard, use **Log in once per method**, or open **▶ terminal** and run `/login` in the Copilot CLI. |
| Fleet shows `budget exhausted`, `budget critical`, or remediation says **Raise or reset the budget limit (Settings → Budget)** | The governor token budget is depleted or set too low. | Open **Settings → Budget**, raise **Total Tokens**, adjust **Period (days)**/**Critical Percent**, or exempt a specific agent if it must keep running. |
| GitHub App health is `broken`, `not-installed`, or repo add says the App needs access | The GitHub App is not installed on the org/repo, points at the wrong installation, or lacks access after a repo/org change. | Open **Settings → Forge App**, click **Install GitHub App on `<host>`** or **View App settings**, complete the GitHub flow, then click **Re-check**. For repo additions, use **Install/authorize the App for `<org>`** and **Re-check & add**. |
| A requested hive disappeared or the old per-hive URL times out | Unconfigured or inactive hosted hives can be reaped to free capacity. | Request a new hive at <https://hive.hivecommons.dev/get-started>, install the Forge App in the first session, and update bookmarks to the new URL. |
| **Settings → Hub → Dashboard URL** is read-only ("Owned by the hub (heartbeat)"), or saving a new value answers `409` | On a hosted hive the hub owns `hub.dashboard_url`: its ingress terminates the hostname and the heartbeat delivers the vanity URL, so a local edit would be reverted by the next beat — and until then it would be the OAuth callback origin, so a typo could lock you out. | Nothing to fix: the field records where the hub serves your dashboard. To serve it from a different host of your own, set `dashboard.public_url` (it takes precedence for OAuth and is never overwritten by the heartbeat). |

## Getting help

- For project questions or hosted-hub problems, open an issue in
  <https://github.com/hivecommons/hive/issues> and include the hive name, repo,
  and the visible fleet/dashboard error text. Do not include API keys, private
  keys, cookies, or tokens.
- For self-hosted hub operation, use the [self-hosted hub deployment guide](hub-deployment.md),
  [fleet health guide](fleet-health.md), and [troubleshooting guide](troubleshooting.md).
