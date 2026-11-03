package github

// Hold-source detection (hivecommons/hive#11564, spec #11507): every held
// issue and PR carries the set of sources currently requiring its hold, the
// check is read-only, and an unchanged refresh makes no GitHub API calls for
// hold sources.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type holdSourceFake struct {
	t        *testing.T
	mu       sync.Mutex
	comments map[int][]map[string]any
	events   map[int][]map[string]any
	reads    int
	writes   []string
}

func (f *holdSourceFake) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	var number int
	var kind string
	if _, err := fmt.Sscanf(r.URL.Path, "/repos/acme/widget/issues/%d/%s", &number, &kind); err != nil {
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.reads++
	switch kind {
	case "comments":
		_ = json.NewEncoder(w).Encode(append([]map[string]any{}, f.comments[number]...))
	case "events":
		_ = json.NewEncoder(w).Encode(append([]map[string]any{}, f.events[number]...))
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *holdSourceFake) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *holdSourceFake) resetReads() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = 0
}

// newHoldSourceTest points Hive's local record at a temp file and returns a
// fake GitHub that fails the test if anything is written to it.
func newHoldSourceTest(t *testing.T) (*holdSourceFake, *Client) {
	t.Helper()
	old := HoldSourcesPath
	HoldSourcesPath = filepath.Join(t.TempDir(), HoldSourcesFile)
	t.Cleanup(func() { HoldSourcesPath = old })

	f := &holdSourceFake{t: t, comments: map[int][]map[string]any{}, events: map[int][]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.writes) != 0 {
			t.Errorf("hold-source detection wrote to GitHub: %v", f.writes)
		}
	})
	c := NewClientForTest(srv.URL, "acme", []string{"acme/widget"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	return f, c
}

func holdSourceComment(author, body string) map[string]any {
	return map[string]any{"body": body, "user": map[string]string{"login": author}}
}

func holdSourceEvent(kind, label, actor string, at time.Time) map[string]any {
	return map[string]any{
		"event":      kind,
		"label":      map[string]string{"name": label},
		"actor":      map[string]string{"login": actor},
		"created_at": at.UTC().Format(time.RFC3339),
	}
}

var holdSourceT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func detectPRHoldSources(t *testing.T, c *Client, number int, updatedAt time.Time, labels ...string) []string {
	t.Helper()
	d := c.newHoldSourceDetector(HoldKindPR, "acme", "widget", time.Now())
	got := d.prSources(t.Context(), number, updatedAt, labels)
	d.flush()
	return got
}

func detectIssueHoldSources(t *testing.T, c *Client, number int, labels ...string) []string {
	t.Helper()
	d := c.newHoldSourceDetector(HoldKindIssue, "acme", "widget", time.Now())
	got := d.issueSources(number, labels)
	d.flush()
	return got
}

func assertHoldSources(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hold sources = %v, want %v", got, want)
	}
}

func appHeld(f *holdSourceFake, number int) {
	f.events[number] = []map[string]any{holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0)}
}

func TestHoldSources_LevelHoldNoticeAndAppLabelIsExactlyLevelGate(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[11] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	appHeld(f, 11)
	assertHoldSources(t, detectPRHoldSources(t, c, 11, holdSourceT0, "hold"), HoldSourceLevelGate)
}

func TestHoldSources_OutreachNoticeIsExactlyOutreach(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[12] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("outreach"))}
	appHeld(f, 12)
	assertHoldSources(t, detectPRHoldSources(t, c, 12, holdSourceT0, "hold"), HoldSourceOutreach)
}

func TestHoldSources_LevelHoldAndReporterTrustNoticesListBoth(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[13] = []map[string]any{
		holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality")),
		holdSourceComment(testHiveAppBotLogin, ReporterTrustNoticeMarker+"\nHeld for maintainer sign-off."),
	}
	appHeld(f, 13)
	assertHoldSources(t, detectPRHoldSources(t, c, 13, holdSourceT0, "hold"), HoldSourceLevelGate, HoldSourceReporterTrust)
}

func TestHoldSources_SelfAuthorizationNotice(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[14] = []map[string]any{holdSourceComment(testHiveAppBotLogin, SelfAuthorizationNoticeMarker+"\nHeld: self-authorized work.")}
	appHeld(f, 14)
	assertHoldSources(t, detectPRHoldSources(t, c, 14, holdSourceT0, "hold"), HoldSourceSelfAuthorization)
}

