package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

// Issue claims (hivecommons/hive#8380).
//
// The worker-claim ledger records who is actively working an issue — a human
// session, a hub-kicked agent, a relay contributor ("clanker"), or an external
// author — ranked human > agent > contributor > external. The dashboard is the
// hub-side seam: it exposes the ledger over HTTP for hivectl and the UI,
// auto-records a contributor claim whenever the relay hands out a task,
// releases it with the lease, excludes claimed items from selectTask, and
// turns a takeover into a targeted yank so the displaced clanker stops and
// picks up different work.

const (
	claimsOwnerFallbackHolder = "owner"
	claimsRepoSep             = "/"
)

// claimsLedger returns the worker-claim ledger or nil when the feature is off.
func (s *Server) claimsLedger() *claims.Ledger {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.IssueClaims
}

// IssueClaims exposes the worker-claim ledger to cmd/hive seams that only
// hold the dashboard (the eval cycle's kick delivery hook). Nil when off.
func (s *Server) IssueClaims() *claims.Ledger { return s.claimsLedger() }

func (h *ContributeWSHub) claimsLedger() *claims.Ledger {
	if h == nil || h.server == nil {
		return nil
	}
	return h.server.claimsLedger()
}

func (s *Server) registerClaimsRoutes() {
	s.mux.HandleFunc("GET /api/claims", s.handleClaimsList)
	s.mux.HandleFunc("GET /api/claims/{owner}/{repo}/{number}", s.handleClaimGet)
	s.mux.HandleFunc("POST /api/claims/{owner}/{repo}/{number}", s.handleClaimCreate)
	s.mux.HandleFunc("DELETE /api/claims/{owner}/{repo}/{number}", s.handleClaimRelease)
}

// InstallClaimHooks wires the ledger's takeover hook to the relay yank so a
// contributor displaced by a higher-ranked claim is told to stop and is handed
// other work at once. cmd/hive calls it once after the ledger is constructed;
// the GitHub-facing side effects (comments, labels) are chained by the caller.
func (s *Server) InstallClaimHooks(base claims.Hooks) {
	l := s.claimsLedger()
	if l == nil {
		return
	}
	prevTaken := base.OnTakenOver
	base.OnTakenOver = func(now, previous claims.Claim) {
		if prevTaken != nil {
			prevTaken(now, previous)
		}
		s.preemptDisplacedHolder(now, previous)
	}
	l.SetHooks(base)
}

// preemptDisplacedHolder is the "stop work and move on" signal for a relay
// contributor whose claim was taken over. Agents and humans are told via the
// GitHub comment; only a relay session has a live channel the hub can yank.
func (s *Server) preemptDisplacedHolder(now, previous claims.Claim) {
	if previous.Kind != claims.KindContributor || s.contributeHub == nil {
		return
	}
	reason := fmt.Sprintf("preempted: %s claimed by %s (%s)", previous.Key(), now.Holder, now.Kind)
	released, assigned := s.contributeHub.PreemptContributorIssue(previous.HolderID, previous.Repo, previous.Issue, reason)
	if released == 0 {
		// No live socket to yank (#4260 resumable-lease case): revoke the lease
		// itself so a reconnecting relay cannot resume the item it lost.
		if s.contributeHub.revokeLeaseForKey(previous.HolderID, previous.Key()) {
			s.logger.Info("[claims] revoked lease of a disconnected contributor after claim takeover",
				"issue", previous.Key(), "holder_id", previous.HolderID)
		}
	}
	next := ""
	if assigned != nil && assigned.Type == "task_assign" {
		next = assigned.TaskKey
		if next == "" {
			next = claims.Key(assigned.Repo, assigned.Number)
		}
	}
	s.logger.Info("[claims] contributor preempted by claim takeover",
		"issue", previous.Key(), "holder", previous.Holder, "holder_id", previous.HolderID,
		"by", now.Holder, "by_kind", now.Kind, "released", released, "reassigned_to", next)
	s.AuditLog(now.Holder, "claim_preempt", fmt.Sprintf("%s taken from %s (%s); released=%d next=%s",
		previous.Key(), previous.Holder, previous.Kind, released, next), "")
}

