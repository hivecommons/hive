package governor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const admissionHistoryCapacity = 500

type WorkAdmissionRequest struct {
	SchemaVersion         string                                `json:"schema_version"`
	SourceExternalRef     string                                `json:"source_external_ref"`
	PacketDigest          string                                `json:"packet_digest"`
	FindingFingerprint    string                                `json:"finding_fingerprint"`
	RepositoryFingerprint string                                `json:"repository_fingerprint"`
	Role                  string                                `json:"role"`
	RoleConfigured        bool                                  `json:"role_configured"`
	RoleConfigSHA256      string                                `json:"role_config_sha256,omitempty"`
	RoutingReason         string                                `json:"routing_reason"`
	RoutingAllowed        bool                                  `json:"routing_allowed"`
	RuntimePaused         bool                                  `json:"runtime_paused"`
	AuthorityAllowsWork   bool                                  `json:"authority_allows_work"`
	SafeExecutionReady    bool                                  `json:"safe_execution_ready"`
	ActiveWIP             int                                   `json:"active_wip"`
	WIPLimit              int                                   `json:"wip_limit"`
	Evidence              visualhive.SpecialistEvidenceIdentity `json:"evidence"`
	WorkIdentitySHA256    string                                `json:"work_identity_sha256"`
	ExecutionPolicySHA256 string                                `json:"execution_policy_sha256"`
	RuntimePolicySHA256   string                                `json:"runtime_policy_sha256"`
	GovernorMode          Mode                                  `json:"governor_mode"`
	ConfiguredCadence     string                                `json:"configured_cadence"`
	BudgetUsed            int64                                 `json:"budget_used"`
	BudgetLimit           int64                                 `json:"budget_limit"`
	BudgetIgnoredForRole  bool                                  `json:"budget_ignored_for_role"`
}

type WorkAdmissionDecision struct {
	Timestamp             time.Time                             `json:"timestamp"`
	Allowed               bool                                  `json:"allowed"`
	Code                  string                                `json:"code"`
	Reason                string                                `json:"reason"`
	Mode                  Mode                                  `json:"mode"`
	SourceExternalRef     string                                `json:"source_external_ref"`
	PacketDigest          string                                `json:"packet_digest"`
	FindingFingerprint    string                                `json:"finding_fingerprint"`
	RepositoryFingerprint string                                `json:"repository_fingerprint"`
	Role                  string                                `json:"role"`
	ActiveWIP             int                                   `json:"active_wip"`
	WIPLimit              int                                   `json:"wip_limit"`
	Evidence              visualhive.SpecialistEvidenceIdentity `json:"evidence"`
	RequestSHA256         string                                `json:"request_sha256"`
	RequestJSON           string                                `json:"request_json"`
}

