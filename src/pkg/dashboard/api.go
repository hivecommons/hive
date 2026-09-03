package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/classify"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/dashboard/collect"
	"github.com/kubestellar/hive/pkg/dashboard/webstatic"
	"github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/hub"
	"github.com/kubestellar/hive/pkg/knowledge"
	"github.com/kubestellar/hive/pkg/policies"
	"github.com/kubestellar/hive/pkg/resolve"
	"github.com/kubestellar/hive/pkg/timeline"
)

func (s *Server) RegisterAPI(deps *Dependencies) {
	s.deps = deps
	s.loadSidebarFromDisk()
	s.registerContributeRoutes()
	// Approval desk (RFC #4000). Routes register unconditionally; the handlers
	// report "not enabled" when deps.ApprovalInbox is nil, so a disabled desk is
	// an honest 200 the panel can render rather than a 404 that looks broken.
	s.registerApprovalRoutes()

	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("GET /api/style", s.handleStyle)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/config/download", s.handleConfigDownload)
	s.mux.HandleFunc("GET /api/config/provenance", s.handleConfigProvenance)
	s.mux.HandleFunc("GET /api/config/variables", s.handleVariablesList)
	s.mux.HandleFunc("GET /api/config/authorized-users", s.handleAuthorizedUsersList)
	s.mux.HandleFunc("PUT /api/config/variables/{name}", s.handleVariableUpsert)
	s.mux.HandleFunc("DELETE /api/config/variables/{name}", s.handleVariableDelete)
	s.mux.HandleFunc("GET /api/audit", s.handleAuditLog)
	s.mux.HandleFunc("GET /api/providers/headroom", s.handleProvidersHeadroom)
	s.mux.HandleFunc("POST /api/presence", s.handlePresence)
	s.mux.HandleFunc("GET /api/prompt-history", s.handlePromptHistory)
	s.mux.HandleFunc("POST /api/self-upgrade", s.handleSelfUpgrade)
	// Self-service, owner-only spoke backup (encrypted; includes the bead
	// ledger the fleet-wide hub backup excludes — see issue #2318).
	s.mux.HandleFunc("GET /api/backup/status", s.handleBackupStatus)
	s.mux.HandleFunc("POST /api/backup", s.handleBackupDownload)
	s.mux.HandleFunc("POST /api/banner-dismissed", s.handleBannerDismissed)
	s.mux.HandleFunc("GET /api/snapshot/frame-ancestors", s.handleSnapshotFrameAncestors)
	s.mux.HandleFunc("GET /api/snapshot", s.handleSnapshotAPI)
	s.mux.HandleFunc("GET /snapshot", s.handleSnapshotPage)
	s.mux.HandleFunc("GET /api/history", s.handleHistory)
	s.mux.HandleFunc("GET /api/trends", s.handleTrends)
	s.mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	s.mux.HandleFunc("GET /api/lifecycle-timeline", s.handleLifecycleTimeline)
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
	s.mux.HandleFunc("POST /api/pause/{agent}", s.handlePause)
	s.mux.HandleFunc("POST /api/resume/{agent}", s.handleResume)
	s.mux.HandleFunc("GET /api/agent-state/{agent}", s.handleAgentState)
	s.mux.HandleFunc("GET /api/breaker", s.handleBreakerState)
	s.mux.HandleFunc("POST /api/breaker/engage", s.handleBreakerEngage)
	s.mux.HandleFunc("POST /api/breaker/release", s.handleBreakerRelease)
	s.mux.HandleFunc("POST /api/pin/{agent}/{dimension}", s.handlePin)
	s.mux.HandleFunc("POST /api/unpin/{agent}/{dimension}", s.handleUnpin)
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
	s.mux.HandleFunc("GET /api/timeseries", s.handleTimeSeries)
	s.mux.HandleFunc("GET /api/model-advisor", s.handleModelAdvisor)
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
	s.mux.HandleFunc("GET /api/config/governor/threshold-scaling", s.handleGovernorThresholdScalingGet)
	s.mux.HandleFunc("PUT /api/config/governor/threshold-scaling", s.handleGovernorThresholdScaling)
	s.mux.HandleFunc("PUT /api/config/governor/labels", s.handleGovernorLabels)
	s.mux.HandleFunc("PUT /api/config/governor/budget", s.handleGovernorBudget)
	s.mux.HandleFunc("POST /api/config/governor/budget/reset", s.handleGovernorBudgetReset)
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

	s.mux.HandleFunc("GET /api/acmm/evaluation", s.handleACMMEvaluation)
	s.mux.HandleFunc("POST /api/acmm/issue", s.handleACMMCreateIssue)
	s.mux.HandleFunc("GET /api/acmm-recommendation", s.handleACMMRecommendation)

	s.mux.HandleFunc("GET /api/config/sidebar", s.handleSidebarGet)
	s.mux.HandleFunc("PUT /api/config/sidebar", s.handleSidebarSet)
	s.mux.HandleFunc("GET /api/config/backends", s.handleBackends)
	s.mux.HandleFunc("GET /api/inference/models/{backend}", s.handleInferenceModels)

	s.mux.HandleFunc("GET /api/knowledge", s.handleKnowledgeList)
	s.mux.HandleFunc("GET /api/knowledge/export", s.handleKnowledgeExport)
	s.mux.HandleFunc("GET /api/knowledge/search", s.handleKnowledgeSearch)
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
	s.mux.HandleFunc("GET /api/plan/{epicID}", s.handlePlanTree)
	s.mux.HandleFunc("POST /api/plan/{epicID}/approve", s.handlePlanApprove)
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

func (s *Server) refreshAndPersistSync() {
	s.noteStatusMutation()
	if s.deps != nil && s.deps.RefreshFunc != nil {
		s.deps.RefreshFunc()
	}
	if s.deps != nil && s.deps.PersistFunc != nil {
		s.deps.PersistFunc()
	}
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

const maxDecodeBodyBytes = 1 << 20 // 1 MiB

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

// --- Core status endpoints ---

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

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"version": "2.0.0",
		"go":      "1.25",
		"hash":    versionHash,
		"short":   versionShort,
		"branch":  upstreamBranch(),
	}
	if versionChannel != "" {
		resp["channel"] = versionChannel
	}
	// autoUpgrade tells the dashboard whether the hub manages this spoke's
	// upgrades. When true the manual spoke Upgrade button is hidden — the hub
	// rolls the upgrade out automatically, so offering a manual button is
	// redundant and confusing.
	if s.deps != nil && s.deps.Config != nil {
		resp["autoUpgrade"] = s.deps.Config.Hub.AutoUpgrade
	}

	s.versionMu.RLock()
	cached := s.cachedLatestHash
	cachedMsg := s.cachedLatestMessage
	cacheAge := time.Since(s.cachedLatestAt)
	stableV4 := s.cachedStableV4Hash
	stableV4Age := time.Since(s.cachedStableV4At)
	s.versionMu.RUnlock()

	if cacheAge > dashboardVersionTipCacheTTL || cached == "" {
		if latest, err := s.fetchLatestRemoteHash(); err == nil && latest != "" {
			msg := s.fetchCommitMessage(latest)
			s.versionMu.Lock()
			s.cachedLatestHash = latest
			s.cachedLatestMessage = msg
			s.cachedLatestAt = time.Now()
			s.versionMu.Unlock()
			cached = latest
			cachedMsg = msg
		}
	}
	if upstreamBranch() == dashboardStableReleaseBranch {
		stableV4 = cached
	} else if stableV4Age > dashboardVersionTipCacheTTL || stableV4 == "" {
		if latest, err := s.fetchRemoteHashForBranch(dashboardStableReleaseBranch); err == nil && latest != "" {
			s.versionMu.Lock()
			s.cachedStableV4Hash = latest
			s.cachedStableV4At = time.Now()
			s.versionMu.Unlock()
			stableV4 = latest
		}
	}

	if cached != "" {
		latestShort := cached
		const shortHashLen = 7
		if len(latestShort) > shortHashLen {
			latestShort = latestShort[:shortHashLen]
		}
		// Only report "behind" if the container image exists on GHCR
		containerReady := ghcrTagExistsCached(latestShort)
		resp["latestHash"] = cached
		resp["latestShort"] = latestShort
		resp["behind"] = containerReady && cached != versionHash
		if cachedMsg != "" {
			resp["latestMessage"] = cachedMsg
		}
	}
	if stableV4 != "" {
		stableShort := shortSHADashboard(stableV4)
		resp["stableV4Hash"] = stableV4
		resp["stableV4Short"] = stableShort
		// Distinguish "the tip has no image yet" (a known state: nothing to
		// upgrade to, compare never attempted) from "the compare failed"
		// (genuinely unknown). Without this the frontend renders a yellow
		// "? behind" next to the green ✓ whenever the tip is unbuilt (#4804).
		stableImageReady := ghcrTagExistsCached(stableShort)
		resp["stableV4ImageReady"] = stableImageReady
		if sameCommitDashboard(versionHash, stableV4) {
			resp["commitsBehind"] = 0
		} else if stableImageReady {
			if count, ok := s.commitsBehindStableTip(versionHash, stableV4); ok {
				resp["commitsBehind"] = count
			}
		}
	}

	jsonResponse(w, resp)
}

const dashboardVersionTipCacheTTL = 5 * time.Minute
const dashboardStableReleaseBranch = "v4"

func (s *Server) commitsBehindStableTip(base, head string) (int, bool) {
	base = shortSHADashboard(base)
	head = shortSHADashboard(head)
	if base == "" || head == "" {
		return 0, false
	}
	if sameCommitDashboard(base, head) {
		return 0, true
	}
	key := base + "..." + head
	s.versionMu.RLock()
	if s.commitBehindCache != nil {
		if count, ok := s.commitBehindCache[key]; ok {
			s.versionMu.RUnlock()
			return count, true
		}
	}
	s.versionMu.RUnlock()
	if s.deps == nil || s.deps.GHClient == nil || s.deps.Ctx == nil {
		return 0, false
	}
	count, err := s.deps.GHClient.CompareAheadBy(s.deps.Ctx, "kubestellar", "hive", base, head)
	if err != nil {
		s.logger.Warn("failed to compare commits behind stable tip", "base", base, "head", head, "error", err)
		return 0, false
	}
	s.versionMu.Lock()
	if s.commitBehindCache == nil {
		s.commitBehindCache = map[string]int{}
	}
	s.commitBehindCache[key] = count
	s.versionMu.Unlock()
	return count, true
}

func shortSHADashboard(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > hub.StandardSHALen {
		return s[:hub.StandardSHALen]
	}
	return s
}

func sameCommitDashboard(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return strings.EqualFold(a[:n], b[:n])
}

func (s *Server) fetchLatestRemoteHash() (string, error) {
	return s.fetchRemoteHashForBranch(upstreamBranch())
}

func (s *Server) fetchRemoteHashForBranch(branch string) (string, error) {
	if s.deps == nil || s.deps.GHClient == nil {
		return "", fmt.Errorf("no github client")
	}
	ctx := s.deps.Ctx
	if ctx == nil {
		return "", fmt.Errorf("no context")
	}
	return s.deps.GHClient.LatestCommitHash(ctx, "kubestellar", "hive", branch)
}

// fetchCommitMessage returns the first line of the commit message for a given SHA.
// Returns empty string on any error (best-effort for tooltip display).
func (s *Server) fetchCommitMessage(sha string) string {
	if s.deps == nil || s.deps.GHClient == nil || s.deps.Ctx == nil {
		return ""
	}
	msg, err := s.deps.GHClient.CommitMessage(s.deps.Ctx, "kubestellar", "hive", sha)
	if err != nil {
		s.logger.Warn("failed to fetch commit message", "sha", sha, "error", err)
		return ""
	}
	return msg
}

// ghcrTagExistsCached checks whether a container tag exists on ghcr.io/kubestellar/hive,
// caching the result to avoid repeated network calls on each version poll.
var (
	ghcrCacheMu     sync.RWMutex
	ghcrCacheResult = map[string]bool{}
	ghcrCacheExpiry = map[string]time.Time{}
)

const ghcrCacheTTL = 2 * time.Minute
const ghcrCheckTimeout = 5 * time.Second

func ghcrTagExistsCached(tag string) bool {
	ghcrCacheMu.RLock()
	if exp, ok := ghcrCacheExpiry[tag]; ok && time.Now().Before(exp) {
		result := ghcrCacheResult[tag]
		ghcrCacheMu.RUnlock()
		return result
	}
	ghcrCacheMu.RUnlock()

	result := ghcrTagExists(tag)
	ghcrCacheMu.Lock()
	ghcrCacheResult[tag] = result
	ghcrCacheExpiry[tag] = time.Now().Add(ghcrCacheTTL)
	ghcrCacheMu.Unlock()
	return result
}

func ghcrTagExists(tag string) bool {
	client := &http.Client{Timeout: ghcrCheckTimeout}
	tokenResp, err := client.Get("https://ghcr.io/token?scope=repository:kubestellar/hive:pull")
	if err != nil {
		return false
	}
	defer closeHTTPBody(tokenResp.Body)
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return false
	}

	manifestURL := fmt.Sprintf("https://ghcr.io/v2/kubestellar/hive/manifests/%s", tag)
	req, _ := http.NewRequest("HEAD", manifestURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	closeHTTPBody(resp.Body)
	return resp.StatusCode == http.StatusOK
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config
	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
		primaryRepo = cfg.Project.Repos[0]
	}
	if primaryRepo != "" && cfg.Project.Org != "" && !strings.Contains(primaryRepo, "/") {
		primaryRepo = cfg.Project.Org + "/" + primaryRepo
	}
	// ResolvedBaseURL (not the raw BaseURL) so pooled/placeholder GHE hives —
	// which legitimately carry base_url: "" but api_url: https://github.ibm.com/api/v3
	// — report github.ibm.com, not github.com. Reading BaseURL alone made the
	// Repos config show bare org/repo with no GHE hint, so operators mistook a
	// github.ibm.com hive for github.com. ResolvedBaseURL falls back to the api_url
	// host in exactly that case (mirrors HostLabel).
	githubBaseURL := cfg.GitHub.ResolvedBaseURL()
	resp := map[string]interface{}{
		"org":       cfg.Project.Org,
		"repos":     cfg.Project.Repos,
		"ai_author": cfg.Project.AIAuthor,
		// ai_author_effective is who agents actually author PRs/commits as: the
		// configured ai_author, or the GitHub App bot login ("<slug>[bot]") when
		// ai_author is empty and the hive authenticates as an App installation.
		"ai_author_effective": cfg.EffectiveAIAuthor(),
		"agents":              len(cfg.EnabledAgents()),
		"eval_interval_s":     cfg.Governor.EvalIntervalS,
		"primaryRepo":         primaryRepo,
		"hub_url":             cfg.Hub.URL,
		"hive_id":             cfg.HiveID,
		"github_base_url":     githubBaseURL,
	}
	// The active project.issue_filter, read-only: which issues agents may
	// initiate work on, by label. Omitted entirely when no filter is
	// configured so the payload (and the UI note keyed off it) stays quiet
	// for the ordinary unfiltered hive.
	if !cfg.Project.IssueFilter.IsZero() {
		resp["issue_filter"] = cfg.Project.IssueFilter
	}
	jsonResponse(w, resp)
}

func (s *Server) handleConfigDownload(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// F9 (CWE-862): the raw hive.yaml carries secrets, so a MISSING X-Hive-Role
	// must NOT default to owner on a spoke that has an auth boundary. Least
	// privilege: only a genuinely open/dev spoke treats an empty role as owner.
	if !s.requestRoleAllowsOwner(r) {
		http.Error(w, "owner access required", http.StatusForbidden)
		return
	}
	configPath := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		configPath = envCfg
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		http.Error(w, "config file not found", http.StatusNotFound)
		return
	}
	org := ""
	repo := ""
	if s.deps != nil && s.deps.Config != nil {
		org = s.deps.Config.Project.Org
		if len(s.deps.Config.Project.Repos) > 0 {
			repo = s.deps.Config.Project.Repos[0]
		}
	}
	timestamp := time.Now().Format("2006-01-02_150405")
	safeOrg := sanitizeFilenameComponent(org)
	safeRepo := sanitizeFilenameComponent(repo)
	filename := fmt.Sprintf("hive-%s-%s-%s.yaml", safeOrg, safeRepo, timestamp)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	_, _ = w.Write(data)
}

func (s *Server) handleSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" {
		jsonError(w, "owner access required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	hubURL := s.deps.Config.Hub.URL
	hiveID := s.deps.Config.HiveID
	if hubURL == "" || hiveID == "" {
		jsonError(w, "hub URL or hive ID not configured", http.StatusBadRequest)
		return
	}
	upgradeURL := hubURL + "/api/saas/hives/" + url.PathEscape(hiveID) + "/upgrade"

	user := r.Header.Get("X-Hive-User")
	proof := s.authToken
	if proof == "" && s.deps.Config.Dashboard.AuthToken != "" {
		proof = s.deps.Config.Dashboard.AuthToken
	}
	cookie, _ := r.Cookie("hive_hub_user")
	if proof == "" && cookie == nil {
		// Fail fast and honestly: with no dashboard-token proof and no hub
		// session cookie to relay, the hub is guaranteed to reject this request,
		// and "not authenticated" would mislead a logged-in owner. Name the
		// missing credential and how to configure it (#4446 honest-error
		// standard).
		jsonError(w, "self-upgrade needs this spoke's dashboard token to prove itself to the hub — set DASHBOARD_AUTH_TOKEN (the hive-secrets/dashboard-token secret) and restart the spoke", http.StatusBadRequest)
		return
	}
	const upgradeTimeout = 30 * time.Second
	client := &http.Client{Timeout: upgradeTimeout}
	req, err := http.NewRequest("POST", upgradeURL, nil)
	if err != nil {
		jsonError(w, "failed to create upgrade request", http.StatusInternalServerError)
		return
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if proof != "" {
		req.Header.Set(proxyAuthHeader, proof)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("self-upgrade: hub request failed", "error", err)
		jsonError(w, "hub unreachable", http.StatusBadGateway)
		return
	}
	defer closeHTTPBody(resp.Body)
	const maxUpgradeResponseBytes = 1 << 16
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpgradeResponseBytes))
	if resp.StatusCode < 300 {
		s.auditFromRequest(r, "self_upgrade", "", "")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (s *Server) handleSnapshotAPI(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.Hub.AutoSnapshot {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"snapshots not enabled"}`))
		return
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		http.Error(w, "no data yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	jsonResponse(w, status)
}

func (s *Server) handleSnapshotFrameAncestors(w http.ResponseWriter, r *http.Request) {
	var origins []string
	if s.deps != nil && s.deps.Config != nil {
		origins = s.deps.Config.Dashboard.SnapshotFrameAncestors
	}
	jsonResponse(w, map[string]any{"origins": origins})
}

func (s *Server) handleSnapshotPage(w http.ResponseWriter, r *http.Request) {
	hubURL := "https://hive.kubestellar.io"
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.URL != "" {
		hubURL = s.deps.Config.Hub.URL
	}

	cfg := s.deps.Config
	if s.deps == nil || cfg == nil || !cfg.Hub.AutoSnapshot {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="3;url=%s"><title>Hive</title>
<style>body{font-family:system-ui,sans-serif;background:#0a0a0a;color:#e0e0e0;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
.card{text-align:center;max-width:480px;padding:40px}.bee{font-size:3rem;margin-bottom:16px}h1{color:#f59e0b;margin:0 0 8px}p{color:#8b949e;line-height:1.6}a{color:#58a6ff}</style>
</head><body><div class="card"><div class="bee">🐝</div><h1>Hive</h1><p>AI Agent Orchestration for Open Source</p><p>Snapshot is not currently published for this hive.</p><p>Redirecting to <a href="%s">%s</a>...</p></div></body></html>`,
			hubURL, hubURL, hubURL)
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "classic" {
		mode = "dark"
	}
	if mode != "dark" {
		mode = "light"
	}
	snapDir := s.snapshotDirOrDefault()
	snapshotFile := filepath.Join(snapDir, fmt.Sprintf("snapshot-%s.html", mode))
	info, err := os.Stat(snapshotFile)
	intervalMin := cfg.Hub.SnapshotIntervalMin
	if intervalMin < 5 {
		intervalMin = 15
	}
	staleThreshold := time.Duration(intervalMin) * time.Minute
	needsRebuild := err != nil || time.Since(info.ModTime()) > staleThreshold

	if needsRebuild {
		s.buildSnapshot(filepath.Join(snapDir, "snapshot-dark.html"), "dark")
		s.buildSnapshot(filepath.Join(snapDir, "snapshot-light.html"), "light")
	}

	data, err := os.ReadFile(snapshotFile)
	if err != nil {
		http.Error(w, "snapshot not yet generated — try again in a moment", http.StatusServiceUnavailable)
		return
	}

	data = []byte(strings.ReplaceAll(string(data),
		`href="/live/hive/light"`,
		`href="/snapshot?mode=light"`))
	data = []byte(strings.ReplaceAll(string(data),
		`href="/live/hive/dark"`,
		`href="/snapshot?mode=dark"`))
	data = []byte(strings.ReplaceAll(string(data),
		`href="/live/hive"`,
		`href="/snapshot"`))

	html := string(data)
	dashURL := ""
	if s.deps != nil && s.deps.Config != nil {
		dashURL = s.deps.Config.Hub.DashboardURL
	}
	if dashURL != "" {
		html = strings.ReplaceAll(html, `href="`+dashURL, `href="/snapshot`)
		html = strings.ReplaceAll(html, `action="`+dashURL, `action="/snapshot`)
		html = strings.ReplaceAll(html, dashURL, "/snapshot")
	}
	html = regexp.MustCompile(`href="https?://[^"]*\.hive\.kubestellar\.io[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)
	html = regexp.MustCompile(`href="http://localhost:\d+[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)
	html = regexp.MustCompile(`href="http://192\.168\.[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	// The snapshot document is built at runtime by build-snapshot.mjs and then
	// rewritten above, so its inline <script> content is only known here. Stamp
	// the CSP script-src-elem hash allowlist from the exact bytes being served
	// (#3848 part 1 / #3907, see pkg/dashboard/webstatic).
	webstatic.ApplyDocumentScriptSrcElem(w, []byte(html))
	_, _ = w.Write([]byte(html))
}

// snapshotDirOrDefault returns the directory handleSnapshotPage/buildSnapshot
// read and write snapshot-{mode}.html under: s.snapshotDir when a test has
// overridden it, otherwise the production default. See the field comment on
// Server.snapshotDir (#5235).
func (s *Server) snapshotDirOrDefault() string {
	if s.snapshotDir != "" {
		return s.snapshotDir
	}
	return "/data/snapshots"
}

func (s *Server) buildSnapshot(outputFile, mode string) {
	if s.buildSnapshotFn != nil {
		s.buildSnapshotFn(s, outputFile, mode)
		return
	}
	buildSnapshotProd(s, outputFile, mode)
}

// buildSnapshotProd is the real Node-builder invocation buildSnapshot runs in
// production. Split out from buildSnapshot so tests can override the whole
// invocation via Server.buildSnapshotFn (#5235) without ever spawning `node`
// — see the field comment on Server.buildSnapshotFn for the seam convention
// this follows (pkg/hub's afterGenerationsReadAttempt, #5080).
func buildSnapshotProd(s *Server, outputFile, mode string) {
	snapDir := s.snapshotDirOrDefault()
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		s.logger.Warn("snapshot directory creation failed", "error", err)
		return
	}
	dashURL := fmt.Sprintf("http://localhost:%d", s.port)
	htmlSource := "/opt/hive/proxy/public/index.html"
	builderScript := "/opt/hive/dashboard/build-snapshot.mjs"
	cmd := exec.Command("node", builderScript,
		"--mode", mode,
		"--base-path", "/snapshot",
		"--html", htmlSource,
		dashURL, outputFile)
	// The builder fetches /api/status (and siblings) from localhost. Those
	// endpoints require auth, so without a token the builder gets 401 and
	// bakes an empty snapshot (blank Governor/Tokens/Cost/Repos/Beads/Agents
	// panels, ACMM "--"). Pass s.authToken as DASHBOARD_AUTH_TOKEN so the
	// builder authenticates via the trusted X-Hive-Internal header path. The
	// token is used ONLY as a request header for the localhost fetch; the
	// builder never writes it into the snapshot HTML output.
	cmd.Env = snapshotBuilderEnv(os.Environ(), s.authToken)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn("snapshot build failed", "error", err, "output", string(out))
	} else {
		s.logger.Info("snapshot built", "file", outputFile)
	}
}

// snapshotBuilderEnv returns the environment for the Node snapshot builder.
// NODE_TLS_REJECT_UNAUTHORIZED=0 is always set for the localhost fetch. When
// authToken is non-empty it is exposed as DASHBOARD_AUTH_TOKEN so the builder
// can send the trusted X-Hive-Internal header and receive live data instead of
// a 401. Open/no-auth spokes (empty token) get the prior behavior unchanged.
func snapshotBuilderEnv(baseEnv []string, authToken string) []string {
	env := append(baseEnv, "NODE_TLS_REJECT_UNAUTHORIZED=0")
	if authToken != "" {
		env = append(env, "DASHBOARD_AUTH_TOKEN="+authToken)
	}
	return env
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
// constructor change) and keeps memory bounded via timeline.MaxEvents.
//
// TODO(timeline): the governor eval loop should Record() real lifecycle events
// into LifecycleTimeline() (issue enumerated → classified → kicked → pr_opened
// → merged/blocked), e.g. via a tracing SpanProcessor adapter that calls
// timeline.FromSpan. Until then the store is present but empty, and the
// endpoint safely returns empty arrays.
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

// lifecycleTimelineDefaultLimit bounds how many recent events the
// /api/lifecycle-timeline endpoint returns by default when the caller does not
// pass ?limit=.
const lifecycleTimelineDefaultLimit = 200

// handleLifecycleTimeline serves the issue→PR lifecycle timeline plus derived
// fleet health as JSON. It is additive and read-only; an empty store yields
// empty arrays (never null), so the dashboard can render unconditionally.
//
// Query params:
//
//	limit  — max recent events to return (default lifecycleTimelineDefaultLimit)
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
		dto.Events = filterTimelineEventsByACMMLevel(dto.Events, level)
		dto.Fleet = fleetHealthForTimelineEvents(store.Recent(0), window, level)
	}
	// Defensive nil-guard: Snapshot already guarantees a non-nil slice, but
	// keep the endpoint's array-always contract explicit.
	if dto.Events == nil {
		dto.Events = []timeline.Event{}
	}
	jsonResponse(w, dto)
}

func (s *Server) lifecycleACMMLevel() int {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return 0
	}
	return detectACMMLevel(s.deps.Config)
}

func filterTimelineEventsByACMMLevel(events []timeline.Event, level int) []timeline.Event {
	filtered := make([]timeline.Event, 0, len(events))
	for _, event := range events {
		if agent.AgentAvailableAtACMMLevel(event.Agent, level) {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func fleetHealthForTimelineEvents(events []timeline.Event, window time.Duration, level int) timeline.FleetHealth {
	if window <= 0 {
		window = timeline.DefaultFleetWindow
	}
	fh := timeline.FleetHealth{WindowMs: window.Milliseconds()}
	cutoff := time.Now().Add(-window).UnixMilli()
	merged := map[string]bool{}
	blocked := map[string]bool{}
	active := map[string]bool{}
	for _, event := range events {
		if event.At < cutoff || !agent.AgentAvailableAtACMMLevel(event.Agent, level) {
			continue
		}
		fh.Events++
		if event.IssueRef == "" {
			continue
		}
		switch event.Kind {
		case timeline.KindMerged:
			merged[event.IssueRef] = true
		case timeline.KindBlocked:
			blocked[event.IssueRef] = true
		default:
			active[event.IssueRef] = true
		}
	}
	for ref := range merged {
		fh.Merged++
		delete(active, ref)
		delete(blocked, ref)
	}
	for ref := range blocked {
		fh.Blocked++
		delete(active, ref)
	}
	fh.InFlight = len(active)
	return fh
}

func (s *Server) handleWidget(w http.ResponseWriter, r *http.Request) {
	state := s.deps.Governor.GetState()
	statuses := s.deps.AgentMgr.AllStatuses()

	running := 0
	paused := 0
	for _, a := range statuses {
		switch a.State {
		case "running":
			running++
		case "paused":
			paused++
		}
	}

	jsonResponse(w, map[string]interface{}{
		"mode":      state.Mode,
		"issues":    state.QueueIssues,
		"prs":       state.QueuePRs,
		"running":   running,
		"paused":    paused,
		"last_eval": state.LastEval,
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

// --- Agent control endpoints ---

func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	var body struct {
		Prompt  string `json:"prompt"`
		Message string `json:"message"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.deps.Logger.Debug("kick body decode failed, using auto-generated message", "agent", name, "error", err)
	}

	msg := body.Prompt
	if msg == "" {
		msg = body.Message
	}

	const maxKickPromptLen = 10000
	if len(msg) > maxKickPromptLen {
		jsonError(w, fmt.Sprintf("prompt too long (%d chars, max %d)", len(msg), maxKickPromptLen), http.StatusBadRequest)
		return
	}

	if msg == "" && s.deps.Scheduler != nil {
		msg = s.deps.Scheduler.BuildAgentMessageFromLastActionable(name)
	}

	// Queue the kick and answer immediately (#5325).
	//
	// The old code called the synchronous SendKick inline. Its slow leg waits
	// for the CLI's input prompt for up to inputPromptTimeout (120s), which
	// exceeds a typical ingress idle timeout (commonly 60s) — so a kick to an
	// agent whose CLI was merely slow to present its prompt was answered by the
	// proxy with 504 while the wait was still running. The wait then completed,
	// the prompt WAS typed, and the agent ran the session; the operator had
	// been told it failed, and the natural retry delivered the work twice.
	//
	// SendKickAsync keeps every fast, deterministic precondition synchronous —
	// unknown agent, paused/stopped, missing tmux session, sandbox rejection
	// still return 400 here — and moves only the prompt wait and the typing to
	// a background goroutine with an exactly-once in-flight guard. The outcome
	// is reported by GET /api/kick/{agent}/status, off the request path.
	started, err := s.deps.AgentMgr.SendKickAsync(name, msg)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !started {
		// A delivery for this agent is already in flight. Answering 202 with
		// status "in-flight" is what makes an operator's retry harmless: the
		// prompt is delivered exactly once regardless of how many times Kick
		// is clicked.
		jsonStatusResponse(w, http.StatusAccepted, map[string]interface{}{
			"ok": true, "status": kickStatusInFlight, "agent": name,
			"message": "a kick is already being delivered to " + name + "; not sending it twice",
		})
		return
	}

	s.deps.Governor.RecordKick(name)
	s.deps.Logger.Info("audit: agent kicked", "agent", name, "trigger", "dashboard-api")
	s.auditFromRequest(r, "kick", "", name)
	s.refreshAfterMutation()
	// 202, not 200: the message is queued, not yet proven delivered.
	jsonStatusResponse(w, http.StatusAccepted, map[string]interface{}{
		"ok": true, "status": kickStatusQueued, "agent": name,
	})
}

// Kick dispatch statuses on the wire. "queued"/"in-flight" are the POST's
// answers; the poll adds the terminal "delivered" and "failed".
const (
	kickStatusQueued    = "queued"
	kickStatusInFlight  = "in-flight"
	kickStatusUnknown   = "unknown"
	kickStatusDelivered = "delivered"
	kickStatusFailed    = "failed"
)

// handleKickStatus reports the outcome of the most recent asynchronous kick
// for an agent (#5325).
//
// This is where kick success or failure is now decided. The POST only promises
// the kick was queued; a client learns whether the prompt actually reached the
// CLI by polling here. While the phase is "queued"/"in-flight" the outcome is
// INDETERMINATE — pending is not failure, and a UI must not render it as one.
//
// Read-only, so any authenticated role may call it.
func (s *Server) handleKickStatus(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	d, ok := s.deps.AgentMgr.KickDispatchState(name)
	if !ok {
		// No async kick has been dispatched for this agent in this process's
		// lifetime. That is not an error — it is simply "nothing to report".
		jsonResponse(w, map[string]interface{}{
			"ok": true, "agent": name, "status": kickStatusUnknown, "pending": false,
		})
		return
	}
	resp := map[string]interface{}{
		"ok":       true,
		"agent":    name,
		"status":   kickPhaseStatus(d.Phase),
		"pending":  d.Pending(),
		"queuedAt": d.QueuedAt.UTC().Format(time.RFC3339),
	}
	if d.Error != "" {
		resp["error"] = d.Error
	}
	if !d.SettledAt.IsZero() {
		resp["settledAt"] = d.SettledAt.UTC().Format(time.RFC3339)
	}
	jsonResponse(w, resp)
}

// kickPhaseStatus maps a manager dispatch phase onto the wire status. The
// pending phase is reported as "in-flight" so the poll's vocabulary matches the
// POST's, and so no client can mistake it for a settled outcome.
func kickPhaseStatus(phase string) string {
	switch phase {
	case agent.KickPhaseDelivered:
		return kickStatusDelivered
	case agent.KickPhaseFailed:
		return kickStatusFailed
	default:
		return kickStatusInFlight
	}
}

// claimAgentFieldOwnership writes an operator's model and/or backend choice
// into hive.yaml and marks those fields operator-owned. Empty arguments leave
// the corresponding field untouched.
//
// This is the durability half of the model/method revert fix. The in-memory
// ModelOverride/BackendOverride on the agent process is replayed from
// /data/hive-state.json on restart, but hive.yaml still carried the PACK's
// model — and ApplyPack re-reconciles from the pack on every restart. Writing
// the operator's value to the same layer the pack writes, plus an ownership
// marker, is what makes the choice actually survive.
func (s *Server) claimAgentFieldOwnership(name, model, backend string) {
	if s.deps == nil || s.deps.Config == nil {
		return
	}
	ac, ok := s.deps.Config.Agents[name]
	if !ok {
		return
	}
	if model != "" {
		ac.Model = model
		ac.ModelOwner = config.FieldOwnerOperator
	}
	if backend != "" {
		ac.Backend = backend
		ac.BackendOwner = config.FieldOwnerOperator
	}
	s.deps.Config.Agents[name] = ac
	_ = s.deps.AgentMgr.UpdateConfig(name, ac)
	if err := s.saveConfig(); err != nil {
		s.deps.Logger.Error("failed to persist agent model/method choice", "agent", name, "error", err)
		s.AddSystemAlert("agent-field-save-failed", "error",
			"Could not save the model/method choice for "+name+" — it will revert on the next restart: "+err.Error())
		return
	}
	s.ClearSystemAlert("agent-field-save-failed")
}

// validateModelForAgent rejects a model the agent's effective backend does not
// offer, so an unhonorable choice surfaces as a 400 the operator can see
// instead of silently degrading to a default at launch time.
//
// Backends whose model list cannot be enumerated (no reachable endpoint, or a
// backend that accepts free-form model ids) are allowed through: refusing a
// value we simply cannot verify would block legitimate configurations.
func (s *Server) validateModelForAgent(name, model string) error {
	if model == "" {
		return fmt.Errorf("model must not be empty")
	}
	proc, err := s.deps.AgentMgr.GetStatus(name)
	if err != nil || proc == nil {
		// Agent lookup failures are reported by the caller's SetModelOverride.
		return nil
	}
	backend := proc.Config.Backend
	if proc.BackendOverride != "" {
		backend = proc.BackendOverride
	}

	known := s.modelIDsForBackend(backend)
	if len(known) == 0 {
		return nil // cannot enumerate — do not block
	}
	// Copilot catalog/CLI nomenclature drifts between "." and "-" (#4262):
	// the served list is canonical, so compare the canonicalized candidate
	// rather than rejecting a dotted/dashed variant of an available model.
	if backend == "copilot" {
		model = agent.CanonicalizeCopilotModel(model)
	}
	for _, id := range known {
		if id == model {
			return nil
		}
	}
	return fmt.Errorf("model %q is not available for backend %q (available: %s)",
		model, backend, strings.Join(known, ", "))
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	backend := sanitizeString(r.PathValue("backend"))

	if err := s.deps.AgentMgr.SetBackendOverride(name, backend); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to hive.yaml and mark the field operator-owned so the next pack
	// apply (which runs on every restart) does not reconcile it away.
	s.claimAgentFieldOwnership(name, "", backend)

	s.deps.Logger.Info("audit: backend switched", "agent", name, "backend", backend, "trigger", "dashboard-api")
	s.auditFromRequest(r, "switch_backend", auditDetail("backend", backend), name)

	// Restart the agent session so the new backend takes effect immediately.
	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		s.deps.Logger.Warn("restart after backend switch failed", "agent", name, "error", err)
	}

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "switched", "agent": name, "backend": backend})
}

