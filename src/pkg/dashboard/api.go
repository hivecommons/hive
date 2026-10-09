package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/github"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/timeline"
)

func (s *Server) RegisterAPI(deps *Dependencies) {
	if s.logger == nil {
		s.logger = slog.Default()
	}
	s.deps = deps
	s.loadSidebarFromDisk()
	s.registerContributeRoutes()
	// Issue claims (hivecommons/hive#8380). Routes register unconditionally;
	// handlers answer "not enabled" when deps.IssueClaims is nil.
	s.registerClaimsRoutes()
	// Approval desk (RFC #4000). Routes register unconditionally; the handlers
	// report "not enabled" when deps.ApprovalInbox is nil, so a disabled desk is
	// an honest 200 the panel can render rather than a 404 that looks broken.
	s.registerApprovalRoutes()
	s.registerSwarmRoutes()

	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("GET /api/version/release-notes", s.handleVersionReleaseNotes)
	s.mux.HandleFunc("GET /api/style", s.handleStyle)
	s.mux.HandleFunc("GET /api/themes", s.handleThemesList)
	s.mux.HandleFunc("GET /api/theme.css", s.handleThemeCSS)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/config/download", s.handleConfigDownload)
	s.mux.HandleFunc("GET /api/config/export", s.handleConfigExport)
	s.mux.HandleFunc("GET /api/config/export.json", s.handleConfigExport)
	s.mux.HandleFunc("GET /api/config/provenance", s.handleConfigProvenance)
	s.mux.HandleFunc("GET /api/config/variables", s.handleVariablesList)
	s.mux.HandleFunc("GET /api/config/authorized-users", s.handleAuthorizedUsersList)
	s.mux.HandleFunc("GET /api/config/dashboard/theme", s.handleDashboardThemeGet)
	s.mux.HandleFunc("PUT /api/config/dashboard/theme", s.handleDashboardThemePut)
	s.mux.HandleFunc("PUT /api/config/variables/{name}", s.handleVariableUpsert)
	s.mux.HandleFunc("DELETE /api/config/variables/{name}", s.handleVariableDelete)
	s.mux.HandleFunc("GET /api/audit", s.handleAuditLog)
	s.mux.HandleFunc("GET /api/providers/headroom", s.handleProvidersHeadroom)
	s.mux.HandleFunc("GET /api/presence", s.handlePresenceSnapshot)
	s.mux.HandleFunc("POST /api/presence", s.handlePresence)
	s.mux.HandleFunc("GET /api/prompt-history", s.handlePromptHistory)
	s.mux.HandleFunc("POST /api/self-upgrade", s.handleSelfUpgrade)
	s.mux.HandleFunc("POST /api/release-channel", s.handleReleaseChannelSwitch)
	// Self-service, owner-only spoke backup (encrypted; includes the bead
	// ledger the fleet-wide hub backup excludes — see issue #2318).
	s.mux.HandleFunc("GET /api/backup/status", s.handleBackupStatus)
	s.mux.HandleFunc("POST /api/backup", s.handleBackupDownload)
	s.mux.HandleFunc("POST /api/banner-dismissed", s.handleBannerDismissed)
	// NPS feedback prompt (#9610): status gates the prompt; submit forwards a
	// response to the hub over the spoke's authenticated hub link. See nps.go.
	s.mux.HandleFunc("GET /api/feedback/nps/status", s.handleNPSStatus)
	s.mux.HandleFunc("POST /api/feedback/nps", s.handleNPSSubmit)
	s.mux.HandleFunc("GET /api/feedback/status", s.handleFeedbackStatus)
	s.mux.HandleFunc("POST /api/feedback/report", s.handleFeedbackReport)
	s.mux.HandleFunc("GET /api/feedback/mine", s.handleFeedbackMine)
	s.mux.HandleFunc("GET /api/snapshot/frame-ancestors", s.handleSnapshotFrameAncestors)
	s.mux.HandleFunc("GET /api/snapshot", s.handleSnapshotAPI)
	s.mux.HandleFunc("GET /snapshot", s.handleSnapshotPage)
	s.mux.HandleFunc("GET /api/history", s.handleHistory)
	s.mux.HandleFunc("GET /api/trends", s.handleTrends)
	s.mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	s.mux.HandleFunc("GET /api/lifecycle-timeline", s.handleLifecycleTimeline)
	s.mux.HandleFunc("GET /api/pr-throughput", s.handlePRThroughput)
	s.mux.HandleFunc("GET /api/runs", s.handleRunsList)
	s.mux.HandleFunc("GET /api/runs/audit", s.handleRunAuditIndex)
	s.mux.HandleFunc("POST /api/runs/audit", s.handleRunAudit)
	s.mux.HandleFunc("GET /api/runs/{key}/trace", s.handleRunTrace)
	s.mux.HandleFunc("GET /api/runs/{key}/checkpoint", s.handleRunCheckpointGet)
	s.mux.HandleFunc("POST /api/runs/{key}/checkpoint", s.handleRunCheckpointDecision)
	s.mux.HandleFunc("GET /api/runs/{key}", s.handleRunGet)
	s.mux.HandleFunc("POST /api/runs/{key}/reset", s.handleRunReset)
	s.mux.HandleFunc("GET /api/campaigns", s.handleCampaignsList)
	s.mux.HandleFunc("GET /api/campaigns/{id}", s.handleCampaignGet)
	s.mux.HandleFunc("POST /api/campaigns/{id}/resume", s.handleCampaignResume)
	s.mux.HandleFunc("POST /api/campaigns/{id}/release", s.handleCampaignRelease)
	s.mux.HandleFunc("POST /api/campaigns/{id}/revise", s.handleCampaignRevise)
	s.mux.HandleFunc("POST /api/campaigns/{id}/recheck", s.handleCampaignRecheck)
	s.mux.HandleFunc("GET /api/campaigns/{id}/jam", s.handleCampaignJamGet)
	s.mux.HandleFunc("POST /api/campaigns/{id}/jam", s.handleCampaignJamPost)
	s.mux.HandleFunc("GET /api/campaigns/{id}/jam/threads", s.handleCampaignJamThreadsGet)
	s.mux.HandleFunc("POST /api/campaigns/{id}/jam/threads", s.handleCampaignJamThreadsPost)
	s.mux.HandleFunc("POST /api/campaigns/{id}/jam/agents", s.handleCampaignJamAgentsPost)
	s.mux.HandleFunc("GET /api/campaigns/{id}/jam/suggestions", s.handleCampaignJamSuggestionsGet)
	s.mux.HandleFunc("POST /api/campaigns/{id}/jam/suggestions", s.handleCampaignJamSuggestionsPost)
	s.mux.HandleFunc("GET /api/campaigns/{id}/jam/polls", s.handleCampaignJamPollsGet)
	s.mux.HandleFunc("POST /api/campaigns/{id}/jam/polls", s.handleCampaignJamPollsPost)
	s.mux.HandleFunc("GET /api/campaigns/{id}/jam/project-sync", s.handleCampaignJamProjectSyncGet)
	s.mux.HandleFunc("POST /api/campaigns/{id}/jam/project-sync", s.handleCampaignJamProjectSyncPost)
	s.mux.HandleFunc("GET /api/campaigns/{id}/jam/ws", s.handleCampaignJamWebSocket)
	s.mux.HandleFunc("GET /api/widget", s.handleWidget)
	s.mux.HandleFunc("GET /api/pane/{agent}", s.handlePane)
	// Full retained scrollback of an agent's latest run, as plain text (#3693).
	// Backs the Terminal's "view / download full log" controls.
	s.mux.HandleFunc("GET /api/agents/{name}/log", s.handleAgentFullLog)
	// URLs visible in the agent's pane, joined across terminal wrapping, for
	// the dashboard's click-to-copy control (#5188). The terminal itself
	// cannot deliver a copy, so the copy is done server-side.
	s.mux.HandleFunc("GET /api/agents/{name}/terminal-urls", s.handleAgentTerminalURLs)
	// Durable per-kick run-log history (#4296, #4295): list archived kick
	// logs, fetch one, and a minimal HTML index page linked from agent cards.
	s.mux.HandleFunc("GET /api/agents/{name}/kicks", s.handleAgentKickLogList)
	s.mux.HandleFunc("GET /api/agents/{name}/kicks/{id}", s.handleAgentKickLog)
	s.mux.HandleFunc("GET /agents/{name}/kicks", s.handleAgentKickHistoryPage)

	s.mux.HandleFunc("GET /api/role", s.handleRole)

	s.mux.HandleFunc("POST /api/kick/{agent}", s.handleKick)
	// Outcome of the most recent asynchronous kick (#5325). The POST answers
	// 202 as soon as the kick is queued; delivery success or failure is read
	// from here, off the request path and therefore never proxy-timed-out.
	s.mux.HandleFunc("GET /api/kick/{agent}/status", s.handleKickStatus)
	s.mux.HandleFunc("POST /api/switch/{agent}/{backend}", s.handleSwitch)
	s.mux.HandleFunc("POST /api/model/{agent}/{model}", s.handleModelSet)
	s.mux.HandleFunc("POST /api/effort/{agent}/{effort}", s.handleEffortSet)
	s.mux.HandleFunc("POST /api/pause/{agent}", s.handlePause)
	s.mux.HandleFunc("POST /api/resume/{agent}", s.handleResume)
	s.mux.HandleFunc("GET /api/agent-state/{agent}", s.handleAgentState)
	s.mux.HandleFunc("GET /api/breaker", s.handleBreakerState)
	s.mux.HandleFunc("POST /api/breaker/engage", s.handleBreakerEngage)
	s.mux.HandleFunc("POST /api/breaker/release", s.handleBreakerRelease)
	s.mux.HandleFunc("POST /api/pin/{agent}/{dimension}", s.handlePin)
	s.mux.HandleFunc("POST /api/unpin/{agent}/{dimension}", s.handleUnpin)
	// Write-side twin of the terminal-urls copy control: the dashboard terminal
	// can neither hand an operator a wrapped login URL nor accept the code back.
	s.mux.HandleFunc("POST /api/agents/{name}/login-code", s.handleAgentLoginCode)
	s.mux.HandleFunc("POST /api/restart/{agent}", s.handleRestart)
	s.mux.HandleFunc("POST /api/reset-restarts/{agent}", s.handleResetRestarts)

	s.mux.HandleFunc("GET /api/token-access", s.handleTokenAccess)
	s.mux.HandleFunc("GET /api/tokens", s.handleTokens)
	s.mux.HandleFunc("GET /api/cost", s.handleCost)
	s.mux.HandleFunc("GET /api/repo-activity", s.handleRepoActivity)
	s.mux.HandleFunc("GET /api/repo-cost", s.handleRepoCost)
	s.mux.HandleFunc("GET /api/cost/history", s.handleCostHistory)
	// #4298: "for every recent reset, how much of the budget had been used".
	s.mux.HandleFunc("GET /api/budget/history", s.handleBudgetHistory)
	s.mux.HandleFunc("GET /api/trend/history", s.handleTrendHistory)
	s.mux.HandleFunc("GET /api/overview/history", s.handleOverviewHistory)
	s.mux.HandleFunc("GET /api/timeseries", s.handleTimeSeries)
	s.mux.HandleFunc("GET /api/model-advisor", s.handleModelAdvisor)
	s.mux.HandleFunc("GET /api/governor/pr-models", s.handleGovernorPRModels)
	s.mux.HandleFunc("GET /api/budget-ignore", s.handleBudgetIgnoreGet)
	s.mux.HandleFunc("POST /api/budget-ignore", s.handleBudgetIgnoreSet)

	s.mux.HandleFunc("GET /api/gh-auth", s.handleGHAuth)
	s.mux.HandleFunc("GET /api/gh-rate-limits", s.handleGHRateLimits)
	s.mux.HandleFunc("GET /api/gh-user-auth/status", s.handleGHUserAuthStatus)
	s.mux.HandleFunc("POST /api/gh-user-auth/start", s.handleGHUserAuthStart)
	s.mux.HandleFunc("POST /api/gh-user-auth/poll", s.handleGHUserAuthPoll)
	s.mux.HandleFunc("POST /api/gh-user-auth/logout", s.handleGHUserAuthLogout)
	s.mux.HandleFunc("GET /api/gh-user-auth/session", s.handleGHUserAuthSession)

	s.registerClaudeAuthRoutes()
	s.registerCopilotAuthRoutes()
	s.mux.HandleFunc("GET /api/summaries", s.handleSummaries)
	s.mux.HandleFunc("POST /api/prs/{owner}/{repo}/{number}/queue-automerge", s.handleQueuePRAutoMerge)

	s.mux.HandleFunc("GET /api/config/agent/{name}", s.handleAgentConfigGet)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/general", s.handleAgentConfigGeneral)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/cadences", s.handleAgentConfigCadences)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/models", s.handleAgentConfigModels)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/pipeline", s.handleAgentConfigPipeline)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/hooks", s.handleAgentConfigHooks)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/restrictions", s.handleAgentConfigRestrictions)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/stats", s.handleAgentConfigStats)
	s.mux.HandleFunc("GET /api/config/agent/{name}/prompt", s.handleAgentPrompt)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/prompt", s.handleAgentPromptSave)
	s.mux.HandleFunc("GET /api/config/agent/{name}/export", s.handleAgentExport)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/channels", s.handleAgentConfigChannels)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/tools", s.handleAgentConfigTools)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/connections", s.handleAgentConfigConnections)
	s.mux.HandleFunc("GET /api/config/stat-sources", s.handleStatSources)

	s.mux.HandleFunc("GET /api/config/governor", s.handleGovernorConfigGet)
	s.mux.HandleFunc("PUT /api/config/governor/sensing", s.handleGovernorSensing)
	s.mux.HandleFunc("PUT /api/config/governor/thresholds", s.handleGovernorThresholds)
	s.mux.HandleFunc("GET /api/config/governor/cadence-scope", s.handleGovernorCadenceScopeGet)
	s.mux.HandleFunc("PUT /api/config/governor/cadence-scope", s.handleGovernorCadenceScope)
	s.mux.HandleFunc("GET /api/config/governor/threshold-scaling", s.handleGovernorThresholdScalingGet)
	s.mux.HandleFunc("PUT /api/config/governor/threshold-scaling", s.handleGovernorThresholdScaling)
	s.mux.HandleFunc("PUT /api/config/governor/labels", s.handleGovernorLabels)
	s.mux.HandleFunc("PUT /api/config/governor/budget", s.handleGovernorBudget)
	s.mux.HandleFunc("POST /api/config/governor/budget/reset", s.handleGovernorBudgetReset)
	s.mux.HandleFunc("GET /api/governor/notifications", s.handleGovernorNotificationsGet)
	s.mux.HandleFunc("PUT /api/governor/notifications", s.handleGovernorNotifications)
	s.mux.HandleFunc("PUT /api/config/governor/notifications", s.handleGovernorNotifications)
	s.mux.HandleFunc("PUT /api/config/governor/health", s.handleGovernorHealth)
	s.mux.HandleFunc("PUT /api/config/governor/watchdog", s.handleGovernorWatchdog)
	// Escalation breaker is a top-level Config field (not GovernorConfig), but
	// its UI lives on the governor Health tab — see api_escalation.go.
	s.mux.HandleFunc("GET /api/config/escalation", s.handleEscalationGet)
	s.mux.HandleFunc("PUT /api/config/escalation", s.handleEscalationPut)
	// Review-swarm merge gate is a top-level Config field (not GovernorConfig),
	// but its UI lives on the governor Features tab — see api_config_review.go.
	s.mux.HandleFunc("GET /api/config/review", s.handleReviewConfigGet)
	s.mux.HandleFunc("GET /api/review/outcomes", s.handleReviewOutcomes)
	s.mux.HandleFunc("GET /api/reviewer/accuracy", s.handleReviewerAccuracy)
	s.mux.HandleFunc("PUT /api/config/review", s.handleReviewConfigPut)
	s.mux.HandleFunc("PUT /api/config/governor/logging", s.handleGovernorLogging)
	s.mux.HandleFunc("PUT /api/config/governor/attribution", s.handleGovernorAttribution)
	s.mux.HandleFunc("PUT /api/config/governor/hub", s.handleGovernorHub)
	s.mux.HandleFunc("PUT /api/config/governor/litellm", s.handleGovernorLiteLLM)
	s.mux.HandleFunc("PUT /api/config/governor/trajectory", s.handleGovernorTrajectory)
	s.mux.HandleFunc("PUT /api/config/governor/features", s.handleGovernorFeatures)
	s.mux.HandleFunc("GET /api/config/governor/general-advanced", s.handleGovernorGeneralAdvancedGet)
	s.mux.HandleFunc("PUT /api/config/governor/general-advanced", s.handleGovernorGeneralAdvancedPut)

	// auto_merge is a top-level Config field (not GovernorConfig), but its UI
	// lives on the governor Features tab — see api_config_automerge.go.
	s.mux.HandleFunc("GET /api/config/auto-merge", s.handleAutoMergeGet)
	s.mux.HandleFunc("PUT /api/config/auto-merge", s.handleAutoMergePut)
	// convergence is a top-level Config field (not GovernorConfig); its UI
	// lives on the governor Features tab — see api_config_convergence.go
	// (#4263 off/shadow/enforce rollout). The soak endpoint is the
	// fixed-commit telemetry read path (convergence_soak.go).
	s.mux.HandleFunc("GET /api/config/convergence", s.handleConvergenceConfigGet)
	s.mux.HandleFunc("PUT /api/config/convergence", s.handleConvergenceConfigPut)
	s.mux.HandleFunc("GET /api/convergence/soak", s.handleConvergenceSoak)
	s.mux.HandleFunc("GET /api/config/governor/advisory", s.handleGovernorAdvisoryGet)
	s.mux.HandleFunc("PUT /api/config/governor/advisory", s.handleGovernorAdvisoryPut)
	s.mux.HandleFunc("GET /api/config/governor/replan", s.handleGovernorReplanGet)
	s.mux.HandleFunc("PUT /api/config/governor/replan", s.handleGovernorReplanPut)
	// Question auto-close (#9584 dashboard follow-up): settings toggle +
	// hours, plus a read-only live-schedule view backed by
	// Dependencies.QuestionAutoclose — see api_governor_question_autoclose.go.
	s.mux.HandleFunc("GET /api/config/governor/question-autoclose", s.handleGovernorQuestionAutocloseGet)
	s.mux.HandleFunc("PUT /api/config/governor/question-autoclose", s.handleGovernorQuestionAutocloseSet)
	s.mux.HandleFunc("GET /api/config/governor/question-autoclose/schedule", s.handleGovernorQuestionAutocloseSchedule)
	s.mux.HandleFunc("GET /api/config/governor/work-source", s.handleGovernorWorkSourceGet)
	s.mux.HandleFunc("PUT /api/config/governor/work-source", s.handleGovernorWorkSourcePut)
	s.mux.HandleFunc("PUT /api/config/governor/security", s.handleGovernorSecurity)
	s.mux.HandleFunc("GET /api/config/governor/project-observability", s.handleGovernorProjectObservabilityGet)
	s.mux.HandleFunc("PUT /api/config/governor/project-observability", s.handleGovernorProjectObservabilityPut)
	// Backup encryption key: presence-only status, set, and clear. The key
	// value is never returned by any of these (#4129).
	s.mux.HandleFunc("GET /api/config/governor/backup", s.handleBackupKeyStatus)
	s.mux.HandleFunc("PUT /api/config/governor/backup", s.handleBackupKeySet)
	s.mux.HandleFunc("DELETE /api/config/governor/backup", s.handleBackupKeyClear)
	// bob API key: PUT sets/replaces, DELETE revokes. Both are non-GET, so the
	// roleEnforcement middleware already 403s a read-only role — no separate
	// authorization rule is needed or wanted here.
	s.mux.HandleFunc("GET /api/config/governor/bob", s.handleGovernorBobStatus)
	s.mux.HandleFunc("PUT /api/config/governor/bob", s.handleGovernorBobKey)
	s.mux.HandleFunc("DELETE /api/config/governor/bob", s.handleGovernorBobKeyClear)
	// Live key validation (pasted or saved key) — see bob_key_probe.go.
	s.mux.HandleFunc("POST /api/config/governor/bob/test", s.handleGovernorBobKeyTest)
	s.mux.HandleFunc("POST /api/config/governor/litellm/test", s.handleGovernorLiteLLMTest)
	s.mux.HandleFunc("GET /api/config/governor/inference-auth", s.handleGovernorInferenceAuthGet)
	s.mux.HandleFunc("PUT /api/config/governor/inference-auth", s.handleGovernorInferenceAuthPut)
	s.mux.HandleFunc("GET /api/config/governor/gateways", s.handleGovernorGatewaysList)
	s.mux.HandleFunc("PUT /api/config/governor/gateways", s.handleGovernorGatewaysUpsert)
	s.mux.HandleFunc("DELETE /api/config/governor/gateways/{name}", s.handleGovernorGatewaysDelete)
	s.mux.HandleFunc("POST /api/config/governor/gateways/{name}/test", s.handleGovernorGatewaysTest)
	s.mux.HandleFunc("POST /api/config/governor/gateways/discover", s.handleGovernorGatewaysDiscover)
	s.registerOpenRouterRoutes()
	s.registerLinearAgentRoutes()
	s.mux.HandleFunc("GET /api/upstream-watch", s.handleUpstreamWatch)
	s.mux.HandleFunc("POST /api/config/governor/agents", s.handleGovernorAddAgent)
	s.mux.HandleFunc("DELETE /api/config/governor/agents/{name}", s.handleGovernorRemoveAgent)
	s.mux.HandleFunc("PUT /api/config/governor/repos", s.handleGovernorRepos)
	// Access probe run when the Repos tab adds a new repo: verifies the hive's
	// GitHub App is installed on the new repo's org before the repo is accepted,
	// and hands back the correct per-forge install URL when it is not.
	s.mux.HandleFunc("POST /api/config/governor/repos/check-access", s.handleGovernorRepoCheckAccess)
	s.mux.HandleFunc("PUT /api/config/github", s.handleConfigGitHub)
	// Read-only inventory for the Forge App tab: every App credential this
	// spoke holds (active config + per-app-id PVC keys), fingerprints only.
	s.mux.HandleFunc("GET /api/config/github/forge-apps", s.handleConfigGitHubForgeApps)

	s.mux.HandleFunc("GET /api/agents", s.handleAgentsList)
	s.mux.HandleFunc("POST /api/agents", s.handleAgentCreate)
	s.mux.HandleFunc("POST /api/agents/import", s.handleAgentImport)
	s.mux.HandleFunc("DELETE /api/agents/{name}", s.handleAgentDelete)

	s.mux.HandleFunc("GET /api/packs", s.handlePacksList)
	s.mux.HandleFunc("POST /api/packs/{level}/apply", s.handlePackApply)
	s.mux.HandleFunc("PUT /api/packs/level", s.handlePackSetLevel)

	// Operator-initiated refresh of the REPOSITORIES cards: re-enumerate
	// every watched repo's open issues/PRs now instead of waiting out the
	// governor's eval interval. Read-only — see handleReposRescan.
	s.mux.HandleFunc("POST /api/repos/rescan", s.handleReposRescan)

	// Per-repo agent pause (#6203): quiet one repo without stopping the hive.
	// The repo travels in the request body, not the path, because a repos entry
	// may be an explicit cross-org reference ("laredo/cuga-agent") whose slash
	// cannot live in a single {repo} path value.
	s.mux.HandleFunc("POST /api/repos/pause", s.handleRepoPause)
	s.mux.HandleFunc("POST /api/repos/resume", s.handleRepoResume)
	s.mux.HandleFunc("POST /api/repos/auto-merge", s.handleRepoAutoMerge)
	s.mux.HandleFunc("GET /api/repos/pauses", s.handleRepoPauses)
	s.mux.HandleFunc("GET /api/repos/{owner}/{repo}/hold-permission", s.handleRepoHoldPermission)
	s.mux.HandleFunc("POST /api/repos/{owner}/{repo}/items/{number}/hold", s.handleRepoItemHold)

	s.mux.HandleFunc("GET /api/acmm/evaluation", s.handleACMMEvaluation)
	s.mux.HandleFunc("POST /api/acmm/issue", s.handleACMMCreateIssue)
	s.mux.HandleFunc("GET /api/acmm-recommendation", s.handleACMMRecommendation)
	s.mux.HandleFunc("PUT /api/acmm-recommendation/repo-pin", s.handleACMMRepoPin)
	s.mux.HandleFunc("GET /api/hive-advice", s.handleHiveAdvice)
	s.mux.HandleFunc("GET /api/compliance/status", s.handleComplianceStatus)
	s.mux.HandleFunc("GET /api/compliance/posture", s.handleCompliancePosture)
	s.mux.HandleFunc("POST /api/compliance/posture/run", s.handleCompliancePostureRun)
	s.mux.HandleFunc("GET /api/compliance/posture/history", s.handleCompliancePostureHistory)
	s.mux.HandleFunc("GET /api/compliance/export", s.handleComplianceExport)
	s.mux.HandleFunc("GET /api/compliance/attestations", s.handleComplianceAttestations)
	s.mux.HandleFunc("POST /api/compliance/attestations", s.handleComplianceAttestationCreate)

	s.mux.HandleFunc("GET /api/config/sidebar", s.handleSidebarGet)
	s.mux.HandleFunc("PUT /api/config/sidebar", s.handleSidebarSet)
	s.mux.HandleFunc("GET /api/config/backends", s.handleBackends)
	s.mux.HandleFunc("GET /api/inference/models/{backend}", s.handleInferenceModels)

	s.mux.HandleFunc("GET /api/knowledge", s.handleKnowledgeList)
	s.mux.HandleFunc("GET /api/knowledge/export", s.handleKnowledgeExport)
	s.mux.HandleFunc("GET /api/knowledge/search", s.handleKnowledgeSearch)
	s.mux.HandleFunc("GET /api/knowledge/toc", s.handleKnowledgeTOC)
	s.mux.HandleFunc("GET /api/knowledge/entry/{id}", s.handleKnowledgeEntry)
	s.mux.HandleFunc("PUT /api/knowledge/entry/{id}/state", s.handleKnowledgeEntryState)
	// Anonymous, owner-switched, read-only MCP surface (#10615). POST only —
	// the mux answers other verbs with 405, and there is no SSE stream.
	s.mux.HandleFunc("POST /mcp/knowledge", s.handlePublicKnowledgeMCP) // path == publicKnowledgeMCPPath
	s.mux.HandleFunc("GET /api/knowledge/public", s.handlePublicKnowledgeGet)
	s.mux.HandleFunc("PUT /api/knowledge/public", s.handlePublicKnowledgePut)
	s.mux.HandleFunc("GET /api/knowledge/health", s.handleKnowledgeHealth)
	s.mux.HandleFunc("GET /api/knowledge/stats", s.handleKnowledgeStats)
	s.mux.HandleFunc("GET /api/knowledge/graph", s.handleKnowledgeGraph)
	s.mux.HandleFunc("GET /api/knowledge/fact-history", s.handleFactHistory)
	s.mux.HandleFunc("POST /api/knowledge/create", s.handleKnowledgeCreate)
	s.mux.HandleFunc("POST /api/knowledge/import", s.handleKnowledgeImport)
	// Channels (user-writable local vaults). Registered under /channels rather
	// than /vaults so they don't collide with the GET /api/knowledge/{layer}
	// wildcard below. This is the import-target surface fixed in #3581.
	s.mux.HandleFunc("GET /api/knowledge/channels", s.handleKnowledgeChannelsList)
	s.mux.HandleFunc("POST /api/knowledge/channels", s.handleKnowledgeChannelCreate)
	s.mux.HandleFunc("POST /api/knowledge/promote", s.handleKnowledgePromote)
	s.mux.HandleFunc("GET /api/knowledge/subscriptions", s.handleKnowledgeSubsList)
	s.mux.HandleFunc("POST /api/knowledge/subscriptions", s.handleKnowledgeSubsAdd)
	s.mux.HandleFunc("DELETE /api/knowledge/subscriptions", s.handleKnowledgeSubsRemove)
	s.mux.HandleFunc("PUT /api/knowledge/{layer}/{slug}", s.handleKnowledgeUpdate)
	s.mux.HandleFunc("DELETE /api/knowledge/{layer}/{slug}", s.handleKnowledgeDelete)
	s.mux.HandleFunc("GET /api/knowledge/{layer}", s.handleKnowledgeLayer)
	s.mux.HandleFunc("GET /api/knowledge/{layer}/{slug}", s.handleKnowledgeFact)
	s.mux.HandleFunc("PUT /api/knowledge/enabled", s.handleKnowledgeToggle)
	s.mux.HandleFunc("GET /api/knowledge/bead-synthesizer", s.handleBeadSynthStatus)
	s.mux.HandleFunc("PUT /api/knowledge/bead-synthesizer/enabled", s.handleBeadSynthToggle)
	s.mux.HandleFunc("GET /api/knowledge/vaults", s.handleVaultsList)
	s.mux.HandleFunc("POST /api/knowledge/vaults", s.handleVaultsConnect)
	s.mux.HandleFunc("DELETE /api/knowledge/vaults", s.handleVaultsDisconnect)
	s.mux.HandleFunc("POST /api/knowledge/vaults/reindex", s.handleVaultsReindex)
	s.mux.HandleFunc("GET /api/knowledge/vaults/{name}/facts", s.handleVaultFacts)
	s.mux.HandleFunc("GET /api/config/knowledge/connectors", s.handleKnowledgeConnectorsGet)
	s.mux.HandleFunc("PUT /api/config/knowledge/connectors", s.handleKnowledgeConnectorsPut)
	s.mux.HandleFunc("GET /api/config/knowledge/connectors/status", s.handleKnowledgeConnectorsStatus)
	s.mux.HandleFunc("POST /api/config/knowledge/connectors/validate", s.handleKnowledgeConnectorsValidate)
	s.mux.HandleFunc("POST /api/config/knowledge/connectors/{name}/sync", s.handleKnowledgeConnectorsSync)
	s.mux.HandleFunc("GET /api/knowledge/git-sources", s.handleGitSourcesList)
	s.mux.HandleFunc("POST /api/knowledge/git-sources", s.handleGitSourcesConnect)
	s.mux.HandleFunc("DELETE /api/knowledge/git-sources", s.handleGitSourcesDisconnect)
	s.mux.HandleFunc("POST /api/knowledge/obsidian/sync", s.handleObsidianSync)
	s.mux.HandleFunc("GET /api/knowledge/documents", s.handleDocumentsList)
	s.mux.HandleFunc("POST /api/knowledge/documents", s.handleDocumentsImport)
	s.mux.HandleFunc("GET /api/knowledge/documents/{slug}", s.handleDocumentGet)
	s.mux.HandleFunc("DELETE /api/knowledge/documents/{slug}", s.handleDocumentDelete)
	s.mux.HandleFunc("POST /api/knowledge/documents/{slug}/reimport", s.handleDocumentReimport)
	s.mux.HandleFunc("GET /api/knowledge/context7/search", s.handleContext7Search)
	s.mux.HandleFunc("POST /api/knowledge/cleanup-orphans", s.handleCleanupOrphans)

	s.mux.HandleFunc("GET /api/hive-id", s.handleHiveIDGet)
	s.mux.HandleFunc("PUT /api/hive-id", s.handleHiveIDSet)

	s.mux.HandleFunc("POST /api/inception/start", s.handleInceptionStart)
	s.mux.HandleFunc("POST /api/inception/scan", s.handleInceptionScan)
	s.mux.HandleFunc("GET /api/inception/state", s.handleInceptionState)
	s.mux.HandleFunc("POST /api/inception/questions", s.handleInceptionSetQuestions)
	s.mux.HandleFunc("POST /api/inception/answer", s.handleInceptionAnswer)
	s.mux.HandleFunc("POST /api/inception/facts", s.handleInceptionRecordFacts)
	s.mux.HandleFunc("GET /api/inception/scaffold", s.handleInceptionScaffold)
	s.mux.HandleFunc("POST /api/inception/approve", s.handleInceptionApprove)
	s.mux.HandleFunc("POST /api/inception/reset", s.handleInceptionReset)
	s.mux.HandleFunc("GET /api/inception/ideation-facts", s.handleInceptionIdeationFacts)
	s.mux.HandleFunc("GET /api/inception/download", s.handleInceptionDownload)
	s.mux.HandleFunc("GET /api/inception/has-files", s.handleInceptionHasFiles)
	s.mux.HandleFunc("PUT /api/inception/wiki-name", s.handleInceptionRenameWiki)
	s.mux.HandleFunc("POST /api/inception/import", s.handleInceptionImport)

	// Plan-review gate (Phase 2 planning intelligence). Mirrors /api/inception/*.
	// Phase 4 adds the issue entry point: mint an epic from a GitHub issue and
	// request its decomposition (the "Plan this issue" dashboard action).
	s.mux.HandleFunc("POST /api/plan/from-issue", s.handlePlanFromIssue)
	s.mux.HandleFunc("POST /api/plan/from-issue/design", s.handleDesignFromIssue)
	s.mux.HandleFunc("GET /api/plans", s.handlePlanList)
	s.mux.HandleFunc("GET /api/plan/{epicID}", s.handlePlanTree)
	s.mux.HandleFunc("POST /api/plan/{epicID}/approve", s.handlePlanApprove)
	s.mux.HandleFunc("POST /api/plan/{epicID}/design/approve", s.handlePlanDesignApprove)
	s.mux.HandleFunc("POST /api/plan/{epicID}/reject", s.handlePlanReject)
	s.mux.HandleFunc("POST /api/plan/{epicID}/child/{childID}", s.handlePlanChild)

	s.mux.HandleFunc("POST /api/chat", s.handleChat)

	s.mux.HandleFunc("GET /api/nous/status", s.handleNousStatus)
	s.mux.HandleFunc("GET /api/nous/ledger", s.handleNousLedger)
	s.mux.HandleFunc("GET /api/nous/principles", s.handleNousPrinciples)
	s.mux.HandleFunc("POST /api/nous/approve", s.handleNousApprove)
	s.mux.HandleFunc("POST /api/nous/abort", s.handleNousAbort)
	s.mux.HandleFunc("PUT /api/nous/mode", s.handleNousMode)
	s.mux.HandleFunc("PUT /api/nous/scope", s.handleNousScope)
	s.mux.HandleFunc("GET /api/nous/phase", s.handleNousPhase)
	s.mux.HandleFunc("PUT /api/nous/gate-decision", s.handleNousGateDecision)
	s.mux.HandleFunc("GET /api/nous/gate-pending", s.handleNousGatePending)
	s.mux.HandleFunc("POST /api/nous/gate-respond", s.handleNousGateRespond)
	s.mux.HandleFunc("GET /api/nous/gate-response", s.handleNousGateResponse)
	s.mux.HandleFunc("GET /api/nous/config", s.handleNousConfigGet)
	s.mux.HandleFunc("PUT /api/nous/config/goals", s.handleNousConfigGoals)
	s.mux.HandleFunc("PUT /api/nous/config/repos", s.handleNousConfigRepos)
	s.mux.HandleFunc("PUT /api/nous/config/output", s.handleNousConfigOutput)
	s.mux.HandleFunc("PUT /api/nous/config/fast-fail", s.handleNousConfigFastFail)
	s.mux.HandleFunc("PUT /api/nous/config/schedule", s.handleNousConfigSchedule)
	s.mux.HandleFunc("PUT /api/nous/config/controllables", s.handleNousConfigControllables)
	s.mux.HandleFunc("PUT /api/nous/config/principles", s.handleNousConfigPrinciples)
	s.mux.HandleFunc("DELETE /api/nous/principles/{id}", s.handleNousDeletePrinciple)

	s.mux.HandleFunc("GET /api/beads", s.handleBeadsList)
	s.mux.HandleFunc("GET /api/beads/{agent}", s.handleBeadsList)
	s.mux.HandleFunc("POST /api/beads/{agent}", s.handleBeadsCreate)
	s.mux.HandleFunc("POST /api/beads/reset", s.handleBeadsReset)
	s.mux.HandleFunc("POST /api/beads/reset/{agent}", s.handleBeadsResetAgent)

	s.mux.HandleFunc("GET /api/auth/token", s.handleAuthToken)

	// Watchdog activity readout for the Health tab (#7254): the watchdog-*
	// audit actions in a trailing window, bucketed per day, plus per-agent
	// liveness — the data an Observe → Heal decision rests on.
	s.mux.HandleFunc("GET /api/watchdog/activity", s.handleWatchdogActivity)

	// PR review queue (#9590): every open PR in the governed repos, agent-
	// or contributor-authored, in one ranked order with the reasons for each
	// position. Read-only; paged like /api/v1/queue (#6537).
	s.mux.HandleFunc("GET /api/review/queue", s.handleReviewQueue)
	// Review pipeline (#11086): the same queue, one card per PR placed in its
	// review stage (unreviewed … approved) with reviewers, severity counts,
	// loop counter and next action. Read-only; paged like /api/review/queue.
	s.mux.HandleFunc("GET /api/review/pipeline", s.handleReviewPipeline)
	s.mux.HandleFunc("POST /api/review/pipeline/{owner}/{repo}/{number}/send-to-human", s.handleReviewPipelineSendToHuman)
	// Review evidence (#11061): the per-PR evidence bundle as sealed on disk,
	// or zipped with the artifacts it references; owner/merger only.
	s.mux.HandleFunc("GET /api/review/evidence", s.handleReviewEvidence)
	s.mux.HandleFunc("GET /api/review/evidence/list", s.handleReviewEvidenceList)
	// write_surface is a top-level Config field; the lane write allowlist
	// editor lives on the governor Security tab (#9587, api_config_write_surface.go).
	s.mux.HandleFunc("GET /api/config/write-surface", s.handleWriteSurfaceGet)
	s.mux.HandleFunc("PUT /api/config/write-surface", s.handleWriteSurfacePut)
	// Settings → Compliance framework picker (#11080, api_compliance_settings.go).
	// Registered last so the api-reference citations above do not shift.
	s.mux.HandleFunc("PUT /api/config/governor/compliance", s.handleComplianceFrameworksPut)
}