// AdmitWork is the normal governor's explicit work-decision boundary. It uses
// the same configured roles, mode/cadence state, and live budget state as
// Evaluate, while WIP is measured from the selected normal role bead store.
func (g *Governor) AdmitWork(request WorkAdmissionRequest) WorkAdmissionDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	request = g.bindAdmissionRequestLocked(request)

	decision := WorkAdmissionDecision{
		Timestamp: time.Now().UTC(), Mode: g.state.Mode, SourceExternalRef: request.SourceExternalRef,
		PacketDigest: request.PacketDigest, FindingFingerprint: request.FindingFingerprint,
		RepositoryFingerprint: request.RepositoryFingerprint, Role: request.Role,
		ActiveWIP: request.ActiveWIP, WIPLimit: request.WIPLimit, Evidence: request.Evidence,
	}
	if encoded, err := json.Marshal(request); err == nil {
		decision.RequestJSON = string(encoded)
		decision.RequestSHA256 = AdmissionRequestSHA256(request)
	}
	deny := func(code, reason string) WorkAdmissionDecision {
		decision.Code, decision.Reason = code, reason
		g.appendAdmissionHistory(decision)
		return decision
	}
	if strings.TrimSpace(request.SourceExternalRef) == "" || strings.TrimSpace(request.PacketDigest) == "" || strings.TrimSpace(request.RepositoryFingerprint) == "" {
		return deny("invalid_identity", "complete source external-ref, packet digest, and repository fingerprint are required")
	}
	if !request.RoutingAllowed || strings.TrimSpace(request.Role) == "" {
		return deny("routing_held", request.RoutingReason)
	}
	if !request.RoleConfigured {
		return deny("role_unconfigured", fmt.Sprintf("role %s is not an enabled matching normal Hive role", request.Role))
	}
	modeConfig, exists := g.cfg.Modes[modeToConfigKey(g.state.Mode)]
	if !exists {
		return deny("mode_unconfigured", fmt.Sprintf("governor mode %s has no admission configuration", g.state.Mode))
	}
	cadence, exists := modeConfig.Cadences[request.Role]
	if !exists {
		return deny("mode_role_unconfigured", fmt.Sprintf("role %s is not enabled in governor mode %s", request.Role, g.state.Mode))
	}
	if cadence == "pause" || cadence == "paused" || request.RuntimePaused {
		return deny("role_paused", fmt.Sprintf("role %s is paused in governor mode %s or by the normal manager", request.Role, g.state.Mode))
	}
	if !request.AuthorityAllowsWork {
		return deny("authority_advisory", "installed Hive authority does not permit issue-backed repair")
	}
	if !request.SafeExecutionReady {
		return deny("execution_held", "verified evidence or executable validation bindings are incomplete")
	}
	if g.budgetExhausted() && !containsString(g.budget.IgnoredAgents, request.Role) {
		return deny("budget_exhausted", "normal Hive token budget is exhausted for the selected role")
	}
	if request.WIPLimit <= 0 || request.ActiveWIP < 0 || request.ActiveWIP >= request.WIPLimit {
		return deny("wip_limit", fmt.Sprintf("role %s WIP %d has reached limit %d", request.Role, request.ActiveWIP, request.WIPLimit))
	}
	decision.Allowed = true
	decision.Code = "allowed"
	decision.Reason = "verified Visual Hive work passed normal role, mode, pause, authority, budget, and WIP admission"
	g.appendAdmissionHistory(decision)
	return decision
}

func (g *Governor) BindAdmissionRequest(request WorkAdmissionRequest) WorkAdmissionRequest {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.bindAdmissionRequestLocked(request)
}

func (g *Governor) bindAdmissionRequestLocked(request WorkAdmissionRequest) WorkAdmissionRequest {
	request.SchemaVersion = "hive.visual-work-admission-request.v1"
	request.GovernorMode = g.state.Mode
	if configured, exists := g.agents[request.Role]; exists {
		request.RoleConfigured = configured.Enabled && (configured.Role == "" || configured.Role == request.Role)
		if encoded, err := json.Marshal(configured); err == nil {
			digest := sha256.Sum256(encoded)
			request.RoleConfigSHA256 = hex.EncodeToString(digest[:])
		}
	} else {
		request.RoleConfigured = false
		request.RoleConfigSHA256 = ""
	}
	request.ConfiguredCadence = ""
	if mode, ok := g.cfg.Modes[modeToConfigKey(g.state.Mode)]; ok {
		request.ConfiguredCadence = string(mode.Cadences[request.Role])
	}
	request.BudgetUsed = g.budget.CurrentSpend
	request.BudgetLimit = g.budget.WeeklyLimit
	request.BudgetIgnoredForRole = g.budget.IgnoreAll || containsString(g.budget.IgnoredAgents, request.Role)
	return request
}

func AdmissionRequestSHA256(request WorkAdmissionRequest) string {
	encoded, err := json.Marshal(request)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (g *Governor) appendAdmissionHistory(decision WorkAdmissionDecision) {
	if len(g.admissionHistory) >= admissionHistoryCapacity {
		g.admissionHistory = g.admissionHistory[1:]
	}
	g.admissionHistory = append(g.admissionHistory, decision)
}

func (g *Governor) AdmissionHistory() []WorkAdmissionDecision {
	g.mu.RLock()
	defer g.mu.RUnlock()
	result := make([]WorkAdmissionDecision, len(g.admissionHistory))
	copy(result, g.admissionHistory)
	return result
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
