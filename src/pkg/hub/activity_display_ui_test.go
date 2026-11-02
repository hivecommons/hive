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

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/webassets/trackerrow"
	"github.com/hivecommons/hive/pkg/webassets/trackerview"
)

// Display mode on the hub Fleet activity page (#16064, part of #15965 / RFC
// #15861 by @hanthor). The page reuses #15860 Part A unchanged: `?kiosk=1`,
// `am.*` URL state and `?display=1` with `am.view` + `interval` playlists,
// read by the spoke's own helpers (pkg/webassets/trackerview). These tests
// run the SHIPPED page script under node with the shared assets loaded first,
// exactly as activity.html loads them.

// activityDisplayHarness is every script activity.html loads, in its order.
func activityDisplayHarness(t *testing.T) string {
	t.Helper()
	var parts []string
	for _, set := range []struct {
		fsys  fs.FS
		files []string
	}{{trackerrow.FS, trackerrow.Files}, {trackerview.FS, trackerview.Files}} {
		for _, name := range set.files {
			b, err := fs.ReadFile(set.fsys, name)
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, string(b))
		}
	}
	return strings.Join(append(parts, readActivityScript(t)), "\n")
}

func TestActivityDisplay_PageLoadsTheSharedViewAssetBeforeThePageScript(t *testing.T) {
	page, err := fs.ReadFile(staticFS, "static/activity.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	view := strings.Index(html, `<script src="`+activityAssetPrefix+`tracker-view.js" defer></script>`)
	pageJS := strings.Index(html, `<script src="/static/activity.js" defer></script>`)
	if view < 0 || pageJS < 0 || view > pageJS {
		t.Fatalf("activity.html must load %stracker-view.js before /static/activity.js (view at %d, page at %d)", activityAssetPrefix, view, pageJS)
	}
	if !strings.Contains(html, `id="activity-playlist"`) {
		t.Errorf("activity.html has no #activity-playlist element for the display playlist bar")
	}

	t.Setenv("HUB_DATA_DIR", t.TempDir())
	srv := newHubServerForTest(t, withHubIdentity("test", "v5"))
	want, err := fs.ReadFile(trackerview.FS, "tracker-view.js")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, activityAssetPrefix+"tracker-view.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != string(want) {
		t.Fatalf("GET %stracker-view.js = %d; want 200 and the bytes of pkg/webassets/trackerview", activityAssetPrefix, rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-cache" || rec.Header().Get("ETag") != webstatic.ETagFor(want) {
		t.Errorf("tracker-view.js cache headers = %q / %q, want the static no-cache + content ETag", rec.Header().Get("Cache-Control"), rec.Header().Get("ETag"))
	}
	for _, bad := range []string{"trackerview.go", "trackerview_test.go"} {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, activityAssetPrefix+bad, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s%s = %d, want 404", activityAssetPrefix, bad, rec.Code)
		}
	}
}

// ?kiosk=1 and ?display=1 resolve exactly as the spoke's boot.js does: kiosk
// wins when both are present, and only the value "1" turns either on.
func TestActivityDisplay_KioskAndDisplayFlags(t *testing.T) {
	src := activityDisplayHarness(t) + "\n" + `result = [
  activityDisplayFlags('https://hub.example/activity'),
  activityDisplayFlags('https://hub.example/activity?kiosk=1'),
  activityDisplayFlags('https://hub.example/activity?display=1'),
  activityDisplayFlags('https://hub.example/activity?kiosk=1&display=1'),
  activityDisplayFlags('https://hub.example/activity?kiosk=true&display=yes'),
  activityDisplayFlags('not a url'),
];`
	var got []struct{ Kiosk, Display bool }
	runEvidenceJS(t, src, &got)
	want := []struct{ Kiosk, Display bool }{{false, false}, {true, false}, {false, true}, {true, false}, {false, false}, {false, false}}
	if len(got) != len(want) {
		t.Fatalf("flags = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("case %d flags = %+v, want %+v", i, got[i], want[i])
		}
	}

	css, err := fs.ReadFile(staticFS, "static/activity.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		"body.kiosk-mode .activity-header", "body.display-mode .activity-header",
		"body.kiosk-mode .activity-toggle", "body.display-mode .activity-toggle",
		"body.kiosk-mode { cursor: none; }", "body.kiosk-mode .activity-playlist",
	} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("activity.css has no %q rule; kiosk/display must hide the chrome like the spoke", rule)
		}
	}
}

