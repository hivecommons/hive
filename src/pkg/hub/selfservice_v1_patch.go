package hub

// Hub registration protocol v1, rollout step 1: PATCH of a registration's
// repos and dashboard_url (hivecommons/hive#18642, #19056).
//
// PATCH /api/hub/v1/registrations/{id} edits a registration in place. The
// heartbeat key is never touched, so an edit causes no outage: before this,
// adding a repo meant deleting the registration (revoking its key) and
// registering again.
//
// Invariants (in addition to selfservice_v1_api.go's):
//   - The ordinary edit requires the hub session owner. A hub admin who does
//     not own the hive gets 404, the same answer as for a missing hive. The
//     separately gated sync-repos operation uses the registration key.
//   - If-Match is required (428 when absent, 412 when stale). The precondition
//     is checked before any GitHub call and again inside the store lock.
//   - Only repos the edit ADDS are proven (the same read-only
//     GET /repos/{owner}/{repo} as create, with the owner's stored token).
//     Repos that stay keep their proof time; removed repos lose theirs.
//   - org, the owner, the ID, the keys and every timestamp are not editable:
//     a body naming any other field is a 400, and the store restores them
//     after the edit regardless.
//   - The response and the audit line carry no key and no hash. Nothing here
//     writes to GitHub.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
)

const (
	hubV1PatchFieldRepos        = "repos"
	hubV1PatchFieldDashboardURL = "dashboard_url"
	hubV1PatchFieldOrg          = "org"
)

// hubV1RepoDelta is the {add, remove} form of the repos field. The other form
// is a full list that replaces the registration's repos.
type hubV1RepoDelta struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// hubV1PatchRefusal is a refusal the planning step produces: a status and the
// JSON body to send with it.
type hubV1PatchRefusal struct {
	status int
	body   map[string]any
}

func hubV1Refuse(status int, format string, args ...any) *hubV1PatchRefusal {
	return &hubV1PatchRefusal{status: status, body: map[string]any{"error": fmt.Sprintf(format, args...)}}
}

// hubV1PatchPlan is a validated edit, relative to the record it was planned
// against.
type hubV1PatchPlan struct {
	repos            []string
	added            []string
	removed          []string
	dashboardURL     string
	dashboardChanged bool
}

func (p hubV1PatchPlan) changed() bool {
	return len(p.added) > 0 || len(p.removed) > 0 || p.dashboardChanged
}

// hubV1PlanPatch validates a PATCH body against the record h and returns the
// edit it describes. It makes no GitHub call and writes nothing.
func hubV1PlanPatch(ctx context.Context, h SelfServiceHive, data []byte) (hubV1PatchPlan, *hubV1PatchRefusal) {
	plan := hubV1PatchPlan{repos: h.Repos, dashboardURL: h.DashboardURL}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return plan, hubV1Refuse(http.StatusBadRequest, "the body must be a JSON object with %q and/or %q", hubV1PatchFieldRepos, hubV1PatchFieldDashboardURL)
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch name {
		case hubV1PatchFieldRepos, hubV1PatchFieldDashboardURL:
		case hubV1PatchFieldOrg:
			return plan, hubV1Refuse(http.StatusBadRequest, "org cannot be changed on a registration: changing it would mean proving every repo again; delete the registration and register it again")
		default:
			return plan, hubV1Refuse(http.StatusBadRequest, "field %q cannot be changed; a registration accepts %q and %q", name, hubV1PatchFieldRepos, hubV1PatchFieldDashboardURL)
		}
	}
	if len(fields) == 0 {
		return plan, hubV1Refuse(http.StatusBadRequest, "send %q and/or %q", hubV1PatchFieldRepos, hubV1PatchFieldDashboardURL)
	}

	if raw, ok := fields[hubV1PatchFieldRepos]; ok {
		if refusal := hubV1PlanRepos(&plan, h, raw); refusal != nil {
			return plan, refusal
		}
	}
	if raw, ok := fields[hubV1PatchFieldDashboardURL]; ok {
		var requested string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &requested) != nil {
			return plan, hubV1Refuse(http.StatusBadRequest, "%s must be a string; send an empty string to clear it", hubV1PatchFieldDashboardURL)
		}
		validated, err := selfServiceValidateDashboardURL(ctx, requested)
		if err != nil {
			return plan, hubV1Refuse(http.StatusBadRequest, "%s", err.Error())
		}
		plan.dashboardURL = validated
		plan.dashboardChanged = validated != h.DashboardURL
	}
	return plan, nil
}

