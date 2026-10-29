package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
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

func mtMember(n int, state string, created time.Time, files ...string) mergeTrainMember {
	return mergeTrainMember{Number: n, Base: "v5", HeadSHA: "sha" + strconv.Itoa(n), Files: files, Created: created, MergeableState: state}
}

func TestMergeTrainGroups(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		members []mergeTrainMember
		want    [][]int
		overlap []string
	}{
		{
			name: "disjoint files form no train",
			members: []mergeTrainMember{
				mtMember(1, "clean", t0, "a.go"),
				mtMember(2, "clean", t0.Add(time.Minute), "b.go"),
			},
		},
		{
			name: "oldest approved leads",
			members: []mergeTrainMember{
				mtMember(3, "clean", t0.Add(2*time.Minute), "tracker.go", "index.html"),
				mtMember(1, "clean", t0, "tracker.go"),
				mtMember(2, "clean", t0.Add(time.Minute), "index.html", "other.go"),
			},
			want:    [][]int{{1, 2, 3}},
			overlap: []string{"index.html", "tracker.go"},
		},
		{
			name: "a live dirty state does not reorder; the lane's blocked verdict does",
			members: []mergeTrainMember{
				mtMember(1, "dirty", t0, "tracker.go"),
				mtMember(2, "behind", t0.Add(time.Minute), "tracker.go"),
				mtMember(3, "clean", t0.Add(2*time.Minute), "tracker.go"),
			},
			want:    [][]int{{1, 2, 3}},
			overlap: []string{"tracker.go"},
		},
		{
			name: "transitive overlap joins one train, a stranger stays out",
			members: []mergeTrainMember{
				mtMember(1, "clean", t0, "a.go"),
				mtMember(2, "clean", t0.Add(time.Minute), "a.go", "b.go"),
				mtMember(3, "clean", t0.Add(2*time.Minute), "b.go"),
				mtMember(9, "clean", t0, "z.go"),
			},
			want:    [][]int{{1, 2, 3}},
			overlap: []string{"a.go", "b.go"},
		},
		{
			name: "different bases never share a train",
			members: []mergeTrainMember{
				mtMember(1, "clean", t0, "a.go"),
				{Number: 2, Base: "main", Files: []string{"a.go"}, MergeableState: "clean", Created: t0},
			},
		},
		{
			name: "equal ages tie on number",
			members: []mergeTrainMember{
				mtMember(5, "", t0, "a.go"),
				mtMember(4, "behind", t0, "a.go"),
				mtMember(6, "", t0, "a.go"),
			},
			want:    [][]int{{4, 5, 6}},
			overlap: []string{"a.go"},
		},
		{
			name: "a mergeable head fronts over an older blocked one",
			members: []mergeTrainMember{
				blocked(mtMember(1, "clean", t0, "x.go")),
				mtMember(2, "clean", t0.Add(time.Minute), "x.go"),
			},
			want:    [][]int{{2, 1}},
			overlap: []string{"x.go"},
		},
		{
			name: "every member blocked holds nobody back (fail-open)",
			members: []mergeTrainMember{
				blocked(mtMember(1, "clean", t0, "x.go")),
				blocked(mtMember(2, "clean", t0.Add(time.Minute), "x.go")),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := mergeTrainGroups(tc.members, planMergeTrains(tc.members))
			var got [][]int
			for _, g := range groups {
				got = append(got, g.Order)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("orders = %v, want %v", got, tc.want)
			}
			if len(groups) == 1 && !reflect.DeepEqual(groups[0].OverlapFiles, tc.overlap) {
				t.Fatalf("overlap = %v, want %v", groups[0].OverlapFiles, tc.overlap)
			}
			if len(groups) == 1 {
				g := groups[0]
				if g.Base != "v5" || g.Front() != tc.want[0][0] || g.Position(tc.want[0][len(tc.want[0])-1]) != len(tc.want[0]) || len(g.Siblings()) != len(tc.want[0])-1 {
					t.Fatalf("base/front/position/siblings wrong: %q %d %v", g.Base, g.Front(), g.Siblings())
				}
			}
		})
	}
}

