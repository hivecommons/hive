package upstreamwatch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testUpstream = "up/widgets"

// fakeSource returns items listed strictly after since, oldest-first, and
// records every since it was asked for.
type fakeSource struct {
	items  []Item
	err    error
	sinces []time.Time
}

func (f *fakeSource) List(_ context.Context, since time.Time) ([]Item, error) {
	f.sinces = append(f.sinces, since)
	if f.err != nil {
		return nil, f.err
	}
	var out []Item
	for _, it := range f.items {
		if it.Timestamp.After(since) {
			out = append(out, it)
		}
	}
	return out, nil
}

// fakeFiler answers marker searches from a map and numbers filed issues from
// 100 upwards.
type fakeFiler struct {
	existing  map[string]Existing
	findErr   error
	fileErr   error
	failAfter int
	filed     []Issue
	searched  []string

	// issues backs GetIssue, keyed by issue number; getErr fails every call
	// when set, gotten records every number asked for.
	issues map[int]IssueOutcome
	getErr error
	gotten []int
}

func (f *fakeFiler) FindMarker(_ context.Context, marker string) (Existing, bool, error) {
	f.searched = append(f.searched, marker)
	if f.findErr != nil {
		return Existing{}, false, f.findErr
	}
	e, ok := f.existing[marker]
	return e, ok, nil
}

func (f *fakeFiler) File(_ context.Context, issue Issue) (int, error) {
	if f.fileErr != nil || (f.failAfter > 0 && len(f.filed) >= f.failAfter) {
		return 0, errors.New("create failed")
	}
	f.filed = append(f.filed, issue)
	return 100 + len(f.filed) - 1, nil
}

func (f *fakeFiler) GetIssue(_ context.Context, number int) (IssueOutcome, bool, error) {
	f.gotten = append(f.gotten, number)
	if f.getErr != nil {
		return IssueOutcome{}, false, f.getErr
	}
	out, ok := f.issues[number]
	if !ok {
		return IssueOutcome{Open: true}, true, nil
	}
	return out, true, nil
}

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func prItem(n int, at time.Time, files ...string) Item {
	return Item{Kind: KindPR, Ref: RefPR(n), Title: "fix: thing " + RefPR(n), HTMLURL: "https://github.com/up/widgets/pull/1", Timestamp: at, Files: files}
}