var (
	versionHash  = "unknown"
	versionShort = "unknown"
	// versionBranch is the branch this binary was built from (ldflags via
	// cmd/hive). The self-version check compares against the tip of THIS
	// branch — a spoke running v3 must not be told it is "behind" v2.
	versionBranch = "unknown"
	// versionChannel is the release channel the Deployment image tracks, ""
	// when not channel-delivered (see SetReleaseChannel).
	versionChannel = ""
	// versionImageSource uses the existing cached Deployment lookup on each
	// poll, allowing a failed initial read to recover after its cache expires.
	versionImageSource func() string
	// versionPendingChannel is the channel the owner most recently selected via
	// POST /api/release-channel; cleared once the observed image tag matches.
	versionPendingChannelMu sync.Mutex
	versionPendingChannel   = ""
	// selfDeploymentImageForDashboard reads the Deployment image through the
	// injected source (SetDeploymentImageSource) so pkg/dashboard stays free of
	// pkg/hub. A var so tests can substitute a fixed image ref.
	selfDeploymentImageForDashboard = func() string {
		if versionImageSource == nil {
			return ""
		}
		return versionImageSource()
	}
	selfDeploymentImageSourceForDashboard = func() string {
		return spoke.SelfDeploymentImageSource()
	}
)

// defaultUpstreamBranch is the fallback branch for the self-version check
// when the build did not inject a branch (e.g. local `go run`), and for the
// clone command on the contribute onboarding page.
//
// v4, not v2: v2 is no longer maintained, so a build with no injected branch
// was comparing itself against — and telling contributors to clone — a branch
// that no longer receives the code this binary is built from. This is the same
// stale-constant defect upgradeBranchOrDefault documents in pkg/hub, where a
// hardcoded "v2" default resolved upgrade targets against a foreign branch.
const defaultUpstreamBranch = "v4"

