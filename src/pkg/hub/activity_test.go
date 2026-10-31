package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/hubactivity"
)

var activityT0 = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

func activityDigest(repo string, numbers ...int) *hubactivity.Digest {
	items := make([]hubactivity.Item, 0, len(numbers))
	for _, n := range numbers {
		items = append(items, hubactivity.Item{Repo: repo, Number: n, Kind: "pr", Title: fmt.Sprintf("%s #%d", repo, n), Stage: "ci_review", StageNumber: 70})
	}
	d := hubactivity.Build(items, hubactivity.BuildOptions{Now: activityT0})
	return &d
}

func newActivityHub() *HubServer {
	return &HubServer{
		logger:                  slog.Default(),
		hubSecret:               testHubSecret,
		keyGenerations:          legacyGenerationSet(testHubSecret),
		saveCh:                  make(chan struct{}, 1),
		hubGitHash:              "abc1234",
		hubGitBranch:            "v5",
		clusters:                map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", Domain: "hive.example.test"}},
		heartbeatHealth:         make(map[string]*HeartbeatHealthEntry),
		heartbeatUpgrade:        make(map[string]string),
		heartbeatSwitchTag:      make(map[string]string),
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		pendingGateways:         make(map[string]*HeartbeatGatewayConfig),
		hubBanners:              make(map[string]*HubBannerEntry),
	}
}

func postActivityHeartbeat(t *testing.T, s *HubServer, hiveID string, d *hubactivity.Digest) HeartbeatResponse {
	t.Helper()
	body, err := json.Marshal(spoke.HeartbeatPayload{HiveID: hiveID, Org: "o", PrimaryRepo: "r", Repos: []string{"r"}, Activity: d})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", heartbeatBearer(testHubSecret, hiveID))
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat %s: status %d body=%s", hiveID, rec.Code, rec.Body.String())
	}
	var resp HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func getActivity(t *testing.T, s *HubServer, user, query, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, activityPath+query, nil)
	if user != "" {
		req.AddCookie(testAuthCookie(user))
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	s.requireAuth(s.handleHubActivity)(rec, req)
	return rec
}

func decodeActivity(t *testing.T, rec *httptest.ResponseRecorder) ActivityView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var v ActivityView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func activityHiveIDs(v ActivityView) map[string]bool {
	out := map[string]bool{}
	for _, it := range v.Items {
		out[it.HiveID] = true
	}
	return out
}

// Acceptance (#15861): two spokes heartbeating to one hub; a user granted
// only one of them sees only that hive's rows, an admin sees both.
func TestHubActivity_TwoSpokesPerUserScoping(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()

	ackA := postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))
	postActivityHeartbeat(t, s, "hive-b", activityDigest("o/b", 3))
	if ackA.ActivityETag == "" {
		t.Fatalf("heartbeat response must acknowledge the stored activity etag")
	}

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hive-a": "viewer"}}); err != nil {
		t.Fatal(err)
	}
	mkUser(t, "root")

	v := decodeActivity(t, getActivity(t, s, "alice", "", ""))
	if got := activityHiveIDs(v); !got["hive-a"] || got["hive-b"] {
		t.Fatalf("alice sees hives %v, want only hive-a", got)
	}
	for _, h := range v.Hives {
		if h.HiveID == "hive-b" {
			t.Fatalf("alice must not even see hive-b's freshness row: %+v", v.Hives)
		}
	}
	if len(v.Items) != 2 {
		t.Fatalf("alice items = %d, want 2", len(v.Items))
	}

	admin := decodeActivity(t, getActivity(t, s, "root", "", ""))
	if got := activityHiveIDs(admin); !got["hive-a"] || !got["hive-b"] {
		t.Fatalf("admin sees hives %v, want both", got)
	}
}

