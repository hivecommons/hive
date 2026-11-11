package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

func resetMarketplaceCache(t *testing.T) {
	t.Helper()
	clear := func() {
		marketplaceCache.mu.Lock()
		marketplaceCache.body, marketplaceCache.etag, marketplaceCache.fetchedAt = nil, "", time.Time{}
		marketplaceCache.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func saveTechdocs(t *testing.T, s *Server) {
	t.Helper()
	m := techdocsModel(t)
	if _, err := s.deps.WorkflowStore.Save(m, "andy", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// uncompilableTechdocs validates (so the store accepts it) but does not
// compile: its first gate names no releasing role.
func uncompilableTechdocs(t *testing.T) *workflowmodel.Model {
	t.Helper()
	m := techdocsModel(t)
	m.Spec.Meta.Version = "9"
	m.Spec.Gates[0].ReleasedBy = ""
	return m
}

func marketplaceServer(t *testing.T, hubURL string) *Server {
	t.Helper()
	resetMarketplaceCache(t)
	s := workflowTestServer(t, true)
	s.deps.Config.Workflow.Marketplace.URL = hubURL
	s.deps.Config.HiveID = "spoke-1"
	t.Setenv("HIVE_HUB_SECRET", "test-hub-secret-marketplace")
	t.Setenv("HIVE_ID", "spoke-1")
	return s
}

// fakeHub is a minimal marketplace hub: one bundle, an ETag'd index, and a
// recorder of what the spoke sent.
type fakeHub struct {
	t         *testing.T
	bundle    []byte
	index     []byte
	hits      atomic.Int32
	notMod    atomic.Int32
	published [][]byte
	lastAuth  string
	lastHive  string
	refreshes atomic.Int32
	publishRC int
}

func newFakeHub(t *testing.T) (*fakeHub, *httptest.Server) {
	t.Helper()
	b, err := bundle.Export(techdocsModel(t), "hub", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b.Tier = bundle.TierCurated
	raw, _ := bundle.Encode(b)
	e := bundle.EntryFor(b, time.Now(), "hub", "")
	idx, _ := json.Marshal(bundle.Index{GeneratedAt: time.Now(), Entries: []bundle.IndexEntry{e}})
	h := &fakeHub{t: t, bundle: raw, index: idx, publishRC: http.StatusCreated}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.lastAuth = r.Header.Get("Authorization")
		h.lastHive = r.Header.Get(marketplaceHiveIDHeader)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/marketplace/workflows":
			h.hits.Add(1)
			if r.Header.Get("If-None-Match") == `"idx-1"` {
				h.notMod.Add(1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"idx-1"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(h.index)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/bundle"):
			if !strings.Contains(r.URL.Path, "/cncf-techdocs/1/") {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			_, _ = w.Write(h.bundle)
		case r.Method == http.MethodPost && r.URL.Path == "/api/marketplace/workflows":
			body, _ := io.ReadAll(r.Body)
			h.published = append(h.published, body)
			w.WriteHeader(h.publishRC)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/marketplace/refresh":
			h.refreshes.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get(marketplaceHiveIDHeader) == "" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"accepted":true,"repo":"` + r.URL.Query().Get("repo") + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

func TestWorkflowBundleGetAndImportRoundTrip(t *testing.T) {
	s := marketplaceServer(t, "")
	saveTechdocs(t, s)

	rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/1/bundle", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle get: %d %s", rec.Code, rec.Body.String())
	}
	b, err := bundle.Decode(rec.Body.Bytes())
	if err != nil || bundle.Verify(b) != nil {
		t.Fatalf("exported bundle does not verify: %v", err)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/missing/1/bundle", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}

	// Import into a fresh store: version 1 is free so it stays pinned.
	s2 := marketplaceServer(t, "")
	rec = wfReq(s2, http.MethodPost, "/api/workflows/import", rec.Body.String(), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entry    wfstore.Entry `json:"entry"`
		Verified bool          `json:"verified"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Verified || out.Entry.Key() != "cncf-techdocs@1" || !out.Entry.Compiled {
		t.Fatalf("import result = %+v", out)
	}
	// Same bundle again: version 1 is taken, so it lands at 2.
	raw, _ := bundle.Encode(b)
	rec = wfReq(s2, http.MethodPost, "/api/workflows/import", string(raw), true)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusCreated || out.Entry.Version != "2" {
		t.Fatalf("second import: %d %+v", rec.Code, out.Entry)
	}
	// Non-owner, bad JSON, tampered.
	if rec := wfReq(s2, http.MethodPost, "/api/workflows/import", string(raw), false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner import: %d", rec.Code)
	}
	if rec := wfReq(s2, http.MethodPost, "/api/workflows/import", "{", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rec.Code)
	}
	b.Model.Spec.Meta.Description = "tampered"
	raw, _ = bundle.Encode(b)
	rec = wfReq(s2, http.MethodPost, "/api/workflows/import", string(raw), true)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "verification failed") {
		t.Fatalf("tampered import: %d %s", rec.Code, rec.Body.String())
	}
	// Built-in names are refused.
	m := techdocsModel(t)
	m.Metadata.Name = "hive-default"
	bb, _ := bundle.Export(m, "x", time.Now())
	raw, _ = bundle.Encode(bb)
	if rec := wfReq(s2, http.MethodPost, "/api/workflows/import", string(raw), true); rec.Code != http.StatusForbidden {
		t.Fatalf("built-in import: %d %s", rec.Code, rec.Body.String())
	}
	// Without a store.
	noStore := workflowTestServer(t, false)
	if rec := wfReq(noStore, http.MethodPost, "/api/workflows/import", string(raw), true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store: %d", rec.Code)
	}
	if rec := wfReq(noStore, http.MethodGet, "/api/workflows/x/1/bundle", nil, false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no store get: %d", rec.Code)
	}
}

func TestWorkflowBundleGetRefusesUncompilable(t *testing.T) {
	s := marketplaceServer(t, "")
	m := uncompilableTechdocs(t)
	if _, err := s.deps.WorkflowStore.Save(m, "andy", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := wfReq(s, http.MethodGet, "/api/workflows/cncf-techdocs/9/bundle", nil, false); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("uncompilable export: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplaceIndexProxyCachesWithETag(t *testing.T) {
	hub, srv := newFakeHub(t)
	s := marketplaceServer(t, srv.URL)

	rec := wfReq(s, http.MethodGet, "/api/marketplace/workflows", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("index: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Stale bool         `json:"stale"`
		ETag  string       `json:"etag"`
		Index bundle.Index `json:"index"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Stale || out.ETag != `"idx-1"` || len(out.Index.Entries) != 1 || out.Index.Entries[0].Tier != bundle.TierCurated {
		t.Fatalf("index out = %+v", out)
	}
	if hub.lastAuth == "" || !strings.HasPrefix(hub.lastAuth, "Bearer ") || hub.lastHive != "spoke-1" {
		t.Fatalf("spoke credentials not sent: auth=%q hive=%q", hub.lastAuth, hub.lastHive)
	}
	// Within TTL: served from cache, no hub hit.
	wfReq(s, http.MethodGet, "/api/marketplace/workflows", nil, false)
	if hub.hits.Load() != 1 {
		t.Fatalf("expected cached, hub hits=%d", hub.hits.Load())
	}
	// ?refresh=1 revalidates with If-None-Match → 304.
	rec = wfReq(s, http.MethodGet, "/api/marketplace/workflows?refresh=1", nil, false)
	if rec.Code != http.StatusOK || hub.notMod.Load() != 1 {
		t.Fatalf("revalidate: %d notMod=%d", rec.Code, hub.notMod.Load())
	}
	// Hub down → 502 with cached index and stale:true.
	srv.Close()
	rec = wfReq(s, http.MethodGet, "/api/marketplace/workflows?refresh=1", nil, false)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusBadGateway || !out.Stale || len(out.Index.Entries) != 1 {
		t.Fatalf("stale: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplaceIndexProxyErrors(t *testing.T) {
	// Hub returns 500 with an empty cache → 502, empty index, stale.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	s := marketplaceServer(t, bad.URL)
	rec := wfReq(s, http.MethodGet, "/api/marketplace/workflows", nil, false)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"stale":true`) || !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("hub 500: %d %s", rec.Code, rec.Body.String())
	}
	// Disabled → 404.
	off := false
	s.deps.Config.Workflow.Marketplace.Enabled = &off
	if rec := wfReq(s, http.MethodGet, "/api/marketplace/workflows", nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: %d", rec.Code)
	}
	s.deps.Config.Workflow.Marketplace.Enabled = nil
	// No usable URL → 503.
	s.deps.Config.Workflow.Marketplace.URL = "not a url"
	s.deps.Config.Hub.URL = ""
	if rec := wfReq(s, http.MethodGet, "/api/marketplace/workflows", nil, false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no url: %d %s", rec.Code, rec.Body.String())
	}
	// Garbage index body with empty cache → stale 502.
	resetMarketplaceCache(t)
	bad2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer bad2.Close()
	s.deps.Config.Workflow.Marketplace.URL = bad2.URL
	if rec := wfReq(s, http.MethodGet, "/api/marketplace/workflows", nil, false); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "unreadable") {
		t.Fatalf("garbage: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplaceImportFromHub(t *testing.T) {
	hub, srv := newFakeHub(t)
	s := marketplaceServer(t, srv.URL)

	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/cncf-techdocs/1/import", nil, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", rec.Code)
	}
	rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/cncf-techdocs/1/import", nil, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Entry  wfstore.Entry `json:"entry"`
		Source string        `json:"source"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Entry.Key() != "cncf-techdocs@1" || out.Source != "cncf-techdocs@1" {
		t.Fatalf("out = %+v", out)
	}
	m, _, err := s.deps.WorkflowStore.Get("cncf-techdocs", "1")
	if err != nil || m.Spec.Meta.Provenance != workflowmodel.ProvenanceCurated {
		t.Fatalf("imported provenance = %v %v", m.Spec.Meta.Provenance, err)
	}
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/nothere/1/import", nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("missing on hub: %d %s", rec.Code, rec.Body.String())
	}
	// Re-import lands on the next free version.
	rec = wfReq(s, http.MethodPost, "/api/marketplace/workflows/cncf-techdocs/1/import", nil, true)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusCreated || out.Entry.Version != "2" {
		t.Fatalf("re-import: %d %+v", rec.Code, out.Entry)
	}
	// Tampered bundle on hub → 422.
	b, _ := bundle.Decode(hub.bundle)
	b.Profile.Columns = nil
	hub.bundle, _ = bundle.Encode(b)
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/cncf-techdocs/1/import", nil, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tampered hub bundle: %d %s", rec.Code, rec.Body.String())
	}
	// Invalid bundle JSON on hub → 502.
	hub.bundle = []byte("{")
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/cncf-techdocs/1/import", nil, true); rec.Code != http.StatusBadGateway {
		t.Fatalf("invalid hub bundle: %d", rec.Code)
	}
	// Hub down → 502.
	srv.Close()
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/cncf-techdocs/1/import", nil, true); rec.Code != http.StatusBadGateway {
		t.Fatalf("hub down: %d", rec.Code)
	}
}

func TestMarketplaceImportKeyMismatch(t *testing.T) {
	// A hub that answers every bundle path with the same bundle.
	b, _ := bundle.Export(techdocsModel(t), "hub", time.Now())
	raw, _ := bundle.Encode(b)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw) }))
	defer srv.Close()
	s := marketplaceServer(t, srv.URL)
	rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/other/7/import", nil, true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "hub returned cncf-techdocs@1") {
		t.Fatalf("mismatch: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMarketplacePublish(t *testing.T) {
	hub, srv := newFakeHub(t)
	s := marketplaceServer(t, srv.URL)
	saveTechdocs(t, s)

	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": "cncf-techdocs", "version": "1"}, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": ""}, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", "{", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rec.Code)
	}
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": "nope", "version": "1"}, true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown entry: %d", rec.Code)
	}
	rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": "cncf-techdocs", "version": "1"}, true)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"published":"cncf-techdocs@1"`) {
		t.Fatalf("publish: %d %s", rec.Code, rec.Body.String())
	}
	if len(hub.published) != 1 {
		t.Fatalf("hub received %d bundles", len(hub.published))
	}
	sent, err := bundle.Decode(hub.published[0])
	if err != nil || bundle.Verify(sent) != nil || sent.Tier != bundle.TierLocalPublished {
		t.Fatalf("sent bundle = %v tier=%q", err, sent.Tier)
	}
	if !strings.HasPrefix(hub.lastAuth, "Bearer ") || hub.lastHive != "spoke-1" {
		t.Fatalf("credentials: %q %q", hub.lastAuth, hub.lastHive)
	}
	// Hub refusals map onto spoke statuses.
	for hubRC, want := range map[int]int{
		http.StatusConflict:            http.StatusConflict,
		http.StatusUnauthorized:        http.StatusForbidden,
		http.StatusForbidden:           http.StatusForbidden,
		http.StatusUnprocessableEntity: http.StatusUnprocessableEntity,
		http.StatusInternalServerError: http.StatusBadGateway,
	} {
		hub.publishRC = hubRC
		rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": "cncf-techdocs", "version": "1"}, true)
		if rec.Code != want || !strings.Contains(rec.Body.String(), "nope") {
			t.Fatalf("hub %d → %d (want %d): %s", hubRC, rec.Code, want, rec.Body.String())
		}
	}
	// Uncompilable entry → 422 before any hub call.
	if _, err := s.deps.WorkflowStore.Save(uncompilableTechdocs(t), "andy", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": "cncf-techdocs", "version": "9"}, true); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("uncompilable publish: %d", rec.Code)
	}
	srv.Close()
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/workflows/publish", map[string]string{"name": "cncf-techdocs", "version": "1"}, true); rec.Code != http.StatusBadGateway {
		t.Fatalf("hub down: %d", rec.Code)
	}
}

