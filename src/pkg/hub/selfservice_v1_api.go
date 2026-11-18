package hub

// Hub registration protocol v1, rollout step 1 (hivecommons/hive#18642).
//
// /api/hub/v1 is a versioned resource view of the self-service (byo-)
// registry that selfservice_api.go already serves under /api/hub/selfservice.
// Step 1 adds two things the v6 routes cannot do:
//
//   - GET /api/hub/v1 discovery, so a client learns the operations, limits and
//     proof methods before it hits a 4xx.
//   - PATCH /api/hub/v1/registrations/{id}, which edits repos and
//     dashboard_url IN PLACE. The heartbeat key is not touched, so adding a
//     repo no longer means delete, re-register, re-key and restart the spoke.
//
// Create, list, delete and rotate are the v6 handlers' logic under resource
// paths, plus an ETag on every record and If-Match on every write. The v6
// routes keep their bodies and status codes unchanged.
//
// Invariants (in addition to selfservice_api.go's):
//   - Nothing here writes to GitHub. The only GitHub call is the read-only
//     ownership check (GET /repos/{owner}/{repo}) and it runs only for repos a
//     PATCH ADDS; repos already on the record keep their earlier proof.
//   - A PATCH can change Repos and DashboardURL only; selfServiceStore.update
//     restores the key hashes, owner, ID and timestamps after the edit.
//   - Every write carries an ETag precondition check inside the store lock, so
//     two operators editing at once get 412, never a silent overwrite.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// hubV1BasePath is the root of the versioned registration API.
	hubV1BasePath = "/api/hub/v1"
	// hubV1Protocol names the protocol discovery describes.
	hubV1Protocol = "hive-hub-registration"
	// hubV1ProtocolVersion is the protocol version discovery advertises.
	hubV1ProtocolVersion = "1"
	// hubV1ProofHubSessionToken is the only ownership proof step 1 offers: the
	// hub checks permissions.admin on each repo with the GitHub token stored at
	// hub sign-in. The RFC's github-user-token (a per-request token with
	// explicit scopes) is rollout step 2 and is not advertised until it exists.
	hubV1ProofHubSessionToken = "hub-session-token"
)

// hubV1Operation is one entry in the discovery operations list.
type hubV1Operation struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// IfMatch is "required", "optional" or empty (not a write).
	IfMatch string `json:"if_match,omitempty"`
}

// hubV1Operations is what this hub implements. Discovery lists exactly this.
var hubV1Operations = []hubV1Operation{
	{Name: "discovery", Method: http.MethodGet, Path: hubV1BasePath},
	{Name: "create", Method: http.MethodPost, Path: hubV1BasePath + "/registrations"},
	{Name: "list", Method: http.MethodGet, Path: hubV1BasePath + "/registrations"},
	{Name: "get", Method: http.MethodGet, Path: hubV1BasePath + "/registrations/{id}"},
	{Name: "update", Method: http.MethodPatch, Path: hubV1BasePath + "/registrations/{id}", IfMatch: "required"},
	{Name: "rotate_key", Method: http.MethodPost, Path: hubV1BasePath + "/registrations/{id}/keys", IfMatch: "optional"},
	{Name: "delete", Method: http.MethodDelete, Path: hubV1BasePath + "/registrations/{id}", IfMatch: "optional"},
}

// hubV1PatchFields are the registration fields PATCH may change.
var hubV1PatchFields = []string{"repos", "dashboard_url"}

