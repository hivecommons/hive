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

	"github.com/hivecommons/hive/pkg/convergence/mutation"
)

// plannedLabelOp journals the SAME logical operation addLabelsObserved /
// removeLabelObserved would derive for the mutation, left Planned: an
// attempt begun and never acknowledged (a process that died between intent
// and result, or a concurrent writer still mid-call — the "op:… is Planned"
// behind hivecommons/hive#13579's failed claimed-by label).
func plannedLabelOp(t *testing.T, b *mutation.Boundary, repo string, number string, inputs map[string]string) string {
	t.Helper()
	subject := repo + "#label_mutation/" + number
	mclaim := mutation.TaskClaim(repo, subject)
	all := map[string]string{"repo": repo, "kind": "label_mutation", "target": number}
	for k, v := range inputs {
		all[k] = v
	}
	e := mutation.Effect{
		OutcomeKey:        repo + "@mutation",
		DesiredGeneration: 1,
		Transition:        "external.label_mutation",
		Subject:           mclaim.Subject,
		ClaimKey:          mclaim.Key(),
		Kind:              "label_mutation",
		Inputs:            all,
	}
	op, err := b.Executor.Journal.Begin(e, 1, "crashed", labelOpSeededAt)
	if err != nil {
		t.Fatalf("seed Planned op: %v", err)
	}
	return op.LogicalID
}

type labelMutationServer struct {
	mu     sync.Mutex
	labels []string
	gets   int
	adds   int
	dels   int
}

func newLabelMutationServer(t *testing.T, initial ...string) (*labelMutationServer, *Client) {
	t.Helper()
	s := &labelMutationServer{labels: initial}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.gets++
		out := map[string]any{"number": 7, "updated_at": "2026-10-23T06:00:00Z", "labels": []map[string]string{}}
		for _, l := range s.labels {
			out["labels"] = append(out["labels"].([]map[string]string), map[string]string{"name": l})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.adds++
		var body []string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.labels = append(s.labels, body...)
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	})
	mux.HandleFunc("/repos/acme/widgets/issues/7/labels/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.dels++
		name := strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/issues/7/labels/")
		var keep []string
		for _, l := range s.labels {
			if l != name {
				keep = append(keep, l)
			}
		}
		s.labels = keep
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
}

