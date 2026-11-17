package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

// A PR older than the backfill cutoff kept whatever stage label the hive
// last put on it: the gate refused the move and the removal alike
// (projectbluefin/utah-packages#1148 sat at stage/100-merge-queue while not
// queued). These tests run the stage labeler through a real gate: a stage
// label the hive applied is moved, one a person applied is left alone, and
// only a PR the guard would hold costs a timeline read.

const (
	hiveStageRepo = "acme/widgets"
	hiveStagePR   = 90
	hiveStageOld  = "2026-01-15T09:00:00Z" // before hiveStageFirstSeen
	hiveStageNew  = "2026-07-01T09:00:00Z" // after it
)

var hiveStageFirstSeen = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// hiveStageServer is acme/widgets with one open PR, #90, carrying labels.
// actor applied its stage label, per the timeline.
type hiveStageServer struct {
	mu        sync.Mutex
	calls     []string
	labels    []string
	actor     string
	createdAt string
}

func (f *hiveStageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	const issue = "/repos/" + hiveStageRepo + "/issues/90"
	switch {
	case r.Method == "GET" && r.URL.Path == issue:
		labels := []any{}
		for _, l := range f.labels {
			labels = append(labels, map[string]any{"name": l})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": hiveStagePR, "state": "open", "created_at": f.createdAt,
			"updated_at": "2026-11-17T10:00:00Z", "user": map[string]any{"login": "contributor"},
			"labels": labels, "pull_request": map[string]any{"url": "x"},
		})
	case r.Method == "GET" && r.URL.Path == issue+"/timeline":
		var events []map[string]any
		for _, l := range f.labels {
			events = append(events, map[string]any{
				"event": "labeled", "created_at": "2026-11-16T05:18:44Z",
				"label": map[string]any{"name": l}, "actor": map[string]any{"login": f.actor},
			})
		}
		_ = json.NewEncoder(w).Encode(events)
	case r.Method == "POST" && r.URL.Path == issue+"/labels":
		var body []string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.labels = append(f.labels, body...)
		_, _ = io.WriteString(w, `[]`)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, issue+"/labels/"):
		gone := strings.TrimPrefix(r.URL.Path, issue+"/labels/")
		var kept []string
		for _, l := range f.labels {
			if l != gone {
				kept = append(kept, l)
			}
		}
		f.labels = kept
		_, _ = io.WriteString(w, `[]`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}
}

func (f *hiveStageServer) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// hiveStageRig is a client over f with a gate that first saw acme/widgets
// at hiveStageFirstSeen; mode full, so the backfill guard is the only check
// that can refuse a stage label here.
func hiveStageRig(t *testing.T, f *hiveStageServer) (*Client, *writeback.Gate) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.appBotLogin = testBot
	gate := writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{hiveStageRepo: {Mode: writeback.RepoModeFull}}}, writeback.Deps{
		LevelFor: func(string) int { return 6 },
		Now:      func() time.Time { return hiveStageFirstSeen },
	}, "")
	gate.ObserveRepo(hiveStageRepo)
	c.SetWritebackGate(gate)
	return c, gate
}

