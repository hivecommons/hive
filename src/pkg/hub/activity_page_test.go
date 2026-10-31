package hub

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/webassets/trackerrow"
)

// Fleet activity page (#16063, part of #15965 / RFC #15861).

func getActivityPage(t *testing.T, s *HubServer, user, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, activityPagePath, nil)
	if user != "" {
		req.AddCookie(testAuthCookie(user))
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	s.handleActivityPage(rec, req)
	return rec
}

func readActivityScript(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(staticFS, "static/activity.js")
	if err != nil {
		t.Fatalf("reading embedded static/activity.js: %v", err)
	}
	return string(b)
}

func cspDirectives(csp string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(csp, ";") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		out[fields[0]] = strings.Join(fields[1:], " ")
	}
	return out
}

func TestActivityPage_AnonymousBrowserIsSentToLogin(t *testing.T) {
	s := newActivityHub()
	rec := getActivityPage(t, s, "", "")
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("anonymous /activity status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("anonymous /activity Location = %q, want /login", loc)
	}
	if strings.Contains(rec.Body.String(), "activity-board") {
		t.Fatalf("anonymous visitor must not receive the page shell")
	}

	// A cookie the hub does not accept is a lapsed session, not an API
	// client: the browser goes to /login rather than to a raw JSON 401.
	req := httptest.NewRequest(http.MethodGet, activityPagePath, nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "forged"})
	forged := httptest.NewRecorder()
	s.handleActivityPage(forged, req)
	if forged.Code != http.StatusTemporaryRedirect || forged.Header().Get("Location") != "/login" {
		t.Fatalf("forged session /activity = %d %q, want redirect to /login", forged.Code, forged.Header().Get("Location"))
	}
}

func TestActivityPage_BlockedUserIsRefusedByRequireAuth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "mallory", Blocked: true}); err != nil {
		t.Fatal(err)
	}
	rec := getActivityPage(t, newActivityHub(), "mallory", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("blocked user /activity status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestActivityPage_ServesShellWithCSPAndStaticCacheHeaders(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	s := newActivityHub()

	rec := getActivityPage(t, s, "alice", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", h.Get("X-Content-Type-Options"))
	}
	body := rec.Body.Bytes()

	// CSP, following pkg/dashboard/webstatic/csp_script_src.go: no inline
	// script elements (script-src-elem is exactly what the document needs),
	// inline event handlers refused, and no 'unsafe-inline' anywhere.
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("the Fleet activity page must be served with a Content-Security-Policy")
	}
	if strings.Contains(csp, "'unsafe-inline'") || strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("CSP must not allow unsafe-inline/unsafe-eval: %s", csp)
	}
	d := cspDirectives(csp)
	for directive, want := range map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self'",
		"script-src-elem": webstatic.ScriptSrcElemSources(body),
		"script-src-attr": "'none'",
		"style-src-attr":  "'none'",
		"connect-src":     "'self'",
		"frame-ancestors": "'none'",
		"base-uri":        "'none'",
		"form-action":     "'none'",
	} {
		if d[directive] != want {
			t.Errorf("CSP %s = %q, want %q (full policy: %s)", directive, d[directive], want, csp)
		}
	}
	if n := len(webstatic.ExtractInlineScripts(body)); n != 0 {
		t.Errorf("page carries %d inline <script> elements; ship them as files under 'self'", n)
	}
	if m := regexp.MustCompile(`(?i)<[^>]+\son[a-z]+\s*=`).Find(body); m != nil {
		t.Errorf("page carries an inline event handler (script-src-attr 'none' blocks it): %s", m)
	}
	if m := regexp.MustCompile(`(?i)<[^>]+\sstyle\s*=`).Find(body); m != nil {
		t.Errorf("page carries an inline style attribute (style-src-attr 'none' blocks it): %s", m)
	}

	// Same cache contract as the hub's other embedded pages.
	etag := h.Get("ETag")
	if etag == "" || h.Get("Cache-Control") != "no-cache" {
		t.Fatalf("cache headers = Cache-Control %q ETag %q, want no-cache + ETag", h.Get("Cache-Control"), etag)
	}
	again := getActivityPage(t, s, "alice", etag)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("revalidation status %d (body %d bytes), want 304 with no body", again.Code, again.Body.Len())
	}
	if again.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("the 304 must keep the CSP header")
	}
}

