package hub

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	hubV1SyncReposEnv = "HIVE_HUB_SELFSERVICE_SYNC_REPOS"
	hubV1SyncReposOp  = "sync-repos"
)

var errHubV1SyncNoWrite = errors.New("sync-repos did not change the registration")

func hubV1SyncReposEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(hubV1SyncReposEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (s *HubServer) routeHubV1Patch(w http.ResponseWriter, r *http.Request) {
	// Never send a registration key through the hub auth middleware, which
	// validates bearer credentials against GitHub. Key shape selects the
	// lane, not authority: only the addressed record's hashes authenticate it.
	if key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok &&
		len(key) == base64.RawURLEncoding.EncodedLen(selfServiceKeyBytes) {
		var decoded [selfServiceKeyBytes]byte
		if _, err := base64.RawURLEncoding.Strict().Decode(decoded[:], []byte(key)); err == nil {
			s.handleHubV1SyncRepos(w, r, key)
			return
		}
	}
	s.requireAuth(s.handleHubV1Patch)(w, r)
}

func (s *HubServer) handleHubV1SyncRepos(w http.ResponseWriter, r *http.Request, key string) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	if !hubV1SyncReposEnabled() {
		writeJSONError(w, http.StatusNotFound, "sync-repos is not enabled")
		return
	}
	if !isNonBrowserAPIRequest(r) || !isCSRFSafe(r) {
		writeJSONError(w, http.StatusForbidden, "sync-repos requires a registration bearer without a hub session cookie and must pass the CSRF check")
		return
	}
	limit := selfServiceRequestBodyLimit()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "could not read the request body")
		return
	}
	if int64(len(data)) > limit {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		return
	}

	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	hash := hashSelfServiceKey(key)
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	var current SelfServiceHive
	var refusal *hubV1PatchRefusal
	stored, err := s.selfService.update(id, "", func(h *SelfServiceHive) error {
		// Authenticate before exposing the current ETag. Check against the
		// live record under the write lock, including after key rotation.
		if !s.selfService.matchesKey(*h, hash) {
			return errSelfServiceNotFound
		}
		current = *h
		switch {
		case ifMatch == "":
			refusal = hubV1Refuse(http.StatusPreconditionRequired, "If-Match is required: send an ETag or '*' for the current registration")
		case !selfServiceETagMatches(*h, ifMatch):
			refusal = hubV1Refuse(http.StatusPreconditionFailed, "the registration changed; retry with its current ETag")
		}
		if refusal != nil {
			return errHubV1SyncNoWrite
		}
		refusal = hubV1ApplySyncRepos(h, data)
		if refusal != nil || (hubV1SameRepos(h.Repos, current.Repos) && hubV1SameRepos(h.PendingProof, current.PendingProof)) {
			return errHubV1SyncNoWrite
		}
		return nil
	})
	switch {
	case errors.Is(err, errSelfServiceNotFound):
		writeJSONError(w, http.StatusNotFound, "self-hosted hive not found")
	case errors.Is(err, errHubV1SyncNoWrite):
		if refusal != nil {
			etag := ""
			if refusal.status == http.StatusPreconditionRequired || refusal.status == http.StatusPreconditionFailed {
				etag = selfServiceETag(current)
			}
			writeHubV1JSON(w, refusal.status, etag, refusal.body)
			return
		}
		writeHubV1JSON(w, http.StatusOK, selfServiceETag(current), map[string]any{"registration": s.hubV1View(current)})
	case err != nil:
		s.logger.Error("self-service sync-repos: store write failed", "hive", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not persist the change")
	default:
		s.logger.Info("audit: selfservice_hive_repos_synced", "hive", id, "repos", len(stored.Repos),
			"pending_proof", len(stored.PendingProof), "revision", stored.Revision)
		writeHubV1JSON(w, http.StatusOK, selfServiceETag(stored), map[string]any{"registration": s.hubV1View(stored)})
	}
}

func hubV1ApplySyncRepos(h *SelfServiceHive, data []byte) *hubV1PatchRefusal {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return hubV1Refuse(http.StatusBadRequest, "sync-repos takes a JSON object with op and repos")
	}
	for field := range fields {
		if field != "op" && field != "repos" {
			return hubV1Refuse(http.StatusBadRequest, "field %q cannot be changed by sync-repos", field)
		}
	}
	var op string
	if json.Unmarshal(fields["op"], &op) != nil || op != hubV1SyncReposOp {
		return hubV1Refuse(http.StatusBadRequest, "a registration key may only PATCH with op %q", hubV1SyncReposOp)
	}
	var requested []string
	if json.Unmarshal(fields["repos"], &requested) != nil || requested == nil {
		return hubV1Refuse(http.StatusBadRequest, "repos must be a list of owner/name repos")
	}
	if maxRepos := selfServiceMaxRepos(); len(requested) > maxRepos {
		return &hubV1PatchRefusal{
			status: http.StatusRequestEntityTooLarge,
			body:   hubV1RepoCapBody(fmt.Errorf("at most %d repos per registration", maxRepos), maxRepos),
		}
	}
	repos, err := selfServiceValidateRepos(requested)
	if err != nil {
		return hubV1Refuse(http.StatusBadRequest, "%s", err.Error())
	}

	// Both lists are replaced. A previously active repo keeps its proof;
	// everything else must go through the owner's ordinary proof path.
	activeByName := make(map[string]string, len(h.Repos))
	for _, repo := range h.Repos {
		activeByName[strings.ToLower(repo)] = repo
	}
	var active, pending []string
	for _, repo := range repos {
		if existing, ok := activeByName[strings.ToLower(repo)]; ok {
			active = append(active, existing)
		} else {
			pending = append(pending, repo)
		}
	}
	if len(active) == 0 {
		return hubV1Refuse(http.StatusBadRequest, "sync-repos must keep at least one already-proven repo active")
	}
	h.Repos = active
	h.PendingProof = pending
	h.RepoProofs = selfServiceProofTimes(h.RepoProofs, active, h.OwnershipCheckedAt, time.Time{}, nil)
	return nil
}