func (s *Server) handleModelSet(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	model := sanitizeString(r.PathValue("model"))

	// Reject a model the effective backend cannot serve BEFORE storing it.
	// Silently accepting an unusable value and then falling back at launch is
	// what made this class of bug invisible: the grid showed the choice, the
	// agent ran on something else, and the value appeared to "revert".
	if err := s.validateModelForAgent(name, model); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := s.deps.AgentMgr.SetModelOverride(name, model); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to hive.yaml and mark the field operator-owned so the next pack
	// apply (which runs on every restart) does not reconcile it away.
	s.claimAgentFieldOwnership(name, model, "")

	s.deps.Logger.Info("audit: model set", "agent", name, "model", model, "trigger", "dashboard-api")
	s.auditFromRequest(r, "set_model", auditDetail("model", model), name)

	// Restart the agent session so the new model takes effect immediately.
	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		s.deps.Logger.Warn("restart after model switch failed", "agent", name, "error", err)
	}

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "model_set", "agent": name, "model": model})
}

// pauseStateLabel names the authoritative pause-dimension state reported by
// the pause/resume/agent-state endpoints. It deliberately says nothing about
// process health — "running" here means "not operator-paused" (the governor
// will kick it), matching what the pause/resume toggle controls.
const (
	pauseStatePaused  = "paused"
	pauseStateRunning = "running"
)

func pauseStateLabel(paused bool) string {
	if paused {
		return pauseStatePaused
	}
	return pauseStateRunning
}

// pauseToggleResponse is the shared response shape for pause/resume. `changed`
// distinguishes a real transition from a no-op: pausing an already-paused
// agent used to return the same undifferentiated success as a real pause,
// which let a dashboard with a stale belief silently re-pause an agent the
// operator was trying to START (audit showed pause-pairs seconds apart while
// the agent stayed paused indefinitely). `state` is the authoritative
// post-request pause state the client must render from.
func pauseToggleResponse(w http.ResponseWriter, status, agent string, changed, paused bool) {
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"status":  status,
		"agent":   agent,
		"changed": changed,
		"state":   pauseStateLabel(paused),
	})
}

// requireOwnerRole returns true when authenticate established owner-level access.
// A client-supplied X-Hive-Role is not enough: authenticate strips inbound role
// headers before auth and sets ownerRoleVerifiedHeader only for trusted owner
// sessions, proof-verified hub proxy identities, or shared-token operators
// (X-Hive-Internal / bearer token — possession of the secret is the owner
// credential on those deployments). Owner-only mutations must require its
// server-only verification marker, so legacy/proofless proxy headers fail
// closed instead of trusting spoofable client input.
func requireOwnerRole(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if !isOwnerRole(role) || r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		jsonError(w, "owner access required", http.StatusForbidden)
		return false
	}
	return true
}

func requireMergerOrOwnerRole(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if isOwnerRole(role) && r.Header.Get(ownerRoleVerifiedHeader) != "true" {
		jsonError(w, "merger or owner access required", http.StatusForbidden)
		return false
	}
	if !config.RoleAtLeast(role, config.RoleMerger) {
		jsonError(w, "merger or owner access required", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))

	// No-op guard: the agent is already paused. Do NOT call Pause again —
	// that would clobber the original PausedAt/reason/trigger — and tell the
	// client explicitly so it can correct its stale UI instead of believing
	// it just paused a running agent. An unknown agent falls through to
	// Pause, which returns the same 400 as before.
	if proc, err := s.deps.AgentMgr.GetStatus(name); err == nil && proc != nil && proc.Paused {
		s.auditFromRequest(r, "pause", auditDetail("result", "noop-already-paused"), name)
		pauseToggleResponse(w, "paused", name, false, true)
		return
	}

	// Record the acting user on the pause itself (#4041): the audit log has
	// always known who clicked, but the agent state did not, so a deliberate
	// owner mass-pause was indistinguishable from a malfunction days later.
	if err := s.deps.AgentMgr.PauseBy(name, "dashboard-api", "manual pause", requestUser(r)); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "pause", "", name)
	s.refreshAndPersist()
	pauseToggleResponse(w, "paused", name, true, true)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	name := s.resolveAgentParam(r.PathValue("agent"))

	// No-op guard, mirror of handlePause: resuming an agent that is not
	// paused must report changed:false with the authoritative state.
	if proc, err := s.deps.AgentMgr.GetStatus(name); err == nil && proc != nil && !proc.Paused {
		s.auditFromRequest(r, "resume", auditDetail("result", "noop-not-paused"), name)
		pauseToggleResponse(w, "resumed", name, false, false)
		return
	}

	if err := s.deps.AgentMgr.Resume(s.deps.Ctx, name, "dashboard-api", "manual resume"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "resume", "", name)
	s.refreshAndPersist()
	pauseToggleResponse(w, "resumed", name, true, false)
}

// handleAgentState is a lightweight authoritative pause-state probe. The
// dashboard's pause/resume toggle calls it immediately before acting so the
// action derives from the SERVER's persisted state rather than a stale DOM
// dataset — the belt to the response contract's suspenders above.
func (s *Server) handleAgentState(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	proc, err := s.deps.AgentMgr.GetStatus(name)
	if err != nil || proc == nil {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"ok":        true,
		"agent":     name,
		"paused":    proc.Paused,
		"state":     pauseStateLabel(proc.Paused),
		"procState": string(proc.State),
	})
}

// handleBreakerState returns the current fleet-breaker state: whether it is
// engaged and the set of agents it is holding paused. Read-only (GET), so any
// authenticated role may call it.
func (s *Server) handleBreakerState(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	engaged, agents := s.deps.AgentMgr.BreakerState()
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"engaged": engaged,
		"agents":  agents,
	})
}

// handleBreakerEngage throws the fleet breaker: it pauses every running,
// non-on-demand agent and records exactly that set. Already-paused and
// on-demand agents are left untouched (see Manager.EngageBreaker). Restricted
// to owner role because engaging the breaker halts all agents fleet-wide.
func (s *Server) handleBreakerEngage(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	paused := s.deps.AgentMgr.EngageBreaker()
	s.auditFromRequest(r, "breaker-engage",
		auditDetail("count", strconv.Itoa(len(paused)), "agents", strings.Join(paused, ",")), "")
	// Persist so an engaged breaker survives a crash, not just graceful
	// shutdown. Refresh so the dashboard reflects the newly-paused agents.
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"engaged": true,
		"agents":  paused,
	})
}

// handleBreakerRelease disengages the fleet breaker: it resumes ONLY the agents
// the breaker paused and still owns (PausedTrigger unchanged). On-demand,
// pre-existing, and operator-re-paused agents are never resumed (see
// Manager.ReleaseBreaker). Restricted to owner role.
func (s *Server) handleBreakerRelease(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.AgentMgr == nil {
		jsonError(w, "agent manager unavailable", http.StatusServiceUnavailable)
		return
	}
	resumed := s.deps.AgentMgr.ReleaseBreaker(s.deps.Ctx)
	s.auditFromRequest(r, "breaker-release",
		auditDetail("count", strconv.Itoa(len(resumed)), "agents", strings.Join(resumed, ",")), "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"engaged": false,
		"agents":  resumed,
	})
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	dimension := r.PathValue("dimension")

	var body struct {
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.deps.Logger.Debug("pin body decode failed, using current value", "agent", name, "error", err)
	}

	if body.Value == "" {
		proc, getErr := s.deps.AgentMgr.GetStatus(name)
		if getErr != nil || proc == nil {
			jsonError(w, "agent not found", http.StatusBadRequest)
			return
		}
		switch dimension {
		case "cli":
			body.Value = proc.Config.Backend
			if proc.BackendOverride != "" {
				body.Value = proc.BackendOverride
			}
		case "model":
			body.Value = proc.Config.Model
			if proc.ModelOverride != "" {
				body.Value = proc.ModelOverride
			}
		}
	}

	var err error
	switch dimension {
	case "cli":
		err = s.deps.AgentMgr.PinCLI(name, body.Value)
	case "model":
		err = s.deps.AgentMgr.PinModel(name, body.Value)
	default:
		jsonError(w, "dimension must be 'cli' or 'model'", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A pin is a stronger statement of intent than a plain switch, so it must
	// at least be as durable. Previously a pin lived only on the agent process
	// (replayed from /data/hive-state.json) while hive.yaml kept the pack's
	// value, so the pin did not protect the field from the pack re-apply that
	// runs on every restart — the pin icon was effectively decorative.
	switch dimension {
	case "model":
		s.claimAgentFieldOwnership(name, body.Value, "")
	case "cli":
		s.claimAgentFieldOwnership(name, "", body.Value)
	}

	s.deps.Logger.Info("audit: agent pinned", "agent", name, "dimension", dimension, "value", body.Value, "trigger", "dashboard-api")
	s.auditFromRequest(r, "pin", auditDetail("dimension", dimension, "value", body.Value), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "pinned", "agent": name, "dimension": dimension, "value": body.Value})
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	dimension := r.PathValue("dimension")

	var err error
	switch dimension {
	case "cli":
		err = s.deps.AgentMgr.UnpinCLI(name)
	case "model":
		err = s.deps.AgentMgr.UnpinModel(name)
	default:
		jsonError(w, "dimension must be 'cli' or 'model'", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent unpinned", "agent", name, "dimension", dimension, "trigger", "dashboard-api")
	s.auditFromRequest(r, "unpin", auditDetail("dimension", dimension), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "unpinned", "agent": name, "dimension": dimension})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))

	// Serialize restart operations to prevent concurrent pause/resume cycles
	// from interfering through shared state (tmux server, config writes).
	s.restartMu.Lock()
	defer s.restartMu.Unlock()

	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent restarted", "agent", name, "trigger", "dashboard-api")
	s.auditFromRequest(r, "restart", "", name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "restarted", "agent": name})
}

func (s *Server) handleResetRestarts(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))

	if err := s.deps.AgentMgr.ResetRestartCount(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: restart count reset", "agent", name, "trigger", "dashboard-api")
	floor := s.refreshAndPersistSeq()
	// minStatusSeq: status snapshots below this seq were built before the
	// reset — the dashboard drops them so the zeroed counter can't flicker
	// back to the stale value (#4348).
	jsonResponse(w, map[string]any{"ok": true, "status": "reset", "agent": name, "minStatusSeq": floor})
}

// --- Token access audit log ---

const (
	tokenAccessLogPath    = "/var/run/hive-metrics/token-access.jsonl"
	tokenAccessMaxEntries = 100
)

func (s *Server) handleTokenAccess(w http.ResponseWriter, r *http.Request) {
	// SECURITY (#3936, CWE-284): the token-access log records every gh CLI
	// command an agent issued, including full arguments (--repo, --title,
	// --body ...). Without a role gate any authenticated user — including
	// read-only contributors — could enumerate the hive's full GitHub operation
	// history. Gate at owner-role, consistent with handleConfigDownload and
	// handleSelfUpgrade which protect equivalent operator-only data.
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
	for _, line := range lines[start:] {
		if line == "" {
			continue
		}
		entries = append(entries, json.RawMessage(line))
	}
	jsonResponse(w, map[string]interface{}{"entries": entries})
}

// --- Token endpoints ---

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

func (s *Server) handleBudgetIgnoreGet(w http.ResponseWriter, r *http.Request) {
	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"ignored": budget.IgnoreAll,
		"agents":  budget.IgnoredAgents,
		// by_agent lets the Budget config tab show each agent's tracked
		// token burn next to its exemption toggle.
		"by_agent": budget.ByAgent,
	})
}

func (s *Server) handleBudgetIgnoreSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// The dashboard checkbox sends {"ignored": bool} (global bypass);
	// {"ignored": [names]} sets the per-agent exemption list.
	var body struct {
		Ignored json.RawMessage `json:"ignored"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	var ignoreAll bool
	if err := json.Unmarshal(body.Ignored, &ignoreAll); err == nil {
		s.deps.Governor.SetBudgetIgnoreAll(ignoreAll)
	} else {
		var agents []string
		if err := json.Unmarshal(body.Ignored, &agents); err != nil {
			jsonError(w, "ignored must be a bool or a list of agent names", http.StatusBadRequest)
			return
		}
		s.deps.Governor.SetBudgetIgnored(agents)
	}

	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"status":  "updated",
		"ignored": budget.IgnoreAll,
		"agents":  budget.IgnoredAgents,
	})
}

// --- GitHub endpoints ---

func (s *Server) handleGHAuth(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.GitHub
	authType := "token"
	if cfg.HasApp() {
		authType = "app"
	}
	jsonResponse(w, map[string]interface{}{
		"ok":              true,
		"type":            authType,
		"app_id":          cfg.AppID,
		"installation_id": cfg.InstallationID,
	})
}

var userTokenPath = "/data/gh-user-token"

func (s *Server) handleGHUserAuthStatus(w http.ResponseWriter, r *http.Request) {
	// The login status must reflect THIS request's user, not the single
	// persisted token. On a direct-route spoke, resolving from the per-user
	// session is the only correct answer — otherwise every visitor would see
	// the last-authenticated user's identity (the reported vulnerability).
	// The role is the LIVE allowlist role (session_live_role.go), not the one
	// frozen into the session at login, so the UI's idea of the user's
	// capabilities always matches what the gated endpoints will enforce; a
	// revoked session reports logged-out rather than a ghost identity.
	if sess := s.sessionFromRequest(r); sess != nil {
		if live, ok := s.liveSessionRole(sess); ok {
			jsonResponse(w, map[string]interface{}{"logged_in": true, "username": sess.Username, "role": live})
			return
		}
	}
	// Hub-proxied path: nginx injects the per-user X-Hive-User/X-Hive-Role, so
	// report THAT user rather than the single shared persisted token (which
	// would show every proxied visitor the owner's identity).
	if hubUser := r.Header.Get("X-Hive-User"); hubUser != "" {
		jsonResponse(w, map[string]interface{}{"logged_in": true, "username": hubUser, "role": r.Header.Get("X-Hive-Role")})
		return
	}
	if s.directRouteAuthzEnabled() {
		// No valid session on a direct-route spoke → not logged in for this
		// request, regardless of any persisted owner token on disk.
		jsonResponse(w, map[string]interface{}{"logged_in": false})
		return
	}
	tokenData, err := os.ReadFile(userTokenPath)
	if err != nil || len(strings.TrimSpace(string(tokenData))) == 0 {
		jsonResponse(w, map[string]interface{}{"logged_in": false})
		return
	}
	token := strings.TrimSpace(string(tokenData))
	user, err := github.ValidateToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		jsonResponse(w, map[string]interface{}{"logged_in": false, "error": "token expired or revoked"})
		return
	}
	jsonResponse(w, map[string]interface{}{"logged_in": true, "username": user.Login, "avatar_url": user.AvatarURL})
}

func (s *Server) handleGHUserAuthStart(w http.ResponseWriter, r *http.Request) {
	// Always resolves to a github.com client ID (configured or the public
	// default) — login is github.com even on GHE hives, so this never fails for
	// a blank oauth_client_id and never uses a GHE client.
	clientID := s.deps.Config.GitHub.OAuthClientIDResolved()

	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()

	state, err := github.StartDeviceFlow(clientID, s.deps.Config.GitHub.OAuthBaseURL(), s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.deviceFlowState = state
	s.auditFromRequest(r, "gh_auth_start", "", "")
	jsonResponse(w, map[string]interface{}{
		"user_code":        state.UserCode,
		"verification_uri": state.VerificationURI,
		"expires_in":       state.ExpiresIn,
		"interval":         state.Interval,
	})
}

func (s *Server) handleGHUserAuthPoll(w http.ResponseWriter, r *http.Request) {
	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()

	if s.deviceFlowState == nil {
		jsonError(w, "no device flow in progress — call /api/gh-user-auth/start first", http.StatusBadRequest)
		return
	}

	clientID := s.deps.Config.GitHub.OAuthClientIDResolved()
	token, status, err := github.PollDeviceFlow(clientID, s.deviceFlowState.DeviceCode, s.deps.Config.GitHub.OAuthBaseURL(), s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil {
		s.deviceFlowState = nil
		jsonResponse(w, map[string]interface{}{"status": "error", "error": err.Error()})
		return
	}
	if status == "authorization_pending" {
		jsonResponse(w, map[string]interface{}{"status": "pending"})
		return
	}
	if status == "slow_down" {
		jsonResponse(w, map[string]interface{}{"status": "slow_down"})
		return
	}

	// Resolve the GitHub identity BEFORE persisting anything. An unauthorized
	// user's token must never be written to disk or wired in as the hive's user
	// client — otherwise a rejected login would still leak its token into the
	// shared client and become the hive's identity.
	user, err := github.ValidateToken(token, s.deps.Config.GitHub.OAuthAPIURL())
	if err != nil || user == nil || user.Login == "" {
		s.deviceFlowState = nil
		// Audit the failed login so the owner can see attempts that never got
		// far enough to resolve a GitHub identity. Actor is "unknown" because we
		// could not verify who they are.
		s.audit.Log("unknown", "login_error", auditDetail("reason", "could not verify GitHub identity"), "")
		jsonResponse(w, map[string]interface{}{"status": "error", "error": "could not verify GitHub identity"})
		return
	}
	s.deviceFlowState = nil
	username := user.Login
	avatarURL := user.AvatarURL

	// Per-hive authorization: on a direct-route spoke, only GitHub users on the
	// configured allowlist may obtain a session. The hub-proxied path is gated
	// upstream by nginx and leaves the allowlist empty, so it is unaffected.
	role := config.RoleOwner
	if s.directRouteAuthzEnabled() {
		resolvedRole, ok := s.deps.Config.Dashboard.AuthorizedRole(username)
		if !ok {
			// Do NOT persist the token, do NOT set a session, do NOT log the
			// user in. Reject before any state is written.
			s.deps.Logger.Warn("device-flow login rejected: user not authorized for this hive", "username", username)
			// Audit the denied login with the attempted GitHub username as the
			// actor so the owner can see WHO tried and was rejected.
			s.audit.Log(username, "login_denied", auditDetail("reason", "not authorized for this hive"), "")
			// Return a terminal {status:"error"} the login page understands, not a
			// bare jsonError — the device-flow poll loop only stops on status
			// "complete" or "error", so a plain {error} would leave it spinning
			// "Waiting for authorization…" forever after a denial.
			jsonResponse(w, map[string]interface{}{
				"status": "error",
				"error":  "your GitHub account (" + username + ") is not authorized to access this hive. Contact the hive owner to request access.",
			})
			return
		}
		role = resolvedRole
	}

	// The login token is used ONLY to prove identity (username + role, above).
	// It is deliberately NOT persisted and NOT installed as a hive write
	// identity: every GitHub write goes through the App installation token, so
	// there is nothing for a user token to do. This is what lets the device flow
	// request no scope at all (issue #1927) — an owner logging in no longer has
	// to grant "read and write all repositories" just to administer their hive.
	// Both owners and viewers get an identity-only per-user session below.

	s.deps.Logger.Info("GitHub user authenticated via device flow", "username", username, "role", role)

	// Issue a per-user session (opaque random id → username+role) instead of a
	// single shared cookie. Each authenticated user gets their own session so
	// requests resolve to the user that owns their cookie — never a shared one.
	//
	// Always mint the session. This used to be gated on s.authToken != "", which
	// meant a spoke with no dashboard token logged "authenticated via device
	// flow", returned {status:"complete"}, and set NO cookie at all — so "/"
	// rejected the request and the login page bounced forever (same failure
	// handleSSO fixed). The session store is the authority on identity here and
	// does not depend on a shared token existing.
	sid := s.createUserSession(username, role)
	if sid == "" {
		jsonResponse(w, map[string]interface{}{"status": "error", "error": "failed to create session"})
		return
	}
	setSessionCookie(w, r, sid)
	// Mint the short-lived per-hive terminal assertion cookie the Node proxy
	// verifies as its PRIMARY per-hive gate (finding C3 follow-up). Same
	// {user,hive,role} just authorized for this session, bound to THIS hive
	// with an expiry. No-op on non-hosted hives.
	s.setTerminalAssertionCookie(w, r, username, role)

	// Audit the successful login with the authenticated GitHub username as the
	// actor (not the request's X-Hive-User, which has no session yet at this
	// point) so the owner can see WHO logged in.
	s.audit.Log(username, "login", auditDetail("method", "github device flow", "role", role), "")
	jsonResponse(w, map[string]interface{}{"status": "complete", "username": username, "avatar_url": avatarURL})
}

// handleSSO exchanges a hub-minted, HMAC-signed handoff token for a local
// per-user session, so a user already authenticated on the hub can open this
// direct-route spoke without a second GitHub device-flow login. It fails closed:
// the token must verify against the shared HIVE_HUB_SECRET, be scoped to THIS
// hive, be unexpired, AND carry a username that is in this spoke's
// authorized_users allowlist. On success it mints the same kind of session the
// device flow does and redirects to the dashboard root.
func (s *Server) handleSSO(w http.ResponseWriter, r *http.Request) {
	// Loop breaker. An authentication failure must never present as an infinite
	// redirect: if this navigation has already been through the handoff
	// maxSSOHops times, something between here and "/" is bouncing us and no
	// further hop will help. Stop and say so.
	hop, _ := strconv.Atoi(r.URL.Query().Get(ssoHopParam))
	if hop >= maxSSOHops {
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("sso handoff loop detected", "hops", hop)
		}
		writeSSOError(w, r, http.StatusLoopDetected, ssoErrLoopDetected,
			"Signing in to this hive redirected in a circle, so it was stopped before your browser gave up.",
			"Sign in directly with GitHub below. If that also fails, the hive's access settings likely disagree with the hub's — ask the hive operator to check that your account is on this hive's authorized-users list.")
		return
	}

	// SSO verification uses the hub's Ed25519 PUBLIC key (C2 follow-up: SSO is
	// asymmetric). A hub-hosted spoke is injected HIVE_SSO_PUBLIC_KEY and never the
	// master or any signing seed, so a spoke operator who reads its pod env cannot
	// mint SSO-as-any-owner tokens — only the hub, holding the private seed, can
	// sign. SpokeSSOPublicKey falls back to deriving the public key from
	// HIVE_HUB_SECRET for self-hosted/legacy spokes that still hold the master, so
	// verification succeeds against the hub-minted token either way.
	// ROTATION (master-key-rotation.md follow-on #6): a spoke may hold TWO hub
	// public keys during a master rotation — the current generation's and the
	// outgoing one's — because the hub starts minting under the new generation
	// immediately while the reconcile lane takes ~6h to walk the fleet. Trying
	// only the primary key would 401 every SSO handoff into a not-yet-reconciled
	// spoke for those hours. SpokeSSOPublicKeys returns current-then-previous,
	// and returns exactly ONE key — byte-identical to SpokeSSOPublicKey — when
	// HIVE_SSO_PUBLIC_KEY_PREV is absent, which is its state on every spoke
	// today and on every spoke that has never seen a rotation.
	pubKeys := hub.SpokeSSOPublicKeys()
	if len(pubKeys) == 0 {
		// No verification key → SSO cannot be verified. Terminate with an
		// explanation. Redirecting to "/" here is what produced the historical
		// infinite bounce: "/" is auth-gated, sends the user back to the hub
		// login, the hub sees a valid session and hands off to /sso again.
		writeSSOError(w, r, http.StatusServiceUnavailable, ssoErrNoSecret,
			"This hive has no hub SSO verification key configured, so single sign-on from the hub cannot be verified.",
			"Ask the hive operator to set HIVE_HUB_SECRET (or HIVE_SSO_PUBLIC_KEY) on this hive. In the meantime you can sign in directly with GitHub using the button below.")
		return
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		writeSSOError(w, r, http.StatusBadRequest, ssoErrMissingToken,
			"This single sign-on link is missing its handoff token.",
			"Open the hive from the hub dashboard rather than pasting the /sso URL directly.")
		return
	}

	hiveID := ""
	if s.deps != nil && s.deps.Config != nil {
		hiveID = s.deps.Config.HiveID
	}

	username, tokenRole, _, err := hub.VerifySSOTokenAcrossKeys(pubKeys, token, hiveID, time.Now())
	if err != nil {
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("sso handoff rejected", "error", err.Error())
		}
		// Don't leak which check failed, but DO terminate here with an
		// explanation rather than serving anything that re-enters the handoff.
		writeSSOError(w, r, http.StatusUnauthorized, ssoErrBadToken,
			"The single sign-on handoff token was rejected — it is expired, malformed, or was issued for a different hive.",
			"Go back to the hub dashboard and open this hive again to get a fresh link. Handoff links are short-lived by design.")
		return
	}

	// The token proves the hub authenticated this user, but authorization is
	// still LOCAL to the spoke: the user must be in THIS hive's allowlist. This
	// keeps the spoke the authority on who may enter, even via SSO — a valid
	// hub token for a user not on the allowlist is refused. The role comes from
	// the allowlist (authoritative), not the token, so the hub can never
	// escalate a user's role on the spoke. liveAllowlistRole is the same shared
	// rule authenticate re-applies on EVERY subsequent request
	// (session_live_role.go), so the role minted here can never outlive a later
	// Manage Access change.
	role, authorized := s.liveAllowlistRole(username, tokenRole)
	if !authorized {
		// Allowlist is enforced and this user isn't on it → deny.
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("sso handoff: user not authorized for this hive", "username", username)
		}
		writeSSOError(w, r, http.StatusForbidden, ssoErrNotAuthorized,
			"You are signed in to the hub, but this hive's own authorized-users list does not include your account.",
			"Ask the hive owner to grant you access to this hive, then open it again from the hub dashboard.")
		return
	}
	if role == "" {
		role = config.RoleRead
	}

	// Always mint the session. This used to be gated on s.authToken != "", which
	// meant a spoke with no dashboard token logged "authenticated via SSO",
	// redirected to "/", and set NO cookie at all — so "/" rejected the request
	// and the browser bounced forever. The session store is the authority on
	// identity here and does not depend on a shared token existing.
	sid := s.createUserSession(username, role)
	if sid == "" {
		writeSSOError(w, r, http.StatusInternalServerError, ssoErrSessionFailed,
			"The hive could not create a login session for you.",
			"This is a problem on the hive itself. Retry in a moment; if it persists, ask the hive operator to check the dashboard logs.")
		return
	}
	setSessionCookie(w, r, sid)
	// Mint the short-lived per-hive terminal assertion cookie the Node proxy
	// verifies as its PRIMARY per-hive gate (finding C3 follow-up). The role here
	// is the spoke's authoritative allowlist role (resolved above), bound to THIS
	// hive with an expiry. No-op on non-hosted hives.
	s.setTerminalAssertionCookie(w, r, username, role)
	s.audit.Log(username, "login", auditDetail("method", "hub sso handoff", "role", role), "")
	if s.deps != nil && s.deps.Logger != nil {
		s.deps.Logger.Info("user authenticated via hub SSO handoff", "username", username, "role", role)
	}
	// Land on the dashboard root, carrying a hop counter. If something upstream
	// bounces us back into /sso anyway, the counter lets the NEXT handoff detect
	// the cycle and stop with an error instead of spinning (see the ssoHopParam
	// check at the top of this handler).
	http.Redirect(w, r, "/?"+ssoHopParam+"="+strconv.Itoa(hop+1), http.StatusSeeOther)
}

// SSO handoff failure identifiers, surfaced on the terminal error page so a
// user can quote one to an operator and an operator can grep for it.
const (
	ssoErrNoSecret      = "SSO_NO_HUB_SECRET"
	ssoErrMissingToken  = "SSO_MISSING_TOKEN"
	ssoErrBadToken      = "SSO_TOKEN_REJECTED"
	ssoErrNotAuthorized = "SSO_NOT_AUTHORIZED"
	ssoErrSessionFailed = "SSO_SESSION_FAILED"
	ssoErrLoopDetected  = "SSO_REDIRECT_LOOP"
)

// ssoHopParam is the query parameter carrying how many times this browser has
// been through the SSO handoff for this navigation. The hub's handoff link
// carries no hop, so a normal first visit is hop 0; each success redirect
// increments it. Anything that bounces the browser back into /sso preserves the
// query string, so a genuine cycle counts upward and trips maxSSOHops.
const ssoHopParam = "sso_hop"

// maxSSOHops is how many SSO handoffs are allowed for one navigation before we
// declare a redirect loop and stop. A healthy handoff takes exactly one hop; a
// small allowance absorbs a legitimate re-handoff (e.g. a racing session
// expiry) without letting a true cycle run away. Browsers give up around 20
// redirects with an opaque error, so we must terminate well before that to be
// the one that explains what happened.
const maxSSOHops = 3

// writeSSOError terminates an SSO handoff with a self-contained HTML page that
// says what failed and what to do about it. It exists because the alternative —
// redirecting a failed handoff back toward login — is what produces an infinite
// browser bounce, the single worst failure mode here: it tells the user nothing
// and is indistinguishable from an outage. Any non-success exit from handleSSO
// must come through this function.
//
// It also clears any stale session cookie, so a half-established session cannot
// keep re-triggering the same failure on reload.
func writeSSOError(w http.ResponseWriter, r *http.Request, status int, code, what, action string) {
	clearSessionCookie(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A failed handoff must never be cached; a cached 401/403 would make the
	// hive look permanently broken even after access is granted.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, ssoErrorPage, html.EscapeString(code), html.EscapeString(what), html.EscapeString(action))
}

// ssoErrorPage is the terminal error shell for a failed SSO handoff. Format
// verbs in order: %[1]s error code, %[2]s what happened, %[3]s what to do. It
// deliberately offers "/" (which serves the device-flow login page when
// unauthenticated) as an escape hatch and a link back to the hub, so the user
// is never stranded.
const ssoErrorPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign-in failed — Hive</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;display:flex;justify-content:center;align-items:center;min-height:100vh;padding:24px}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:40px;max-width:560px}
.bee{font-size:2.5rem;margin-bottom:12px}
h1{font-size:1.5rem;margin-bottom:16px}
p{color:#8b949e;line-height:1.6;margin-bottom:16px}
.action{color:#e6edf3}
code{background:#0d1117;border:1px solid #30363d;border-radius:6px;padding:2px 8px;font-size:0.8rem;color:#f0883e}
.btn{display:inline-block;padding:10px 20px;border-radius:8px;text-decoration:none;font-weight:600;font-size:0.9rem;margin:16px 8px 0 0}
.btn-primary{background:#238636;color:#fff}
.btn-secondary{background:transparent;color:#58a6ff;border:1px solid #30363d}
</style></head>
<div class="card">
<div class="bee">&#128029;</div>
<h1>Sign-in didn't complete</h1>
<p>%[2]s</p>
<p class="action">%[3]s</p>
<p>Error code: <code>%[1]s</code></p>
<a class="btn btn-primary" href="/">Sign in with GitHub</a>
<a class="btn btn-secondary" href="https://hive.kubestellar.io/dashboard">Back to the hub</a>
</div>
</html>`

func (s *Server) handleGHUserAuthLogout(w http.ResponseWriter, r *http.Request) {
	// Clear only THIS request's session so logging out affects one user, not
	// everyone. Removing the disk token only makes sense when the logging-out
	// user is the one whose token is persisted (the owner/last-authenticated
	// user); an anonymous POST must never wipe the owner's persisted token.
	var loggedOut, loggedOutRole string
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		clearSessionCookie(w)
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	sess := s.lookupSession(c.Value)
	if sess == nil {
		s.deleteSession(c.Value)
		clearSessionCookie(w)
		jsonError(w, "GitHub user session required", http.StatusUnauthorized)
		return
	}
	loggedOut = sess.Username
	loggedOutRole = sess.Role
	s.deleteSession(c.Value)

	// Only clear the persisted GitHub token when the logging-out user is the
	// owner (read-write). Use the role bound to the session at login time — not
	// a fresh config lookup — so a later allowlist change can't leave a
	// logging-out owner's own token stranded on disk. Viewer logouts leave the
	// hive's user client intact.
	if loggedOutRole == config.RoleOwner {
		if err := os.Remove(userTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.deps.Logger.Error("GitHub user token removal failed", "error", err)
			jsonError(w, "failed to remove persisted GitHub credentials", http.StatusInternalServerError)
			return
		}
	}
	clearSessionCookie(w)
	s.auditFromRequest(r, "gh_auth_logout", "", "")
	s.deps.Logger.Info("GitHub user logged out", "username", loggedOut)
	jsonResponse(w, map[string]interface{}{"status": "logged_out"})
}

func (s *Server) handleGHUserAuthSession(w http.ResponseWriter, r *http.Request) {
	// This endpoint only lands the user on the dashboard after the device flow
	// completed. The per-user session cookie was already set by the poll
	// handler; we never mint a session here (doing so from a shared secret or a
	// disk token file would grant any caller a valid session).
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleGHRateLimits(w http.ResponseWriter, r *http.Request) {
	if s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	limits, err := s.deps.GHClient.RateLimits(s.deps.Ctx)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, limits)
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

// --- Agent config endpoints ---

func (s *Server) handleAgentConfigGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	proc, err := s.deps.AgentMgr.GetStatus(name)

	cli := agentCfg.Backend
	if err == nil && proc.BackendOverride != "" {
		cli = proc.BackendOverride
	}
	model := agentCfg.Model
	if err == nil && proc.ModelOverride != "" {
		model = proc.ModelOverride
	}

	// Use configured launch command if available; otherwise construct one
	launchCmd := agentCfg.LaunchCmd
	if launchCmd == "" {
		launchCmd = fmt.Sprintf("%s --model %s", cli, model)
		switch cli {
		case "claude":
			launchCmd = fmt.Sprintf("claude --model %s --dangerously-skip-permissions", model)
		case "copilot":
			launchCmd = fmt.Sprintf("/usr/bin/copilot --allow-all --model %s", model)
		}
	}

	// Use configured display name if available
	displayName := agentCfg.DisplayName
	if displayName == "" {
		displayName = ""
	}

	// Stale timeout from config, default to 28800 (8 hours).
	// Must exceed the longest cadence interval to avoid false stale flags.
	const defaultStaleTimeoutS = 28800
	staleTimeout := agentCfg.StaleTimeout
	if staleTimeout == 0 {
		staleTimeout = defaultStaleTimeoutS
	}

	// Restart strategy from config, default to "immediate"
	restartStrategy := agentCfg.RestartStrategy
	if restartStrategy == "" {
		restartStrategy = "immediate"
	}

	// Cadences as seconds (int) — frontend expects numbers, not duration strings
	cadences := map[string]any{}
	for modeName, modeCfg := range s.deps.Config.Governor.Modes {
		if c, ok := modeCfg.Cadences[name]; ok {
			if c.Mode() != config.CadenceModeInterval {
				cadences[modeName] = c
			} else if c.Interval() == "pause" || c.Interval() == "off" || c.Interval() == "0" {
				cadences[modeName] = 0
			} else {
				d := parseCadenceDuration(c.Interval())
				cadences[modeName] = int64(d.Seconds())
			}
		}
	}

	// Per-mode models (empty strings = inherit from general)
	models := map[string]string{}
	for modeName := range s.deps.Config.Governor.Modes {
		models[modeName] = ""
	}

	lastPrompt := ""
	pinnedCLI := agentCfg.CLIPinned
	if err == nil {
		lastPrompt = proc.LastKickMessage
		pinnedCLI = pinnedCLI || proc.PinnedCLI != ""
	}

	// Read restrictions from agent work dir files
	restrictions := s.loadAgentRestrictions(name)

	// Read prompt template from agent policy file
	promptTemplate := s.loadPromptTemplate(name)

	// Read stat sources from config
	stats := s.loadAgentStats(name)

	pipeline := s.getAgentPipeline(name)
	hooks := s.getAgentHooks(name)

	includeRepos := true
	if agentCfg.IncludeRepos != nil {
		includeRepos = *agentCfg.IncludeRepos
	}

	jsonResponse(w, map[string]interface{}{
		"general": map[string]interface{}{
			"enabled":          agentCfg.Enabled,
			"launchCmd":        launchCmd,
			"displayName":      displayName,
			"description":      agentCfg.Description,
			"cliPinned":        pinnedCLI,
			"cliPinValue":      cli,
			"modelOwner":       agentCfg.ModelOwner,
			"backendOwner":     agentCfg.BackendOwner,
			"staleTimeout":     staleTimeout,
			"restartStrategy":  restartStrategy,
			"model":            model,
			"clearOnKick":      agentCfg.ClearOnKick,
			"emoji":            agentCfg.Emoji,
			"color":            agentCfg.Color,
			"sortOrder":        agentCfg.SortOrder,
			"beadRole":         agentCfg.BeadRole,
			"role":             agentCfg.Role,
			"kickTemplate":     agentCfg.KickTemplate,
			"promptSource":     agentCfg.PromptSource,
			"definitionSource": agentCfg.DefinitionSource,
			"mode":             agentCfg.Mode,
			"includeRepos":     includeRepos,
			"laneKeywords":     agentCfg.LaneKeywords,
			"detectKeywords":   agentCfg.DetectKeywords,
			"aliases":          agentCfg.Aliases,
			"cavemanMode":      agentCfg.CavemanMode,
			"explainMode":      agentCfg.ExplainMode,
			"sandboxEnabled":   agentCfg.Sandbox != nil && agentCfg.Sandbox.Enabled != nil && *agentCfg.Sandbox.Enabled,
			"sandboxEffective": agentCfg.SandboxEnabled(s.deps.Config.AgentSandbox),
			"replicas":         agentCfg.Replicas,
		},
		"cadences":       cadences,
		"models":         models,
		"pipeline":       pipeline,
		"hooks":          hooks,
		"restrictions":   restrictions,
		"stats":          stats,
		"prompt":         lastPrompt,
		"promptTemplate": promptTemplate,
		"channels":       agentCfg.Channels,
		"tools":          agentCfg.Tools,
		"connections":    maskConnectionAuth(agentCfg.Connections),
	})
}

type restriction struct {
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

func (s *Server) loadAgentRestrictions(name string) map[string]interface{} {
	result := map[string]interface{}{
		"agent":  []any{},
		"global": []any{},
		"policy": []any{},
	}

	// Read global restrictions from /data/restrictions.conf (one pattern per line)
	globalRestrictions := []any{}
	if data, err := os.ReadFile("/data/restrictions.conf"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			r := restriction{Pattern: parts[0], Source: "global"}
			if len(parts) > 1 {
				r.Reason = parts[1]
			}
			globalRestrictions = append(globalRestrictions, r)
		}
	}
	result["global"] = globalRestrictions

	// Read agent-specific restrictions
	agentRestrictions := []any{}
	agentRestFile := fmt.Sprintf("/data/agents/%s/restrictions.conf", name)
	if data, err := os.ReadFile(agentRestFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			r := restriction{Pattern: parts[0], Source: "agent"}
			if len(parts) > 1 {
				r.Reason = parts[1]
			}
			agentRestrictions = append(agentRestrictions, r)
		}
	}
	result["agent"] = agentRestrictions

	// Read policy restrictions from agent policy file
	// Old hive extracts lines containing policy-relevant keywords, including
	// markdown-formatted lines with ** bold markers and numbered list items.
	policyRestrictions := []any{}
	policyContent := s.loadPromptTemplate(name)
	if policyContent != "" {
		content := policyContent
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			// Strip leading markdown list markers (-, *, numbered)
			stripped := line
			if len(stripped) > 2 && (stripped[0] == '-' || stripped[0] == '*') && stripped[1] == ' ' {
				stripped = strings.TrimSpace(stripped[2:])
			}
			// Strip bold markers for pattern matching
			plain := strings.ReplaceAll(stripped, "**", "")

			if strings.HasPrefix(plain, "NEVER ") ||
				strings.HasPrefix(plain, "Do not ") ||
				strings.HasPrefix(plain, "Do NOT ") ||
				strings.HasPrefix(plain, "ALWAYS ") ||
				strings.HasPrefix(plain, "Never ") ||
				strings.Contains(plain, "HARD RULE") ||
				strings.Contains(plain, "LANE BOUNDARY") ||
				strings.Contains(plain, "DO NOT") {
				// Truncate very long lines to keep the response manageable
				const maxPolicyLen = 200
				entry := stripped
				if runes := []rune(entry); len(runes) > maxPolicyLen {
					entry = string(runes[:maxPolicyLen]) + "..."
				}
				policyRestrictions = append(policyRestrictions, restriction{
					Pattern: entry,
					Source:  "policy",
				})
			}
		}
	}
	result["policy"] = policyRestrictions

	return result
}