func TestWatchRun(t *testing.T) {
	fixed := t0.Add(48 * time.Hour)
	tests := []struct {
		name          string
		items         []Item
		seed          func(State)
		exists        map[string]bool
		existing      map[string]Existing
		max           int
		failAfter     int
		wantFiled     []string
		wantSkipped   []string
		wantDeduped   []string
		wantDismissed []string
		wantCapped    bool
		wantRemaining int
		wantErr       bool
		wantWatermark time.Time
		wantStatus    map[string]RefStatus
	}{
		{
			name:          "files applicable items and advances the watermark",
			items:         []Item{prItem(1, t0.Add(time.Hour), "a.go"), prItem(2, t0.Add(2*time.Hour), "b.go")},
			exists:        map[string]bool{"a.go": true, "b.go": true},
			wantFiled:     []string{"upstream#1", "upstream#2"},
			wantWatermark: t0.Add(2 * time.Hour),
			wantStatus:    map[string]RefStatus{"upstream#1": StatusFiled, "upstream#2": StatusFiled},
		},
		{
			name:          "skips items whose files are absent from the fork",
			items:         []Item{prItem(1, t0.Add(time.Hour), "gone.go"), prItem(2, t0.Add(2*time.Hour), "a.go")},
			exists:        map[string]bool{"a.go": true},
			wantFiled:     []string{"upstream#2"},
			wantSkipped:   []string{"upstream#1"},
			wantWatermark: t0.Add(2 * time.Hour),
			wantStatus:    map[string]RefStatus{"upstream#1": StatusSkipped, "upstream#2": StatusFiled},
		},
		{
			name:  "state index dedupes an already filed ref",
			items: []Item{prItem(1, t0.Add(time.Hour), "a.go"), prItem(2, t0.Add(2*time.Hour), "a.go")},
			seed: func(s State) {
				s.Repo("widgets").Put(Outcome{Ref: "upstream#1", Status: StatusFiled, IssueNumber: 7}, t0)
			},
			exists:        map[string]bool{"a.go": true},
			wantFiled:     []string{"upstream#2"},
			wantDeduped:   []string{"upstream#1"},
			wantWatermark: t0.Add(2 * time.Hour),
		},
		{
			name:          "marker search dedupes and records the existing issue",
			items:         []Item{prItem(3, t0.Add(time.Hour), "a.go")},
			exists:        map[string]bool{"a.go": true},
			existing:      map[string]Existing{"<!-- upstream-ref: up/widgets#3 -->": {Number: 55}},
			wantDeduped:   []string{"upstream#3"},
			wantWatermark: t0.Add(time.Hour),
			wantStatus:    map[string]RefStatus{"upstream#3": StatusFiled},
		},
		{
			name:  "a ref dismissed in state is never resurfaced",
			items: []Item{prItem(4, t0.Add(time.Hour), "a.go")},
			seed: func(s State) {
				s.Repo("widgets").Dismiss("upstream#4", "not planned", t0)
			},
			exists:        map[string]bool{"a.go": true},
			wantDismissed: []string{"upstream#4"},
			wantWatermark: t0.Add(time.Hour),
			wantStatus:    map[string]RefStatus{"upstream#4": StatusDismissed},
		},
		{
			name:          "a dismissed fork issue found by marker is recorded as dismissed",
			items:         []Item{prItem(5, t0.Add(time.Hour), "a.go")},
			exists:        map[string]bool{"a.go": true},
			existing:      map[string]Existing{"<!-- upstream-ref: up/widgets#5 -->": {Number: 9, Dismissed: true}},
			wantDismissed: []string{"upstream#5"},
			wantWatermark: t0.Add(time.Hour),
			wantStatus:    map[string]RefStatus{"upstream#5": StatusDismissed},
		},
		{
			name: "cap leaves unfiled items behind the watermark",
			items: []Item{
				prItem(1, t0.Add(time.Hour), "a.go"),
				prItem(2, t0.Add(2*time.Hour), "a.go"),
				prItem(3, t0.Add(3*time.Hour), "a.go"),
			},
			exists:        map[string]bool{"a.go": true},
			max:           2,
			wantFiled:     []string{"upstream#1", "upstream#2"},
			wantCapped:    true,
			wantRemaining: 1,
			wantWatermark: t0.Add(2 * time.Hour),
		},
		{
			name:          "cap holds the watermark behind an item sharing the last timestamp",
			items:         []Item{prItem(1, fixed, "a.go"), prItem(2, fixed, "a.go")},
			exists:        map[string]bool{"a.go": true},
			max:           1,
			wantFiled:     []string{"upstream#1"},
			wantCapped:    true,
			wantRemaining: 1,
			wantWatermark: fixed.Add(-time.Nanosecond),
		},
		{
			name:          "a filing error stops the run without passing the failed item",
			items:         []Item{prItem(1, t0.Add(time.Hour), "a.go"), prItem(2, t0.Add(2*time.Hour), "a.go")},
			exists:        map[string]bool{"a.go": true},
			failAfter:     1,
			wantFiled:     []string{"upstream#1"},
			wantErr:       true,
			wantWatermark: t0.Add(time.Hour),
		},
		{
			name:          "releases are filed with no file probe",
			items:         []Item{{Kind: KindRelease, Ref: RefRelease("v2.0.0"), Title: "v2.0.0", Timestamp: t0.Add(time.Hour)}},
			wantFiled:     []string{"release:v2.0.0"},
			wantWatermark: t0.Add(time.Hour),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &memStore{state: State{}}
			if tc.seed != nil {
				tc.seed(store.state)
			}
			src := &fakeSource{items: tc.items}
			filer := &fakeFiler{existing: tc.existing, failAfter: tc.failAfter}
			w := New(Options{Repo: "widgets", Upstream: testUpstream, Label: "upstream/port", MaxIssuesPerRun: tc.max,
				Now: func() time.Time { return t0.Add(72 * time.Hour) }},
				src, &fakeContents{exists: tc.exists}, store, filer)
			res, err := w.Run(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("Run err = %v, wantErr %v", err, tc.wantErr)
			}
			check := func(field string, got, want []string) {
				t.Helper()
				if len(got) == 0 && len(want) == 0 {
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s = %v, want %v", field, got, want)
				}
			}
			check("Filed", res.Filed, tc.wantFiled)
			check("Skipped", res.Skipped, tc.wantSkipped)
			check("Deduped", res.Deduped, tc.wantDeduped)
			check("Dismissed", res.Dismissed, tc.wantDismissed)
			if res.Capped != tc.wantCapped || res.Remaining != tc.wantRemaining {
				t.Errorf("Capped/Remaining = %v/%d, want %v/%d", res.Capped, res.Remaining, tc.wantCapped, tc.wantRemaining)
			}
			if len(filer.filed) != len(tc.wantFiled) {
				t.Errorf("filed %d issues, want %d", len(filer.filed), len(tc.wantFiled))
			}
			rs := store.state["widgets"]
			if rs == nil {
				t.Fatal("no repo state saved")
			}
			if !rs.Watermark.Equal(tc.wantWatermark) {
				t.Errorf("Watermark = %v, want %v", rs.Watermark, tc.wantWatermark)
			}
			if rs.Upstream != testUpstream || !rs.LastRunAt.Equal(t0.Add(72*time.Hour)) {
				t.Errorf("Upstream/LastRunAt = %q/%v", rs.Upstream, rs.LastRunAt)
			}
			for ref, want := range tc.wantStatus {
				rec, ok := rs.Record(ref)
				if !ok || rec.Status != want {
					t.Errorf("%s status = %+v, want %s", ref, rec, want)
				}
			}
		})
	}
}

