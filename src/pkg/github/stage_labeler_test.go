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