func (s *Server) loadPromptTemplate(name string) string {
	templateName := ""
	if s.deps != nil && s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
			templateName = ac.KickTemplate
		}
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	paths := []string{
		fmt.Sprintf("/data/policies/examples/kubestellar/agents/%s", templateName),
	}
	if s.deps != nil && s.deps.Config != nil {
		policyDir := s.deps.Config.Policies.LocalDir
		if policyDir != "" {
			paths = append(paths,
				fmt.Sprintf("%s/examples/kubestellar/agents/%s", policyDir, templateName),
				fmt.Sprintf("%s/%s%s", policyDir, s.deps.Config.Policies.Path, templateName),
			)
		}
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return s.substituteTemplateVars(string(data), name)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return s.substituteTemplateVars(string(data), name)
	}
	return ""
}

// substituteTemplateVars replaces ${VAR} placeholders in a prompt template
// with values from the running config, so the dashboard shows resolved content
// instead of raw variable names.
// handleVariablesList returns the operator-defined ${VAR} substitutions from
// the config `variables:` block, plus whether the exec/http resolver gates are
// enabled. It is READ-ONLY and never returns any secret value — only the
// variable name, type, scope, and (for env vars) the source env var NAME. This
// is the admin-facing view of custom variables; script/http variables and the
// security gates remain seed-only (GitOps/ConfigMap), so there is intentionally
// no companion write endpoint for them.
func (s *Server) handleVariablesList(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonResponse(w, map[string]any{"variables": []any{}, "exec_enabled": false, "http_enabled": false})
		return
	}
	v := s.deps.Config.Variables
	type varView struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Scope string `json:"scope"`
		// Source is a non-secret provenance hint: for env, the source env var
		// name; for static, "static"; for script, "script"; for http, the host.
		Source string `json:"source"`
	}
	out := make([]varView, 0, len(v.Defs))
	for name, def := range v.Defs {
		typ := def.Type
		if typ == "" {
			if def.Value != "" {
				typ = "static"
			} else {
				typ = "env"
			}
		}
		scope := def.Scope
		if scope == "" {
			scope = "template"
		}
		source := typ
		switch typ {
		case "env":
			if def.Env != "" {
				source = "env:" + def.Env
			} else {
				source = "env:" + name
			}
		case "http":
			if u, err := url.Parse(def.URL); err == nil && u.Host != "" {
				source = "http:" + u.Host
			} else {
				source = "http"
			}
		}
		out = append(out, varView{Name: name, Type: typ, Scope: scope, Source: source})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	jsonResponse(w, map[string]any{
		"variables":    out,
		"exec_enabled": v.Security.AllowExec,
		"http_enabled": v.Security.AllowHTTP,
	})
}

// variableNamePattern restricts dashboard-created variable names to the safe
// ${NAME} identifier shape (letters, digits, underscore; not starting with a
// digit). This is what a kick template references as ${NAME}.
var variableNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// looksLikeApiKeyValue heuristically flags a string that looks like a secret
// key value rather than a safe static config value — an sk-/ghp-/token-style
// prefix, or a long high-entropy run with no spaces. Mirrors the client-side
// guard that keeps operators from inlining a secret into hive.yaml (where
// Config.Save would persist it in plaintext).
func looksLikeApiKeyValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"sk-", "sk_", "ghp_", "gho_", "github_pat_", "xoxb-", "bearer "} {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	// A long token with no whitespace and no path/URL punctuation is likely a
	// secret, not a config value like "production" or "us-east-1".
	if len(v) >= 32 && !strings.ContainsAny(v, " /\\:.") {
		return true
	}
	return false
}