func SetGitVersion(hash, short string) {
	versionHash = hash
	versionShort = short
}

// SetGitBranch records the branch the running binary was built from so the
// version check and Upgrade affordance compare against the right upstream.
func SetGitBranch(branch string) {
	versionBranch = branch
}

// SetReleaseChannel records the release channel this spoke's Deployment image
// tracks ("stable"/"candidate"/"edge"), or "" when it tracks a branch tag or
// SHA pin. Display-only: the navbar badge shows "stable (v4)" instead of the
// bare built-from branch. The upstream comparison logic is untouched — the
// binary is still a build of versionBranch.
func SetReleaseChannel(channel string) {
	versionChannel = channel
}

func setPendingReleaseChannel(channel string) {
	versionPendingChannelMu.Lock()
	versionPendingChannel = channel
	versionPendingChannelMu.Unlock()
}

func pendingReleaseChannel(observed string) string {
	versionPendingChannelMu.Lock()
	defer versionPendingChannelMu.Unlock()
	if versionPendingChannel != "" && versionPendingChannel == observed {
		versionPendingChannel = ""
	}
	return versionPendingChannel
}

// upstreamBranch returns the branch to compare against for the self-version
// check: the build's own branch when known, else defaultUpstreamBranch.
func upstreamBranch() string {
	if versionBranch != "" && versionBranch != "unknown" {
		return versionBranch
	}
	return defaultUpstreamBranch
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Warn("jsonResponse encode failed", "error", err)
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg}); err != nil {
		slog.Warn("jsonError encode failed", "error", err)
	}
}