// am.* state round-trips with the spoke's keys: a URL copied from a spoke
// display applies on the hub where the state applies (kind, repo, q, author,
// sort, dir, group), keeps the keys that do not apply, and says so.
func TestActivityDisplay_AmStateRoundTripsAndFiltersTheBoard(t *testing.T) {
	view := map[string]any{
		"hives": []map[string]any{{"hive_id": "hive-a", "status": "ok"}, {"hive_id": "hive-b", "status": "ok"}},
		"items": []map[string]any{
			{"hive_id": "hive-a", "repo": "o/a", "n": 7, "kind": "pr", "title": "Fix the parser", "author_kind": "hive", "stage": "ci_review", "stage_n": 50, "since": "2026-10-31T08:00:00Z"},
			{"hive_id": "hive-b", "repo": "o/b", "n": 9, "kind": "pr", "title": "Parser docs", "author_kind": "human", "stage": "pr_open", "stage_n": 30, "since": "2026-10-31T07:00:00Z"},
			{"hive_id": "hive-a", "repo": "o/a", "n": 3, "kind": "issue", "title": "Parser crash", "author_kind": "hive", "stage": "open"},
			{"hive_id": "hive-b", "repo": "o/a", "n": 12, "kind": "pr", "title": "Unrelated", "author_kind": "hive", "stage": "merge_gate", "stage_n": 80, "since": "2026-10-31T06:00:00Z"},
		},
		"events": []any{},
	}
	vb, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	src := activityDisplayHarness(t) + "\nvar view = " + string(vb) + ";\n" + `
var nums = function (v) { return v.items.map(function (it) { return it.repo + '#' + it.n; }); };
var spoke = 'https://spoke.example/?display=1&am.kind=pr&am.q=parser&am.author=hive&am.labels=bug&am.group=repo#section-activity-monitor-section';
var s = activityUrlView(spoke);
var hub = 'https://hub.example/activity?display=1&am.sort=number&am.dir=asc&am.group=none&token=secret';
var h = activityUrlView(hub);
result = {
  defaults: activityUrlView('https://hub.example/activity'),
  spokeGroup: s.group,
  spokeItems: nums(activityVisibleView(view, s.filter)),
  unapplied: activityUnappliedKeys(s.filter),
  hubGroup: h.group,
  hubItems: nums(activityVisibleView(view, h.filter)),
  furthest: nums(activityVisibleView(view, activityUrlView('https://hub.example/activity').filter)),
  repo: nums(activityVisibleView(view, activityUrlView('https://hub.example/activity?am.repo=o/b').filter)),
  untouched: nums(view),
  back: activityViewHref(hub, h.filter, h.group),
  regroup: activityViewHref(hub, h.filter, 'hive'),
  keepsSpokeKeys: activityViewHref('https://hub.example/activity', s.filter, s.group),
  newest: activityUnappliedKeys(activityUrlView('https://hub.example/activity?am.sort=newest').filter),
};`
	var got struct {
		Defaults struct {
			Group  string
			Filter struct{ Kind, Sort, Dir string }
		}
		SpokeGroup, HubGroup                      string
		SpokeItems, HubItems, Furthest, Untouched []string
		Repo                                      []string
		Unapplied, Newest                         []string
		Back, Regroup, KeepsSpokeKeys             string
	}
	runEvidenceJS(t, src, &got)

	if got.Defaults.Group != "hive" || got.Defaults.Filter.Kind != "all" || got.Defaults.Filter.Sort != "furthest" || got.Defaults.Filter.Dir != "desc" {
		t.Errorf("no am.* = %+v, want the spoke defaults with the board grouped by hive", got.Defaults)
	}
	// am.group=repo is a spoke grouping the hub board does not have: it falls
	// back to the hub default exactly as an unknown value does on the spoke.
	if got.SpokeGroup != "hive" {
		t.Errorf("spoke am.group=repo on the hub = %q, want the default 'hive'", got.SpokeGroup)
	}
	if strings.Join(got.SpokeItems, ",") != "o/a#7" {
		t.Errorf("am.kind=pr&am.q=parser&am.author=hive kept %v, want only o/a#7", got.SpokeItems)
	}
	if strings.Join(got.Unapplied, ",") != "am.labels" {
		t.Errorf("unapplied keys = %v, want [am.labels]", got.Unapplied)
	}
	if strings.Join(got.Newest, ",") != "am.sort" {
		t.Errorf("am.sort=newest needs created_at, which the digest lacks; unapplied = %v, want [am.sort]", got.Newest)
	}
	if got.HubGroup != "none" || strings.Join(got.HubItems, ",") != "o/a#3,o/a#7,o/a#12,o/b#9" {
		t.Errorf("am.sort=number&am.dir=asc&am.group=none = %q %v", got.HubGroup, got.HubItems)
	}
	if strings.Join(got.Furthest, ",") != "o/a#12,o/a#7,o/b#9,o/a#3" {
		t.Errorf("default furthest sort = %v, want the furthest strip column first", got.Furthest)
	}
	if strings.Join(got.Repo, ",") != "o/b#9" {
		t.Errorf("am.repo=o/b kept %v", got.Repo)
	}
	if strings.Join(got.Untouched, ",") != "o/a#7,o/b#9,o/a#3,o/a#12" {
		t.Errorf("activityVisibleView must not mutate the polled view; items now %v", got.Untouched)
	}
	if got.Back != "https://hub.example/activity?display=1&am.sort=number&am.dir=asc&am.group=none" {
		t.Errorf("round trip = %q; want the same am.* keys back and no credential", got.Back)
	}
	if got.Regroup != "https://hub.example/activity?display=1&am.sort=number&am.dir=asc" {
		t.Errorf("grouping by hive again = %q; the default must be omitted", got.Regroup)
	}
	if got.KeepsSpokeKeys != "https://hub.example/activity?am.kind=pr&am.q=parser&am.author=hive&am.labels=bug" {
		t.Errorf("a spoke view re-serialised on the hub = %q; the spoke keys must survive", got.KeepsSpokeKeys)
	}
}