// handleAuthorizedUsersList returns the spoke's device-flow login allowlist —
// the users permitted to sign in to this hive and their roles — READ-ONLY. The
// list is authoritative on the hub (Manage Access) and propagated to the spoke
// via heartbeat; it is shown here so an operator can see who has access without
// editing it on the spoke. The first entry defaults to owner, later entries to
// read, matching AuthorizedRole's resolution.
func (s *Server) handleAuthorizedUsersList(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonResponse(w, map[string]any{"users": []any{}, "enforced": false})
		return
	}
	entries := s.deps.Config.Dashboard.AuthorizedUsers
	// names is the purely cosmetic key→display-name companion delivered by the
	// hub's heartbeat (AuthorizedUserNames). It is never consulted for
	// authorization — only AuthorizedUsers (the raw key) is — so a nil/empty
	// map here just means every row falls back to its raw key, exactly as
	// before this field existed.
	names := s.deps.Config.Dashboard.AuthorizedUserNames
	type userView struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		// DisplayName is the human-readable name for Username, when the hub
		// knows one (see AuthorizedUserNames). omitempty: absent means "no
		// known name", and the UI must render Username (the raw identity key)
		// instead — never blank, never "undefined".
		DisplayName string `json:"display_name,omitempty"`
	}
	out := make([]userView, 0, len(entries))
	for i, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		name, role := e, ""
		if idx := strings.LastIndex(e, ":"); idx >= 0 {
			name, role = strings.TrimSpace(e[:idx]), strings.TrimSpace(e[idx+1:])
		}
		if name == "" {
			continue
		}
		if role == "" {
			if i == 0 {
				role = "owner"
			} else {
				role = "read"
			}
		}
		out = append(out, userView{Username: name, Role: role, DisplayName: strings.TrimSpace(names[name])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	jsonResponse(w, map[string]any{
		// enforced == this is a direct-route spoke that gates logins by this list
		// (the heartbeat-only cluster); false means hub-proxied (the hub-reachable cluster), where nginx gates instead.
		"users":    out,
		"enforced": len(entries) > 0,
	})
}

// handleVariableUpsert creates or updates a single operator variable via the
// dashboard. It is deliberately restricted to the two SAFE resolver types —
// static and env — which cannot execute code or reach the network. script and
// http variables, and the exec/http security policy, are seed-only (GitOps): a
// user-writable overlay must never be able to introduce them, so this endpoint
// rejects them. The change is persisted to the dashboard overlay like any other
// dashboard config edit.
func (s *Server) handleVariableUpsert(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	if !variableNamePattern.MatchString(name) {
		jsonError(w, "invalid variable name (use letters, digits, underscore; not starting with a digit)", http.StatusBadRequest)
		return
	}
	var body struct {
		Type    string  `json:"type"`
		Scope   string  `json:"scope"`
		Value   string  `json:"value"`
		Env     string  `json:"env"`
		Default *string `json:"default"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	switch body.Type {
	case "static", "env":
		// allowed
	case "script", "http":
		jsonError(w, "script and http variables can only be defined in the seed config (GitOps), not from the dashboard", http.StatusForbidden)
		return
	default:
		jsonError(w, "type must be 'static' or 'env'", http.StatusBadRequest)
		return
	}
	switch body.Scope {
	case "", "template", "config", "both":
		// allowed
	default:
		jsonError(w, "scope must be 'template', 'config', or 'both'", http.StatusBadRequest)
		return
	}
	// Guard against a secret value being pasted into a static var (it would be
	// persisted to hive.yaml in plaintext). Reuse the same heuristic the LiteLLM
	// key fields use.
	if body.Type == "static" && looksLikeApiKeyValue(body.Value) {
		jsonError(w, "that value looks like an API key or secret; use type 'env' pointing at an environment variable instead of inlining a secret", http.StatusBadRequest)
		return
	}

	def := config.VarDef{Type: body.Type, Scope: body.Scope, Value: body.Value, Env: body.Env, Default: body.Default}

	if s.deps.Config.Variables.Defs == nil {
		s.deps.Config.Variables.Defs = map[string]config.VarDef{}
	}
	s.deps.Config.Variables.Defs[name] = def
	if err := s.saveConfig(); err != nil {
		jsonError(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "variable_upsert", auditDetail("name", name, "type", body.Type), "")
	jsonResponse(w, map[string]any{"status": "saved", "name": name})
}

// handleVariableDelete removes an operator variable. Only static/env variables
// are dashboard-managed, so this refuses to delete a script/http variable
// (those are seed-only and must be removed via the seed config).
func (s *Server) handleVariableDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	name := r.PathValue("name")
	def, ok := s.deps.Config.Variables.Defs[name]
	if !ok {
		jsonError(w, "variable not found", http.StatusNotFound)
		return
	}
	if def.Type == "script" || def.Type == "http" {
		jsonError(w, "script and http variables are seed-managed and cannot be deleted from the dashboard", http.StatusForbidden)
		return
	}
	delete(s.deps.Config.Variables.Defs, name)
	if err := s.saveConfig(); err != nil {
		jsonError(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "variable_delete", auditDetail("name", name), "")
	jsonResponse(w, map[string]any{"status": "deleted", "name": name})
}

func (s *Server) substituteTemplateVars(template, agentName string) string {
	if s.deps == nil || s.deps.Config == nil {
		return template
	}
	cfg := s.deps.Config
	org := cfg.Project.Org
	primaryRepo := cfg.Project.PrimaryRepo

	// Build full primary repo path (org/repo) if not already qualified
	fullPrimaryRepo := primaryRepo
	if org != "" && !strings.Contains(primaryRepo, "/") {
		fullPrimaryRepo = fmt.Sprintf("%s/%s", org, primaryRepo)
	}

	reposList := strings.Join(cfg.Project.Repos, ", ")

	displayName := agentName
	if ac, ok := cfg.Agents[agentName]; ok && ac.DisplayName != "" {
		displayName = ac.DisplayName
	}

	// The dashboard preview substitutes the config-only subset of kick-template
	// variables (it has no live GitHub context). Route it through the same
	// resolve engine as the scheduler so operator-defined ${VAR}s (from the
	// config `variables:` block) render in previews too, and the two paths can
	// no longer drift. Built-ins win; with no operator variables the output
	// matches the previous strings.NewReplacer exactly.
	lit := func(v string) func() string { return func() string { return v } }
	rt := &resolve.RuntimeContext{Vars: map[string]func() string{
		"AGENT_NAME":           lit(agentName),
		"AGENT_DISPLAY_NAME":   lit(displayName),
		"PROJECT_NAME":         lit(cfg.Project.Name),
		"PROJECT_ORG":          lit(org),
		"PROJECT_PRIMARY_REPO": lit(fullPrimaryRepo),
		"PROJECT_AI_AUTHOR":    lit(cfg.EffectiveAIAuthor()),
		"PROJECT_REPOS_LIST":   lit(reposList),
		"HIVE_REPO":            lit(fmt.Sprintf("%s/hive", org)),
		"HIVE_ID":              lit(cfg.HiveID),
	}}
	return cfg.ResolveRegistry(s.deps.Logger).Expand(context.Background(), template, resolve.ScopeTemplate, rt)
}

func (s *Server) loadAgentStats(name string) []any {
	statsFile := fmt.Sprintf("/data/agents/%s/stats.json", name)
	data, err := os.ReadFile(statsFile)
	if err == nil {
		var wrapper struct {
			Stats []any `json:"stats"`
		}
		if json.Unmarshal(data, &wrapper) == nil && len(wrapper.Stats) > 0 {
			return wrapper.Stats
		}
		var stats []any
		if json.Unmarshal(data, &stats) == nil && len(stats) > 0 {
			return stats
		}
	}
	return defaultStatsConfig(name)
}

func (s *Server) handleAgentConfigGeneral(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateAgentGeneralInput(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := s.deps.Config.Agents[name]
	if v, ok := body["enabled"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.Enabled = b
		}
	}
	if v, ok := body["clearOnKick"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.ClearOnKick = b
		}
	}
	if v, ok := body["displayName"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.DisplayName = sanitizeString(s)
		}
	}
	if v, ok := body["description"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Description = sanitizeString(s)
		}
	}
	if v, ok := body["launchCmd"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.LaunchCmd = sanitizeString(s)
		}
	}
	if v, ok := body["staleTimeout"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.StaleTimeout = int(f)
		}
	}
	if v, ok := body["replicas"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.Replicas = int(f)
		}
	}
	if v, ok := body["restartStrategy"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.RestartStrategy = sanitizeString(s)
		}
	}
	if v, ok := body["cliPinned"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.CLIPinned = b
		}
	}
	// model / cliPinValue: the dialog's Model and CLI-Pin selects. These were
	// historically dropped here (the handler only read a fixed allowlist), so
	// changes made in the config dialog silently never persisted. Track whether
	// they changed so we can apply them live after the config write, the same
	// way the card dropdowns do (SetModelOverride + restart) — otherwise a stale
	// live override would mask the freshly-saved value and it still wouldn't stick.
	modelChanged, backendChanged := false, false
	// Both are operator edits, so they claim ownership of the field — without
	// the marker the next pack apply (every restart) reconciles them away.
	if v, ok := body["model"]; ok {
		if str, ok := v.(string); ok {
			agentCfg.Model = sanitizeString(str)
			agentCfg.ModelOwner = config.FieldOwnerOperator
			modelChanged = true
		}
	}
	if v, ok := body["cliPinValue"]; ok {
		if str, ok := v.(string); ok && str != "" {
			backend := sanitizeString(str)
			// Validate at set time against the same list the launcher
			// dispatches on — persisting an unsupported backend produces an
			// agent that is accepted now and fails to launch later.
			if err := s.deps.Config.Governor.ValidateBackend(backend); err != nil {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			agentCfg.Backend = backend
			agentCfg.BackendOwner = config.FieldOwnerOperator
			backendChanged = true
		}
	}
	if v, ok := body["emoji"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Emoji = sanitizeString(s)
		}
	}
	if v, ok := body["color"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Color = sanitizeString(s)
		}
	}
	if v, ok := body["sortOrder"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.SortOrder = int(f)
		}
	}
	if v, ok := body["beadRole"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.BeadRole = sanitizeString(s)
		}
	}
	if v, ok := body["role"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Role = sanitizeString(s)
		}
	}
	if v, ok := body["kickTemplate"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.KickTemplate = sanitizeString(s)
		}
	}
	if v, ok := body["mode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			if s != "" {
				validModes := map[string]bool{"ADVISORY": true, "ISSUES_ONLY": true, "ISSUES_AND_PRS": true, "ISSUES_PRS_MERGE": true, "NO_GITHUB": true}
				if !validModes[s] {
					jsonError(w, "mode must be one of: ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE, NO_GITHUB", http.StatusBadRequest)
					return
				}
			}
			agentCfg.Mode = s
		}
	}
	if v, ok := body["includeRepos"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.IncludeRepos = &b
		}
	}
	if v, ok := body["laneKeywords"]; ok {
		if arr, ok := v.([]interface{}); ok {
			kw := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					kw = append(kw, sanitizeString(s))
				}
			}
			agentCfg.LaneKeywords = kw
		}
	}
	if v, ok := body["detectKeywords"]; ok {
		if arr, ok := v.([]interface{}); ok {
			kw := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					kw = append(kw, sanitizeString(s))
				}
			}
			agentCfg.DetectKeywords = kw
		}
	}
	if v, ok := body["aliases"]; ok {
		if arr, ok := v.([]interface{}); ok {
			a := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					a = append(a, s)
				}
			}
			agentCfg.Aliases = a
		}
	}
	if v, ok := body["sandboxEnabled"]; ok {
		if b, ok := v.(bool); ok {
			if agentCfg.Sandbox == nil {
				agentCfg.Sandbox = &config.AgentSandboxOverride{}
			}
			bb := b
			agentCfg.Sandbox.Enabled = &bb
		}
	}
	if v, ok := body["cavemanMode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			// Same gate as config.Validate, so the write path cannot persist a
			// value that would fail the next config load.
			if !config.ValidateCavemanMode(s) {
				jsonError(w, "caveman_mode must be one of: lite, full, ultra, wenyan (or empty to disable)", http.StatusBadRequest)
				return
			}
			agentCfg.CavemanMode = s
		}
	}
	if v, ok := body["explainMode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			// Same gate as config.Validate, so the write path cannot persist a
			// value that would fail the next config load.
			if !config.ValidateExplainMode(s) {
				jsonError(w, "explain_mode must be one of: off, brief, full (or empty to inherit the hive default)", http.StatusBadRequest)
				return
			}
			agentCfg.ExplainMode = s
		}
	}
	// promptSource: a nested {owner,repo,path,ref} object (or explicit null to
	// clear). When set, validate against the seed-only allowlist and re-bake so
	// the agent has a fresh last-known-good prompt on disk.
	if v, ok := body["promptSource"]; ok {
		if v == nil {
			agentCfg.PromptSource = nil
		} else if m, ok := v.(map[string]interface{}); ok {
			ps := &config.PromptSourceConfig{
				Type:  "github",
				Owner: sanitizeString(stringField(m, "owner")),
				Repo:  sanitizeString(stringField(m, "repo")),
				Path:  sanitizeString(stringField(m, "path")),
				Ref:   sanitizeString(stringField(m, "ref")),
			}
			if !ps.IsSet() {
				// Partially filled → treat as cleared rather than erroring, so an
				// operator can blank the fields to disable the source.
				agentCfg.PromptSource = nil
			} else {
				agentCfg.PromptSource = ps
				if err := s.bakePromptSource(r.Context(), name, &agentCfg); err != nil {
					jsonError(w, "prompt source: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
		}
	}
	// definitionSource: a nested {owner,repo,path,ref,url} object (or explicit
	// null to clear) keeping the whole agent linked to a repo. When set, validate
	// against the seed-only allowlist so a non-allowlisted repo is rejected here
	// rather than silently ignored at reload. The actual re-apply happens on the
	// reload path (resolveDefinitionSources); this handler only persists the pointer.
	if v, ok := body["definitionSource"]; ok {
		if v == nil {
			agentCfg.DefinitionSource = nil
		} else if m, ok := v.(map[string]interface{}); ok {
			ds := &config.DefinitionSourceConfig{
				Type:  "github",
				Owner: sanitizeString(stringField(m, "owner")),
				Repo:  sanitizeString(stringField(m, "repo")),
				Path:  sanitizeString(stringField(m, "path")),
				Ref:   sanitizeString(stringField(m, "ref")),
				URL:   sanitizeString(stringField(m, "url")),
			}
			if !ds.IsSet() {
				agentCfg.DefinitionSource = nil
			} else if !s.deps.Config.GitHubDefinitionAllowed(ds.Slug()) {
				jsonError(w, fmt.Sprintf("definition source: repo %q is not on the GitHub definition allowlist", ds.Slug()), http.StatusBadRequest)
				return
			} else {
				agentCfg.DefinitionSource = ds
			}
		}
	}

	prevAgents := make(map[string]config.AgentConfig, len(s.deps.Config.Agents))
	for k, v := range s.deps.Config.Agents {
		prevAgents[k] = v
	}
	s.deps.Config.Agents[name] = agentCfg
	if err := s.deps.Config.ExpandAgentReplicas(); err != nil {
		s.deps.Config.Agents = prevAgents
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	agentCfg = s.deps.Config.Agents[name]
	addedAgents := s.deps.AgentMgr.ReconcileAgents(s.deps.Config.EnabledAgents())
	for _, added := range addedAgents {
		if ac, ok := s.deps.Config.Agents[added]; ok && !ac.OnDemand {
			if err := s.deps.AgentMgr.Start(s.deps.Ctx, added); err != nil {
				s.logger.Warn("failed to start reconciled agent", "agent", added, "error", err)
			}
		}
	}
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateAgents(s.deps.Config.EnabledAgents())
	}

	// Sync the updated config into the agent process so that status builders
	// (which read from AgentProcess.Config, not the global config map) reflect
	// changes like display_name immediately.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent update", "agent", name, "error", err)
	}

	s.deps.AgentMgr.SyncModeFiles(s.deps.AgentMgr.GetACMMLevel())
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after update", "agent", name, "error", err)
		}
	}
	// Apply model/backend live so the change takes effect immediately and isn't
	// masked by a pre-existing override from a card dropdown. SetModelOverride
	// with the saved value (or "" to clear back to the launch-command default)
	// keeps the dialog and the card in sync; a single restart applies both.
	if modelChanged {
		if err := s.deps.AgentMgr.SetModelOverride(name, agentCfg.Model); err != nil {
			s.logger.Warn("failed to apply model from config dialog", "agent", name, "error", err)
		}
	}
	if backendChanged {
		if err := s.deps.AgentMgr.SetBackendOverride(name, agentCfg.Backend); err != nil {
			s.logger.Warn("failed to apply backend from config dialog", "agent", name, "error", err)
		}
	}
	if modelChanged || backendChanged {
		if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
			s.logger.Warn("restart after config-dialog model/backend change failed", "agent", name, "error", err)
		}
	}

	s.auditFromRequest(r, "config_agent_general", auditDetail("section", "general"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigCadences(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]json.RawMessage
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	for modeName, raw := range body {
		mode, ok := s.deps.Config.Governor.Modes[modeName]
		if !ok {
			continue
		}
		if mode.Cadences == nil {
			mode.Cadences = make(map[string]config.Cadence)
		}
		var seconds int64
		var cadence config.Cadence
		if err := json.Unmarshal(raw, &seconds); err == nil {
			if seconds <= 0 {
				cadence = "pause"
			} else {
				cadence = config.Cadence(formatCadenceDuration(seconds))
			}
		} else if err := json.Unmarshal(raw, &cadence); err != nil {
			jsonError(w, "invalid cadence for "+modeName+": must be seconds, interval string, or schedule object", http.StatusBadRequest)
			return
		}
		if err := cadence.Validate(); err != nil {
			jsonError(w, "invalid cadence for "+modeName+": "+err.Error(), http.StatusBadRequest)
			return
		}
		if cadence.Mode() == config.CadenceModeInterval && cadence.IsPaused() {
			mode.Cadences[name] = "pause"
		} else {
			mode.Cadences[name] = cadence
		}
		s.deps.Config.Governor.Modes[modeName] = mode
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after cadence update", "agent", name, "error", err)
	}
	s.auditFromRequest(r, "config_agent_cadences", auditDetail("section", "cadences"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigModels(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	var body struct {
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	// Operator edits claim ownership so the pack apply that runs on every
	// restart cannot reconcile the choice back to the pack default.
	if body.Backend != "" {
		backend := sanitizeString(body.Backend)
		// Refuse an unsupported backend HERE, at set time, with a message that
		// names what is valid. Without this the value is persisted happily and
		// the failure surfaces hours later as "unknown backend: <x>" on the
		// kick path, with the agent silently never launching.
		if err := s.deps.Config.Governor.ValidateBackend(backend); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		agentCfg.Backend = backend
		agentCfg.BackendOwner = config.FieldOwnerOperator
	}
	if body.Model != "" {
		agentCfg.Model = sanitizeString(body.Model)
		agentCfg.ModelOwner = config.FieldOwnerOperator
	}
	s.deps.Config.Agents[name] = agentCfg

	// Sync updated backend/model into the agent process so status builders
	// reflect the change without requiring a restart.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after model update", "agent", name, "error", err)
	}
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after model update", "agent", name, "error", err)
		}
	}
	s.auditFromRequest(r, "config_agent_models", auditDetail("backend", agentCfg.Backend, "model", agentCfg.Model), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigPipeline(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]bool
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.pipelineMu.Lock()
	s.agentPipelines[name] = body
	s.pipelineMu.Unlock()

	s.auditFromRequest(r, "config_agent_pipeline", auditDetail("section", "pipeline"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigHooks(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string][]any
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.hooksMu.Lock()
	s.agentHooks[name] = body
	s.hooksMu.Unlock()

	s.auditFromRequest(r, "config_agent_hooks", auditDetail("section", "hooks"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigRestrictions(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Agent []restriction `json:"agent"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	restFile := fmt.Sprintf("/data/agents/%s/restrictions.conf", name)
	_ = os.MkdirAll(fmt.Sprintf("/data/agents/%s", name), 0o755)

	var lines []string
	for _, r := range body.Agent {
		pattern := strings.ReplaceAll(r.Pattern, "\n", "")
		pattern = strings.ReplaceAll(pattern, "\r", "")
		reason := strings.ReplaceAll(r.Reason, "\n", "")
		reason = strings.ReplaceAll(reason, "\r", "")
		line := pattern
		if reason != "" {
			line += "|" + reason
		}
		lines = append(lines, line)
	}
	tmpRest := restFile + ".tmp"
	if err := os.WriteFile(tmpRest, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		s.logger.Warn("rest file write failed", "error", err)
	} else if err := os.Rename(tmpRest, restFile); err != nil {
		s.logger.Warn("rest file rename failed", "error", err)
	}

	s.auditFromRequest(r, "config_agent_restrictions", auditDetail("section", "restrictions"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigStats(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Stats []any `json:"stats"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	statsFile := fmt.Sprintf("/data/agents/%s/stats.json", name)
	_ = os.MkdirAll(fmt.Sprintf("/data/agents/%s", name), 0o755)

	data, err := json.Marshal(body)
	if err == nil {
		tmpStats := statsFile + ".tmp"
		if err := os.WriteFile(tmpStats, data, 0o644); err != nil {
			s.logger.Warn("stats file write failed", "error", err)
		} else if err := os.Rename(tmpStats, statsFile); err != nil {
			s.logger.Warn("stats file rename failed", "error", err)
		}
	}

	s.auditFromRequest(r, "config_agent_stats", auditDetail("section", "stats"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigChannels(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Channels []config.ChannelConfig `json:"channels"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg.Channels = body.Channels
	s.deps.Config.Agents[name] = agentCfg
	s.auditFromRequest(r, "config_agent_channels", auditDetail("section", "channels"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigTools(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body config.ToolsConfig
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg.Tools = &body
	s.deps.Config.Agents[name] = agentCfg
	s.auditFromRequest(r, "config_agent_tools", auditDetail("section", "tools"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigConnections(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Connections []config.ConnectionConfig `json:"connections"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg.Connections = body.Connections
	s.deps.Config.Agents[name] = agentCfg
	s.auditFromRequest(r, "config_agent_connections", auditDetail("section", "connections"), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

const maskedSecret = "***"

func maskConnectionAuth(conns []config.ConnectionConfig) []config.ConnectionConfig {
	if len(conns) == 0 {
		return conns
	}
	result := make([]config.ConnectionConfig, len(conns))
	copy(result, conns)
	for i, c := range result {
		if c.Auth != nil {
			masked := *c.Auth
			if masked.EnvVar != "" {
				masked.EnvVar = masked.EnvVar + " (" + maskedSecret + ")"
			}
			result[i].Auth = &masked
		}
	}
	return result
}

func (s *Server) handleAgentPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	rawParam := r.URL.Query().Get("raw")
	var template string
	if rawParam == "1" {
		template = s.loadPromptTemplateRaw(name)
	} else {
		template = s.loadPromptTemplate(name)
	}

	const repoBaseURL = "https://github.com/kubestellar/hive/blob/HEAD/"
	sourceFiles := []map[string]string{}

	templateName := ""
	if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
		templateName = ac.KickTemplate
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	sourceFiles = append(sourceFiles, map[string]string{
		"label": "Kick template",
		"path":  "src/pkg/policies/defaults/" + templateName,
		"url":   repoBaseURL + "src/pkg/policies/defaults/" + templateName,
		"note":  "kick_template: " + templateName,
	})

	jsonResponse(w, map[string]interface{}{
		"agent":       name,
		"prompt":      template,
		"sourceFiles": sourceFiles,
	})
}

// loadPromptTemplateRaw returns the raw template content without variable substitution.
func (s *Server) loadPromptTemplateRaw(name string) string {
	templateName := ""
	if s.deps != nil && s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
			templateName = ac.KickTemplate
		}
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	paths := []string{
		fmt.Sprintf("/data/policies/examples/kubestellar/agents/%s", templateName),
	}
	if s.deps != nil && s.deps.Config != nil {
		policyDir := s.deps.Config.Policies.LocalDir
		if policyDir != "" {
			paths = append(paths,
				fmt.Sprintf("%s/examples/kubestellar/agents/%s", policyDir, templateName),
				fmt.Sprintf("%s/%s%s", policyDir, s.deps.Config.Policies.Path, templateName),
			)
		}
	}
	// Check /data/policies first (user-saved templates)
	userPath := fmt.Sprintf("/data/policies/%s", templateName)
	if data, err := os.ReadFile(userPath); err == nil {
		return string(data)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return string(data)
	}
	return ""
}

const promptTemplateSaveDir = "/data/policies"

func (s *Server) handleAgentPromptSave(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Template string `json:"template"`
		// PromptSource + KeepLinked let the Prompt Template tab import the kick
		// prompt from a GitHub repo. When PromptSource is set:
		//   - KeepLinked=true  → persist prompt_source (live: resolved at kick time)
		//   - KeepLinked=false → FetchOnce + bake into the template file, and CLEAR
		//     any existing prompt_source so the prompt is a plain, unlinked copy.
		// When PromptSource is nil this behaves exactly as before (save inline text).
		PromptSource *struct {
			Owner string `json:"owner"`
			Repo  string `json:"repo"`
			Path  string `json:"path"`
			Ref   string `json:"ref"`
		} `json:"promptSource"`
		KeepLinked bool `json:"keepLinked"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := s.deps.Config.Agents[name]

	// Repo-sourced prompt import (Prompt Template tab).
	if body.PromptSource != nil {
		ps := &config.PromptSourceConfig{
			Type:  "github",
			Owner: sanitizeString(body.PromptSource.Owner),
			Repo:  sanitizeString(body.PromptSource.Repo),
			Path:  sanitizeString(body.PromptSource.Path),
			Ref:   sanitizeString(body.PromptSource.Ref),
		}
		if !ps.IsSet() {
			jsonError(w, "prompt source: owner, repo, and path are all required", http.StatusBadRequest)
			return
		}
		agentCfg.PromptSource = ps
		// bakePromptSource validates against the seed-only allowlist (rejecting a
		// non-allowlisted repo server-side), fetches once, writes the baked prompt
		// to the template file, and repoints KickTemplate.
		if err := s.bakePromptSource(r.Context(), name, &agentCfg); err != nil {
			jsonError(w, "prompt source: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !body.KeepLinked {
			// Bake-only: the prompt is now a plain copy on disk; drop the live link.
			agentCfg.PromptSource = nil
		}
		s.deps.Config.Agents[name] = agentCfg
		if err := s.saveConfig(); err != nil {
			s.logger.Error("failed to persist config after prompt import", "agent", name, "error", err)
		}
		if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
			if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
				s.logger.Error("failed to persist agent overlay after prompt import", "agent", name, "error", err)
			}
		}
		s.refreshAndPersist()
		s.auditFromRequest(r, "config_agent_prompt", auditDetail("section", "prompt"), name)
		s.logger.Info("prompt imported from repo", "agent", name, "linked", body.KeepLinked)
		okResponse(w, map[string]string{"status": "imported", "agent": name, "kickTemplate": agentCfg.KickTemplate})
		return
	}

	templateFileName := name + ".md"
	if agentCfg.KickTemplate != "" {
		templateFileName = agentCfg.KickTemplate
	}

	if err := os.MkdirAll(promptTemplateSaveDir, 0o755); err != nil {
		jsonError(w, "failed to create policies directory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	savePath := filepath.Join(promptTemplateSaveDir, templateFileName)
	if err := os.WriteFile(savePath, []byte(body.Template), 0o644); err != nil {
		jsonError(w, "failed to save template: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Point the agent's KickTemplate to the saved file
	agentCfg.KickTemplate = templateFileName
	s.deps.Config.Agents[name] = agentCfg

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after template save", "agent", name, "error", err)
	}
	s.refreshAndPersist()

	s.auditFromRequest(r, "config_agent_prompt", auditDetail("section", "prompt"), name)
	s.logger.Info("prompt template saved", "agent", name, "path", savePath)
	okResponse(w, map[string]string{"status": "saved", "path": savePath})
}

const exportAPIVersion = "hive.kubestellar.io/v1"
const exportKind = "AgentDefinition"

func (s *Server) handleAgentExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	rawTemplate := s.loadPromptTemplateRaw(name)

	includeRepos := true
	if agentCfg.IncludeRepos != nil {
		includeRepos = *agentCfg.IncludeRepos
	}

	// Collect cadences from governor modes
	cadences := map[string]string{}
	for modeName, modeCfg := range s.deps.Config.Governor.Modes {
		if c, ok := modeCfg.Cadences[name]; ok {
			cadences[modeName] = c.String()
		}
	}

	yamlContent := s.buildExportYAML(name, agentCfg, cadences, includeRepos, rawTemplate)

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/yaml") || strings.Contains(accept, "application/yaml") {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.yaml", name))
		_, _ = w.Write([]byte(yamlContent))
		return
	}

	jsonResponse(w, map[string]interface{}{
		"name": name,
		"yaml": yamlContent,
	})
}

func (s *Server) buildExportYAML(name string, cfg config.AgentConfig, cadences map[string]string, includeRepos bool, promptTemplate string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("apiVersion: %s\n", exportAPIVersion))
	b.WriteString(fmt.Sprintf("kind: %s\n", exportKind))
	b.WriteString("metadata:\n")
	b.WriteString(fmt.Sprintf("  name: %s\n", name))
	if cfg.DisplayName != "" {
		b.WriteString(fmt.Sprintf("  displayName: %q\n", cfg.DisplayName))
	}
	if cfg.Description != "" {
		b.WriteString(fmt.Sprintf("  description: %q\n", cfg.Description))
	}
	if cfg.Emoji != "" {
		b.WriteString(fmt.Sprintf("  emoji: %q\n", cfg.Emoji))
	}
	if cfg.Color != "" {
		b.WriteString(fmt.Sprintf("  color: %q\n", cfg.Color))
	}
	hasNewFeatures := len(cfg.Channels) > 0 || cfg.Tools != nil || len(cfg.Connections) > 0
	if hasNewFeatures {
		b.WriteString("  specVersion: 1\n")
	}
	b.WriteString("spec:\n")
	b.WriteString(fmt.Sprintf("  backend: %s\n", valueOrDefault(cfg.Backend, "copilot")))
	if cfg.Model != "" {
		b.WriteString(fmt.Sprintf("  model: %s\n", cfg.Model))
	}
	if cfg.Role != "" {
		b.WriteString(fmt.Sprintf("  role: %s\n", cfg.Role))
	}
	if cfg.Mode != "" {
		b.WriteString(fmt.Sprintf("  mode: %s\n", cfg.Mode))
	}
	b.WriteString(fmt.Sprintf("  sortOrder: %d\n", cfg.SortOrder))
	b.WriteString(fmt.Sprintf("  beadRole: %s\n", cfg.GetBeadRole()))
	b.WriteString(fmt.Sprintf("  staleTimeout: %d\n", cfg.StaleTimeout))
	b.WriteString(fmt.Sprintf("  restartStrategy: %s\n", valueOrDefault(cfg.RestartStrategy, "immediate")))
	b.WriteString(fmt.Sprintf("  clearOnKick: %t\n", cfg.ClearOnKick))
	b.WriteString(fmt.Sprintf("  includeRepos: %t\n", includeRepos))

	if len(cfg.LaneKeywords) > 0 {
		b.WriteString("  laneKeywords:\n")
		for _, kw := range cfg.LaneKeywords {
			b.WriteString(fmt.Sprintf("    - %s\n", kw))
		}
	}
	if len(cfg.DetectKeywords) > 0 {
		b.WriteString("  detectKeywords:\n")
		for _, kw := range cfg.DetectKeywords {
			b.WriteString(fmt.Sprintf("    - %s\n", kw))
		}
	}
	if len(cfg.Aliases) > 0 {
		b.WriteString("  aliases:\n")
		for _, a := range cfg.Aliases {
			b.WriteString(fmt.Sprintf("    - %s\n", a))
		}
	}

	if len(cfg.Channels) > 0 {
		b.WriteString("  channels:\n")
		for _, ch := range cfg.Channels {
			b.WriteString(fmt.Sprintf("    - type: %s\n", ch.Type))
			if ch.Enabled != nil {
				b.WriteString(fmt.Sprintf("      enabled: %t\n", *ch.Enabled))
			}
			if len(ch.Events) > 0 {
				b.WriteString("      events:\n")
				for _, e := range ch.Events {
					b.WriteString(fmt.Sprintf("        - %s\n", e))
				}
			}
			if len(ch.Patterns) > 0 {
				b.WriteString("      patterns:\n")
				for _, p := range ch.Patterns {
					b.WriteString(fmt.Sprintf("        - %q\n", p))
				}
			}
			if ch.Schedule != "" {
				b.WriteString(fmt.Sprintf("      schedule: %q\n", ch.Schedule))
			}
			if len(ch.Match) > 0 {
				b.WriteString("      match:\n")
				for k, v := range ch.Match {
					b.WriteString(fmt.Sprintf("        %s: %q\n", k, v))
				}
			}
			if len(ch.Repos) > 0 {
				b.WriteString("      repos:\n")
				for _, r := range ch.Repos {
					b.WriteString(fmt.Sprintf("        - %s\n", r))
				}
			}
		}
	}

	if cfg.Tools != nil {
		b.WriteString("  tools:\n")
		if cfg.Tools.Preset != "" {
			b.WriteString(fmt.Sprintf("    preset: %s\n", cfg.Tools.Preset))
		}
		if len(cfg.Tools.Rules) > 0 {
			b.WriteString("    rules:\n")
			for _, r := range cfg.Tools.Rules {
				b.WriteString(fmt.Sprintf("      - pattern: %q\n", r.Pattern))
				b.WriteString(fmt.Sprintf("        action: %s\n", r.Action))
				if r.Reason != "" {
					b.WriteString(fmt.Sprintf("        reason: %q\n", r.Reason))
				}
			}
		}
	}

	if len(cfg.Connections) > 0 {
		b.WriteString("  connections:\n")
		for _, c := range cfg.Connections {
			b.WriteString(fmt.Sprintf("    - name: %s\n", c.Name))
			b.WriteString(fmt.Sprintf("      type: %s\n", c.Type))
			if c.URI != "" {
				b.WriteString(fmt.Sprintf("      uri: %q\n", c.URI))
			}
			if c.EnvName != "" {
				b.WriteString(fmt.Sprintf("      env_name: %s\n", c.EnvName))
			}
			if c.Auth != nil {
				b.WriteString("      auth:\n")
				b.WriteString(fmt.Sprintf("        type: %s\n", c.Auth.Type))
				if c.Auth.EnvVar != "" {
					b.WriteString(fmt.Sprintf("        env_var: %s\n", c.Auth.EnvVar))
				}
				if c.Auth.File != "" {
					b.WriteString(fmt.Sprintf("        file: %s\n", c.Auth.File))
				}
			}
			if len(c.Options) > 0 {
				b.WriteString("      options:\n")
				for k, v := range c.Options {
					b.WriteString(fmt.Sprintf("        %s: %q\n", k, v))
				}
			}
		}
	}

	if len(cadences) > 0 {
		b.WriteString("  cadences:\n")
		modeOrder := []string{"idle", "quiet", "busy", "surge"}
		for _, mode := range modeOrder {
			if c, ok := cadences[mode]; ok {
				if strings.Contains(c, "\n") {
					b.WriteString(fmt.Sprintf("    %s:\n", mode))
					for _, line := range strings.Split(c, "\n") {
						if strings.TrimSpace(line) == "" {
							continue
						}
						b.WriteString("      " + line + "\n")
					}
				} else {
					b.WriteString(fmt.Sprintf("    %s: %s\n", mode, c))
				}
			}
		}
	}

	if promptTemplate != "" {
		b.WriteString("  promptTemplate: |\n")
		for _, line := range strings.Split(promptTemplate, "\n") {
			b.WriteString("    " + line + "\n")
		}
	}

	return b.String()
}

func valueOrDefault(v, dflt string) string {
	if v != "" {
		return v
	}
	return dflt
}

func (s *Server) handleStatSources(w http.ResponseWriter, r *http.Request) {
	sources := map[string]any{
		"status": map[string]any{
			"label":  "Repo Status",
			"fields": []string{"actionableCount", "openPrCount", "mergeableCount"},
		},
		"health": map[string]any{
			"label": "Health Checks",
			"fields": []string{
				"brew", "helm", "ci", "weekly", "nightly",
				"nightlyCompliance", "nightlyDashboard", "nightlyGhaw",
				"nightlyPlaywright", "nightlyRel", "weeklyRel",
				"deploy_vllm_d", "deploy_pok_prod",
			},
		},
		"agentMetrics": map[string]any{
			"label": "Agent Metrics",
			"fields": []string{
				"stars", "forks", "contributors", "adopters", "acmm",
				"outreachOpen", "outreachMerged", "coverage", "prs", "closed",
			},
		},
		"tokens": map[string]any{
			"label":  "Token Usage",
			"fields": []string{"input", "output", "cacheRead", "cacheCreate", "sessions", "messages"},
		},
	}
	styles := []string{"number", "dot", "pct", "pct-bar", "spark"}
	jsonResponse(w, map[string]any{"sources": sources, "styles": styles})
}

// --- Governor config endpoints ---

func (s *Server) handleGovernorConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config

	// Build agents list
	agents := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agents = append(agents, name)
	}

	// Extract thresholds from modes (exclude idle which is always 0)
	thresholds := map[string]int{}
	for modeName, mode := range cfg.Governor.Modes {
		if modeName != "idle" {
			thresholds[modeName] = mode.Threshold
		}
	}

	// The raw map above is what the operator SET (0 = unset). The governor
	// ladders on the resolved values, which for an unset mode are the defaults
	// scaled by repo count (#3498). Send both, so the settings panel can show
	// the numbers actually in force next to the ones being edited instead of
	// falling back to its own copy of the unscaled defaults.
	repoCount := cfg.Project.RepoCount()
	effectiveThresholds := map[string]int{
		"quiet": cfg.Governor.EffectiveThreshold("quiet", repoCount),
		"busy":  cfg.Governor.EffectiveThreshold("busy", repoCount),
		"surge": cfg.Governor.EffectiveThreshold("surge", repoCount),
	}

	// Build full org/repo paths
	org := cfg.Project.Org
	repos := make([]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if strings.Contains(repo, "/") {
			repos = append(repos, repo)
		} else {
			repos = append(repos, org+"/"+repo)
		}
	}

	// Build notifications — mask sensitive values like the old hive does
	notifications := map[string]interface{}{
		"ntfyServer":     "",
		"ntfyTopic":      "",
		"discordWebhook": "",
		"hasNtfy":        false,
		"hasDiscord":     false,
	}
	if cfg.Notifications.Ntfy != nil {
		notifications["ntfyServer"] = cfg.Notifications.Ntfy.Server
		notifications["ntfyTopic"] = cfg.Notifications.Ntfy.Topic
		notifications["hasNtfy"] = cfg.Notifications.Ntfy.Server != ""
	}
	if cfg.Notifications.Discord != nil {
		notifications["discordWebhook"] = maskSecret(cfg.Notifications.Discord.Webhook)
		notifications["hasDiscord"] = cfg.Notifications.Discord.Webhook != ""
	}

	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo != "" && org != "" && !strings.Contains(primaryRepo, "/") {
		primaryRepo = org + "/" + primaryRepo
	}

	jsonResponse(w, map[string]interface{}{
		"agents":              agents,
		"thresholds":          thresholds,
		"effectiveThresholds": effectiveThresholds,
		"thresholdScaling":    cfg.Governor.ThresholdScalingMode(),
		"repoCount":           repoCount,
		"labels":              cfg.Governor.Labels.Exempt,
		"holdLabels":          github.HoldLabels,
		// requireLabels is the OPPOSITE polarity from "labels" (exempt):
		// project.issue_filter.require_labels — when non-empty, agents may
		// ONLY initiate work on issues carrying at least one of them. Edited
		// on the same Labels tab so operators have one place for label policy.
		"requireLabels": cfg.Project.IssueFilter.RequireLabels,
		"repos":         repos,
		"primaryRepo":   primaryRepo,
		"budget": map[string]interface{}{
			"totalTokens": cfg.Governor.Budget.TotalTokens,
			"periodDays":  cfg.Governor.Budget.PeriodDays,
			"criticalPct": cfg.Governor.Budget.CriticalPct,
		},
		"notifications": notifications,
		"health": map[string]interface{}{
			"healthcheckInterval": cfg.Governor.Health.HealthcheckInterval,
			"restartCooldown":     cfg.Governor.Health.RestartCooldown,
			"modelLock":           cfg.Governor.Health.ModelLock,
			"watchdog":            watchdogConfigPayload(cfg),
		},
		"sensing": map[string]interface{}{
			"ghRatePatterns":     cfg.Governor.Sensing.GHRatePatterns,
			"cliExcludePatterns": cfg.Governor.Sensing.CLIExcludePatterns,
			"loginPatterns":      cfg.Governor.Sensing.LoginPatterns,
			"ttlSeconds":         cfg.Governor.Sensing.TTLSeconds,
			"pullbackSeconds":    cfg.Governor.Sensing.PullbackSeconds,
		},
		"logging": map[string]interface{}{
			"dir":        cfg.Governor.Logging.Dir,
			"maxSizeMB":  cfg.Governor.Logging.MaxSizeMB,
			"maxAgeDays": cfg.Governor.Logging.MaxAgeDays,
			"maxBackups": cfg.Governor.Logging.MaxBackups,
			"compress":   cfg.Governor.Logging.Compress,
			"level":      cfg.Governor.Logging.Level,
		},
		"litellm":               litellmSectionResponse(&cfg.Governor.LiteLLM),
		"trajectory":            trajectorySectionResponse(&cfg.Governor),
		"classifier":            classifierSectionResponse(),
		"features":              featuresSectionResponse(cfg),
		"review":                cfg.Review,
		"auto_merge":            autoMergeSectionResponse(cfg),
		"convergence":           s.convergenceSectionResponse(cfg),
		"advisory":              advisorySectionResponse(cfg),
		"project_observability": s.projectObservabilityResponse(cfg),
		"replan":                replanSectionResponse(cfg),
		"work_source":           workSourceSectionResponse(cfg),
		"security":              securitySectionResponse(cfg),
		"attribution": map[string]interface{}{
			// Effective value (default ON when unset) — the UI renders the
			// switch from this, so an untouched hive shows it on.
			"attributionTrailer": cfg.Governor.AttributionTrailerEnabled(),
		},
		"general_advanced": generalAdvancedSectionResponse(cfg),
		"hub": map[string]interface{}{
			"enabled": cfg.Hub.Enabled,
			// namespace is read-only, runtime-derived display info (never
			// persisted to hive.yaml, never accepted back on save) — the
			// Kubernetes namespace this pod is actually running in. See
			// podNamespace's doc comment for the POD_NAMESPACE/NAMESPACE/
			// service-account-file fallback chain. Omitted (empty string)
			// outside a cluster, which the Hub tab renders by skipping the
			// line rather than showing a blank/"undefined" value.
			"namespace":                          podNamespace(),
			"url":                                cfg.Hub.URL,
			"dashboard_url":                      cfg.Hub.DashboardURL,
			"snapshot_url":                       cfg.Hub.SnapshotURL,
			"is_public":                          cfg.Hub.IsPublic,
			"auto_snapshot":                      cfg.Hub.AutoSnapshot,
			"snapshot_frame_ancestors":           cfg.Dashboard.SnapshotFrameAncestors,
			"auto_upgrade":                       cfg.Hub.AutoUpgrade,
			"snapshot_interval_min":              cfg.Hub.SnapshotIntervalMin,
			"contribute_suspended":               cfg.Hub.ContributeSuspended,
			"contribute_titles_mode":             cfg.Hub.ContributeTitlesMode,
			"contribute_authors_mode":            cfg.Hub.ContributeAuthorsMode,
			"contribute_labels_mode":             cfg.Hub.ContributeLabelsMode,
			"contribute_allow_labels":            cfg.Hub.ContributeAllowLabels,
			"contribute_deny_labels":             cfg.Hub.ContributeDenyLabels,
			"contribute_deny_titles":             cfg.Hub.ContributeDenyTitles,
			"contribute_deny_authors":            cfg.Hub.ContributeDenyAuthors,
			"contribute_allow_models":            cfg.Hub.ContributeAllowModels,
			"contribute_reject_unknown_models":   cfg.Hub.ContributeRejectUnknownModels,
			"contribute_skip_assigned_to_others": cfg.Hub.ContributeSkipAssignedToOthers,
			// Cooldown toggle + period. contribute_cooldown_enabled is the RESOLVED
			// on/off (nil pointer -> true) so both UI surfaces render a concrete
			// switch state; contribute_cooldown_hours is the EFFECTIVE period (the
			// 168h default surfaces when unset) so the number input shows the value
			// actually in force.
			"contribute_cooldown_enabled":  cfg.Hub.IsContributeCooldownEnabled(),
			"contribute_cooldown_hours":    cfg.Hub.ContributeCooldownHoursOrDefault(),
			"contribute_delegatable_roles": normalizeContributeDelegatableRoles(cfg.Hub.ContributeDelegatableRoles),
			"disabled_repos":               cfg.Hub.DisabledRepos,
			"disabled_tiers":               cfg.Hub.DisabledTiers,
			"tier_limits":                  cfg.Hub.TierLimits,
			// available_repos is the READ-ONLY list of repo full-names the hive knows
			// about (from the live status snapshot), so the Management tab can render
			// a per-repo enable toggle mirror of the Governor Hub "Repos for Contribute"
			// list. A repo is ENABLED unless it appears in disabled_repos.
			"available_repos": s.contributeAvailableRepos(),
		},
	})
}

// contributeAvailableRepos returns the sorted, de-duplicated set of repo full-names
// the hive currently knows about (from the status snapshot). Read-only; used to
// render the "Repos for Contribute" enable toggles in the Management tab mirror.
func (s *Server) contributeAvailableRepos() []string {
	seen := map[string]struct{}{}
	var out []string
	s.statusMu.RLock()
	if s.status != nil {
		for _, repo := range s.status.Repos {
			name := repo.Full
			if name == "" {
				name = repo.Name
			}
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	s.statusMu.RUnlock()
	sort.Strings(out)
	return out
}

func (s *Server) handleGovernorSensing(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		EvalIntervalS      int      `json:"eval_interval_s"`
		GHRatePatterns     []string `json:"ghRatePatterns"`
		CLIExcludePatterns []string `json:"cliExcludePatterns"`
		LoginPatterns      []string `json:"loginPatterns"`
		TTLSeconds         int      `json:"ttlSeconds"`
		PullbackSeconds    int      `json:"pullbackSeconds"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	const minEvalIntervalS = 10    // 10 seconds minimum
	const maxEvalIntervalS = 86400 // 24 hours
	if body.EvalIntervalS != 0 {
		if body.EvalIntervalS < minEvalIntervalS || body.EvalIntervalS > maxEvalIntervalS {
			jsonError(w, fmt.Sprintf("eval_interval_s must be between %d and %d", minEvalIntervalS, maxEvalIntervalS), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.EvalIntervalS = body.EvalIntervalS
	}
	if body.GHRatePatterns != nil {
		for _, p := range body.GHRatePatterns {
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid ghRatePattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
		}
		s.deps.Config.Governor.Sensing.GHRatePatterns = body.GHRatePatterns
	}
	if body.CLIExcludePatterns != nil {
		for _, p := range body.CLIExcludePatterns {
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid cliExcludePattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
		}
		s.deps.Config.Governor.Sensing.CLIExcludePatterns = body.CLIExcludePatterns
	}
	if body.LoginPatterns != nil {
		var filtered []string
		for _, p := range body.LoginPatterns {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid login pattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
			filtered = append(filtered, p)
		}
		s.deps.Config.Governor.Sensing.LoginPatterns = filtered
	}
	const maxTTLSeconds = 86400 // 24 hours
	if body.TTLSeconds != 0 {
		if body.TTLSeconds < 1 || body.TTLSeconds > maxTTLSeconds {
			jsonError(w, fmt.Sprintf("ttlSeconds must be between 1 and %d", maxTTLSeconds), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.Sensing.TTLSeconds = body.TTLSeconds
	}
	const maxPullbackSeconds = 86400 // 24 hours
	if body.PullbackSeconds != 0 {
		if body.PullbackSeconds < 1 || body.PullbackSeconds > maxPullbackSeconds {
			jsonError(w, fmt.Sprintf("pullbackSeconds must be between 1 and %d", maxPullbackSeconds), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.Sensing.PullbackSeconds = body.PullbackSeconds
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config", "error", err)
	}
	s.auditFromRequest(r, "config_governor_sensing", auditDetail("section", "sensing"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorThresholds(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body map[string]int
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateGovernorThresholds(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	for modeName, threshold := range body {
		if mode, ok := s.deps.Config.Governor.Modes[modeName]; ok {
			mode.Threshold = threshold
			s.deps.Config.Governor.Modes[modeName] = mode
		}
	}

	// #4037: the operator just typed these numbers, so they stop being
	// pack-seeded bases and become absolutes that EffectiveThreshold returns
	// verbatim — the #3498 guarantee that a hand-tuned `surge: 300` is never
	// multiplied by the repo count. Cleared for the WHOLE set, deliberately:
	// leaving the untouched modes scaling while this one does not can invert
	// the mode ladder (a scaled busy above an unscaled surge). Editing any
	// threshold means the operator owns all of them.
	s.deps.Config.Governor.ThresholdsSource = ""

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after threshold update", "error", err)
	}

	// Trigger immediate governor re-evaluation so mode change is visible
	// without waiting for the next eval ticker (up to 5 minutes).
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}

	s.auditFromRequest(r, "config_governor_thresholds", auditDetail("section", "thresholds"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// handleGovernorThresholdScalingGet returns the configured threshold-scaling
// curve (with its default applied) so the Thresholds tab can prefill its
// select without loading the whole governor config payload. OWNER-ONLY,
// matching the write side and the rest of the governor-config surface.
func (s *Server) handleGovernorThresholdScalingGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]string{
		"threshold_scaling": s.deps.Config.Governor.ThresholdScalingMode(),
	})
}

// handleGovernorThresholdScaling sets how the DEFAULT mode thresholds scale
// with the hive's repo count (#3498).
//
// It is a separate route from handleGovernorThresholds because that one's body
// is a flat map[string]int of mode name to threshold; a string curve does not
// fit it, and widening it to map[string]any would put a parse branch on the
// path every threshold drag already takes.
func (s *Server) handleGovernorThresholdScaling(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		ThresholdScaling string `json:"thresholdScaling"`
		// Snake-case alias so callers can send the same key the GET returns
		// and the YAML config uses.
		ThresholdScalingSnake string `json:"threshold_scaling"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	raw := body.ThresholdScaling
	if raw == "" {
		raw = body.ThresholdScalingSnake
	}
	scaling := sanitizeString(raw)
	// Same gate as config.validate, so the write path cannot persist a value
	// that fails the next config load.
	if !config.ValidateThresholdScaling(scaling) {
		jsonError(w, "thresholdScaling must be one of: linear, sqrt, none (or empty for the default)", http.StatusBadRequest)
		return
	}
	s.deps.Config.Governor.ThresholdScaling = scaling

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after threshold scaling update", "error", err)
	}

	// Changing the curve moves every scaled threshold, so re-evaluate now
	// rather than leaving the hive on the old ladder until the next eval tick —
	// matching what a threshold drag already does.
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}

	s.auditFromRequest(r, "config_governor_threshold_scaling", auditDetail("section", "thresholdScaling"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorLabels(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// Both label polarities save through this one endpoint (the Labels tab
	// edits both). POINTER-typed so an absent key means "unchanged" — a save
	// that only touched the require list must not wipe the exempt list to
	// empty, and vice versa.
	var body struct {
		Labels        *[]string `json:"labels"`
		RequireLabels *[]string `json:"require_labels"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Labels != nil {
		if err := validateGovernorLabels(*body.Labels); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if body.RequireLabels != nil {
		if err := validateGovernorLabels(*body.RequireLabels); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if body.Labels != nil {
		filtered := make([]string, 0, len(*body.Labels))
		for _, l := range *body.Labels {
			isPermanent := false
			for _, h := range github.HoldLabels {
				if l == h {
					isPermanent = true
					break
				}
			}
			for _, p := range github.PermanentExemptLabels {
				if l == p {
					isPermanent = true
					break
				}
			}
			if !isPermanent {
				filtered = append(filtered, l)
			}
		}
		s.deps.Config.Governor.Labels.Exempt = filtered
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetExemptLabels(filtered)
		}
	}
	if body.RequireLabels != nil {
		// The require gate (project.issue_filter): empty list = filter off.
		// Takes effect on the next enumeration via the scan client.
		s.deps.Config.Project.IssueFilter.RequireLabels = *body.RequireLabels
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetIssueFilter(s.deps.Config.Project.IssueFilter)
		}
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after label update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_labels", auditDetail("section", "labels"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorBudget(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// Pointer fields distinguish "field absent from the JSON" (nil) from
	// "field explicitly set to 0". The dashboard sends only the inputs the
	// user actually touched, so editing Total Tokens alone POSTs
	// {"totalTokens":N} with no periodDays/criticalPct. With plain ints
	// those absent fields decoded to 0 and were rejected by validation as
	// out-of-range. Nil now means "leave the stored value alone", while an
	// explicit 0 is still honored (totalTokens: 0 disables budget tracking).
	var body struct {
		TotalTokens *int64 `json:"totalTokens"`
		PeriodDays  *int   `json:"periodDays"`
		CriticalPct *int   `json:"criticalPct"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// Validate against the effective post-update values: supplied fields use
	// the incoming value, omitted fields keep what is already stored. This
	// keeps a partial update from being judged against a phantom zero.
	current := s.deps.Config.Governor.Budget
	totalTokens, periodDays, criticalPct := current.TotalTokens, current.PeriodDays, current.CriticalPct
	if body.TotalTokens != nil {
		totalTokens = *body.TotalTokens
	}
	if body.PeriodDays != nil {
		periodDays = *body.PeriodDays
	}
	if body.CriticalPct != nil {
		criticalPct = *body.CriticalPct
	}

	if err := validateGovernorBudget(totalTokens, periodDays, criticalPct); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.TotalTokens != nil {
		s.deps.Config.Governor.Budget.TotalTokens = totalTokens
		s.deps.Governor.SetBudgetLimit(totalTokens)
	}
	if body.PeriodDays != nil {
		s.deps.Config.Governor.Budget.PeriodDays = periodDays
	}
	if body.CriticalPct != nil {
		s.deps.Config.Governor.Budget.CriticalPct = criticalPct
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after budget update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_budget", auditDetail("section", "budget"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// handleGovernorBudgetReset opens a fresh budget window on demand. The
// operator's recovery path when the window's spend has (or a mis-sized limit
// has) closed the kick gate: fix the limit via PUT /budget, then reset the
// window here so kicks resume immediately — budgeting stays ON, unlike the
// "ignore budget" bypass. Spend re-anchors at zero and the once-per-window
// alerts rearm.
func (s *Server) handleGovernorBudgetReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	s.deps.Governor.ResetBudgetWindow()
	s.auditFromRequest(r, "governor_budget_window_reset", auditDetail("section", "budget"), "")
	floor := s.refreshAndPersistSeq()
	// minStatusSeq: see handleResetRestarts — lets the dashboard discard
	// pre-reset status snapshots so the budget bar can't revert (#4348).
	jsonResponse(w, map[string]any{"ok": true, "status": "reset", "minStatusSeq": floor})
}

func (s *Server) handleGovernorNotifications(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		NtfyServer     string `json:"ntfyServer"`
		NtfyTopic      string `json:"ntfyTopic"`
		DiscordWebhook string `json:"discordWebhook"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateNotificationURL(body.NtfyServer, "ntfyServer"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateNotificationURL(body.DiscordWebhook, "discordWebhook"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	isMasked := func(v string) bool { return strings.HasPrefix(v, "•") }
	if (body.NtfyServer != "" && !isMasked(body.NtfyServer)) || (body.NtfyTopic != "" && !isMasked(body.NtfyTopic)) {
		if s.deps.Config.Notifications.Ntfy == nil {
			s.deps.Config.Notifications.Ntfy = &config.NtfyConfig{}
		}
		if body.NtfyServer != "" && !isMasked(body.NtfyServer) {
			s.deps.Config.Notifications.Ntfy.Server = body.NtfyServer
		}
		if body.NtfyTopic != "" && !isMasked(body.NtfyTopic) {
			s.deps.Config.Notifications.Ntfy.Topic = body.NtfyTopic
		}
	}
	if body.DiscordWebhook != "" && !isMasked(body.DiscordWebhook) {
		if s.deps.Config.Notifications.Discord == nil {
			s.deps.Config.Notifications.Discord = &config.DiscordConfig{}
		}
		s.deps.Config.Notifications.Discord.Webhook = body.DiscordWebhook
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after notification update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_notifications", auditDetail("section", "notifications"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorHealth(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		HealthcheckInterval int   `json:"healthcheckInterval"`
		RestartCooldown     int   `json:"restartCooldown"`
		ModelLock           *bool `json:"modelLock"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorHealth(body.HealthcheckInterval, body.RestartCooldown); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.HealthcheckInterval > 0 {
		s.deps.Config.Governor.Health.HealthcheckInterval = body.HealthcheckInterval
	}
	if body.RestartCooldown > 0 {
		s.deps.Config.Governor.Health.RestartCooldown = body.RestartCooldown
	}
	if body.ModelLock != nil {
		s.deps.Config.Governor.Health.ModelLock = *body.ModelLock
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after health update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_health", auditDetail("section", "health"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorLogging(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		MaxSizeMB  int    `json:"maxSizeMB"`
		MaxAgeDays int    `json:"maxAgeDays"`
		MaxBackups int    `json:"maxBackups"`
		Compress   *bool  `json:"compress"`
		Level      string `json:"level"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorLogging(s.deps.Config.Governor.Logging.Dir, body.MaxSizeMB, body.MaxAgeDays); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.MaxSizeMB > 0 {
		s.deps.Config.Governor.Logging.MaxSizeMB = body.MaxSizeMB
	}
	if body.MaxAgeDays > 0 {
		s.deps.Config.Governor.Logging.MaxAgeDays = body.MaxAgeDays
	}
	if body.MaxBackups > 0 {
		s.deps.Config.Governor.Logging.MaxBackups = body.MaxBackups
	}
	if body.Compress != nil {
		s.deps.Config.Governor.Logging.Compress = *body.Compress
	}
	if body.Level != "" {
		switch body.Level {
		case "debug", "info", "warn", "error":
			s.deps.Config.Governor.Logging.Level = body.Level
		default:
			jsonError(w, "level must be one of: debug, info, warn, error", http.StatusBadRequest)
			return
		}
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after logging update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_logging", auditDetail("section", "logging"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// handleGovernorAttribution updates the hive-wide attribution-trailer toggle.
// One boolean, applied to ALL agents (no per-agent granularity): it gates ONLY
// the visible "— hive: …" trailer appended to hive-created PRs and issues.
// The audit-log entry for every such creation is written unconditionally, so
// turning the trailer off never loses the invocation record.
func (s *Server) handleGovernorAttribution(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		AttributionTrailer *bool `json:"attributionTrailer"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.AttributionTrailer != nil {
		s.deps.Config.Governor.AttributionTrailer = body.AttributionTrailer
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after attribution update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_attribution", auditDetail("section", "attribution"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func normalizeContributeDelegatableRoles(roles []string) []string {
	seen := map[string]bool{}
	for _, role := range []string{"scanner", "quality", "outreach"} {
		seen[role] = true
	}
	for _, role := range roles {
		role = normalizeAgentRole(role)
		if role == "" || role == "supervisor" {
			continue
		}
		seen[role] = true
	}
	out := make([]string, 0, len(seen))
	for role := range seen {
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleGovernorHub(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled                        *bool                      `json:"enabled"`
		URL                            string                     `json:"url"`
		DashboardURL                   string                     `json:"dashboard_url"`
		SnapshotURL                    string                     `json:"snapshot_url"`
		IsPublic                       *bool                      `json:"is_public"`
		AutoSnapshot                   *bool                      `json:"auto_snapshot"`
		SnapshotFrameAncestors         []string                   `json:"snapshot_frame_ancestors"`
		AutoUpgrade                    *bool                      `json:"auto_upgrade"`
		ContributeSuspended            *bool                      `json:"contribute_suspended"`
		ContributeTitlesMode           *string                    `json:"contribute_titles_mode"`
		ContributeAuthorsMode          *string                    `json:"contribute_authors_mode"`
		ContributeLabelsMode           *string                    `json:"contribute_labels_mode"`
		ContributeAllowLabels          []string                   `json:"contribute_allow_labels"`
		ContributeDenyLabels           []string                   `json:"contribute_deny_labels"`
		ContributeDenyTitles           []string                   `json:"contribute_deny_titles"`
		ContributeDenyAuthors          []string                   `json:"contribute_deny_authors"`
		ContributeAllowModels          []string                   `json:"contribute_allow_models"`
		ContributeRejectUnknownModels  *bool                      `json:"contribute_reject_unknown_models"`
		ContributeSkipAssignedToOthers *bool                      `json:"contribute_skip_assigned_to_others"`
		ContributeCooldownEnabled      *bool                      `json:"contribute_cooldown_enabled"`
		ContributeCooldownHours        *int                       `json:"contribute_cooldown_hours"`
		ContributeDelegatableRoles     []string                   `json:"contribute_delegatable_roles"`
		DisabledRepos                  []string                   `json:"disabled_repos"`
		DisabledTiers                  []string                   `json:"disabled_tiers"`
		TierLimits                     map[string]config.TierRate `json:"tier_limits"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if body.Enabled != nil {
		cfg.Hub.Enabled = *body.Enabled
	}
	if body.URL != "" {
		cfg.Hub.URL = body.URL
	}
	if body.DashboardURL != "" {
		cfg.Hub.DashboardURL = body.DashboardURL
	}
	cfg.Hub.SnapshotURL = body.SnapshotURL
	if body.IsPublic != nil {
		cfg.Hub.IsPublic = *body.IsPublic
	}
	if body.AutoSnapshot != nil {
		cfg.Hub.AutoSnapshot = *body.AutoSnapshot
	}
	if body.SnapshotFrameAncestors != nil {
		normalized, err := config.ValidateSnapshotFrameAncestors(body.SnapshotFrameAncestors)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		cfg.Dashboard.SnapshotFrameAncestors = normalized
	}
	if body.AutoUpgrade != nil {
		cfg.Hub.AutoUpgrade = *body.AutoUpgrade
	}
	if body.ContributeSuspended != nil {
		cfg.Hub.ContributeSuspended = *body.ContributeSuspended
	}
	if body.ContributeTitlesMode != nil {
		cfg.Hub.ContributeTitlesMode = *body.ContributeTitlesMode
	}
	if body.ContributeAuthorsMode != nil {
		cfg.Hub.ContributeAuthorsMode = *body.ContributeAuthorsMode
	}
	if body.ContributeLabelsMode != nil {
		cfg.Hub.ContributeLabelsMode = *body.ContributeLabelsMode
	}
	if body.ContributeAllowLabels != nil {
		cfg.Hub.ContributeAllowLabels = body.ContributeAllowLabels
	}
	if body.ContributeDenyLabels != nil {
		cfg.Hub.ContributeDenyLabels = body.ContributeDenyLabels
	}
	if body.ContributeDenyTitles != nil {
		cfg.Hub.ContributeDenyTitles = body.ContributeDenyTitles
	}
	if body.ContributeDenyAuthors != nil {
		cfg.Hub.ContributeDenyAuthors = body.ContributeDenyAuthors
	}
	if body.ContributeAllowModels != nil {
		cfg.Hub.ContributeAllowModels = body.ContributeAllowModels
	}
	if body.ContributeRejectUnknownModels != nil {
		cfg.Hub.ContributeRejectUnknownModels = *body.ContributeRejectUnknownModels
	}
	if body.ContributeSkipAssignedToOthers != nil {
		cfg.Hub.ContributeSkipAssignedToOthers = *body.ContributeSkipAssignedToOthers
	}
	// Cooldown toggle: store the client's on/off intent as a non-nil pointer so it
	// is round-tripped exactly (nil would default back to enabled). A client that
	// omits the field leaves the current value untouched.
	if body.ContributeCooldownEnabled != nil {
		v := *body.ContributeCooldownEnabled
		cfg.Hub.ContributeCooldownEnabled = &v
	}
	// Cooldown period (hours): stored as-is; the config resolver
	// (ContributeCooldownHoursOrDefault) clamps to the valid range at read time and
	// applyDefaults clamps any persisted value, so a stray input can never park an
	// issue forever. A client sending 0 means "use the default".
	if body.ContributeCooldownHours != nil {
		cfg.Hub.ContributeCooldownHours = *body.ContributeCooldownHours
	}
	if body.ContributeDelegatableRoles != nil {
		cfg.Hub.ContributeDelegatableRoles = normalizeContributeDelegatableRoles(body.ContributeDelegatableRoles)
	}
	if body.DisabledRepos != nil {
		cfg.Hub.DisabledRepos = body.DisabledRepos
	}
	if body.DisabledTiers != nil {
		cfg.Hub.DisabledTiers = body.DisabledTiers
	}
	if body.TierLimits != nil {
		cfg.Hub.TierLimits = body.TierLimits
	}
	// Normalize any client-supplied filter mode to a valid value (unknown/empty
	// -> deny), so a bad payload can't leave a mode that silently changes
	// enforcement in an unexpected way.
	cfg.Hub.ContributeTitlesMode = config.NormalizeFilterMode(cfg.Hub.ContributeTitlesMode)
	cfg.Hub.ContributeAuthorsMode = config.NormalizeFilterMode(cfg.Hub.ContributeAuthorsMode)
	cfg.Hub.ContributeLabelsMode = config.NormalizeFilterMode(cfg.Hub.ContributeLabelsMode)
	s.auditFromRequest(r, "config_governor_hub", auditDetail("section", "hub"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// apiKeyLikeLength: env var NAMES are short and conventionally use
// underscores, while bearer keys are typically 40+ chars without any.
const apiKeyLikeLength = 40

// looksLikeAPIKeyValue guards the env-name field against users pasting an
// actual API key VALUE into it (which would bake "sk-..." into hive.yaml
// as an env var name and silently resolve to an empty key at runtime).
func looksLikeAPIKeyValue(s string) bool {
	return strings.HasPrefix(s, "sk-") ||
		(len(s) > apiKeyLikeLength && !strings.Contains(s, "_"))
}

const (
	// litellmProbeTimeout bounds the save-time /v1/models check.
	litellmProbeTimeout = 8 * time.Second
	// litellmProbeMaxErrBody caps how much of a gateway error body is
	// surfaced back to the dialog.
	litellmProbeMaxErrBody = 512
)

// litellmKeyMaterialPatterns match key-identifying material that some gateways
// (e.g. LiteLLM) echo back in auth-failure bodies: the SHA-256 key hash, the
// masked "Received API Key = sk-...XXXX" hint, and bare sk- tokens. We keep the
// human-useful "401 auth failed" signal but strip these — the key hash is a
// stable fingerprint of the secret that should not appear in the dashboard or
// logs.
var litellmKeyMaterialPatterns = []*regexp.Regexp{
	// "Key Hash (Token) = <64 hex>" (also matches "Key Hash = <hex>").
	regexp.MustCompile(`(?i)(key hash[^=]*=\s*)[0-9a-f]{16,}`),
	// "Received API Key = sk-...GTNw" (masked hint) or any explicit key= field.
	regexp.MustCompile(`(?i)(received api key\s*=\s*)\S+`),
	// Any bare sk- token that slipped through.
	regexp.MustCompile(`sk-[A-Za-z0-9._\-]{6,}`),
	// A long standalone hex fingerprint (32+ hex chars).
	regexp.MustCompile(`\b[0-9a-f]{32,}\b`),
}

// redactLiteLLMKeyMaterial removes API-key hints and key hashes from a gateway
// error body before it is surfaced to the dashboard or logged.
func redactLiteLLMKeyMaterial(s string) string {
	for i, re := range litellmKeyMaterialPatterns {
		if i < 2 {
			// Keep the field label, redact the value.
			s = re.ReplaceAllString(s, "${1}[redacted]")
		} else {
			s = re.ReplaceAllString(s, "[redacted]")
		}
	}
	return s
}

// probeLiteLLMModels queries {endpoint}/v1/models with the given key and
// returns the number of models the gateway offers. This is a LIVE probe
// only — it never falls back to the static model aliases, so a failing
// gateway can never masquerade as "N models available" (the bug where
// Test Connection reported the 7 static fallback aliases as success).
// On failure the error carries the gateway's actual message (truncated)
// so the UI can show e.g. LiteLLM's "token not found" instead of a bare
// status code.
func probeLiteLLMModels(endpoint, apiKey string) (int, error) {
	return probeModelsWithHeaders(endpoint, apiKey, nil)
}

// probeModelsWithHeaders is probeLiteLLMModels with an optional set of extra
// request headers (e.g. watsonx's X-IBM-Project-ID). apiKey, when non-empty, is
// still sent as the Bearer — for watsonx the caller passes the minted IAM token
// as apiKey, not the raw key.
func probeModelsWithHeaders(endpoint, apiKey string, extraHeaders map[string]string) (int, error) {
	modelsURL := strings.TrimRight(endpoint, "/") + "/v1/models"
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	// Re-validate every redirect hop: a gateway endpoint is operator-supplied
	// but the redirect target is chosen by the remote server at request time.
	client := &http.Client{Timeout: litellmProbeTimeout, CheckRedirect: noRedirectToPrivate}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach gateway: %w", err)
	}
	defer closeHTTPBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// Error path: only a truncated slice of the body is surfaced to the
		// dialog (error bodies can be huge and may echo the key).
		body, _ := io.ReadAll(io.LimitReader(resp.Body, litellmProbeMaxErrBody))
		gatewayMsg := redactLiteLLMKeyMaterial(strings.TrimSpace(string(body)))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			// The two auth failures lead users to different fixes.
			if apiKey == "" {
				return 0, fmt.Errorf("gateway requires an API key and none is configured (HTTP %d): %s",
					resp.StatusCode, gatewayMsg)
			}
			return 0, fmt.Errorf("gateway rejected the configured key (HTTP %d): %s",
				resp.StatusCode, gatewayMsg)
		default:
			return 0, fmt.Errorf("gateway returned HTTP %d: %s", resp.StatusCode, gatewayMsg)
		}
	}

	// Success path: parse the FULL body with the exact same lenient decoder
	// the model-discovery dropdown uses (fetchModelsFromEndpoint) — a JSON
	// object with a "data" array of items each carrying an "id" string is
	// sufficient. We do NOT require top-level object=="list" (some gateways
	// omit or reorder it) and tolerate extra/unknown fields. Reading only a
	// truncated prefix here (the old bug) corrupted large valid lists into
	// invalid JSON and produced a false "non-OpenAI response" negative.
	models, err := parseModelsResponse(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("gateway returned a non-OpenAI /v1/models response")
	}
	return len(models), nil
}

// redactSecret removes any occurrence of a secret value from a message so
// gateway error bodies can never echo the key back to the client or logs.
func redactSecret(msg, secret string) string {
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, "[redacted]")
}

// maskHintVisibleChars is how many trailing characters of a secret survive
// masking in UI hints ("••••WMg" style) — enough to recognize a key,
// useless to reconstruct one.
const maskHintVisibleChars = 4

// maskSecretHint returns a display-safe hint for a secret value: bullets
// plus the last few characters. Values too short to safely reveal a tail
// are fully masked.
func maskSecretHint(v string) string {
	if len(v) <= maskHintVisibleChars*2 {
		return "••••"
	}
	return "••••" + v[len(v)-maskHintVisibleChars:]
}

// litellmSectionResponse builds the governor-config GET payload for the
// LiteLLM tab. The resolved key VALUE is never serialized — only hasKey,
// the store it came from, and a masked tail hint. The apiKeyEnv/apiKeyFile
// CONFIG fields normally hold a var NAME / file PATH (not secrets), but a
// user who pasted an actual key into them before the guardrails existed
// would otherwise get it echoed straight back into the tab — in one live
// case the raw key rendered in plaintext during screen shares. Key-like
// values in those fields are therefore masked too, with LooksLikeKey
// flags so the UI can tell the user to move the value.
func litellmSectionResponse(lc *config.LiteLLMConfig) map[string]interface{} {
	apiKeyEnv := lc.APIKeyEnv
	apiKeyEnvLooksLikeKey := looksLikeAPIKeyValue(apiKeyEnv)
	if apiKeyEnvLooksLikeKey {
		apiKeyEnv = maskSecretHint(apiKeyEnv)
	}
	apiKeyFile := lc.APIKeyFile
	apiKeyFileLooksLikeKey := !strings.HasPrefix(apiKeyFile, "/") && looksLikeAPIKeyValue(apiKeyFile)
	if apiKeyFileLooksLikeKey {
		apiKeyFile = maskSecretHint(apiKeyFile)
	}
	key := lc.ResolveAPIKey()
	keyHint := ""
	if key != "" {
		keyHint = maskSecretHint(key)
	}
	return map[string]interface{}{
		"endpoint":               lc.Endpoint,
		"apiKeyEnv":              apiKeyEnv,
		"apiKeyEnvLooksLikeKey":  apiKeyEnvLooksLikeKey,
		"apiKeyFile":             apiKeyFile,
		"apiKeyFileLooksLikeKey": apiKeyFileLooksLikeKey,
		"defaultModel":           lc.DefaultModel,
		"caBundle":               lc.CABundle,
		"localProxy":             lc.LocalProxy,
		"hasKey":                 key != "",
		"keyHint":                keyHint,
		// redactSecret guards the pathological case of a key-like env var
		// NAME appearing in the source string.
		"keySource": redactSecret(lc.ResolveAPIKeySource(), key),
	}
}

// classifierSectionResponse surfaces the effective tier-classification keyword
// lists (Phase 4 Part C) to the dashboard governor-config view. It returns the
// keywords actually in force — config-driven when a `classifier:` block is set,
// else the built-in defaults — so operators can see and (via hive.yaml) edit
// which title keywords map issues to the Simple/Complex tiers. classify.SetLanes
// already surfaces per-agent lane_keywords per-agent; these are the analogous
// global tier lists.
func classifierSectionResponse() map[string]interface{} {
	simple, complex := classify.TierKeywords()
	return map[string]interface{}{
		"simpleKeywords": simple,
		"complexSignals": complex,
	}
}

// liteLLMProbeResult runs the live probe against the effective endpoint
// using the SAME key resolution the inference translator uses
// (ResolveAPIKey: api_key_file → Secret mount → PVC file → env), unless
// overrideKey is set (a key just submitted in the same request, which the
// key files may not reflect yet). Returns nil when no endpoint is
// configured.
func (s *Server) liteLLMProbeResult(lc *config.LiteLLMConfig, overrideKey string) map[string]interface{} {
	ep := lc.ResolveEndpoint()
	if ep == "" {
		return nil
	}
	probeKey := overrideKey
	if probeKey == "" {
		probeKey = lc.ResolveAPIKey()
	}
	n, err := probeLiteLLMModels(ep, probeKey)
	if err != nil {
		return map[string]interface{}{"ok": false, "error": redactSecret(err.Error(), probeKey)}
	}
	result := map[string]interface{}{"ok": true, "models": n}
	// If the proxy has learned this key's entitled subset (some gateways
	// advertise the full catalog on /v1/models but scope the key to fewer),
	// surface how many of the ADVERTISED models are actually usable so Test
	// Connection can say "N models available (M usable with your key)". Use
	// the intersection against the advertised list — not the raw entitled
	// count, which may list ids this gateway doesn't advertise.
	if fn := getEntitledModelsFn(); fn != nil {
		if entitled, _, ok := fn(ep); ok && len(entitled) > 0 {
			if advertised, ferr := fetchModelsFromEndpoint(ep, probeKey); ferr == nil {
				if usable := len(intersectEntitled(advertised, entitled)); usable < n {
					result["usable"] = usable
				}
			}
		}
	}
	return result
}

// handleGovernorLiteLLMTest is the Test Connection endpoint: a live
// /v1/models probe with the currently effective endpoint + key. Unlike
// /api/inference/models/{backend} it NEVER substitutes the static
// fallback aliases, so the result reflects only what the gateway said.
func (s *Server) handleGovernorLiteLLMTest(w http.ResponseWriter, r *http.Request) {
	lc := s.deps.Config.Governor.LiteLLM
	probe := s.liteLLMProbeResult(&lc, "")
	if probe == nil {
		jsonError(w, "no litellm endpoint configured", http.StatusBadRequest)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "probe": probe})
}

// handleGovernorLiteLLM updates governor.litellm from the dashboard's
// LiteLLM config tab / first-use dialog. An API key VALUE (apiKey) is
// stored via storeLiteLLMAPIKey (PVC file + best-effort hive-secrets
// Secret) — never in hive.yaml, logs, or responses; hive.yaml records
// only the file path. After saving, the gateway is probed at /v1/models
// and the result returned so a bad endpoint/key fails visibly at save
// time instead of as agent 401s later.
func (s *Server) handleGovernorLiteLLM(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Endpoint     *string `json:"endpoint"`
		APIKey       *string `json:"apiKey"`
		APIKeyEnv    *string `json:"apiKeyEnv"`
		APIKeyFile   *string `json:"apiKeyFile"`
		DefaultModel *string `json:"defaultModel"`
		CABundle     *string `json:"caBundle"`
		LocalProxy   *bool   `json:"localProxy"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	// Apply to a copy first so validation failure leaves config untouched.
	lc := cfg.Governor.LiteLLM
	if body.Endpoint != nil {
		lc.Endpoint = strings.TrimSpace(*body.Endpoint)
	}
	if body.APIKeyEnv != nil {
		keyEnv := strings.TrimSpace(*body.APIKeyEnv)
		if looksLikeAPIKeyValue(keyEnv) {
			jsonError(w, "this looks like an API key — paste it in the API Key field instead; "+
				"this field takes an environment variable NAME (e.g. HIVE_LITELLM_API_KEY)",
				http.StatusBadRequest)
			return
		}
		lc.APIKeyEnv = keyEnv
	}
	if body.APIKeyFile != nil {
		keyFile := strings.TrimSpace(*body.APIKeyFile)
		// Same paste-the-key-value guardrail as the env-name field; real
		// key file paths are absolute, keys never are.
		if !strings.HasPrefix(keyFile, "/") && looksLikeAPIKeyValue(keyFile) {
			jsonError(w, "this looks like an API key — paste it in the API Key field instead; "+
				"this field takes a file PATH (e.g. /secrets/litellm_api_key)",
				http.StatusBadRequest)
			return
		}
		// SECURITY (audit N8, CWE-200/918): the same confinement the gateway
		// upsert applies. This is the LiteLLM twin of that handler and carries
		// the identical defect — the guardrail above short-circuits for ANY
		// absolute path, so an arbitrary file could be stored and later read.
		if keyFile != "" && !config.SecretFilePathAllowed(keyFile) {
			jsonError(w, "api_key_file must be under /secrets or "+config.WritableSecretsDir,
				http.StatusBadRequest)
			return
		}
		lc.APIKeyFile = keyFile
	}
	if body.DefaultModel != nil {
		lc.DefaultModel = strings.TrimSpace(*body.DefaultModel)
	}
	if body.CABundle != nil {
		lc.CABundle = strings.TrimSpace(*body.CABundle)
	}
	if body.LocalProxy != nil {
		lc.LocalProxy = *body.LocalProxy
	}
	if err := lc.Validate(); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Store a submitted key VALUE outside hive.yaml and point api_key_file
	// at it. Empty apiKey means "no change" so the tab can be re-saved
	// without re-entering the key.
	submittedKey := ""
	if body.APIKey != nil {
		submittedKey = strings.TrimSpace(*body.APIKey)
	}
	if submittedKey != "" {
		keyFile, err := s.storeLiteLLMAPIKey(submittedKey)
		if err != nil {
			jsonError(w, "failed to store API key: "+redactSecret(err.Error(), submittedKey),
				http.StatusInternalServerError)
			return
		}
		lc.APIKeyFile = keyFile
		// Remediation for the pre-guardrail leak: a key VALUE pasted into
		// the env-name field never worked as a name (os.Getenv("sk-...")
		// is empty) and is a plaintext secret in hive.yaml. Now that a
		// real key is stored properly, scrub it.
		if looksLikeAPIKeyValue(lc.APIKeyEnv) {
			lc.APIKeyEnv = ""
			s.logger.Info("cleared key-like value from litellm api_key_env (replaced by stored API key)")
		}
	}
	cfg.Governor.LiteLLM = lc

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after litellm update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_litellm", auditDetail("section", "litellm"), "")

	// Register the endpoint for model discovery (empty list unregisters)
	// and re-apply routes for live agents already running on litellm.
	// This must come after the key store above: SetInferenceRoute
	// snapshots the key into each route, so the refresh is what makes a
	// rotated key take effect without an agent restart.
	var endpoints []string
	if ep := lc.ResolveEndpoint(); ep != "" {
		endpoints = []string{ep}
	}
	s.UpdateInferenceEndpoint("litellm", endpoints)
	if s.deps.AgentMgr != nil {
		s.deps.AgentMgr.RefreshInferenceRoutes("litellm")
	}

	s.refreshAndPersist()

	// Save-time validation probe (live /v1/models — never the static
	// fallback list) so the dialog can immediately show "N models
	// available" or the gateway's real error.
	resp := map[string]interface{}{"ok": true, "status": "updated"}
	if probe := s.liteLLMProbeResult(&lc, submittedKey); probe != nil {
		resp["probe"] = probe
	}
	jsonResponse(w, resp)
}

// handleGovernorBobStatus reports whether a bob API key is configured, and
// WHERE it came from — never the value. The source string
// ("file:/data/secrets/bob_api_key", "env:HIVE_BOB_API_KEY") is the same
// safe-to-log form ResolveAPIKeySource returns and is what the dashboard
// shows so an operator can tell an admin-managed Secret key from one they
// entered here.
func (s *Server) handleGovernorBobStatus(w http.ResponseWriter, r *http.Request) {
	bc := s.deps.Config.Governor.Bob
	source := bc.ResolveAPIKeySource()
	jsonResponse(w, map[string]interface{}{
		"ok": true,
		// configured is presence-only; the key value is never serialized.
		"configured": source != "",
		"source":     source,
		// keyName is the operator-chosen LABEL for the key, not the key value —
		// safe to serialize. Empty on hives that never recorded a name (the
		// dashboard renders that as "(unnamed)"), so no backwards-compat break.
		"keyName": bc.KeyName,
	})
}

// handleGovernorBobKey stores a bob API key VALUE submitted from the spoke
// dashboard's governor Bob tab. The key is hive-wide, not per-agent: this
// endpoint backs a single hive-scoped setting shared by every bob agent.
// The value is written to a 0600 PVC file
// (and best-effort into the hive-secrets Secret); hive.yaml records only the
// resulting file PATH. The value never appears in the response, in hive.yaml,
// or in any log line.
//
// Authorization is the standard config-endpoint gating: this is a PUT, so the
// roleEnforcement middleware rejects a read-only role with 403 before the
// handler runs.
func (s *Server) handleGovernorBobKey(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		APIKey *string `json:"apiKey"`
		// KeyName is an optional human LABEL recorded alongside the key so
		// managers can tell keys apart without seeing values. Absent
		// (nil) leaves any existing name untouched; present-but-empty clears it.
		KeyName *string `json:"keyName"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.APIKey == nil {
		jsonError(w, "apiKey is required", http.StatusBadRequest)
		return
	}
	var keyName string
	nameProvided := body.KeyName != nil
	if nameProvided {
		keyName = strings.TrimSpace(*body.KeyName)
		if len(keyName) > bobKeyNameMaxLen {
			jsonError(w, fmt.Sprintf("keyName is too long (limit %d characters)", bobKeyNameMaxLen),
				http.StatusBadRequest)
			return
		}
	}
	key := strings.TrimSpace(*body.APIKey)
	if key == "" {
		// Distinct from DELETE: an empty PUT is a mistake (an empty field
		// submitted), not an intentional revoke, so say so rather than
		// silently wiping a working key.
		jsonError(w, "apiKey is empty — paste the bob API key, or use Clear to remove the existing one",
			http.StatusBadRequest)
		return
	}
	if len(key) > bobKeyMaxLen {
		jsonError(w, fmt.Sprintf("apiKey is too long (limit %d characters)", bobKeyMaxLen),
			http.StatusBadRequest)
		return
	}
	// A key with interior whitespace/newlines is always a broken paste, and
	// storing one is a proven auth failure ("invalid jwt string" 401 → every
	// bob agent parks at the auth prompt). Refuse at save time with the real
	// explanation. Leading/trailing whitespace was already trimmed above.
	if strings.ContainsAny(key, " \t\r\n") {
		jsonError(w, "apiKey contains interior whitespace or line breaks — a bob API key is a single unbroken token; re-copy it from bob.ibm.com and paste it again",
			http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	// Apply to a copy so a store failure leaves config untouched.
	bc := cfg.Governor.Bob
	keyFile, err := s.storeBobAPIKey(key)
	if err != nil {
		// redactSecret guards against the key surfacing via a filesystem
		// error that happened to embed it.
		jsonError(w, "failed to store API key: "+redactSecret(err.Error(), key),
			http.StatusInternalServerError)
		return
	}
	bc.APIKeyFile = keyFile
	// A key VALUE pasted into api_key_env would be a plaintext secret in
	// hive.yaml and never worked as a name (os.Getenv("...") is empty).
	// Now that a real key is stored properly, scrub it.
	// Record the label only when the field was present: absent leaves the
	// existing name, present-but-empty clears it (both per the body contract).
	if nameProvided {
		bc.KeyName = keyName
	}
	if looksLikeAPIKeyValue(bc.APIKeyEnv) {
		bc.APIKeyEnv = ""
		s.logger.Info("cleared key-like value from bob api_key_env (replaced by stored API key)")
	}
	cfg.Governor.Bob = bc

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after bob api key update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_bob_key", auditDetail("section", "bob", "action", "set"), "")
	s.refreshAndPersist()

	// No agent restart is required to ADOPT the key: main.go injected a
	// resolver closure over the live config into the agent manager
	// (SetBobAPIKeyResolver), and it re-reads the key file on every agent
	// launch. Agents already parked in "failed: no API key" cannot pick it up
	// on their own, though: the key is Secret, so it is delivered only by tmux
	// set-environment, which is inherited by shells created AFTER it runs — and
	// their pane shell predates the key. RelaunchBobAgentsAwaitingKey therefore
	// recreates those sessions so a fresh shell inherits the key. Running,
	// paused, and non-bob agents are untouched, and a second save finds nothing
	// left to do.
	// s.deps.Ctx, NOT r.Context(): the launch path derives the agent's
	// long-lived pane-polling goroutine from this context, so a request-scoped
	// one would cancel it the moment this response is written. Every other
	// Start/Restart/Resume caller in the dashboard passes s.deps.Ctx too.
	var relaunched []string
	if s.deps != nil && s.deps.AgentMgr != nil {
		relaunched = s.deps.AgentMgr.RelaunchBobAgentsAwaitingKey(s.deps.Ctx)
	}
	if len(relaunched) > 0 {
		s.logger.Info("relaunched bob agents after api key save",
			"count", len(relaunched), "agents", strings.Join(relaunched, ","))
	}

	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"configured": true,
		// Path only, never the value.
		"source": "file:" + keyFile,
		// The pod does not need restarting; the resolver reads the key live.
		"restartNeeded": false,
		// How many parked bob agents were started by this save, so the UI can
		// report what actually happened instead of telling the user to do it.
		"relaunched": len(relaunched),
		// The label, never the value (see handleGovernorBobStatus).
		"keyName": bc.KeyName,
	})
}

// handleGovernorBobKeyClear revokes a stored bob API key: it removes the PVC
// key file and drops the api_key_file pointer from hive.yaml, returning the
// hive to the documented "key required" state rather than a half-configured
// one.
//
// It deliberately does NOT try to delete the key from the hive-secrets Secret
// or from an admin-mounted /secrets/bob_api_key: those are managed outside the
// dashboard, and ResolveAPIKey consults them ahead of the PVC file. The
// response reports the source that remains so the UI can tell the user the key
// is still supplied by an admin-managed store instead of falsely claiming it
// was cleared.
func (s *Server) handleGovernorBobKeyClear(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	removed, err := clearBobKeyFile()
	if err != nil {
		jsonError(w, "failed to clear API key: "+err.Error(), http.StatusInternalServerError)
		return
	}

	cfg := s.deps.Config
	bc := cfg.Governor.Bob
	// Only drop the pointer if it names the file we just removed; an operator
	// who pointed api_key_file at their own path keeps that setting. The label
	// describes the key we just cleared, so drop it in the same case — leaving
	// a stale "Team inference key" beside a now-empty slot would mislead.
	if bc.APIKeyFile == writableBobKeyFile {
		bc.APIKeyFile = ""
		bc.KeyName = ""
	}
	cfg.Governor.Bob = bc

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after bob api key clear", "error", err)
	}
	s.logger.Info("bob api key cleared", "api_key_file", writableBobKeyFile, "file_removed", removed)
	s.auditFromRequest(r, "config_governor_bob_key", auditDetail("section", "bob", "action", "clear"), "")
	s.refreshAndPersist()

	// Re-resolve: an admin-managed Secret/env key may still supply one.
	remaining := cfg.Governor.Bob.ResolveAPIKeySource()
	jsonResponse(w, map[string]interface{}{
		"ok":         true,
		"configured": remaining != "",
		"source":     remaining,
	})
}

func (s *Server) handleGovernorAddAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Name    string `json:"name"`
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil || body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	body.Name = sanitizeString(body.Name)
	body.Backend = sanitizeString(body.Backend)
	body.Model = sanitizeString(body.Model)

	if body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(body.Name, " ./\\") || !kickTemplatePattern.MatchString(body.Name+".md") {
		jsonError(w, "name must contain only alphanumeric characters, hyphens, and underscores", http.StatusBadRequest)
		return
	}
	const maxAgentNameLen = 64
	if len(body.Name) > maxAgentNameLen {
		jsonError(w, fmt.Sprintf("name must be at most %d characters", maxAgentNameLen), http.StatusBadRequest)
		return
	}

	if _, exists := s.deps.Config.Agents[body.Name]; exists {
		jsonError(w, "agent already exists", http.StatusConflict)
		return
	}

	if body.Backend == "" {
		body.Backend = "claude"
	}
	// Reject an unsupported backend before the agent is created, rather than
	// creating an agent that can never launch.
	if err := s.deps.Config.Governor.ValidateBackend(body.Backend); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := config.AgentConfig{
		Backend: body.Backend,
		Model:   body.Model,
		Enabled: true,
	}
	// An explicit re-add is the operator changing their mind, and it is the
	// only signal that lifts a deletion tombstone. Without this the agent
	// would be added now and pruned again by the next config reload.
	if s.deps.Config.ClearAgentRemoved(body.Name) {
		s.logger.Info("agent deletion tombstone lifted by explicit re-add", "agent", body.Name)
	}
	s.deps.Config.Agents[body.Name] = agentCfg
	s.deps.AgentMgr.AddAgent(body.Name, agentCfg)
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config", "error", err)
	}

	s.auditFromRequest(r, "add_agent", auditDetail("backend", body.Backend, "model", body.Model), body.Name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "added", "agent": body.Name})
}

func (s *Server) handleGovernorRemoveAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	delete(s.deps.Config.Agents, name)
	s.deps.AgentMgr.RemoveAgent(name)

	// Tombstone the name BEFORE saving. Deleting from the in-memory map and
	// re-saving the overlay was all this handler used to do, and it did not
	// stick for two independent reasons:
	//
	//  1. /data/agent-configs/<name>.yaml was left on disk, and Load()'s
	//     MergeAgentOverrides UNIONS that directory over the config — so the
	//     next config reload (fsnotify; measured at ~36s after the delete on a
	//     live hive) re-materialized the agent. That is the reported
	//     "readded themselves a minute or so later".
	//  2. Even with the file gone, ApplyPack — which runs on every restart —
	//     re-created any pack agent missing from the roster.
	//
	// The tombstone closes both: MergeAgentOverrides skips and prunes it, and
	// ApplyPack refuses to re-create it.
	tombstoned := s.deps.Config.MarkAgentRemoved(name)
	overlayExisted, overlayDeleted := s.removeAgentOverlayFile(name)

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent removal", "error", err)
	}
	s.auditFromRequest(r, "remove_agent", "", name)
	s.refreshAndPersist()
	s.logger.Info("agent removed and tombstoned", "agent", name,
		"note", "will not be re-created by an ACMM pack apply until explicitly re-added")
	// One greppable audit line for the whole removal action: whether the
	// tombstone was newly written, whether a stale per-agent overlay file was
	// present and whether it was deleted, and the resulting tombstone set so a
	// grep by agent name tells the story of a non-sticking removal report
	// (#2439) without exec'ing into the pod to diff config files.
	s.logger.Info("audit: agent removed via dashboard",
		"hive_id", s.deps.Config.HiveID,
		"agent", name,
		"tombstoned", tombstoned,
		"overlay_file_existed", overlayExisted,
		"overlay_file_deleted", overlayDeleted,
		"removed_agents", s.deps.Config.RemovedAgents,
	)
	jsonResponse(w, agentDeletionResponse("removed", name, packLevelsDefining(name)))
}

// removeAgentOverlayFile deletes /data/agent-configs/<name>.yaml. A leftover
// overlay file is re-merged by every config load, which is one of the two ways
// a deleted agent used to come back.
//
// Returns (existed, deleted) purely for the caller's audit log: existed reports
// whether a per-agent overlay file was present before the delete (the stale-file
// resurrection vector), and deleted reports whether the removal succeeded. The
// deletion behavior itself is unchanged — an error is still non-fatal because the
// tombstone already prevents the merge from resurrecting the agent.
func (s *Server) removeAgentOverlayFile(name string) (existed, deleted bool) {
	agentsDir := s.deps.Config.Data.AgentsDir
	if agentsDir == "" {
		return false, false
	}
	// Observe presence before deleting so the audit log can distinguish
	// "no stale file" from "stale file removed". Stat errors other than
	// not-exist leave existed false — we only claim a file was present when
	// we positively saw one.
	overlayPath := filepath.Join(agentsDir, name+".yaml")
	if _, statErr := os.Stat(overlayPath); statErr == nil {
		existed = true
	}
	if err := config.RemoveAgentFile(agentsDir, name); err != nil {
		// Not fatal: the tombstone already prevents the merge from
		// resurrecting the agent. Log it at WARN so a stale file — the
		// resurrection vector for #2439 — is loud and greppable with its path.
		s.logger.Warn("failed to remove agent overlay file after delete (tombstone still applies)",
			"hive_id", s.deps.Config.HiveID, "agent", name, "path", overlayPath, "error", err)
		return existed, false
	}
	return existed, existed
}

// packLevelsDefining returns the ACMM levels whose pack defines this agent, so
// the delete response can tell the operator up front that the level's pack
// would otherwise have re-added it. Legibility is the point: silently undoing
// the operator's action a minute later is what turned this into a bug report
// instead of a question.
func packLevelsDefining(name string) []int {
	var levels []int
	for _, pack := range config.ACMMPacks() {
		for _, pa := range pack.Agents {
			if pa.Name == name {
				levels = append(levels, pack.Level)
				break
			}
		}
	}
	return levels
}

// agentDeletionResponse builds the delete response, including an explicit
// human-readable note when the agent is part of an ACMM pack.
func agentDeletionResponse(status, name string, packLevels []int) map[string]any {
	resp := map[string]any{"ok": true, "status": status, "agent": name, "tombstoned": true}
	if len(packLevels) == 0 {
		resp["note"] = "This agent is not part of any ACMM pack; it will stay deleted."
		return resp
	}
	parts := make([]string, 0, len(packLevels))
	for _, level := range packLevels {
		parts = append(parts, strconv.Itoa(level))
	}
	resp["packLevels"] = packLevels
	resp["note"] = fmt.Sprintf(
		"%s is defined by the ACMM pack for level %s. It has been recorded as deliberately deleted, so no restart or level change will bring it back. Re-add it from the Governor grid if you change your mind.",
		name, strings.Join(parts, ", "))
	return resp
}

func (s *Server) handleGovernorRepos(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Repos       []string `json:"repos"`
		PrimaryRepo *string  `json:"primaryRepo,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if len(body.Repos) == 0 && body.PrimaryRepo == nil {
		jsonError(w, "at least one repo is required", http.StatusBadRequest)
		return
	}
	org := s.deps.Config.Project.Org

	// Single-host-per-spoke (defence in depth; the Repos tab also checks this
	// client-side so it fails fast). Every repo the user submits must live on
	// this hive's forge — mixing github.com and a GHE instance silently breaks
	// App auth for half the repos. Check the raw pasted values BEFORE the loop
	// below strips the host off each one. hiveForgeHost() is this hive's own
	// GitHub host (github.com or the GHE hostname), the same value the dashboard
	// derives from github_base_url.
	// Snapshot so a validation failure AFTER we mutate the in-memory config can
	// restore it — the "always exactly one default" guard below runs once the
	// final repos+primary are known, and a reject must not leave a half-applied
	// config in memory (which would then be persisted on the next unrelated save).
	prevRepos := append([]string(nil), s.deps.Config.Project.Repos...)
	prevPrimary := s.deps.Config.Project.PrimaryRepo

	spokeHost := s.hiveForgeHost()
	validateRepos := prevRepos
	if len(body.Repos) > 0 {
		validateRepos = body.Repos
	}
	validatePrimary := prevPrimary
	if body.PrimaryRepo != nil {
		validatePrimary = *body.PrimaryRepo
	}
	// Normalize org-qualified entries ("myorg/myrepo" under org "myorg") to the
	// bare repo name BEFORE validating, so the accepted value and the persisted
	// value are the same shape. Without this an owner pasting the org/repo form
	// GitHub shows everywhere is rejected with a 400 here, or — worse, on paths
	// that skip this handler — persisted org-qualified and then resolved as
	// "org/org/repo", which fails every agent. An entry qualified with a
	// different org is left untouched and still 400s below.
	validateRepos, _ = config.NormalizeProjectRepos(org, validateRepos)
	validatePrimary, _ = config.NormalizeRepoForOrg(org, validatePrimary)
	if issue := config.ValidateProjectRepoTargets(org, validateRepos, validatePrimary, spokeHost); issue != nil {
		jsonError(w, issue.Message, http.StatusBadRequest)
		return
	}
	// Feed the normalized values back into the body so the persistence code
	// below stores bare names even when its own url-parse branch does not fire.
	if len(body.Repos) > 0 {
		body.Repos = validateRepos
	}
	if body.PrimaryRepo != nil {
		body.PrimaryRepo = &validatePrimary
	}
	for _, ref := range body.Repos {
		if h := repoRefHostLabel(ref); h != "" && !sameForgeHost(h, spokeHost) {
			jsonError(w, fmt.Sprintf("repo %q is on %s but this hive is on %s — a hive's repos must all be on one GitHub host. Remove the mismatched repo or use a repo on %s.", strings.TrimSpace(ref), h, spokeHost, spokeHost), http.StatusBadRequest)
			return
		}
	}
	if body.PrimaryRepo != nil {
		if h := repoRefHostLabel(*body.PrimaryRepo); h != "" && !sameForgeHost(h, spokeHost) {
			jsonError(w, fmt.Sprintf("default repo %q is on %s but this hive is on %s — the default must live on this hive's forge.", strings.TrimSpace(*body.PrimaryRepo), h, spokeHost), http.StatusBadRequest)
			return
		}
	}

	if len(body.Repos) > 0 {
		stripped := make([]string, 0, len(body.Repos))
		for _, repo := range body.Repos {
			repo = sanitizeString(repo)
			if repo == "" || strings.Contains(repo, "..") || strings.ContainsAny(repo, "<>\"';&|") {
				jsonError(w, fmt.Sprintf("invalid repo name: %s", repo), http.StatusBadRequest)
				return
			}
			if parsed, err := url.Parse(repo); err == nil && parsed.Scheme != "" && parsed.Host != "" {
				parts := strings.SplitN(strings.TrimPrefix(parsed.Path, "/"), "/", 3)
				if len(parts) >= 2 {
					if parts[0] != "" {
						org = parts[0]
						s.deps.Config.Project.Org = org
					}
					repo = parts[1]
					if parsed.Host != "github.com" {
						s.deps.Config.GitHub.BaseURL = parsed.Scheme + "://" + parsed.Host
						s.deps.Config.GitHub.APIURL = parsed.Scheme + "://" + parsed.Host + "/api/v3"
					}
				}
			}
			if org != "" && strings.HasPrefix(repo, org+"/") {
				stripped = append(stripped, strings.TrimPrefix(repo, org+"/"))
			} else {
				stripped = append(stripped, repo)
			}
		}
		s.deps.Config.Project.Repos = stripped
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetRepos(stripped)
		}
	}

	if body.PrimaryRepo != nil {
		newPrimary := sanitizeString(*body.PrimaryRepo)
		if parsed, err := url.Parse(newPrimary); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			parts := strings.SplitN(strings.TrimPrefix(parsed.Path, "/"), "/", 3)
			if len(parts) >= 2 {
				newPrimary = parts[1]
			}
		}
		if org != "" && strings.HasPrefix(newPrimary, org+"/") {
			newPrimary = strings.TrimPrefix(newPrimary, org+"/")
		}
		oldPrimary := s.deps.Config.Project.PrimaryRepo
		s.deps.Config.Project.PrimaryRepo = newPrimary
		if newPrimary != oldPrimary {
			s.logger.Info("primary repo changed", "from", oldPrimary, "to", newPrimary)
			if s.deps.AdvisoryResetFunc != nil {
				go s.deps.AdvisoryResetFunc(newPrimary)
			}
		}
	}

	// Always exactly one default: a hive with repos must name a primary_repo that
	// is one of them — that is the repo the advisory issue is maintained in. The
	// Repos tab disables Save until a star is chosen, but enforce it server-side
	// too so no client (or a stale one) can persist repos with no default. Reject
	// and restore the pre-mutation config so the in-memory state stays coherent.
	if final := s.deps.Config.Project.Repos; len(final) > 0 {
		primary := s.deps.Config.Project.PrimaryRepo
		inList := false
		for _, r := range final {
			if r == primary {
				inList = true
				break
			}
		}
		if primary == "" || !inList {
			s.deps.Config.Project.Repos = prevRepos
			s.deps.Config.Project.PrimaryRepo = prevPrimary
			if s.deps.GHClient != nil {
				s.deps.GHClient.SetRepos(prevRepos)
			}
			jsonError(w, "set a default repo before saving — one of the monitored repos must be marked as the default (the repo where the advisory issue is maintained)", http.StatusBadRequest)
			return
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config", "error", err)
	}
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}
	s.auditFromRequest(r, "config_governor_repos", auditDetail("section", "repos"), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// hiveForgeHost returns the bare GitHub hostname this hive's repos and App live
// on ("github.com" or a GHE hostname like "github.ibm.com"). It is the single
// source of truth the single-host-per-spoke guard compares each submitted repo
// against, and mirrors config.GitHubConfig.HostLabel() (the same value the
// dashboard reads as github_base_url's host). Falls back to public github.com
// when no config is loaded (tests/early boot).
func (s *Server) hiveForgeHost() string {
	if s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.GitHub.HostLabel()
	}
	return "github.com"
}

// repoRefHostLabel extracts the GitHub host a repo string was pasted with, or ""
// when none was given (a bare "repo" or "owner/repo", which by definition belongs
// to the hive's own forge). Mirrors hub.repoRefHost: a full URL or a leading
// dotted segment ("github.ibm.com/org/repo") names an explicit host. Returned
// lowercased for case-insensitive comparison in sameForgeHost.
func repoRefHostLabel(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.Trim(s, "/")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		return strings.ToLower(parts[0])
	}
	return ""
}

// sameForgeHost reports whether two host labels refer to the same GitHub. Both
// "" and "github.com" mean public GitHub; a GHE host equals only itself.
// Case-insensitive. Mirrors hub.sameGitHubHost so the dashboard's single-host
// rule matches the hub's request/assign validation exactly.
func sameForgeHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || h == "github.com" {
			return "github.com"
		}
		return h
	}
	return norm(a) == norm(b)
}

func configuredProjectOrgForGitHubApp(cfg *config.Config, logger *slog.Logger) string {
	if cfg == nil {
		return ""
	}
	org := strings.TrimSpace(cfg.Project.Org)
	if org == "" {
		return ""
	}
	forgeHost := strings.TrimSpace(cfg.GitHub.HostLabel())
	if forgeHost == "" {
		forgeHost = "github.com"
	}
	if !strings.Contains(org, ".") || !sameForgeHost(org, forgeHost) {
		return org
	}

	primary := strings.TrimSpace(cfg.Project.PrimaryRepo)
	if primary == "" && len(cfg.Project.Repos) > 0 {
		primary = strings.TrimSpace(cfg.Project.Repos[0])
	}
	derived := firstRepoPathSegment(primary)
	if derived == "" || strings.EqualFold(derived, org) {
		return org
	}
	if logger != nil {
		logger.Warn("project org is configured as the GitHub forge host; deriving GitHub App organization from primary repo",
			"configured_org", org, "derived_org", derived, "primary_repo", primary, "forge_host", forgeHost)
	}
	return derived
}

func firstRepoPathSegment(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "http://")
	ref = strings.Trim(ref, "/")
	if ref == "" {
		return ""
	}
	parts := strings.Split(ref, "/")
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// handleGovernorRepoCheckAccess verifies that this hive's GitHub App can access
// a repo the operator is about to add, and — when it cannot — returns the
// per-forge App install/authorize URL so the Repos tab can guide the user to
// grant access before the repo is finally accepted (requirement #3).
//
// The check today probes at ORG granularity: DiscoverInstallationID(org) returns
// ErrNoInstallationForOrg when the App is not installed on the repo's org at all,
// which is the common "new org the App was never installed on" case and the one
// that hard-blocks reads and writes. On a positive result we report ok:true.
//
// TODO(repo-access-probe): tighten to REPO granularity. An App installed on the
// org "Only select repositories" may still lack THIS repo (see #2353's
// write-forbidden case). The deeper probe should mint an installation token and
// GET /repos/{org}/{repo} (s.deps.GHClient.GetRepo) — a 404/403 there means the
// installation exists but this repo is not in its selected set, which needs the
// same "manage installation" URL to add the repo to the selection. That probe is
// scoped out of this change to keep it focused; the org-level check plus the
// install workflow already covers the "App not installed on the org" footgun.
func (s *Server) handleGovernorRepoCheckAccess(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	ref := sanitizeString(body.Repo)
	if ref == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	// Single-host guard here too: an added repo on a different forge is rejected
	// before we even probe access, with the same message as the save path.
	spokeHost := s.hiveForgeHost()
	if h := repoRefHostLabel(ref); h != "" && !sameForgeHost(h, spokeHost) {
		jsonError(w, fmt.Sprintf("repo %q is on %s but this hive is on %s — a hive's repos must all be on one GitHub host.", ref, h, spokeHost), http.StatusBadRequest)
		return
	}
	// Resolve the org the repo lives in. The paste may be "org/repo", a bare
	// "repo" (org is the hive's configured org), or a full URL. Strip any host
	// and pull the org segment when present.
	org := s.deps.Config.Project.Org
	stripped := ref
	stripped = strings.TrimPrefix(stripped, "https://")
	stripped = strings.TrimPrefix(stripped, "http://")
	stripped = strings.Trim(stripped, "/")
	parts := strings.Split(stripped, "/")
	// Drop a leading host segment ("github.ibm.com/org/repo" -> "org/repo").
	if len(parts) > 1 && strings.Contains(parts[0], ".") {
		parts = parts[1:]
	}
	if len(parts) >= 2 && parts[0] != "" {
		org = parts[0]
	}
	if org == "" {
		jsonError(w, "cannot determine the org for this repo — use org/repo format", http.StatusBadRequest)
		return
	}

	// An App-less hive (advisory-only, no private key) cannot and need not probe
	// installation access: it never writes. Treat it as "no access check needed"
	// so the add proceeds — the single-host guard above already ran.
	if s.deps.GHAppAuth == nil || !s.deps.GHAppAuth.HasKey() {
		okResponse(w, map[string]string{"status": "no-app"})
		return
	}

	ctx := s.deps.Ctx
	if ctx == nil {
		ctx = r.Context()
	}
	if _, err := s.deps.GHAppAuth.DiscoverInstallationID(ctx, org); err != nil {
		// Not installed on this org (or discovery failed). Surface the correct
		// per-forge install URL and arm the existing pending-install mechanism so
		// the banner/recheck plumbing the App-required flow already uses drives
		// this to completion. AppInstallURL() returns "" for a GHE host whose App
		// slug was never configured — the UI renders that as a config message
		// rather than a dead link.
		installURL := ""
		if s.deps.Config != nil {
			installURL = s.deps.Config.GitHub.AppInstallURL()
		}
		s.SetPendingGitHubAppInstall()
		s.logger.Info("repo add blocked: app not installed on org", "org", org, "repo", ref, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":           false,
			"needsInstall": true,
			"org":          org,
			"installUrl":   installURL,
			"error":        fmt.Sprintf("the Hive GitHub App is not installed on %q, so it cannot read or write %s. Install/authorize the App for %q, then re-check.", org, ref, org),
		})
		return
	}
	okResponse(w, map[string]string{"status": "ok", "org": org})
}

func (s *Server) handleGitHubAppInstallClicked(w http.ResponseWriter, r *http.Request) {
	s.SetPendingGitHubAppInstall()
	s.deps.Logger.Info("github app install link clicked, flagging heartbeat")
	okResponse(w, map[string]string{"status": "pending"})
}

// --- GitHub config endpoint ---

type githubConfigUpdate struct {
	AppID          *int64
	InstallationID *int64
	KeyFile        string
	PrivateKey     string
}

type githubConfigUpdateError struct {
	message string
	code    int
}

func (e *githubConfigUpdateError) Error() string { return e.message }

func newGitHubConfigUpdateError(message string, code int) *githubConfigUpdateError {
	return &githubConfigUpdateError{message: message, code: code}
}

func (s *Server) handleConfigGitHub(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		AppID          *int64 `json:"app_id"`
		InstallationID *int64 `json:"installation_id"`
		KeyFile        string `json:"key_file"`
		PrivateKey     string `json:"private_key"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if body.AppID == nil && body.InstallationID == nil && body.KeyFile == "" && body.PrivateKey == "" {
		jsonError(w, "at least one field required: app_id, installation_id, key_file, private_key", http.StatusBadRequest)
		return
	}

	result, err := s.applyGitHubConfigUpdate(r, githubConfigUpdate{
		AppID:          body.AppID,
		InstallationID: body.InstallationID,
		KeyFile:        body.KeyFile,
		PrivateKey:     body.PrivateKey,
	})
	if err != nil {
		code := http.StatusInternalServerError
		if updateErr, ok := err.(*githubConfigUpdateError); ok {
			code = updateErr.code
		}
		jsonError(w, err.Error(), code)
		return
	}
	jsonResponse(w, result)
}

func (s *Server) applyGitHubConfigUpdate(r *http.Request, body githubConfigUpdate) (map[string]interface{}, error) {
	if s.deps == nil || s.deps.Config == nil {
		return nil, newGitHubConfigUpdateError("config not available", http.StatusInternalServerError)
	}

	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()

	cfg := s.deps.Config

	// saveConfig() silently no-ops when the config has no source path, so
	// without this guard the handler would report "status":"updated" for a
	// value that lives only in memory and is lost on the next restart (#2459).
	// Refuse up front, before mutating anything, and name the cause.
	if cfg.SourcePath == "" {
		s.logger.Error("github config update rejected: config has no source path, save would be an in-memory no-op")
		return nil, newGitHubConfigUpdateError("config not persisted: config has no source path, so the change would be lost on restart", http.StatusInternalServerError)
	}

	if body.PrivateKey != "" {
		keyPath := body.KeyFile
		if keyPath == "" {
			keyPath = "/data/gh-app-key.pem"
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
			s.logger.Warn("could not create key directory", "path", keyPath, "error", err)
			return nil, newGitHubConfigUpdateError(fmt.Sprintf("failed to create key directory: %v", err), http.StatusInternalServerError)
		}
		const keyFileMode = 0o600
		if err := os.WriteFile(keyPath, []byte(body.PrivateKey), keyFileMode); err != nil {
			return nil, newGitHubConfigUpdateError(fmt.Sprintf("failed to write key file: %v", err), http.StatusInternalServerError)
		}
		cfg.GitHub.KeyFile = keyPath
		s.logger.Info("github app private key written", "path", keyPath)
	} else if body.KeyFile != "" {
		cfg.GitHub.KeyFile = body.KeyFile
	}

	if body.AppID != nil {
		cfg.GitHub.AppID = *body.AppID
	}
	if body.InstallationID != nil {
		cfg.GitHub.InstallationID = *body.InstallationID
	}

	result, err := s.finishGitHubConfigUpdateLocked(requestAuditUser(r), "")
	if err != nil {
		if strings.Contains(err.Error(), "source path") {
			s.logger.Error("github config update rejected: config has no source path, save would be an in-memory no-op")
			return nil, newGitHubConfigUpdateError("config not persisted: config has no source path, so the change would be lost on restart", http.StatusInternalServerError)
		}
		s.logger.Error("failed to persist github config", "error", err)
		return nil, newGitHubConfigUpdateError("failed to save config", http.StatusInternalServerError)
	}
	return result, nil
}

func requestAuditUser(r *http.Request) string {
	user := r.Header.Get("X-Hive-User")
	if user == "" {
		return "local"
	}
	return user
}

// finishGitHubConfigUpdateLocked is the shared tail of the manual
// PUT /api/config/github flow and automatic installation-ID discovery. The
// caller must hold githubConfigMu and must have already mutated s.deps.Config.
func (s *Server) finishGitHubConfigUpdateLocked(auditUser, detail string) (map[string]interface{}, error) {
	cfg := s.deps.Config
	if cfg.SourcePath == "" {
		return nil, fmt.Errorf("config has no source path")
	}
	if err := s.saveConfig(); err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"status":          "updated",
		"app_id":          cfg.GitHub.AppID,
		"installation_id": cfg.GitHub.InstallationID,
		"key_file":        cfg.GitHub.KeyFile,
	}

	// Resolve the signing key the same way the boot and heartbeat-apply paths
	// do, NOT from the raw config value: on hosted spokes the key is
	// hub-delivered to the per-app-id path with key_file deliberately left
	// empty, and gating reinit on cfg.GitHub.KeyFile alone made Set ID save the
	// installation_id but never rebuild the client — banner never cleared,
	// Re-check dead-ended on a nil client (#2459).
	keyFile := cfg.GitHub.KeyFile
	if s.deps.ResolveAppKeyFileFunc != nil {
		keyFile = s.deps.ResolveAppKeyFileFunc(cfg.GitHub.KeyFile, cfg.GitHub.AppID)
	}

	// HasUsableApp() rejects the placeholder sentinel: reinitializing App auth
	// against it would fail on every save and, before this guard, could not
	// succeed no matter what installation_id the operator supplied.
	if cfg.GitHub.HasUsableApp() && keyFile != "" {
		if s.deps.ReinitGitHubFunc != nil {
			if err := s.deps.ReinitGitHubFunc(cfg.GitHub.AppID, cfg.GitHub.InstallationID, keyFile); err != nil {
				s.logger.Error("github client reinit failed", "error", err)
				result["reinit"] = "failed"
				result["reinit_error"] = err.Error()
			} else {
				result["reinit"] = "ok"
				s.SetGitHubAppRequired(false)
				s.ClearPendingGitHubAppInstall()
			}
		}
	}

	s.AuditLog(auditUser, "config_github", detail, "")
	s.refreshAndPersist()
	return result, nil
}

// AutoDiscoverGitHubInstallationID tries to fill an empty github.installation_id
// from the App-level GitHub API, then persists and reinitializes through the
// same path used by PUT /api/config/github. All failures are soft; callers keep
// showing the manual installation-ID flow.
func (s *Server) AutoDiscoverGitHubInstallationID(ctx context.Context, force bool) (int64, error) {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.GHAppAuth == nil || !s.deps.GHAppAuth.HasKey() {
		return 0, nil
	}
	cfg := s.deps.Config
	if cfg.GitHub.InstallationID != 0 {
		return 0, nil
	}
	org := configuredProjectOrgForGitHubApp(cfg, s.logger)
	if org == "" {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if force {
		s.deps.GHAppAuth.ForgetInstallationDiscovery(org)
	}

	id, err := s.deps.GHAppAuth.DiscoverInstallationID(ctx, org)
	if err != nil {
		s.logger.Warn("github app installation auto-discovery failed; manual installation ID entry remains available",
			"org", org, "error", err)
		return 0, err
	}
	if id == 0 {
		return 0, nil
	}

	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()
	if cfg.GitHub.InstallationID != 0 {
		return 0, nil
	}
	cfg.GitHub.InstallationID = id
	result, err := s.finishGitHubConfigUpdateLocked("system",
		auditDetail("source", "auto_discovery", "org", org, "installation_id", strconv.FormatInt(id, 10)))
	if err != nil {
		cfg.GitHub.InstallationID = 0
		s.logger.Warn("github app installation auto-discovered but could not be persisted; manual installation ID entry remains available",
			"org", org, "installation_id", id, "error", err)
		return 0, err
	}
	if result["reinit"] == "failed" {
		cfg.GitHub.InstallationID = 0
		if saveErr := s.saveConfig(); saveErr != nil {
			s.logger.Warn("github app installation auto-discovery reinit failed and rollback could not be persisted",
				"org", org, "installation_id", id, "reinit_error", result["reinit_error"], "rollback_error", saveErr)
		}
		return 0, fmt.Errorf("reinitializing github client after auto-discovery: %v", result["reinit_error"])
	}
	s.logger.Info("github app installation auto-discovered", "org", org, "installation_id", id)
	return id, nil
}

func (s *Server) handleGitHubAppSetupCallback(w http.ResponseWriter, r *http.Request) {
	redirect := func(flag string) {
		http.Redirect(w, r, "/?ghSetup="+flag, http.StatusSeeOther)
	}

	action := strings.TrimSpace(r.URL.Query().Get("setup_action"))
	switch action {
	case "request":
		s.logger.Info("github app setup callback recorded pending approval request")
		redirect("requested")
		return
	case "install", "update":
	default:
		s.logger.Warn("github app setup callback rejected: unsupported setup_action", "setup_action", action)
		redirect("error")
		return
	}

	rawID := strings.TrimSpace(r.URL.Query().Get("installation_id"))
	installationID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || installationID <= 0 {
		s.logger.Warn("github app setup callback rejected: invalid installation_id", "installation_id", rawID)
		redirect("error")
		return
	}

	if err := s.verifyGitHubSetupInstallation(r, installationID); err != nil {
		s.logger.Warn("github app setup callback rejected", "installation_id", installationID, "error", err)
		redirect("error")
		return
	}

	if err := s.persistGitHubSetupInstallation(r, installationID); err != nil {
		s.logger.Warn("github app setup callback failed to persist installation", "installation_id", installationID, "error", err)
		redirect("error")
		return
	}

	redirect("ok")
}

func (s *Server) verifyGitHubSetupInstallation(r *http.Request, installationID int64) error {
	if s.deps == nil || s.deps.Config == nil {
		return fmt.Errorf("config not available")
	}
	cfg := s.deps.Config
	if cfg.GitHub.InstallationID != 0 && cfg.GitHub.InstallationID != installationID && !s.requestHasGitHubSetupAdmin(r) {
		return fmt.Errorf("installation_id is already configured; authenticated admin required to replace it")
	}
	if !cfg.GitHub.HasApp() {
		return fmt.Errorf("github app_id is not configured")
	}
	keyFile := cfg.GitHub.KeyFile
	if s.deps.ResolveAppKeyFileFunc != nil {
		keyFile = s.deps.ResolveAppKeyFileFunc(cfg.GitHub.KeyFile, cfg.GitHub.AppID)
	}
	if strings.TrimSpace(keyFile) == "" {
		return fmt.Errorf("github app key file is not configured")
	}
	auth, err := github.NewAppAuth(cfg.GitHub.AppID, installationID, keyFile, s.logger, cfg.GitHub.ResolvedAPIURL())
	if err != nil {
		return err
	}
	ctx := r.Context()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	// /gh-setup is public because GitHub redirects a fresh browser here without
	// a hive session. We still do not trust the query string: the App JWT call
	// below proves the ID exists for this App and that its account is exactly
	// this hive's configured org, preventing installation hijacking.
	return auth.VerifyInstallationForOrg(ctx, installationID, configuredProjectOrgForGitHubApp(cfg, s.logger))
}

func (s *Server) persistGitHubSetupInstallation(r *http.Request, installationID int64) error {
	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()

	cfg := s.deps.Config
	if cfg.GitHub.InstallationID != 0 && cfg.GitHub.InstallationID != installationID && !s.requestHasGitHubSetupAdmin(r) {
		return fmt.Errorf("installation_id is already configured; authenticated admin required to replace it")
	}
	cfg.GitHub.InstallationID = installationID
	result, err := s.finishGitHubConfigUpdateLocked(requestAuditUser(r),
		auditDetail("source", "setup_url", "installation_id", strconv.FormatInt(installationID, 10)))
	if err != nil {
		return err
	}
	if result["reinit"] == "failed" {
		return fmt.Errorf("reinitializing github client after setup callback: %v", result["reinit_error"])
	}
	return nil
}

func (s *Server) requestHasGitHubSetupAdmin(r *http.Request) bool {
	if sess := s.sessionFromRequest(r); sess != nil {
		// Authz decision: use the LIVE allowlist role (session_live_role.go),
		// never the role frozen into the session at login — a downgrade or
		// revocation must strip GitHub-setup admin immediately, and a granted
		// owner must gain it without re-login (same class as #4299).
		live, ok := s.liveSessionRole(sess)
		return ok && config.RoleAtLeast(live, config.RoleReadWrite)
	}
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		return false
	}
	if s.directRouteAuthzEnabled() {
		return false
	}
	proof := r.Header.Get(proxyAuthHeader)
	return s.authToken != "" && proof != "" && secureCompare(proof, s.authToken)
}

// --- Sidebar endpoints ---

func (s *Server) handleSidebarGet(w http.ResponseWriter, r *http.Request) {
	s.sidebarMu.RLock()
	sb := s.sidebar
	s.sidebarMu.RUnlock()
	if sb == nil {
		jsonResponse(w, map[string]interface{}{"sidebar": nil})
		return
	}
	jsonResponse(w, map[string]interface{}{"sidebar": sb})
}

func (s *Server) handleSidebarSet(w http.ResponseWriter, r *http.Request) {
	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.sidebarMu.Lock()
	s.sidebar = body
	s.sidebarMu.Unlock()

	s.auditFromRequest(r, "config_sidebar", "", "")
	s.saveSidebarToDisk(body)
	okResponse(w, map[string]string{"status": "updated"})
}

const sidebarFile = "/data/sidebar.json"

func (s *Server) loadSidebarFromDisk() {
	data, err := os.ReadFile(sidebarFile)
	if err != nil {
		return
	}
	var sb interface{}
	if json.Unmarshal(data, &sb) == nil {
		s.sidebarMu.Lock()
		s.sidebar = sb
		s.sidebarMu.Unlock()
	}
}

func (s *Server) saveSidebarToDisk(sb interface{}) {
	data, err := json.Marshal(sb)
	if err != nil {
		return
	}
	tmpSidebar := sidebarFile + ".tmp"
	if os.WriteFile(tmpSidebar, data, 0o644) == nil {
		_ = os.Rename(tmpSidebar, sidebarFile)
	}
}

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	vllmModels := s.queryInferenceModels("vllm")
	llmdModels := s.queryInferenceModels("llm-d")
	litellmModels := s.queryInferenceModels("litellm")

	// CLI backends each have a DIFFERENT discovery source (see cli_models.go):
	// provider HTTP APIs, vendor CLI protocols/subcommands, or a deliberately
	// authoritative single option. Every probe is best-effort and falls back to
	// a current static list, so a dropdown is never empty.
	claudeCLI := s.queryCLIModels("claude")
	copilotCLI := s.queryCLIModels("copilot")
	geminiCLI := s.queryCLIModels("gemini")
	gooseCLI := s.queryCLIModels("goose")
	agyCLI := s.queryCLIModels(agyBackendID)
	// bob has no discovery source and no usable --model flag: it selects its
	// own model. Served explicitly so the client never falls through to the
	// copilot catalog and offers models bob cannot honor (see bobStaticModels).
	bobCLI := s.queryCLIModels(bobBackendID)

	jsonResponse(w, []map[string]interface{}{
		{"id": "claude", "name": "Claude Code", "models": claudeCLI.models, "fallback": claudeCLI.fallback},
		{"id": "copilot", "name": "GitHub Copilot", "models": copilotCLI.models, "fallback": copilotCLI.fallback},
		{"id": bobBackendID, "name": "bob (IBM bobshell)", "models": bobCLI.models, "fallback": bobCLI.fallback},
		{"id": "gemini", "name": "Gemini", "models": geminiCLI.models, "fallback": geminiCLI.fallback},
		{"id": "goose", "name": "Goose", "models": gooseCLI.models, "fallback": gooseCLI.fallback},
		{"id": agyBackendID, "name": "Google Antigravity (agy)", "models": agyCLI.models, "fallback": agyCLI.fallback},
		{"id": "vllm", "name": "vLLM (self-hosted)", "models": vllmModels, "inference": true},
		{"id": "llm-d", "name": "llm-d (self-hosted)", "models": llmdModels, "inference": true},
		{"id": "litellm", "name": "LiteLLM (proxy)", "models": litellmModels, "inference": true},
	})
}

func (s *Server) handleInferenceModels(w http.ResponseWriter, r *http.Request) {
	backend := r.PathValue("backend")
	if backend == "" {
		jsonError(w, "backend required", http.StatusBadRequest)
		return
	}
	endpoints, ok := s.getInferenceEndpoints(backend)
	if !ok {
		jsonError(w, "unknown inference backend: "+backend, http.StatusNotFound)
		return
	}

	models, complete := s.fetchInferenceModelsForBackendDetailed(backend, endpoints)
	fallback := false
	if len(models) == 0 {
		s.logger.Warn("no models found from any endpoint", "backend", backend, "endpoints", len(endpoints))
		// Discovery is authoritative (a LiteLLM gateway entitlement-filters
		// /v1/models per API key); only when it fails do we fall back to
		// the common static aliases, flagged so the UI can mark them as
		// unverified against the configured endpoint/key.
		models = inferenceStaticModelAliases
		fallback = true
	}

	resp := map[string]interface{}{
		"backend":  backend,
		"models":   models,
		"fallback": fallback,
		// partial: some of the backend's endpoints answered and others did
		// not, so `models` is a FLOOR, not a census — every id in it really
		// was discovered (so the dropdown labels them normally, unlike the
		// static fallback), but a model's absence proves nothing. Auto-heal
		// must sit out this sample rather than switch an agent off a model
		// that only the unreachable endpoint serves (#4438).
		"partial": !fallback && !complete,
	}
	// Some LiteLLM gateways advertise the FULL catalog on /v1/models but scope
	// a key's team to a SUBSET; a non-entitled model 403s at inference. When
	// the proxy has learned the entitled set (key-info probe or a "team not
	// allowed" 403), narrow the returned list to it so the dropdown offers
	// only usable models. Not applied to the static fallback (unverified).
	if !fallback {
		if entitled, source, ok := s.entitledModelsFor(backend, endpoints); ok {
			usable := intersectEntitled(models, entitled)
			if len(usable) > 0 {
				resp["models"] = usable
				resp["entitled"] = true
				resp["entitledSource"] = source
			}
		}
	}
	jsonResponse(w, resp)
}

// entitledModelsFor returns the entitled model set the proxy has learned for a
// LiteLLM backend's endpoint, if any. Only litellm gateways entitlement-filter
// per key; vllm/llm-d return no entitlement signal.
func (s *Server) entitledModelsFor(backend string, endpoints []string) (models []string, source string, known bool) {
	if backend != "litellm" {
		return nil, "", false
	}
	fn := getEntitledModelsFn()
	if fn == nil {
		return nil, "", false
	}
	for _, ep := range endpoints {
		if m, src, ok := fn(ep); ok {
			return m, src, true
		}
	}
	return nil, "", false
}

// intersectEntitled returns the discovered models that are also in the entitled
// set, preserving discovery order. Matching tolerates a missing/extra provider
// prefix (stored "gpt-4o" vs entitled "Azure/gpt-4o") by comparing the id tail
// after the last slash when the full ids differ.
func intersectEntitled(discovered, entitled []string) []string {
	full := make(map[string]bool, len(entitled))
	tail := make(map[string]bool, len(entitled))
	for _, e := range entitled {
		full[e] = true
		tail[e[strings.LastIndex(e, "/")+1:]] = true
	}
	out := make([]string, 0, len(discovered))
	for _, d := range discovered {
		if full[d] || tail[d[strings.LastIndex(d, "/")+1:]] {
			out = append(out, d)
		}
	}
	return out
}

// fetchInferenceModelsForBackend discovers a backend's models, honouring the
// AUTH SCHEME of the gateway that backend resolves to. The plain path sends the
// resolved key as a raw bearer, which is correct for litellm/vllm/llm-d — but a
// watsonx gateway needs an IAM-minted bearer plus the X-IBM-Project-ID header,
// so sending the raw IBM Cloud key returns 401 and the dropdown silently fell
// back to unrelated static aliases. Reuses the same gatewayProbeAuth +
// fetchModelsWithHeaders pair the Model Gateways tab's discover/test paths use,
// so the agent's model dropdown and the gateway form agree on what is available.
// Falls back to the legacy raw-key path whenever no gateway resolves for this
// name (env-configured vllm/llm-d endpoints), keeping existing behaviour.
func (s *Server) fetchInferenceModelsForBackend(backend string, endpoints []string) []string {
	models, _ := s.fetchInferenceModelsForBackendDetailed(backend, endpoints)
	return models
}

// fetchInferenceModelsForBackendDetailed additionally reports whether EVERY
// endpoint answered, so a caller can tell a complete census from a partial one
// (#4438). It matters most here: this is the discovery the dashboard's model
// auto-heal reads, and auto-heal does not merely toast — it rewrites the
// agent's configured model and relaunches its session. A gateway that drops
// out of a multi-endpoint sweep must never be able to spend an agent's
// selection that way.
func (s *Server) fetchInferenceModelsForBackendDetailed(backend string, endpoints []string) ([]string, bool) {
	gw := s.resolveGatewayForBackend(backend)
	if gw == nil || !gatewayKindNeedsProbeAuth(gw.Kind) {
		return fetchModelsFromEndpointsDetailed(endpoints, s.inferenceAPIKey(backend))
	}
	bearer, headers, err := s.gatewayProbeAuth(gw.Kind, gw.ResolveAPIKey(), gw.ProjectID)
	if err != nil {
		s.logger.Warn("gateway auth for model discovery failed",
			"backend", backend, "kind", gw.Kind, "error", err)
		return nil, false
	}
	seen := make(map[string]bool)
	var all []string
	complete := true
	for _, ep := range endpoints {
		models, err := fetchModelsWithHeaders(ep, bearer, headers)
		if err != nil {
			s.logger.Warn("model discovery failed",
				"backend", backend, "kind", gw.Kind, "endpoint", ep, "error", err)
			complete = false
			continue
		}
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				all = append(all, m)
			}
		}
	}
	return all, complete
}

// gatewayKindNeedsProbeAuth reports whether a gateway kind requires the
// gatewayProbeAuth path (a minted bearer and/or extra headers) rather than the
// raw key. Only watsonx does today; keeping it a predicate means a future kind
// with its own auth scheme is a one-line change here rather than a new branch in
// every discovery caller.
func gatewayKindNeedsProbeAuth(kind string) bool {
	return kind == config.GatewayKindWatsonx
}

// resolveGatewayForBackend finds the configured gateway an agent backend routes
// through. Agent backends name a gateway directly (registerGatewayEndpoints
// keys the endpoint registry by gateway NAME), and the built-in gateway methods
// share their name with their kind, so match on either.
func (s *Server) resolveGatewayForBackend(backend string) *config.GatewayConfig {
	if s.deps == nil || s.deps.Config == nil {
		return nil
	}
	for _, gw := range s.deps.Config.Governor.ResolvedGateways() {
		if strings.EqualFold(gw.Name, backend) || strings.EqualFold(gw.Kind, backend) {
			out := gw
			return &out
		}
	}
	return nil
}

// inferenceAPIKey returns the bearer key used for a backend's model
// discovery requests. litellm requires auth on /v1/models. vllm/llm-d are
// usually unauthenticated, but the configured endpoint may in fact be a
// LiteLLM gateway, which entitlement-filters /v1/models and hides
// key-gated models from anonymous callers — so resolve a backend-specific
// key first (governor.vllm / governor.llm-d api_key_env|api_key_file, or
// the HIVE_VLLM_API_KEY / HIVE_LLMD_API_KEY defaults), then fall back to
// the litellm key. A plain vLLM/llm-d server without --api-key ignores the
// Authorization header, so sending a key is harmless there.
func (s *Server) inferenceAPIKey(backend string) string {
	if s.deps == nil || s.deps.Config == nil {
		return ""
	}
	gov := &s.deps.Config.Governor
	switch backend {
	case "vllm":
		if key := gov.VLLM.ResolveAPIKey(config.DefaultVLLMAPIKeyEnv); key != "" {
			return key
		}
	case "llm-d":
		if key := gov.LLMD.ResolveAPIKey(config.DefaultLLMDAPIKeyEnv); key != "" {
			return key
		}
	}
	// Same contract as inference-time forwarding: an explicit gateway for
	// this backend wins over the legacy litellm: key store (see
	// ResolveLiteLLMInferenceKey). A key rotated via the Model Gateways tab
	// lands only in the gateway's key file, so resolving the legacy file here
	// made discovery send the revoked key and log "no models found from any
	// endpoint" (surfaced as a sticky error toast) while the save-time probe
	// — which uses the submitted key — reported the models fine.
	//
	// A matching gateway that resolves NO key of its own still falls back to
	// the legacy store: keyless gateway entries (e.g. an endpoint-only entry
	// with the key kept in the classic litellm: block) predate per-gateway
	// keys and must keep discovering with the legacy key.
	if key := gov.ResolveLiteLLMInferenceKey(backend); key != "" {
		return key
	}
	return gov.LiteLLM.ResolveAPIKey()
}

// inferenceStaticModelAliases is the FALLBACK model list for the inference
// model dropdowns (vllm, llm-d, litellm), offered only when runtime
// /v1/models discovery against the backend's configured endpoint fails
// (and no HIVE_*_MODELS env override is set), so a dropdown is never
// empty. Discovery is authoritative: a LiteLLM gateway entitlement-filters
// /v1/models per API key, so when discovery succeeds we show ONLY the
// discovered set — appending these aliases on top would offer unlicensed
// models that just 403 at runtime. The entries use the LiteLLM gateway
// naming convention observed live: UNDERSCORES between words and a DOT in
// the version (claude_opus_4.8) — unlike the Claude CLI (all hyphens,
// claude-opus-4-8) and the Copilot/Gemini backends (hyphenated words with
// a dotted version, claude-opus-4.8 / gemini-2.5-pro). Model set derived
// from the CLAUDE_CLI_MODELS/COPILOT_CLI_MODELS lists in static/index.html
// and the gemini backend list in handleBackends; keep in sync when those
// change.
var inferenceStaticModelAliases = []string{
	"claude_opus_4.8",
	"claude_opus_4.7",
	"claude_opus_4.6",
	"claude_sonnet_4.6",
	"claude_haiku_4.5",
	"gemini_2.5_pro",
	"gemini_2.5_flash",
}

func (s *Server) queryInferenceModels(backend string) []string {
	models, _ := s.queryInferenceModelsDetailed(backend)
	return models
}

// queryInferenceModelsDetailed additionally reports whether the returned list
// is NON-AUTHORITATIVE — a stand-in for a census that did not fully succeed —
// rather than a complete live-discovered or operator-configured set. Callers
// surfacing model add/remove events must ignore diffs against such a list.
//
// Three shapes are non-authoritative, and all three produced the same false
// "Model removed" storm before they were flagged:
//   - the static aliases, when discovery failed outright (#4426);
//   - a PARTIAL sweep, when some endpoints answered and others did not;
//   - the HIVE_*_MODELS env list, when it is reached only BECAUSE a
//     registered endpoint failed to answer (#4438).
//
// The env list is authoritative in the one case where it is the configured
// source of models rather than a consolation prize: no endpoint registered at
// all, so nothing was ever probed.
func (s *Server) queryInferenceModelsDetailed(backend string) ([]string, bool) {
	endpoints, ok := s.getInferenceEndpoints(backend)
	probed := ok && len(endpoints) > 0
	if probed {
		models, complete := fetchModelsFromEndpointsDetailed(endpoints, s.inferenceAPIKey(backend))
		if len(models) > 0 {
			// Show a partial sweep — a short dropdown beats an empty one —
			// but never let it stand as a census of what exists.
			return models, !complete
		}
	}
	envVar := "HIVE_VLLM_MODELS"
	switch backend {
	case "llm-d":
		envVar = "HIVE_LLMD_MODELS"
	case "litellm":
		envVar = "HIVE_LITELLM_MODELS"
	}
	if val := os.Getenv(envVar); val != "" {
		return inferenceModelsFromEnv(envVar, ""), probed
	}
	// Discovery failed or the backend is unconfigured — fall back to the
	// common static aliases (unverified against any endpoint/key).
	return inferenceStaticModelAliases, true
}

const inferenceModelQueryTimeout = 5 * time.Second

// fetchModelsFromEndpoints queries /v1/models on each endpoint and returns
// a deduplicated, combined list of all model IDs found. apiKey is optional
// (litellm requires bearer auth; vllm/llm-d do not).
func fetchModelsFromEndpoints(endpoints []string, apiKey string) []string {
	models, _ := fetchModelsFromEndpointsDetailed(endpoints, apiKey)
	return models
}

// fetchModelsFromEndpointsDetailed additionally reports whether EVERY endpoint
// answered. A PARTIAL sweep — one gateway of several timing out or answering
// 403 while its siblings reply — still returns a non-empty list, and a caller
// that diffs model sets must not read the survivors as "the unreachable
// endpoint's models were removed" (#4438: a partial sweep wallpapered the
// dashboard with false "Model removed from litellm: …" toasts, including for
// the very model the hive's agents were configured to use).
func fetchModelsFromEndpointsDetailed(endpoints []string, apiKey string) ([]string, bool) {
	seen := make(map[string]bool)
	var all []string
	complete := true
	for _, ep := range endpoints {
		models, err := fetchModelsFromEndpoint(ep, apiKey)
		if err != nil {
			complete = false
			continue
		}
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				all = append(all, m)
			}
		}
	}
	return all, complete
}

func fetchModelsFromEndpoint(baseURL, apiKey string) ([]string, error) {
	return fetchModelsWithHeaders(baseURL, apiKey, nil)
}

// fetchModelsWithHeaders is fetchModelsFromEndpoint with optional extra request
// headers (e.g. watsonx's X-IBM-Project-ID). apiKey, when non-empty, is sent as
// the Bearer; for watsonx the caller passes the minted IAM token as apiKey.
func fetchModelsWithHeaders(baseURL, apiKey string, extraHeaders map[string]string) ([]string, error) {
	modelsURL := strings.TrimRight(baseURL, "/") + "/v1/models"
	client := &http.Client{
		Timeout: inferenceModelQueryTimeout,
		// SECURITY (audit F13): Go's default redirect policy PRESERVES the
		// Authorization header across same-host hops and follows up to 10
		// redirects. An upstream that answers /v1/models with a 302 could
		// therefore walk the gateway key onward. Strip credentials on any hop
		// that changes host, so a redirect can never carry the key somewhere the
		// original request was not authorized to reach. probeModelsWithHeaders
		// already used this policy; this sibling was simply missed.
		CheckRedirect: noRedirectToPrivate,
	}
	req, err := http.NewRequest("GET", modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range extraHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer closeHTTPBody(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	models, err := parseModelsResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return models, nil
}

// parseModelsResponse decodes an OpenAI-style GET /v1/models body into the
// list of model IDs. It is the single source of truth shared by the
// model-discovery dropdown (fetchModelsFromEndpoint) and the Test Connection
// probe (probeLiteLLMModels), so both agree on what "N models" means.
//
// It is deliberately lenient: any JSON object with a "data" array whose items
// carry a non-empty "id" string is accepted. It does NOT require top-level
// object=="list" (gateways may omit or reorder it), imposes no field order,
// and tolerates extra/unknown per-item fields (object, created, owned_by, …).
// IDs are returned verbatim, so provider-prefixed / hyphenated ids like
// "Azure/gpt-5.1-codex-2025-11-13" or "claude-opus-4-7" survive intact.
func parseModelsResponse(r io.Reader) ([]string, error) {
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(r).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, m := range result.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

func inferenceModelsFromEnv(envVar, defaultModel string) []string {
	val := os.Getenv(envVar)
	if val == "" {
		return []string{defaultModel}
	}
	models := strings.Split(val, ",")
	for i := range models {
		models[i] = strings.TrimSpace(models[i])
	}
	return models
}

// --- Knowledge endpoints ---

func (s *Server) handleKnowledgeToggle(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Config.Knowledge.Enabled = body.Enabled

	if body.Enabled && s.deps.Knowledge == nil {
		layers := make([]knowledge.LayerConfig, len(s.deps.Config.Knowledge.Layers))
		for i, l := range s.deps.Config.Knowledge.Layers {
			layers[i] = knowledge.LayerConfig{Type: knowledge.LayerType(l.Type), Path: l.Path, URL: l.URL, Shared: l.Shared}
		}
		kcfg := knowledge.KnowledgeConfig{
			Enabled: true,
			Layers:  layers,
			Primer: knowledge.PrimerConfig{
				MaxFacts:      s.deps.Config.Knowledge.Primer.MaxFacts,
				MergeStrategy: s.deps.Config.Knowledge.Primer.MergeStrategy,
			},
		}
		api := knowledge.NewKnowledgeAPI(layers, kcfg, s.deps.Logger)
		s.deps.Knowledge = api
	} else if !body.Enabled {
		s.deps.Knowledge = nil
	}

	if s.deps.BeadSynthesizer != nil {
		if body.Enabled && s.deps.Config.Knowledge.BeadSynthesizer.IsEnabled() {
			s.deps.BeadSynthesizer.StartBackground(s.deps.Ctx)
			s.logger.Info("bead synthesizer started via knowledge toggle")
		} else if !body.Enabled {
			s.deps.BeadSynthesizer.Stop()
			s.logger.Info("bead synthesizer stopped via knowledge toggle")
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after knowledge toggle", "error", err)
	}
	s.auditFromRequest(r, "knowledge_toggle", auditDetail("enabled", fmt.Sprintf("%v", body.Enabled)), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "enabled": fmt.Sprintf("%v", body.Enabled)})
}

func (s *Server) handleBeadSynthStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.Knowledge.BeadSynthesizer
	running := false
	if s.deps.BeadSynthesizer != nil {
		running = s.deps.BeadSynthesizer.IsRunning()
	}
	jsonResponse(w, map[string]interface{}{
		"enabled":             cfg.IsEnabled(),
		"running":             running,
		"schedule":            cfg.Schedule,
		"min_confidence":      cfg.MinConfidence,
		"target_layer":        cfg.TargetLayer,
		"max_facts_per_cycle": cfg.MaxFactsPerCycle,
		"vault_path":          cfg.VaultPath,
	})
}

func (s *Server) handleBeadSynthToggle(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	enabled := body.Enabled
	s.deps.Config.Knowledge.BeadSynthesizer.Enabled = &enabled

	if s.deps.BeadSynthesizer != nil {
		if enabled {
			s.deps.BeadSynthesizer.StartBackground(s.deps.Ctx)
			s.logger.Info("bead synthesizer enabled via dashboard")
		} else {
			s.deps.BeadSynthesizer.Stop()
			s.logger.Info("bead synthesizer disabled via dashboard")
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after bead-synth toggle", "error", err)
	}
	s.auditFromRequest(r, "bead_synth_toggle", auditDetail("enabled", fmt.Sprintf("%v", enabled)), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "bead_synthesizer_enabled": fmt.Sprintf("%v", enabled)})
}

func (s *Server) handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "facts": []interface{}{}})
		return
	}

	typeFilter := r.URL.Query().Get("type")
	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", typeFilter, 0)
	jsonResponse(w, map[string]interface{}{
		"enabled": true,
		"count":   len(facts),
		"facts":   facts,
	})
}

// titleCaseWords capitalizes the first letter following each run of
// whitespace, replicating the exact word-boundary rule of the deprecated
// strings.Title (boundary = unicode.IsSpace, not "non-letter" — so
// underscore-joined identifiers like "test_scaffold" are left as
// "Test_scaffold", matching prior output byte-for-byte). It exists only as a
// dependency-free fallback for a fact type absent from typeLabels;
// golang.org/x/text/cases is not a module dependency here.
func titleCaseWords(s string) string {
	prevSpace := true
	return strings.Map(func(r rune) rune {
		if prevSpace && unicode.IsLetter(r) {
			r = unicode.ToTitle(r)
		}
		prevSpace = unicode.IsSpace(r)
		return r
	}, s)
}

func (s *Server) handleKnowledgeExport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte("# Agent Knowledge\n\nKnowledge base not available.\n"))
		return
	}

	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0)

	grouped := make(map[string][]knowledge.Fact)
	for _, f := range facts {
		t := string(f.Type)
		if t == "" {
			t = "general"
		}
		grouped[t] = append(grouped[t], f)
	}

	typeLabels := map[string]string{
		"pattern":       "Patterns",
		"gotcha":        "Gotchas",
		"decision":      "Decisions",
		"regression":    "Regressions",
		"test_scaffold": "Test Scaffolds",
		"integration":   "Integration",
		"coverage_rule": "Coverage Rules",
		"idea":          "Ideas",
		"vision":        "Vision",
		"constitution":  "Constitution",
		"requirement":   "Requirements",
		"constraint":    "Constraints",
		"stakeholder":   "Stakeholders",
		"general":       "General",
	}

	var sb strings.Builder
	sb.WriteString("# Agent Knowledge\n\n")
	sb.WriteString("This file is auto-generated from the hive knowledge base.\n")
	sb.WriteString("It refreshes periodically — do not edit manually.\n\n")

	order := []string{"constitution", "constraint", "requirement", "decision",
		"pattern", "gotcha", "regression", "coverage_rule", "test_scaffold",
		"integration", "idea", "vision", "stakeholder", "general"}

	for _, t := range order {
		ff, ok := grouped[t]
		if !ok || len(ff) == 0 {
			continue
		}
		// Facts within a type group arrive aggregated across layers, vaults, and
		// git sources, so their relative order is not stable between fetches
		// (see #3090). Sort by the natural identifier (Slug, then Title, then
		// Body) so identical knowledge always serializes byte-for-byte the same
		// and unchanged data does not rewrite downstream files on every refresh.
		sortFactsStable(ff)
		label := typeLabels[t]
		if label == "" {
			label = titleCaseWords(t)
		}
		sb.WriteString("## " + label + "\n\n")
		for _, f := range ff {
			sb.WriteString("### " + f.Title + "\n\n")
			if f.Body != "" {
				sb.WriteString(f.Body + "\n\n")
			}
			if len(f.Tags) > 0 {
				sb.WriteString("Tags: " + strings.Join(f.Tags, ", ") + "\n\n")
			}
		}
	}

	body := sb.String()
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// ETag over the rendered body (not just the fact count) so consumers can
	// skip a rewrite whenever the content is unchanged, and always see a new
	// tag when it genuinely changes.
	sum := sha256.Sum256([]byte(body))
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sum))
	_, _ = w.Write([]byte(body))
}

// sortFactsStable orders facts by their natural identifier so an unchanged
// knowledge base serializes to byte-identical output regardless of the order
// facts were aggregated across sources. Slug is the primary key (unique per
// fact); Title and Body are tiebreakers for facts that lack a slug.
func sortFactsStable(facts []knowledge.Fact) {
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].Slug != facts[j].Slug {
			return facts[i].Slug < facts[j].Slug
		}
		if facts[i].Title != facts[j].Title {
			return facts[i].Title < facts[j].Title
		}
		return facts[i].Body < facts[j].Body
	})
}

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"results": []interface{}{}})
		return
	}

	query := r.URL.Query().Get("q")
	tagFilter := r.URL.Query().Get("tag")
	if query == "" && tagFilter == "" {
		jsonError(w, "q or tag parameter is required", http.StatusBadRequest)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	limitStr := r.URL.Query().Get("limit")
	limit := 0
	if limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	var results []knowledge.Fact
	if tagFilter != "" {
		all := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", typeFilter, 0)
		for _, f := range all {
			for _, t := range f.Tags {
				if strings.EqualFold(t, tagFilter) {
					results = append(results, f)
					break
				}
			}
		}
		if limit > 0 && len(results) > limit {
			results = results[:limit]
		}
	} else {
		results = s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, query, typeFilter, limit)
	}

	jsonResponse(w, map[string]interface{}{
		"query":   query,
		"tag":     tagFilter,
		"count":   len(results),
		"results": results,
	})
}

func (s *Server) handleKnowledgeHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Health(s.deps.Ctx))
}

func (s *Server) handleKnowledgeStats(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	stats := s.deps.Knowledge.Stats(s.deps.Ctx)
	stats["vaults"] = s.deps.Knowledge.Vaults()
	stats["git_sources"] = s.deps.Knowledge.GitSources()

	if layers, ok := stats["layers"].([]map[string]interface{}); ok {
		total := 0
		for _, m := range layers {
			if p, ok := m["total_pages"].(int); ok {
				total += p
			}
		}
		s.AppendFactHistory(total)
	}

	// Sample the estimated cost on the same cadence as the fact history so the
	// cost sparkline shares the fact sparkline's timer (both throttled to
	// ~5-min intervals). The figure is the same all-time cumulative estimated
	// total that GET /api/cost returns; the per-agent map feeds the agent
	// cards' spend-over-window display and the per-model map feeds the cost
	// table's mini sparklines (unpriced models included — tokens still trend).
	est := s.estimatedCost()
	perAgent := make(map[string]float64, len(est.ByAgent))
	for _, row := range est.ByAgent {
		if row.USD > 0 {
			perAgent[row.Name] = row.USD
		}
	}
	perModel := make(map[string]CostModelSnap, len(est.ByModel))
	for _, row := range est.ByModel {
		if row.Input > 0 || row.Output > 0 || row.USD > 0 {
			perModel[row.Name] = CostModelSnap{Input: row.Input, Output: row.Output, USD: row.USD}
		}
	}
	s.AppendCostHistoryFull(est.TotalUSD, perAgent, perModel)

	jsonResponse(w, stats)
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

// handleBudgetHistory serves the per-budget-window report (#4298): one row per
// CLOSED window, newest first, plus the window still open so an operator sees
// the whole picture in one response rather than joining two endpoints.
//
// `windows` is ALWAYS an array, never null — a hive that has not yet seen a
// roll returns `[]`, which a client renders as "no history yet" rather than
// crashing on a nil. That is the compatibility requirement #4298 names: new
// code must not break an environment that was never keeping this history.
func (s *Server) handleBudgetHistory(w http.ResponseWriter, r *http.Request) {
	windows := s.BudgetWindowHistory()
	if windows == nil {
		windows = []collect.BudgetWindowEntry{}
	}

	resp := map[string]any{"windows": windows}

	// The open window, straight from the live status, so the report reads
	// continuously from "now" back through the closed rows.
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status != nil {
		b := status.Budget
		current := map[string]any{
			"limit":     b.WeeklyBudget,
			"used":      b.Used,
			"pctUsed":   b.PctUsed,
			"exhausted": b.Exhausted,
		}
		if b.WindowStartsAt != "" {
			current["windowStart"] = b.WindowStartsAt
		}
		if b.WindowEndsAt != "" {
			current["windowEnd"] = b.WindowEndsAt
		}
		resp["current"] = current
	}

	jsonResponse(w, resp)
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

func (s *Server) handleKnowledgeGraph(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, knowledge.GraphData{Nodes: []knowledge.GraphNode{}, Edges: []knowledge.GraphEdge{}})
		return
	}
	gs := s.deps.Knowledge.GraphStore()
	if gs == nil {
		jsonResponse(w, knowledge.GraphData{Nodes: []knowledge.GraphNode{}, Edges: []knowledge.GraphEdge{}})
		return
	}
	rootSlug := r.URL.Query().Get("root")
	const defaultGraphDepth = 2
	depth := defaultGraphDepth
	if d := r.URL.Query().Get("depth"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v > 0 {
			depth = v
		}
	}
	stores := s.deps.Knowledge.FileStores()
	data := gs.BuildGraphData(stores, rootSlug, depth)
	jsonResponse(w, data)
}

func (s *Server) handleKnowledgeLayer(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "facts": []interface{}{}})
		return
	}

	layer := r.PathValue("layer")
	typeFilter := r.URL.Query().Get("type")

	const gitSourcePrefix = "git_source:"
	if strings.HasPrefix(layer, gitSourcePrefix) {
		name := strings.TrimPrefix(layer, gitSourcePrefix)
		store := s.deps.Knowledge.GetGitSourceStore(name)
		if store == nil {
			jsonResponse(w, map[string]interface{}{"layer": layer, "count": 0, "facts": []interface{}{}})
			return
		}
		facts := store.ListPages(typeFilter)
		for i := range facts {
			facts[i].Layer = knowledge.LayerType(layer)
		}
		jsonResponse(w, map[string]interface{}{
			"layer": layer,
			"count": len(facts),
			"facts": facts,
		})
		return
	}

	knowledgeLayer := knowledge.LayerType(layer)
	facts := s.deps.Knowledge.LayerFacts(s.deps.Ctx, knowledgeLayer, typeFilter)
	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, map[string]interface{}{
		"layer": layer,
		"count": len(facts),
		"facts": facts,
	})
}

func (s *Server) handleKnowledgeFact(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusNotFound)
		return
	}

	slug := r.PathValue("slug")
	fact, err := s.deps.Knowledge.ReadFact(s.deps.Ctx, slug)
	if err != nil || fact == nil {
		jsonError(w, "fact not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, fact)
}

func (s *Server) handleKnowledgeCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.CreateFactRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Title == "" || req.Body == "" {
		jsonError(w, "title and body are required", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}
	if req.Type == "" {
		req.Type = "pattern"
	}
	const defaultConfidence = 0.7
	if req.Confidence <= 0 {
		req.Confidence = defaultConfidence
	}

	if err := s.deps.Knowledge.CreateFact(s.deps.Ctx, req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_create_fact", auditDetail("title", req.Title, "layer", req.Layer), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "title": req.Title, "layer": req.Layer})
}

func (s *Server) handleKnowledgeUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	layer := r.PathValue("layer")
	slug := r.PathValue("slug")

	var req knowledge.UpdateFactRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.UpdateFact(s.deps.Ctx, knowledge.LayerType(layer), slug, req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_update_fact", auditDetail("slug", slug, "layer", layer), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "slug": slug, "layer": layer})
}

func (s *Server) handleKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	layer := r.PathValue("layer")
	slug := r.PathValue("slug")

	if err := s.deps.Knowledge.DeleteFact(s.deps.Ctx, knowledge.LayerType(layer), slug); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_delete_fact", auditDetail("slug", slug, "layer", layer), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "deleted": slug})
}

func (s *Server) handleKnowledgePromote(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.PromoteRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Slug == "" || req.FromLayer == "" || req.ToLayer == "" {
		jsonError(w, "slug, from_layer, and to_layer are required", http.StatusBadRequest)
		return
	}
	if req.Promoter == "" {
		req.Promoter = "dashboard"
	}

	result := s.deps.Knowledge.PromoteFact(s.deps.Ctx, req)
	if !result.Success {
		fact, err := s.deps.Knowledge.VaultFact(req.Slug)
		if err != nil || fact == nil {
			jsonError(w, result.Error, http.StatusBadRequest)
			return
		}
		syncReq := knowledge.ObsidianSyncRequest{
			Filename: req.Slug + ".md",
			Content:  fact.Body,
		}
		syncReq.Frontmatter = map[string]interface{}{
			"title":      fact.Title,
			"type":       string(fact.Type),
			"layer":      string(req.ToLayer),
			"confidence": fact.Confidence,
			"tags":       fact.Tags,
		}
		syncResult, syncErr := s.deps.Knowledge.ObsidianSync(s.deps.Ctx, syncReq)
		if syncErr != nil {
			jsonError(w, syncErr.Error(), http.StatusInternalServerError)
			return
		}
		s.auditFromRequest(r, "knowledge_promote_fact", auditDetail("slug", req.Slug, "from", string(req.FromLayer), "to", string(req.ToLayer)), "")
		jsonResponse(w, knowledge.PromoteResult{
			Slug:      syncResult.Slug,
			FromLayer: req.FromLayer,
			ToLayer:   req.ToLayer,
			Success:   true,
		})
		return
	}
	s.auditFromRequest(r, "knowledge_promote_fact", auditDetail("slug", req.Slug, "from", string(req.FromLayer), "to", string(req.ToLayer)), "")
	jsonResponse(w, result)
}

func (s *Server) handleKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Content string `json:"content"`
		Format  string `json:"format"`
		Layer   string `json:"layer"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Content == "" {
		jsonError(w, "content is required", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}
	if req.Format == "" {
		req.Format = "markdown"
	}

	count, err := s.deps.Knowledge.ImportFacts(s.deps.Ctx, knowledge.LayerType(req.Layer), req.Content, req.Format)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "knowledge_import", auditDetail("layer", req.Layer, "format", req.Format), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "imported": count, "layer": req.Layer, "format": req.Format})
}

// handleKnowledgeChannelsList returns the user-writable channels (vaults) that
// facts can be imported into. The reserved automation vault is excluded (#3581).
func (s *Server) handleKnowledgeChannelsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.WritableVaults())
}

// handleKnowledgeChannelCreate creates a new local channel (vault) to import into.
func (s *Server) handleKnowledgeChannelCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	info, err := s.deps.Knowledge.CreateVault(req.Name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, "knowledge_channel_create", auditDetail("channel", info.Name), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "channel": info})
}

func (s *Server) handleKnowledgeSubsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Subscriptions())
}

func (s *Server) handleKnowledgeSubsAdd(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var sub knowledge.Subscription
	if err := decodeBody(r, &sub); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if sub.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(sub.URL, "https://") && !strings.HasPrefix(sub.URL, "http://") {
		jsonError(w, "url must use http or https scheme", http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), sub.URL) {
		jsonError(w, "subscription url must not point to private/internal addresses", http.StatusBadRequest)
		return
	}
	if sub.Layer == "" {
		sub.Layer = knowledge.LayerOrg
	}

	if err := s.deps.Knowledge.AddSubscription(sub); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	s.auditFromRequest(r, "knowledge_add_subscription", auditDetail("url", sub.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "subscription": sub})
}

func (s *Server) handleKnowledgeSubsRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.RemoveSubscription(req.URL); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "knowledge_remove_subscription", auditDetail("url", req.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.URL})
}