func TestWatchRun_NextRunResumesFromWatermark(t *testing.T) {
	store := &memStore{state: State{}}
	src := &fakeSource{items: []Item{
		prItem(1, t0.Add(time.Hour), "a.go"),
		prItem(2, t0.Add(2*time.Hour), "a.go"),
		prItem(3, t0.Add(3*time.Hour), "a.go"),
	}}
	filer := &fakeFiler{}
	w := New(Options{Repo: "widgets", Upstream: testUpstream, MaxIssuesPerRun: 2}, src, &fakeContents{exists: map[string]bool{"a.go": true}}, store, filer)
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Filed, []string{"upstream#3"}) || res.Capped {
		t.Fatalf("second run = %+v", res)
	}
	if len(src.sinces) != 2 || !src.sinces[0].IsZero() || !src.sinces[1].Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("List since = %v", src.sinces)
	}
	if len(filer.filed) != 3 {
		t.Fatalf("filed %d issues across two runs, want 3", len(filer.filed))
	}
}

func TestWatchRun_Errors(t *testing.T) {
	items := []Item{prItem(1, t0.Add(time.Hour), "a.go")}
	tests := []struct {
		name     string
		store    *memStore
		src      *fakeSource
		contents *fakeContents
		filer    *fakeFiler
		want     string
	}{
		{name: "load", store: &memStore{err: errFake}, src: &fakeSource{}, contents: &fakeContents{}, filer: &fakeFiler{}, want: "load"},
		{name: "list", store: &memStore{state: State{}}, src: &fakeSource{err: errors.New("boom")}, contents: &fakeContents{}, filer: &fakeFiler{}, want: "list upstream items"},
		{name: "judge", store: &memStore{state: State{}}, src: &fakeSource{items: items},
			contents: &fakeContents{errs: map[string]error{"a.go": errors.New("api down")}}, filer: &fakeFiler{}, want: "judge"},
		{name: "marker search", store: &memStore{state: State{}}, src: &fakeSource{items: items},
			contents: &fakeContents{exists: map[string]bool{"a.go": true}}, filer: &fakeFiler{findErr: errors.New("search down")}, want: "search fork for marker"},
		{name: "file", store: &memStore{state: State{}}, src: &fakeSource{items: items},
			contents: &fakeContents{exists: map[string]bool{"a.go": true}}, filer: &fakeFiler{fileErr: errors.New("nope")}, want: "file issue"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := New(Options{Repo: "widgets", Upstream: testUpstream}, tc.src, tc.contents, tc.store, tc.filer)
			_, err := w.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if tc.name != "load" {
				if rs := tc.store.state["widgets"]; rs == nil || !rs.Watermark.IsZero() {
					t.Fatalf("state after error = %+v, want saved with zero watermark", rs)
				}
			}
		})
	}
}