// claimIssueForContributor records a contributor claim after the relay has
// leased an item to a connection. Best-effort: a ledger error never blocks the
// assignment — the lease is the source of truth for the relay; the claim is
// what makes the hold visible to humans, agents and other hives.
func (h *ContributeWSHub) claimIssueForContributor(c *ContributorConnection, repoFull string, number int) {
	l := h.claimsLedger()
	if l == nil || c == nil || c.profile == nil || number <= 0 || repoFull == "" {
		return
	}
	holder := c.profile.GitHubUsername
	if holder == "" {
		holder = c.profile.ContributorID
	}
	res, err := l.Claim(claims.Request{
		Repo: repoFull, Issue: number,
		Holder: holder, HolderID: identityOf(c), Kind: claims.KindContributor,
		Session: c.session,
	})
	if err != nil {
		h.logger.Warn("[claims] contributor claim not recorded", "repo", repoFull, "number", number, "holder", holder, "error", err)
		return
	}
	if !res.Outcome.Changed() {
		// selectTask already excluded held items; reaching here means a
		// higher-ranked holder claimed it between exclusion and lease. Log it —
		// the ledger keeps the higher claim and the next takeover hook or
		// expiry sorts the relay out.
		h.logger.Warn("[claims] contributor assigned an item another holder claims",
			"repo", repoFull, "number", number, "holder", holder, "held_by", res.Claim.Holder, "held_kind", res.Claim.Kind)
	}
}

// renewClaimForLease extends the contributor claim alongside a lease renewal.
// Same holder → OutcomeRenewed; if someone else has since claimed the item the
// ledger keeps their claim (the relay will be preempted or refused on its own).
func (h *ContributeWSHub) renewClaimForLease(identity, repo string, number int) {
	l := h.claimsLedger()
	if l == nil || identity == "" || number <= 0 {
		return
	}
	c, ok := l.LookupKey(claims.Key(repo, number))
	if !ok || c.HolderID != identity {
		return
	}
	if _, err := l.Claim(claims.Request{Repo: c.Repo, Issue: c.Issue, Holder: c.Holder, HolderID: c.HolderID,
		Kind: c.Kind, Session: c.Session}); err != nil {
		h.logger.Warn("[claims] contributor claim not renewed", "issue", c.Key(), "holder", c.Holder, "error", err)
	}
}

// releaseClaimForLease drops the contributor claim that travelled with a lease
// when the lease is revoked (task done, abandoned, yanked, or expired).
func (h *ContributeWSHub) releaseClaimForLease(identity, key, reason string) {
	l := h.claimsLedger()
	if l == nil || identity == "" {
		return
	}
	for _, c := range l.ReleaseByHolderID(identity, reason, key) {
		h.logger.Info("[claims] contributor claim released with lease", "issue", c.Key(), "holder", c.Holder, "reason", reason)
	}
}

// claimedIssueKeys returns the canonical keys of every issue held by a claim
// that does NOT belong to the given identity, for selectTask's exclusion set.
func (h *ContributeWSHub) claimedIssueKeys(exceptIdentity string) map[string]bool {
	l := h.claimsLedger()
	if l == nil {
		return nil
	}
	return l.HeldKeys(exceptIdentity)
}

// ---- HTTP ----

type claimRequestBody struct {
	Force   bool   `json:"force"`
	TTLS    int    `json:"ttl_s"`
	Session string `json:"session"`
	Reason  string `json:"reason"`
}

// claimCaller resolves the server-verified identity behind a claim mutation.
// A signed-in / hub-proxied / GitHub-token caller is a human under their own
// login. A shared-token operator (verified owner role, no login) claims as the
// owner. Anonymous callers get "".
func (s *Server) claimCaller(r *http.Request) (holder string, owner bool) {
	owner = isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true"
	if u := s.resolveContributeCaller(r); u != "" {
		return u, owner
	}
	if owner {
		if u := strings.TrimSpace(r.Header.Get("X-Hive-User")); u != "" {
			return u, true
		}
		return claimsOwnerFallbackHolder, true
	}
	return "", false
}

func claimPathRef(r *http.Request) (repo string, number int, err error) {
	owner, name := strings.TrimSpace(r.PathValue("owner")), strings.TrimSpace(r.PathValue("repo"))
	number, convErr := strconv.Atoi(strings.TrimSpace(r.PathValue("number")))
	if owner == "" || name == "" || convErr != nil || number <= 0 {
		return "", 0, fmt.Errorf("path must be /api/claims/{owner}/{repo}/{number}")
	}
	return owner + claimsRepoSep + name, number, nil
}