// --- Vault endpoints ---

func (s *Server) handleVaultsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Vaults())
}

// vaultPathDeniedPrefixes lists filesystem subtrees that must never be indexed
// as knowledge vaults regardless of caller role. These prefixes contain secrets,
// credentials, and sensitive OS/runtime state that an attacker could exfiltrate
// via the knowledge search API if connected as a vault.
var vaultPathDeniedPrefixes = []string{
	"/etc",
	"/proc",
	"/sys",
	"/run",
	"/var/run",
	"/dev",
	"/root",
	"/boot",
}

func (s *Server) handleVaultsConnect(w http.ResponseWriter, r *http.Request) {
	// Vault connection is an owner-only operation: it indexes an entire directory
	// tree and makes its contents queryable. Restricting to owner prevents a
	// write-role contributor from exfiltrating secrets via the knowledge API.
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "vault path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "vault path must be absolute", http.StatusBadRequest)
		return
	}
	// Reject well-known sensitive filesystem prefixes regardless of role to
	// prevent accidental or deliberate secret exposure via the knowledge search.
	cleanPath := filepath.Clean(req.Path)
	for _, denied := range vaultPathDeniedPrefixes {
		if cleanPath == denied || strings.HasPrefix(cleanPath, denied+"/") {
			jsonError(w, "vault path is in a restricted filesystem location", http.StatusBadRequest)
			return
		}
	}
	if req.Name == "" {
		req.Name = filepath.Base(req.Path)
	}

	if err := s.deps.Knowledge.ConnectVault(req.Path, req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "vault_connect", auditDetail("name", req.Name, "path", req.Path), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name, "path": req.Path})
}

