package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#12443: the reconciler logged "removed stage/020-claimed"
// every cycle while GitHub kept the label, because the mutation journal had
// recorded that exact remove as applied once before and replayed it as an
// idempotent no-op. Writes through the stage labeler are journaled against
// the item's observed updated_at, so after another writer puts the label
// back (which bumps updated_at) the remove is a new effect and reaches the
// API again — while a retry within the same observed state still dedupes.
func TestStageLabeler_RemoveAgainAfterExternalReadd(t *testing.T) {
	var mu sync.Mutex
	labels := []string{"bug", "stage/010-actionable", "stage/020-claimed"}
	updated := time.Date(2026, 10, 16, 19, 0, 0, 0, time.UTC)
	var deletes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]any{"number": 7, "updated_at": updated.Format(time.RFC3339), "labels": []map[string]string{}}
		for _, l := range labels {
			out["labels"] = append(out["labels"].([]map[string]string), map[string]string{"name": l})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/7/labels/")
		deletes = append(deletes, name)
		var keep []string
		for _, l := range labels {
			if l != name {
				keep = append(keep, l)
			}
		}
		labels = keep
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	c.SetMutationBoundary(replayTestBoundary(t))
	ctx := context.Background()

	if _, err := c.ApplyStageLabel(ctx, "acme/widgets", 7, stagelabels.Claimed); err != nil {
		t.Fatal(err)
	}
	// Another writer puts 010 back; GitHub bumps updated_at.
	mu.Lock()
	labels = append(labels, "stage/010-actionable")
	updated = updated.Add(time.Minute)
	mu.Unlock()
	if _, err := c.ApplyStageLabel(ctx, "acme/widgets", 7, stagelabels.Claimed); err != nil {
		t.Fatal(err)
	}
	// Same state again: nothing to do, so no third delete either way.
	if out, err := c.ApplyStageLabel(ctx, "acme/widgets", 7, stagelabels.Claimed); err != nil || out.Changed() {
		t.Fatalf("idempotent pass: out=%+v err=%v", out, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deletes) != 2 || deletes[0] != "stage/010-actionable" || deletes[1] != "stage/010-actionable" {
		t.Fatalf("deletes = %v, want the remove to reach the API both times", deletes)
	}
	if strings.Join(labels, ",") != "bug,stage/020-claimed" {
		t.Fatalf("item carries %v", labels)
	}
}

// The plain RemoveLabel keeps its crash-retry dedupe: the same remove in
// the same journal is applied once.
func TestRemoveLabel_PlainReplayDedupes(t *testing.T) {
	var mu sync.Mutex
	deletes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deletes++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	c.SetMutationBoundary(replayTestBoundary(t))
	for i := 0; i < 2; i++ {
		if err := c.RemoveLabel(context.Background(), "acme/widgets", 7, "stage/010-actionable"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if deletes != 1 {
		t.Fatalf("deletes = %d, want the replay deduped", deletes)
	}
}

// issueLabelFixture is one issue whose labels and updated_at the handlers
// mutate the way GitHub does: every add or remove bumps updated_at.
type issueLabelFixture struct {
	mu      sync.Mutex
	labels  []string
	updated time.Time
	adds    [][]string
	deletes []string
}

func newIssueLabelFixture(t *testing.T, labels ...string) (*issueLabelFixture, *Client) {
	t.Helper()
	f := &issueLabelFixture{labels: labels, updated: time.Date(2026, 10, 16, 19, 0, 0, 0, time.UTC)}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]any{"number": 7, "updated_at": f.updated.Format(time.RFC3339), "labels": []map[string]string{}}
		for _, l := range f.labels {
			out["labels"] = append(out["labels"].([]map[string]string), map[string]string{"name": l})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		var body []string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.adds = append(f.adds, body)
		f.labels = append(f.labels, body...)
		f.updated = f.updated.Add(time.Minute)
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/7/labels/")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deletes = append(f.deletes, name)
		var keep []string
		for _, l := range f.labels {
			if l != name {
				keep = append(keep, l)
			}
		}
		f.labels = keep
		f.updated = f.updated.Add(time.Minute)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	c.SetMutationBoundary(replayTestBoundary(t))
	return f, c
}

func (f *issueLabelFixture) readd(labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, labels...)
	f.updated = f.updated.Add(time.Minute)
}

// hivecommons/hive#12560 had no stage label at all because every plain
// label write on it replayed as "already applied". The claim-release hook
// removes through a fresh read: the second lapse of a claim on the same
// issue reaches the API again, and labels the item does not carry are not
// requested.
func TestRemoveLabelsFresh_RemovesAgainAfterReadd(t *testing.T) {
	f, c := newIssueLabelFixture(t, "bug", "stage/020-claimed", "stage/030-pr-in-progress")
	ctx := context.Background()
	if err := c.RemoveLabelsFresh(ctx, "acme/widgets", 7, "stage/020-claimed", "stage/030-pr-in-progress", "hive/preempted-x"); err != nil {
		t.Fatal(err)
	}
	f.readd("stage/030-pr-in-progress")
	if err := c.RemoveLabelsFresh(ctx, "acme/widgets", 7, "stage/020-claimed", "stage/030-pr-in-progress"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.deletes, ",") != "stage/020-claimed,stage/030-pr-in-progress,stage/030-pr-in-progress" {
		t.Fatalf("deletes = %v", f.deletes)
	}
	if strings.Join(f.labels, ",") != "bug" {
		t.Fatalf("item carries %v", f.labels)
	}
}

// A claim request's labels go on through the enforcer: one read, the stale
// stage off, one add carrying the stage and the ownership label, and a
// second claim after the first lapsed is written, not replayed.
func TestAddLabelsFresh_EnforcesStageAndRewritesAfterLapse(t *testing.T) {
	f, c := newIssueLabelFixture(t, "bug", "stage/010-actionable")
	ctx := context.Background()
	if err := c.AddLabelsFresh(ctx, "acme/widgets", 7, stagelabels.PRInProgress, "hive/claimed-by-scanner"); err != nil {
		t.Fatal(err)
	}
	// The claim lapsed: the hooks took both labels off and the reconciler
	// staged the issue actionable again.
	f.mu.Lock()
	f.labels = []string{"bug", "stage/010-actionable"}
	f.updated = f.updated.Add(time.Hour)
	f.mu.Unlock()
	if err := c.AddLabelsFresh(ctx, "acme/widgets", 7, stagelabels.PRInProgress, "hive/claimed-by-scanner"); err != nil {
		t.Fatal(err)
	}
	// Already there: nothing is written.
	if err := c.AddLabelsFresh(ctx, "acme/widgets", 7, stagelabels.PRInProgress, "hive/claimed-by-scanner"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.adds) != 2 || strings.Join(f.adds[0], ",") != "stage/030-pr-in-progress,hive/claimed-by-scanner" || strings.Join(f.adds[1], ",") != "stage/030-pr-in-progress,hive/claimed-by-scanner" {
		t.Fatalf("adds = %v, want one combined add per claim", f.adds)
	}
	if strings.Join(f.deletes, ",") != "stage/010-actionable,stage/010-actionable" {
		t.Fatalf("deletes = %v", f.deletes)
	}
	if strings.Join(f.labels, ",") != "bug,stage/030-pr-in-progress,hive/claimed-by-scanner" {
		t.Fatalf("item carries %v", f.labels)
	}
}

// The in-progress sweep's "start mark lapsed" removal is keyed to the list
// payload's updated_at (hivecommons/hive#12481 logged "label removed" while
// GitHub kept stage/030): a second lapse on the same issue, seen with a
// newer updated_at, reaches the API; the same payload twice still dedupes.
func TestRemoveInProgressLabel_ObservedKeyedPerPayload(t *testing.T) {
	f, c := newIssueLabelFixture(t, "stage/030-pr-in-progress")
	ctx := context.Background()
	at := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	first := Issue{Repo: "acme/widgets", Number: 7, Labels: []string{"stage/030-pr-in-progress"}, UpdatedAt: at}
	c.removeInProgressLabel(ctx, &first, "start mark lapsed", nil)
	retry := Issue{Repo: "acme/widgets", Number: 7, Labels: []string{"stage/030-pr-in-progress"}, UpdatedAt: at}
	c.removeInProgressLabel(ctx, &retry, "start mark lapsed", nil)
	f.readd("stage/030-pr-in-progress")
	later := Issue{Repo: "acme/widgets", Number: 7, Labels: []string{"stage/030-pr-in-progress"}, UpdatedAt: at.Add(2 * time.Hour)}
	c.removeInProgressLabel(ctx, &later, "start mark lapsed", nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deletes) != 2 {
		t.Fatalf("deletes = %v, want the first and the later removal, not the retry", f.deletes)
	}
	if len(f.labels) != 0 {
		t.Fatalf("item carries %v", f.labels)
	}
}