func TestHubActivity_RequiresAuthentication(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()

	rec := getActivity(t, s, "", "", "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
}

func TestHubActivity_UserWithNoGrantsSeesNothing(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	mkUser(t, "mallory")

	v := decodeActivity(t, getActivity(t, s, "mallory", "", ""))

	if len(v.Items) != 0 || len(v.Hives) != 0 {
		t.Fatalf("ungranted user saw %+v", v)
	}
}

// Acceptance: with share_private false a private-repo PR never reaches the
// hub API, but it is counted in totals.
func TestHubActivity_PrivateItemsNeverServedButCounted(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()
	d := hubactivity.Build([]hubactivity.Item{
		{Repo: "o/public", Number: 1, Kind: "pr", Title: "public", Stage: "ci_review"},
		{Repo: "o/secret", Number: 2, Kind: "pr", Title: "secret title", Stage: "ci_review", Private: true},
	}, hubactivity.BuildOptions{Now: activityT0})
	postActivityHeartbeat(t, s, "hive-a", &d)
	mkUser(t, "root")

	rec := getActivity(t, s, "root", "", "")
	v := decodeActivity(t, rec)

	if strings.Contains(rec.Body.String(), "o/secret") || strings.Contains(rec.Body.String(), "secret title") {
		t.Fatalf("private item leaked: %s", rec.Body.String())
	}
	if len(v.Hives) != 1 || v.Hives[0].Totals == nil || v.Hives[0].Totals.Items != 2 {
		t.Fatalf("totals must count the private item: %+v", v.Hives)
	}
}

func TestHubActivity_ETagPolling(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	mkUser(t, "root")

	first := getActivity(t, s, "root", "", "")
	etag := first.Header().Get("ETag")
	if etag == "" || first.Header().Get("Cache-Control") != "private, no-cache" || first.Header().Get("Vary") != "Cookie, Authorization" {
		t.Fatalf("headers = %v", first.Header())
	}
	again := getActivity(t, s, "root", "", etag)
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("matching If-None-Match: status %d body %q, want 304 empty", again.Code, again.Body.String())
	}

	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1, 2))
	changed := getActivity(t, s, "root", "", etag)
	if changed.Code != http.StatusOK || changed.Header().Get("ETag") == etag {
		t.Fatalf("changed content must return 200 with a new etag, got %d %q", changed.Code, changed.Header().Get("ETag"))
	}
}

func TestHubActivity_ExpiredGrantServesNothing(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	s := newActivityHub()
	postActivityHeartbeat(t, s, "hive-a", activityDigest("o/a", 1))
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "bob",
		Hives:          map[string]string{"hive-a": "read"},
		HiveExpiry:     map[string]string{"hive-a": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
	}); err != nil {
		t.Fatal(err)
	}

	v := decodeActivity(t, getActivity(t, s, "bob", "", ""))

	if len(v.Items) != 0 || len(v.Hives) != 0 {
		t.Fatalf("expired grant still served %+v", v)
	}
}

// A beat without an activity block (older spoke, or one that skipped a
// beat) keeps the held digest and still acknowledges its etag.
func TestHubActivity_BeatWithoutBlockKeepsDigest(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	full := activityDigest("o/a", 1)
	postActivityHeartbeat(t, s, "hive-a", full)

	resp := postActivityHeartbeat(t, s, "hive-a", nil)

	if resp.ActivityETag != full.ETag {
		t.Fatalf("ack = %q, want held %q", resp.ActivityETag, full.ETag)
	}
	if got := s.activity.snapshot([]string{"hive-a"})["hive-a"]; len(got.digest.Items) != 1 {
		t.Fatalf("held digest lost: %+v", got)
	}
}

func TestHubActivity_RejectsOversizedFilter(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newActivityHub()
	mkUser(t, "alice")

	rec := getActivity(t, s, "alice", "?q="+url.QueryEscape(strings.Repeat("x", maxActivityFilterRunes+1)), "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized filter status = %d, want 400", rec.Code)
	}
}

func TestActivityStore_RecordSemantics(t *testing.T) {
	var st activityStore
	full := activityDigest("o/a", 1)

	if ack := st.record("h", nil, activityT0); ack != "" {
		t.Fatalf("nil block on empty store acked %q", ack)
	}
	ack := st.record("h", full, activityT0)
	if ack != full.ETag {
		t.Fatalf("ack = %q, want %q", ack, full.ETag)
	}

	// Matching unchanged marker refreshes freshness, keeps items.
	later := activityT0.Add(3 * time.Minute)
	if got := st.record("h", &hubactivity.Digest{ETag: full.ETag, GeneratedAt: later}, later); got != full.ETag {
		t.Fatalf("marker ack = %q", got)
	}
	snap := st.snapshot([]string{"h"})["h"]
	if !snap.receivedAt.Equal(later) || len(snap.digest.Items) != 1 {
		t.Fatalf("marker must refresh in place: %+v", snap)
	}

	// Mismatched marker is ignored and acks the held etag.
	if got := st.record("h", &hubactivity.Digest{ETag: `W/"other"`, GeneratedAt: later}, later); got != full.ETag {
		t.Fatalf("mismatched marker ack = %q, want held %q", got, full.ETag)
	}
	if len(st.snapshot([]string{"h"})["h"].digest.Items) != 1 {
		t.Fatalf("mismatched marker must not clear content")
	}

	// A marker whose etag only differs by what cleaning strips still matches.
	if got := st.record("h", &hubactivity.Digest{ETag: " " + full.ETag + "\n"}, later); got != full.ETag {
		t.Fatalf("dirty marker etag ack = %q, want %q", got, full.ETag)
	}

	// A marker for an unknown hive acks nothing, so the spoke resends full.
	if got := st.record("new", &hubactivity.Digest{ETag: full.ETag}, later); got != "" {
		t.Fatalf("marker for unknown hive acked %q", got)
	}
}