func (s *Server) handleClaimsList(w http.ResponseWriter, r *http.Request) {
	l := s.claimsLedger()
	if l == nil {
		writeClaimsJSON(w, http.StatusOK, map[string]any{"enabled": false, "claims": []claims.Claim{}})
		return
	}
	list := l.List()
	if list == nil {
		list = []claims.Claim{}
	}
	writeClaimsJSON(w, http.StatusOK, map[string]any{"enabled": true, "claims": list})
}

func (s *Server) handleClaimGet(w http.ResponseWriter, r *http.Request) {
	repo, number, err := claimPathRef(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	l := s.claimsLedger()
	if l == nil {
		jsonError(w, "issue claims are not enabled on this hive", http.StatusNotFound)
		return
	}
	c, ok := l.Lookup(repo, number)
	if !ok {
		writeClaimsJSON(w, http.StatusOK, map[string]any{"held": false})
		return
	}
	writeClaimsJSON(w, http.StatusOK, map[string]any{"held": true, "claim": c})
}

func (s *Server) handleClaimCreate(w http.ResponseWriter, r *http.Request) {
	repo, number, err := claimPathRef(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	l := s.claimsLedger()
	if l == nil {
		jsonError(w, "issue claims are not enabled on this hive", http.StatusNotFound)
		return
	}
	holder, _ := s.claimCaller(r)
	if holder == "" {
		jsonError(w, "Sign in with GitHub (or use an owner token) to claim an issue.", http.StatusUnauthorized)
		return
	}
	var body claimRequestBody
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	res, err := l.Claim(claims.Request{
		Repo: repo, Issue: number, Holder: holder, Kind: claims.KindHuman,
		Session: strings.TrimSpace(body.Session), Force: body.Force,
		TTL: time.Duration(body.TTLS) * time.Second,
	})
	if err != nil {
		jsonError(w, "claim failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if !res.Outcome.Changed() {
		status = http.StatusConflict
	} else {
		s.AuditLog(holder, "claim_"+string(res.Outcome), res.Claim.Key(), "")
	}
	writeClaimsJSON(w, status, claimResultJSON(res))
}

func (s *Server) handleClaimRelease(w http.ResponseWriter, r *http.Request) {
	repo, number, err := claimPathRef(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	l := s.claimsLedger()
	if l == nil {
		jsonError(w, "issue claims are not enabled on this hive", http.StatusNotFound)
		return
	}
	holder, owner := s.claimCaller(r)
	if holder == "" {
		jsonError(w, "Sign in with GitHub (or use an owner token) to release a claim.", http.StatusUnauthorized)
		return
	}
	var body claimRequestBody
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "released by " + holder
	}
	c, ok, rerr := l.Release(repo, number, holder, claims.KindHuman, reason)
	if !ok && rerr != nil && owner && body.Force {
		c, ok = l.ForceRelease(repo, number, reason+" (owner override)")
		rerr = nil
	}
	switch {
	case rerr != nil:
		writeClaimsJSON(w, http.StatusConflict, map[string]any{"released": false, "claim": c, "error": rerr.Error()})
	case !ok:
		writeClaimsJSON(w, http.StatusOK, map[string]any{"released": false})
	default:
		s.AuditLog(holder, "claim_release", c.Key()+" from "+c.Holder, "")
		writeClaimsJSON(w, http.StatusOK, map[string]any{"released": true, "claim": c})
	}
}

// writeClaimsJSON is the JSON responder for the /api/claims routes.
func writeClaimsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func claimResultJSON(res claims.Result) map[string]any {
	out := map[string]any{"outcome": res.Outcome, "changed": res.Outcome.Changed(), "claim": res.Claim}
	if res.Previous != nil {
		out["previous"] = *res.Previous
	}
	switch res.Outcome {
	case claims.OutcomeHeld:
		out["hint"] = fmt.Sprintf("held by %s (%s) at the same rank — re-run with force to take it over", res.Claim.Holder, res.Claim.Kind)
	case claims.OutcomeRefused:
		out["hint"] = fmt.Sprintf("held by %s (%s), which outranks you", res.Claim.Holder, res.Claim.Kind)
	}
	return out
}
