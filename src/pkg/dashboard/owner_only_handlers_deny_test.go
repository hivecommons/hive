package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBeadsResetHandlersRejectNonOwners(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"all agents", "/api/beads/reset", newFullServer(t).handleBeadsReset},
		{"single agent", "/api/beads/scanner/reset", newFullServer(t).handleBeadsResetAgent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("X-Hive-Role", "read-write")
			req.SetPathValue("agent", "scanner")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("non-owner status = %d, want %d", w.Code, http.StatusForbidden)
			}
		})
	}
}

func TestV4OwnerOnlyHandlerGapsRejectUnverifiedOwners(t *testing.T) {
	srv := &Server{}
	tests := []struct {
		name    string
		method  string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"ACMM create issue", http.MethodPost, "/api/acmm/issues", srv.handleACMMCreateIssue},
		{"agent config cadences", http.MethodPut, "/api/config/agent/scanner/cadences", srv.handleAgentConfigCadences},
		{"agent config channels", http.MethodPut, "/api/config/agent/scanner/channels", srv.handleAgentConfigChannels},
		{"agent config connections", http.MethodPut, "/api/config/agent/scanner/connections", srv.handleAgentConfigConnections},
		{"agent config general", http.MethodPut, "/api/config/agent/scanner/general", srv.handleAgentConfigGeneral},
		{"agent config hooks", http.MethodPut, "/api/config/agent/scanner/hooks", srv.handleAgentConfigHooks},
		{"agent config models", http.MethodPut, "/api/config/agent/scanner/models", srv.handleAgentConfigModels},
		{"agent config pipeline", http.MethodPut, "/api/config/agent/scanner/pipeline", srv.handleAgentConfigPipeline},
		{"agent config restrictions", http.MethodPut, "/api/config/agent/scanner/restrictions", srv.handleAgentConfigRestrictions},
		{"agent config stats", http.MethodPut, "/api/config/agent/scanner/stats", srv.handleAgentConfigStats},
		{"agent config tools", http.MethodPut, "/api/config/agent/scanner/tools", srv.handleAgentConfigTools},
		{"agent delete", http.MethodDelete, "/api/agents/scanner", srv.handleAgentDelete},
		{"agent prompt save", http.MethodPut, "/api/config/agent/scanner/prompt", srv.handleAgentPromptSave},
		{"backup download", http.MethodPost, "/api/backup", srv.handleBackupDownload},
		{"backup status", http.MethodGet, "/api/backup/status", srv.handleBackupStatus},
		{"bead synth toggle", http.MethodPut, "/api/bead-synth/enabled", srv.handleBeadSynthToggle},
		{"budget ignore", http.MethodPut, "/api/config/governor/budget/ignore", srv.handleBudgetIgnoreSet},
		{"config download", http.MethodGet, "/api/config/download", srv.handleConfigDownload},
		{"github config", http.MethodPut, "/api/config/github", srv.handleConfigGitHub},
		{"git sources connect", http.MethodPost, "/api/config/git-sources/connect", srv.handleGitSourcesConnect},
		{"git sources disconnect", http.MethodPost, "/api/config/git-sources/disconnect", srv.handleGitSourcesDisconnect},
		{"governor add agent", http.MethodPost, "/api/config/governor/agents", srv.handleGovernorAddAgent},
		{"governor attribution", http.MethodPut, "/api/config/governor/attribution", srv.handleGovernorAttribution},
		{"governor bob key", http.MethodPut, "/api/config/governor/bob", srv.handleGovernorBobKey},
		{"governor bob key clear", http.MethodDelete, "/api/config/governor/bob", srv.handleGovernorBobKeyClear},
		{"governor budget", http.MethodPut, "/api/config/governor/budget", srv.handleGovernorBudget},
		{"gateway delete", http.MethodDelete, "/api/config/governor/gateways/openrouter", srv.handleGovernorGatewaysDelete},
		{"gateway upsert", http.MethodPut, "/api/config/governor/gateways", srv.handleGovernorGatewaysUpsert},
		{"governor health", http.MethodPut, "/api/config/governor/health", srv.handleGovernorHealth},
		{"governor hub", http.MethodPut, "/api/config/governor/hub", srv.handleGovernorHub},
		{"governor labels", http.MethodPut, "/api/config/governor/labels", srv.handleGovernorLabels},
		{"governor litellm", http.MethodPut, "/api/config/governor/litellm", srv.handleGovernorLiteLLM},
		{"governor logging", http.MethodPut, "/api/config/governor/logging", srv.handleGovernorLogging},
		{"governor notifications", http.MethodPut, "/api/config/governor/notifications", srv.handleGovernorNotifications},
		{"governor remove agent", http.MethodDelete, "/api/config/governor/agents/scanner", srv.handleGovernorRemoveAgent},
		{"governor repos", http.MethodPut, "/api/config/governor/repos", srv.handleGovernorRepos},
		{"governor sensing", http.MethodPut, "/api/config/governor/sensing", srv.handleGovernorSensing},
		{"governor thresholds", http.MethodPut, "/api/config/governor/thresholds", srv.handleGovernorThresholds},
		{"governor trajectory", http.MethodPut, "/api/config/governor/trajectory", srv.handleGovernorTrajectory},
		{"hive id set", http.MethodPut, "/api/hive-id", srv.handleHiveIDSet},
		{"inception answer", http.MethodPost, "/api/inception/answer", srv.handleInceptionAnswer},
		{"inception approve", http.MethodPost, "/api/inception/approve", srv.handleInceptionApprove},
		{"inception import", http.MethodPost, "/api/inception/import", srv.handleInceptionImport},
		{"inception record facts", http.MethodPost, "/api/inception/facts", srv.handleInceptionRecordFacts},
		{"inception rename wiki", http.MethodPost, "/api/inception/rename-wiki", srv.handleInceptionRenameWiki},
		{"inception reset", http.MethodPost, "/api/inception/reset", srv.handleInceptionReset},
		{"inception scan", http.MethodPost, "/api/inception/scan", srv.handleInceptionScan},
		{"inception set questions", http.MethodPost, "/api/inception/questions", srv.handleInceptionSetQuestions},
		{"inception start", http.MethodPost, "/api/inception/start", srv.handleInceptionStart},
		{"knowledge toggle", http.MethodPut, "/api/knowledge/enabled", srv.handleKnowledgeToggle},
		{"nous abort", http.MethodPost, "/api/nous/abort", srv.handleNousAbort},
		{"nous approve", http.MethodPost, "/api/nous/approve", srv.handleNousApprove},
		{"nous config section", http.MethodPut, "/api/nous/config/goals", func(w http.ResponseWriter, r *http.Request) { srv.handleNousConfigSection(w, r, "goals") }},
		{"nous delete principle", http.MethodDelete, "/api/nous/config/principles/0", srv.handleNousDeletePrinciple},
		{"nous gate decision", http.MethodPost, "/api/nous/gate/decision", srv.handleNousGateDecision},
		{"nous gate respond", http.MethodPost, "/api/nous/gate/respond", srv.handleNousGateRespond},
		{"nous mode", http.MethodPut, "/api/nous/mode", srv.handleNousMode},
		{"nous scope", http.MethodPut, "/api/nous/scope", srv.handleNousScope},
		{"pack apply", http.MethodPost, "/api/packs/apply", srv.handlePackApply},
		{"pack set level", http.MethodPut, "/api/packs/level", srv.handlePackSetLevel},
		{"self upgrade", http.MethodPost, "/api/self-upgrade", srv.handleSelfUpgrade},
		{"variable delete", http.MethodDelete, "/api/config/variables/VAR", srv.handleVariableDelete},
		{"variable upsert", http.MethodPut, "/api/config/variables/VAR", srv.handleVariableUpsert},
	}

	authCases := []struct {
		name string
		role string
	}{
		{"read-write", "read-write"},
		{"proofless owner", "owner"},
	}
	for _, tc := range tests {
		for _, ac := range authCases {
			t.Run(tc.name+"/"+ac.name, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("handler panicked for unverified request: %v; want status %d", r, http.StatusForbidden)
					}
				}()
				req := httptest.NewRequest(tc.method, tc.path, nil)
				req.Header.Set("X-Hive-Role", ac.role)
				req.SetPathValue("agent", "scanner")
				req.SetPathValue("name", "openrouter")
				req.SetPathValue("key", "VAR")
				req.SetPathValue("index", "0")
				w := httptest.NewRecorder()

				tc.handler(w, req)

				if w.Code != http.StatusForbidden {
					t.Fatalf("unverified %q status = %d, want %d", ac.role, w.Code, http.StatusForbidden)
				}
			})
		}
	}
}

func TestNousMutationHandlersRejectMissingRole(t *testing.T) {
	srv := &Server{}
	tests := []struct {
		name    string
		method  string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"nous approve", http.MethodPost, "/api/nous/approve", srv.handleNousApprove},
		{"nous abort", http.MethodPost, "/api/nous/abort", srv.handleNousAbort},
		{"nous mode", http.MethodPut, "/api/nous/mode", srv.handleNousMode},
		{"nous scope", http.MethodPut, "/api/nous/scope", srv.handleNousScope},
		{"nous gate decision", http.MethodPost, "/api/nous/gate/decision", srv.handleNousGateDecision},
		{"nous gate respond", http.MethodPost, "/api/nous/gate/respond", srv.handleNousGateRespond},
		{"nous delete principle", http.MethodDelete, "/api/nous/config/principles/0", srv.handleNousDeletePrinciple},
		{"nous config section", http.MethodPut, "/api/nous/config/section", func(w http.ResponseWriter, r *http.Request) {
			srv.handleNousConfigSection(w, r, "general")
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.SetPathValue("id", "0")
			w := httptest.NewRecorder()

			tc.handler(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("missing-role status = %d, want %d", w.Code, http.StatusForbidden)
			}
		})
	}
}
