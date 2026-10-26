# Dashboard settings explained

This page is the starting point for the dashboard's **Settings** tabs. It has one entry per tab: what the tab controls and which page explains it in depth.

Every setting on a Settings tab has a small **?** mark next to its name. Rest the pointer on it, or move to it with the Tab key, to see a short explanation of the setting. Click it, tap it, or press Enter on it to open the page that explains the setting in a new tab. When a setting has its own guide, the mark opens that guide; otherwise it opens this page at the tab's entry. Clicking a mark never changes the setting.

For the dashboard's other sections, see [Dashboard sections explained](dashboard-sections.md). For which file a saved setting is written to, see [Spoke configuration layering](config-layering.md).

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
- Linear agent: [Linear agent integration](linear-agent.md).
- Upstream watch: [Upstream watch](upstream-watch.md#configuration).

The other features on this tab (OpenTelemetry export, external execution, auto-plan, issue claims, PR follow-up, long-running runs, provider rotation, merge recommendations, convergence rollout, stall replan, and question auto-close) are explained by their **?** tooltips, and their configuration keys are listed in the [Hive operator reference](operator-reference.md#configuration-blocks).

## Knowledge

Whether the public read-only knowledge MCP endpoint is on, the knowledge pipeline, and the external knowledge connectors. The **Knowledge pipeline** block sets the bead synthesizer (schedule, target layer, minimum confidence, per-cycle fact and GitHub API caps, bead retention), scheduled curator promotion (schedule, threshold, source and target layers), repository code maps (schedule, layer, size cap) and the kick primer's fact and byte caps. Turning the bead synthesizer on or off applies at once; the other pipeline settings apply after a restart, and 0 or empty restores the default. The engine, the vault paths and the primer merge strategy are shown read-only because they are set in `hive.yaml`. The repositories to map and the primer priority list stay yaml-only. See [Knowledge curator](knowledge-curator.md) and [Knowledge connectors](knowledge-connectors.md).

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

## Compliance

How your configuration maps to SOC 2, FedRAMP, and ISO 27001 controls. See [The Settings → Compliance tab](compliance.md#the-settings--compliance-tab).

## Agent: General

One agent's identity, engine, model, and behaviour: name, role, kick template, operating mode, sandbox, CLI pin, caveman, Jev, and explain modes, and the repos and keywords it works on. See [Agent Configuration](agent-configuration.md#anatomy-of-an-agent).

## Agent: Pipeline

The deterministic pre-kick pipeline stages that run for this agent before the model is invoked. Turn a stage off to skip it for this agent. Each stage's **?** tooltip explains the stage. See [Agent Configuration](agent-configuration.md).

## Agent: Hooks

Shell scripts the agent runs before each governor kick (pre-kick) and after it finishes and returns to idle (post-idle). Pre-kick scripts run in order. These per-agent scripts are separate from the hive-wide [state-triggered hooks](hooks.md).
