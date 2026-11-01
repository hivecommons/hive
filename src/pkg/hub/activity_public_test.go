package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hubactivity"
)

// Public-safe Fleet activity view (#15966, part of RFC #15861).

func setActivityHiveVisibility(t *testing.T, s *HubServer, hiveID string, public bool, host string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].IsPublic = public
			s.registry.Hives[i].GitHubHost = host
			return
		}
	}
	t.Fatalf("hive %s is not in the registry", hiveID)
}

// getPublicActivity requests the public-safe endpoint, optionally with the
// credentials a signed-in admin would send.
func getPublicActivity(t *testing.T, s *HubServer, user, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	return getPublicActivityQuery(t, s, "", user, ifNoneMatch)
}

// getPublicActivityQuery is getPublicActivity with a raw query string.
func getPublicActivityQuery(t *testing.T, s *HubServer, rawQuery, user, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	target := activityPublicPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if user != "" {
		req.AddCookie(testAuthCookie(user))
		req.Header.Set("Authorization", "Bearer "+testHubSecret)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	s.handleHubActivityPublic(rec, req)
	return rec
}

// mixedVisibilityDigest carries a public and a private item, plus one public
// and one private event, as a spoke that opted in to share private items
// sends them.
func mixedVisibilityDigest() *hubactivity.Digest {
	d := hubactivity.Build([]hubactivity.Item{
		{Repo: "o/pub", Number: 1, Kind: "pr", Title: "public change", Stage: "ci_review", StageNumber: 70},
		{Repo: "o/secret", Number: 2, Kind: "pr", Title: "secret change", Stage: "ci_review", StageNumber: 70, Private: true},
	}, hubactivity.BuildOptions{SharePrivate: true, Now: activityT0})
	pub := activityEv(1, hubactivity.EventOpened, "o/pub", 1)
	priv := activityEv(2, hubactivity.EventOpened, "o/secret", 2)
	priv.Title = "secret change"
	priv.Private = true
	d.Events = []hubactivity.Event{pub, priv}
	d.EventsDropped = 3
	return &d
}

// Acceptance (#15966): the public-safe view omits private rows by
// construction — private-repository items and events, hives not listed as
// public, hives on a non-public GitHub, and every count that may include
// private items — whoever asks and whatever credentials they present.
func TestActivityPublic_OmitsPrivateRowsByConstruction(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	mkUser(t, "root")
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-pub", mixedVisibilityDigest())
	postActivityHeartbeat(t, s, "hive-priv", activityDigest("o/privhive", 3))
	postActivityHeartbeat(t, s, "hive-ghe", activityDigest("o/ghe", 4))
	postActivityHeartbeat(t, s, "hive-unset", activityDigest("o/unset", 5))
	setActivityHiveVisibility(t, s, "hive-pub", true, "")
	setActivityHiveVisibility(t, s, "hive-priv", false, "")
	setActivityHiveVisibility(t, s, "hive-ghe", true, "github.example.com")

	// The signed-in admin view still sees everything (unchanged behaviour).
	full := decodeActivity(t, getActivity(t, s, "root", "", ""))
	if got := activityHiveIDs(full); len(got) != 4 {
		t.Fatalf("admin /api/hub/activity rows from %v, want all four hives", got)
	}

	anon := getPublicActivity(t, s, "", "")
	if anon.Code != http.StatusOK {
		t.Fatalf("anonymous public view status %d body=%s", anon.Code, anon.Body.String())
	}
	authed := getPublicActivity(t, s, "root", "")
	if authed.Body.String() != anon.Body.String() {
		t.Fatalf("credentials must not change the public-safe view:\nanonymous: %s\nadmin:     %s", anon.Body.String(), authed.Body.String())
	}
	body := anon.Body.String()
	for _, leak := range []string{"o/secret", "secret change", "hive-priv", "o/privhive", "hive-ghe", "o/ghe", "hive-unset", "o/unset", `"private"`, `"totals"`, `"truncated"`, `"events_dropped"`, "root"} {
		if strings.Contains(body, leak) {
			t.Errorf("public-safe view leaks %q: %s", leak, body)
		}
	}

	v := decodeActivity(t, anon)
	if len(v.Hives) != 1 || v.Hives[0].HiveID != "hive-pub" {
		t.Fatalf("public hives = %+v, want only hive-pub", v.Hives)
	}
	if len(v.Items) != 1 || v.Items[0].Repo != "o/pub" || v.Items[0].Number != 1 {
		t.Fatalf("public items = %+v, want only o/pub#1", v.Items)
	}
	if len(v.Events) != 1 || v.Events[0].Repo != "o/pub" {
		t.Fatalf("public events = %+v, want only the o/pub event", v.Events)
	}

	h := anon.Header()
	if h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-cache" || h.Get("ETag") == "" {
		t.Fatalf("headers = %v, want JSON + no-cache + ETag", h)
	}
	if again := getPublicActivity(t, s, "", h.Get("ETag")); again.Code != http.StatusNotModified {
		t.Fatalf("revalidation status %d, want 304", again.Code)
	}
}