func TestActivityPage_LoadsSharedRowAssetAndPollsTheActivityAPI(t *testing.T) {
	page, err := fs.ReadFile(staticFS, "static/activity.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	last := -1
	for _, name := range append(append([]string{}, trackerrow.Files...), "activity.js") {
		src := activityAssetPrefix + name
		if name == "activity.js" {
			src = "/static/activity.js"
		}
		tag := `<script src="` + src + `" defer></script>`
		idx := strings.Index(html, tag)
		if idx < 0 {
			t.Fatalf("activity.html does not load %s (want %s)", name, tag)
		}
		if idx < last {
			t.Errorf("%s must load after the shared row asset files, in trackerrow.Files order", name)
		}
		last = idx
	}
	for _, want := range []string{`id="activity-board"`, `id="activity-disclosures"`, `<link rel="canonical" href="https://hive.hivecommons.dev/activity">`} {
		if !strings.Contains(html, want) {
			t.Errorf("activity.html missing %q", want)
		}
	}

	js := readActivityScript(t)
	for _, want := range []string{"'" + activityPath + "'", "'If-None-Match'", "ACTIVITY_POLL_MS = 15000", "stageTrackerAnchor(", "stageTrackerSplitTitleTag("} {
		if !strings.Contains(js, want) {
			t.Errorf("activity.js missing %q", want)
		}
	}
	// Visibility-only and text-only: no HTML sinks, no write calls, no
	// action menus.
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "data-action", "method:", "eval(", "new Function"} {
		if strings.Contains(js, banned) {
			t.Errorf("activity.js contains %q; the page renders text only and offers no actions", banned)
		}
	}
}

func TestActivityPage_RoutesThroughTheHubMux(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	srv := newHubServerForTest(t, withHubIdentity("test", "v5"))
	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := serve(activityPagePath); rec.Code != http.StatusTemporaryRedirect {
		t.Errorf("GET /activity anonymous = %d, want redirect to login", rec.Code)
	}
	for _, name := range trackerrow.Files {
		want, err := fs.ReadFile(trackerrow.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		rec := serve(activityAssetPrefix + name)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s%s = %d", activityAssetPrefix, name, rec.Code)
		}
		if rec.Body.String() != string(want) {
			t.Errorf("%s must be served byte-identical to pkg/webassets/trackerrow", name)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("%s Content-Type = %q, want text/javascript", name, ct)
		}
		if rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") != webstatic.ETagFor(want) {
			t.Errorf("%s cache headers = %q / %q, want the static no-cache + content ETag", name, rec.Header().Get("Cache-Control"), rec.Header().Get("ETag"))
		}
	}
	for _, bad := range []string{"nope.js", "trackerrow.go", "trackerrow_test.go"} {
		if rec := serve(activityAssetPrefix + bad); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s%s = %d, want 404", activityAssetPrefix, bad, rec.Code)
		}
	}
	if rec := serve("/static/activity.js"); rec.Code != http.StatusOK {
		t.Errorf("GET /static/activity.js = %d, want 200", rec.Code)
	}
	if !hubNoIndexPath(activityPagePath) || !hubNoIndexPath(activityAssetPrefix+trackerrow.Files[0]) {
		t.Errorf("/activity is a signed-in page and must be noindex")
	}
}

// Acceptance (#16063): two spokes heartbeating to one hub; through the page,
// a user scoped to one hive sees only that hive's rows and an admin sees
// both. The page shell carries no data; every row comes from the endpoint
// the page polls, which scopes per viewer.
func TestActivityPage_TwoSpokesScopedThroughThePage(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))
	postActivityHeartbeat(t, s, "hive-b", activityDigest("o/b", 3))
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hive-a": "viewer"}}); err != nil {
		t.Fatal(err)
	}
	mkUser(t, "root")

	js := readActivityScript(t)
	if !strings.Contains(js, "'"+activityPath+"'") {
		t.Fatalf("the page must poll %s", activityPath)
	}
	for user, wantHives := range map[string][]string{"alice": {"hive-a"}, "root": {"hive-a", "hive-b"}} {
		page := getActivityPage(t, s, user, "")
		if page.Code != http.StatusOK {
			t.Fatalf("%s: page status %d", user, page.Code)
		}
		for _, other := range []string{"hive-a", "hive-b", "o/a", "o/b"} {
			if strings.Contains(page.Body.String(), other) {
				t.Fatalf("%s: the page shell must carry no fleet data, found %q", user, other)
			}
		}
		v := decodeActivity(t, getActivity(t, s, user, "", ""))
		got := activityHiveIDs(v)
		if len(got) != len(wantHives) {
			t.Fatalf("%s sees rows of %v, want %v", user, got, wantHives)
		}
		for _, id := range wantHives {
			if !got[id] {
				t.Fatalf("%s sees rows of %v, want %v", user, got, wantHives)
			}
		}
		if len(v.Hives) != len(wantHives) {
			t.Fatalf("%s sees group headers %+v, want %v", user, v.Hives, wantHives)
		}
	}
}

