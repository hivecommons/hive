package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hasKind reports whether evs contains an event of the given kind.
func hasKind(evs []TimelineEvent, kind string) bool {
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func kindsOf(evs []TimelineEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestDetectTransitions(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	// A baseline "steady state" entry. Cases mutate a copy of it.
	base := func() RegistryEntry {
		return RegistryEntry{
			ID:         "hosted-demo",
			Online:     true,
			GitHash:    "aaaaaaa",
			GitBranch:  "v2",
			Version:    "1.0.0",
			ACMMLevel:  3,
			AgentCount: 4,
			StartedAt:  now.Add(-2 * time.Hour).Format(time.RFC3339),
			Health:     map[string]any{"status": "ok", "disk": true},
		}
	}

	tests := []struct {
		name      string
		mutate    func(old, new *RegistryEntry)
		wantKinds []string
		wantNone  bool
		detailHas string
	}{
		{
			name:     "identical entries emit nothing",
			mutate:   func(old, new *RegistryEntry) {},
			wantNone: true,
		},
		{
			name: "unchanged SHA with new heartbeat emits nothing",
			mutate: func(old, new *RegistryEntry) {
				new.LastHeartbeat = now.Format(time.RFC3339)
				new.TotalTokens24h = 12345 // noisy field, deliberately not watched
			},
			wantNone: true,
		},
		{
			name: "version change",
			mutate: func(old, new *RegistryEntry) {
				new.GitHash = "bbbbbbb"
				new.Version = "1.1.0"
			},
			wantKinds: []string{TimelineVersionChanged},
			detailHas: "aaaaaaa → bbbbbbb",
		},
		{
			name: "same commit at different SHA lengths is not a change",
			mutate: func(old, new *RegistryEntry) {
				new.GitHash = "aaaaaaa1234567"
			},
			wantNone: true,
		},
		{
			name: "branch switch",
			mutate: func(old, new *RegistryEntry) {
				new.GitBranch = "v3"
			},
			wantKinds: []string{TimelineBranchChanged},
			detailHas: "v2 → v3",
		},
		{
			name: "came back online",
			mutate: func(old, new *RegistryEntry) {
				old.Online = false
			},
			wantKinds: []string{TimelineCameOnline},
		},
		{
			name: "health degraded names the flipped check",
			mutate: func(old, new *RegistryEntry) {
				new.Health = map[string]any{"status": "degraded", "disk": false}
			},
			wantKinds: []string{TimelineHealthChanged},
			detailHas: "changed: disk",
		},
		{
			name: "health check flip without status change emits nothing",
			mutate: func(old, new *RegistryEntry) {
				new.Health = map[string]any{"status": "ok", "disk": false}
			},
			wantNone: true,
		},
		{
			name: "upgrade started",
			mutate: func(old, new *RegistryEntry) {
				new.Upgrading = true
				new.UpgradeTarget = "ccccccc"
			},
			wantKinds: []string{TimelineUpgradeStarted},
			detailHas: "ccccccc",
		},
		{
			name: "upgrade completed at new SHA",
			mutate: func(old, new *RegistryEntry) {
				old.Upgrading = true
				old.UpgradeTarget = "ccccccc"
				new.GitHash = "ccccccc"
			},
			wantKinds: []string{TimelineVersionChanged, TimelineUpgradeCompleted},
			detailHas: "upgrade completed at ccccccc",
		},
		{
			name: "upgrade stale fires once at the crossing",
			mutate: func(old, new *RegistryEntry) {
				old.Upgrading = true
				old.UpgradeTarget = "ccccccc"
				old.UpgradeStartedAt = now.Add(-timelineUpgradeStaleAfter - time.Minute)
				new.Upgrading = true
				new.UpgradeTarget = "ccccccc"
			},
			wantKinds: []string{TimelineUpgradeStale},
			detailHas: "in flight",
		},
		{
			name: "upgrade long past stale does not re-fire",
			mutate: func(old, new *RegistryEntry) {
				old.Upgrading = true
				old.UpgradeTarget = "ccccccc"
				// Well past the crossing window — already reported.
				old.UpgradeStartedAt = now.Add(-timelineUpgradeStaleAfter - 10*heartbeatStalePollWindow)
				new.Upgrading = true
				new.UpgradeTarget = "ccccccc"
			},
			wantNone: true,
		},
		{
			name: "upgrade in flight but not yet stale",
			mutate: func(old, new *RegistryEntry) {
				old.Upgrading = true
				old.UpgradeTarget = "ccccccc"
				old.UpgradeStartedAt = now.Add(-time.Minute)
				new.Upgrading = true
				new.UpgradeTarget = "ccccccc"
			},
			wantNone: true,
		},
		{
			name: "restart detected when StartedAt moves forward",
			mutate: func(old, new *RegistryEntry) {
				new.StartedAt = now.Add(-1 * time.Minute).Format(time.RFC3339)
			},
			wantKinds: []string{TimelineRestarted},
			detailHas: "process restarted",
		},
		{
			name: "StartedAt jitter within slack is not a restart",
			mutate: func(old, new *RegistryEntry) {
				oldT, _ := time.Parse(time.RFC3339, old.StartedAt)
				new.StartedAt = oldT.Add(timelineRestartSlack / 2).Format(time.RFC3339)
			},
			wantNone: true,
		},
		{
			name: "ACMM level change",
			mutate: func(old, new *RegistryEntry) {
				new.ACMMLevel = 4
			},
			wantKinds: []string{TimelineACMMChanged},
			detailHas: "3 → 4",
		},
		{
			name: "agent count moves materially",
			mutate: func(old, new *RegistryEntry) {
				new.AgentCount = old.AgentCount + timelineAgentCountDelta
			},
			wantKinds: []string{TimelineAgentsChanged},
		},
		{
			name: "agent count jitter below threshold is ignored",
			mutate: func(old, new *RegistryEntry) {
				new.AgentCount = old.AgentCount + timelineAgentCountDelta - 1
			},
			wantNone: true,
		},
		{
			name: "github app permission issue appears",
			mutate: func(old, new *RegistryEntry) {
				new.GitHubAppPermIssue = "missing contents:write"
			},
			wantKinds: []string{TimelineGitHubAppChanged},
			detailHas: "missing contents:write",
		},
		{
			name: "github app permission issue clears",
			mutate: func(old, new *RegistryEntry) {
				old.GitHubAppPermIssue = "missing contents:write"
			},
			wantKinds: []string{TimelineGitHubAppChanged},
			detailHas: "cleared",
		},
		{
			name: "github app install completes",
			mutate: func(old, new *RegistryEntry) {
				old.PendingGitHubAppInstall = true
			},
			wantKinds: []string{TimelineGitHubAppChanged},
			detailHas: "install completed",
		},
		{
			name: "several transitions on one beat",
			mutate: func(old, new *RegistryEntry) {
				old.Online = false
				new.GitHash = "ddddddd"
				new.ACMMLevel = 5
			},
			wantKinds: []string{TimelineCameOnline, TimelineVersionChanged, TimelineACMMChanged},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old, new := base(), base()
			tc.mutate(&old, &new)
			got := detectTransitions(&old, &new, now)

			if tc.wantNone {
				if len(got) != 0 {
					t.Fatalf("expected no events, got %v", kindsOf(got))
				}
				return
			}
			for _, want := range tc.wantKinds {
				if !hasKind(got, want) {
					t.Errorf("missing event kind %q; got %v", want, kindsOf(got))
				}
			}
			if tc.detailHas != "" {
				found := false
				for _, e := range got {
					if strings.Contains(e.Detail, tc.detailHas) {
						found = true
					}
				}
				if !found {
					t.Errorf("no event detail contained %q; got %+v", tc.detailHas, got)
				}
			}
		})
	}
}

func TestDetectTransitionsNilSafe(t *testing.T) {
	e := &RegistryEntry{ID: "x"}
	if got := detectTransitions(nil, e, time.Now()); got != nil {
		t.Errorf("nil old should yield nil, got %v", kindsOf(got))
	}
	if got := detectTransitions(e, nil, time.Now()); got != nil {
		t.Errorf("nil new should yield nil, got %v", kindsOf(got))
	}
	if got := detectTransitions(nil, nil, time.Now()); got != nil {
		t.Errorf("both nil should yield nil, got %v", kindsOf(got))
	}
}

func TestFlippedHealthChecksNilSafe(t *testing.T) {
	if got := flippedHealthChecks(nil, map[string]any{"a": true}); got != nil {
		t.Errorf("nil old health should yield nil, got %v", got)
	}
	if got := flippedHealthChecks(map[string]any{"a": true}, nil); got != nil {
		t.Errorf("nil new health should yield nil, got %v", got)
	}
	// Deterministic ordering: map iteration order must not leak into details.
	oldH := map[string]any{"status": "ok", "zeta": true, "alpha": true, "mid": "up"}
	newH := map[string]any{"status": "bad", "zeta": false, "alpha": false, "mid": "down"}
	got := flippedHealthChecks(oldH, newH)
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted)", got, want)
		}
	}
}

