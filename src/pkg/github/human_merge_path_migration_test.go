package github

// The boot migration swaps, once, the `hold` the human-merge-path rule
// applied before it used needs-owner-merge, and only on proof: the rule's
// own legacy notice, every `hold` event the App's, nothing else holding the
// PR. A `hold` a person set or touched is never changed.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

type pathMigrationItem struct {
	issue    bool
	author   string
	labels   []string
	events   []map[string]any
	comments []map[string]any
	// head and files are served for GET pulls/N and pulls/N/files.
	head  string
	files []string
	// liftOnly marks an item whose stale `hold` is lifted with no label
	// added, so removing it first is expected.
	liftOnly bool
}

type pathMigrationFake struct {
	mu       sync.Mutex
	items    map[int]pathMigrationItem
	added    map[int][]string
	removed  map[int][]string
	comments map[int][]string
}

func pathMigrationEvent(kind, actor, at string) map[string]any {
	return map[string]any{"event": kind, "created_at": at, "actor": map[string]string{"login": actor}, "label": map[string]string{"name": "hold"}}
}

func (f *pathMigrationFake) serve(t *testing.T) *httptest.Server {
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
			out := []map[string]any{}
			for n, item := range f.items {
				labels := []map[string]string{{"name": "hold"}}
				for _, l := range item.labels {
					labels = append(labels, map[string]string{"name": l})
				}
				author := item.author
				if author == "" {
					author = "renovate[bot]"
				}
				row := map[string]any{"number": n, "labels": labels, "user": map[string]string{"login": author}}
				if !item.issue {
					row["pull_request"] = map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/" + strconv.Itoa(n)}
				}
				out = append(out, row)
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/pulls/"):
			numStr, tail, _ := strings.Cut(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), "/")
			n, _ := strconv.Atoi(numStr)
			item := f.items[n]
			if tail == "files" {
				var out []map[string]any
				for _, name := range item.files {
					out = append(out, map[string]any{"filename": name, "status": "modified"})
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "head": map[string]string{"sha": item.head}, "changed_files": len(item.files)})
			return
		}
		rest := strings.TrimPrefix(path, "/repos/acme/widget/issues/")
		numStr, tail, _ := strings.Cut(rest, "/")
		n, err := strconv.Atoi(numStr)
		item, ok := f.items[n]
		if err != nil || !ok {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && tail == "events":
			_ = json.NewEncoder(w).Encode(item.events)
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
			if len(f.added[n]) == 0 && !item.liftOnly {
				t.Errorf("#%d: hold removed before the label was added", n)
			}
			f.removed[n] = append(f.removed[n], strings.TrimPrefix(tail, "labels/"))
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMigrateHiveAppliedHoldsSwapsTheHumanMergePathHoldOnce(t *testing.T) {
	old := HoldSourcesPath
	HoldSourcesPath = filepath.Join(t.TempDir(), HoldSourcesFile)
	t.Cleanup(func() { HoldSourcesPath = old })

	legacy := legacyHumanMergePathNotice(".github/workflows/ci.yml")
	appOnly := []map[string]any{pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-01T12:00:00Z")}
	const (
		proven         = 1
		reHeldByApp    = 2
		personTouched  = 3
		personHold     = 4
		newNotice      = 5
		alreadySwapped = 6
		secondHold     = 7
		issue          = 8
		quotedNotice   = 9
	)
	f := &pathMigrationFake{items: map[int]pathMigrationItem{
		proven: {events: appOnly, comments: []map[string]any{appComment(legacy)}},
		reHeldByApp: {events: []map[string]any{
			pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-01T12:00:00Z"),
			pathMigrationEvent("unlabeled", testHiveAppBotLogin, "2026-11-02T12:00:00Z"),
			pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-03T12:00:00Z"),
		}, comments: []map[string]any{appComment(legacy)}},
		personTouched: {events: []map[string]any{
			pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-01T12:00:00Z"),
			pathMigrationEvent("unlabeled", "alice", "2026-11-02T12:00:00Z"),
			pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-03T12:00:00Z"),
		}, comments: []map[string]any{appComment(legacy)}},
		personHold:     {events: []map[string]any{pathMigrationEvent("labeled", "alice", "2026-11-01T12:00:00Z")}, comments: []map[string]any{appComment(legacy)}},
		newNotice:      {events: appOnly, comments: []map[string]any{appComment(HumanMergePathComment([]string{"a.yml"}))}},
		alreadySwapped: {events: appOnly, comments: []map[string]any{appComment(legacy), appComment(HumanMergePathRelabelMarker + "\nswapped")}},
		secondHold:     {labels: []string{"on-hold"}, events: appOnly, comments: []map[string]any{appComment(legacy)}},
		issue:          {issue: true, events: appOnly, comments: []map[string]any{appComment(legacy)}},
		// A person's comment that quotes the notice is not provenance.
		quotedNotice: {events: appOnly, comments: []map[string]any{{"body": legacy, "user": map[string]string{"login": "mallory"}}}},
	}}
	srv := f.serve(t)
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})

	res, err := c.MigrateHiveAppliedHolds(context.Background())
	if err != nil {
		t.Fatalf("MigrateHiveAppliedHolds: %v", err)
	}
	// No hive_id (no canonical hold label): the dashboard and admin holds
	// write `hold` as the App too, so an App re-hold after a lift may be a
	// person's and stays.
	if res.Relabeled != 1 || res.Skipped != 8 {
		t.Fatalf("result = %+v, want 1 relabeled, 8 left alone", res)
	}
	for _, n := range []int{proven} {
		if got := f.added[n]; len(got) != 1 || got[0] != HumanMergePathLabel {
			t.Errorf("#%d added %v, want [%s]", n, got, HumanMergePathLabel)
		}
		if got := f.removed[n]; len(got) != 1 || got[0] != "hold" {
			t.Errorf("#%d removed %v, want [hold]", n, got)
		}
		if got := f.comments[n]; len(got) != 1 || !strings.Contains(got[0], HumanMergePathRelabelMarker) || !strings.Contains(got[0], "not a hold") {
			t.Errorf("#%d comments %v, want one swap explanation", n, got)
		}
	}
	for _, n := range []int{reHeldByApp, personTouched, personHold, newNotice, alreadySwapped, secondHold, issue, quotedNotice} {
		if len(f.added[n]) != 0 || len(f.removed[n]) != 0 || len(f.comments[n]) != 0 {
			t.Errorf("#%d was touched (added=%v removed=%v comments=%v); a hold the hive cannot prove the rule applied is hands-off", n, f.added[n], f.removed[n], f.comments[n])
		}
	}
}

