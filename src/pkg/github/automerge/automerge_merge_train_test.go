package automerge

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestPlanMergeTrains(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name    string
		members []mergeTrainMember
		want    map[int]mergeTrainSlot
	}{
		{
			name: "disjoint PRs form no train",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Files: []string{"a.go"}},
				{Number: 2, Base: "v5", Files: []string{"b.go"}},
			},
			want: map[int]mergeTrainSlot{},
		},
		{
			name: "overlap picks the lowest number when ages tie",
			members: []mergeTrainMember{
				{Number: 2, Base: "v5", Files: []string{"stage_tracker.go"}},
				{Number: 1, Base: "v5", Files: []string{"stage_tracker.go", "a.go"}},
			},
			want: map[int]mergeTrainSlot{
				1: {Front: 1, Position: 1, Size: 2, OverlapFiles: []string{"stage_tracker.go"}},
				2: {Front: 1, Position: 2, Size: 2, OverlapFiles: []string{"stage_tracker.go"}},
			},
		},
		{
			name: "oldest fronts over a lower number",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Created: day(3), Files: []string{"x.go"}},
				{Number: 2, Base: "v5", Created: day(1), Files: []string{"x.go"}},
			},
			want: map[int]mergeTrainSlot{
				2: {Front: 2, Position: 1, Size: 2, OverlapFiles: []string{"x.go"}},
				1: {Front: 2, Position: 2, Size: 2, OverlapFiles: []string{"x.go"}},
			},
		},
		{
			name: "mergeable fronts over an older blocked head",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Created: day(1), Blocked: true, Files: []string{"x.go"}},
				{Number: 2, Base: "v5", Created: day(2), Files: []string{"x.go"}},
			},
			want: map[int]mergeTrainSlot{
				2: {Front: 2, Position: 1, Size: 2, OverlapFiles: []string{"x.go"}},
				1: {Front: 2, Position: 2, Size: 2, OverlapFiles: []string{"x.go"}},
			},
		},
		{
			name: "every member blocked holds nobody back",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Blocked: true, Files: []string{"x.go"}},
				{Number: 2, Base: "v5", Blocked: true, Files: []string{"x.go"}},
			},
			want: map[int]mergeTrainSlot{},
		},
		{
			name: "transitive overlap is one train",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Files: []string{"a.go", "b.go"}},
				{Number: 2, Base: "v5", Files: []string{"b.go", "c.go"}},
				{Number: 3, Base: "v5", Files: []string{"c.go", "d.go"}},
				{Number: 4, Base: "v5", Files: []string{"e.go"}},
			},
			want: map[int]mergeTrainSlot{
				1: {Front: 1, Position: 1, Size: 3, OverlapFiles: []string{"b.go"}},
				2: {Front: 1, Position: 2, Size: 3, OverlapFiles: []string{"b.go", "c.go"}},
				3: {Front: 1, Position: 3, Size: 3, OverlapFiles: []string{"c.go"}},
			},
		},
		{
			name: "different bases never share a train",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Files: []string{"x.go"}},
				{Number: 2, Base: "main", Files: []string{"x.go"}},
			},
			want: map[int]mergeTrainSlot{},
		},
		{
			name: "two separate trains on one base",
			members: []mergeTrainMember{
				{Number: 1, Base: "v5", Files: []string{"x.go"}},
				{Number: 2, Base: "v5", Files: []string{"y.go"}},
				{Number: 3, Base: "v5", Files: []string{"x.go"}},
				{Number: 4, Base: "v5", Files: []string{"y.go"}},
			},
			want: map[int]mergeTrainSlot{
				1: {Front: 1, Position: 1, Size: 2, OverlapFiles: []string{"x.go"}},
				3: {Front: 1, Position: 2, Size: 2, OverlapFiles: []string{"x.go"}},
				2: {Front: 2, Position: 1, Size: 2, OverlapFiles: []string{"y.go"}},
				4: {Front: 2, Position: 2, Size: 2, OverlapFiles: []string{"y.go"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planMergeTrains(tc.members)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("planMergeTrains = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestMergeTrainBlockingReason(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", false},
		{"conflicting", true},
		{"not-mergeable", true},
		{autoMergeReasonForkNotEditable, true},
		{"behind", false},
		{"updated-branch", false},
		{autoMergeReasonMergeTrainQueued, false},
	}
	for _, tc := range cases {
		if got := mergeTrainBlockingReason(tc.reason); got != tc.want {
			t.Errorf("mergeTrainBlockingReason(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func greenTrainPR(number int, files ...string) selfAuthoredPR {
	return selfAuthoredPR{
		number: number, author: testHiveAppBotLogin, mergeableState: "clean",
		statusState: "success", checkStatus: "completed", checkConclusion: "success",
		files: files,
	}
}

func TestSweepSelfAuthoredMergeTrain(t *testing.T) {
	cases := []struct {
		name       string
		disabled   bool
		prs        []selfAuthoredPR
		wantMerged []int
		wantQueued int
	}{
		{
			name: "only the front of an overlapping group merges",
			prs: []selfAuthoredPR{
				greenTrainPR(11, "src/pkg/dashboard/stage_tracker.go", "static/index.html"),
				greenTrainPR(12, "src/pkg/dashboard/stage_tracker.go"),
				greenTrainPR(13, "README.md"),
			},
			wantMerged: []int{11, 13},
			wantQueued: 1,
		},
		{
			name: "the oldest PR fronts the train",
			prs: func() []selfAuthoredPR {
				newer, older := greenTrainPR(11, "static/index.html"), greenTrainPR(12, "static/index.html")
				newer.createdAt, older.createdAt = "2026-10-28T10:00:00Z", "2026-10-27T10:00:00Z"
				return []selfAuthoredPR{newer, older}
			}(),
			wantMerged: []int{12},
			wantQueued: 1,
		},
		{
			name:     "disabled merges every green PR as before",
			disabled: true,
			prs: []selfAuthoredPR{
				greenTrainPR(11, "src/pkg/dashboard/stage_tracker.go"),
				greenTrainPR(12, "src/pkg/dashboard/stage_tracker.go"),
			},
			wantMerged: []int{11, 12},
		},
		{
			name: "disjoint PRs all merge",
			prs: []selfAuthoredPR{
				greenTrainPR(11, "a.go"),
				greenTrainPR(12, "b.go"),
			},
			wantMerged: []int{11, 12},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var merged []int
			api := newSelfAuthoredAutoMergeAPI(t, tc.prs, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			if tc.disabled {
				c.mergeTrainEnabledFn = func() bool { return false }
			}
			result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
			if err != nil {
				t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
			}
			sort.Ints(merged)
			if !reflect.DeepEqual(merged, tc.wantMerged) {
				t.Fatalf("merged = %v, want %v", merged, tc.wantMerged)
			}
			if queued := result.Skipped; queued != tc.wantQueued {
				t.Fatalf("skipped = %d, want %d train-queued sibling(s)", queued, tc.wantQueued)
			}
		})
	}
}

// A conflicting front must not stall its train: once the sweep has seen the
// head conflict, the next pass fronts the mergeable sibling instead.
func TestSweepSelfAuthoredMergeTrainSkipsConflictingFront(t *testing.T) {
	var merged []int
	dirty := greenTrainPR(11, "static/index.html")
	dirty.mergeableState = "dirty"
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{dirty, greenTrainPR(12, "static/index.html")}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)

	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(merged) != 0 {
		t.Fatalf("first pass merged %v, want the sibling queued behind the untried front", merged)
	}
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if !reflect.DeepEqual(merged, []int{12}) {
		t.Fatalf("second pass merged %v, want the mergeable sibling #12 as the new front", merged)
	}
}

func TestSweepSelfAuthoredMergeTrainCachesFilesByHead(t *testing.T) {
	var merged []int
	fetches11, fetches12 := 0, 0
	a, b := greenTrainPR(11, "x.go"), greenTrainPR(12, "x.go")
	a.filesFetches, b.filesFetches = &fetches11, &fetches12
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{a, b}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	if fetches11 != 1 || fetches12 != 1 {
		t.Fatalf("files fetches = #11:%d #12:%d, want one per unchanged head", fetches11, fetches12)
	}
}
