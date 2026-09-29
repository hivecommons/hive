package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// npsTestSecret is the hub master the ingest tests sign bearers with.
const npsTestSecret = "nps-test-secret"

// useTempNPSStore points the NPS store at a fresh temp file for one test. The
// store reloads whenever npsStorePath changes, so nothing leaks between tests.
func useTempNPSStore(t *testing.T) string {
	t.Helper()
	old := npsStorePath
	npsStorePath = filepath.Join(t.TempDir(), "hub-nps.json")
	t.Cleanup(func() {
		npsStorePath = old
		hubNPS.mu.Lock()
		hubNPS.loaded = false
		hubNPS.records = nil
		hubNPS.mu.Unlock()
	})
	return npsStorePath
}

func npsTestHub(hives ...string) *HubServer {
	s := newTestHubServer(npsTestSecret)
	for _, id := range hives {
		s.registry.Hives = append(s.registry.Hives, RegistryEntry{ID: id, Name: "Hive " + id, Online: true})
	}
	return s
}

func npsIngestReq(body io.Reader, bearer string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, npsIngestPath, body)
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

func npsIngest(s *HubServer, body, bearer string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleNPSIngest(rec, npsIngestReq(strings.NewReader(body), bearer))
	return rec
}

func npsStored(t *testing.T) []npsRecord {
	t.Helper()
	recs, err := hubNPS.snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return recs
}

func TestNPSCategoryFromRawScore(t *testing.T) {
	want := map[int]string{
		1: npsCategoryDetractor,
		2: npsCategoryPassive,
		3: npsCategoryPassive,
		4: npsCategoryPromoter,
	}
	for score, cat := range want {
		if got := npsCategory(score); got != cat {
			t.Errorf("npsCategory(%d) = %q, want %q", score, got, cat)
		}
	}
}