func pathMigrationClient(t *testing.T, f *pathMigrationFake, logs io.Writer) *Client {
	t.Helper()
	old := HoldSourcesPath
	HoldSourcesPath = filepath.Join(t.TempDir(), HoldSourcesFile)
	t.Cleanup(func() { HoldSourcesPath = old })
	srv := f.serve(t)
	if logs == nil {
		logs = io.Discard
	}
	c := NewClientForTest(srv.URL, "acme", nil, slog.New(slog.NewTextHandler(logs, nil)))
	c.SetAppBotLogin(testHiveAppBotLogin)
	c.SetRepos([]string{"acme/widget"})
	return c
}

// An App hold that was lifted and put back is the rule's re-hold only on a
// hive whose own hold label is hive-pause/<id>: there the dashboard and admin
// holds never write `hold`. Where that label is `hold`, or unknown, the
// history must be the rule's single application.
func TestMigrateHumanMergePathHoldReHoldNeedsAHiveID(t *testing.T) {
	legacy := legacyHumanMergePathNotice(".github/workflows/ci.yml")
	reHeld := []map[string]any{
		pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-01T12:00:00Z"),
		pathMigrationEvent("unlabeled", testHiveAppBotLogin, "2026-11-02T12:00:00Z"),
		pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-03T12:00:00Z"),
	}
	for _, tc := range []struct {
		name      string
		holdLabel string
		swapped   bool
	}{
		{name: "hive_id set", holdLabel: CanonicalHiveHoldLabel("hive-1"), swapped: true},
		{name: "no hive_id", holdLabel: CanonicalHiveHoldLabel("")},
		{name: "hold label unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &pathMigrationFake{items: map[int]pathMigrationItem{1: {events: reHeld, comments: []map[string]any{appComment(legacy)}}}}
			c := pathMigrationClient(t, f, nil)
			if tc.holdLabel != "" {
				c.SetCanonicalHoldLabel(tc.holdLabel)
			}
			if _, err := c.MigrateHiveAppliedHolds(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(f.added[1]) == 1 && f.added[1][0] == HumanMergePathLabel && len(f.removed[1]) == 1; got != tc.swapped {
				t.Fatalf("swapped = %v (added=%v removed=%v), want %v", got, f.added[1], f.removed[1], tc.swapped)
			}
		})
	}
}