// mergeTrainAPI is a GitHub API for one repository whose PRs have files and
// a mergeable state; it counts update-branch calls.
type mergeTrainAPI struct {
	mu      sync.Mutex
	files   map[int][]string
	state   map[int]string
	heads   map[int]string
	closed  map[int]bool
	updates []int
	gets    int
	lists   int
}

func newMergeTrainServer(t *testing.T, api *mergeTrainAPI) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/pulls/")
		parts := strings.Split(path, "/")
		n, _ := strconv.Atoi(parts[0])
		switch {
		case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "files":
			api.lists++
			var out []map[string]any
			for _, f := range api.files[n] {
				out = append(out, map[string]any{"filename": f})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && len(parts) == 1:
			api.gets++
			state := "open"
			if api.closed[n] {
				state = "closed"
			}
			head := api.heads[n]
			if head == "" {
				head = "sha" + strconv.Itoa(n)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": state, "mergeable_state": api.state[n],
				"head": map[string]any{"sha": head, "ref": "feat/" + strconv.Itoa(n), "repo": map[string]any{"full_name": "acme/widget"}},
				"base": map[string]any{"ref": "v5", "repo": map[string]any{"full_name": "acme/widget"}}})
		case r.Method == http.MethodPut && len(parts) == 2 && parts[1] == "update-branch":
			api.updates = append(api.updates, n)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func mtPR(n int, created time.Time, state string) *gh.PullRequest {
	return &gh.PullRequest{Number: gh.Ptr(n), State: gh.Ptr("open"), MergeableState: gh.Ptr(state), CreatedAt: &gh.Timestamp{Time: created},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha" + strconv.Itoa(n)), Ref: gh.Ptr("feat/" + strconv.Itoa(n)), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr("v5"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}}}
}

func newMergeTrainEngine(t *testing.T, srvURL string, policy MergeTrainPolicy, resolver RebaseResolver) (*Engine, *bytes.Buffer) {
	t.Helper()
	c := newAutoMergeSweepClient(srvURL)
	var logs bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	c.mergeTrainEnabledFn = func() bool { return policy.Enabled }
	c.mergeTrainMaxRebaseAttemptsFn = func() int { return policy.MaxRebaseAttempts }
	c.rebaseResolver = resolver
	return c, &logs
}

func admitAll(*gh.PullRequest) bool { return true }

func plan0(c *Engine, prs []*gh.PullRequest) *mergeTrainPlan {
	return c.mergeTrainSlots(context.Background(), "self", "acme/widget", "acme", "widget", prs, admitAll)
}

func TestMergeTrainSlotsSiblingsAndFront(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{10: {"tracker.go", "index.html"}, 11: {"tracker.go"}, 12: {"index.html", "x.go"}, 13: {"readme.md"}},
		state: map[int]string{10: "clean", 11: "behind", 12: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	// The list API carries no mergeable state; the planner reads the front's.
	prs := []*gh.PullRequest{mtPR(13, t0, ""), mtPR(11, t0.Add(time.Minute), ""), mtPR(10, t0.Add(2*time.Minute), ""), mtPR(12, t0.Add(3*time.Minute), "")}
	plan := plan0(c, prs)
	if !plan.isFront(11) || !plan.isSibling(10) || !plan.isSibling(12) || plan.isSibling(13) || plan.isFront(13) {
		t.Fatalf("front/siblings wrong: front11=%v sib10=%v sib12=%v 13 in train=%v", plan.isFront(11), plan.isSibling(10), plan.isSibling(12), plan.isSibling(13) || plan.isFront(13))
	}
	g, _ := plan.group(11)
	if !reflect.DeepEqual(g.Order, []int{11, 10, 12}) {
		t.Fatalf("order = %v, want oldest first", g.Order)
	}
	reason := c.mergeTrainSkip(plan, 12)
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok || mt.State != prstage.MergeTrainStateQueuedBehind || mt.Front != 11 || mt.Position != 3 || mt.Size != 3 {
		t.Fatalf("sibling reason %q parsed %+v", reason, mt)
	}
	if got := mt.Sentence(); !strings.Contains(got, "behind #11 in merge train (3rd of 3, overlapping files: index.html, tracker.go)") {
		t.Fatalf("sentence = %q", got)
	}
	// A front that is not conflicting is the lane's to merge on its normal path.
	if r, handled := c.mergeTrainFront(context.Background(), plan, prs[1]); handled || r != "" {
		t.Fatalf("behind front handled=%v reason=%q", handled, r)
	}
	// One INFO line per PR joining the train; a replan with the same slots
	// adds none, and the files are served from the head-SHA cache.
	lists, gets := api.lists, api.gets
	_ = plan0(c, prs)
	if n := strings.Count(logs.String(), `msg="automerge merge train"`); n != 3 {
		t.Fatalf("transition lines = %d, want 3\n%s", n, logs.String())
	}
	if api.lists != lists {
		t.Fatalf("files re-listed on an unchanged head: %d -> %d", lists, api.lists)
	}
	if api.gets <= gets {
		t.Fatalf("the front's mergeable state must be re-read each pass")
	}
	// The lane saw the front conflict: the next pass fronts #10 and every
	// slot change logs once.
	c.noteMergeTrainOutcome("acme", "widget", 11, "sha11", "conflicting")
	plan = plan0(c, prs)
	if !plan.isFront(10) || !plan.isSibling(11) {
		t.Fatalf("expected #10 to lead once #11 was seen conflicting; groups %v", plan.groups)
	}
	if n := strings.Count(logs.String(), `msg="automerge merge train"`); n != 6 {
		t.Fatalf("transition lines = %d, want 6", n)
	}
}

func TestMergeTrainSlotsDisabledAndSingletons(t *testing.T) {
	t0 := time.Now()
	api := &mergeTrainAPI{files: map[int][]string{1: {"a.go"}, 2: {"a.go"}}, state: map[int]string{1: "clean", 2: "clean"}}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, _ := newMergeTrainEngine(t, srv.URL, MergeTrainPolicy{Enabled: false}, nil)
	prs := []*gh.PullRequest{mtPR(1, t0, ""), mtPR(2, t0, "")}
	plan := plan0(c, prs)
	if len(plan.groups) != 0 || api.lists != 0 {
		t.Fatalf("disabled train planned %d groups and listed %d files", len(plan.groups), api.lists)
	}
	c, _ = newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	// A lone candidate on its base costs no API call at all.
	plan = plan0(c, prs[:1])
	if len(plan.groups) != 0 || api.lists != 0 {
		t.Fatalf("singleton planned %d groups, listed %d", len(plan.groups), api.lists)
	}
	// Prefiltered-out PRs (draft, hold, …) never join.
	plan = c.mergeTrainSlots(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) bool { return false })
	if len(plan.groups) != 0 {
		t.Fatalf("prefiltered PRs joined a train")
	}
	if c.mergeTrainCandidate("label:needs-rebase") || c.mergeTrainCandidate("label:hold") || !c.mergeTrainCandidate("") {
		t.Fatal("candidate gate without a resolver: only a clean prefilter admits")
	}
	c.rebaseResolver = RebaseResolverFunc(func(context.Context, RebaseRequest) error { return nil })
	if !c.mergeTrainCandidate("label:needs-rebase") || c.mergeTrainCandidate("label:hold") {
		t.Fatal("candidate gate with a resolver: needs-rebase admits, hold does not")
	}
}

func TestMergeTrainAfterFrontMergeBehindAndDirty(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{20: {"a.go"}, 21: {"a.go"}, 22: {"a.go"}, 23: {"a.go"}},
		state: map[int]string{20: "clean", 21: "clean", 22: "clean", 23: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	var mu sync.Mutex
	var requests []RebaseRequest
	resolver := RebaseResolverFunc(func(_ context.Context, req RebaseRequest) error {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, req)
		return nil
	})
	c, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, resolver)
	prs := []*gh.PullRequest{mtPR(20, t0, ""), mtPR(21, t0.Add(time.Minute), ""), mtPR(22, t0.Add(2*time.Minute), ""), mtPR(23, t0.Add(3*time.Minute), "")}
	plan := plan0(c, prs)
	if !plan.isFront(20) {
		t.Fatalf("front = %v", plan.groups)
	}
	// The front merged: GitHub now says #21 is behind, #22 dirty, #23 closed.
	api.mu.Lock()
	api.state[21], api.state[22] = "behind", "dirty"
	api.closed = map[int]bool{23: true}
	api.mu.Unlock()
	c.mergeTrainAfterFrontMerge(context.Background(), plan, 20)
	api.mu.Lock()
	updates := append([]int(nil), api.updates...)
	api.mu.Unlock()
	if !reflect.DeepEqual(updates, []int{21}) {
		t.Fatalf("update-branch on %v, want [21]", updates)
	}
	mu.Lock()
	got := append([]RebaseRequest(nil), requests...)
	mu.Unlock()
	if len(got) != 1 || got[0].Number != 22 || got[0].Front != 20 || got[0].Attempt != 1 || got[0].MaxAttempts != 2 || got[0].Base != "v5" || got[0].Repo != "acme/widget" || got[0].Lane != "self" {
		t.Fatalf("rebase requests = %+v", got)
	}
	if c.mergeTrainLastFront("acme", "widget", "v5") != 20 {
		t.Fatal("merged front not recorded")
	}
	if s := got[0].String(); !strings.Contains(s, "acme/widget#22 onto v5 (behind #20, attempt 1/2, self)") {
		t.Fatalf("String() = %q", s)
	}
}

func TestMergeTrainFrontRebaseLoopGuard(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{30: {"a.go"}, 31: {"a.go"}},
		state: map[int]string{30: "dirty", 31: "dirty"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	var mu sync.Mutex
	calls := 0
	resolver := RebaseResolverFunc(func(context.Context, RebaseRequest) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	})
	c, logs := newMergeTrainEngine(t, srv.URL, MergeTrainPolicy{Enabled: true, MaxRebaseAttempts: 2}, resolver)
	// #29 merged earlier on this base: the dirty front is "behind" it.
	c.noteMergeTrainFrontMerged("acme", "widget", "v5", 29)
	prs := []*gh.PullRequest{mtPR(30, t0, ""), mtPR(31, t0.Add(time.Minute), "")}
	var states []string
	for pass := 0; pass < 3; pass++ {
		plan := plan0(c, prs)
		reason, handled := c.mergeTrainFront(context.Background(), plan, prs[0])
		if !handled {
			t.Fatalf("pass %d: dirty front with a resolver must be handled", pass)
		}
		mt, ok := prstage.ParseMergeTrain(reason)
		if !ok {
			t.Fatalf("pass %d: reason %q", pass, reason)
		}
		states = append(states, mt.State+":"+strconv.Itoa(mt.Attempt)+"/"+strconv.Itoa(mt.MaxAttempts))
	}
	want := []string{"rebasing:1/2", "rebasing:2/2", "rebase-exhausted:2/2"}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	// Past the guard the head stops fronting, so it cannot stall its train.
	plan := plan0(c, prs)
	if !plan.isFront(31) || !plan.isSibling(30) {
		t.Fatalf("exhausted front still leads: groups %v", plan.groups)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("resolver calls = %d, want 2 (loop guard)", calls)
	}
	if !strings.Contains(logs.String(), "rebase requested for the front") {
		t.Fatalf("missing rebase log\n%s", logs.String())
	}
	// The exhausted reason classifies to a human stage with the action.
	mt, _ := prstage.ParseMergeTrain(mergeTrainFrontReason(&plan.groups[0], prstage.MergeTrainStateRebaseExhausted, 2, 2))
	if mt.Sentence() == "" || !strings.Contains(mt.Sentence(), "after 2 rebase attempts") {
		t.Fatalf("exhausted sentence = %q", mt.Sentence())
	}
}

func TestMergeTrainFrontNoResolverFallsThroughOnce(t *testing.T) {
	t0 := time.Now()
	api := &mergeTrainAPI{files: map[int][]string{40: {"a.go"}, 41: {"a.go"}}, state: map[int]string{40: "dirty", 41: "dirty"}}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	prs := []*gh.PullRequest{mtPR(40, t0, ""), mtPR(41, t0.Add(time.Minute), "")}
	for i := 0; i < 2; i++ {
		plan := plan0(c, prs)
		if r, handled := c.mergeTrainFront(context.Background(), plan, prs[0]); handled || r != "" {
			t.Fatalf("no resolver: front must fall through, got handled=%v %q", handled, r)
		}
	}
	if n := strings.Count(logs.String(), "no rebase resolver is wired"); n != 1 {
		t.Fatalf("no-resolver line logged %d times, want once\n%s", n, logs.String())
	}
	// Without a resolver a sibling's dirty state after the merge is a no-op.
	plan := plan0(c, prs)
	c.mergeTrainAfterFrontMerge(context.Background(), plan, 40)
	if len(api.updates) != 0 {
		t.Fatalf("unexpected update-branch calls %v", api.updates)
	}
}

func TestMergeTrainPolicyDefaults(t *testing.T) {
	var c *Engine
	if p := c.mergeTrainPolicyNow(); p != DefaultMergeTrainPolicy {
		t.Fatalf("nil engine policy = %+v", p)
	}
	if p := (&Engine{}).mergeTrainPolicyNow(); p != DefaultMergeTrainPolicy {
		t.Fatalf("unhooked engine policy = %+v", p)
	}
	c = &Engine{mergeTrainMaxRebaseAttemptsFn: func() int { return 0 }, mergeTrainEnabledFn: func() bool { return false }}
	if p := c.mergeTrainPolicyNow(); p.MaxRebaseAttempts != 2 || p.Enabled {
		t.Fatalf("zero max attempts must default to 2 and the switch must read live, got %+v", p)
	}
	var plan *mergeTrainPlan
	if plan.isFront(1) || plan.isSibling(1) {
		t.Fatal("nil plan has no members")
	}
}

func blocked(m mergeTrainMember) mergeTrainMember {
	m.Blocked = true
	return m
}

// TestMergeTrainBlockedHeadMemory: the lane's verdict per head decides
// whether it may front; a conflicting/red/terminal verdict marks the exact
// head, any other verdict clears it, and a new head starts clean.
func TestMergeTrainBlockedHeadMemory(t *testing.T) {
	for reason, want := range map[string]bool{
		"":                                   false,
		"conflicting":                        true,
		"label:needs-rebase":                 true,
		"check-pending":                      false,
		"not-mergeable":                      true,
		autoMergeReasonForkNotEditable:       true,
		autoMergeReasonClosedSuperseded:      true,
		"required-check-failing:ci/build":    true,
		workflowApprovalRequiredPrefix + "x": true,
	} {
		if got := mergeTrainBlockingReason(reason); got != want {
			t.Fatalf("mergeTrainBlockingReason(%q) = %v, want %v", reason, got, want)
		}
	}
	c := &Engine{}
	c.noteMergeTrainOutcome("acme", "widget", 7, "sha1", "not-mergeable")
	if !c.mergeTrainHeadBlocked("acme", "widget", 7, "sha1") || c.mergeTrainHeadBlocked("acme", "widget", 7, "sha2") {
		t.Fatal("the exact head is blocked, a new head is not")
	}
	c.noteMergeTrainOutcome("acme", "widget", 7, "sha1", "")
	if c.mergeTrainHeadBlocked("acme", "widget", 7, "sha1") {
		t.Fatal("a clean verdict clears the mark")
	}
	c.noteMergeTrainOutcome("acme", "widget", 7, "", "not-mergeable")
	if c.mergeTrainHeadBlocked("acme", "widget", 7, "") {
		t.Fatal("no sha, no mark")
	}
	var nilEngine *Engine
	nilEngine.noteMergeTrainOutcome("acme", "widget", 7, "sha1", "not-mergeable")
	if nilEngine.mergeTrainHeadBlocked("acme", "widget", 7, "sha1") {
		t.Fatal("nil engine blocks nothing")
	}
}