// hubV1PlanRepos fills plan's repo list and its added and removed sets from
// the repos field: a JSON list replaces the repos, an {add, remove} object
// changes them. Existing repos keep their stored spelling.
func hubV1PlanRepos(plan *hubV1PatchPlan, h SelfServiceHive, raw json.RawMessage) *hubV1PatchRefusal {
	shapeError := hubV1Refuse(http.StatusBadRequest, "%s must be a list of owner/name repos or an object with add and remove lists", hubV1PatchFieldRepos)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return shapeError
	}
	var requested []string
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(trimmed, &requested); err != nil {
			return shapeError
		}
	case '{':
		var delta hubV1RepoDelta
		if err := decodeStrict(trimmed, &delta); err != nil {
			return hubV1Refuse(http.StatusBadRequest, "%s takes only add and remove lists of owner/name repos", hubV1PatchFieldRepos)
		}
		var refusal *hubV1PatchRefusal
		requested, refusal = hubV1ApplyRepoDelta(h.Repos, delta)
		if refusal != nil {
			return refusal
		}
	default:
		return shapeError
	}

	if maxRepos := selfServiceMaxRepos(); len(requested) > maxRepos {
		return &hubV1PatchRefusal{
			status: http.StatusRequestEntityTooLarge,
			body:   hubV1RepoCapBody(fmt.Errorf("at most %d repos per registration", maxRepos), maxRepos),
		}
	}
	validated, err := selfServiceValidateRepos(requested)
	if err != nil {
		return hubV1Refuse(http.StatusBadRequest, "%s", err.Error())
	}

	current := make(map[string]string, len(h.Repos))
	for _, repo := range h.Repos {
		current[strings.ToLower(repo)] = repo
	}
	keep := make(map[string]struct{}, len(validated))
	plan.repos = make([]string, 0, len(validated))
	plan.added = nil
	for _, repo := range validated {
		key := strings.ToLower(repo)
		keep[key] = struct{}{}
		if existing, ok := current[key]; ok {
			plan.repos = append(plan.repos, existing)
			continue
		}
		plan.repos = append(plan.repos, repo)
		plan.added = append(plan.added, repo)
	}
	plan.removed = nil
	for _, repo := range h.Repos {
		if _, ok := keep[strings.ToLower(repo)]; !ok {
			plan.removed = append(plan.removed, repo)
		}
	}
	return nil
}

// hubV1ApplyRepoDelta returns the repo list after applying delta to current.
// A removal of a repo the registration does not list is ignored, so a retried
// request is harmless; adding and removing the same repo is refused.
func hubV1ApplyRepoDelta(current []string, delta hubV1RepoDelta) ([]string, *hubV1PatchRefusal) {
	remove := make(map[string]struct{}, len(delta.Remove))
	for _, repo := range delta.Remove {
		remove[strings.ToLower(strings.TrimSpace(repo))] = struct{}{}
	}
	out := make([]string, 0, len(current)+len(delta.Add))
	for _, repo := range current {
		if _, drop := remove[strings.ToLower(repo)]; !drop {
			out = append(out, repo)
		}
	}
	for _, raw := range delta.Add {
		repo := strings.TrimSpace(raw)
		if _, drop := remove[strings.ToLower(repo)]; drop {
			return nil, hubV1Refuse(http.StatusBadRequest, "repo %q is in both add and remove", repo)
		}
		if !hubV1ContainsFold(out, repo) {
			out = append(out, repo)
		}
	}
	return out, nil
}