func TestTimelineRetentionPruning(t *testing.T) {
	tests := []struct {
		name       string
		appendN    int
		wantStored int
		// wantOldest is the detail of the oldest surviving event.
		wantOldest string
		wantNewest string
	}{
		{
			name:       "under the cap keeps everything",
			appendN:    10,
			wantStored: 10,
			wantOldest: "event-0",
			wantNewest: "event-9",
		},
		{
			name:       "exactly at the cap keeps everything",
			appendN:    timelineMaxEvents,
			wantStored: timelineMaxEvents,
			wantOldest: "event-0",
			wantNewest: fmt.Sprintf("event-%d", timelineMaxEvents-1),
		},
		{
			name:       "over the cap prunes oldest first",
			appendN:    timelineMaxEvents + 50,
			wantStored: timelineMaxEvents,
			wantOldest: "event-50",
			wantNewest: fmt.Sprintf("event-%d", timelineMaxEvents+49),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			origDir := timelineDir
			timelineDir = dir
			defer func() { timelineDir = origDir }()

			ts := newTimelineStore()
			for i := 0; i < tc.appendN; i++ {
				ts.append("hosted-prune", TimelineVersionChanged, fmt.Sprintf("event-%d", i), "")
			}

			// recent() returns newest first.
			got := ts.recent("hosted-prune", timelineMaxEvents*2)
			if len(got) != tc.wantStored {
				t.Fatalf("stored %d events, want %d", len(got), tc.wantStored)
			}
			if got[0].Detail != tc.wantNewest {
				t.Errorf("newest = %q, want %q", got[0].Detail, tc.wantNewest)
			}
			if got[len(got)-1].Detail != tc.wantOldest {
				t.Errorf("oldest = %q, want %q", got[len(got)-1].Detail, tc.wantOldest)
			}

			// The persisted file must respect the same cap — this is the PVC
			// growth bound, not just an in-memory one.
			data, err := os.ReadFile(filepath.Join(dir, "hosted-prune.json"))
			if err != nil {
				t.Fatalf("timeline not persisted: %v", err)
			}
			var tl hiveTimeline
			if err := json.Unmarshal(data, &tl); err != nil {
				t.Fatalf("persisted timeline is not valid JSON: %v", err)
			}
			if len(tl.Events) != tc.wantStored {
				t.Errorf("persisted %d events, want %d", len(tl.Events), tc.wantStored)
			}
		})
	}
}