func TestHoldSources_HumanMergePathNotice(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[15] = []map[string]any{holdSourceComment(testHiveAppBotLogin, HumanMergePathComment([]string{".github/workflows/ci.yml"}))}
	appHeld(f, 15)
	assertHoldSources(t, detectPRHoldSources(t, c, 15, holdSourceT0, "hold"), HoldSourceHumanMergePath)
}

func TestHoldSources_NoticeFromNonAppAuthorIsIgnored(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[16] = []map[string]any{holdSourceComment("mallory", levelHoldNotice("quality"))}
	appHeld(f, 16)
	assertHoldSources(t, detectPRHoldSources(t, c, 16, holdSourceT0, "hold"), HoldSourceUnknown)
}

func TestHoldSources_HumanMergePathMarkerFromNonAppAuthorIsIgnored(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[16] = []map[string]any{
		holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality")),
		holdSourceComment("mallory", HumanMergePathComment([]string{".github/workflows/ci.yml"})),
	}
	appHeld(f, 16)
	assertHoldSources(t, detectPRHoldSources(t, c, 16, holdSourceT0, "hold"), HoldSourceLevelGate)

	f.comments[17] = []map[string]any{holdSourceComment("mallory", HumanMergePathComment([]string{"a.yml"}))}
	appHeld(f, 17)
	assertHoldSources(t, detectPRHoldSources(t, c, 17, holdSourceT0, "hold"), HoldSourceUnknown)
}

func TestHoldSources_ForeignHivePauseLabelIsNotASource(t *testing.T) {
	f, c := newHoldSourceTest(t)
	c.SetHoldLabels([]string{CanonicalHiveHoldLabel("hive-1")})
	f.comments[16] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	appHeld(f, 16)
	assertHoldSources(t, detectPRHoldSources(t, c, 16, holdSourceT0, "hold", CanonicalHiveHoldLabel("hive-2")), HoldSourceLevelGate)
	assertHoldSources(t, detectIssueHoldSources(t, c, 33, CanonicalHiveHoldLabel("hive-2")))
	assertHoldSources(t, detectIssueHoldSources(t, c, 34, "hold", "HIVE-PAUSE/HIVE-1"), HoldSourceDashboardPause, HoldSourcePerson)
}

func TestHoldSources_DashboardPausedPRListsDashboardPauseWithoutAPICalls(t *testing.T) {
	f, c := newHoldSourceTest(t)
	c.SetHoldLabels([]string{CanonicalHiveHoldLabel("hive-1")})
	assertHoldSources(t, detectPRHoldSources(t, c, 17, holdSourceT0, CanonicalHiveHoldLabel("hive-1")), HoldSourceDashboardPause)
	if n := f.readCount(); n != 0 {
		t.Fatalf("a pause-only hold needs no check, got %d API calls", n)
	}
}

func TestHoldSources_DashboardPauseAlongsideLevelHold(t *testing.T) {
	f, c := newHoldSourceTest(t)
	c.SetHoldLabels([]string{CanonicalHiveHoldLabel("hive-1")})
	f.comments[18] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	appHeld(f, 18)
	assertHoldSources(t, detectPRHoldSources(t, c, 18, holdSourceT0, "hold", CanonicalHiveHoldLabel("hive-1")),
		HoldSourceLevelGate, HoldSourceDashboardPause)
}

func TestHoldSources_ReporterTrustExtraHoldLabel(t *testing.T) {
	f, c := newHoldSourceTest(t)
	c.SetIssueFilter(clankerFilter(true))
	assertHoldSources(t, detectPRHoldSources(t, c, 19, holdSourceT0, "clanker-requested"), HoldSourceReporterTrust)
	if n := f.readCount(); n != 0 {
		t.Fatalf("a label-only reporter-trust hold needs no check, got %d API calls", n)
	}
}

func TestHoldSources_HoldguardFromLocalRecord(t *testing.T) {
	f, c := newHoldSourceTest(t)
	if err := RecordHoldSource(HoldKindPR, "acme/widget", 20, HoldSourceHoldguard); err != nil {
		t.Fatalf("RecordHoldSource: %v", err)
	}
	appHeld(f, 20)
	assertHoldSources(t, detectPRHoldSources(t, c, 20, holdSourceT0, "hold"), HoldSourceHoldguard)
}