func TestActivityStore_EnforcesHubCeiling(t *testing.T) {
	var st activityStore
	d := hubactivity.Digest{ETag: `W/"big"`, Totals: &hubactivity.Totals{Items: hubactivity.MaxItemsCeiling + 30}}
	for i := 1; i <= hubactivity.MaxItemsCeiling+30; i++ {
		d.Items = append(d.Items, hubactivity.Item{Repo: "o/r", Number: i})
	}

	st.record("h", &d, activityT0)

	got := st.snapshot([]string{"h"})["h"].digest
	if len(got.Items) != hubactivity.MaxItemsCeiling || got.Truncated != 30 {
		t.Fatalf("stored %d items truncated %d, want %d/30", len(got.Items), got.Truncated, hubactivity.MaxItemsCeiling)
	}
}

func TestActivityStore_ForgetsLongSilentHives(t *testing.T) {
	var st activityStore
	st.record("old", activityDigest("o/a", 1), activityT0)

	st.record("other", activityDigest("o/b", 1), activityT0.Add(activityForgetAfter+time.Minute))

	if _, ok := st.snapshot([]string{"old"})["old"]; ok {
		t.Fatalf("a block silent for longer than activityForgetAfter must be dropped")
	}
}

// Acceptance: a stopped spoke is stale after two missed heartbeats; a spoke
// that sends no block shows as not reporting; a granted hive the registry
// does not know is disclosed as missing.
func TestBuildActivityView_FreshnessDisclosure(t *testing.T) {
	now := activityT0.Add(10 * time.Minute)
	hives := []activityHive{{ID: "fresh"}, {ID: "gone"}, {ID: "old-spoke"}, {ID: "ghost", Missing: true}}
	blocks := map[string]storedActivity{
		"fresh": {digest: *activityDigest("o/a", 1), receivedAt: now.Add(-activityHeartbeatInterval)},
		"gone":  {digest: *activityDigest("o/b", 2), receivedAt: now.Add(-activityStaleAfter - time.Second)},
	}

	v := buildActivityView(hives, blocks, activityFilter{}, now)

	want := map[string]string{"fresh": activityStatusOK, "gone": activityStatusStale, "old-spoke": activityStatusNotReporting, "ghost": activityStatusMissing}
	if len(v.Hives) != len(want) {
		t.Fatalf("hives = %+v", v.Hives)
	}
	for _, h := range v.Hives {
		if h.Status != want[h.HiveID] {
			t.Errorf("%s status = %q, want %q", h.HiveID, h.Status, want[h.HiveID])
		}
		if h.HiveID == "old-spoke" && h.Reporting {
			t.Errorf("old spoke must report reporting=false")
		}
	}
	if len(v.Items) != 2 {
		t.Fatalf("stale hive rows stay visible with their status: %d items", len(v.Items))
	}
}

func TestBuildActivityView_FiltersAndResponseCap(t *testing.T) {
	now := activityT0
	var big []int
	for i := 1; i <= hubactivity.MaxItemsCeiling; i++ {
		big = append(big, i)
	}
	hives := make([]activityHive, 0, 9)
	blocks := map[string]storedActivity{}
	for i := 0; i < 9; i++ {
		id := fmt.Sprintf("h%d", i)
		hives = append(hives, activityHive{ID: id})
		d := hubactivity.Build(itemsFor("o/"+id, big), hubactivity.BuildOptions{Now: now, MaxItems: hubactivity.MaxItemsCeiling})
		blocks[id] = storedActivity{digest: d, receivedAt: now}
	}

	all := buildActivityView(hives, blocks, activityFilter{}, now)
	if len(all.Items) != maxActivityResponseItems || all.Truncated != hubactivity.MaxItemsCeiling {
		t.Fatalf("items %d truncated %d, want %d/%d", len(all.Items), all.Truncated, maxActivityResponseItems, hubactivity.MaxItemsCeiling)
	}

	f, err := parseActivityFilter(url.Values{"hive": {"h3"}, "q": {"#7"}})
	if err != nil {
		t.Fatal(err)
	}
	one := buildActivityView(hives, blocks, f, now)
	if len(one.Items) != 1 || one.Items[0].HiveID != "h3" || one.Items[0].Number != 7 || len(one.Hives) != 1 {
		t.Fatalf("hive+number filter = %+v", one)
	}
}