func TestTimelineAppendAllPrunes(t *testing.T) {
	dir := t.TempDir()
	origDir := timelineDir
	timelineDir = dir
	defer func() { timelineDir = origDir }()

	ts := newTimelineStore()
	batch := make([]TimelineEvent, 0, timelineMaxEvents+20)
	for i := 0; i < timelineMaxEvents+20; i++ {
		batch = append(batch, TimelineEvent{Kind: TimelineACMMChanged, Detail: fmt.Sprintf("e-%d", i)})
	}
	ts.appendAll("hosted-batch", batch)

	got := ts.recent("hosted-batch", timelineMaxEvents*2)
	if len(got) != timelineMaxEvents {
		t.Fatalf("stored %d, want cap %d", len(got), timelineMaxEvents)
	}
	if got[len(got)-1].Detail != "e-20" {
		t.Errorf("oldest surviving = %q, want e-20", got[len(got)-1].Detail)
	}
	// Empty kinds must be skipped, not stored blank.
	ts.appendAll("hosted-batch2", []TimelineEvent{{Kind: "", Detail: "ignored"}})
	if n := len(ts.recent("hosted-batch2", 10)); n != 0 {
		t.Errorf("empty-kind event was stored (%d events)", n)
	}
}

func TestTimelinePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origDir := timelineDir
	timelineDir = dir
	defer func() { timelineDir = origDir }()

	ts := newTimelineStore()
	ts.append("hosted-rt", TimelineRestarted, "process restarted", "")
	ts.append("hosted-rt", TimelineHealthChanged, "health ok → degraded", "")

	// A fresh store loading from the same directory must see both events.
	ts2 := newTimelineStore()
	ts2.load()
	got := ts2.recent("hosted-rt", timelineMaxEvents)
	if len(got) != 2 {
		t.Fatalf("reloaded %d events, want 2", len(got))
	}
	if got[0].Kind != TimelineHealthChanged {
		t.Errorf("newest reloaded = %q, want %q", got[0].Kind, TimelineHealthChanged)
	}
	// No .tmp files should survive an atomic write.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestTimelinePathRejectsTraversal(t *testing.T) {
	tests := []struct {
		name   string
		hiveID string
		want   bool // want a non-empty path
	}{
		{"valid id", "hosted-demo-01", true},
		{"valid with dots", "hosted.demo.01", true},
		{"empty", "", false},
		{"parent traversal", "..", false},
		{"embedded traversal", "../../etc/passwd", false},
		{"forward slash", "a/b", false},
		{"backslash", `a\b`, false},
		{"null-ish junk", "a\x00b", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := timelinePath(tc.hiveID)
			if (got != "") != tc.want {
				t.Errorf("timelinePath(%q) = %q, want non-empty=%v", tc.hiveID, got, tc.want)
			}
		})
	}
}

