package github

// hivecommons/hive#18468: the hive never applies `hold` itself; its gates use
// ready-for-review / decision-needed, which block exactly like `hold`; a
// person's `hold` still blocks everything and is never touched by the hive.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestHiveGateLabelsBlockExactlyLikeHold(t *testing.T) {
	for _, labels := range [][]string{
		{"hold"},
		{DefaultReadyForReviewLabel},
		{DefaultDecisionNeededLabel},
		{"kind/bug", "Ready-For-Review"},
	} {
		if !HasHoldLabel(labels) {
			t.Errorf("HasHoldLabel(%v) = false, want held", labels)
		}
		var nilClient *Client
		if !nilClient.isHeld(labels) {
			t.Errorf("isHeld(%v) = false, want held", labels)
		}
	}
	for _, labels := range [][]string{
		{"not-ready-for-review"},
		{"decision-needed-later"},
		{"kind/bug"},
	} {
		if HasHiveGateLabel(labels) {
			t.Errorf("HasHiveGateLabel(%v) = true; gate labels match exactly", labels)
		}
	}
}

func TestSetHiveGateLabelNamesNeverAcceptsHold(t *testing.T) {
	t.Cleanup(func() { SetHiveGateLabelNames("", "") })
	SetHiveGateLabelNames("hold", " HOLD ")
	if ReadyForReviewLabelName() != DefaultReadyForReviewLabel || DecisionNeededLabelName() != DefaultDecisionNeededLabel {
		t.Fatalf("names = %q/%q, want the defaults: the hive must never apply hold", ReadyForReviewLabelName(), DecisionNeededLabelName())
	}
	SetHiveGateLabelNames("awaiting-review", "awaiting-decision")
	if !HasHoldLabel([]string{"awaiting-review"}) || !HasHoldLabel([]string{"awaiting-decision"}) {
		t.Fatal("configured gate labels must block like hold")
	}
	if !HasHoldLabel([]string{DefaultReadyForReviewLabel}) {
		t.Fatal("a default-named gate label applied before a rename must keep blocking")
	}
	if got := reporterTrustHoldLabels(); got[0] != "awaiting-decision" {
		t.Fatalf("reporter-trust labels = %v, want the configured decision label", got)
	}
}

func TestHiveGateLabelsAreReservedFromAgents(t *testing.T) {
	for _, label := range []string{"hold", DefaultReadyForReviewLabel, DefaultDecisionNeededLabel} {
		if !reservedLabelForAgents(label, "lgtm") {
			t.Errorf("reservedLabelForAgents(%q) = false; agents must not set or lift it", label)
		}
	}
}

func TestHiveGateLabelDescriptionsSayWhoActsNext(t *testing.T) {
	for _, label := range HiveGateLabels() {
		_, desc, ok := hiveGateLabelDefinition(label)
		if !ok || !strings.Contains(desc, "maintainer") {
			t.Errorf("%s description = %q, want it to name the maintainer as the next actor", label, desc)
		}
		if len([]rune(desc)) > githubLabelDescriptionMax {
			t.Errorf("%s description is %d runes, over GitHub's %d", label, len([]rune(desc)), githubLabelDescriptionMax)
		}
	}
}

// A person's `hold` on a PR that also carries an App level-hold notice is not
// a pending level hold: only ready-for-review is ever released.
func TestPendingLevelHoldsIgnoresAPersonsHold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 11, "title": "fix", "body": "safe change",
				"labels": []map[string]string{{"name": "hold"}},
			}})
		default:
			t.Fatalf("unexpected request: %s %s (a held-only PR must not be inspected for release)", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})
	pending, err := c.PendingLevelHolds(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("PendingLevelHolds = (%v, %v), want none", pending, err)
	}
}

// migrationFake serves one repository's held items for MigrateHiveAppliedHolds.
type migrationFake struct {
	mu       sync.Mutex
	items    map[int]migrationItem
	added    map[int][]string
	removed  map[int][]string
	comments map[int][]string
}

type migrationItem struct {
	holdActor string
	comments  []map[string]any
}

func appComment(body string) map[string]any {
	return map[string]any{"body": body, "user": map[string]string{"login": testHiveAppBotLogin}, "created_at": "2026-11-01T12:00:05Z"}
}