func TestWatchRun_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &memStore{state: State{}}
	filer := &fakeFiler{}
	w := New(Options{Repo: "widgets", Upstream: testUpstream}, &fakeSource{items: []Item{prItem(1, t0.Add(time.Hour), "a.go")}},
		&fakeContents{exists: map[string]bool{"a.go": true}}, store, filer)
	if _, err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(filer.filed) != 0 {
		t.Fatalf("filed %d issues after cancel", len(filer.filed))
	}
}

// TestWatchRun_Reconcile covers the reconciliation pass (hivecommons/hive#9969
// remainder 2): a previously filed ref whose fork issue has since closed is
// updated to ported or dismissed; one still open is left alone.
func TestWatchRun_Reconcile(t *testing.T) {
	tests := []struct {
		name       string
		issues     map[int]IssueOutcome
		wantStatus RefStatus
	}{
		{name: "still open leaves the ref filed", issues: map[int]IssueOutcome{7: {Open: true}}, wantStatus: StatusFiled},
		{name: "closed as completed becomes ported", issues: map[int]IssueOutcome{7: {Ported: true}}, wantStatus: StatusPorted},
		{name: "closed as not planned becomes dismissed", issues: map[int]IssueOutcome{7: {Dismissed: true}}, wantStatus: StatusDismissed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &memStore{state: State{}}
			store.state.Repo("widgets").Put(Outcome{Ref: "upstream#1", Status: StatusFiled, IssueNumber: 7}, t0)
			filer := &fakeFiler{issues: tc.issues}
			w := New(Options{Repo: "widgets", Upstream: testUpstream, Now: func() time.Time { return t0.Add(72 * time.Hour) }},
				&fakeSource{}, &fakeContents{}, store, filer)
			res, err := w.Run(context.Background())
			if err != nil {
				t.Fatalf("Run err = %v", err)
			}
			rs := store.state["widgets"]
			rec, ok := rs.Record("upstream#1")
			if !ok || rec.Status != tc.wantStatus {
				t.Fatalf("status = %+v, want %s", rec, tc.wantStatus)
			}
			switch tc.wantStatus {
			case StatusPorted:
				if !reflect.DeepEqual(res.Ported, []string{"upstream#1"}) {
					t.Errorf("Ported = %v", res.Ported)
				}
			case StatusDismissed:
				if !reflect.DeepEqual(res.Dismissed, []string{"upstream#1"}) {
					t.Errorf("Dismissed = %v", res.Dismissed)
				}
			}
			if len(filer.gotten) != 1 || filer.gotten[0] != 7 {
				t.Errorf("gotten = %v, want [7]", filer.gotten)
			}
			if !rs.Watermark.IsZero() {
				t.Errorf("Watermark = %v, want zero (reconciliation must not move it)", rs.Watermark)
			}
		})
	}
}

// TestWatchRun_ReconcileSkipsJustFiled confirms a ref filed in this very run
// is not immediately reconciled: a freshly opened issue is certainly still
// open, so reconciling it would only waste a GitHub call.
func TestWatchRun_ReconcileSkipsJustFiled(t *testing.T) {
	store := &memStore{state: State{}}
	src := &fakeSource{items: []Item{prItem(1, t0.Add(time.Hour), "a.go")}}
	filer := &fakeFiler{}
	w := New(Options{Repo: "widgets", Upstream: testUpstream}, src, &fakeContents{exists: map[string]bool{"a.go": true}}, store, filer)
	if _, err := w.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(filer.gotten) != 0 {
		t.Errorf("gotten = %v, want none for a ref filed this run", filer.gotten)
	}
}

// TestWatchRun_ReconcileError confirms a GetIssue failure is reported and
// leaves the ref's recorded status untouched for next run to retry.
func TestWatchRun_ReconcileError(t *testing.T) {
	store := &memStore{state: State{}}
	store.state.Repo("widgets").Put(Outcome{Ref: "upstream#1", Status: StatusFiled, IssueNumber: 7}, t0)
	filer := &fakeFiler{getErr: errors.New("rate limited")}
	w := New(Options{Repo: "widgets", Upstream: testUpstream}, &fakeSource{}, &fakeContents{}, store, filer)
	_, err := w.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reconcile filed issues") {
		t.Fatalf("err = %v, want it to mention reconcile filed issues", err)
	}
	rec, ok := store.state["widgets"].Record("upstream#1")
	if !ok || rec.Status != StatusFiled {
		t.Fatalf("status = %+v, want unchanged StatusFiled", rec)
	}
}
