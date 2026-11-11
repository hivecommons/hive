package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
)

// RFC #17620 phase 7: hub marketplace routes.
//
//	GET  /api/marketplace/workflows                         index (ETag / 304)
//	GET  /api/marketplace/workflows/{name}/{version}/bundle one bundle
//	POST /api/marketplace/workflows                         publish (spoke bearer or admin)
//	POST /api/marketplace/refresh[?repo=]                   on-demand ingest (admin)
//	GET  /api/marketplace/status                            last refresh + counts by tier
//
// Tier policy: a publisher may assert `local`, `local-published` or
// `ingested-unreviewed`; `curated` and `built-in` require a hub admin
// session. Spokes authenticate with their heartbeat bearer bound to the
// X-Hive-ID header, exactly as /api/nps does with hive_id in the body.

const (
	marketplaceMaxBundleBytes = 4 << 20
	marketplaceHiveIDHeader   = "X-Hive-ID"
)

func (s *HubServer) registerMarketplaceRoutes() {
	s.mux.HandleFunc("GET /api/marketplace/workflows", s.handleMarketplaceIndex)
	s.mux.HandleFunc("GET /api/marketplace/workflows/{name}/{version}/bundle", s.handleMarketplaceBundle)
	s.mux.HandleFunc("POST /api/marketplace/workflows", s.handleMarketplacePublish)
	s.mux.HandleFunc("POST /api/marketplace/refresh", s.requireAdmin(s.handleMarketplaceRefresh))
	s.mux.HandleFunc("GET /api/marketplace/status", s.handleMarketplaceStatus)
}

func marketplaceJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSONError(w, code, msg)
}

func (s *HubServer) marketplaceGate(w http.ResponseWriter) bool {
	if marketplaceDisabled() {
		marketplaceJSONError(w, http.StatusNotFound, "marketplace disabled")
		return false
	}
	return true
}

// handleMarketplaceIndex serves the catalogue index with a strong ETag.
func (s *HubServer) handleMarketplaceIndex(w http.ResponseWriter, r *http.Request) {
	if !s.marketplaceGate(w) {
		return
	}
	_, raw, etag, err := s.marketplace().Index()
	if err != nil {
		marketplaceJSONError(w, http.StatusInternalServerError, "index unavailable: "+err.Error())
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && marketplaceETagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func marketplaceETagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "W/"))
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

// handleMarketplaceBundle serves one immutable bundle.
func (s *HubServer) handleMarketplaceBundle(w http.ResponseWriter, r *http.Request) {
	if !s.marketplaceGate(w) {
		return
	}
	data, err := s.marketplace().Get(r.PathValue("name"), r.PathValue("version"))
	switch {
	case errors.Is(err, errMarketplaceNotFound):
		marketplaceJSONError(w, http.StatusNotFound, "not found")
		return
	case errors.Is(err, errMarketplaceBadKey):
		marketplaceJSONError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		marketplaceJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// marketplacePublisher identifies who may publish: a hub admin session or
// a registered spoke presenting its heartbeat bearer.
type marketplacePublisher struct {
	name  string
	admin bool
}

func (s *HubServer) marketplaceAuthenticate(r *http.Request) (marketplacePublisher, int, string) {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if s.hubSecret == "" {
			return marketplacePublisher{}, http.StatusServiceUnavailable, "publishing requires a configured hub secret"
		}
		hiveID := sanitizeHeartbeatField(r.Header.Get(marketplaceHiveIDHeader))
		if hiveID == "" || hiveID != r.Header.Get(marketplaceHiveIDHeader) || !isValidName(hiveID) {
			return marketplacePublisher{}, http.StatusBadRequest, "invalid " + marketplaceHiveIDHeader
		}
		if !s.verifyHeartbeatBearer(strings.TrimPrefix(auth, "Bearer "), hiveID) {
			return marketplacePublisher{}, http.StatusUnauthorized, "unauthorized"
		}
		if !s.npsHiveRegistered(hiveID) {
			return marketplacePublisher{}, http.StatusForbidden, "unknown hive - heartbeat first"
		}
		return marketplacePublisher{name: "hive:" + hiveID}, 0, ""
	}
	if !isCSRFSafe(r) {
		return marketplacePublisher{}, http.StatusForbidden, "CSRF check failed"
	}
	user := s.getRealAuthUser(r)
	if user == "" {
		return marketplacePublisher{}, http.StatusUnauthorized, "unauthorized"
	}
	if !isHubAdmin(user) {
		return marketplacePublisher{}, http.StatusForbidden, "admin access required to publish from a session; spokes publish with their heartbeat credential"
	}
	return marketplacePublisher{name: user, admin: true}, 0, ""
}

// handleMarketplacePublish verifies and stores a bundle.
func (s *HubServer) handleMarketplacePublish(w http.ResponseWriter, r *http.Request) {
	if !s.marketplaceGate(w) {
		return
	}
	who, code, msg := s.marketplaceAuthenticate(r)
	if code != 0 {
		marketplaceJSONError(w, code, msg)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, marketplaceMaxBundleBytes+1))
	if err != nil {
		marketplaceJSONError(w, http.StatusBadRequest, "read error")
		return
	}
	if len(raw) > marketplaceMaxBundleBytes {
		marketplaceJSONError(w, http.StatusRequestEntityTooLarge, "bundle too large")
		return
	}
	b, err := bundle.Decode(raw)
	if err != nil {
		marketplaceJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch b.Tier {
	case "":
		b.Tier = bundle.TierLocalPublished
	case bundle.TierLocal:
		b.Tier = bundle.TierLocalPublished
	case bundle.TierLocalPublished, bundle.TierIngestedUnreviewed:
	case bundle.TierCurated, bundle.TierBuiltIn:
		if !who.admin {
			marketplaceJSONError(w, http.StatusForbidden, fmt.Sprintf("tier %q requires a hub admin", b.Tier))
			return
		}
	default:
		marketplaceJSONError(w, http.StatusBadRequest, "unknown tier "+b.Tier)
		return
	}
	if err := bundle.Verify(b); err != nil {
		marketplaceJSONError(w, http.StatusUnprocessableEntity, "bundle verification failed: "+err.Error())
		return
	}
	entry, created, err := s.marketplace().Publish(b, who.name, time.Now().UTC(), "")
	switch {
	case errors.Is(err, errMarketplaceConflict):
		marketplaceJSONError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, errMarketplaceBadKey):
		marketplaceJSONError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		marketplaceJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logger.Info("marketplace publish", "key", entry.Key(), "tier", entry.Tier, "by", who.name, "created", created, "hash", entry.Hash)
	code = http.StatusOK
	if created {
		code = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"published": entry.Key(), "created": created, "entry": entry})
}