// TestNPSRoundMatchesJavaScript pins half-up rounding so the hub agrees with
// the console's Math.round on negative halves.
func TestNPSRoundMatchesJavaScript(t *testing.T) {
	cases := map[float64]int{12.5: 13, -12.5: -12, 0.49: 0, -0.5: 0, 99.5: 100, -33.4: -33}
	for in, want := range cases {
		if got := npsRound(in); got != want {
			t.Errorf("npsRound(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestBuildNPSAggregationEmpty(t *testing.T) {
	agg := buildNPSAggregation(nil, nil)
	if agg.Total != 0 || agg.NPSScore != 0 || agg.ScoreMax != npsScoreMax {
		t.Fatalf("empty aggregation = %+v", agg)
	}
	// Empty slices, not null, so the client can iterate unconditionally.
	b, _ := json.Marshal(agg)
	for _, key := range []string{`"trend":[]`, `"per_hive":[]`, `"recent":[]`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("empty aggregation JSON missing %s: %s", key, b)
		}
	}
}

// TestBuildNPSAggregationMath checks every derived number against values
// worked out by hand.
func TestBuildNPSAggregationMath(t *testing.T) {
	records := []npsRecord{
		{HiveID: "a", Score: 4, Timestamp: "2026-08-03T10:00:00Z"},
		{HiveID: "a", Score: 1, Timestamp: "2026-08-20T10:00:00Z", Feedback: "slow"},
		{HiveID: "b", Score: 2, Timestamp: "2026-09-01T10:00:00Z"},
		{HiveID: "b", Score: 3, Timestamp: "2026-09-02T10:00:00Z"},
		{HiveID: "a", Score: 4, Timestamp: "2026-09-03T10:00:00Z"},
		{HiveID: "c", Score: 4, Timestamp: "not-a-time"},
	}
	agg := buildNPSAggregation(records, map[string]string{"a": "Alpha"})

	// 3 promoters (4,4,4), 2 passives (2,3), 1 detractor (1) of 6.
	if agg.Total != 6 || agg.Promoters != 3 || agg.Passives != 2 || agg.Detractors != 1 {
		t.Fatalf("counts = %+v", agg.npsTally)
	}
	// NPS = (3-1)/6*100 = 33.3 -> 33.
	if agg.NPSScore != 33 {
		t.Errorf("NPSScore = %d, want 33", agg.NPSScore)
	}
	// 50%, 33.3%, 16.7%.
	if agg.PromoterPct != 50 || agg.PassivePct != 33 || agg.DetractorPct != 17 {
		t.Errorf("pcts = %d/%d/%d, want 50/33/17", agg.PromoterPct, agg.PassivePct, agg.DetractorPct)
	}
	// (4+1+2+3+4+4)/6 = 3.0.
	if agg.AverageScore != 3.0 {
		t.Errorf("AverageScore = %v, want 3", agg.AverageScore)
	}

	// Trend: the unparseable timestamp is left out of the trend only.
	if len(agg.Trend) != 2 || agg.Trend[0].Month != "2026-08" || agg.Trend[1].Month != "2026-09" {
		t.Fatalf("trend = %+v", agg.Trend)
	}
	// Aug: 4 and 1 -> NPS 0, avg 2.5. Sep: 2,3,4 -> NPS 33, avg 3.
	if aug := agg.Trend[0]; aug.Total != 2 || aug.NPSScore != 0 || aug.AverageScore != 2.5 {
		t.Errorf("Aug = %+v", aug)
	}
	if sep := agg.Trend[1]; sep.Total != 3 || sep.NPSScore != 33 || sep.AverageScore != 3 {
		t.Errorf("Sep = %+v", sep)
	}

	// Per hive, busiest first, names attached where known.
	if len(agg.PerHive) != 3 || agg.PerHive[0].HiveID != "a" || agg.PerHive[0].Total != 3 || agg.PerHive[0].HiveName != "Alpha" {
		t.Fatalf("per_hive = %+v", agg.PerHive)
	}
	// Hive a: 4,1,4 -> (2-1)/3 = 33.
	if agg.PerHive[0].NPSScore != 33 || agg.PerHive[0].LastAt != "2026-09-03T10:00:00Z" {
		t.Errorf("hive a = %+v", agg.PerHive[0])
	}

	// Recent: newest first, category derived from the raw score.
	if len(agg.Recent) != 6 || agg.Recent[0].HiveID != "c" || agg.Recent[4].Feedback != "slow" || agg.Recent[4].Category != npsCategoryDetractor {
		t.Fatalf("recent = %+v", agg.Recent)
	}
}

func TestBuildNPSAggregationRecentIsCapped(t *testing.T) {
	var records []npsRecord
	for i := 0; i < npsRecentCount+5; i++ {
		records = append(records, npsRecord{HiveID: "a", Score: 3, Timestamp: fmt.Sprintf("2026-09-01T10:%02d:00Z", i)})
	}
	agg := buildNPSAggregation(records, nil)
	if len(agg.Recent) != npsRecentCount {
		t.Fatalf("recent len = %d, want %d", len(agg.Recent), npsRecentCount)
	}
	if agg.Recent[0].Timestamp != records[len(records)-1].Timestamp {
		t.Errorf("recent[0] = %q, want the newest", agg.Recent[0].Timestamp)
	}
}

// TestNPSTrimRollingCaps drops the oldest of the growing hive first, then the
// oldest overall, and never touches another hive for the per-hive cap.
func TestNPSTrimRollingCaps(t *testing.T) {
	var recs []npsRecord
	recs = append(recs, npsRecord{HiveID: "other", Feedback: "keep"})
	for i := 0; i < npsMaxResponsesPerHive+1; i++ {
		recs = append(recs, npsRecord{HiveID: "a", Feedback: fmt.Sprint(i)})
	}
	got := npsTrim(recs, "a")
	if len(got) != npsMaxResponsesPerHive+1 {
		t.Fatalf("len = %d, want %d", len(got), npsMaxResponsesPerHive+1)
	}
	if got[0].HiveID != "other" || got[1].Feedback != "1" {
		t.Errorf("per-hive trim dropped the wrong record: first=%+v second=%+v", got[0], got[1])
	}

	recs = recs[:0]
	for i := 0; i < npsMaxResponsesTotal+3; i++ {
		recs = append(recs, npsRecord{HiveID: fmt.Sprintf("h%d", i), Feedback: fmt.Sprint(i)})
	}
	got = npsTrim(recs, "h0")
	if len(got) != npsMaxResponsesTotal || got[0].Feedback != "3" {
		t.Fatalf("global trim: len=%d first=%+v", len(got), got[0])
	}
}

func TestNPSSanitizeFeedback(t *testing.T) {
	in := "  line one\r\nline\x1b[31m two\x00  "
	if got := npsSanitizeFeedback(in); got != "line one\nline[31m two" {
		t.Errorf("npsSanitizeFeedback = %q", got)
	}
	long := strings.Repeat("é", npsMaxFeedbackRunes+50)
	if got := npsSanitizeFeedback(long); len([]rune(got)) != npsMaxFeedbackRunes {
		t.Errorf("feedback not capped: %d runes", len([]rune(got)))
	}
}

// --- ingest -----------------------------------------------------------------

func TestNPSIngestAcceptsOwnHive(t *testing.T) {
	path := useTempNPSStore(t)
	s := npsTestHub("h1")
	rec := npsIngest(s, `{"hive_id":"h1","score":4,"feedback":"love it","dashboard_version":"abc123","user":"alice"}`, s.heartbeatKeyFor("h1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"category":"promoter"`) {
		t.Errorf("response missing derived category: %s", rec.Body.String())
	}
	got := npsStored(t)
	if len(got) != 1 || got[0].HiveID != "h1" || got[0].Score != 4 || got[0].Feedback != "love it" || got[0].DashboardVersion != "abc123" {
		t.Fatalf("stored = %+v", got)
	}
	// PRIVACY: an identity a spoke smuggles in has nowhere to land.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(data), "alice") {
		t.Errorf("store persisted a user identity: %s", data)
	}
	// Durable: a fresh load from disk sees the same record.
	hubNPS.mu.Lock()
	hubNPS.loaded = false
	hubNPS.mu.Unlock()
	if again := npsStored(t); len(again) != 1 || again[0].Feedback != "love it" {
		t.Fatalf("reloaded = %+v", again)
	}
}

// TestNPSIngestRejectsSpoofing is the console #13664/#13758 regression: only
// the per-hive bearer for the hive named in the body authenticates.
func TestNPSIngestRejectsSpoofing(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1", "h2")
	body := `{"hive_id":"h1","score":1}`
	for name, bearer := range map[string]string{
		"no bearer":         "",
		"garbage bearer":    "not-a-key",
		"other hive bearer": s.heartbeatKeyFor("h2"),
		"fleet-wide bearer": s.heartbeatKey(),
	} {
		if rec := npsIngest(s, body, bearer); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code = %d, want 401", name, rec.Code)
		}
	}
	if got := npsStored(t); len(got) != 0 {
		t.Fatalf("a spoofed submission was stored: %+v", got)
	}
	// Positive control: the right bearer is accepted.
	if rec := npsIngest(s, body, s.heartbeatKeyFor("h1")); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: code = %d; body=%s", rec.Code, rec.Body.String())
	}
}