// ?display=1 turns `am.view` + `interval` into the display playlist with the
// spoke's dwell and wall-clock rotation, and every entry is a hub-activity
// view. Kiosk and the plain page never rotate, and one view is not a playlist.
func TestActivityDisplay_PlaylistListsHubActivityViews(t *testing.T) {
	src := activityDisplayHarness(t) + "\n" + `
var two = 'https://hub.example/activity?display=1&am.view=' + encodeURIComponent('am.kind=pr') + '&am.view=' + encodeURIComponent('am.group=none&am.repo=o/a') + '&am.view=bogus&interval=2m';
var pl = activityPlaylist(two);
result = {
  kind: ACTIVITY_DISPLAY_VIEW,
  views: pl.views.map(function (v) { return { kind: v.kind, filterKind: v.filter.kind, repo: v.filter.repo, group: v.group }; }),
  intervalMs: pl.intervalMs,
  at0: activityPlaylistAt(pl, 0),
  at1: activityPlaylistAt(pl, 120000 + 30000),
  at2: activityPlaylistAt(pl, 240000),
  kiosk: activityPlaylist(two.replace('display=1', 'kiosk=1')),
  plain: activityPlaylist(two.replace('display=1&', '')),
  one: activityPlaylist('https://hub.example/activity?display=1&am.view=' + encodeURIComponent('am.kind=pr')),
};`
	var got struct {
		Kind          string
		Views         []struct{ Kind, FilterKind, Repo, Group string }
		IntervalMs    int
		At0, At1, At2 struct {
			Index       int
			RemainingMs int
			Label       string
		}
		Kiosk, Plain, One *struct{}
	}
	runEvidenceJS(t, src, &got)

	if got.Kind != "hub-activity" {
		t.Errorf("ACTIVITY_DISPLAY_VIEW = %q, want hub-activity", got.Kind)
	}
	if len(got.Views) != 2 {
		t.Fatalf("playlist views = %+v, want the two valid am.view entries (bogus skipped)", got.Views)
	}
	if got.Views[0].Kind != "hub-activity" || got.Views[0].FilterKind != "pr" || got.Views[0].Group != "hive" {
		t.Errorf("view 0 = %+v", got.Views[0])
	}
	if got.Views[1].Kind != "hub-activity" || got.Views[1].Repo != "o/a" || got.Views[1].Group != "none" {
		t.Errorf("view 1 = %+v", got.Views[1])
	}
	if got.IntervalMs != 120000 {
		t.Errorf("interval=2m = %d ms", got.IntervalMs)
	}
	if got.At0.Index != 0 || got.At0.RemainingMs != 120000 || got.At0.Label != "1/2 · next in 120s" {
		t.Errorf("at t=0: %+v", got.At0)
	}
	if got.At1.Index != 1 || got.At1.RemainingMs != 90000 || got.At1.Label != "2/2 · next in 90s" {
		t.Errorf("at t=150s: %+v", got.At1)
	}
	if got.At2.Index != 0 {
		t.Errorf("at t=240s the playlist wraps to view 0, got %+v", got.At2)
	}
	if got.Kiosk != nil || got.Plain != nil || got.One != nil {
		t.Errorf("only ?display=1 with two or more views rotates: kiosk=%v plain=%v one=%v", got.Kiosk, got.Plain, got.One)
	}
}