// A Planned add whose label is NOT on the item reconciles to not-applied
// from one read and then runs, instead of failing the caller with "op:… is
// Planned: operation must be reconciled".
func TestAddLabels_PlannedOpReconcilesFromLabelsAndApplies(t *testing.T) {
	srv, c := newLabelMutationServer(t, "bug")
	b := replayTestBoundary(t)
	c.SetMutationBoundary(b)
	plannedLabelOp(t, b, "acme/widgets", "7", map[string]string{"labels": "hive/claimed-by-human"})

	if err := c.AddLabels(context.Background(), "acme/widgets", 7, []string{"hive/claimed-by-human"}); err != nil {
		t.Fatalf("AddLabels after Planned op: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gets != 1 || srv.adds != 1 {
		t.Fatalf("gets=%d adds=%d, want one reconciling read and one add", srv.gets, srv.adds)
	}
	if !labelsAllPresent(srv.labels, []string{"hive/claimed-by-human"}) {
		t.Fatalf("item carries %v", srv.labels)
	}
}

// A Planned add whose label IS already on the item (the earlier attempt
// landed before it could acknowledge) reconciles to applied and does not
// call the API again.
func TestAddLabels_PlannedOpAlreadyAppliedSkipsEffect(t *testing.T) {
	srv, c := newLabelMutationServer(t, "bug", "hive/claimed-by-human")
	b := replayTestBoundary(t)
	c.SetMutationBoundary(b)
	plannedLabelOp(t, b, "acme/widgets", "7", map[string]string{"labels": "hive/claimed-by-human"})

	if err := c.AddLabels(context.Background(), "acme/widgets", 7, []string{"hive/claimed-by-human"}); err != nil {
		t.Fatalf("AddLabels after Planned op: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.gets != 1 || srv.adds != 0 {
		t.Fatalf("gets=%d adds=%d, want one read and no add", srv.gets, srv.adds)
	}
}

// The remove side: a Planned remove of a label still on the item runs; one
// of a label already gone resolves without an API call.
func TestRemoveLabel_PlannedOpReconcilesFromLabels(t *testing.T) {
	srv, c := newLabelMutationServer(t, "bug", "stage/010-actionable")
	b := replayTestBoundary(t)
	c.SetMutationBoundary(b)
	plannedLabelOp(t, b, "acme/widgets", "7", map[string]string{"remove_label": "stage/010-actionable"})
	if err := c.RemoveLabel(context.Background(), "acme/widgets", 7, "stage/010-actionable"); err != nil {
		t.Fatalf("RemoveLabel after Planned op: %v", err)
	}
	srv.mu.Lock()
	if srv.dels != 1 || labelsAllPresent(srv.labels, []string{"stage/010-actionable"}) {
		srv.mu.Unlock()
		t.Fatalf("dels=%d labels=%v, want the remove to run", srv.dels, srv.labels)
	}
	srv.mu.Unlock()

	srv2, c2 := newLabelMutationServer(t, "bug")
	b2 := replayTestBoundary(t)
	c2.SetMutationBoundary(b2)
	plannedLabelOp(t, b2, "acme/widgets", "7", map[string]string{"remove_label": "stage/010-actionable"})
	if err := c2.RemoveLabel(context.Background(), "acme/widgets", 7, "stage/010-actionable"); err != nil {
		t.Fatalf("RemoveLabel of absent label after Planned op: %v", err)
	}
	srv2.mu.Lock()
	defer srv2.mu.Unlock()
	if srv2.dels != 0 || srv2.gets != 1 {
		t.Fatalf("dels=%d gets=%d, want the read alone to resolve it", srv2.dels, srv2.gets)
	}
}

// labelOpSeededAt is when plannedLabelOp journals its attempt.
var labelOpSeededAt = time.Date(2026, 10, 23, 6, 0, 0, 0, time.UTC)

// When the reconciling read itself fails a young operation is not expired
// blind: the needs-reconciliation refusal stands.
func TestAddLabels_PlannedOpReadFailureKeepsRefusal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
	b := replayTestBoundary(t)
	b.Executor.Now = func() time.Time { return labelOpSeededAt.Add(time.Minute) }
	c.SetMutationBoundary(b)
	plannedLabelOp(t, b, "acme/widgets", "7", map[string]string{"labels": "hive/claimed-by-human"})
	err := c.AddLabels(context.Background(), "acme/widgets", 7, []string{"hive/claimed-by-human"})
	if err == nil || !strings.Contains(err.Error(), "reconcil") {
		t.Fatalf("err = %v, want the reconciliation refusal to stand", err)
	}
}

// An operation left Planned/Unknown whose reconciling read keeps failing must
// not fence the label forever (hivecommons/hive#15944): once it has been
// unresolved past effects.DefaultUnresolvedTTL it is expired to not-applied
// and the idempotent label write replays; a younger one keeps the refusal.
func TestLabelMutation_UnresolvedOpReadFailureExpiresPastTTL(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		age       time.Duration
		remove    bool
		wantWrite bool
	}{
		{name: "unknown add past ttl replays", status: mutation.StatusUnknown, age: 83 * time.Minute, wantWrite: true},
		{name: "planned add past ttl replays", status: mutation.StatusPlanned, age: 2 * time.Hour, wantWrite: true},
		{name: "unknown remove past ttl replays", status: mutation.StatusUnknown, age: 83 * time.Minute, remove: true, wantWrite: true},
		{name: "unknown add within ttl keeps refusal", status: mutation.StatusUnknown, age: 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			gets, writes := 0, 0
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/acme/widgets/issues/7", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gets++
				mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
			})
			write := func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				writes++
				mu.Unlock()
				_ = json.NewEncoder(w).Encode([]map[string]string{})
			}
			mux.HandleFunc("/repos/acme/widgets/issues/7/labels", write)
			mux.HandleFunc("/repos/acme/widgets/issues/7/labels/", write)
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, testLogger())
			b := replayTestBoundary(t)
			b.Executor.Now = func() time.Time { return labelOpSeededAt.Add(tc.age) }
			c.SetMutationBoundary(b)

			inputs := map[string]string{"labels": "hive/claimed-by-human"}
			if tc.remove {
				inputs = map[string]string{"remove_label": "stage/010-actionable"}
			}
			id := plannedLabelOp(t, b, "acme/widgets", "7", inputs)
			if tc.status == mutation.StatusUnknown {
				if _, err := b.Executor.Journal.RecordResult(id, 1, mutation.StatusUnknown, "", labelOpSeededAt); err != nil {
					t.Fatalf("record Unknown: %v", err)
				}
			}

			var err error
			if tc.remove {
				err = c.RemoveLabel(context.Background(), "acme/widgets", 7, "stage/010-actionable")
			} else {
				err = c.AddLabels(context.Background(), "acme/widgets", 7, []string{"hive/claimed-by-human"})
			}
			mu.Lock()
			defer mu.Unlock()
			if gets == 0 {
				t.Fatalf("no reconciling read was attempted")
			}
			if tc.wantWrite {
				if err != nil {
					t.Fatalf("err = %v, want the expired operation to replay", err)
				}
				if writes != 1 {
					t.Fatalf("writes = %d, want the label write to replay once", writes)
				}
				if op, ok := b.Executor.Journal.Get(id); !ok || op.Status != mutation.StatusApplied {
					t.Fatalf("operation = %+v (found %v), want Applied after replay", op, ok)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "reconcil") {
				t.Fatalf("err = %v, want the reconciliation refusal to stand", err)
			}
			if writes != 0 {
				t.Fatalf("writes = %d, want no write while the refusal stands", writes)
			}
		})
	}
}
