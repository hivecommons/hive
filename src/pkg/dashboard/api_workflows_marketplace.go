package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// RFC #17620 phase 7: bundle export/import on the spoke plus a proxy to the
// hub marketplace catalogue. Routes live in their own file so concurrent
// work on api_workflows.go only needs the one-line registration hook.

const (
	// marketplaceHubTimeout bounds one spoke→hub call.
	marketplaceHubTimeout = 20 * time.Second
	// marketplaceMaxBody caps bundle and index payloads read from clients
	// or the hub.
	marketplaceMaxBody = 4 << 20
	// marketplaceHiveIDHeader carries the spoke's hive_id alongside the
	// heartbeat bearer so the hub can bind the credential to a hive.
	marketplaceHiveIDHeader = "X-Hive-ID"
	// marketplaceStatusStale is the status returned when the hub is
	// unreachable; the body may still carry a cached index.
	marketplaceStatusStale = http.StatusBadGateway
)

// marketplaceHTTP is the client used for hub calls; tests swap it.
var marketplaceHTTP = &http.Client{Timeout: marketplaceHubTimeout}

// marketplaceIndexCache is the spoke's ETag-validated copy of the hub index.
type marketplaceIndexCache struct {
	mu        sync.Mutex
	etag      string
	body      []byte
	fetchedAt time.Time
}

var marketplaceCache marketplaceIndexCache

func (s *Server) registerWorkflowMarketplaceRoutes() {
	s.mux.HandleFunc("GET /api/workflows/{name}/{version}/bundle", s.handleWorkflowBundleGet)
	s.mux.HandleFunc("POST /api/workflows/import", s.handleWorkflowImport)
	s.mux.HandleFunc("GET /api/marketplace/workflows", s.handleMarketplaceIndex)
	s.mux.HandleFunc("POST /api/marketplace/workflows/publish", s.handleMarketplacePublish)
	s.mux.HandleFunc("POST /api/marketplace/workflows/{name}/{version}/import", s.handleMarketplaceImport)
	s.mux.HandleFunc("POST /api/marketplace/refresh", s.handleMarketplaceRefresh)
}

// marketplaceEnabled reports workflow.marketplace.enabled (default true).
func (s *Server) marketplaceEnabled() bool {
	if s.deps == nil || s.deps.Config == nil {
		return true
	}
	return s.deps.Config.Workflow.Marketplace.MarketplaceEnabled()
}

// marketplaceBase is the hub base URL for /api/marketplace/*, or "" when the
// marketplace is disabled or no hub is configured.
func (s *Server) marketplaceBase() string {
	if !s.marketplaceEnabled() || s.deps == nil || s.deps.Config == nil {
		return ""
	}
	base := s.deps.Config.MarketplaceURL()
	if u, err := url.Parse(base); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return base
}

func (s *Server) requireMarketplace(w http.ResponseWriter) (string, bool) {
	if !s.marketplaceEnabled() {
		jsonError(w, "workflow marketplace disabled (workflow.marketplace.enabled)", http.StatusNotFound)
		return "", false
	}
	base := s.marketplaceBase()
	if base == "" {
		jsonError(w, "workflow marketplace has no hub URL (workflow.marketplace.url or hub.url)", http.StatusServiceUnavailable)
		return "", false
	}
	return base, true
}