func TestMarketplaceRefreshProxy(t *testing.T) {
	hub, srv := newFakeHub(t)
	s := marketplaceServer(t, srv.URL)
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/refresh", nil, false); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", rec.Code)
	}
	rec := wfReq(s, http.MethodPost, "/api/marketplace/refresh?repo=org/repo", nil, true)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"repo":"org/repo"`) || hub.refreshes.Load() != 1 {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	deny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.RawQuery, "boom"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("plain text failure"))
		case strings.Contains(r.URL.RawQuery, "slow"):
			w.Header().Set("Retry-After", "42")
			http.Error(w, `{"error":"full catalogue refresh was requested recently by a hive; try again in 42s"}`, http.StatusTooManyRequests)
		case strings.Contains(r.URL.RawQuery, "busy"):
			http.Error(w, `{"error":"marketplace: refresh already running"}`, http.StatusConflict)
		default:
			http.Error(w, `{"error":"admin only"}`, http.StatusForbidden)
		}
	}))
	defer deny.Close()
	s.deps.Config.Workflow.Marketplace.URL = deny.URL
	rec = wfReq(s, http.MethodPost, "/api/marketplace/refresh", nil, true)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin only") {
		t.Fatalf("denied: %d %s", rec.Code, rec.Body.String())
	}
	// Hub rate limit and busy keep their status and message so the toast
	// tells the owner when to retry.
	rec = wfReq(s, http.MethodPost, "/api/marketplace/refresh?repo=slow/x", nil, true)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "42" || !strings.Contains(rec.Body.String(), "try again in 42s") {
		t.Fatalf("rate limited: %d %s retry-after=%q", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	rec = wfReq(s, http.MethodPost, "/api/marketplace/refresh?repo=busy/x", nil, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already running") {
		t.Fatalf("busy: %d %s", rec.Code, rec.Body.String())
	}
	rec = wfReq(s, http.MethodPost, "/api/marketplace/refresh?repo=boom", nil, true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "plain text failure") {
		t.Fatalf("hub 500: %d %s", rec.Code, rec.Body.String())
	}
	srv.Close()
	s.deps.Config.Workflow.Marketplace.URL = srv.URL
	if rec := wfReq(s, http.MethodPost, "/api/marketplace/refresh", nil, true); rec.Code != http.StatusBadGateway {
		t.Fatalf("hub down: %d", rec.Code)
	}
}

func TestMarketplaceBaseFallsBackToHubURL(t *testing.T) {
	s := marketplaceServer(t, "")
	s.deps.Config.Hub.URL = "https://hub.example/"
	if got := s.marketplaceBase(); got != "https://hub.example" {
		t.Fatalf("base = %q", got)
	}
	bare := &Server{}
	if !bare.marketplaceEnabled() || bare.marketplaceBase() != "" {
		t.Fatal("server without deps should be enabled but have no base")
	}
}

func TestImportedProvenance(t *testing.T) {
	cases := []struct {
		model workflowmodel.Provenance
		tier  string
		want  workflowmodel.Provenance
	}{
		// Empty model provenance takes the envelope tier.
		{"", bundle.TierCurated, workflowmodel.ProvenanceCurated},
		{"", bundle.TierLocalPublished, workflowmodel.ProvenanceLocal},
		{"", bundle.TierBuiltIn, workflowmodel.ProvenanceLocal},
		{"", "", workflowmodel.ProvenanceLocal},
		// A vouched tier the envelope agrees with is kept (built-in still lands as local).
		{workflowmodel.ProvenanceCurated, bundle.TierCurated, workflowmodel.ProvenanceCurated},
		{workflowmodel.ProvenanceBuiltIn, bundle.TierBuiltIn, workflowmodel.ProvenanceLocal},
		// A vouched tier the envelope does not assert is laundering: fall back to the tier.
		{workflowmodel.ProvenanceCurated, bundle.TierLocalPublished, workflowmodel.ProvenanceLocal},
		{workflowmodel.ProvenanceCurated, bundle.TierIngestedUnreviewed, workflowmodel.ProvenanceIngestedUnreviewed},
		{workflowmodel.ProvenanceCurated, "", workflowmodel.ProvenanceLocal},
		{workflowmodel.ProvenanceBuiltIn, bundle.TierLocalPublished, workflowmodel.ProvenanceLocal},
		// Lower tiers in the model are honoured as-is.
		{workflowmodel.ProvenanceIngestedUnreviewed, bundle.TierLocalPublished, workflowmodel.ProvenanceIngestedUnreviewed},
		{workflowmodel.ProvenanceLocal, bundle.TierCurated, workflowmodel.ProvenanceLocal},
	}
	for _, tc := range cases {
		if got := importedProvenance(tc.model, tc.tier); got != tc.want {
			t.Errorf("importedProvenance(%q, %q) = %q, want %q", tc.model, tc.tier, got, tc.want)
		}
	}
}

// A bundle whose envelope the hub normalised to local-published but whose
// hash-covered model still claims curated must not import as curated.
func TestWorkflowImportDoesNotLaunderCuratedProvenance(t *testing.T) {
	s := marketplaceServer(t, "")
	m := techdocsModel(t)
	m.Spec.Meta.Provenance = workflowmodel.ProvenanceCurated
	b, err := bundle.Export(m, "spoke", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b.Tier = bundle.TierLocalPublished
	raw, _ := bundle.Encode(b)
	rec := wfReq(s, http.MethodPost, "/api/workflows/import", string(raw), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	got, _, err := s.deps.WorkflowStore.Get("cncf-techdocs", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Meta.Provenance != workflowmodel.ProvenanceLocal {
		t.Fatalf("laundered provenance = %q, want local", got.Spec.Meta.Provenance)
	}
}