// moveStage is one stage sync: read #90, add 085, remove 100.
func moveStage(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	l := c.StageLabeler()
	if _, err := l.Labels(ctx, "acme", "widgets", hiveStagePR); err != nil {
		t.Fatal(err)
	}
	if err := l.AddLabels(ctx, "acme", "widgets", hiveStagePR, []string{"stage/085-needs-decision"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveLabel(ctx, "acme", "widgets", hiveStagePR, "stage/100-merge-queue"); err != nil {
		t.Fatal(err)
	}
}

func TestStageLabelerMovesAStageLabelTheHiveAppliedOnAnOldPR(t *testing.T) {
	f := &hiveStageServer{labels: []string{"stage/100-merge-queue", "role/trusted"}, actor: testBot, createdAt: hiveStageOld}
	c, gate := hiveStageRig(t, f)
	moveStage(t, c)
	if f.count("POST ") != 1 || f.count("DELETE ") != 1 {
		t.Fatalf("calls = %v, want 085 added and 100 removed", f.calls)
	}
	if held := gate.Recent(); len(held) != 0 {
		t.Fatalf("gate held %+v, want nothing held", held)
	}
	if n := gate.Snapshot().Repos[hiveStageRepo].PreExisting; n != 0 {
		t.Fatalf("pre-existing untouched = %d, want 0: the hive wrote to this PR", n)
	}
}

func TestStageLabelerLeavesAPersonsStageLabelOnAnOldPR(t *testing.T) {
	f := &hiveStageServer{labels: []string{"stage/100-merge-queue"}, actor: "maintainer", createdAt: hiveStageOld}
	c, gate := hiveStageRig(t, f)
	moveStage(t, c)
	if f.count("POST ") != 0 || f.count("DELETE ") != 0 {
		t.Fatalf("calls = %v, want no label write on a person's stage label", f.calls)
	}
	held := gate.Recent()
	if len(held) == 0 || held[0].Reason != writeback.ReasonBackfill {
		t.Fatalf("gate held %+v, want the backfill guard's refusal", held)
	}
}

// A PR the guard does not cover is decided as before, with no timeline read;
// on an old PR the answer is read once and reused while its stage labels
// stay the same.
func TestStageLabelerReadsTheTimelineOnlyForAHeldPROnce(t *testing.T) {
	newer := &hiveStageServer{labels: []string{"stage/100-merge-queue"}, actor: testBot, createdAt: hiveStageNew}
	c, _ := hiveStageRig(t, newer)
	moveStage(t, c)
	if n := newer.count("GET /repos/acme/widgets/issues/90/timeline"); n != 0 {
		t.Fatalf("timeline read %d times for a PR the guard does not cover, want 0", n)
	}
	if newer.count("POST ") != 1 {
		t.Fatalf("calls = %v, want the move to land", newer.calls)
	}

	old := &hiveStageServer{labels: []string{"stage/100-merge-queue"}, actor: "maintainer", createdAt: hiveStageOld}
	c, _ = hiveStageRig(t, old)
	moveStage(t, c)
	moveStage(t, c)
	if n := old.count("GET /repos/acme/widgets/issues/90/timeline"); n != 1 {
		t.Fatalf("timeline read %d times over two syncs with the same labels, want 1", n)
	}
	// Past the TTL the answer is read again: a person may have removed and
	// re-added a label under the same name.
	later := time.Now().Add(hiveStageFactTTL + time.Minute)
	defer func(prev func() time.Time) { hiveStageNow = prev }(hiveStageNow)
	hiveStageNow = func() time.Time { return later }
	moveStage(t, c)
	if n := old.count("GET /repos/acme/widgets/issues/90/timeline"); n != 2 {
		t.Fatalf("timeline read %d times after the TTL, want 2", n)
	}
}

func TestStageLabelsAppliedByHive(t *testing.T) {
	c := NewClientForTest("http://unused", "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.appBotLogin = testBot
	ev := func(label, actor string) IssueEvent { return IssueEvent{Event: "labeled", Label: label, Actor: actor} }
	cases := []struct {
		name   string
		stage  []string
		events []IssueEvent
		want   bool
	}{
		{"hive applied the one stage label", []string{"stage/100-merge-queue"}, []IssueEvent{ev("stage/100-merge-queue", testBot)}, true},
		{"a person re-applied it later", []string{"stage/100-merge-queue"}, []IssueEvent{ev("stage/100-merge-queue", testBot), ev("stage/100-merge-queue", "maintainer")}, false},
		{"the hive re-applied a person's label", []string{"stage/100-merge-queue"}, []IssueEvent{ev("stage/100-merge-queue", "maintainer"), ev("stage/100-merge-queue", testBot)}, true},
		{"one of two is a person's", []string{"stage/071-needs-author", "stage/100-merge-queue"}, []IssueEvent{ev("stage/071-needs-author", testBot), ev("stage/100-merge-queue", "maintainer")}, false},
		{"no labeled event for it", []string{"stage/100-merge-queue"}, nil, false},
		{"no stage label", nil, []IssueEvent{ev("stage/100-merge-queue", testBot)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.stageLabelsAppliedByHive(tc.stage, tc.events); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if got := stageLabelSet([]string{"role/trusted", "Stage/100-merge-queue", "stage/071-needs-author", "stage/health-x"}); got != "stage/071-needs-author" {
		// IsStageLabel is case-sensitive on the prefix, as the stage engine's
		// labels are; only ordered stage/NNN-slug labels count.
		t.Fatalf("stageLabelSet = %q", got)
	}
}
