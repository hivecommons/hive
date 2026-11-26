# Dashboard settings explained

This page is the starting point for the dashboard's **Settings** tabs. It has one entry per tab: what the tab controls and which page explains it in depth.

Every setting on a Settings tab has a small **?** mark next to its name. Rest the pointer on it, or move to it with the Tab key, to see a short explanation of the setting. Click it, tap it, or press Enter on it to open the page that explains the setting in a new tab. When a setting has its own guide, the mark opens that guide; otherwise it opens this page at the tab's entry. Clicking a mark never changes the setting.

For the dashboard's other sections, see [Dashboard sections explained](dashboard-sections.md); to find a section or a tab by its on-screen name, see the [Dashboard UI map](dashboard-ui-map.md). If a key has no editable control, see [Editing `hive.yaml`](editing-hive-yaml.md) for file locations and reload behavior.

## Linking to a setting

`<dashboard>/#config/governor/<tab>` opens Settings on a tab. Add a search term as a third part, `<dashboard>/#config/governor/<tab>/<query>`, and Settings opens with the search box already holding `<query>`, so the results list the matching settings. For example, `#config/governor/Features/issues.close_on_merge` opens Settings searching for that key.

### Triage: does this RFC already exist?

Many RFCs ask for something a setting already does. With `issues.rfc_existing_check.enabled: true` (default off), Hive checks each new open issue labelled `rfc`, `enhancement` or `kind/feature` against the Settings search index. It compares the issue title and body with each indexed setting's key, label, section and search text by keyword overlap. There is no model call. Hive then posts one comment carrying `<!-- hive:rfc-existing-coverage -->`. The comment lists the best matches. Each match links to Settings with the search set to its key (the link above, built from `dashboard.public_url`) and to a docs search for the key. Hive finds the marker before posting, so it never comments twice, including after a restart. The check only comments: it never adds labels, closes issues, or decides anything.

| Key | Default | Meaning |
|---|---|---|
| `issues.rfc_existing_check.enabled` | `false` | Turn the check on. |
| `issues.rfc_existing_check.post_when_nothing_found` | `false` | Also post a one-time "Nothing found" comment when no setting matches. |
| `issues.rfc_existing_check.labels` | `[rfc, enhancement, kind/feature]` | Labels that mark an issue as an RFC or feature request. Matching ignores case. |
| `issues.rfc_existing_check.max_matches` | `5` | Most settings listed in one comment. |
| `issues.rfc_existing_check.max_age` | `168h` | Skip issues opened longer ago than this, so turning the check on does not comment on the backlog. |

These keys are set in `hive.yaml` only; they have no Settings control yet.

## General

Hive-wide basics: the hive ID, the ACMM maturity level, how often the governor evaluates the hive, the fleet-wide default for explain mode, and whether agent-created PRs and issues carry an attribution trailer. The **Eval loop pacing** block sets the eval-interval cap, the webhook-healthy interval, the conserve-mode multiplier, how often optional sweeps run and the provider spend-limit probe interval; a saved change applies from the next eval cycle. It also shows, read-only, whether the mode thresholds are pack-seeded or operator-owned.