// The board's pure helpers, run under node with the shared row asset loaded
// first, exactly as the page loads them.
func TestActivityPage_BoardModel(t *testing.T) {
	var sources []string
	for _, name := range trackerrow.Files {
		b, err := fs.ReadFile(trackerrow.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, string(b))
	}
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	view := map[string]any{
		"hives": []map[string]any{
			{"hive_id": "hive-a", "name": "Alpha", "status": "ok", "reporting": true, "received_at": now.Add(-time.Minute), "totals": map[string]any{"items": 4}, "truncated": 2},
			{"hive_id": "hive-b", "status": "stale", "reporting": true, "stale": true, "received_at": now.Add(-10 * time.Minute), "events_dropped": 5},
			{"hive_id": "hive-c", "name": "Gamma", "status": "not_reporting", "last_heartbeat": now.Add(-time.Minute)},
			{"hive_id": "hive-d", "status": "missing"},
		},
		"items": []map[string]any{
			{"hive_id": "hive-a", "repo": "o/a", "n": 7, "kind": "pr", "title": "[scanner] Fix the <b>thing</b>", "stage": "fixing", "stage_n": 70},
			{"hive_id": "hive-b", "repo": "o/b", "n": 9, "kind": "issue", "title": "Plain", "stage": "ready-for-human", "stage_n": 90},
			{"hive_id": "hive-a", "repo": "o/a", "n": 8, "kind": "issue", "title": "Open one", "stage": "open"},
		},
		"events":    []any{},
		"truncated": 12,
	}
	vb, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	src := strings.Join(append(sources, readActivityScript(t),
		"var view = "+string(vb)+";",
		"var now = Date.parse('"+now.Format(time.RFC3339)+"');",
		"var groups = activityGroups(view, now);",
		"result = {",
		"  groups: groups.map(function(g){ return {id: g.header.hiveId, label: g.header.label, count: g.header.count, freshness: g.header.freshness, note: g.header.note, rows: g.rows.map(function(r){ return r.number; })}; }),",
		"  disclosures: activityDisclosures(view, now),",
		"  rows: activityRows(view).map(function(r){ return {hive: r.hiveId, url: r.url, tag: r.tag, title: r.title, column: r.column, hiveURL: r.hiveURL}; }),",
		"  hue: [activityHiveHue('hive-a'), activityHiveHue('hive-a'), activityHiveHue('hive-b')],",
		"  odd: activityHiveURL('a/b c'),",
		"};",
	), "\n")
	var got struct {
		Groups []struct {
			ID, Label, Freshness, Note string
			Count                      int
			Rows                       []int
		}
		Disclosures []string
		Rows        []struct{ Hive, URL, Tag, Title, Column, HiveURL string }
		Hue         []int
		Odd         string
	}
	runEvidenceJS(t, src, &got)

	// One header per visible hive, in the API's order, even with no rows.
	if len(got.Groups) != 4 {
		t.Fatalf("groups = %+v, want one header per hive", got.Groups)
	}
	wantGroups := []struct {
		id, label, freshness, note string
		count                      int
		rows                       []int
	}{
		{"hive-a", "Alpha", "fresh", "", 2, []int{7, 8}},
		{"hive-b", "hive-b", "stale", "stale", 1, []int{9}},
		{"hive-c", "Gamma", "not reporting", "upgrade to report activity", 0, nil},
		{"hive-d", "hive-d", "missing", "not in the hub registry", 0, nil},
	}
	for i, w := range wantGroups {
		g := got.Groups[i]
		if g.ID != w.id || g.Label != w.label || g.Freshness != w.freshness || g.Count != w.count || len(g.Rows) != len(w.rows) {
			t.Errorf("group %d = %+v, want %+v", i, g, w)
			continue
		}
		if !strings.Contains(g.Note, w.note) {
			t.Errorf("group %s note = %q, want it to mention %q", w.id, g.Note, w.note)
		}
		for j := range w.rows {
			if g.Rows[j] != w.rows[j] {
				t.Errorf("group %s rows = %v, want %v", w.id, g.Rows, w.rows)
			}
		}
	}

	// Truncation and stale/missing sources are always spelled out.
	joined := strings.Join(got.Disclosures, "\n")
	for _, want := range []string{"12 more items", "Alpha (hive-a): 2 items", "hive-b: 5 events dropped", "hive-b: stale", "Gamma (hive-c): upgrade to report activity", "hive-d: not in the hub registry"} {
		if !strings.Contains(joined, want) {
			t.Errorf("disclosures missing %q:\n%s", want, joined)
		}
	}

	if len(got.Rows) != 3 {
		t.Fatalf("rows = %+v", got.Rows)
	}
	r := got.Rows[0]
	if r.URL != "https://github.com/o/a/pull/7" || r.Tag != "scanner" || r.Title != "Fix the <b>thing</b>" || r.Column != "CI / Review" {
		t.Errorf("PR row = %+v", r)
	}
	if r.HiveURL != "/api/saas/hives/hive-a/open" {
		t.Errorf("hive chip link = %q, want the hub's hive open route", r.HiveURL)
	}
	if got.Rows[1].URL != "https://github.com/o/b/issues/9" || got.Rows[1].Column != "Merge gate" {
		t.Errorf("issue row = %+v", got.Rows[1])
	}
	if got.Rows[2].Column != "Open" {
		t.Errorf("open row column = %q", got.Rows[2].Column)
	}
	if got.Hue[0] != got.Hue[1] || got.Hue[0] == got.Hue[2] || got.Hue[0] < 0 || got.Hue[0] >= 360 {
		t.Errorf("hive hues %v must be stable per hive and differ between these two", got.Hue)
	}
	if got.Odd != "/api/saas/hives/a%2Fb%20c/open" {
		t.Errorf("hive id must be path-escaped: %q", got.Odd)
	}
}

