package automerge

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type recordedHotspot struct {
	mu        sync.Mutex
	files     map[int][]string
	opened    map[int]time.Time
	conflicts map[int][]string
}

func (r *recordedHotspot) RecordPRFiles(_ string, number int, files []string, openedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files == nil {
		r.files, r.opened = map[int][]string{}, map[int]time.Time{}
	}
	r.files[number] = append([]string(nil), files...)
	r.opened[number] = openedAt
}

func (r *recordedHotspot) RecordConflict(_ string, number int, files []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conflicts == nil {
		r.conflicts = map[int][]string{}
	}
	r.conflicts[number] = append([]string(nil), files...)
}

// The merge train hands its PR file lists and the conflicts it repairs to
// the hotspot recorder, so the advisor never lists files itself.
func TestMergeTrainFeedsHotspotRecorder(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{20: {"a.go", "b.go"}, 21: {"a.go", "c.go"}, 22: {"b.go", "d.go"}},
		state: map[int]string{20: "clean", 21: "clean", 22: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	resolver := RebaseResolverFunc(func(context.Context, RebaseRequest) error { return nil })
	c, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, resolver)
	rec := &recordedHotspot{}
	c.hotspotRecorder = rec
	prs := []*gh.PullRequest{mtPR(20, t0, ""), mtPR(21, t0.Add(time.Minute), ""), mtPR(22, t0.Add(2*time.Minute), "")}
	mtReady(c, "self", prs...)
	plan := c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) string { return "" })
	if !plan.isFront(20) {
		t.Fatalf("front = %v", plan.groups)
	}
	rec.mu.Lock()
	if len(rec.files) != 3 || len(rec.files[21]) != 2 || !rec.opened[21].Equal(t0.Add(time.Minute)) {
		t.Fatalf("recorded files = %v opened = %v", rec.files, rec.opened)
	}
	rec.mu.Unlock()

	// Front merged, #22 is dirty: the conflict is blamed on the overlap
	// files #22 itself touches (b.go), not on #21's a.go.
	api.mu.Lock()
	api.state[21], api.state[22] = "behind", "dirty"
	api.mu.Unlock()
	c.mergeTrainAfterFrontMerge(context.Background(), plan, 20)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	got := rec.conflicts[22]
	sort.Strings(got)
	if len(rec.conflicts) != 1 || len(got) != 1 || got[0] != "b.go" {
		t.Fatalf("conflicts = %v, want 22:[b.go]", rec.conflicts)
	}
}