- ACMM levels and the agents each level runs: [ACMM levels](agent-configuration.md#acmm-levels-agent-rosters-as-packs).
- Explain mode: [Explain mode](agent-configuration.md#explain-mode-debugging-agent-behaviour).
- Every configuration block and notable field: [Hive operator reference](operator-reference.md#configuration-blocks).

## Thresholds

The issue and PR counts that move the governor between modes, and how those thresholds scale as the hive manages more repos. See [Governor mode thresholds](governor-thresholds.md).

## Labels

Which labels hold, require, or route work, the label the merger queue applies to queue a PR for auto-merge (applies after a restart), and the reporter-trust rules for outside PRs. See [Labels and control signals](labels-and-control-signals.md) and [Label policy](agent-configuration.md#label-policy-which-issues-agents-may-work).

## Repos

The repos the hive manages and the caps on how many issues and PRs are listed in one kick (`governor.kick_limits`). See the `governor.kick_limits` row of the [notable fields](operator-reference.md#notable-fields).

To add a repository to an existing hive, open its dashboard and choose
**Settings → Repos**, or use the gear on **Projects**. Enter `org/repo` or the
GitHub URL for the hive's forge and choose **+ Add**. If the GitHub App needs
access, follow **Manage App access**, then choose **Re-check & add**. Choose
**Save & close** to apply the monitored list. Adding a repository from a
different organization migrates the hive to that organization. To request
a *new* hosted hive instead, use
[Request a hosted hive for a repo](hosted-hub.md#2-request-a-hosted-hive-for-a-repo).

The same tab has a **Parent auto-close** card (owner only, `repos.<key>.parent_autoclose`): per monitored repo, choose "Close finished parents (default)", "Also close epics and holds", "Also close trackers whose work is done" or "Never auto-close parents". It shows the effective value for each repo, applies on save without a restart, and is audited. A parent a person reopened is never re-closed, and issues marked as tracking issues are never auto-closed. See [stage-labels](labels-and-control-signals/stage-labels.md#decomposed-parents--closed-by-their-children-never-stalled).

### Open PRs per repo

Each repo row has an **open PRs** select (`project.repo_policies[].open_prs`). The repo card on **Projects** has the same select next to its **auto-merge** switch. It works at every ACMM level, and a change saves at once, without **Save & close**.

| Mode | YAML | What agents may do in the repo |
|---|---|---|
| **On** (default) | unset, or `on` | Open pull requests as usual. |
| **Workflow only** | `workflow_only` | Open a pull request only for a workflow hand-off (`workflow.dispatch`) the hive delivered to that agent. The agent's own cadence work, such as coverage gaps and test hardening, may analyse and file issues but opens no PR. |
| **Off** | `off` | Analyse, comment and file issues. No agent opens a pull request. |

```yaml
project:
  repo_policies:
    - repo: docs
      open_prs: workflow_only
```

- `project.open_prs: false` (env `PROJECT_OPEN_PRS`) still wins: every repo is then off, whatever its own mode.
- The mode is enforced where agent PRs open, not only in the prompt: the `hive-open-pr` relay, the forge relay for non-GitHub repos, the sandbox PR path and the sidecar callback all refuse a PR the mode forbids. The request result says why, and the refusal is audited as `agent_write_refused` with `gate=open-prs`.
- For **Workflow only**, the hive decides whether a PR comes from a workflow hand-off by its own records of the hand-off kicks it planned and delivered, never by the PR's title, body or branch name:
  - A `hive-open-pr` request must name the handed-off issue (`--issues <n>`, which the hand-off kick tells the agent to pass). The hand-off must have gone to that agent for that repo within the last 8 hours, and must not yet be settled (its deliverable has not shown up, and it was not marked as not started). A request that names no issue, or another issue, is refused, so the same agent's cadence PRs stay off.
  - A sandboxed kick may push and open its PR only when its kick text is exactly a hand-off kick the hive planned for that agent and repo.
  - A hand-off of a pull-request item (rather than an issue) cannot open a new PR through `hive-open-pr` in a **Workflow only** repo.
- A runtime outside the hive process (the sidecar callback) cannot see those records, so a sandboxed kick there opens no PR in a **Workflow only** repo.
- Who can trigger a hand-off matters: a hand-off is planned when an item enters a workflow state, so anyone who can move items into that state can cause the hive to hand work to the profile's agent.
- Each kick tells the agent about a limited repo: "PRs are off for `<repo>`" or "only workflow hand-off PRs".
- A workflow profile that opens a PR resolves to no agent in an **Off** repo.
- Narrowing the mode needs repo write or the owner role. Widening it needs a verified owner, as for switching auto-merge back on. Switching back to **On** is stored as `open_prs: on`, so it also outranks a seed `hive.yaml` that says otherwise.
- Pushing more commits to a PR that is already open is not opening a PR, and the mode does not stop it.

## Budget

The token budget and its rolling period, optional dollar and coin caps, the percentage at which the governor enters budget-critical mode, per-agent exemptions, and the model lock that stops automatic model downgrades. See [Governor cadence and budget](operator-reference.md#governor-cadence-and-budget) and [Token collection and usage tracking](token-tracking.md).

## Notifications

The ntfy, Discord, and Slack destinations and which hive events are sent to them. See [Notifications](notifications.md#configuration).

## Health

The escalation breaker, which hands an agent's work to a human after a set number of consecutive red attempts, and the agent self-healing watchdog. See [Agent Self-Healing Watchdog](agent-watchdog.md#configuration).

## Sensing

How long a rate-limit alert stays active and how long an agent is paused after a rate limit is detected.

## Logging

Where rolling log files are written, when they rotate, how long rotated files are kept, the log level, and whether rotated files are compressed. The log directory is read-only at runtime; change it in `hive.yaml`.

## Security and Access

Input/output scanning, the agent sandbox runner, and suspicious-activity alerts. See [ioscan](ioscan.md), [Credential-free sandbox isolation](sandbox-isolation.md), [Suspicious-activity alerts](operator-reference.md#suspicious-activity-alerts-sentinel), and the [Security Model](security-model.md).

"Advanced sandbox: default image and Job settings", then "Job scheduling, resources, preflight, setup and GitHub read relay", holds the hive-wide Kubernetes Job defaults: node selector, resource limits and requests (including device resources such as `ibm.com/spyre_pf`), tolerations, image pull secret names, schedule timeout, the node preflight commands and timeouts (`agent_sandbox.preflight`), the workspace setup step (`agent_sandbox.setup`: setup commands and health checks run in the checked-out workspace after the preflight and before the agent, and their two timeouts), and the opt-in GitHub read relay (`agent_sandbox.job.github_read_proxy`, applied when hive restarts). Maps take one `key=value` per line and tolerations one taint per line. See [Setting up a sandbox from the dashboard](sandbox-isolation.md#setting-up-a-sandbox-from-the-dashboard).

## Features

Optional capabilities, each off or inert until you turn it on. Where a feature has its own guide, its **?** mark opens that guide:

- Retro loop: [Retro lane](retro-lane.md).
- Formal verification: [Formal verification](formal-verification.md).
- Audit publication: [Audit campaign](audit-campaign.md#publication).
- Token mint: [Token mint](token-mint.md).
- Spek stage runner: [Spektacular](spektacular.md).
- Auto-merge: [App self-merge sweep](operator-reference.md#app-self-merge-sweep-auto_merge), [Trusted bot authors](hive-merge.md#trusted-bot-authors), and [Trusted-author auto-merge](hive-merge.md#trusted-author-auto-merge).
- Review gate: [Review swarm](review-swarm.md#configuration), [Blocking line and backlog](review-swarm.md#blocking-line-reviewseverity-reviewbacklog), and [external review bot threads](review-bot-threads.md).
- Trajectory review: [Trajectory-Review Lane](trajectory-review.md#configuration).
- Reviewer-stated rules: [Reviewer-stated rules from review feedback](review-feedback-rules.md#settings). A card under Review (owner only, `review_feedback.rules.*`) turns capture on or off, picks whose rules are recorded at once, adds phrases that count, and switches built-in signals off; each control shows its default and where the current value comes from.
- Linear agent: [Linear agent integration](linear-agent.md).
- Upstream watch: [Upstream watch](upstream-watch.md#configuration).
- Chat config assist: [Hive Chat](dashboard.md#hive-chat). A card under Chat (owner only, `chat.config_assist.*`) turns on model-proposed, owner-confirmed config changes in Hive Chat (off by default) and picks the model for the proposal turn; an empty model falls back to `chat.grounded_answers.model`, then the reviewer model. Other roles see an owner-only note instead of the controls.
- Ask Hive: [Ask AI help](ask-ai-help.md). On by default on a hosted hive (hub mode), off by default on a self-hosted hive; switching it off saves `ask.enabled: false`.

The other features on this tab (OpenTelemetry export, external execution, auto-plan, issue claims, PR follow-up, long-running runs, provider rotation, merge recommendations, convergence rollout, stall replan, and question auto-close) are explained by their **?** tooltips, and their configuration keys are listed in the [Hive operator reference](operator-reference.md#configuration-blocks).

## Knowledge

Whether the public read-only knowledge MCP endpoint is on, the knowledge pipeline, and the external knowledge connectors. The **Knowledge pipeline** block opens with the **Mid-task advisor for sidecar runtimes** switch (`knowledge.sidecar_advise`, off by default, applies at once, needs `knowledge.advisor` set in `hive.yaml`; see [Sidecar runtimes](agent-knowledge-and-memory.md#sidecar-runtimes-knowledgesidecar_advise)), then sets the bead synthesizer (schedule, target layer, minimum confidence, per-cycle fact and GitHub API caps, bead retention), scheduled curator promotion (schedule, threshold, source and target layers), repository code maps (schedule, layer, size cap) and the kick primer's fact and byte caps. Turning the bead synthesizer on or off applies at once; the other pipeline settings apply after a restart, and 0 or empty restores the default. The engine, the vault paths and the primer merge strategy are shown read-only because they are set in `hive.yaml`. The repositories to map and the primer priority list stay yaml-only. See [Knowledge curator](knowledge-curator.md) and [Knowledge connectors](knowledge-connectors.md).

## Meeting attendee

The persona hive attends meetings as (Betty by default): pick a preset or edit its name, display name, aliases, pronouns, voice type, language, accent, Kokoro voice, humor and intro (with a live preview and a **Play sample** button; clicking a preset card also plays that persona's introduction in its own voice, see [Hearing a preset](meetings.md#hearing-a-preset)), set how often she answers room questions unprompted, and see the address people invite her with. Owner or read-write. Changes apply after a restart. See [Meeting attendee settings](meetings.md#meeting-attendee-settings).

## Advisory

How many advisory findings are shown, how often they refresh, and when a finding counts as stale. See [Advisory](advisory.md) and [Advisory staleness](advisory-staleness.md).

## Work Source

Where Hive reads its work from (GitHub Issues, GitHub Projects, Linear, or Jira) and each source's credentials and filters. See [Work sources](work-sources.md).

## Hub

The link between this hive and its hub: hub registration, public listing, heartbeat identifiers, automatic updates, public snapshots, and the contributor queue that relays draw work from.

- Hub registration: [Registering spokes to your hub](hub-deployment.md#registering-spokes-to-your-hub).
- Heartbeat identifiers: [Withholding heartbeat identifiers](telemetry.md#withholding-heartbeat-identifiers).
- Snapshots: [Public snapshots](snapshots.md).
- Contributors: [ClankeR contributor relay](contributor-relay.md) and [Contributor trust tiers and delegated agent roles](contributor-trust-and-roles.md).

The **Hub settings** block at the bottom of the tab sets explicit task acceptance, the already-done label and hold, contributor wall retention, the contributor announcement (text, level, expiry), the snapshot rebuild interval, the self-upgrade drain timeout, the task-status push loop and the NPS prompt. They save with the rest of the tab; 0 or empty restores the default. The upgrade drain timeout and task-status push apply after a restart. While a self-upgrade is draining, the spoke reports the kicks it is waiting on and the time left in every heartbeat, so the hub holds the upgrade instead of abandoning it as orphaned; the hub timeline and stuck-upgrade alert show the drain. The cluster ID and hive type are shown read-only because hub provisioning writes them, as are the deprecated `contribute_already_done_unverified_hold_days` and `contribute_close_already_done` keys and the announcement ID the dashboard mints. The hub-only provision-request Discord webhook and NPS relay pull secret are write-only: the tab shows only whether each is set.

## Model Gateways

The OpenAI-compatible gateways (LiteLLM, OpenRouter, vLLM, and others) agents send inference through, and their keys. See [Configure model gateways and keys](hosted-hub.md#configure-model-gateways-and-keys).

## Appearance

The dashboard's look, stored under `dashboard.theme_overrides`: a background image (URL, opacity, position, size, attachment), the UI and monospace fonts, CSS token overrides, light-mode token overrides and custom CSS. See [Appearance themes](dashboard.md#appearance-themes).

## Agents

The managed agents (click a name to open that agent's settings tabs, described under [Agent: General](#agent-general) and below) and the **Backend placement policy**, which restricts the CLI or inference backends agents may use. Deny wins over allow, and an empty allow list permits every supported backend. See [Agent Configuration](agent-configuration.md).

## Workflows

The workflow library: the workflows this hive can run for its repos. Each control's **?** tooltip explains it.

## Inbox

Settings for the [Inbox](dashboard-sections.md#inbox) section. See [Input methods and the Inbox](inbox-input-methods.md) and [Inbox and voice sources](inbox-voice-sources.md).

## Project Observability

Targets for the managed project's telemetry and operations agents. Platforms are referenced by name only (an endpoint environment variable and a credential secret reference); literal URLs, tokens and API keys are refused. Hive's own OpenTelemetry export is set elsewhere. See [How to opt in](telemetry.md#how-to-opt-in-governorproject_observability).

## Meetings

The connections Hive uses to join meetings: the Vexa tenant key and, for Discord, the bot token and the transcription and speech tokens. The tab shows only whether each secret is set. See [Meetings](meetings.md).

## Forge App

The GitHub App (or other forge app) this hive authenticates as, the per-app key files it holds, and the GitHub API budget. See [GitHub App setup](github-app-setup.md) and [GitHub API budget governor](operator-reference/sweeps-and-operations.md#github-api-budget-governor).

## Bob

The hive-wide API key for the IBM Bob CLI backend (agents that use bob cannot start without it) and the Bob session prefix. See [Tagging Bob sessions for Bobalytics](backend-support-tiers.md#tagging-bob-sessions-for-bobalytics).

## Variables

Named variables that agents' prompt templates can use: each has a name, a type, a scope, and a value or an environment variable, with an optional default.

## Dashboard

Settings for the dashboard itself (`dashboard.*`): its port and agent poll interval, the Experiment Lab, whether the header also shows the usage gauge, hub-proxied auth, GitHub sign-in, Sessions, the dashboard auth token, the access allowlist, and the issue bands.

**GitHub sign-in** (`dashboard.github_sign_in`, on by default) offers the "Sign in with GitHub" device flow on the login page. Turn it off on a hive whose people all sign in through the hub: the login page then shows a "Sign in through the hub" link instead, `/api/gh-user-auth/start` and `/api/gh-user-auth/poll` refuse, and `hivectl login` to this hive stops working. The change applies without a restart. The link points at `dashboard.hub_sign_in_url`, else the `HIVE_HUB_URL` environment variable, else `hub.url`, and needs `hive_id`; the toggle shows the link it will use. When the hive cannot build one, the toggle is disabled and the server refuses to save it off, because the login page would then offer no way to sign in and lock everyone out, the owner included.

**Inception (spek) sessions** (`dashboard.sessions.enabled`, on by default) turns on the [Inception (spek)](dashboard-sections.md#inception-spek) section, named shared CLI-agent terminals inside the dashboard that open in the hive's shared spek workspace, which replaced the removed Inception wizard. Set it to false to hide the section. The `HIVE_SESSIONS_ENABLED` environment variable, when set, overrides the switch, and the dialog says so. Beside it are the three limits, `dashboard.sessions.max_sessions`, `dashboard.sessions.max_per_user` and `dashboard.sessions.idle_ttl_hours`; 0 keeps the built-in default (12, 4 and 72), and `HIVE_SESSIONS_MAX`, `HIVE_SESSIONS_MAX_PER_USER` and `HIVE_SESSIONS_IDLE_TTL_HOURS` override them. **Answer the CLI startup screens** (`dashboard.sessions.autostart`, on by default) lets the hive take a starting session's CLI through its startup screens (first-run notice, trust for the session's folder, onboarding) to its input prompt, so the first request runs without a person; it never answers a permission prompt and never signs in, and `HIVE_SESSIONS_AUTOSTART` overrides the switch. See the [Sessions API](api-reference/sessions.md).

**Also show the usage gauge in the header** (`dashboard.subscription_gauge_header`, off by default) adds a small fuel-dial chip to the header on every page, showing how much is left in the limit closest to running out across the plans the hive's agents spend (Claude, Copilot and others). The full gauge, with a row per provider, is on the Overview by default for the owner and can be hidden there from the Overview settings. Both show the measurements hive already makes; only the owner sees them; and they show the providers' plans, not the hive's token budget. See [Usage gauge](dashboard.md#usage-gauge).

## Compliance

How your configuration maps to SOC 2, FedRAMP, and ISO 27001 controls. See [The Settings → Compliance tab](compliance.md#the-settings--compliance-tab).

## Agent: General

One agent's identity, engine, model, and behaviour: name, role, kick template, operating mode, sandbox, CLI pin, caveman, Jev, and explain modes, and the repos and keywords it works on. See [Agent Configuration](agent-configuration.md#anatomy-of-an-agent).

"Sandbox settings: image, runtime and Job" also has the agent's own Job scheduling (node selector, resource limits and requests, tolerations, image pull secrets, schedule timeout) and "Node preflight" with an override switch: off inherits `agent_sandbox.preflight`, on uses this agent's list, and an empty list turns preflight off for it. "Workspace setup" works the same way with "Use this agent's own setup" (`sandbox.setup`; empty lists turn it off). "In effect for this agent" shows each resolved value, including the preflight and the setup step, and whether it comes from this agent or from `agent_sandbox` defaults, and warns when the agent's own Job block hides hive-wide scheduling fields. See [Setting up a sandbox from the dashboard](sandbox-isolation.md#setting-up-a-sandbox-from-the-dashboard).

## Agent: Pipeline

The deterministic pre-kick pipeline stages that run for this agent before the model is invoked. Turn a stage off to skip it for this agent. Each stage's **?** tooltip explains the stage. See [Agent Configuration](agent-configuration.md).

## Agent: Hooks

Shell scripts the agent runs before each governor kick (pre-kick) and after it finishes and returns to idle (post-idle). Pre-kick scripts run in order. These per-agent scripts are separate from the hive-wide [state-triggered hooks](hooks.md).