// jsonStatusResponse writes a JSON body under an explicit status code.
//
// The Content-Type MUST be set before WriteHeader — writing the status first
// freezes the header map, and a JSON body served without its content type is
// exactly what the dashboard's postJSON guard (#5301/#5306) treats as an
// intermediary's HTML error page. Getting this backwards on the kick endpoint
// would turn a healthy 202 into a reported failure, which is the whole class
// of bug #5325 is about.
func jsonStatusResponse(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Warn("jsonStatusResponse encode failed", "error", err)
	}
}

func okResponse(w http.ResponseWriter, extra map[string]string) {
	result := map[string]interface{}{"ok": true}
	for k, v := range extra {
		result[k] = v
	}
	jsonResponse(w, result)
}

// resolveAgentParam resolves an agent path parameter (name or ID) to the
// canonical YAML key (name). Returns the resolved name.
func (s *Server) resolveAgentParam(nameOrID string) string {
	if s.deps != nil && s.deps.AgentMgr != nil {
		return s.deps.AgentMgr.ResolveAgent(nameOrID)
	}
	return nameOrID
}

func (s *Server) refreshAfterMutation() {
	s.refreshAfterMutationSeq()
}

// refreshAfterMutationSeq bumps the mutation epoch (so any in-flight snapshot
// rebuild that started before this mutation is dropped at publish, #4348),
// kicks an async rebuild, and returns the minimum StatusSeq of snapshots
// guaranteed to reflect the mutation. Handlers whose UI patches the DOM
// optimistically should return that floor to the frontend.
func (s *Server) refreshAfterMutationSeq() uint64 {
	floor := s.noteStatusMutation()
	if s.deps != nil && s.deps.RefreshFunc != nil {
		go s.deps.RefreshFunc()
	}
	return floor
}

