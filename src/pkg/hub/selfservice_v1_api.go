package hub

// Hub registration protocol v1, rollout step 1 (hivecommons/hive#18642).
//
// /api/hub/v1 is a versioned resource view of the self-service (byo-)
// registry that selfservice_api.go already serves under /api/hub/selfservice.
// It adds discovery (GET /api/hub/v1) and the registry as resources: create,
// list, get, update (PATCH, selfservice_v1_patch.go), delete and key rotation,
// with an ETag on every record and an If-Match on every write (required on
// PATCH, optional on delete and rotate). The legacy routes keep their bodies
// and status codes.
//
// Invariants (in addition to selfservice_api.go's):
//   - Nothing here writes to GitHub. The only GitHub call is the read-only
//     ownership check (GET /repos/{owner}/{repo}) at create, shared with the
//     legacy route.
//   - Every write carries an ETag precondition check inside the store lock, so
//     two operators acting at once get 412, never a silent overwrite.

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

// hubV1Operations are the operations available on every enabled hub.
var hubV1Operations = []hubV1Operation{
	{Name: "discovery", Method: http.MethodGet, Path: hubV1BasePath},
	{Name: "create", Method: http.MethodPost, Path: hubV1BasePath + "/registrations"},
	{Name: "list", Method: http.MethodGet, Path: hubV1BasePath + "/registrations"},
	{Name: "get", Method: http.MethodGet, Path: hubV1BasePath + "/registrations/{id}"},
	{Name: "update", Method: http.MethodPatch, Path: hubV1BasePath + "/registrations/{id}", IfMatch: "required"},
	{Name: "rotate_key", Method: http.MethodPost, Path: hubV1BasePath + "/registrations/{id}/keys", IfMatch: "optional"},
	{Name: "delete", Method: http.MethodDelete, Path: hubV1BasePath + "/registrations/{id}", IfMatch: "optional"},
}

// hubV1PatchFields are the registration fields an update may change. org is
// not one of them: changing it would mean re-proving every repo, so a client
// deletes and registers again (#18642).
var hubV1PatchFields = []string{"repos", "dashboard_url"}

// registerHubV1Routes wires the v1 routes onto the hub mux. Discovery is
// public (a spoke reads it before it has a session); the registration-key
// PATCH lane is opt-in, and the remaining routes require hub auth.
// All routes answer 404 while HIVE_HUB_SELFSERVICE_REGISTRATION is off
// for callers with the appropriate credential.
func (s *HubServer) registerHubV1Routes() {
	s.mux.HandleFunc("GET "+hubV1BasePath, s.handleHubV1Discovery)
	s.mux.HandleFunc("POST "+hubV1BasePath+"/registrations", s.requireAuth(s.handleHubV1Create))
	s.mux.HandleFunc("GET "+hubV1BasePath+"/registrations", s.requireAuth(s.handleHubV1List))
	s.mux.HandleFunc("GET "+hubV1BasePath+"/registrations/{id}", s.requireAuth(s.handleHubV1Get))
	s.mux.HandleFunc("PATCH "+hubV1BasePath+"/registrations/{id}", s.routeHubV1Patch)
	s.mux.HandleFunc("DELETE "+hubV1BasePath+"/registrations/{id}", s.requireAuth(s.handleHubV1Delete))
	s.mux.HandleFunc("POST "+hubV1BasePath+"/registrations/{id}/keys", s.requireAuth(s.handleHubV1RotateKey))
}

// hubV1RegistrationView is a v6 view plus the record's ETag, so a list
// response carries a usable precondition for every item, and the per-repo
// proof times.
type hubV1RegistrationView struct {
	selfServiceHiveView
	ETag         string           `json:"etag"`
	RepoProofs   []hubV1RepoProof `json:"repo_proofs"`
	PendingProof []string         `json:"pending_proof,omitempty"`
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
		PendingProof:        h.PendingProof,
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
	operations := hubV1Operations
	if hubV1SyncReposEnabled() {
		operations = append(operations, hubV1Operation{
			Name: hubV1SyncReposOp, Method: http.MethodPatch,
			Path: hubV1BasePath + "/registrations/{id}", IfMatch: "required",
		})
	}
	writeHubV1JSON(w, http.StatusOK, "", map[string]any{
		"protocol":     hubV1Protocol,
		"version":      hubV1ProtocolVersion,
		"base_path":    hubV1BasePath,
		"operations":   operations,
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