func (f *migrationFake) serve(t *testing.T) *httptest.Server {
	t.Helper()
	f.added, f.removed, f.comments = map[int][]string{}, map[int][]string{}, map[int][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/labels/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"name": strings.TrimPrefix(path, "/repos/acme/widget/labels/")})
			return
		case r.Method == http.MethodGet && path == "/repos/acme/widget/issues":
			if r.URL.Query().Get("labels") != "hold" {
				t.Errorf("listing labels = %q, want hold", r.URL.Query().Get("labels"))
			}
			out := []map[string]any{}
			for n := range f.items {
				out = append(out, map[string]any{"number": n, "labels": []map[string]string{{"name": "hold"}}})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		rest := strings.TrimPrefix(path, "/repos/acme/widget/issues/")
		numStr, tail, _ := strings.Cut(rest, "/")
		n, err := strconv.Atoi(numStr)
		item, ok := f.items[n]
		if err != nil || !ok {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		switch {
		case r.Method == http.MethodGet && tail == "events":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event": "labeled", "created_at": "2026-11-01T12:00:00Z",
				"actor": map[string]string{"login": item.holdActor}, "label": map[string]string{"name": "hold"},
			}})
		case r.Method == http.MethodGet && tail == "comments":
			_ = json.NewEncoder(w).Encode(item.comments)
		case r.Method == http.MethodPost && tail == "comments":
			var c map[string]string
			_ = json.NewDecoder(r.Body).Decode(&c)
			f.comments[n] = append(f.comments[n], c["body"])
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && tail == "labels":
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			f.added[n] = append(f.added[n], labels...)
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == http.MethodDelete && strings.HasPrefix(tail, "labels/"):
			if len(f.added[n]) == 0 {
				t.Errorf("#%d: hold removed before the gate label was added", n)
			}
			f.removed[n] = append(f.removed[n], strings.TrimPrefix(tail, "labels/"))
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMigrateHiveAppliedHoldsOnlyTouchesHiveAppliedHolds(t *testing.T) {
	const (
		levelHeld    = 1
		reporterHeld = 2
		humanHeld    = 3
		unattributed = 4
		humanOverApp = 5
	)
	f := &migrationFake{items: map[int]migrationItem{
		levelHeld:    {holdActor: testHiveAppBotLogin, comments: []map[string]any{appComment(levelHoldNotice("quality"))}},
		reporterHeld: {holdActor: testHiveAppBotLogin, comments: []map[string]any{appComment(levelHoldNotice("quality")), appComment(ReporterTrustNoticeBody(ReporterTrust{Held: true, Issue: 9, Repo: "acme/widget", Reporter: "stranger"}))}},
		humanHeld:    {holdActor: "alice", comments: []map[string]any{appComment(levelHoldNotice("quality"))}},
		unattributed: {holdActor: testHiveAppBotLogin, comments: []map[string]any{appComment("routine status")}},
		// A person's comment that quotes the notice is not provenance.
		humanOverApp: {holdActor: testHiveAppBotLogin, comments: []map[string]any{{"body": levelHoldNotice("quality"), "user": map[string]string{"login": "mallory"}}}},
	}}
	srv := f.serve(t)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})

	res, err := c.MigrateHiveAppliedHolds(context.Background())
	if err != nil {
		t.Fatalf("MigrateHiveAppliedHolds: %v", err)
	}
	if res.Relabeled != 2 || res.Skipped != 3 {
		t.Fatalf("result = %+v, want 2 relabeled, 3 left alone", res)
	}
	if got := f.added[levelHeld]; len(got) != 1 || got[0] != DefaultReadyForReviewLabel {
		t.Errorf("level hold added %v, want [ready-for-review]", got)
	}
	if got := f.added[reporterHeld]; len(got) != 1 || got[0] != DefaultDecisionNeededLabel {
		t.Errorf("reporter-trust hold added %v, want [decision-needed]", got)
	}
	for _, n := range []int{levelHeld, reporterHeld} {
		if got := f.removed[n]; len(got) != 1 || got[0] != "hold" {
			t.Errorf("#%d removed %v, want [hold]", n, got)
		}
		if got := f.comments[n]; len(got) != 1 || !strings.Contains(got[0], hiveGateRelabelMarker) || !strings.Contains(got[0], "hands-off") {
			t.Errorf("#%d comments %v, want one relabel explanation", n, got)
		}
	}
	for _, n := range []int{humanHeld, unattributed, humanOverApp} {
		if len(f.added[n]) != 0 || len(f.removed[n]) != 0 || len(f.comments[n]) != 0 {
			t.Errorf("#%d was touched (added=%v removed=%v comments=%v); a hold the hive cannot prove it applied is hands-off", n, f.added[n], f.removed[n], f.comments[n])
		}
	}
	for _, label := range f.added[levelHeld] {
		if label == "hold" {
			t.Fatal("the migration applied hold")
		}
	}
}

func TestMigrateHiveAppliedHoldsWithoutAppLoginTouchesNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request without an App login: %s %s", r.Method, r.URL.String())
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetRepos([]string{"acme/widget"})
	if res, err := c.MigrateHiveAppliedHolds(context.Background()); err != nil || res.Relabeled != 0 {
		t.Fatalf("(%+v, %v), want a no-op", res, err)
	}
}