func TestHoldSources_StandbyDonationFromLocalRecord(t *testing.T) {
	f, c := newHoldSourceTest(t)
	if err := RecordHoldSource(HoldKindPR, "Acme/Widget", 21, HoldSourceStandbyDonation); err != nil {
		t.Fatalf("RecordHoldSource: %v", err)
	}
	appHeld(f, 21)
	assertHoldSources(t, detectPRHoldSources(t, c, 21, holdSourceT0, "hold"), HoldSourceStandbyDonation)
}

func TestHoldSources_NoNoticeNoRecordPersonLabelIsPerson(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.events[22] = []map[string]any{holdSourceEvent("labeled", "hold", "alice", holdSourceT0)}
	assertHoldSources(t, detectPRHoldSources(t, c, 22, holdSourceT0, "hold"), HoldSourcePerson)
}

func TestHoldSources_UnattributableHoldIsUnknown(t *testing.T) {
	cases := map[string][]map[string]any{
		"no events":       nil,
		"other bot":       {holdSourceEvent("labeled", "hold", "renovate[bot]", holdSourceT0)},
		"app, no notice":  {holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0)},
		"latest unlabels": {holdSourceEvent("labeled", "hold", "alice", holdSourceT0), holdSourceEvent("unlabeled", "hold", "alice", holdSourceT0.Add(time.Hour))},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			f, c := newHoldSourceTest(t)
			f.events[23] = events
			assertHoldSources(t, detectPRHoldSources(t, c, 23, holdSourceT0, "hold"), HoldSourceUnknown)
		})
	}
}

func TestHoldSources_BlankAppLoginFailsClosedToUnknownWithoutAPICalls(t *testing.T) {
	f, c := newHoldSourceTest(t)
	c.SetAppBotLogin("")
	f.events[24] = []map[string]any{holdSourceEvent("labeled", "hold", "alice", holdSourceT0)}
	assertHoldSources(t, detectPRHoldSources(t, c, 24, holdSourceT0, "hold"), HoldSourceUnknown)
	if n := f.readCount(); n != 0 {
		t.Fatalf("no App identity must mean no check, got %d API calls", n)
	}
}

func TestHoldSources_PersonReHoldOnLevelHeldPRListsPerson(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[25] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	appHeld(f, 25)
	assertHoldSources(t, detectPRHoldSources(t, c, 25, holdSourceT0, "hold"), HoldSourceLevelGate)

	// A person removes and re-applies hold: updated_at moves, the next
	// refresh re-checks and lists person.
	f.events[25] = append(f.events[25],
		holdSourceEvent("unlabeled", "hold", "alice", holdSourceT0.Add(time.Hour)),
		holdSourceEvent("labeled", "hold", "alice", holdSourceT0.Add(2*time.Hour)))
	got := detectPRHoldSources(t, c, 25, holdSourceT0.Add(2*time.Hour), "hold")
	assertHoldSources(t, got, HoldSourceLevelGate, HoldSourcePerson)
}

func TestHoldSources_UnchangedRefreshMakesNoAPICalls(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[26] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	appHeld(f, 26)
	f.events[27] = []map[string]any{holdSourceEvent("labeled", "hold", "alice", holdSourceT0)}

	refresh := func(updated26, updated27 time.Time) ([]string, []string) {
		d := c.newHoldSourceDetector(HoldKindPR, "acme", "widget", time.Now())
		a := d.prSources(t.Context(), 26, updated26, []string{"hold"})
		b := d.prSources(t.Context(), 27, updated27, []string{"hold"})
		d.flush()
		return a, b
	}

	a, b := refresh(holdSourceT0, holdSourceT0)
	assertHoldSources(t, a, HoldSourceLevelGate)
	assertHoldSources(t, b, HoldSourcePerson)
	if f.readCount() == 0 {
		t.Fatal("first refresh must check never-checked held PRs")
	}

	f.resetReads()
	a, b = refresh(holdSourceT0, holdSourceT0)
	assertHoldSources(t, a, HoldSourceLevelGate)
	assertHoldSources(t, b, HoldSourcePerson)
	if n := f.readCount(); n != 0 {
		t.Fatalf("unchanged refresh made %d API calls for hold sources, want 0", n)
	}

	// Only the PR whose updated_at changed is re-checked: comments + events.
	f.resetReads()
	_, _ = refresh(holdSourceT0, holdSourceT0.Add(time.Minute))
	if n := f.readCount(); n != 2 {
		t.Fatalf("changed refresh made %d API calls, want 2 (one PR's comments and events)", n)
	}
}