func (s *Server) persistAfterMutation() {
	if s.deps != nil && s.deps.PersistFunc != nil {
		go s.deps.PersistFunc()
	}
}

func (s *Server) refreshAndPersist() {
	s.refreshAndPersistSeq()
}

// refreshAndPersistSeq is refreshAndPersist returning the post-mutation
// StatusSeq floor (see refreshAfterMutationSeq).
func (s *Server) refreshAndPersistSeq() uint64 {
	floor := s.refreshAfterMutationSeq()
	s.persistAfterMutation()
	return floor
}

// saveConfig persists the in-memory config to disk, skipping the next
// watcher reload to prevent the watcher from overwriting concurrent
// in-memory mutations with a stale file read.
func (s *Server) saveConfig() error {
	if s.deps == nil || s.deps.Config == nil || s.deps.Config.SourcePath == "" {
		return nil
	}
	if s.deps.SkipReloadFunc != nil {
		s.deps.SkipReloadFunc()
	}
	if err := s.deps.Config.Save(); err != nil {
		return err
	}
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateConfig(s.deps.Config.Governor)
		// The dashboard can add or remove repos, which moves every scaled
		// default threshold (#3498). Without this, a repo change would not
		// reach the governor until the next process restart.
		s.deps.Governor.SetRepoCount(s.deps.Config.Project.RepoCount())
	}
	return nil
}