// registerHubV1Routes wires the v1 routes onto the hub mux. Discovery is
// public (a spoke reads it before it has a session); every other route is
// behind the hub session like the v6 routes. All of them answer 404 while
// HIVE_HUB_SELFSERVICE_REGISTRATION is off.
func (s *HubServer) registerHubV1Routes() {
	s.mux.HandleFunc("GET "+hubV1BasePath, s.handleHubV1Discovery)
	s.mux.HandleFunc("POST "+hubV1BasePath+"/registrations", s.requireAuth(s.handleHubV1Create))
	s.mux.HandleFunc("GET "+hubV1BasePath+"/registrations", s.requireAuth(s.handleHubV1List))
	s.mux.HandleFunc("GET "+hubV1BasePath+"/registrations/{id}", s.requireAuth(s.handleHubV1Get))
	s.mux.HandleFunc("PATCH "+hubV1BasePath+"/registrations/{id}", s.requireAuth(s.handleHubV1Patch))
	s.mux.HandleFunc("DELETE "+hubV1BasePath+"/registrations/{id}", s.requireAuth(s.handleHubV1Delete))
	s.mux.HandleFunc("POST "+hubV1BasePath+"/registrations/{id}/keys", s.requireAuth(s.handleHubV1RotateKey))
}

// hubV1RegistrationView is a v6 view plus the record's ETag, so a list
// response carries a usable precondition for every item, and the per-repo
// proof times.
type hubV1RegistrationView struct {
	selfServiceHiveView
	ETag       string           `json:"etag"`
	RepoProofs []hubV1RepoProof `json:"repo_proofs"`
}

// hubV1RepoProof is when the owner's admin permission on one repo was last
// proven.
type hubV1RepoProof struct {
	Repo      string    `json:"repo"`
	CheckedAt time.Time `json:"checked_at"`
}

func (s *HubServer) hubV1View(h SelfServiceHive) hubV1RegistrationView {
	proofs := make([]hubV1RepoProof, 0, len(h.Repos))
	for _, repo := range h.Repos {
		at, ok := h.RepoProofs[strings.ToLower(repo)]
		if !ok {
			// A record from before per-repo proofs: every repo was proven
			// when the record was.
			at = h.OwnershipCheckedAt
		}
		proofs = append(proofs, hubV1RepoProof{Repo: repo, CheckedAt: at})
	}
	return hubV1RegistrationView{
		selfServiceHiveView: selfServiceView(h, s.selfServiceOnline(h.ID)),
		ETag:                selfServiceETag(h),
		RepoProofs:          proofs,
	}
}

// writeHubV1RepoCapError is the v1 over-cap answer for create and PATCH: 413
// naming the limit, so a client can read it without parsing the message.
func writeHubV1RepoCapError(w http.ResponseWriter, err error, maxRepos int) {
	writeHubV1JSON(w, http.StatusRequestEntityTooLarge, "", hubV1RepoCapBody(err, maxRepos))
}

func hubV1RepoCapBody(err error, maxRepos int) map[string]any {
	return map[string]any{"error": err.Error(), "limit": "max_repos", "max_repos": maxRepos}
}