// handleWorkflowBundleGet exports one stored model as a verified bundle.
// Any role: bundles contain nothing the library view does not already show.
func (s *Server) handleWorkflowBundleGet(w http.ResponseWriter, r *http.Request) {
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	b, err := s.exportBundle(st, r.PathValue("name"), r.PathValue("version"), requestUser(r))
	if err != nil {
		if errors.Is(err, workflowmodel.ErrCompile) || errors.Is(err, workflowmodel.ErrInvalid) {
			jsonError(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		workflowStoreError(w, err)
		return
	}
	data, err := bundle.Encode(b)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *Server) exportBundle(st *wfstore.Store, name, version, by string) (*bundle.Bundle, error) {
	m, _, err := st.Get(name, version)
	if err != nil {
		return nil, err
	}
	return bundle.Export(m, by, time.Now().UTC())
}

// handleWorkflowImport verifies a bundle posted by the client and stores its
// model, recompiling locally. 422 when the bundle does not verify. Owner.
func (s *Server) handleWorkflowImport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, marketplaceMaxBody+1))
	if err != nil || len(raw) > marketplaceMaxBody {
		jsonError(w, "bundle too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	b, err := bundle.Decode(raw)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	e, err := s.importBundle(st, b, requestUser(r))
	if err != nil {
		var ve *importVerifyError
		if errors.As(err, &ve) {
			jsonError(w, ve.Error(), http.StatusUnprocessableEntity)
			return
		}
		workflowStoreError(w, err)
		return
	}
	s.auditFromRequest(r, "workflow_import", fmt.Sprintf("%s hash=%s tier=%s", e.Key(), shortHash(b.Hash), b.Tier), "")
	jsonStatusResponse(w, http.StatusCreated, map[string]interface{}{"entry": e, "verified": true})
}

type importVerifyError struct{ err error }

func (e *importVerifyError) Error() string { return "bundle verification failed: " + e.err.Error() }
func (e *importVerifyError) Unwrap() error { return e.err }

// importBundle verifies b, pins the model to the bundle's version when that
// slot is free (otherwise the next integer version) and saves it. The
// store recompiles on Save, so the published profile is never persisted.
func (s *Server) importBundle(st *wfstore.Store, b *bundle.Bundle, by string) (wfstore.Entry, error) {
	if err := bundle.Verify(b); err != nil {
		return wfstore.Entry{}, &importVerifyError{err}
	}
	m := b.Model
	name := m.Metadata.Name
	version := m.Spec.Meta.Version
	if wfstore.IsBuiltin(name) {
		return wfstore.Entry{}, fmt.Errorf("%w: %s is a built-in model", wfstore.ErrReadOnly, name)
	}
	if version != "" {
		if _, _, err := st.Get(name, version); err == nil {
			version = ""
		} else if !errors.Is(err, wfstore.ErrNotFound) {
			return wfstore.Entry{}, err
		}
	}
	if version == "" {
		next, err := nextWorkflowVersion(st, name)
		if err != nil {
			return wfstore.Entry{}, err
		}
		version = next
	}
	m.Spec.Meta.Version = version
	m.Spec.Meta.Provenance = importedProvenance(m.Spec.Meta.Provenance, b.Tier)
	return st.Save(m, by, time.Now().UTC())
}

// importedProvenance decides what provenance an imported model is stored
// with. The envelope tier is the field the hub polices (curated/built-in
// need a hub admin); the model's own provenance is hash-covered and set by
// whoever authored the model, so a vouched tier claimed there is honoured
// only when the envelope asserts the same tier. built-in and
// local-published always land as local: this hive did not ship the model
// and has not published it.
func importedProvenance(model workflowmodel.Provenance, tier string) workflowmodel.Provenance {
	p := model
	vouched := p == workflowmodel.ProvenanceCurated || p == workflowmodel.ProvenanceBuiltIn
	if p == "" || (vouched && string(p) != tier) {
		p = workflowmodel.Provenance(tier)
	}
	if p == workflowmodel.ProvenanceBuiltIn || string(p) == bundle.TierLocalPublished || p == "" {
		p = workflowmodel.ProvenanceLocal
	}
	return p
}

// hubRequest builds an authenticated spoke→hub request.
func (s *Server) hubRequest(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if key := spoke.SpokeHeartbeatKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if s.deps != nil && s.deps.Config != nil {
		if id := strings.TrimSpace(s.deps.Config.HiveID); id != "" {
			req.Header.Set(marketplaceHiveIDHeader, id)
		}
	}
	return req, nil
}

// handleMarketplaceIndex proxies the hub index with an ETag cache. When the
// hub is unreachable it answers 502 carrying the last cached index (if any)
// and stale:true so the UI can still render.
func (s *Server) handleMarketplaceIndex(w http.ResponseWriter, r *http.Request) {
	base, ok := s.requireMarketplace(w)
	if !ok {
		return
	}
	ttl := s.deps.Config.Workflow.Marketplace.EffectiveRefreshInterval()
	marketplaceCache.mu.Lock()
	defer marketplaceCache.mu.Unlock()
	c := &marketplaceCache
	fresh := c.body != nil && time.Since(c.fetchedAt) < ttl && r.URL.Query().Get("refresh") == ""
	if !fresh {
		ctx, cancel := context.WithTimeout(r.Context(), marketplaceHubTimeout)
		defer cancel()
		req, err := s.hubRequest(ctx, http.MethodGet, base+"/api/marketplace/workflows", nil)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if c.etag != "" {
			req.Header.Set("If-None-Match", c.etag)
		}
		resp, err := marketplaceHTTP.Do(req)
		if err != nil {
			writeMarketplaceIndex(w, c, true, "hub unreachable: "+err.Error())
			return
		}
		defer closeHTTPBody(resp.Body)
		switch resp.StatusCode {
		case http.StatusNotModified:
			c.fetchedAt = time.Now()
		case http.StatusOK:
			body, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceMaxBody+1))
			if err != nil || len(body) > marketplaceMaxBody || !json.Valid(body) {
				writeMarketplaceIndex(w, c, true, "hub index unreadable")
				return
			}
			c.body = body
			c.etag = resp.Header.Get("ETag")
			c.fetchedAt = time.Now()
		default:
			writeMarketplaceIndex(w, c, true, fmt.Sprintf("hub returned HTTP %d", resp.StatusCode))
			return
		}
	}
	writeMarketplaceIndex(w, c, false, "")
}