// Within one strip column the furthest sort keeps the spoke's attention tier:
// rows with a modifier first, held or draft rows last, whatever am.dir says.
// The tier's vocabulary is the spoke's.
func TestActivityDisplay_FurthestSortKeepsTheSpokeAttentionTier(t *testing.T) {
	src := activityDisplayHarness(t) + "\n" + `
var view = { hives: [], items: [
  { repo: 'o/a', n: 1, kind: 'pr', stage_n: 50, since: '2026-10-31T06:00:00Z', mods: ['hold'] },
  { repo: 'o/a', n: 2, kind: 'pr', stage_n: 50, since: '2026-10-31T07:00:00Z' },
  { repo: 'o/a', n: 3, kind: 'pr', stage_n: 50, since: '2026-10-31T08:00:00Z', mods: ['stalled'] },
] };
var order = function (q) { return activityVisibleView(view, activityUrlView('https://hub.example/activity' + q).filter).items.map(function (it) { return it.n; }); };
result = { desc: order(''), asc: order('?am.dir=asc') };`
	var got struct{ Desc, Asc []int }
	runEvidenceJS(t, src, &got)
	if len(got.Desc) != 3 || got.Desc[0] != 3 || got.Desc[1] != 2 || got.Desc[2] != 1 {
		t.Errorf("furthest desc = %v, want [3 2 1] (stalled, normal, held)", got.Desc)
	}
	if len(got.Asc) != 3 || got.Asc[0] != 3 || got.Asc[2] != 1 {
		t.Errorf("furthest asc = %v; am.dir must never flip the tier", got.Asc)
	}

	spoke, err := os.ReadFile(filepath.Join("..", "dashboard", "static-src", "js", "tracker-words-stage.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := readActivityScript(t)
	for hub, name := range map[string]string{"ACTIVITY_SUNK_MODS": "STAGE_TRACKER_SUNK_MODS", "ACTIVITY_ATTENTION_STAGES": "STAGE_TRACKER_ATTENTION_STAGES"} {
		want := regexp.MustCompile(`(?m)^\s*const ` + name + ` = (.+?);`).FindStringSubmatch(string(spoke))
		have := regexp.MustCompile(`(?m)^\s*const ` + hub + ` = (.+?);`).FindStringSubmatch(js)
		if want == nil || have == nil || want[1] != have[1] {
			t.Errorf("activity.js %s = %v, want the spoke's %s = %v", hub, have, name, want)
		}
	}
}

// Leaving display mode (Esc or d, like the spoke) keeps the view and drops
// only display=1; typing, modified keys and browser full screen never leave it.
func TestActivityDisplay_ExitKeysAndHref(t *testing.T) {
	src := activityDisplayHarness(t) + "\n" + `result = {
  href: activityDisplayExitHref('https://hub.example/activity?display=1&am.kind=pr&interval=2m'),
  esc: activityDisplayExitKey({ key: 'Escape' }, false, false),
  d: activityDisplayExitKey({ key: 'd' }, false, false),
  fullscreen: activityDisplayExitKey({ key: 'Escape' }, false, true),
  typing: activityDisplayExitKey({ key: 'd' }, true, false),
  ctrl: activityDisplayExitKey({ key: 'd', ctrlKey: true }, false, false),
  other: activityDisplayExitKey({ key: 'x' }, false, false),
};`
	var got struct {
		Href                                    string
		Esc, D, Fullscreen, Typing, Ctrl, Other bool
	}
	runEvidenceJS(t, src, &got)
	if got.Href != "https://hub.example/activity?am.kind=pr&interval=2m" {
		t.Errorf("exit href = %q", got.Href)
	}
	if !got.Esc || !got.D || got.Fullscreen || got.Typing || got.Ctrl || got.Other {
		t.Errorf("exit keys = %+v", got)
	}
}

// The shared view asset reads the spoke's STAGE_TRACKER_* vocabulary as free
// globals. activity.js must define each with the spoke's value, so am.*
// normalises identically, except the group vocabulary: the hub board groups
// by hive (default) or not at all, never by repo.
func TestActivityDisplay_DefinesTrackerViewGlobalsWithSpokeValues(t *testing.T) {
	globalRe := regexp.MustCompile(`\bSTAGE_TRACKER_[A-Z_]+\b`)
	b, err := fs.ReadFile(trackerview.FS, "tracker-view.js")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*const (STAGE_TRACKER_[A-Z_]+) =`).FindAllStringSubmatch(string(b), -1) {
		declared[m[1]] = true
	}
	required := map[string]bool{}
	for _, g := range globalRe.FindAllString(string(b), -1) {
		if !declared[g] {
			required[g] = true
		}
	}
	if len(required) == 0 {
		t.Fatal("found no host-supplied STAGE_TRACKER_* globals in tracker-view.js; the scan is broken")
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
	hubOnly := map[string]string{
		"STAGE_TRACKER_GROUPS":        "['hive', 'none']",
		"STAGE_TRACKER_GROUP_DEFAULT": "'hive'",
	}
	for name := range required {
		want, ok := hubOnly[name]
		if !ok {
			want = decl(spoke.String(), name)
			if want == "" {
				t.Errorf("the spoke no longer defines %s; update the shared view asset contract", name)
				continue
			}
		}
		if got := decl(js, name); got != want {
			t.Errorf("activity.js %s = %q, want %q", name, got, want)
		}
	}
	for name := range hubOnly {
		if !required[name] {
			t.Errorf("%s is no longer read by tracker-view.js; drop it from the hub-only list", name)
		}
	}
}