func TestNPSIngestRejectsUnregisteredHive(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1")
	rec := npsIngest(s, `{"hive_id":"ghost","score":4}`, s.heartbeatKeyFor("ghost"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if got := npsStored(t); len(got) != 0 {
		t.Fatalf("unregistered hive stored: %+v", got)
	}
}

func TestNPSIngestFailsClosedWithoutHubSecret(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1")
	bearer := s.heartbeatKeyFor("h1")
	s.hubSecret = ""
	if rec := npsIngest(s, `{"hive_id":"h1","score":4}`, bearer); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestNPSIngestValidatesPayload(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1")
	bearer := s.heartbeatKeyFor("h1")
	for _, body := range []string{
		`{"hive_id":"h1","score":0}`,
		`{"hive_id":"h1","score":5}`,
		`{"hive_id":"h1","score":-1}`,
		`{"hive_id":"h1","score":"4"}`,
		`{"hive_id":"h1","score":3.5}`,
		`{"hive_id":"h1"}`,
		`{"hive_id":"h 1","score":4}`,
		`{"hive_id":"","score":4}`,
		`not json`,
	} {
		if rec := npsIngest(s, body, bearer); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400", body, rec.Code)
		}
	}
	if got := npsStored(t); len(got) != 0 {
		t.Fatalf("invalid payload stored: %+v", got)
	}
}

// chunkedReader hides its length so httptest cannot set Content-Length, the
// shape of a chunked request.
type chunkedReader struct{ r io.Reader }

func (c chunkedReader) Read(p []byte) (int, error) { return c.r.Read(p) }

// TestNPSIngestSizeCapIgnoresContentLength is the console #16666 regression:
// the cap is enforced on bytes read, for a chunked body and for a lying
// Content-Length alike.
func TestNPSIngestSizeCapIgnoresContentLength(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1")
	bearer := s.heartbeatKeyFor("h1")
	big := `{"hive_id":"h1","score":4,"feedback":"` + strings.Repeat("x", npsIngestMaxBodyBytes) + `"}`

	chunked := npsIngestReq(chunkedReader{strings.NewReader(big)}, bearer)
	if chunked.ContentLength != -1 {
		t.Fatalf("test setup: ContentLength = %d, want -1 (unknown)", chunked.ContentLength)
	}
	rec := httptest.NewRecorder()
	s.handleNPSIngest(rec, chunked)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversize: code = %d, want 413", rec.Code)
	}

	lying := npsIngestReq(strings.NewReader(big), bearer)
	lying.ContentLength = 10
	rec = httptest.NewRecorder()
	s.handleNPSIngest(rec, lying)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("lying Content-Length: code = %d, want 413", rec.Code)
	}
	if got := npsStored(t); len(got) != 0 {
		t.Fatalf("oversize body stored: %+v", got)
	}

	// Positive control: just under the cap is fine (feedback then truncated).
	ok := `{"hive_id":"h1","score":4,"feedback":"` + strings.Repeat("y", npsMaxFeedbackRunes+10) + `"}`
	if rec := npsIngest(s, ok, bearer); rec.Code != http.StatusCreated {
		t.Fatalf("in-cap body: code = %d; body=%s", rec.Code, rec.Body.String())
	}
	if got := npsStored(t); len([]rune(got[0].Feedback)) != npsMaxFeedbackRunes {
		t.Errorf("feedback not truncated to %d runes: %d", npsMaxFeedbackRunes, len([]rune(got[0].Feedback)))
	}
}