func (s *Server) handleVaultsDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "vault path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "vault path must be absolute", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.DisconnectVault(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "vault_disconnect", auditDetail("path", req.Path), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.Path})
}

func (s *Server) handleVaultsReindex(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "path must be absolute", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.ReindexVault(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "vault_reindex", auditDetail("path", req.Path), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "reindexed": req.Path})
}

func (s *Server) handleVaultFacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}

	name := r.PathValue("name")
	facts := s.deps.Knowledge.VaultFacts(name)
	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, facts)
}

// --- Git source endpoints ---

func (s *Server) handleGitSourcesList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.GitSources())
}

func (s *Server) handleGitSourcesConnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Branch  string `json:"branch"`
		Subpath string `json:"subpath"`
		Layer   string `json:"layer"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL == "" || req.Name == "" {
		jsonError(w, "name and url are required", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	// Context-aware so the SSRF DNS lookup inherits this request's deadline and
	// cancellation rather than running on a background context.
	if err := knowledge.ValidateGitSourceURLContext(r.Context(), req.URL); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Name, "..") || strings.ContainsAny(req.Name, "/\\") {
		jsonError(w, "invalid name", http.StatusBadRequest)
		return
	}
	if req.Branch != "" && strings.HasPrefix(req.Branch, "-") {
		jsonError(w, "branch must not start with '-'", http.StatusBadRequest)
		return
	}
	if req.Subpath != "" && (strings.HasPrefix(req.Subpath, "-") || strings.Contains(req.Subpath, "..")) {
		jsonError(w, "subpath must not start with '-' or contain '..'", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}

	gsConfig := knowledge.GitSourceConfig{
		Name:    req.Name,
		URL:     req.URL,
		Branch:  req.Branch,
		Subpath: req.Subpath,
		Layer:   knowledge.LayerType(req.Layer),
	}
	if err := s.deps.Knowledge.ConnectGitSource(s.deps.Ctx, gsConfig); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	alreadyInConfig := false
	for _, gs := range s.deps.Config.Knowledge.GitSources {
		if gs.URL == req.URL && gs.Subpath == req.Subpath {
			alreadyInConfig = true
			break
		}
	}
	if !alreadyInConfig {
		s.deps.Config.Knowledge.GitSources = append(s.deps.Config.Knowledge.GitSources, config.GitSourceConfigYAML{
			Name:    req.Name,
			URL:     req.URL,
			Branch:  req.Branch,
			Subpath: req.Subpath,
			Layer:   req.Layer,
		})
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after git source connect", "error", err)
	}

	s.auditFromRequest(r, "git_source_connect", auditDetail("name", req.Name, "url", req.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name, "url": req.URL})
}

func (s *Server) handleGitSourcesDisconnect(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL     string `json:"url"`
		Subpath string `json:"subpath"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.DisconnectGitSource(req.URL, req.Subpath); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	filtered := make([]config.GitSourceConfigYAML, 0, len(s.deps.Config.Knowledge.GitSources))
	for _, gs := range s.deps.Config.Knowledge.GitSources {
		if gs.URL == req.URL && gs.Subpath == req.Subpath {
			continue
		}
		filtered = append(filtered, gs)
	}
	s.deps.Config.Knowledge.GitSources = filtered
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after git source disconnect", "error", err)
	}

	s.auditFromRequest(r, "git_source_disconnect", auditDetail("url", req.URL), "")
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.URL})
}