func (s *Server) persistOnly() {
	if s.deps != nil && s.deps.PersistFunc != nil {
		s.deps.PersistFunc()
	}
}

func (s *Server) refreshAsync() {
	if s.deps != nil && s.deps.RefreshFunc != nil {
		s.deps.RefreshFunc()
	}
}

const maxDecodeBodyBytes = 1 << 20

func decodeBody(r *http.Request, v interface{}) error {
	defer closeHTTPBody(r.Body)
	r.Body = http.MaxBytesReader(nil, r.Body, maxDecodeBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}

// htmlTagPattern matches HTML/XML tags for sanitization.
var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// sanitizeString strips HTML tags and env var placeholders from user input
// to prevent stored XSS and environment variable injection on config reload.
func sanitizeString(s string) string {
	s = strings.TrimSpace(htmlTagPattern.ReplaceAllString(s, ""))
	s = envVarEscapePattern.ReplaceAllString(s, "")
	return s
}

// stringField extracts a string value from a decoded JSON object, returning ""
// when the key is missing or not a string.
func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

var envVarEscapePattern = regexp.MustCompile(`\$\{[^}]*\}`)

var tokenRedactor = regexp.MustCompile(`(ghp_|gho_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}`)

func redactTokensInLine(s string) string {
	return tokenRedactor.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) > 7 {
			return m[:7] + "***REDACTED***"
		}
		return "***REDACTED***"
	})
}

func sanitizeFilenameComponent(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

func (s *Server) handleRole(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	user := r.Header.Get("X-Hive-User")
	if sess := s.sessionFromRequest(r); sess != nil {
		// Live allowlist role, never the role frozen into the session at login
		// (session_live_role.go): this endpoint drives what the UI believes the
		// user may do, so a Manage Access grant/downgrade must show up here the
		// moment the heartbeat delivers it — the same rule authenticate applies
		// to every gated request. A revoked session reports no identity.
		if live, ok := s.liveSessionRole(sess); ok {
			user = sess.Username
			role = live
		}
	}
	if role == "" {
		role = "owner"
	}
	resp := map[string]string{
		"role": role,
		"user": user,
		// The queue label is server-configured, so the dashboard must be told
		// it rather than hard-coding a name that a hive may have changed.
		"automerge_label": s.autoMergeLabel(),
	}
	// display_name is the human name for an opaque OIDC identity key
	// ("ibmid:5500…"), delivered by the hub heartbeat (AuthorizedUserNames).
	// Purely cosmetic — the header chip shows it; every auth decision stays on
	// the raw user key. Absent when unknown; the UI falls back to the key.
	if dn := s.authorizedDisplayName(user); dn != "" && dn != user {
		resp["display_name"] = dn
	}
	jsonResponse(w, resp)
}

// authorizedDisplayName looks up the hub-delivered cosmetic display name for
// an identity key. Nil-safe: /api/role is served before deps are required.
func (s *Server) authorizedDisplayName(user string) string {
	if s == nil || s.deps == nil || s.deps.Config == nil || user == "" {
		return ""
	}
	return strings.TrimSpace(s.deps.Config.Dashboard.AuthorizedUserNames[user])
}

// autoMergeLabel reports the configured queue label. /api/role is served
// before GitHub credentials are required, so deps and the client may both be
// nil here even though handleQueuePRAutoMerge rejects that state outright.
func (s *Server) autoMergeLabel() string {
	if s == nil || s.deps == nil {
		return github.AutoMergeQueuedLabel
	}
	return s.deps.GHClient.AutoMergeLabel()
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	history := s.deps.Governor.EvalHistory()

	seedData, err := os.ReadFile("/data/sparkline-history.json")
	if err == nil {
		var seed []json.RawMessage
		if json.Unmarshal(seedData, &seed) == nil && len(seed) > 0 {
			liveData, err := json.Marshal(history)
			if err != nil {
				jsonResponse(w, history)
				return
			}
			var liveEntries []json.RawMessage
			if json.Unmarshal(liveData, &liveEntries) != nil {
				jsonResponse(w, history)
				return
			}
			combined := append(seed, liveEntries...)
			jsonResponse(w, combined)
			return
		}
	}

	jsonResponse(w, history)
}

func (s *Server) handleTrends(w http.ResponseWriter, r *http.Request) {
	const hoursPerDay = 24
	const hoursPerWeek = 168

	rangeParam := r.URL.Query().Get("range")
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))

	const maxTrendHours = 720 // 30 days
	switch rangeParam {
	case "week":
		hours = hoursPerWeek
	case "day":
		hours = hoursPerDay
	default:
		if hours <= 0 {
			hours = hoursPerDay
		} else if hours > maxTrendHours {
			hours = maxTrendHours
		}
	}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)

	evals := s.deps.Governor.EvalHistory()
	filtered := make([]interface{}, 0)
	for _, e := range evals {
		if e.Timestamp > cutoff.UnixMilli() {
			filtered = append(filtered, e)
		}
	}

	// Include token sparkline history within the requested time range
	allTokenHistory := s.TokenSparklineHistory()
	tokenFiltered := make([]TokenSparklineEntry, 0)
	cutoffMs := cutoff.UnixMilli()
	for _, entry := range allTokenHistory {
		if entry.Timestamp > cutoffMs {
			tokenFiltered = append(tokenFiltered, entry)
		}
	}

	jsonResponse(w, map[string]interface{}{
		"evals":        filtered,
		"tokenHistory": tokenFiltered,
	})
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	kicks := s.deps.Governor.KickHistory()

	evals := s.deps.Governor.EvalHistory()
	type timelineMode struct {
		T    int64  `json:"t"`
		Mode string `json:"mode"`
	}
	modes := make([]timelineMode, 0, len(evals))
	for _, e := range evals {
		modes = append(modes, timelineMode{
			T:    e.Timestamp,
			Mode: strings.ToLower(string(e.Mode)),
		})
	}

	seedData, err := os.ReadFile("/data/sparkline-history.json")
	if err == nil {
		var seed []json.RawMessage
		if json.Unmarshal(seedData, &seed) == nil && len(seed) > 0 {
			var seedModes []timelineMode
			for _, raw := range seed {
				var entry struct {
					T       int64  `json:"t"`
					GovMode string `json:"govMode"`
				}
				if json.Unmarshal(raw, &entry) == nil && entry.T > 0 {
					m := strings.ToLower(entry.GovMode)
					if m == "" {
						m = "idle"
					}
					seedModes = append(seedModes, timelineMode{T: entry.T, Mode: m})
				}
			}
			modes = append(seedModes, modes...)
		}
	}

	// If eval-based modes are empty, fall back to explicit mode history
	// so the timeline always shows at least the startup mode.
	if len(modes) == 0 {
		modeChanges := s.deps.Governor.ModeHistory()
		for _, mc := range modeChanges {
			modes = append(modes, timelineMode{
				T:    mc.Timestamp.UnixMilli(),
				Mode: strings.ToLower(string(mc.To)),
			})
		}
	}

	jsonResponse(w, map[string]interface{}{
		"kicks": kicks,
		"modes": modes,
	})
}