func TestHoldSources_LocalRecordPrunedWhenNoLongerHeld(t *testing.T) {
	f, c := newHoldSourceTest(t)
	if err := RecordHoldSource(HoldKindPR, "acme/widget", 28, HoldSourceHoldguard); err != nil {
		t.Fatalf("RecordHoldSource: %v", err)
	}
	// A later pass in which #28 was not held drops its record: the detector's
	// start is moved past the record's timestamp instead of sleeping.
	d := c.newHoldSourceDetector(HoldKindPR, "acme", "widget", time.Now().Add(time.Minute))
	d.flush()

	f.events[28] = []map[string]any{holdSourceEvent("labeled", "hold", "alice", holdSourceT0)}
	assertHoldSources(t, detectPRHoldSources(t, c, 28, holdSourceT0, "hold"), HoldSourcePerson)
}

// The prune cut-off is the pass start the caller took before listing: a hold
// recorded after that moment, while the pass was still working from its list
// snapshot, is not in the pass's held set and must not be pruned.
func TestHoldSources_HoldRecordedDuringPassSurvivesPrune(t *testing.T) {
	f, c := newHoldSourceTest(t)
	passStart := time.Now().Add(-time.Minute)
	if err := RecordHoldSource(HoldKindPR, "acme/widget", 29, HoldSourceStandbyDonation); err != nil {
		t.Fatalf("RecordHoldSource: %v", err)
	}
	c.newHoldSourceDetector(HoldKindPR, "acme", "widget", passStart).flush()

	appHeld(f, 29)
	assertHoldSources(t, detectPRHoldSources(t, c, 29, holdSourceT0, "hold"), HoldSourceStandbyDonation)
}

func TestHoldSources_HeldIssueHiveDidNotHoldIsPerson(t *testing.T) {
	f, c := newHoldSourceTest(t)
	assertHoldSources(t, detectIssueHoldSources(t, c, 30, "hold"), HoldSourcePerson)
	if n := f.readCount(); n != 0 {
		t.Fatalf("issue hold sources must not call GitHub, got %d calls", n)
	}
}

func TestHoldSources_SHAHoldIssueFromLocalRecord(t *testing.T) {
	_, c := newHoldSourceTest(t)
	if err := RecordHoldSource(HoldKindIssue, "acme/widget", 31, HoldSourceSHAHold); err != nil {
		t.Fatalf("RecordHoldSource: %v", err)
	}
	assertHoldSources(t, detectIssueHoldSources(t, c, 31, "hold"), HoldSourceSHAHold)
}

func TestHoldSources_DashboardPausedIssue(t *testing.T) {
	_, c := newHoldSourceTest(t)
	c.SetHoldLabels([]string{CanonicalHiveHoldLabel("hive-1")})
	assertHoldSources(t, detectIssueHoldSources(t, c, 32, CanonicalHiveHoldLabel("hive-1")), HoldSourceDashboardPause)
}

func TestHoldSources_SentinelLabelIsNotAHoldSource(t *testing.T) {
	f, c := newHoldSourceTest(t)
	c.SetSentinelAlertLabel("sentinel-alert")
	c.SetHoldLabels([]string{"sentinel-alert"})
	if got := detectPRHoldSources(t, c, 33, holdSourceT0, "sentinel-alert"); got != nil {
		t.Fatalf("hold sources = %v, want none for a sentinel-only hold", got)
	}
	if n := f.readCount(); n != 0 {
		t.Fatalf("sentinel-only hold must not call GitHub, got %d calls", n)
	}
}