func writeHubV1JSON(w http.ResponseWriter, code int, etag string, body any) {
	w.Header().Set("Content-Type", "application/json")
	if etag != "" {
		w.Header().Set("ETag", etag)
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// handleHubV1Discovery is GET /api/hub/v1.
func (s *HubServer) handleHubV1Discovery(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	writeHubV1JSON(w, http.StatusOK, "", map[string]any{
		"protocol":     hubV1Protocol,
		"version":      hubV1ProtocolVersion,
		"base_path":    hubV1BasePath,
		"operations":   hubV1Operations,
		"patch_fields": hubV1PatchFields,
		"limits": map[string]any{
			"max_repos":            selfServiceMaxRepos(),
			"max_per_user":         selfServiceMaxPerUser(),
			"expiry":               selfServiceExpiry().String(),
			"key_rotation_overlap": selfServiceKeyRotationOverlap.String(),
		},
		"proof_methods": []string{hubV1ProofHubSessionToken},
	})
}

// handleHubV1Create is POST /api/hub/v1/registrations: the v6 register logic
// with a resource-shaped 201 (Location and ETag headers).
func (s *HubServer) handleHubV1Create(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	stored, rawKey, ok := s.selfServiceRegisterCore(w, r, true)
	if !ok {
		return
	}
	hubURL := hubPublicURL()
	w.Header().Set("Location", hubV1BasePath+"/registrations/"+stored.ID)
	writeHubV1JSON(w, http.StatusCreated, selfServiceETag(stored), map[string]any{
		"registration": s.hubV1View(stored),
		"key":          rawKey,
		"hub_url":      hubURL,
		"snippets":     selfServiceSnippets(hubURL, stored.ID, rawKey),
	})
}

// handleHubV1List is GET /api/hub/v1/registrations: the caller's own records.
func (s *HubServer) handleHubV1List(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	views := make([]hubV1RegistrationView, 0)
	for _, h := range s.selfService.list() {
		if selfServiceOwns(h, login) {
			views = append(views, s.hubV1View(h))
		}
	}
	writeHubV1JSON(w, http.StatusOK, "", map[string]any{"registrations": views})
}

// handleHubV1Get is GET /api/hub/v1/registrations/{id}. If-None-Match with the
// current ETag answers 304.
func (s *HubServer) handleHubV1Get(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	h, ok := s.selfServiceOwnedHive(w, r, s.getAuthUser(r), false)
	if !ok {
		return
	}
	etag := selfServiceETag(h)
	if inm := strings.TrimSpace(r.Header.Get("If-None-Match")); inm != "" && selfServiceETagMatches(h, strings.ReplaceAll(inm, "W/", "")) {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeHubV1JSON(w, http.StatusOK, etag, map[string]any{"registration": s.hubV1View(h)})
}

// hubV1PatchRequest is the PATCH body. Repos is either a JSON array (a full
// replacement) or an object with add and remove lists. DashboardURL is a
// pointer so "absent" (no change) differs from "" (clear it).
type hubV1PatchRequest struct {
	Repos        json.RawMessage `json:"repos"`
	DashboardURL *string         `json:"dashboard_url"`
}

type hubV1RepoDelta struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

// hubV1PatchError is a refusal decided while working out a PATCH.
type hubV1PatchError struct {
	code int
	body map[string]any
}

func hubV1Refuse(code int, msg string) *hubV1PatchError {
	return &hubV1PatchError{code: code, body: map[string]any{"error": msg}}
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

// hubV1PlanRepos works out the repo list a PATCH asks for and which of its
// repos are new to the record. changed is false when the list is the same set.
func hubV1PlanRepos(current []string, raw json.RawMessage) (next, added []string, changed bool, perr *hubV1PatchError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return current, nil, false, nil
	}
	have := make(map[string]struct{}, len(current))
	for _, repo := range current {
		have[strings.ToLower(repo)] = struct{}{}
	}
	var proposed []string
	switch trimmed[0] {
	case '[':
		var repos []string
		if err := json.Unmarshal(trimmed, &repos); err != nil {
			return nil, nil, false, hubV1Refuse(http.StatusBadRequest, "repos must be a list of owner/name strings or an {add, remove} object")
		}
		proposed = repos
	case '{':
		var delta hubV1RepoDelta
		if err := decodeStrict(trimmed, &delta); err != nil {
			return nil, nil, false, hubV1Refuse(http.StatusBadRequest, "repos object takes only add and remove lists")
		}
		if len(delta.Add) == 0 && len(delta.Remove) == 0 {
			return nil, nil, false, hubV1Refuse(http.StatusBadRequest, "repos object needs a non-empty add or remove list")
		}
		removing := make(map[string]struct{}, len(delta.Remove))
		for _, repo := range delta.Remove {
			removing[strings.ToLower(strings.TrimSpace(repo))] = struct{}{}
		}
		for _, repo := range delta.Add {
			if _, both := removing[strings.ToLower(strings.TrimSpace(repo))]; both {
				return nil, nil, false, hubV1Refuse(http.StatusBadRequest, fmt.Sprintf("repo %q is in both add and remove", strings.TrimSpace(repo)))
			}
		}
		for _, repo := range current {
			if _, drop := removing[strings.ToLower(repo)]; !drop {
				proposed = append(proposed, repo)
			}
		}
		proposed = append(proposed, delta.Add...)
	default:
		return nil, nil, false, hubV1Refuse(http.StatusBadRequest, "repos must be a list of owner/name strings or an {add, remove} object")
	}

	if len(proposed) == 0 {
		return nil, nil, false, hubV1Refuse(http.StatusBadRequest, "a registration must keep at least one repo")
	}
	next, err := selfServiceValidateRepos(proposed)
	if err != nil {
		if maxRepos := selfServiceMaxRepos(); len(proposed) > maxRepos {
			return nil, nil, false, &hubV1PatchError{code: http.StatusRequestEntityTooLarge, body: hubV1RepoCapBody(err, maxRepos)}
		}
		return nil, nil, false, hubV1Refuse(http.StatusBadRequest, err.Error())
	}
	for _, repo := range next {
		if _, ok := have[strings.ToLower(repo)]; !ok {
			added = append(added, repo)
		}
	}
	changed = len(added) > 0 || len(next) != len(current)
	return next, added, changed, nil
}

// handleHubV1Patch is PATCH /api/hub/v1/registrations/{id}: change repos
// and/or dashboard_url without touching the key. If-Match is required.
func (s *HubServer) handleHubV1Patch(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	user := loadSaaSUser(login)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	h, ok := s.selfServiceOwnedHive(w, r, login, false)
	if !ok {
		return
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	if ifMatch == "" {
		writeHubV1JSON(w, http.StatusPreconditionRequired, selfServiceETag(h), map[string]any{
			"error": "PATCH needs an If-Match header with the registration's ETag",
		})
		return
	}
	// Fast refusal before any GitHub call; the store re-checks under its lock.
	if !selfServiceETagMatches(h, ifMatch) {
		writeHubV1JSON(w, http.StatusPreconditionFailed, selfServiceETag(h), map[string]any{
			"error": "the registration changed since you read it; GET it again and retry",
		})
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, selfServiceRequestBodyLimit()))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	// org changes who the record speaks for, so it is not an edit: refuse it
	// by name rather than as an unknown field.
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) == nil {
		if _, ok := fields["org"]; ok {
			writeJSONError(w, http.StatusBadRequest, "org is not editable; delete and re-register")
			return
		}
	}
	var body hubV1PatchRequest
	if err := decodeStrict(data, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body; only repos and dashboard_url can be changed")
		return
	}

	nextRepos, added, reposChanged, perr := hubV1PlanRepos(h.Repos, body.Repos)
	if perr != nil {
		s.auditSelfServiceRefused(login, h.ID, selfServiceRefusedInvalidRepos)
		writeHubV1JSON(w, perr.code, "", perr.body)
		return
	}
	nextURL := h.DashboardURL
	if body.DashboardURL != nil {
		nextURL, err = selfServiceValidateDashboardURL(r.Context(), *body.DashboardURL)
		if err != nil {
			s.auditSelfServiceRefused(login, h.ID, selfServiceRefusedInvalidURL)
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	urlChanged := nextURL != h.DashboardURL
	if len(body.Repos) == 0 && body.DashboardURL == nil {
		writeJSONError(w, http.StatusBadRequest, "nothing to change; send repos and/or dashboard_url")
		return
	}
	if !reposChanged && !urlChanged {
		writeHubV1JSON(w, http.StatusOK, selfServiceETag(h), map[string]any{"registration": s.hubV1View(h)})
		return
	}

	// Only repos new to the record need a proof; the rest were proven when
	// they were added. A read-only GitHub call per added repo, nothing more.
	if len(added) > 0 && !s.selfServiceProveOwnership(w, r, login, h.ID, user, added) {
		return
	}

	stored, err := s.selfService.update(h.ID, ifMatch, func(rec *SelfServiceHive) error {
		rec.Repos = nextRepos
		rec.DashboardURL = nextURL
		// Added repos are stamped now; kept repos keep their proof time; a
		// removed repo's entry goes, so re-adding it needs a new proof.
		rec.RepoProofs = selfServiceProofTimes(rec.RepoProofs, nextRepos, rec.OwnershipCheckedAt, time.Now(), added)
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errSelfServiceNotFound):
			writeJSONError(w, http.StatusNotFound, "self-hosted hive not found")
		case errors.Is(err, errSelfServiceStale):
			current, _ := s.selfService.get(h.ID)
			writeHubV1JSON(w, http.StatusPreconditionFailed, selfServiceETag(current), map[string]any{
				"error": "the registration changed since you read it; GET it again and retry",
			})
		default:
			s.logger.Error("self-service update: store write failed", "user", login, "hive", h.ID, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "could not persist the update")
		}
		return
	}
	s.logger.Info("audit: selfservice_hive_updated", "user", login, "hive", h.ID,
		"repos", len(stored.Repos), "repos_added", len(added), "repos_removed", len(h.Repos)+len(added)-len(stored.Repos),
		"dashboard_url_changed", urlChanged)
	writeHubV1JSON(w, http.StatusOK, selfServiceETag(stored), map[string]any{"registration": s.hubV1View(stored)})
}

// handleHubV1RotateKey is POST /api/hub/v1/registrations/{id}/keys: the v6
// rotation (the previous key keeps working for selfServiceKeyRotationOverlap)
// with an optional If-Match. A caller-chosen grace window is rollout step 3,
// so a body asking for one is refused rather than silently ignored.
func (s *HubServer) handleHubV1RotateKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	h, ok := s.selfServiceOwnedHive(w, r, login, false)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, selfServiceMaxRequestBody))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	if len(bytes.TrimSpace(data)) > 0 {
		var body struct{}
		if err := decodeStrict(data, &body); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("this hub takes no rotation options; the previous key stays valid for %s", selfServiceKeyRotationOverlap))
			return
		}
	}
	rawKey, err := s.selfService.rotateIfMatch(h.ID, r.Header.Get("If-Match"))
	if err != nil {
		switch {
		case errors.Is(err, errSelfServiceNotFound):
			writeJSONError(w, http.StatusNotFound, "self-hosted hive not found")
		case errors.Is(err, errSelfServiceStale):
			current, _ := s.selfService.get(h.ID)
			writeHubV1JSON(w, http.StatusPreconditionFailed, selfServiceETag(current), map[string]any{
				"error": "the registration changed since you read it; GET it again and retry",
			})
		default:
			s.logger.Error("self-service rotation: store write failed", "user", login, "hive", h.ID, "error", err)
			writeJSONError(w, http.StatusInternalServerError, "could not persist the new key")
		}
		return
	}
	s.logger.Info("audit: selfservice_hive_rotated", "user", login, "hive", h.ID, "overlap", selfServiceKeyRotationOverlap.String())
	stored, _ := s.selfService.get(h.ID)
	hubURL := hubPublicURL()
	writeHubV1JSON(w, http.StatusCreated, selfServiceETag(stored), map[string]any{
		"id":       h.ID,
		"key":      rawKey,
		"hub_url":  hubURL,
		"overlap":  selfServiceKeyRotationOverlap.String(),
		"snippets": selfServiceSnippets(hubURL, h.ID, rawKey),
	})
}

// handleHubV1Delete is DELETE /api/hub/v1/registrations/{id}: the owner or a
// hub admin revokes the key and removes the registry entry, with an optional
// If-Match.
func (s *HubServer) handleHubV1Delete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	h, ok := s.selfServiceOwnedHive(w, r, login, true)
	if !ok {
		return
	}
	removed, err := s.selfService.revokeIfMatch(h.ID, r.Header.Get("If-Match"))
	if err != nil {
		if errors.Is(err, errSelfServiceStale) {
			current, _ := s.selfService.get(h.ID)
			writeHubV1JSON(w, http.StatusPreconditionFailed, selfServiceETag(current), map[string]any{
				"error": "the registration changed since you read it; GET it again and retry",
			})
			return
		}
		s.logger.Error("self-service revocation: store write failed", "user", login, "hive", h.ID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not persist the revocation")
		return
	}
	s.removeRegistryEntry(h.ID, login)
	s.logger.Info("audit: selfservice_hive_revoked", "user", login, "hive", h.ID, "owner", h.Owner, "by_admin", !selfServiceOwns(h, login))
	writeHubV1JSON(w, http.StatusOK, "", map[string]any{"removed": removed, "id": h.ID})
}