// A numeric q matches the item number OR a title containing it.
func TestActivityFilter_NumericQueryAlsoMatchesTitles(t *testing.T) {
	f, err := parseActivityFilter(url.Values{"q": {"2024"}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.matches("h", hubactivity.Item{Repo: "o/r", Number: 2024, Title: "x"}) {
		t.Errorf("number match failed")
	}
	if !f.matches("h", hubactivity.Item{Repo: "o/r", Number: 1, Title: "Roadmap 2024"}) {
		t.Errorf("title match failed")
	}
	if f.matches("h", hubactivity.Item{Repo: "o/r", Number: 1, Title: "nope"}) {
		t.Errorf("unrelated item matched")
	}
}

func itemsFor(repo string, numbers []int) []hubactivity.Item {
	out := make([]hubactivity.Item, 0, len(numbers))
	for _, n := range numbers {
		out = append(out, hubactivity.Item{Repo: repo, Number: n, Kind: "issue", Title: fmt.Sprintf("t%d", n), Stage: "actionable", StageNumber: 10})
	}
	return out
}

func TestVisibleActivityHives_ExpiredGrantIsNotVisible(t *testing.T) {
	cleanup := helperSetupTempDirs(t) // isHubAdmin also reads the granted-admins file
	defer cleanup()
	t.Setenv(hubAdminsEnv, "github:root")
	now := activityT0
	user := &SaaSUser{
		GitHubUsername: "alice",
		Hives:          map[string]string{"live": "viewer", "expired": "viewer", "ghost": "viewer"},
		HiveExpiry:     map[string]string{"expired": now.Add(-time.Hour).Format(time.RFC3339)},
	}
	registry := []activityHive{{ID: "live"}, {ID: "expired"}, {ID: "owned", Owner: "alice"}, {ID: "other"}}

	got := visibleActivityHives("alice", user, registry, now)

	ids := map[string]bool{}
	for _, h := range got {
		ids[h.ID] = true
		if h.ID == "ghost" && !h.Missing {
			t.Errorf("granted hive unknown to the registry must be marked missing")
		}
	}
	if !ids["live"] || !ids["owned"] || !ids["ghost"] || ids["expired"] || ids["other"] {
		t.Fatalf("visible = %v", ids)
	}
	if len(visibleActivityHives("", user, registry, now)) != 0 {
		t.Fatalf("anonymous viewer must see nothing")
	}
}

func TestActivityETagMatches(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{`"abc"`, true},
		{`W/"abc"`, true},
		{`"x", "abc"`, true},
		{`*`, true},
		{`"x"`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := activityETagMatches(c.header, `"abc"`); got != c.want {
			t.Errorf("activityETagMatches(%q) = %v, want %v", c.header, got, c.want)
		}
	}
}

// The spoke's activity block and ack decode field-for-field on the hub.
func TestSpokeActivityWireCompat(t *testing.T) {
	in := spoke.HeartbeatPayload{HiveID: "h", Activity: activityDigest("o/a", 1)}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Activity == nil || out.Activity.ETag != in.Activity.ETag || len(out.Activity.Items) != 1 || out.Activity.Items[0].Repo != "o/a" {
		t.Fatalf("activity lost on the wire: %+v", out.Activity)
	}

	// A spoke predating the block sends nothing: the field stays nil.
	raw, err = json.Marshal(spoke.HeartbeatPayload{HiveID: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"activity"`) {
		t.Fatalf("an empty activity block must be omitted: %s", raw)
	}

	ackRaw, err := json.Marshal(HeartbeatResponse{OK: true, ActivityETag: `W/"e"`})
	if err != nil {
		t.Fatal(err)
	}
	var ack spoke.HeartbeatResponse
	if err := json.Unmarshal(ackRaw, &ack); err != nil || ack.ActivityETag != `W/"e"` {
		t.Fatalf("spoke did not decode the ack: %+v err=%v", ack, err)
	}
}

func TestActivityStoreForget(t *testing.T) {
	var st activityStore
	st.record("hive-a", activityDigest("o/r", 1), activityT0)
	st.record("hive-b", activityDigest("o/r", 2), activityT0)

	st.forget("hive-a")
	if got := st.snapshot([]string{"hive-a"}); len(got) != 0 {
		t.Errorf("forgotten hive still held: %v", got)
	}
	if got := st.snapshot([]string{"hive-b"}); len(got) != 1 {
		t.Errorf("forget dropped an unrelated hive: %v", got)
	}

	st.forget("unknown")
	var nilStore *activityStore
	nilStore.forget("hive-a")
}