// A spoke that sends no private items still has every public row rendered.
func TestActivityPublic_PublicRowsRender(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))
	setActivityHiveVisibility(t, s, "hive-a", true, "github.com")
	v := decodeActivity(t, getPublicActivity(t, s, "", ""))
	if len(v.Items) != 2 || len(v.Hives) != 1 || v.Hives[0].Status != activityStatusOK {
		t.Fatalf("public view = %+v, want hive-a ok with both items", v)
	}
	if v.Hives[0].Totals != nil {
		t.Fatalf("public view must omit totals, got %+v", v.Hives[0].Totals)
	}
	if rec := getPublicActivity(t, s, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, activityPublicPath+"?repo="+strings.Repeat("x", maxActivityFilterRunes+1), nil)
	rec := httptest.NewRecorder()
	s.handleHubActivityPublic(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized filter status %d, want 400", rec.Code)
	}
}

// The public page and endpoint are anonymous through the real mux, while
// the signed-in page and endpoint keep requiring a session.
func TestActivityPublic_RoutesAnonymouslyThroughTheHubMux(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	srv := newHubServerForTest(t, withHubIdentity("test", "v5"))
	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	page := serve(activityPublicPagePath + "?kiosk=1")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="activity-board"`) {
		t.Fatalf("GET %s anonymous = %d, want the page shell", activityPublicPagePath, page.Code)
	}
	if page.Header().Get("Content-Security-Policy") != activityPageCSP {
		t.Errorf("public page CSP = %q, want the Fleet activity CSP", page.Header().Get("Content-Security-Policy"))
	}
	api := serve(activityPublicPath)
	if api.Code != http.StatusOK {
		t.Fatalf("GET %s anonymous = %d, want 200", activityPublicPath, api.Code)
	}
	var v ActivityView
	if err := json.Unmarshal(api.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if rec := serve(activityPagePath); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET /activity anonymous = %d, want redirect to login", rec.Code)
	}
	if rec := serve(activityPath); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET %s anonymous = %d, want 401", activityPath, rec.Code)
	}
	if !hubNoIndexPath(activityPublicPagePath) || !hubNoIndexPath(activityPublicPath) {
		t.Errorf("the public-safe view is a stream source, not a search result: it must be noindex")
	}
}

// On the public path the page polls only the public endpoint, without
// credentials; on /activity it keeps polling the signed-in endpoint.
func TestActivityPublic_PagePollsThePublicEndpointWithoutCredentials(t *testing.T) {
	js := readActivityScript(t)
	for _, want := range []string{"'" + activityPublicPagePath + "'", "'" + activityPublicPath + "'", "'omit'"} {
		if !strings.Contains(js, want) {
			t.Fatalf("activity.js missing %q", want)
		}
	}
	src := strings.Join(append([]string{js},
		"result = {",
		"  pub: activityApiFor('/activity/public'),",
		"  pubSlash: activityApiFor('/activity/public/'),",
		"  full: activityApiFor('/activity'),",
		"  other: activityApiFor('/activity/publicx'),",
		"  pubCreds: activityFetchOptions({}, true).credentials,",
		"  fullCreds: activityFetchOptions({}, false).credentials,",
		"};",
	), "\n")
	var got struct{ Pub, PubSlash, Full, Other, PubCreds, FullCreds string }
	runEvidenceJS(t, src, &got)
	if got.Pub != activityPublicPath || got.PubSlash != activityPublicPath {
		t.Errorf("public page polls %q / %q, want %s", got.Pub, got.PubSlash, activityPublicPath)
	}
	if got.Full != activityPath || got.Other != activityPath {
		t.Errorf("signed-in page polls %q / %q, want %s", got.Full, got.Other, activityPath)
	}
	if got.PubCreds != "omit" || got.FullCreds != "same-origin" {
		t.Errorf("credentials public=%q signed-in=%q, want omit / same-origin", got.PubCreds, got.FullCreds)
	}
}

// The anonymous default view is built once per TTL window (#16111), a
// matching ETag is a 304 served from the cache, and expiry rebuilds.
func TestActivityPublic_CachesBodyForTTL(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	setActivityHiveVisibility(t, s, "hive-a", true, "github.com")

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow := publicActivityNow
	publicActivityNow = func() time.Time { return clock }
	defer func() { publicActivityNow = oldNow }()

	first := getPublicActivity(t, s, "", "")
	etag := first.Header().Get("ETag")
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))

	second := getPublicActivity(t, s, "", "")
	if second.Body.String() != first.Body.String() {
		t.Fatalf("body changed inside the TTL window")
	}
	if rec := getPublicActivity(t, s, "", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("conditional status %d, want 304", rec.Code)
	}

	clock = clock.Add(publicActivityCacheTTL + time.Second)
	third := getPublicActivity(t, s, "", "")
	if third.Body.String() == first.Body.String() {
		t.Fatalf("body not rebuilt after TTL expiry")
	}
}

// Query strings that parse to the empty filter (#16111 review) share the
// default cache entry, so `?x=1` or `?hive=` cannot force a rebuild.
func TestActivityPublic_EmptyFilterQueriesHitCache(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	setActivityHiveVisibility(t, s, "hive-a", true, "github.com")

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow := publicActivityNow
	publicActivityNow = func() time.Time { return clock }
	defer func() { publicActivityNow = oldNow }()

	first := getPublicActivity(t, s, "", "")
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))
	for _, q := range []string{"x=1", "hive=", "hive=%20&repo=", "q=&x=y"} {
		rec := getPublicActivityQuery(t, s, q, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("?%s status %d", q, rec.Code)
		}
		if rec.Body.String() != first.Body.String() {
			t.Errorf("?%s bypassed the default cache entry", q)
		}
		if rec.Header().Get("ETag") != first.Header().Get("ETag") {
			t.Errorf("?%s ETag differs from the cached body's", q)
		}
	}
}

// Filtered views are cached per parsed filter for the same TTL, bounded in
// number, and never mixed up with the default view.
func TestActivityPublic_FilteredViewsCachedAndBounded(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	postActivityHeartbeat(t, s, "hive-b", activityDigest("o/b", 7))
	setActivityHiveVisibility(t, s, "hive-a", true, "github.com")
	setActivityHiveVisibility(t, s, "hive-b", true, "github.com")

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow := publicActivityNow
	publicActivityNow = func() time.Time { return clock }
	defer func() { publicActivityNow = oldNow }()
	oldCap := publicActivityFilteredCap
	publicActivityFilteredCap = 2
	defer func() { publicActivityFilteredCap = oldCap }()

	def := getPublicActivity(t, s, "", "").Body.String()
	filtered := getPublicActivityQuery(t, s, "hive=hive-a", "", "").Body.String()
	if filtered == def || !strings.Contains(filtered, "o/a") || strings.Contains(filtered, "o/b") {
		t.Fatalf("filtered body not restricted to hive-a: %s", filtered)
	}

	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))
	if again := getPublicActivityQuery(t, s, "hive=hive-a", "", "").Body.String(); again != filtered {
		t.Fatalf("filtered body rebuilt inside the TTL window")
	}
	// The same filter spelled differently is the same key.
	if again := getPublicActivityQuery(t, s, "hive=%20hive-a%20&x=1", "", "").Body.String(); again != filtered {
		t.Fatalf("equivalent filtered query bypassed its cache entry")
	}
	if got := getPublicActivity(t, s, "", "").Body.String(); got != def {
		t.Fatalf("default body disturbed by filtered requests")
	}

	// Distinct filters beyond the cap evict rather than grow.
	getPublicActivityQuery(t, s, "hive=hive-b", "", "")
	getPublicActivityQuery(t, s, "repo=o/a", "", "")
	getPublicActivityQuery(t, s, "q=zzz", "", "")
	s.publicActivity.mu.Lock()
	n := len(s.publicActivity.filtered)
	s.publicActivity.mu.Unlock()
	if n > publicActivityFilteredCap {
		t.Fatalf("filtered cache holds %d entries, cap %d", n, publicActivityFilteredCap)
	}

	clock = clock.Add(publicActivityCacheTTL + time.Second)
	if third := getPublicActivityQuery(t, s, "hive=hive-a", "", "").Body.String(); third == filtered {
		t.Fatalf("filtered body not rebuilt after TTL expiry")
	}
}

// A hive made private stops appearing anonymously as soon as the toggle
// invalidates the cache, not at TTL expiry.
func TestActivityPublic_InvalidateDropsHiveAtOnce(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	setActivityHiveVisibility(t, s, "hive-a", true, "github.com")

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldNow := publicActivityNow
	publicActivityNow = func() time.Time { return clock }
	defer func() { publicActivityNow = oldNow }()

	if body := getPublicActivity(t, s, "", "").Body.String(); !strings.Contains(body, "hive-a") {
		t.Fatalf("public hive missing before the flip: %s", body)
	}
	getPublicActivityQuery(t, s, "hive=hive-a", "", "")
	setActivityHiveVisibility(t, s, "hive-a", false, "github.com")
	if body := getPublicActivity(t, s, "", "").Body.String(); !strings.Contains(body, "hive-a") {
		t.Fatalf("cache unexpectedly bypassed before invalidate")
	}
	s.publicActivity.invalidate()
	for _, q := range []string{"", "hive=hive-a"} {
		if body := getPublicActivityQuery(t, s, q, "", "").Body.String(); strings.Contains(body, "hive-a") {
			t.Fatalf("?%s still shows the private hive after invalidate: %s", q, body)
		}
	}
}
