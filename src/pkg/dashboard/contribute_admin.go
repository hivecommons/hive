// Contributor management (trust, agent roles, revoke, requeue, delete) handlers,
// moved verbatim out of api_contribute.go (#7435). Same package — no call sites changed.

package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// ── Contributor management ─────────────────────────────────────────────────

// requireContributorWrite enforces owner/read-write authorization on the
// contributor mutation endpoints (trust/revoke/delete) that the Management &
// Operations tab surfaces as admin controls.
//
// These handlers live under the /api/contributors/... path. Each mutation handler
// must enforce the write boundary itself, because these routes are otherwise only
// as protected as the surrounding auth layer — and on a direct OpenShift Route or
// in-cluster (no hub nginx, no NetworkPolicy) there is NO auth layer in front.
//
// SECURITY (C5): FAIL CLOSED. An absent/empty X-Hive-Role is treated as NOT
// authorized (deny), not as owner. The previous code defaulted an absent header to
// "owner" for "local/dev, no hub nginx" convenience — but that same absence is
// exactly what an anonymous caller hitting the pod directly (bypassing the hub
// nginx that would otherwise inject the header) presents. Combined with the
// prefix-match bug that exempted /api/contributors/... from authentication, an
// unauthenticated caller could promote/revoke/delete/requeue contributors. Only an
// explicit owner/read-write role may mutate; everything else (absent, "read", or
// any unrecognized value) is rejected. UI hiding on the ops tab is UX; this is the
// security boundary.
func (s *Server) requireContributorWrite(w http.ResponseWriter, r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role != config.RoleOwner && role != config.RoleReadWrite {
		jsonError(w, "your permissions on this hive are read-only, so changes are not allowed. Contact the owner of this hive to ask for write permissions.", http.StatusForbidden)
		return false
	}
	return true
}

// paneTailViewer reports whether this request may see agent pane output —
// the pane_tail on a fleet row or a task-run record (#7317 item 3).
//
// Pane text is what the agent printed: more sensitive than the bounded reason
// string the same endpoints already serve anonymously, since it can carry
// repository paths, partial secrets the redactor did not recognize, or a
// half-typed credential prompt. So it follows the same line the per-clanker
// controls draw: owner and read-write see it, everyone else does not.
//
// The empty-role case mirrors requestRoleAllowsOwner, not requireContributorWrite:
// on a spoke with any auth boundary an absent X-Hive-Role is an anonymous caller
// and gets nothing, while on a genuinely open spoke (no token, no allowlist) the
// whole dashboard is already anonymous and hiding one field from its only
// operator would be theatre. Read-only decision, no side effects.
func (s *Server) paneTailViewer(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role == config.RoleOwner || role == config.RoleReadWrite {
		return true
	}
	if role == "" {
		return s.authToken == "" && !s.directRouteAuthzEnabled()
	}
	return false
}

func (s *Server) handleContributorsList(w http.ResponseWriter, r *http.Request) {
	profiles := listContributorProfiles()
	var liveStates map[string]ContributorLiveState
	if s.contributeHub != nil {
		liveStates = s.contributeHub.LiveStates()
	}
	for i := range profiles {
		profiles[i].TokenPlain = ""
		profiles[i].RegistrationToken = ""
		if ls, ok := liveStates[profiles[i].ContributorID]; ok {
			profiles[i].Active = ls.Active
			profiles[i].CurrentTask = ls.CurrentTask
			profiles[i].ActiveTasks = ls.Tasks
			profiles[i].Sessions = ls.Sessions
		}
	}
	jsonResponse(w, map[string]any{"contributors": profiles})
}

func (s *Server) handleContributorGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TokenPlain = ""
	p.RegistrationToken = ""
	jsonResponse(w, p)
}

func (s *Server) handleContributorTrust(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	var req struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	validTiers := map[string]bool{"newcomer": true, "contributor": true, "trusted": true, "merger": true, "advisor": true, "revoked": true}
	if !validTiers[req.Tier] {
		jsonError(w, "Invalid tier", http.StatusBadRequest)
		return
	}
	p.TrustTier = req.Tier
	// H2: admin path — adminOverride lets this change a terminal (revoked) tier and
	// wins the CAS reconcile against any concurrent stale WS save. The write is
	// server-authoritative.
	if err := saveContributorProfileCAS(p, true); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	// H2: if this change revokes access, fence any live WebSocket sessions the
	// contributor holds so an in-flight connection cannot keep working (or keep
	// saving a stale "contributor" profile) after the revoke.
	if req.Tier == "revoked" && s.contributeHub != nil {
		s.contributeHub.DisconnectContributor(p.ContributorID, "contribution access revoked")
	}
	s.logger.Info("contributor tier changed", "username", p.GitHubUsername, "tier", req.Tier)
	jsonResponse(w, map[string]any{"ok": true, "trust_tier": req.Tier})
}