// handleMarketplaceRefresh runs an on-demand ingest in the background and
// answers 202 immediately; ?repo= limits it to one repository (and runs
// synchronously, since a single repo is quick enough to await).
func (s *HubServer) handleMarketplaceRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.marketplaceGate(w) {
		return
	}
	repo := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("repo")))
	user := s.getRealAuthUser(r)
	if repo != "" {
		if !strings.Contains(repo, "/") {
			marketplaceJSONError(w, http.StatusBadRequest, "repo must be owner/name")
			return
		}
		res, err := s.runMarketplaceRefresh(r.Context(), []string{repo}, "on-demand:"+user)
		if errors.Is(err, errMarketplaceRefreshBusy) {
			marketplaceJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			marketplaceJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.logger.Info("marketplace refresh (single repo)", "by", user, "repo", repo, "published", res.Published, "errors", res.Errors)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"accepted": true, "repo": repo, "result": res})
		return
	}
	rf := s.refresher()
	rf.mu.Lock()
	busy := rf.running
	rf.mu.Unlock()
	if busy {
		marketplaceJSONError(w, http.StatusConflict, errMarketplaceRefreshBusy.Error())
		return
	}
	s.logger.Info("marketplace refresh requested", "by", user)
	go func() {
		if _, err := s.runMarketplaceRefresh(s.marketplaceBackgroundContext(), nil, "on-demand:"+user); err != nil {
			s.logger.Warn("marketplace on-demand refresh skipped", "error", err)
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"accepted": true, "repos": len(marketplaceRepos())})
}

// handleMarketplaceStatus reports the last refresh and tier counts.
func (s *HubServer) handleMarketplaceStatus(w http.ResponseWriter, r *http.Request) {
	if !s.marketplaceGate(w) {
		return
	}
	idx, _, etag, err := s.marketplace().Index()
	if err != nil {
		marketplaceJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byTier := map[string]int{}
	for _, e := range idx.Entries {
		byTier[e.Tier]++
	}
	rf := s.refresher()
	rf.mu.Lock()
	last := rf.last
	running := rf.running
	rf.mu.Unlock()
	records := s.marketplace().refreshRecords()
	var lastChecked time.Time
	failing := 0
	for _, rec := range records {
		if rec.CheckedAt.After(lastChecked) {
			lastChecked = rec.CheckedAt
		}
		if rec.Error != "" {
			failing++
		}
	}
	out := map[string]interface{}{
		"entries":       len(idx.Entries),
		"byTier":        byTier,
		"etag":          etag,
		"generatedAt":   idx.GeneratedAt,
		"corpusRepos":   len(marketplaceRepos()),
		"trackedRepos":  len(records),
		"failingRepos":  failing,
		"refreshing":    running,
		"schedule":      marketplaceSchedule(),
		"lastRefresh":   last,
		"lastCheckedAt": nil,
	}
	if !lastChecked.IsZero() {
		out["lastCheckedAt"] = lastChecked
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