// --- Obsidian sync endpoint ---

func (s *Server) ensureKnowledge() bool {
	if s.deps == nil {
		return false
	}
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()
	if s.deps.Knowledge == nil {
		s.deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, s.logger)
		s.logger.Info("created file-based knowledge API for vault/obsidian access")
		entries, err := os.ReadDir("/data/knowledge")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					dir := filepath.Join("/data/knowledge", e.Name())
					if connErr := s.deps.Knowledge.ConnectVault(dir, e.Name()); connErr == nil {
						s.logger.Info("auto-connected knowledge vault", "name", e.Name(), "dir", dir)
					}
				}
			}
		}
	}
	return true
}

func (s *Server) handleObsidianSync(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "server not initialized", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.ObsidianSyncRequest
	if err := decodeBody(r, &req); err != nil {
		s.logger.Warn("obsidian sync: json decode failed", "error", err)
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Filename == "" {
		jsonError(w, "filename is required", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		if title, ok := req.Frontmatter["title"]; ok {
			if s, ok := title.(string); ok && s != "" {
				req.Content = s
			}
		}
		if req.Content == "" {
			req.Content = "(no body)"
		}
	}

	result, err := s.deps.Knowledge.ObsidianSync(s.deps.Ctx, req)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"ok":     true,
		"slug":   result.Slug,
		"action": result.Action,
		"fact":   result.Fact,
	})
}