func hubV1ContainsFold(repos []string, repo string) bool {
	for _, existing := range repos {
		if strings.EqualFold(existing, repo) {
			return true
		}
	}
	return false
}

// hubV1SameRepos reports whether two repo lists name the same repos in the
// same order, ignoring case.
func hubV1SameRepos(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// writeHubV1Stale answers 412 with the current ETag so the client can retry.
func (s *HubServer) writeHubV1Stale(w http.ResponseWriter, id string) {
	current, _ := s.selfService.get(id)
	writeHubV1JSON(w, http.StatusPreconditionFailed, selfServiceETag(current), map[string]any{
		"error": "the registration changed since you read it; GET it again and retry",
	})
}

// handleHubV1Patch is PATCH /api/hub/v1/registrations/{id}: change repos
// and/or dashboard_url with the key kept. 200 {registration} with the new
// ETag; an edit that changes nothing answers 200 with the current ETag and
// does not bump the revision.
func (s *HubServer) handleHubV1Patch(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	h, ok := s.selfServiceOwnedHive(w, r, login, false)
	if !ok {
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch == "" {
		writeHubV1JSON(w, http.StatusPreconditionRequired, selfServiceETag(h), map[string]any{
			"error": "If-Match is required: GET the registration and send its ETag",
		})
		return
	}
	if !selfServiceETagMatches(h, ifMatch) {
		s.writeHubV1Stale(w, h.ID)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, selfServiceRequestBodyLimit()))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	plan, refusal := hubV1PlanPatch(r.Context(), h, data)
	if refusal != nil {
		s.auditSelfServiceRefused(login, h.ID, selfServiceRefusedInvalidUpdate)
		writeHubV1JSON(w, refusal.status, "", refusal.body)
		return
	}
	if !plan.changed() {
		writeHubV1JSON(w, http.StatusOK, selfServiceETag(h), map[string]any{"registration": s.hubV1View(h)})
		return
	}
	if len(plan.added) > 0 {
		user := loadSaaSUser(login)
		if user == nil {
			writeJSONError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		if !s.selfServiceProveOwnership(w, r, login, h.ID, user, plan.added) {
			return
		}
	}

	now := s.selfService.clock().UTC()
	stored, err := s.selfService.update(h.ID, ifMatch, func(n *SelfServiceHive) error {
		// An If-Match of "*" skips the ETag comparison, so make sure the repos
		// this plan was computed from are still the stored ones.
		if !hubV1SameRepos(n.Repos, h.Repos) {
			return errSelfServiceStale
		}
		n.Repos = plan.repos
		n.RepoProofs = selfServiceProofTimes(n.RepoProofs, plan.repos, n.OwnershipCheckedAt, now, plan.added)
		if len(n.PendingProof) > 0 && len(plan.added) > 0 {
			n.PendingProof = slices.DeleteFunc(n.PendingProof, func(repo string) bool {
				return hubV1ContainsFold(plan.added, repo)
			})
		}
		n.DashboardURL = plan.dashboardURL
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errSelfServiceNotFound):
			writeJSONError(w, http.StatusNotFound, "self-hosted hive not found")
		case errors.Is(err, errSelfServiceStale):
			s.writeHubV1Stale(w, h.ID)
		default:
			s.logger.Error("self-service update: store write failed", "user", login, "hive", h.ID, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "could not persist the change")
		}
		return
	}
	s.logger.Info("audit: selfservice_hive_updated", "user", login, "hive", h.ID,
		"repos_added", len(plan.added), "repos_removed", len(plan.removed),
		"dashboard_changed", plan.dashboardChanged, "revision", stored.Revision)
	writeHubV1JSON(w, http.StatusOK, selfServiceETag(stored), map[string]any{"registration": s.hubV1View(stored)})
}