// lifecycleTimelineOnce/lifecycleStore back the lazily-constructed lifecycle
// timeline Store. Lazy construction keeps the zero-value Server valid (no
// constructor change) and keeps memory bounded via timeline.MaxJourneys.
//
// The store is fed by real producers now (#5656): the governor eval loop
// (enumerated, kicked), the scheduler's classifier (classified), the
// attribution audit sink + PR-opened hook (pr_opened, merged) and the
// escalation sweep (blocked) — see cmd/hive/lifecyclewire.go.
var (
	lifecycleTimelineOnce sync.Once
	lifecycleStore        *timeline.Store
)

// LifecycleTimeline returns the process-wide lifecycle timeline Store,
// constructing it on first use. Never returns nil.
func (s *Server) LifecycleTimeline() *timeline.Store {
	lifecycleTimelineOnce.Do(func() {
		lifecycleStore = timeline.NewStore()
	})
	return lifecycleStore
}

// EnableLifecyclePersistence loads previously persisted lifecycle journeys
// from path and turns on atomic re-persistence, so a pod restart no longer
// zeroes the panel's merged/blocked history (#5656). Call once at startup,
// before the governor starts recording; mirrors EnableSessionPersistence.
func (s *Server) EnableLifecyclePersistence(path string) {
	if err := s.LifecycleTimeline().EnablePersistence(path, s.logger); err != nil && s.logger != nil {
		s.logger.Warn("lifecycle timeline persistence unavailable — journeys reset on restart",
			"path", path, "error", err)
	}
}

// lifecycleTimelineDefaultLimit bounds how many journeys the
// /api/lifecycle-timeline endpoint returns by default when the caller does not
// pass ?limit=.
const lifecycleTimelineDefaultLimit = 200

// handleLifecycleTimeline serves the issue→PR lifecycle journeys plus derived
// fleet health as JSON. It is additive and read-only; an empty store yields
// empty arrays (never null), so the dashboard can render unconditionally.
//
// Query params:
//
//	limit  — max journeys to return (default lifecycleTimelineDefaultLimit)
//	window — fleet-health look-back, in minutes (default timeline.DefaultFleetWindow)
func (s *Server) handleLifecycleTimeline(w http.ResponseWriter, r *http.Request) {
	limit := lifecycleTimelineDefaultLimit
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	var window time.Duration // zero → timeline.DefaultFleetWindow
	if v := strings.TrimSpace(r.URL.Query().Get("window")); v != "" {
		if mins, err := strconv.Atoi(v); err == nil && mins > 0 {
			window = time.Duration(mins) * time.Minute
		}
	}

	store := s.LifecycleTimeline()
	dto := store.Snapshot(limit, window)
	if level := s.lifecycleACMMLevel(); level > 0 {
		dto.Journeys = filterJourneysByACMMLevel(dto.Journeys, level)
		// Re-derive fleet counts over the FULL filtered journey set (not the
		// limit-truncated one) so the counters match what the level may see.
		dto.Fleet = timeline.DeriveFleetHealth(filterJourneysByACMMLevel(store.Journeys(0), level), window)
	}
	// Defensive nil-guard: Snapshot already guarantees a non-nil slice, but
	// keep the endpoint's array-always contract explicit.
	if dto.Journeys == nil {
		dto.Journeys = []timeline.Journey{}
	}
	jsonResponse(w, dto)
}

func (s *Server) lifecycleACMMLevel() int {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return 0
	}
	return detectACMMLevel(s.deps.Config)
}

// filterJourneysByACMMLevel drops journeys whose most recent agent is not
// available at the given maturity level (the operability agents below L5).
// Journeys with no agent yet (enumerated/classified only) always pass.
func filterJourneysByACMMLevel(journeys []timeline.Journey, level int) []timeline.Journey {
	filtered := make([]timeline.Journey, 0, len(journeys))
	for _, j := range journeys {
		if agent.AgentAvailableAtACMMLevel(j.Agent, level) {
			filtered = append(filtered, j)
		}
	}
	return filtered
}

func (s *Server) handleWidget(w http.ResponseWriter, r *http.Request) {
	state := s.deps.Governor.GetState()

	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()

	agents := []map[string]any{}
	running := 0
	paused := 0
	busy := 0
	frontendAgents := []FrontendAgent(nil)
	if status != nil {
		frontendAgents = status.Agents
	} else {
		frontendAgents = buildAgents(s.deps.AgentMgr.AllStatuses(), s.deps.Config, state)
	}
	for _, a := range frontendAgents {
		if a.State == "running" {
			running++
		}
		if a.Paused || a.State == "paused" {
			paused++
		}
		if strings.TrimSpace(a.Busy) != "" {
			busy++
		}
		agents = append(agents, map[string]any{
			"name":       a.Name,
			"display":    nonEmpty(a.DisplayName, a.Name),
			"state":      a.State,
			"paused":     a.Paused || a.State == "paused",
			"busy":       a.Busy,
			"next_kick":  a.NextKick,
			"nextKick":   a.NextKick,
			"nextKickIn": a.NextKickIn,
		})
	}

	openIssues, openPRs := state.QueueIssues, state.QueuePRs
	acmmLevel := 0
	if s.deps != nil && s.deps.Config != nil {
		acmmLevel = s.deps.Config.ACMMLevelOrZero()
	}
	if status != nil {
		openIssues, openPRs = 0, 0
		acmmLevel = status.ACMMLevel
		for _, repo := range status.Repos {
			openIssues += len(repo.ActionableIssues)
			openPRs += len(repo.OpenPrs)
		}
	}
	governorSummary := map[string]any{"mode": state.Mode}
	if status != nil {
		governorSummary["nextKick"] = status.Governor.NextKick
		governorSummary["nextKickAt"] = status.Governor.NextKickAt
		governorSummary["nextKickIn"] = status.Governor.NextKickIn
	}

	throughput := PRThroughput{Hours: 168, Source: "audit"}
	if s.audit != nil {
		throughput = buildPRThroughputWindow(prThroughputEntries(s.audit), time.Now().UTC(), 168, "", prThroughputRoleMerged)
	}

	breakerEngaged := false
	breakerAgents := []string{}
	if s.deps != nil && s.deps.AgentMgr != nil {
		breakerEngaged, breakerAgents = s.deps.AgentMgr.BreakerState()
	}

	upgradeAvailable := false
	s.versionMu.RLock()
	if s.cachedLatestHash != "" && versionHash != "" {
		upgradeAvailable = !sameCommitDashboard(versionHash, s.cachedLatestHash)
	}
	s.versionMu.RUnlock()

	jsonResponse(w, map[string]interface{}{
		"mode":       state.Mode,
		"issues":     openIssues,
		"prs":        openPRs,
		"running":    running,
		"paused":     paused,
		"busy":       busy,
		"last_eval":  state.LastEval,
		"agents":     agents,
		"governor":   governorSummary,
		"acmmLevel":  acmmLevel,
		"openPRs":    openPRs,
		"openIssues": openIssues,
		"prThroughput7d": map[string]any{
			"opened": throughput.Opened,
			"merged": throughput.Merged,
			"closed": throughput.Closed,
			"hours":  throughput.Hours,
		},
		"spoke": map[string]any{
			"version":          nonEmpty(versionShort, versionHash),
			"upgradeAvailable": upgradeAvailable,
		},
		"fleetBreaker": map[string]any{
			"engaged": breakerEngaged,
			"agents":  breakerAgents,
		},
	})
}