// fetchPRs puts the detected sources on both the hold row and the full held
// PR row of the status snapshot.
func TestFetchPRs_HeldRowsCarryHoldSources(t *testing.T) {
	old := HoldSourcesPath
	HoldSourcesPath = filepath.Join(t.TempDir(), HoldSourcesFile)
	t.Cleanup(func() { HoldSourcesPath = old })

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/pulls", prsHandler(t, []wirePR{{
		Number:    188,
		Title:     "fix: gate",
		User:      wireUser{Login: "acme-bot[bot]"},
		Labels:    []wireLabel{{Name: "hold"}},
		CreatedAt: hoursAgo(2),
		UpdatedAt: hoursAgo(1),
		HTMLURL:   "https://github.com/acme/widget/pull/188",
	}}))
	mux.HandleFunc("/repos/acme/widget/issues/188/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))})
	})
	mux.HandleFunc("/repos/acme/widget/issues/188/events", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0)})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"widget"})
	c.SetAppBotLogin(testHiveAppBotLogin)

	_, held, heldPRs, _, _, _, _, err := c.fetchPRs(t.Context(), "widget", nil)
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if len(held) != 1 || len(heldPRs) != 1 {
		t.Fatalf("held=%d heldPRs=%d, want 1 each", len(held), len(heldPRs))
	}
	assertHoldSources(t, held[0].HoldSources, HoldSourceLevelGate)
	assertHoldSources(t, heldPRs[0].HoldSources, HoldSourceLevelGate)
}

// A level-hold notice followed by a hold label from a bot other than the App
// (for example a prow bot answering a maintainer's /hold) is not provably only
// the level gate: the label's origin is unattributed, so `unknown` is listed
// with it.
func TestHoldSources_NonAppBotLatestHoldLabelListsUnknownWithLevelGate(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[16] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	f.events[16] = []map[string]any{
		holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0),
		holdSourceEvent("labeled", "hold", "kubestellar-prow[bot]", holdSourceT0.Add(time.Minute)),
	}
	assertHoldSources(t, detectPRHoldSources(t, c, 16, holdSourceT0, "hold"), HoldSourceLevelGate, HoldSourceUnknown)
}

func TestHoldSources_BlankActorLatestHoldLabelListsUnknownWithLevelGate(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[17] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	f.events[17] = []map[string]any{
		holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0),
		holdSourceEvent("labeled", "hold", "", holdSourceT0.Add(time.Minute)),
	}
	assertHoldSources(t, detectPRHoldSources(t, c, 17, holdSourceT0, "hold"), HoldSourceLevelGate, HoldSourceUnknown)
}

func TestHoldSources_PersonLatestHoldLabelListsPersonWithLevelGate(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[18] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	f.events[18] = []map[string]any{
		holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0),
		holdSourceEvent("labeled", "hold", "alice", holdSourceT0.Add(time.Minute)),
	}
	assertHoldSources(t, detectPRHoldSources(t, c, 18, holdSourceT0, "hold"), HoldSourceLevelGate, HoldSourcePerson)
}

// A check cached before the "other" actor existed recorded an unattributed
// non-App bot hold as no event. It is re-read once instead of trusted, and a
// current check is still reused with no API calls.
func TestHoldSources_LegacyCachedCheckIsReReadOnce(t *testing.T) {
	f, c := newHoldSourceTest(t)
	f.comments[19] = []map[string]any{holdSourceComment(testHiveAppBotLogin, levelHoldNotice("quality"))}
	f.events[19] = []map[string]any{
		holdSourceEvent("labeled", "hold", testHiveAppBotLogin, holdSourceT0),
		{
			"event":      "labeled",
			"label":      map[string]string{"name": "hold"},
			"actor":      map[string]string{"login": "some-automation", "type": "Bot"},
			"created_at": holdSourceT0.Add(time.Minute).UTC().Format(time.RFC3339),
		},
	}
	// Legacy record: same updated_at, no version, latest actor recorded empty.
	holdSourcesMu.Lock()
	l := readHoldSourceLedgerLocked(HoldSourcesPath)
	l.Checks[holdSourceKey(HoldKindPR, "acme/widget", 19)] = holdSourceCheck{
		UpdatedAt: holdSourceT0,
		Notices:   []string{HoldSourceLevelGate},
	}
	if err := writeHoldSourceLedgerLocked(HoldSourcesPath, l); err != nil {
		holdSourcesMu.Unlock()
		t.Fatal(err)
	}
	holdSourcesMu.Unlock()

	assertHoldSources(t, detectPRHoldSources(t, c, 19, holdSourceT0, "hold"), HoldSourceLevelGate, HoldSourceUnknown)
	if f.readCount() == 0 {
		t.Fatal("legacy cached check was trusted without re-reading GitHub")
	}
	f.resetReads()
	assertHoldSources(t, detectPRHoldSources(t, c, 19, holdSourceT0, "hold"), HoldSourceLevelGate, HoldSourceUnknown)
	if n := f.readCount(); n != 0 {
		t.Fatalf("unchanged refresh of a current check made %d API reads, want 0", n)
	}
}
