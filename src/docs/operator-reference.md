# Hive operator reference

This page is a concise operator reference for fields and runtime knobs that are
easy to miss in `hive.yaml.example`. It was checked against `pkg/config/config.go`
and `cmd/hive/main.go` on branch `v4`.

For the full centralized environment variable table, including hub, backup,
inference, deployment, contributor, and legacy helper-script variables, see
[Environment variable reference](env-vars.md).


The reference is organized into the sections below. Existing links to this page remain available here.

<a id="agent-go-toolchain-shim"></a>
- [Agent Go toolchain shim](operator-reference/README.md#agent-go-toolchain-shim)
<a id="abandoned-contributor-prs"></a>
- [Abandoned contributor PRs](operator-reference/README.md#abandoned-contributor-prs)
<a id="minimum-required-configuration"></a>
- [Minimum required configuration](operator-reference/README.md#minimum-required-configuration)
<a id="github-api-quota--who-is-spending-it"></a>
- [GitHub API quota — who is spending it?](operator-reference/README.md#github-api-quota--who-is-spending-it)
<a id="opentelemetry-github-api-spend-and-cache-freshness"></a>
- [OpenTelemetry: GitHub API spend and cache freshness](operator-reference/README.md#opentelemetry-github-api-spend-and-cache-freshness)
<a id="configuration-blocks"></a>
- [Configuration blocks](operator-reference/README.md#configuration-blocks)
<a id="agent-runtime-blocks-configagentruntimeconfig"></a>
  - [Agent-runtime blocks (`config.AgentRuntimeConfig`)](operator-reference/README.md#agent-runtime-blocks-configagentruntimeconfig)
<a id="notable-fields"></a>
- [Notable fields](operator-reference/README.md#notable-fields)
<a id="label-driven-approval"></a>
  - [Label-driven approval](operator-reference/README.md#label-driven-approval)
<a id="github-pr-request-creation"></a>
  - [GitHub PR-request creation](operator-reference/README.md#github-pr-request-creation)
<a id="issue-request-quarantine-issue-requestsfailed"></a>
  - [Issue-request quarantine (`issue-requests/*.failed`)](operator-reference/README.md#issue-request-quarantine-issue-requestsfailed)
<a id="app-self-merge-sweep-auto_merge"></a>
- [App self-merge sweep (`auto_merge`)](operator-reference/README.md#app-self-merge-sweep-auto_merge)
<a id="supersession-sweep-supersession_sweep"></a>
- [Supersession sweep (`supersession_sweep`)](operator-reference/sweeps-and-operations.md#supersession-sweep-supersession_sweep)
<a id="stage-labels-governorlabelsstage"></a>
- [Stage labels (`governor.labels.stage`)](operator-reference/sweeps-and-operations.md#stage-labels-governorlabelsstage)
<a id="central-stage-engine-rollout"></a>
  - [Central stage engine rollout](operator-reference/sweeps-and-operations.md#central-stage-engine-rollout)
<a id="shared-ci-rerun-sweep-shared_ci_rerun"></a>
- [Shared-CI rerun sweep (`shared_ci_rerun`)](operator-reference/sweeps-and-operations.md#shared-ci-rerun-sweep-shared_ci_rerun)
<a id="open-pr-dispatch-guard-and-duplicate-fix-sweep-scanner"></a>
- [Open-PR dispatch guard and duplicate-fix sweep (`scanner`)](operator-reference/sweeps-and-operations.md#open-pr-dispatch-guard-and-duplicate-fix-sweep-scanner)
<a id="what-hive-writes-to-github-and-when"></a>
- [What hive writes to GitHub, and when](operator-reference/sweeps-and-operations.md#what-hive-writes-to-github-and-when)
<a id="configuration"></a>
  - [Configuration](operator-reference/sweeps-and-operations.md#configuration)
<a id="backfill-guard"></a>
  - [Backfill guard](operator-reference/sweeps-and-operations.md#backfill-guard)
<a id="duplicate-write-suppression"></a>
  - [Duplicate-write suppression](operator-reference/sweeps-and-operations.md#duplicate-write-suppression)
<a id="notification-mode-githubnotificationsmode"></a>
  - [Notification mode (`github.notifications.mode`)](operator-reference/sweeps-and-operations.md#notification-mode-githubnotificationsmode)
<a id="suspicious-activity-alerts-sentinel"></a>
- [Suspicious-activity alerts (`sentinel`)](operator-reference/sweeps-and-operations.md#suspicious-activity-alerts-sentinel)
<a id="image-provenance-and-tags"></a>
- [Image provenance and tags](operator-reference/sweeps-and-operations.md#image-provenance-and-tags)
<a id="one-source-of-truth-for-standalone-assets"></a>
  - [One source of truth for standalone assets](operator-reference/sweeps-and-operations.md#one-source-of-truth-for-standalone-assets)
<a id="choosing-a-tag"></a>
  - [Choosing a tag](operator-reference/sweeps-and-operations.md#choosing-a-tag)
<a id="verify-and-pin-a-digest"></a>
  - [Verify and pin a digest](operator-reference/sweeps-and-operations.md#verify-and-pin-a-digest)
<a id="scheduled-upgrade-progress-and-history"></a>
- [Scheduled-upgrade progress and history](operator-reference/sweeps-and-operations.md#scheduled-upgrade-progress-and-history)
<a id="governor-cadence-and-budget"></a>
- [Governor cadence and budget](operator-reference/sweeps-and-operations.md#governor-cadence-and-budget)
<a id="kick-size-and-the-list-caps"></a>
  - [Kick size and the list caps](operator-reference/sweeps-and-operations.md#kick-size-and-the-list-caps)
<a id="what-consumes-tokens-while-agents-are-paused"></a>
  - [What consumes tokens while agents are paused](operator-reference/sweeps-and-operations.md#what-consumes-tokens-while-agents-are-paused)
<a id="github-api-budget-governor"></a>
  - [GitHub API budget governor](operator-reference/sweeps-and-operations.md#github-api-budget-governor)
<a id="merge-hotspot-advisor-governorhotspot_"></a>
- [Merge-hotspot advisor (`governor.hotspot_*`)](operator-reference/sweeps-and-operations.md#merge-hotspot-advisor-governorhotspot_)
<a id="delivery-sla"></a>
- [Delivery SLA](operator-reference/sweeps-and-operations.md#delivery-sla)
<a id="fleet-breaker"></a>
- [Fleet breaker](operator-reference/runtime-and-quota.md#fleet-breaker)
<a id="self-reported-issues"></a>
- [Self-reported issues](operator-reference/runtime-and-quota.md#self-reported-issues)
<a id="agent-cli-session-recycling"></a>
- [Agent CLI session recycling](operator-reference/runtime-and-quota.md#agent-cli-session-recycling)
<a id="github-api-quota"></a>
- [GitHub API quota](operator-reference/runtime-and-quota.md#github-api-quota)
<a id="webhook-driven-pr-cache-invalidation"></a>
  - [Webhook-driven PR cache invalidation](operator-reference/runtime-and-quota.md#webhook-driven-pr-cache-invalidation)
<a id="what-the-operator-configures"></a>
    - [What the operator configures](operator-reference/runtime-and-quota.md#what-the-operator-configures)
<a id="what-a-delivery-does"></a>
    - [What a delivery does](operator-reference/runtime-and-quota.md#what-a-delivery-does)
<a id="polling-cadence-vs-webhook-patch-vs-reconcile"></a>
    - [Polling cadence vs webhook patch vs reconcile](operator-reference/runtime-and-quota.md#polling-cadence-vs-webhook-patch-vs-reconcile)
<a id="reading-from-the-forgestate-cache-forgestateenabled-true"></a>
      - [Reading from the ForgeState cache (`forgestate.enabled: true`)](operator-reference/runtime-and-quota.md#reading-from-the-forgestate-cache-forgestateenabled-true)
<a id="rate-limit-pacing-priority"></a>
      - [Rate-limit pacing priority](operator-reference/runtime-and-quota.md#rate-limit-pacing-priority)
<a id="event-driven-readers-forgestate-phase-2d"></a>
      - [Event-driven readers (ForgeState phase 2d)](operator-reference/runtime-and-quota.md#event-driven-readers-forgestate-phase-2d)
<a id="hive_github_token-permissions"></a>
- [`HIVE_GITHUB_TOKEN` permissions](operator-reference/runtime-and-quota.md#hive_github_token-permissions)
<a id="github-app-permissions-the-hive-needs-and-what-each-one-unblocks"></a>
  - [GitHub App permissions the hive needs, and what each one unblocks](operator-reference/runtime-and-quota.md#github-app-permissions-the-hive-needs-and-what-each-one-unblocks)
<a id="onboarding-errors-point-to-the-getting-started-guide"></a>
  - [Onboarding errors point to the Getting Started guide](operator-reference/runtime-and-quota.md#onboarding-errors-point-to-the-getting-started-guide)
<a id="runtime-diagnostics-debug-gomemlimit-mem-log-line"></a>
- [Runtime diagnostics (`debug`, `GOMEMLIMIT`, `mem:` log line)](operator-reference/runtime-and-quota.md#runtime-diagnostics-debug-gomemlimit-mem-log-line)
<a id="cmdhive-flags-and-environment"></a>
- [`cmd/hive` flags and environment](operator-reference/runtime-and-quota.md#cmdhive-flags-and-environment)
<a id="startup"></a>
  - [Startup](operator-reference/runtime-and-quota.md#startup)
<a id="github-and-dashboard-auth"></a>
  - [GitHub and dashboard auth](operator-reference/runtime-and-quota.md#github-and-dashboard-auth)
<a id="hosted-spoke-and-fleet-metadata"></a>
  - [Hosted-spoke and fleet metadata](operator-reference/runtime-and-quota.md#hosted-spoke-and-fleet-metadata)
<a id="dashboard-metrics"></a>
  - [Dashboard metrics](operator-reference/runtime-and-quota.md#dashboard-metrics)
<a id="knowledge-context7-content-caps"></a>
  - [Knowledge: Context7 content caps](operator-reference/runtime-and-quota.md#knowledge-context7-content-caps)
<a id="inference-endpoint-fallbacks"></a>
  - [Inference endpoint fallbacks](operator-reference/runtime-and-quota.md#inference-endpoint-fallbacks)