func (s *Server) handlePane(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	const maxPaneLines = 1000
	if lines <= 0 {
		lines = 100
	} else if lines > maxPaneLines {
		lines = maxPaneLines
	}
	source := r.URL.Query().Get("source")

	var output []string
	var err error

	if source == "buffer" {
		output, err = s.deps.AgentMgr.GetBufferOutput(name, lines)
	} else {
		output, err = s.deps.AgentMgr.GetOutput(name, lines)
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	for i, line := range output {
		output[i] = redactTokensInLine(line)
	}

	jsonResponse(w, map[string]interface{}{
		"agent": name,
		"lines": output,
		"count": len(output),
	})
}

const (
	tokenAccessMaxEntries = 100
)

// tokenAccessLogPath is github.TokenAccessLogPath. The file is written ONLY
// by the hive process (pkg/github's token-access ingester) and is hive-owned
// 0600: the agents whose gh calls it records cannot append to, truncate, or
// read it (#6287). The wrappers drop per-call events into a spool the hive
// ingests, attributing each to the uid that owns the event file.
var tokenAccessLogPath = "/var/run/hive-metrics/token-access.jsonl"

func (s *Server) handleTokenAccess(w http.ResponseWriter, r *http.Request) {
	// SECURITY (#3936, CWE-284): the token-access log records every gh CLI
	// command an agent issued, including full arguments (--repo, --title,
	// --body ...). Without a role gate any authenticated user — including
	// read-only contributors — could enumerate the hive's full GitHub operation
	// history. Gate at owner-role, consistent with handleConfigDownload and
	// handleSelfUpgrade which protect equivalent operator-only data. The
	// write side is protected too: see tokenAccessLogPath.
	if !requireOwnerRole(w, r) {
		return
	}
	data, err := os.ReadFile(tokenAccessLogPath)
	if err != nil {
		jsonResponse(w, map[string]interface{}{"entries": []interface{}{}, "error": "no audit log"})
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	start := 0
	if len(lines) > tokenAccessMaxEntries {
		start = len(lines) - tokenAccessMaxEntries
	}
	entries := make([]json.RawMessage, 0, len(lines)-start)
	skipped := 0
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// The log is appended concurrently by the gh-wrapper audit path, so a
		// reader can observe a torn (partially written) final line. Skip and
		// count any line that isn't valid JSON instead of letting it corrupt
		// the whole response (#6407).
		if !json.Valid([]byte(line)) {
			skipped++
			continue
		}
		entries = append(entries, json.RawMessage(line))
	}
	jsonResponse(w, map[string]interface{}{"entries": entries, "skipped": skipped})
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tokens == nil {
		jsonResponse(w, map[string]string{"status": "no_collector"})
		return
	}
	summary := s.deps.Tokens.Summary()
	if summary == nil {
		jsonResponse(w, map[string]interface{}{"total_tokens": 0, "sessions": []interface{}{}})
		return
	}
	jsonResponse(w, summary)
}

func (s *Server) handleModelAdvisor(w http.ResponseWriter, r *http.Request) {
	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"budget":         budget,
		"recommendation": "Use haiku for simple tasks, sonnet for default, opus for complex refactors",
	})
}

func (s *Server) handleSummaries(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()

	if status == nil {
		jsonResponse(w, map[string]interface{}{"issues": []interface{}{}, "prs": []interface{}{}})
		return
	}

	allIssues := make([]any, 0)
	allPRs := make([]any, 0)
	for _, repo := range status.Repos {
		allIssues = append(allIssues, repo.ActionableIssues...)
		allPRs = append(allPRs, repo.OpenPrs...)
	}

	jsonResponse(w, map[string]interface{}{
		"issues": allIssues,
		"prs":    allPRs,
		"hold":   status.Hold.Items,
	})
}

func (s *Server) handleQueuePRAutoMerge(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if !requireMergerOrOwnerRole(w, r) {
		return
	}
	if user == "" {
		jsonError(w, "authenticated GitHub user required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		jsonError(w, "invalid pull request number", http.StatusBadRequest)
		return
	}
	owner := strings.TrimSpace(r.PathValue("owner"))
	repoName := strings.TrimSpace(r.PathValue("repo"))
	if owner == "" || repoName == "" || strings.Contains(owner, "/") || strings.Contains(repoName, "/") {
		jsonError(w, "invalid repository", http.StatusBadRequest)
		return
	}
	repo := owner + "/" + repoName
	if !s.prQueueRepoAllowed(repo) {
		jsonError(w, "repository is not managed by this hive", http.StatusForbidden)
		return
	}
	author, err := s.deps.GHClient.GetPRAuthor(r.Context(), repo, number)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	if strings.EqualFold(author, user) {
		jsonError(w, "users cannot queue their own pull requests", http.StatusForbidden)
		return
	}
	if err := s.deps.GHClient.QueuePRAutoMerge(r.Context(), repo, number, user); err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	detail := auditDetail("repo", repo, "pr", strconv.Itoa(number), "author", author)
	s.auditFromRequest(r, "queue-pr-automerge", detail, "")
	jsonResponse(w, map[string]any{
		"status": "queued",
		"repo":   repo,
		"number": number,
		"label":  s.deps.GHClient.AutoMergeLabel(),
	})
}

func (s *Server) prQueueRepoAllowed(repo string) bool {
	if s.deps == nil || s.deps.Config == nil {
		return false
	}
	for _, configured := range s.deps.Config.Project.Repos {
		full := configured
		if !strings.Contains(full, "/") {
			full = s.deps.Config.Project.Org + "/" + full
		}
		if strings.EqualFold(full, repo) {
			return true
		}
	}
	return false
}

func (s *Server) handleFactHistory(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.FactHistory())
}

func (s *Server) handleCostHistory(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.CostHistory())
}

func (s *Server) handleTrendHistory(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.TrendHistory())
}

func (s *Server) handleOverviewHistory(w http.ResponseWriter, r *http.Request) {
	var since int64
	if raw := r.URL.Query().Get("since"); raw != "" {
		since, _ = strconv.ParseInt(raw, 10, 64)
	}
	jsonResponse(w, s.OverviewKPIHistory(since))
}

// handleTimeSeries is the unified read endpoint over the sparkline histories.
// GET /api/timeseries?series=<name> returns the same JSON the dedicated
// endpoint returns (token → /api/tokens history, fact → /api/knowledge/
// fact-history, cost → /api/cost/history), so it's an additive alias rather
// than a replacement — the existing endpoints stay for back-compat.
func (s *Server) handleTimeSeries(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("series") {
	case "token", "tokens":
		jsonResponse(w, s.TokenSparklineHistory())
	case "fact", "facts":
		jsonResponse(w, s.FactHistory())
	case "cost":
		jsonResponse(w, s.CostHistory())
	default:
		http.Error(w, `{"error":"unknown series; valid: token, fact, cost"}`, http.StatusBadRequest)
	}
}

// SetDeploymentImageSource installs the cached Deployment image lookup before
// the server starts. Channel and tracking are derived from the same snapshot.
// Invalid refs are omitted from the API and reported as unknown tracking.
func SetDeploymentImageSource(source func() string) {
	versionImageSource = source
}