func (s *Server) handleContributorAgentRoleGrants(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	var req struct {
		AgentRoleGrants []string `json:"agent_role_grants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	grantable := contributorAgentRoleGrantableRoles(nil)
	if s.deps != nil {
		grantable = contributorAgentRoleGrantableRoles(s.deps.Config)
	}
	allowed := make(map[string]bool, len(grantable))
	for _, role := range grantable {
		allowed[role] = true
	}
	grants := make([]string, 0, len(req.AgentRoleGrants))
	for _, role := range req.AgentRoleGrants {
		role = normalizeAgentRole(role)
		if role == "" {
			continue
		}
		if !allowed[role] {
			jsonError(w, fmt.Sprintf("agent role %q is not a grantable delegated privileged role", role), http.StatusBadRequest)
			return
		}
		grants = append(grants, role)
	}
	grants = normalizeUniqueAgentRoles(grants)
	assigned := effectiveAssignedAgentRole(p.AssignedAgentRole)
	if roleClaimNeedsGrant[assigned] && !hasAgentRoleGrant(&ContributorProfile{AgentRoleGrants: grants}, assigned) {
		grants = normalizeUniqueAgentRoles(append(grants, assigned))
	}
	p.AgentRoleGrants = grants
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	if s.contributeHub != nil {
		s.contributeHub.SetContributorAgentRoleGrants(p.ContributorID, grants)
	}
	s.logger.Info("contributor agent-role grants changed", "username", p.GitHubUsername, "grants", strings.Join(grants, ","))
	jsonResponse(w, map[string]any{"ok": true, "agent_role_grants": grants, "grantable_roles": grantable})
}

// handleContributorAgentRole assigns a contributor's agent role.
//
// OWNER-ONLY (issue #3011, High). This was gated on requireContributorWrite,
// which admits any caller whose role is not "read" — so a read-write
// contributor could assign privileged agent roles. Its sibling
// handleContributorAgentRoleGrants was moved to requireOwnerRole by F16; this
// half of #3011 was left behind. Role assignment is an operator action.
//
// The handler also used to PRE-POPULATE its probe profile with the very grant
// it was about to check:
//
//	probeProfile := *p
//	if roleClaimNeedsGrant[role] && !hasAgentRoleGrant(&probeProfile, role) {
//	    probeProfile.AgentRoleGrants = append(probeProfile.AgentRoleGrants, role)
//	}
//
// which made roleClaimAllowed's "requires an operator grant" check pass
// trivially, and then wrote that grant to the REAL profile — auto-granting
// sec-check/architect/ci-maintainer as a side effect of assigning them. That
// is removed: the probe is now the unmodified profile, so a role needing a
// grant is REFUSED unless the target already holds it. Grant issuance stays a
// separate explicit owner action (handleContributorAgentRoleGrants).
//
// Assignment is additionally checked against the server-side assignable
// allowlist, so a role outside contributorAgentRoleAssignableRoles cannot be
// assigned even when roleClaimAllowed would tolerate it.
func (s *Server) handleContributorAgentRole(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	var req struct {
		AgentRole string `json:"agent_role"`
		Role      string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "Invalid request", http.StatusBadRequest)
		return
	}
	role := normalizeAgentRole(req.AgentRole)
	if role == "" {
		role = normalizeAgentRole(req.Role)
	}
	if role == "" || role == "none" {
		p.AssignedAgentRole = "none"
	} else {
		// Server-side allowlist: only roles the hive actually exposes as
		// assignable may be assigned (issue #3011 recommendation 3).
		var allowCfg *config.Config
		if s.deps != nil {
			allowCfg = s.deps.Config
		}
		assignable := false
		for _, candidate := range contributorAgentRoleAssignableRoles(allowCfg) {
			if candidate == role {
				assignable = true
				break
			}
		}
		if !assignable {
			jsonError(w, fmt.Sprintf("agent role %q is not assignable", role), http.StatusBadRequest)
			return
		}

		// Probe with the profile AS IT IS. Pre-seeding the grant here was the
		// #3011 bypass: it made the grant check below tautological.
		probe := &ContributorConnection{profile: p}
		if s.contributeHub == nil {
			s.contributeHub = NewContributeWSHub(s.logger, s)
		}
		if ok, reason := s.contributeHub.roleClaimAllowed(probe, role); !ok {
			jsonError(w, reason, http.StatusBadRequest)
			return
		}
		// Belt and braces: roleClaimAllowed returns early for the no-config
		// hubs used by unit tests and legacy deployments, so re-assert the
		// grant requirement here unconditionally. No grant is ever WRITTEN by
		// this handler — that is handleContributorAgentRoleGrants' job.
		if roleClaimNeedsGrant[role] && !hasAgentRoleGrant(p, role) {
			jsonError(w, fmt.Sprintf("agent role %q requires an operator grant", role), http.StatusBadRequest)
			return
		}
		p.AssignedAgentRole = role
	}
	if err := saveContributorProfile(p); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	if s.contributeHub != nil {
		s.contributeHub.SetAssignedAgentRole(p.ContributorID, p.AssignedAgentRole, p.AgentRoleGrants)
	}
	s.logger.Info("contributor assigned agent role changed", "username", p.GitHubUsername, "assigned_role", p.AssignedAgentRole)
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	jsonResponse(w, map[string]any{
		"ok":                  true,
		"assigned_agent_role": p.AssignedAgentRole,
		"effective_role":      effectiveAssignedAgentRole(p.AssignedAgentRole),
		"agent_role_grants":   p.AgentRoleGrants,
		"assignable_roles":    contributorAgentRoleAssignableRoles(cfg),
	})
}

func normalizeUniqueAgentRoles(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, role := range in {
		role = normalizeAgentRole(role)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		out = append(out, role)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleContributorRevoke(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	p.TrustTier = "revoked"
	// H2: admin path — adminOverride wins the CAS reconcile so this revoke cannot be
	// silently clobbered by a concurrent stale WS save, and the tier is now
	// server-authoritative and terminal for any later non-admin write.
	if err := saveContributorProfileCAS(p, true); err != nil {
		jsonError(w, "Failed to save", http.StatusInternalServerError)
		return
	}
	// H2: fence live sessions — close any WebSocket connections this contributor
	// holds so an in-flight session cannot keep working, keep minting credentials, or
	// keep saving a stale non-revoked profile after the revoke lands.
	if s.contributeHub != nil {
		s.contributeHub.DisconnectContributor(p.ContributorID, "contribution access revoked")
	}
	s.logger.Info("contributor revoked", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true})
}

// handleContributorRequeue is the operator YANK action — the manual release of a
// wedged clanker's in-flight task, repurposed (kubestellar/hive#2568 + the yank
// follow-up) to ALSO immediately reassign that clanker its next-priority item so it
// keeps working instead of idling. It is a CONTROL, so it is owner/read-write ONLY,
// enforced server-side by requireContributorWrite (a read/anon caller gets 403),
// exactly like trust/revoke/remove.
//
// It still reuses the SAME release+cooldown machinery the automatic disconnect-release
// (#2356/#2435) and ready-abandon (#2545) paths use — see
// ContributeWSHub.RequeueContributorTask — so the release can NOT recreate the
// duplicate-assignment race #2492/#2557 closed: the released issue books the same short
// failure cooldown and is therefore not instantly re-handed to a stale worker, and the
// connection's assignment generation is BUMPED so a stale worker's later completion is
// fenced out (the Gate).
//
// The YANK addition: after that release, the hub immediately calls selectTask for the
// SAME clanker and hands it its next-priority item (honouring the operator-pinned → own
// work → label-affinity → fewer-failures → rest order), and the just-released issue is
// briefly self-excluded from THIS clanker (yankSelfExcludeSeconds) so it moves to
// genuinely DIFFERENT work — while the released issue is immediately offerable to every
// OTHER contributor. When nothing else is admissible the clanker is simply released +
// idle (the old requeue-only outcome, now the fallback). The operator may pass a REASON
// (JSON body {"reason":...} or ?reason=), recorded in the audit + activity log and
// pushed to the still-connected worker on task_revoke. A contributor with no in-flight
// task is a 404 (nothing to release/reassign).
func (s *Server) handleContributorRequeue(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "Contributor relay is not available", http.StatusServiceUnavailable)
		return
	}
	// #2568: accept an optional operator reason from a JSON body or query param. Both
	// are optional — an empty reason falls back to the hub's default recovery label —
	// so existing callers that POST no body keep working unchanged.
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if reason == "" && r.Body != nil {
		var body struct {
			Reason string `json:"reason"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			reason = strings.TrimSpace(body.Reason)
		}
	}
	// Key the live release+reassign by the registered ContributorID (what the ops tab
	// passes), matching how the hub tracks connections. GitHubUsername is only for logs.
	released, assigned := s.contributeHub.RequeueContributorTask(p.ContributorID, reason)
	if released == 0 {
		jsonError(w, "That contributor has no in-flight task to yank.", http.StatusNotFound)
		return
	}
	s.auditFromRequest(r, "contributor_requeue", auditDetail("username", p.GitHubUsername, "reason", reason), "")
	// Report whether the clanker was reassigned (and to what) so the ops tab can show
	// the clanker was moved to different work. reassigned==false means it was released
	// but nothing else was admissible right now — a legitimate "released, now idle" state.
	resp := map[string]any{"ok": true, "released": released, "reassigned": false}
	if assigned != nil && assigned.Type == "task_assign" {
		resp["reassigned"] = true
		resp["assigned_repo"] = assigned.Repo
		resp["assigned_number"] = assigned.Number
		resp["assigned_title"] = assigned.Title
	}
	s.logger.Info("contributor task yanked by operator", "username", p.GitHubUsername, "sessions_released", released, "reassigned", resp["reassigned"], "reason", reason)
	jsonResponse(w, resp)
}

func (s *Server) handleContributorDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := r.PathValue("id")
	p := findContributor(id)
	if p == nil {
		jsonError(w, "Contributor not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(getContributorsDir(), p.GitHubUsername+".json")
	if err := os.Remove(path); err != nil {
		jsonError(w, "Failed to delete", http.StatusInternalServerError)
		return
	}
	s.logger.Info("contributor deleted", "username", p.GitHubUsername)
	jsonResponse(w, map[string]any{"ok": true, "deleted": p.GitHubUsername})
}