// The shared row asset reads the STAGE_TRACKER_* constants as free globals
// that only the spoke page defines (#16067 / #16081). The hub page must
// define every one the embedded asset references, with the spoke's value,
// or the hub would light different strip columns than the spoke tracker.
// The set is derived from the embedded files themselves, so a constant the
// asset starts reading fails here until the page defines it.
func TestActivityPage_DefinesTrackerRowGlobalsWithSpokeValues(t *testing.T) {
	globalRe := regexp.MustCompile(`\bSTAGE_TRACKER_[A-Z_]+\b`)
	required := map[string]bool{}
	for _, name := range trackerrow.Files {
		b, err := fs.ReadFile(trackerrow.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range globalRe.FindAllString(string(b), -1) {
			required[g] = true
		}
	}
	if len(required) == 0 {
		t.Fatal("found no STAGE_TRACKER_* globals in the shared row asset; the scan is broken")
	}

	spokeFiles, err := filepath.Glob(filepath.Join("..", "dashboard", "static-src", "js", "*.js"))
	if err != nil || len(spokeFiles) == 0 {
		t.Fatalf("spoke static-src not found (%v)", err)
	}
	var spoke strings.Builder
	for _, f := range spokeFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		spoke.Write(b)
		spoke.WriteByte('\n')
	}
	js := readActivityScript(t)
	decl := func(src, name string) string {
		m := regexp.MustCompile(`(?m)^\s*const ` + name + ` = (.+?);`).FindStringSubmatch(src)
		if m == nil {
			return ""
		}
		return strings.TrimSpace(m[1])
	}
	for name := range required {
		want := decl(spoke.String(), name)
		if want == "" {
			t.Errorf("the spoke no longer defines %s; update the shared asset contract", name)
			continue
		}
		if got := decl(js, name); got != want {
			t.Errorf("activity.js %s = %q, want the spoke's %q", name, got, want)
		}
	}
	if !strings.Contains(js, "function esc(") {
		t.Errorf("activity.js must define esc(): tracker-row-title.js calls it")
	}
}