// A legacy hold whose PR's current head no longer touches a listed path has
// no reason left: the migration lifts it with the stale lift's notice and
// adds no label. A head that still touches the path is swapped.
func TestMigrateHumanMergePathHoldLiftsAStaleHold(t *testing.T) {
	head := strings.Repeat("c", 40)
	legacy := legacyHumanMergePathNotice(".github/workflows/ci.yml")
	appOnly := []map[string]any{pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-01T12:00:00Z")}
	f := &pathMigrationFake{items: map[int]pathMigrationItem{
		1: {events: appOnly, comments: []map[string]any{appComment(legacy)}, head: head, files: []string{"package.json"}, liftOnly: true},
		2: {events: appOnly, comments: []map[string]any{appComment(legacy)}, head: head, files: []string{".github/workflows/ci.yml"}},
		// Lifted once at this head and held again: stays, as in the stage lift.
		3: {events: appOnly, comments: []map[string]any{appComment(legacy), appComment(HumanMergePathLiftMarker + head + " -->\nHold lifted.")}, head: head, files: []string{"package.json"}},
	}}
	c := pathMigrationClient(t, f, nil)
	c.SetHumanMergePaths(map[string][]string{"acme/widget": {".github/workflows/**"}})
	res, err := c.MigrateHiveAppliedHolds(context.Background())
	if err != nil || res.Relabeled != 2 {
		t.Fatalf("result = %+v err=%v, want both migrated", res, err)
	}
	if len(f.added[1]) != 0 || len(f.removed[1]) != 1 || f.removed[1][0] != "hold" {
		t.Fatalf("#1 added=%v removed=%v, want hold lifted with no label", f.added[1], f.removed[1])
	}
	if len(f.comments[1]) != 1 || !strings.HasPrefix(f.comments[1][0], HumanMergePathLiftMarker+head+" -->") {
		t.Fatalf("#1 comments = %q, want one lift notice for the head", f.comments[1])
	}
	if len(f.added[2]) != 1 || f.added[2][0] != HumanMergePathLabel || len(f.removed[2]) != 1 {
		t.Fatalf("#2 added=%v removed=%v, want the swap", f.added[2], f.removed[2])
	}
	if len(f.added[3]) != 0 || len(f.removed[3]) != 0 || len(f.comments[3]) != 0 {
		t.Fatalf("#3 added=%v removed=%v comments=%v, want a head already lifted once left alone", f.added[3], f.removed[3], f.comments[3])
	}
}

// The swap goes through the write-back gate as human_merge_path_release: the
// hive's own PR is allowed whatever its labels (like the other releases), a
// PR it did not open needs the opt-in label, and a refusal is logged.
func TestMigrateHumanMergePathHoldWritebackGate(t *testing.T) {
	legacy := legacyHumanMergePathNotice(".github/workflows/ci.yml")
	appOnly := []map[string]any{pathMigrationEvent("labeled", testHiveAppBotLogin, "2026-11-01T12:00:00Z")}
	f := &pathMigrationFake{items: map[int]pathMigrationItem{
		1: {author: testHiveAppBotLogin, events: appOnly, comments: []map[string]any{appComment(legacy)}},
		2: {events: appOnly, comments: []map[string]any{appComment(legacy)}},
	}}
	var logs bytes.Buffer
	c := pathMigrationClient(t, f, &logs)
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, ""))
	if _, err := c.MigrateHiveAppliedHolds(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.added[1]) != 1 || f.added[1][0] != HumanMergePathLabel {
		t.Fatalf("hive-authored PR added %v, want the swap", f.added[1])
	}
	if len(f.added[2]) != 0 || len(f.removed[2]) != 0 {
		t.Fatalf("PR without the opt-in label was touched: added=%v removed=%v", f.added[2], f.removed[2])
	}
	if !strings.Contains(logs.String(), "the write-back gate refused the swap") {
		t.Fatalf("refusal not logged:\n%s", logs.String())
	}
}