func TestTimelineTruncatesDetail(t *testing.T) {
	dir := t.TempDir()
	origDir := timelineDir
	timelineDir = dir
	defer func() { timelineDir = origDir }()

	ts := newTimelineStore()
	long := strings.Repeat("x", timelineMaxDetailRunes*3)
	ts.append("hosted-long", TimelineHealthChanged, long, long)

	got := ts.recent("hosted-long", 1)
	if len(got) != 1 {
		t.Fatal("expected one event")
	}
	if r := []rune(got[0].Detail); len(r) > timelineMaxDetailRunes+1 {
		t.Errorf("detail not truncated: %d runes", len(r))
	}
	if r := []rune(got[0].Actor); len(r) > timelineMaxDetailRunes+1 {
		t.Errorf("actor not truncated: %d runes", len(r))
	}
}

func TestTimelineConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	origDir := timelineDir
	timelineDir = dir
	defer func() { timelineDir = origDir }()

	ts := newTimelineStore()
	const goroutines = 16
	const perGoroutine = 20

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ts.append("hosted-race", TimelineVersionChanged, fmt.Sprintf("g%d-%d", g, i), "")
				ts.recent("hosted-race", 10)
			}
		}(g)
	}
	wg.Wait()

	got := ts.recent("hosted-race", timelineMaxEvents*2)
	if len(got) != timelineMaxEvents {
		t.Errorf("got %d events, want cap %d", len(got), timelineMaxEvents)
	}
}

