package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/bundle"
)

const mktTestHive = "hive-mkt"

// marketplaceTestHub returns a handler hub with an isolated marketplace dir,
// a registered spoke and a fixed clock; fetch is the fake tree fetcher.
func marketplaceTestHub(t *testing.T, fetch marketplaceTreeFetcher) *HubServer {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	old := hubMarketplaceDir
	hubMarketplaceDir = filepath.Join(t.TempDir(), "marketplace")
	t.Cleanup(func() { hubMarketplaceDir = old })
	oldPause := marketplaceRepoPause
	marketplaceRepoPause = 0
	t.Cleanup(func() { marketplaceRepoPause = oldPause })
	t.Setenv(hubMarketplaceDisableEnv, "")
	t.Setenv(hubMarketplaceExtraReposEnv, "")

	s := newHandlerHub()
	s.registry.Hives = append(s.registry.Hives, RegistryEntry{ID: mktTestHive, Name: "Hive", Online: true})
	clock := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	s.marketplaceRefresher = &marketplaceRefresher{fetch: fetch, now: func() time.Time { return clock }}
	mkUser(t, hubAdminUsername)
	mkUser(t, "someuser")
	return s
}

func mktModel(t *testing.T) *workflowmodel.Model {
	t.Helper()
	raw, err := os.ReadFile("../workflowmodel/testdata/models/techdocs.json")
	if err != nil {
		t.Fatal(err)
	}
	var m workflowmodel.Model
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func mktBundle(t *testing.T, tier string) *bundle.Bundle {
	t.Helper()
	b, err := bundle.Export(mktModel(t), "tester", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if tier != "" {
		b.Tier = tier
	}
	return b
}

func mktBody(t *testing.T, b *bundle.Bundle) string {
	t.Helper()
	raw, err := bundle.Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mktPublishReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/marketplace/workflows", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func mktSpokeReq(body string) *http.Request {
	req := mktPublishReq(body)
	req.Header.Set("Authorization", heartbeatBearer(testHubSecret, mktTestHive))
	req.Header.Set(marketplaceHiveIDHeader, mktTestHive)
	return req
}

func mktAdminReq(body string) *http.Request {
	req := mktPublishReq(body)
	req.AddCookie(testAuthCookie(hubAdminUsername))
	req.Header.Set("Origin", "http://localhost")
	return req
}

func mktDo(s *HubServer, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.mux = http.NewServeMux()
	s.registerMarketplaceRoutes()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// --- store ---

func TestMarketplaceStoreImmutable(t *testing.T) {
	st := newMarketplaceStore(filepath.Join(t.TempDir(), "m"))
	b := mktBundle(t, bundle.TierLocalPublished)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	e, created, err := st.Publish(b, "a", now, "")
	if err != nil || !created || e.Key() != "cncf-techdocs@1" {
		t.Fatalf("first publish: %v %v %s", err, created, e.Key())
	}
	// Same hash → idempotent.
	_, created, err = st.Publish(b, "b", now, "")
	if err != nil || created {
		t.Fatalf("idempotent republish: %v created=%v", err, created)
	}
	// Different content under the same key → refused.
	m2 := mktModel(t)
	m2.Spec.Meta.Description = "changed"
	b2, err := bundle.Export(m2, "c", now)
	if err != nil {
		t.Fatal(err)
	}
	b2.Tier = bundle.TierLocalPublished
	if _, _, err := st.Publish(b2, "c", now, ""); !errors.Is(err, errMarketplaceConflict) {
		t.Fatalf("overwrite: want conflict, got %v", err)
	}
	// Bad keys.
	m3 := mktModel(t)
	m3.Metadata.Name = "../escape"
	b3, err := bundle.Export(m3, "c", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Publish(b3, "c", now, ""); !errors.Is(err, errMarketplaceBadKey) {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := st.Get("x", "../y"); !errors.Is(err, errMarketplaceBadKey) {
		t.Fatalf("get bad key: %v", err)
	}
	if _, err := st.Get("cncf-techdocs", "99"); !errors.Is(err, errMarketplaceNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	raw, err := st.Get("cncf-techdocs", "1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := bundle.Decode(raw)
	if err != nil || got.Hash != b.Hash {
		t.Fatalf("stored bundle: %v", err)
	}
	// Index survives a deleted index.json (rebuild from bundles).
	if err := os.Remove(st.indexPath()); err != nil {
		t.Fatal(err)
	}
	st2 := newMarketplaceStore(st.root)
	idx, _, etag, err := st2.Index()
	if err != nil || len(idx.Entries) != 1 || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("rebuilt index: %v %+v %q", err, idx, etag)
	}
	vs, err := st2.Versions("cncf-techdocs")
	if err != nil || len(vs) != 1 {
		t.Fatalf("versions: %v %d", err, len(vs))
	}
}

func TestMarketplaceNormaliseIndexFlagsCurated(t *testing.T) {
	idx := &bundle.Index{Entries: []bundle.IndexEntry{
		{Name: "a", Version: "ingested-20260314.1", Tier: bundle.TierIngestedUnreviewed, PublishedAt: time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC)},
		{Name: "a", Version: "2", Tier: bundle.TierCurated, PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "b", Version: "1", Tier: bundle.TierCurated, PublishedAt: time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC)},
		{Name: "b", Version: "ingested-20260310.1", Tier: bundle.TierIngestedUnreviewed, PublishedAt: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)},
	}}
	marketplaceNormaliseIndex(idx)
	byKey := map[string]bundle.IndexEntry{}
	for _, e := range idx.Entries {
		byKey[e.Key()] = e
		if string(e.Adoption) == "" || string(e.Ratings) == "" || string(e.Benchmarks) == "" {
			t.Errorf("%s: reserved objects not filled", e.Key())
		}
	}
	if !byKey["a@2"].NewerIngestAvailable {
		t.Error("a@2 curated should be flagged: newer ingest exists")
	}
	if byKey["b@1"].NewerIngestAvailable {
		t.Error("b@1 curated is newer than its ingest; must not be flagged")
	}
	if byKey["a@ingested-20260314.1"].NewerIngestAvailable {
		t.Error("ingested entries are never flagged")
	}
	if idx.Entries[0].Name != "a" || idx.Entries[len(idx.Entries)-1].Name != "b" {
		t.Error("index not sorted by name")
	}
}

func TestMarketplaceFilesHashStable(t *testing.T) {
	a := marketplaceFilesHash(map[string][]byte{"x": []byte("1"), "y": []byte("2")})
	b := marketplaceFilesHash(map[string][]byte{"y": []byte("2"), "x": []byte("1")})
	c := marketplaceFilesHash(map[string][]byte{"x": []byte("1"), "y": []byte("3")})
	if a != b || a == c {
		t.Fatalf("hash: %s %s %s", a, b, c)
	}
}

// --- routes ---

func TestMarketplaceIndexETag304(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	if _, _, err := s.marketplace().Publish(mktBundle(t, bundle.TierLocalPublished), "x", time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	rec := mktDo(s, httptest.NewRequest(http.MethodGet, "/api/marketplace/workflows", nil))
	if rec.Code != 200 {
		t.Fatalf("index: %d %s", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	var idx bundle.Index
	if err := json.Unmarshal(rec.Body.Bytes(), &idx); err != nil || len(idx.Entries) != 1 {
		t.Fatalf("index body: %v %s", err, rec.Body.String())
	}
	if idx.Entries[0].Tier != bundle.TierLocalPublished || string(idx.Entries[0].Adoption) != "{}" {
		t.Fatalf("entry: %+v", idx.Entries[0])
	}
	req := httptest.NewRequest(http.MethodGet, "/api/marketplace/workflows", nil)
	req.Header.Set("If-None-Match", `W/"nope", `+etag)
	if rec = mktDo(s, req); rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	req.Header.Set("If-None-Match", `"other"`)
	if rec = mktDo(s, req); rec.Code != 200 {
		t.Fatalf("stale etag: %d", rec.Code)
	}

	rec = mktDo(s, httptest.NewRequest(http.MethodGet, "/api/marketplace/workflows/cncf-techdocs/1/bundle", nil))
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("bundle: %d %s", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec = mktDo(s, httptest.NewRequest(http.MethodGet, "/api/marketplace/workflows/cncf-techdocs/9/bundle", nil)); rec.Code != 404 {
		t.Fatalf("missing bundle: %d", rec.Code)
	}
	if rec = mktDo(s, httptest.NewRequest(http.MethodGet, "/api/marketplace/workflows/bad!/1/bundle", nil)); rec.Code != 400 {
		t.Fatalf("bad key: %d", rec.Code)
	}

	t.Setenv(hubMarketplaceDisableEnv, "1")
	if rec = mktDo(s, httptest.NewRequest(http.MethodGet, "/api/marketplace/workflows", nil)); rec.Code != 404 {
		t.Fatalf("disabled: %d", rec.Code)
	}
}

func TestMarketplacePublishAuth(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	body := mktBody(t, mktBundle(t, bundle.TierLocal))

	// Anonymous.
	anon := mktPublishReq(body)
	anon.Header.Set("Origin", "http://localhost")
	if rec := mktDo(s, anon); rec.Code != 401 {
		t.Fatalf("anon: %d %s", rec.Code, rec.Body.String())
	}
	// Non-admin session.
	user := mktPublishReq(body)
	user.AddCookie(testAuthCookie("someuser"))
	user.Header.Set("Origin", "http://localhost")
	if rec := mktDo(s, user); rec.Code != 403 {
		t.Fatalf("non-admin: %d", rec.Code)
	}
	// Admin cookie without CSRF evidence.
	noCSRF := mktPublishReq(body)
	noCSRF.AddCookie(testAuthCookie(hubAdminUsername))
	noCSRF.Header.Set("Origin", "https://evil.example")
	if rec := mktDo(s, noCSRF); rec.Code != 403 {
		t.Fatalf("csrf: %d", rec.Code)
	}
	// Spoke with wrong bearer.
	bad := mktSpokeReq(body)
	bad.Header.Set("Authorization", heartbeatBearer("wrong", mktTestHive))
	if rec := mktDo(s, bad); rec.Code != 401 {
		t.Fatalf("bad bearer: %d", rec.Code)
	}
	// Spoke missing hive id.
	noID := mktSpokeReq(body)
	noID.Header.Del(marketplaceHiveIDHeader)
	if rec := mktDo(s, noID); rec.Code != 400 {
		t.Fatalf("no hive id: %d", rec.Code)
	}
	// Unknown hive (valid bearer for an unregistered id).
	unknown := mktSpokeReq(body)
	unknown.Header.Set("Authorization", heartbeatBearer(testHubSecret, "ghost"))
	unknown.Header.Set(marketplaceHiveIDHeader, "ghost")
	if rec := mktDo(s, unknown); rec.Code != 403 {
		t.Fatalf("unknown hive: %d", rec.Code)
	}
	// Spoke asserting curated → 403.
	if rec := mktDo(s, mktSpokeReq(mktBody(t, mktBundle(t, bundle.TierCurated)))); rec.Code != 403 {
		t.Fatalf("spoke curated: %d %s", rec.Code, rec.Body.String())
	}
	// Spoke OK: `local` is normalised to local-published.
	rec := mktDo(s, mktSpokeReq(body))
	if rec.Code != 201 {
		t.Fatalf("spoke publish: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Published string            `json:"published"`
		Created   bool              `json:"created"`
		Entry     bundle.IndexEntry `json:"entry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Published != "cncf-techdocs@1" || !out.Created || out.Entry.Tier != bundle.TierLocalPublished || out.Entry.PublishedBy != "hive:"+mktTestHive {
		t.Fatalf("publish result: %+v", out)
	}
	// Idempotent republish → 200.
	if rec = mktDo(s, mktSpokeReq(body)); rec.Code != 200 {
		t.Fatalf("republish: %d", rec.Code)
	}
	// Conflicting content under the same key → 409.
	conflict := mktBundle(t, bundle.TierLocalPublished)
	conflict.Model.Spec.Meta.Description = "changed"
	conflict.Hash = bundle.CanonicalHash(conflict.Model)
	conflict.Profile = nil
	cb, err := bundle.Export(conflict.Model, "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cb.Tier = bundle.TierLocalPublished
	if rec = mktDo(s, mktSpokeReq(mktBody(t, cb))); rec.Code != 409 {
		t.Fatalf("conflict: %d %s", rec.Code, rec.Body.String())
	}
	// Admin may publish curated (different version).
	curModel := mktModel(t)
	curModel.Spec.Meta.Version = "2"
	cur, err := bundle.Export(curModel, "admin", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cur.Tier = bundle.TierCurated
	if rec = mktDo(s, mktAdminReq(mktBody(t, cur))); rec.Code != 201 {
		t.Fatalf("admin curated: %d %s", rec.Code, rec.Body.String())
	}
	// Tampered bundle → 422.
	tampered := mktBundle(t, bundle.TierLocalPublished)
	tampered.Model.Spec.Meta.Version = "3"
	if rec = mktDo(s, mktSpokeReq(mktBody(t, tampered))); rec.Code != 422 {
		t.Fatalf("tampered: %d %s", rec.Code, rec.Body.String())
	}
	// Garbage / unknown tier / too large.
	if rec = mktDo(s, mktSpokeReq("{nope")); rec.Code != 400 {
		t.Fatalf("garbage: %d", rec.Code)
	}
	if rec = mktDo(s, mktSpokeReq(mktBody(t, mktBundle(t, "platinum")))); rec.Code != 400 {
		t.Fatalf("unknown tier: %d", rec.Code)
	}
	if rec = mktDo(s, mktSpokeReq(strings.Repeat(" ", marketplaceMaxBundleBytes+2))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", rec.Code)
	}
	// No hub secret → 503 for spoke lane.
	s.hubSecret = ""
	if rec = mktDo(s, mktSpokeReq(body)); rec.Code != 503 {
		t.Fatalf("no secret: %d", rec.Code)
	}
}

// --- refresh ---

func mktFixtureTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	root := filepath.Join("..", "workflowmodel", "testdata", "corpus", dir)
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestMarketplaceRefreshNoopVsNewVersion(t *testing.T) {
	trees := map[string]map[string][]byte{
		"envoyproxy/envoy":   mktFixtureTree(t, "envoyproxy__envoy"),
		"example/empty":      {},
		"example/broken":     nil,
		"konflux-ci/konflux": mktFixtureTree(t, "konflux-ci__konflux-ci"),
	}
	shas := map[string]string{"envoyproxy/envoy": "sha1", "example/empty": "e", "konflux-ci/konflux": "k"}
	calls := 0
	fetch := func(ctx context.Context, repo string) (map[string][]byte, string, error) {
		calls++
		if repo == "example/broken" {
			return nil, "", errors.New("boom")
		}
		return trees[repo], shas[repo], nil
	}
	s := marketplaceTestHub(t, fetch)
	repos := []string{"envoyproxy/envoy", "example/empty", "example/broken", "konflux-ci/konflux"}

	res, err := s.runMarketplaceRefresh(context.Background(), repos, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Published) != 2 || res.Skipped != 1 || len(res.Errors) != 1 || res.Unchanged != 0 {
		t.Fatalf("first run: %+v", res)
	}
	for _, k := range res.Published {
		if !strings.HasSuffix(k, "@ingested-20260314.1") {
			t.Errorf("version naming: %s", k)
		}
	}
	idx, _, _, err := s.marketplace().Index()
	if err != nil || len(idx.Entries) != 2 {
		t.Fatalf("index after first run: %v %d", err, len(idx.Entries))
	}
	for _, e := range idx.Entries {
		if e.Tier != bundle.TierIngestedUnreviewed || e.SourceSHA == "" {
			t.Errorf("entry: %+v", e)
		}
	}

	// Second run with identical trees → all unchanged, nothing published.
	res, err = s.runMarketplaceRefresh(context.Background(), repos, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Published) != 0 || res.Unchanged != 3 || len(res.Errors) != 1 {
		t.Fatalf("second run: %+v", res)
	}

	// Change a pipeline file → new immutable version .2; the old one stays.
	trees["envoyproxy/envoy"][".github/workflows/envoy-release.yml"] = append(trees["envoyproxy/envoy"][".github/workflows/envoy-release.yml"], []byte("\n# touched\n")...)
	shas["envoyproxy/envoy"] = "sha2"
	res, err = s.runMarketplaceRefresh(context.Background(), []string{"envoyproxy/envoy"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Published) != 1 || !strings.HasSuffix(res.Published[0], "@ingested-20260314.2") {
		t.Fatalf("third run: %+v", res)
	}
	name := strings.Split(res.Published[0], "@")[0]
	vs, err := s.marketplace().Versions(name)
	if err != nil || len(vs) != 2 {
		t.Fatalf("versions: %v %d", err, len(vs))
	}

	// A curated version of the same name is flagged, never overwritten.
	curRaw, err := s.marketplace().Get(name, "ingested-20260314.1")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := bundle.Decode(curRaw)
	if err != nil {
		t.Fatal(err)
	}
	cur.Model.Spec.Meta.Version = "1"
	cur.Model.Spec.Meta.Provenance = workflowmodel.ProvenanceCurated
	cur, err = bundle.Export(cur.Model, "admin", time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	cur.Tier = bundle.TierCurated
	if _, _, err := s.marketplace().Publish(cur, "admin", time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC), ""); err != nil {
		t.Fatal(err)
	}
	idx, _, _, err = s.marketplace().Index()
	if err != nil {
		t.Fatal(err)
	}
	var sawCurated bool
	for _, e := range idx.Entries {
		if e.Name == name && e.Tier == bundle.TierCurated {
			sawCurated = true
			if !e.NewerIngestAvailable {
				t.Errorf("curated %s should be flagged newerIngestAvailable", e.Key())
			}
		}
	}
	if !sawCurated {
		t.Fatal("curated entry missing")
	}
	if calls != 4+4+1 {
		t.Errorf("fetch calls: %d", calls)
	}

	// Refresh records.
	recs := s.marketplace().refreshRecords()
	if len(recs) != 4 {
		t.Fatalf("records: %d", len(recs))
	}
	rec, ok := s.marketplace().refreshRecord("example/broken")
	if !ok || rec.Error == "" {
		t.Fatalf("broken record: %v %+v", ok, rec)
	}
}

func TestMarketplaceRefreshRoutesAndStatus(t *testing.T) {
	fetch := func(ctx context.Context, repo string) (map[string][]byte, string, error) {
		if repo != "konflux-ci/konflux-ci" {
			return map[string][]byte{}, "none", nil // corpus repos: no pipelines → skipped
		}
		return mktFixtureTree(t, "konflux-ci__konflux-ci"), "k1", nil
	}
	s := marketplaceTestHub(t, fetch)

	// Non-admin / anon are refused by requireAdmin.
	rec := mktDo(s, httptest.NewRequest(http.MethodPost, "/api/marketplace/refresh?repo=a/b", nil))
	if rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("anon refresh: %d", rec.Code)
	}
	admin := func(target string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.AddCookie(testAuthCookie(hubAdminUsername))
		req.Header.Set("Origin", "http://localhost")
		return req
	}
	if rec = mktDo(s, admin("/api/marketplace/refresh?repo=nope")); rec.Code != 400 {
		t.Fatalf("bad repo: %d", rec.Code)
	}
	rec = mktDo(s, admin("/api/marketplace/refresh?repo=Konflux-CI/konflux-ci"))
	if rec.Code != 200 {
		t.Fatalf("single repo: %d %s", rec.Code, rec.Body.String())
	}
	var single struct {
		Repo   string                   `json:"repo"`
		Result marketplaceRefreshResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if single.Repo != "konflux-ci/konflux-ci" || len(single.Result.Published) != 1 {
		t.Fatalf("single result: %+v", single)
	}

	// Full run while another is running → 409; otherwise 202.
	rf := s.refresher()
	rf.mu.Lock()
	rf.running = true
	rf.mu.Unlock()
	if rec = mktDo(s, admin("/api/marketplace/refresh")); rec.Code != 409 {
		t.Fatalf("busy: %d", rec.Code)
	}
	if rec = mktDo(s, admin("/api/marketplace/refresh?repo=x/y")); rec.Code != 409 {
		t.Fatalf("busy single: %d", rec.Code)
	}
	rf.mu.Lock()
	rf.running = false
	rf.mu.Unlock()
	t.Setenv(hubMarketplaceExtraReposEnv, "konflux-ci/konflux-ci")
	if rec = mktDo(s, admin("/api/marketplace/refresh")); rec.Code != 202 {
		t.Fatalf("full: %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rf.mu.Lock()
		last := rf.last
		running := rf.running
		rf.mu.Unlock()
		if !running && last != nil && strings.HasPrefix(last.Trigger, "on-demand:") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec = mktDo(s, httptest.NewRequest(http.MethodGet, "/api/marketplace/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	var status map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["entries"].(float64) != 1 || status["schedule"] != hubMarketplaceDefaultSchedule || status["lastRefresh"] == nil || status["lastCheckedAt"] == nil {
		t.Fatalf("status body: %s", rec.Body.String())
	}
	byTier := status["byTier"].(map[string]interface{})
	if byTier[bundle.TierIngestedUnreviewed].(float64) != 1 {
		t.Fatalf("byTier: %v", byTier)
	}
}

func TestMarketplaceReposMergesExtra(t *testing.T) {
	t.Setenv(hubMarketplaceExtraReposEnv, " Foo/Bar ,foo/bar, nope ,")
	repos := marketplaceRepos()
	n := 0
	for _, r := range repos {
		if r == "foo/bar" {
			n++
		}
		if r == "nope" || r != strings.ToLower(strings.TrimSpace(r)) {
			t.Errorf("bad repo passed through: %q", r)
		}
	}
	if n != 1 || len(repos) < 200 {
		t.Fatalf("repos: foo/bar×%d, total %d", n, len(repos))
	}
}

func TestMarketplaceNextRun(t *testing.T) {
	now := time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)
	if _, ok := marketplaceNextRun("", now); ok {
		t.Error("empty schedule should disable")
	}
	if _, ok := marketplaceNextRun("25:99", now); ok {
		t.Error("invalid schedule should disable")
	}
	next, ok := marketplaceNextRun("03:00", now)
	if !ok || !next.Equal(time.Date(2026, 3, 15, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("03:00 after noon → tomorrow, got %v", next)
	}
	next, _ = marketplaceNextRun("13:30", now)
	if !next.Equal(time.Date(2026, 3, 14, 13, 30, 0, 0, time.UTC)) {
		t.Errorf("13:30 → today, got %v", next)
	}
	t.Setenv(hubMarketplaceScheduleEnv, "")
	if marketplaceSchedule() != "" {
		t.Error("explicit empty env must disable")
	}
	os.Unsetenv(hubMarketplaceScheduleEnv)
	if marketplaceSchedule() != hubMarketplaceDefaultSchedule {
		t.Error("unset env → default")
	}
}

func TestMarketplaceHonourRateLimit(t *testing.T) {
	var slept time.Duration
	old := marketplaceSleep
	marketplaceSleep = func(ctx context.Context, d time.Duration) { slept += d }
	t.Cleanup(func() { marketplaceSleep = old })

	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "1000")
	marketplaceHonourRateLimit(context.Background(), h)
	if slept != 0 {
		t.Fatal("plenty remaining must not sleep")
	}
	h.Set("X-RateLimit-Remaining", "3")
	h.Set("X-RateLimit-Reset", "garbage")
	marketplaceHonourRateLimit(context.Background(), h)
	if slept != 0 {
		t.Fatal("unparseable reset must not sleep")
	}
	h.Set("X-RateLimit-Reset", "1")
	marketplaceHonourRateLimit(context.Background(), h)
	if slept != 0 {
		t.Fatal("past reset must not sleep")
	}
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(2*time.Hour).Unix(), 10))
	marketplaceHonourRateLimit(context.Background(), h)
	if slept != marketplaceRateMaxWait {
		t.Fatalf("far reset should cap at max wait, slept %v", slept)
	}
}

func TestMarketplaceStartRefreshDisabledAndSeedGate(t *testing.T) {
	s := marketplaceTestHub(t, nil)
	t.Setenv(hubMarketplaceDisableEnv, "1")
	done := make(chan struct{})
	go func() { s.StartMarketplaceRefresh(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled StartMarketplaceRefresh should return immediately")
	}
	t.Setenv(hubMarketplaceDisableEnv, "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("HIVE_HUB_GITHUB_TOKEN", "")
	if s.marketplaceShouldSeed() {
		t.Error("no token → no seed")
	}

	// Empty schedule: loop parks on ctx and exits on cancel.
	t.Setenv(hubMarketplaceScheduleEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { s.StartMarketplaceRefresh(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StartMarketplaceRefresh did not stop on cancel")
	}
}

func mktTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "repo-abc/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMarketplaceGitHubFetcher(t *testing.T) {
	tree := mktFixtureTree(t, "konflux-ci__konflux-ci")
	strTree := map[string]string{"README.md": "hi"}
	for k, v := range tree {
		strTree[k] = string(v)
	}
	tarball := mktTarball(t, strTree)
	var failBranch bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "4000")
		switch r.URL.Path {
		case "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/acme/widgets/branches/main":
			if failBranch {
				http.Error(w, "nope", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"commit":{"sha":"abc123"}}`))
		case "/repos/acme/widgets/tarball/main":
			_, _ = w.Write(tarball)
		case "/repos/acme/nobranch":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldBase })
	t.Setenv(hubGitHubTokenEnv, "pat-token")

	s := marketplaceTestHub(t, nil)
	files, sha, err := s.marketplaceGitHubFetcher(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if sha != "abc123" || len(files) != len(tree) {
		t.Fatalf("fetch: sha=%s files=%d want %d", sha, len(files), len(tree))
	}
	if _, ok := files["README.md"]; ok {
		t.Error("ReadTree should only keep pipeline files")
	}
	if _, _, err := s.marketplaceGitHubFetcher(context.Background(), "acme/missing"); err == nil {
		t.Error("404 repo should fail")
	}
	if _, _, err := s.marketplaceGitHubFetcher(context.Background(), "acme/nobranch"); err == nil {
		t.Error("missing default branch should fail")
	}
	if _, _, err := s.marketplaceGitHubFetcher(context.Background(), "garbage"); err == nil {
		t.Error("bad repo spec should fail")
	}
	failBranch = true
	if _, _, err := s.marketplaceGitHubFetcher(context.Background(), "acme/widgets"); err == nil {
		t.Error("branch lookup failure should fail")
	}

	// With a token and an empty catalogue the seed gate opens.
	if !s.marketplaceShouldSeed() {
		t.Error("token + empty catalogue → should seed")
	}
}