// writeMarketplaceIndex wraps the cached hub index as
// {index, stale, etag, fetchedAt, error?}.
func writeMarketplaceIndex(w http.ResponseWriter, c *marketplaceIndexCache, stale bool, errMsg string) {
	out := map[string]interface{}{"stale": stale}
	if c.body != nil {
		out["index"] = json.RawMessage(c.body)
		out["etag"] = c.etag
		out["fetchedAt"] = c.fetchedAt.UTC()
	} else {
		out["index"] = bundle.Index{Entries: []bundle.IndexEntry{}}
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	if stale {
		jsonStatusResponse(w, marketplaceStatusStale, out)
		return
	}
	jsonResponse(w, out)
}

// fetchHubBundle downloads name@version from the hub and decodes it.
func (s *Server) fetchHubBundle(ctx context.Context, base, name, version string) (*bundle.Bundle, int, error) {
	u := base + "/api/marketplace/workflows/" + url.PathEscape(name) + "/" + url.PathEscape(version) + "/bundle"
	req, err := s.hubRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	resp, err := marketplaceHTTP.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("hub unreachable: %w", err)
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotFound, fmt.Errorf("%s@%s not in marketplace", name, version)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, http.StatusBadGateway, fmt.Errorf("hub returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, marketplaceMaxBody+1))
	if err != nil || len(raw) > marketplaceMaxBody {
		return nil, http.StatusBadGateway, errors.New("hub bundle unreadable or too large")
	}
	b, err := bundle.Decode(raw)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("hub bundle invalid: %w", err)
	}
	return b, http.StatusOK, nil
}

// handleMarketplaceImport fetches a catalogue bundle and imports it. Owner.
func (s *Server) handleMarketplaceImport(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	base, ok := s.requireMarketplace(w)
	if !ok {
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	name, version := r.PathValue("name"), r.PathValue("version")
	ctx, cancel := context.WithTimeout(r.Context(), marketplaceHubTimeout)
	defer cancel()
	b, code, err := s.fetchHubBundle(ctx, base, name, version)
	if err != nil {
		jsonError(w, err.Error(), code)
		return
	}
	if b.Model.Metadata.Name != name || b.Model.Spec.Meta.Version != version {
		jsonError(w, fmt.Sprintf("hub returned %s for %s@%s", b.Key(), name, version), http.StatusBadGateway)
		return
	}
	e, err := s.importBundle(st, b, requestUser(r))
	if err != nil {
		var ve *importVerifyError
		if errors.As(err, &ve) {
			jsonError(w, ve.Error(), http.StatusUnprocessableEntity)
			return
		}
		workflowStoreError(w, err)
		return
	}
	s.auditFromRequest(r, "workflow_import", fmt.Sprintf("%s from marketplace %s@%s hash=%s tier=%s", e.Key(), name, version, shortHash(b.Hash), b.Tier), "")
	jsonStatusResponse(w, http.StatusCreated, map[string]interface{}{"entry": e, "verified": true, "source": name + "@" + version})
}

// marketplacePublishRequest is the body of POST /api/marketplace/workflows/publish.
type marketplacePublishRequest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleMarketplacePublish exports a library entry and posts it to the hub
// with the spoke's heartbeat credential. Owner; audited.
func (s *Server) handleMarketplacePublish(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	base, ok := s.requireMarketplace(w)
	if !ok {
		return
	}
	st := s.workflowStore(w)
	if st == nil {
		return
	}
	var req marketplacePublishRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Name, req.Version = strings.TrimSpace(req.Name), strings.TrimSpace(req.Version)
	if req.Name == "" || req.Version == "" {
		jsonError(w, "name and version are required", http.StatusBadRequest)
		return
	}
	b, err := s.exportBundle(st, req.Name, req.Version, requestUser(r))
	if err != nil {
		if errors.Is(err, workflowmodel.ErrCompile) || errors.Is(err, workflowmodel.ErrInvalid) {
			jsonError(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		workflowStoreError(w, err)
		return
	}
	// A spoke publishes from its own library: the hub records such bundles
	// as local-published whatever the model's provenance says.
	if b.Tier != bundle.TierIngestedUnreviewed {
		b.Tier = bundle.TierLocalPublished
	}
	payload, err := bundle.Encode(b)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), marketplaceHubTimeout)
	defer cancel()
	hreq, err := s.hubRequest(ctx, http.MethodPost, base+"/api/marketplace/workflows", payload)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := marketplaceHTTP.Do(hreq)
	if err != nil {
		jsonError(w, "hub unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		code := http.StatusBadGateway
		switch resp.StatusCode {
		case http.StatusConflict:
			code = http.StatusConflict
		case http.StatusUnauthorized, http.StatusForbidden:
			code = http.StatusForbidden
		case http.StatusUnprocessableEntity, http.StatusBadRequest:
			code = http.StatusUnprocessableEntity
		}
		jsonError(w, fmt.Sprintf("hub refused publish (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(hubErrorMessage(msg))), code)
		return
	}
	marketplaceCache.mu.Lock()
	marketplaceCache.fetchedAt = time.Time{}
	marketplaceCache.mu.Unlock()
	s.auditFromRequest(r, "workflow_publish", fmt.Sprintf("%s hash=%s tier=%s to %s", b.Key(), shortHash(b.Hash), b.Tier, base), "")
	jsonStatusResponse(w, http.StatusCreated, map[string]interface{}{"published": b.Key(), "hash": b.Hash, "tier": b.Tier})
}

// handleMarketplaceRefresh asks the hub to re-ingest its corpus (or one
// ?repo=). Owner on the spoke; the request carries the spoke's heartbeat
// bearer + X-Hive-ID, which the hub accepts for refresh the way it does
// for publish. The hub's error body is relayed verbatim: 401/403 → 403,
// 429 (hive rate limit) and 409 (run in progress) keep their status, and
// anything else → 502.
func (s *Server) handleMarketplaceRefresh(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	base, ok := s.requireMarketplace(w)
	if !ok {
		return
	}
	u := base + "/api/marketplace/refresh"
	if repo := strings.TrimSpace(r.URL.Query().Get("repo")); repo != "" {
		u += "?repo=" + url.QueryEscape(repo)
	}
	ctx, cancel := context.WithTimeout(r.Context(), marketplaceHubTimeout)
	defer cancel()
	hreq, err := s.hubRequest(ctx, http.MethodPost, u, []byte("{}"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := marketplaceHTTP.Do(hreq)
	if err != nil {
		jsonError(w, "hub unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer closeHTTPBody(resp.Body)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, marketplaceMaxBody))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		jsonError(w, "hub refused refresh: "+strings.TrimSpace(hubErrorMessage(body)), http.StatusForbidden)
		return
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusConflict:
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		jsonError(w, "hub refresh not started: "+strings.TrimSpace(hubErrorMessage(body)), resp.StatusCode)
		return
	case resp.StatusCode >= 300:
		jsonError(w, fmt.Sprintf("hub refresh failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(hubErrorMessage(body))), http.StatusBadGateway)
		return
	}
	marketplaceCache.mu.Lock()
	marketplaceCache.fetchedAt = time.Time{}
	marketplaceCache.mu.Unlock()
	s.auditFromRequest(r, "workflow_marketplace_refresh", "requested hub refresh "+r.URL.Query().Get("repo"), "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if json.Valid(body) {
		_, _ = w.Write(body)
	} else {
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}
}

// hubErrorMessage extracts {"error": "..."} from a hub reply, else the raw text.
func hubErrorMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return string(body)
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