func TestNPSIngestRateLimitsPerHive(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1", "h2")
	for i := 0; i < npsHubMaxPerHivePerWindow; i++ {
		if rec := npsIngest(s, `{"hive_id":"h1","score":3}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusCreated {
			t.Fatalf("submission %d: code = %d", i, rec.Code)
		}
	}
	if rec := npsIngest(s, `{"hive_id":"h1","score":3}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit: code = %d, want 429", rec.Code)
	}
	// Another hive has its own quota.
	if rec := npsIngest(s, `{"hive_id":"h2","score":3}`, s.heartbeatKeyFor("h2")); rec.Code != http.StatusCreated {
		t.Fatalf("other hive: code = %d, want 201", rec.Code)
	}
	if got := npsStored(t); len(got) != npsHubMaxPerHivePerWindow+1 {
		t.Fatalf("stored %d, want %d", len(got), npsHubMaxPerHivePerWindow+1)
	}
}

// TestNPSRateWindowSlides proves old responses stop counting once they leave
// the window, so the limit is a rate and not a lifetime cap.
func TestNPSRateWindowSlides(t *testing.T) {
	old := time.Now().Add(-npsHubRateWindow - time.Hour).UTC().Format(time.RFC3339)
	var recs []npsRecord
	for i := 0; i < npsHubMaxPerHivePerWindow; i++ {
		recs = append(recs, npsRecord{HiveID: "h1", Timestamp: old})
	}
	recs = append(recs, npsRecord{HiveID: "h1", Timestamp: "garbage"})
	if n := npsCountSince(recs, "h1", time.Now().Add(-npsHubRateWindow)); n != 1 {
		t.Fatalf("npsCountSince = %d, want 1 (only the unparseable one, fail closed)", n)
	}
}

func TestNPSStoreRefusesToOverwriteCorruptFile(t *testing.T) {
	path := useTempNPSStore(t)
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := npsTestHub("h1")
	if rec := npsIngest(s, `{"hive_id":"h1","score":4}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{corrupt" {
		t.Fatalf("corrupt store was overwritten: %q", data)
	}
	rec := httptest.NewRecorder()
	s.handleAdminNPS(rec, httptest.NewRequest(http.MethodGet, npsAdminPath, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("admin read of corrupt store: code = %d, want 500", rec.Code)
	}
}

func TestNPSStoreRollsBackOnSaveFailure(t *testing.T) {
	path := useTempNPSStore(t)
	// A directory where the temp file must go makes the write fail, for root
	// too, while the (absent) store itself still loads cleanly.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hubNPS.add(npsRecord{HiveID: "h1", Score: 4, Timestamp: time.Now().UTC().Format(time.RFC3339)}, time.Now()); err == nil {
		t.Fatal("add succeeded with an unwritable store")
	}
	if got := npsStored(t); len(got) != 0 {
		t.Fatalf("in-memory store kept a response the disk does not have: %+v", got)
	}
}

// --- admin read path ----------------------------------------------------------

func TestHandleAdminNPSServesAggregate(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1")
	if rec := npsIngest(s, `{"hive_id":"h1","score":1,"feedback":"broken"}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	s.handleAdminNPS(rec, httptest.NewRequest(http.MethodGet, npsAdminPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var agg npsAggregation
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if agg.Total != 1 || agg.NPSScore != -100 || agg.Recent[0].Feedback != "broken" || agg.Recent[0].HiveName != "Hive h1" {
		t.Fatalf("aggregate = %+v", agg)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("admin NPS response is cacheable")
	}
}

// TestHandleAdminNPSAdminGated asserts the route is registered behind
// requireAdmin (source) and that the gate refuses a non-admin with no data
// while admitting an admin (behavior, with a positive control).
func TestHandleAdminNPSAdminGated(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "")
	useTempNPSStore(t)

	src, err := os.ReadFile("saas.go")
	if err != nil {
		t.Fatalf("read saas.go: %v", err)
	}
	if !strings.Contains(string(src), `s.mux.HandleFunc("GET "+npsAdminPath, s.requireAdmin(s.handleAdminNPS))`) {
		t.Error("GET /api/admin/nps is not registered behind requireAdmin")
	}
	serverSrc, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	if strings.Count(string(src)+string(serverSrc), "s.handleAdminNPS") != 1 {
		t.Error("handleAdminNPS is registered more than once; every read path must be admin-gated")
	}

	s := newHandlerHub()
	s.registry.Hives = []RegistryEntry{{ID: "h1", Name: "Hive h1"}}
	if err := hubNPS.add(npsRecord{HiveID: "h1", Score: 1, Feedback: "secret free text", Timestamp: time.Now().UTC().Format(time.RFC3339)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	mkUser(t, hubAdminUsername)
	mkUser(t, "notanadmin")
	gated := s.requireAdmin(s.handleAdminNPS)

	rec := httptest.NewRecorder()
	gated(rec, reqWithUser(http.MethodGet, npsAdminPath, "", "notanadmin"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin: code = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret free text") || strings.Contains(rec.Body.String(), "nps_score") {
		t.Errorf("non-admin 403 body leaked NPS data: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	gated(rec, httptest.NewRequest(http.MethodGet, npsAdminPath, nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("anonymous request got 200")
	}

	rec = httptest.NewRecorder()
	gated(rec, reqWithUser(http.MethodGet, npsAdminPath, "", hubAdminUsername))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secret free text") {
		t.Fatalf("admin: code = %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- admin UI -------------------------------------------------------------------

func TestNPSAdminCardWired(t *testing.T) {
	html := dashScript(t)
	for _, snippet := range []string{
		`id="nps-container"`,
		"function renderAdminNPS(data)",
		"async function loadAdminNPS()",
		"fetch('/api/admin/nps')",
		"loadAdminNPS();",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("dashboardHTML is missing %q - the NPS card is not wired", snippet)
		}
	}
	// The container must live inside the admin-gated section, i.e. after the
	// admin-section opens and before the banner section that follows it.
	adminAt := strings.Index(html, `id="admin-section"`)
	npsAt := strings.Index(html, `id="nps-container"`)
	bannerAt := strings.Index(html, `id="hub-banner-section"`)
	if adminAt < 0 || npsAt < adminAt || npsAt > bannerAt {
		t.Errorf("nps-container is not inside the admin section (admin=%d nps=%d banner=%d)", adminAt, npsAt, bannerAt)
	}
}

// npsHubJSFunc extracts a top-level `function name(` from the hub dashboard.
func npsHubJSFunc(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "function "+name+"(")
	if start < 0 {
		t.Fatalf("dashboardHTML does not define %s()", name)
	}
	depth := 0
	for i := strings.Index(html[start:], "{") + start; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces extracting %s()", name)
	return ""
}

// TestNPSAdminCardNeverUsesInnerHTML is the console #17030 regression at the
// source level: the NPS render path has no markup sink at all.
func TestNPSAdminCardNeverUsesInnerHTML(t *testing.T) {
	html := dashScript(t)
	for _, fn := range []string{"npsEl", "npsTallyText", "renderAdminNPS", "loadAdminNPS"} {
		body := npsHubJSFunc(t, html, fn)
		for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
			if strings.Contains(body, sink) {
				t.Errorf("%s() uses %s - NPS free text must be rendered as text only", fn, sink)
			}
		}
	}
}

// TestNPSAdminCardRendersFeedbackAsText executes the real render function
// against a minimal DOM double and proves hostile free text is kept as
// literal text and never becomes an element.
func TestNPSAdminCardRendersFeedbackAsText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH - the NPS text-rendering rule was NOT executed by this run; the source-level guard above still ran")
	}
	html := dashScript(t)
	script := "var NPS_TREND_BAR_MAX_PCT = 100;\n" +
		npsHubJSFunc(t, html, "npsEl") + "\n" +
		npsHubJSFunc(t, html, "npsTallyText") + "\n" +
		npsHubJSFunc(t, html, "renderAdminNPS") + "\n" + npsAdminDOMAssertions
	path := filepath.Join(t.TempDir(), "nps_admin.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("NPS admin rendering check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

const npsAdminDOMAssertions = `
const created = [];
function makeEl(tag) {
  const el = { tagName: String(tag).toUpperCase(), children: [], className: '', style: {}, _text: '' };
  Object.defineProperty(el, 'textContent', {
    get() { return this._text + this.children.map(c => c.textContent).join(''); },
    set(v) { this._text = String(v); this.children = []; },
  });
  Object.defineProperty(el, 'innerHTML', {
    set() { throw new Error('innerHTML assigned'); },
    get() { throw new Error('innerHTML read'); },
  });
  Object.defineProperty(el, 'firstChild', { get() { return this.children[0] || null; } });
  el.appendChild = c => { el.children.push(c); return c; };
  el.removeChild = c => { el.children = el.children.filter(x => x !== c); return c; };
  created.push(el);
  return el;
}
const container = makeEl('div');
globalThis.document = {
  createElement: makeEl,
  getElementById: id => (id === 'nps-container' ? container : null),
};
const hostile = '<img src=x onerror=alert(1)><script>alert(2)</script>';
renderAdminNPS({
  total: 1, nps_score: -100, promoters: 0, passives: 0, detractors: 1,
  promoter_pct: 0, passive_pct: 0, detractor_pct: 100, average_score: 1, score_max: 4,
  trend: [{ month: '2026-09', total: 1, nps_score: -100, average_score: 1 }],
  per_hive: [{ hive_id: 'h1', hive_name: '<b>evil</b>', total: 1, nps_score: -100, average_score: 1 }],
  recent: [{ hive_id: 'h1', score: 1, category: 'detractor', feedback: hostile, timestamp: '2026-09-29T00:00:00Z' }],
});
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
const text = container.textContent;
check('hostile feedback is present as literal text', text.includes(hostile));
check('hostile hive name is present as literal text', text.includes('<b>evil</b>'));
check('only div elements were created', created.every(e => e.tagName === 'DIV'));
check('the summary shows the NPS score', text.includes('NPS -100'));
check('the trend bar is sized', created.some(e => e.style.width === '100%'));
// Re-render replaces rather than appends.
renderAdminNPS({ total: 0 });
check('empty state replaces previous content', !container.textContent.includes(hostile) && container.textContent.includes('No NPS responses yet.'));
if (fails) { console.log(fails + ' NPS admin rendering assertion(s) failed'); process.exit(1); }
`