// --- Document source endpoints ---

func (s *Server) handleDocumentsList(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.deps.Knowledge.ListDocuments())
}

func (s *Server) handleDocumentsImport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.DocSourceConfig
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.URL == "" && req.FilePath == "" && req.Context7ID == "" {
		jsonError(w, "url, file_path, or context7_id is required", http.StatusBadRequest)
		return
	}
	if req.URL != "" {
		if !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") {
			jsonError(w, "document url must use http or https scheme", http.StatusBadRequest)
			return
		}
		if isPrivateURL(r.Context(), req.URL) {
			jsonError(w, "document url must not point to private/internal addresses", http.StatusBadRequest)
			return
		}
	}
	if req.Layer == "" {
		req.Layer = knowledge.LayerProject
	}

	meta, err := s.deps.Knowledge.ImportDocument(s.deps.Ctx, req)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "document_import", auditDetail("name", req.Name), "")
	jsonResponse(w, meta)
}

func (s *Server) handleDocumentGet(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	slug := r.PathValue("slug")
	meta, err := s.deps.Knowledge.GetDocument(slug)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, meta)
}

func (s *Server) handleDocumentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	slug := r.PathValue("slug")
	if err := s.deps.Knowledge.DeleteDocument(slug); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "document_delete", auditDetail("slug", slug), "")
	okResponse(w, map[string]string{"status": "deleted", "slug": slug})
}

func (s *Server) handleDocumentReimport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	slug := r.PathValue("slug")
	meta, err := s.deps.Knowledge.ReimportDocument(s.deps.Ctx, slug)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "document_reimport", auditDetail("slug", slug), "")
	jsonResponse(w, meta)
}

func (s *Server) handleCleanupOrphans(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	removed, err := s.deps.Knowledge.CleanupOrphanedDocFacts()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": removed})
}

func (s *Server) handleContext7Search(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not configured", http.StatusServiceUnavailable)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		jsonError(w, "missing q parameter", http.StatusBadRequest)
		return
	}

	results, err := s.deps.Knowledge.SearchContext7Libraries(r.Context(), query)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonResponse(w, results)
}

// --- Hive ID endpoints ---

// hiveIDFilePath is the persistent file where the Hive ID is stored.
const hiveIDFilePath = "/data/hive-id"

func (s *Server) handleHiveIDGet(w http.ResponseWriter, r *http.Request) {
	id := ""
	if s.deps != nil && s.deps.Config != nil {
		id = s.deps.Config.HiveID
	}
	jsonResponse(w, map[string]string{"id": id})
}

func (s *Server) handleHiveIDSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil || body.ID == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}

	const maxHiveIDLen = 64
	if len(body.ID) > maxHiveIDLen {
		jsonError(w, fmt.Sprintf("id must be at most %d characters", maxHiveIDLen), http.StatusBadRequest)
		return
	}
	if !displayNamePattern.MatchString(body.ID) {
		jsonError(w, "id must contain only alphanumeric characters, spaces, hyphens, and underscores", http.StatusBadRequest)
		return
	}
	body.ID = sanitizeString(body.ID)

	if s.deps != nil && s.deps.Config != nil {
		s.deps.Config.HiveID = body.ID
	}

	// Persist the new ID to disk so it survives restarts
	tmpHiveID := hiveIDFilePath + ".tmp"
	if err := os.WriteFile(tmpHiveID, []byte(body.ID+"\n"), 0o644); err != nil {
		s.logger.Warn("failed to persist hive ID", "error", err)
	} else if err := os.Rename(tmpHiveID, hiveIDFilePath); err != nil {
		s.logger.Warn("failed to rename hive ID file", "error", err)
	}

	s.auditFromRequest(r, "set_hive_id", auditDetail("id", body.ID), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "id": body.ID})
}

// --- Chat endpoint ---

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query   string `json:"query"`
		History []any  `json:"history"`
	}
	if err := decodeBody(r, &body); err != nil || body.Query == "" {
		jsonError(w, "message is required", http.StatusBadRequest)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"answer": fmt.Sprintf("Chat is not yet fully implemented. You asked: %s", sanitizeString(body.Query)),
		"status": "stub",
	})
}

// --- Nous endpoints ---

func (s *Server) handleNousStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"status": "not_configured"})
		return
	}
	s.deps.Nous.Mu.Lock()
	status := make(map[string]interface{}, len(s.deps.Nous.Status))
	for k, v := range s.deps.Nous.Status {
		status[k] = v
	}
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, status)
}

func (s *Server) handleNousLedger(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	ledger := s.deps.Nous.Ledger
	s.deps.Nous.Mu.Unlock()
	if ledger == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, ledger)
}

func (s *Server) handleNousPrinciples(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	principles := s.deps.Nous.Principles
	s.deps.Nous.Mu.Unlock()
	if principles == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, principles)
}

func (s *Server) handleNousApprove(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	s.auditFromRequest(r, "nous_approve", "", "")
	okResponse(w, map[string]string{"status": "approved"})
}

func (s *Server) handleNousAbort(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	s.auditFromRequest(r, "nous_abort", "", "")
	okResponse(w, map[string]string{"status": "aborted"})
}

func (s *Server) handleNousMode(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil || body.Mode == "" {
		jsonError(w, "mode is required", http.StatusBadRequest)
		return
	}

	body.Mode = sanitizeString(body.Mode)
	if body.Mode == "" {
		jsonError(w, "mode is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.Mode = body.Mode
	}

	s.auditFromRequest(r, "nous_set_mode", auditDetail("mode", body.Mode), "")
	okResponse(w, map[string]string{"status": "updated", "mode": body.Mode})
}

func (s *Server) handleNousScope(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Scope string `json:"scope"`
	}
	if err := decodeBody(r, &body); err != nil || body.Scope == "" {
		jsonError(w, "scope is required", http.StatusBadRequest)
		return
	}

	body.Scope = sanitizeString(body.Scope)
	if body.Scope == "" {
		jsonError(w, "scope is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.Scope = body.Scope
	}

	s.auditFromRequest(r, "nous_set_scope", auditDetail("scope", body.Scope), "")
	okResponse(w, map[string]string{"status": "updated", "scope": body.Scope})
}

func (s *Server) handleNousPhase(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"phase": "inactive"})
		return
	}
	jsonResponse(w, map[string]string{"phase": s.deps.Nous.Phase})
}

func (s *Server) handleNousGateDecision(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil || body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	body.Decision = sanitizeString(body.Decision)
	body.Reason = sanitizeString(body.Reason)
	if body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous.GatePending == nil {
		s.deps.Nous.GatePending = make(map[string]interface{})
	}
	s.deps.Nous.GateResponse = map[string]interface{}{
		"decision": body.Decision,
		"reason":   body.Reason,
	}

	s.auditFromRequest(r, "nous_gate_decision", auditDetail("decision", body.Decision), "")
	okResponse(w, map[string]string{"status": "decided", "decision": body.Decision})
}

func (s *Server) handleNousGatePending(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Nous.GatePending)
}

func (s *Server) handleNousGateRespond(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.GateResponse = body
	}

	s.auditFromRequest(r, "nous_gate_respond", "", "")
	okResponse(w, map[string]string{"status": "responded"})
}

func (s *Server) handleNousGateResponse(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Nous.GateResponse)
}

func (s *Server) handleNousConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	cfg := s.deps.Nous.Config
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, cfg)
}

func (s *Server) handleNousConfigGoals(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "goals")
}

func (s *Server) handleNousConfigRepos(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "repos")
}

func (s *Server) handleNousConfigOutput(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "output")
}

func (s *Server) handleNousConfigFastFail(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "fast_fail")
}

func (s *Server) handleNousConfigSchedule(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "schedule")
}

func (s *Server) handleNousConfigControllables(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "controllables")
}

func (s *Server) handleNousConfigPrinciples(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "principles")
}

func (s *Server) handleNousDeletePrinciple(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	id := r.PathValue("id")
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	s.deps.Nous.Mu.Lock()
	filtered := make([]NousPrinciple, 0, len(s.deps.Nous.Principles))
	for _, p := range s.deps.Nous.Principles {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	s.deps.Nous.Principles = filtered
	s.deps.Nous.Mu.Unlock()

	s.auditFromRequest(r, "nous_delete_principle", auditDetail("id", id), "")
	okResponse(w, map[string]string{"status": "deleted", "id": id})
}

func (s *Server) handleNousConfigSection(w http.ResponseWriter, r *http.Request, section string) {
	if !requireOwnerRole(w, r) {
		return
	}

	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Nous.Mu.Lock()
	if s.deps.Nous.Config == nil {
		s.deps.Nous.Config = make(map[string]interface{})
	}
	s.deps.Nous.Config[section] = body
	s.deps.Nous.Mu.Unlock()

	s.auditFromRequest(r, "nous_config_update", auditDetail("section", section), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "section": section})
}

var defaultPipelineSteps = map[string]bool{
	"resolve-beads":  true,
	"track-prs":      true,
	"stale-check":    true,
	"repo-scan":      true,
	"coverage-gate":  true,
	"prompt-compose": true,
	"budget-check":   true,
	"api-collect":    true,
	"final-compose":  true,
}

func (s *Server) getAgentPipeline(name string) map[string]bool {
	s.pipelineMu.RLock()
	defer s.pipelineMu.RUnlock()
	if p, ok := s.agentPipelines[name]; ok {
		return p
	}
	result := make(map[string]bool, len(defaultPipelineSteps))
	for k, v := range defaultPipelineSteps {
		result[k] = v
	}
	return result
}

func (s *Server) getAgentHooks(name string) map[string][]any {
	s.hooksMu.RLock()
	defer s.hooksMu.RUnlock()
	if h, ok := s.agentHooks[name]; ok {
		return h
	}
	return map[string][]any{"preKick": {}, "postIdle": {}}
}

// maskSecret replaces the interior of a secret string with bullet characters,
// revealing only the last 4 characters (matching old hive behavior).
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	const visibleSuffix = 4
	if len(s) <= visibleSuffix {
		return strings.Repeat("•", len(s))
	}
	masked := strings.Repeat("•", len(s)-visibleSuffix)
	return masked + s[len(s)-visibleSuffix:]
}

func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	// On a direct-route spoke the shared token must never be handed to a
	// browser: identity there is per-user (device-flow sessions), and the
	// shared token no longer grants API access on that path (see authenticate).
	// Exposing it publicly here would leak the internal server-to-server secret
	// (used as X-Hive-Internal by the local proxy) to any visitor.
	// The same applies on a hub-proxied spoke: nginx injects per-user identity,
	// so the token is server-to-server only there too — and reporting
	// configured=true made the SPA prompt hosted operators for a token they
	// were never given (and don't need; their hub login already authorizes).
	if s.directRouteAuthzEnabled() || s.hubProxied() {
		jsonError(w, "not available on this hive", http.StatusNotFound)
		return
	}
	// SECURITY: never return the token VALUE. On a self-hosted (non-direct-route)
	// hive the dashboard token IS the API credential, so handing it to any
	// same-origin caller made "authentication" meaningless (an unauthenticated
	// visitor could GET it and then mutate). This endpoint now only reports
	// WHETHER a token is configured; a browser that needs to authenticate an
	// operator obtains the token by having the operator paste it (see the SPA's
	// token prompt), never by reading it back from the server.
	token := s.authToken
	if token == "" {
		token = os.Getenv("HIVE_DASHBOARD_TOKEN")
	}
	okResponse(w, map[string]string{"configured": strconv.FormatBool(token != "")})
}

// maskToken replaces all but the last 4 characters with bullet characters.
func maskToken(token string) string {
	const visibleSuffix = 4
	if len(token) <= visibleSuffix {
		return token
	}
	masked := make([]byte, 0, len(token))
	hideLen := len(token) - visibleSuffix
	for i := 0; i < hideLen; i++ {
		masked = append(masked, "•"...)
	}
	masked = append(masked, token[hideLen:]...)
	return string(masked)
}

func (s *Server) handleBeadsReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		body.Reason = "manual reset via API"
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		body.Reason = "manual reset via API"
	}

	results := make(map[string]int)
	for name, store := range s.deps.BeadStores {
		closed, err := store.CloseAll(body.Reason)
		if err != nil {
			s.deps.Logger.Error("beads reset failed", "agent", name, "error", err)
		}
		results[name] = closed
	}

	s.auditFromRequest(r, "beads_reset", "", "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]any{"status": "reset", "closed": results, "reason": body.Reason})
}

func (s *Server) handleBeadsResetAgent(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	agentName := r.PathValue("agent")
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		body.Reason = "manual reset via API"
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		body.Reason = "manual reset via API"
	}

	closed, err := store.CloseAll(body.Reason)
	if err != nil {
		jsonError(w, fmt.Sprintf("reset failed: %v", err), http.StatusInternalServerError)
		return
	}

	s.auditFromRequest(r, "beads_reset_agent", "", agentName)
	s.refreshAndPersist()
	jsonResponse(w, map[string]any{"status": "reset", "agent": agentName, "closed": closed, "reason": body.Reason})
}

func (s *Server) handleBeadsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	agentName := r.PathValue("agent")
	result := make(map[string]any)

	if agentName != "" {
		store, ok := s.deps.BeadStores[agentName]
		if !ok {
			jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
			return
		}
		result[agentName] = store.List(beads.ListFilter{})
	} else {
		for name, store := range s.deps.BeadStores {
			result[name] = store.List(beads.ListFilter{})
		}
	}
	jsonResponse(w, result)
}

const maxBeadPriority = 4

func (s *Server) handleBeadsCreate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	agentName := r.PathValue("agent")
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
		return
	}

	var body struct {
		Title       string            `json:"title"`
		Type        string            `json:"type"`
		Priority    int               `json:"priority"`
		ExternalRef string            `json:"external_ref"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.Title = sanitizeString(body.Title)
	const maxBeadTitleLen = 500
	if body.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(body.Title) > maxBeadTitleLen {
		jsonError(w, fmt.Sprintf("title too long (%d chars, max %d)", len(body.Title), maxBeadTitleLen), http.StatusBadRequest)
		return
	}
	body.Type = sanitizeString(body.Type)
	if body.Type == "" {
		body.Type = "advisory"
	}
	body.ExternalRef = sanitizeString(body.ExternalRef)
	if body.Priority < 0 || body.Priority > maxBeadPriority {
		jsonError(w, "priority must be 0-4", http.StatusBadRequest)
		return
	}

	b, err := store.Create(body.Title, beads.BeadType(body.Type), beads.Priority(body.Priority), agentName, body.ExternalRef)
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to create bead: %v", err), http.StatusInternalServerError)
		return
	}

	const maxMetadataKeyLen = 100
	const maxMetadataValueLen = 1000
	const maxMetadataEntries = 50
	metaCount := 0
	for k, v := range body.Metadata {
		if metaCount >= maxMetadataEntries || len(k) > maxMetadataKeyLen || len(v) > maxMetadataValueLen {
			continue
		}
		_ = store.SetMetadata(b.ID, k, v)
		metaCount++
	}

	s.auditFromRequest(r, "bead_create", auditDetail("title", body.Title), agentName)
	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, b)
}