func TestHandleHiveTimelineAuthorization(t *testing.T) {
	dir := t.TempDir()
	origTimelineDir, origHivesDir, origUsersDir := timelineDir, saasHivesDir, saasUsersDir
	timelineDir = filepath.Join(dir, "timeline")
	saasHivesDir = filepath.Join(dir, "hives")
	saasUsersDir = filepath.Join(dir, "users")
	defer func() {
		timelineDir, saasHivesDir, saasUsersDir = origTimelineDir, origHivesDir, origUsersDir
	}()

	const hiveID = "hosted-authz"
	const owner = "alice"
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: owner, Status: "running"}); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	// getAuthUser only trusts the session cookie when a SaaS user record
	// exists, so the identities under test must be real users.
	for _, u := range []string{owner, hubAdminUsername, "mallory"} {
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: u, Hives: map[string]string{}}); err != nil {
			t.Fatalf("saveSaaSUser(%s): %v", u, err)
		}
	}

	s := &HubServer{
		logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		timeline: newTimelineStore(),
	}
	s.timeline.append(hiveID, TimelineRestarted, "process restarted", "")

	tests := []struct {
		name     string
		hiveID   string
		user     string
		wantCode int
	}{
		{"owner may read", hiveID, owner, http.StatusOK},
		{"hub admin may read", hiveID, hubAdminUsername, http.StatusOK},
		{"unrelated user is forbidden", hiveID, "mallory", http.StatusForbidden},
		{"anonymous is forbidden", hiveID, "", http.StatusForbidden},
		{"unknown hive is not found", "hosted-nope", owner, http.StatusNotFound},
		{"unknown hive for admin is not found", "hosted-nope", hubAdminUsername, http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/saas/hives/"+tc.hiveID+"/timeline", nil)
			req.SetPathValue("id", tc.hiveID)
			if tc.user != "" {
				req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: tc.user})
			}
			rec := httptest.NewRecorder()
			s.handleHiveTimeline(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			var body struct {
				Events []TimelineEvent `json:"events"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("bad JSON: %v", err)
			}
			if len(body.Events) != 1 || body.Events[0].Kind != TimelineRestarted {
				t.Errorf("unexpected events: %+v", body.Events)
			}
		})
	}
}

func TestRecordHeartbeatTransitionsNilStore(t *testing.T) {
	// A HubServer without a timeline store (older constructor paths, tests)
	// must not panic — the timeline is strictly additive.
	s := &HubServer{}
	s.recordHeartbeatTransitions(&RegistryEntry{ID: "x"}, &RegistryEntry{ID: "x", GitHash: "b"})
	s.recordTimeline("x", TimelineAccess, "detail", "actor")
	s.flushOfflineEvents([]offlineSweepEvent{{HiveID: "x", Detail: "d"}})
}

func TestMarkStaleHivesReportsOfflineOnce(t *testing.T) {
	s := &HubServer{
		logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		timeline: newTimelineStore(),
	}
	stale := time.Now().Add(-maxHeartbeatAge - time.Minute).UTC().Format(time.RFC3339)
	s.registry.Hives = []RegistryEntry{
		{ID: "hosted-a", Online: true, LastHeartbeat: stale},
		{ID: "hosted-b", Online: true, LastHeartbeat: time.Now().UTC().Format(time.RFC3339)},
	}

	got := s.markStaleHives()
	if len(got) != 1 || got[0].HiveID != "hosted-a" {
		t.Fatalf("expected one offline event for hosted-a, got %+v", got)
	}

	// A second sweep must NOT re-report: the hive is already Online=false.
	if got2 := s.markStaleHives(); len(got2) != 0 {
		t.Errorf("offline re-reported on second sweep: %+v", got2)
	}
}

func TestShortDur(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
		{-3 * time.Hour, "3h"},
	}
	for _, tc := range tests {
		if got := shortDur(tc.d); got != tc.want {
			t.Errorf("shortDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
