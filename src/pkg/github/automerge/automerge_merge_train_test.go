package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func mtMember(n int, state string, created time.Time, files ...string) mergeTrainMember {
	return mergeTrainMember{Number: n, Base: "v5", HeadSHA: "sha" + strconv.Itoa(n), Files: files, CreatedAt: created, MergeableState: state}
}

func TestGroupMergeTrains(t *testing.T) {
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
			name: "oldest approved leads when all are clean",
			members: []mergeTrainMember{
				mtMember(3, "clean", t0.Add(2*time.Minute), "tracker.go", "index.html"),
				mtMember(1, "clean", t0, "tracker.go"),
				mtMember(2, "clean", t0.Add(time.Minute), "index.html", "other.go"),
			},
			want:    [][]int{{1, 2, 3}},
			overlap: []string{"index.html", "tracker.go"},
		},
		{
			name: "clean mergeable leads over an older dirty one",
			members: []mergeTrainMember{
				mtMember(1, "dirty", t0, "tracker.go"),
				mtMember(2, "behind", t0.Add(time.Minute), "tracker.go"),
				mtMember(3, "clean", t0.Add(2*time.Minute), "tracker.go"),
			},
			want:    [][]int{{3, 2, 1}},
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
				{Number: 2, Base: "main", Files: []string{"a.go"}, MergeableState: "clean", CreatedAt: t0},
			},
		},
		{
			name: "unknown state sorts between clean and behind, ties on number",
			members: []mergeTrainMember{
				mtMember(5, "", t0, "a.go"),
				mtMember(4, "behind", t0, "a.go"),
				mtMember(6, "", t0, "a.go"),
			},
			want:    [][]int{{5, 6, 4}},
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
			groups := groupMergeTrains(tc.members)
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
				if g.Front() != tc.want[0][0] || g.Position(tc.want[0][len(tc.want[0])-1]) != len(tc.want[0]) || len(g.Siblings()) != len(tc.want[0])-1 {
					t.Fatalf("front/position/siblings wrong: %d %v", g.Front(), g.Siblings())
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
	c.mergeTrainPolicy = func() MergeTrainPolicy { return policy }
	c.rebaseResolver = resolver
	return c, &logs
}

func TestPlanMergeTrainsSiblingsAndFront(t *testing.T) {
	t0 := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{10: {"tracker.go", "index.html"}, 11: {"tracker.go"}, 12: {"index.html", "x.go"}, 13: {"readme.md"}, 14: {"x.go"}},
		state: map[int]string{10: "clean", 11: "behind", 12: "behind", 14: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	// The list API carries no mergeable state; the planner reads it.
	prs := []*gh.PullRequest{mtPR(13, t0, ""), mtPR(11, t0.Add(time.Minute), ""), mtPR(10, t0.Add(2*time.Minute), ""), mtPR(12, t0.Add(3*time.Minute), ""), mtPR(14, t0.Add(4*time.Minute), "")}
	prefilter := func(pr *gh.PullRequest) string {
		if pr.GetNumber() == 11 {
			return "label:needs-rebase" // admitted: a merely-behind head is what the train updates
		}
		return ""
	}
	plan := c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, prefilter)
	if !plan.isFront(10) || !plan.isSibling(11) || !plan.isSibling(12) || !plan.isSibling(14) || plan.isSibling(13) || plan.isFront(13) {
		t.Fatalf("front/siblings wrong: front10=%v sib11=%v sib12=%v sib14=%v 13 in train=%v", plan.isFront(10), plan.isSibling(11), plan.isSibling(12), plan.isSibling(14), plan.isSibling(13) || plan.isFront(13))
	}
	g, _ := plan.group(10)
	if !reflect.DeepEqual(g.Order, []int{10, 14, 11, 12}) {
		t.Fatalf("order = %v, want clean (earliest first), then behind", g.Order)
	}
	reason := c.mergeTrainSkip(plan, 12)
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok || mt.State != prstage.MergeTrainStateQueuedBehind || mt.Front != 10 || mt.Position != 4 || mt.Size != 4 {
		t.Fatalf("sibling reason %q parsed %+v", reason, mt)
	}
	if got := mt.Sentence(); !strings.Contains(got, "behind #10 in merge train (4th of 4, overlapping files: index.html, tracker.go, x.go)") {
		t.Fatalf("sentence = %q", got)
	}
	// A clean front is the lane's to merge on its normal path.
	if r, handled := c.mergeTrainFront(context.Background(), plan, prs[2]); handled || r != "" {
		t.Fatalf("clean front handled=%v reason=%q", handled, r)
	}
	// One INFO transition line for the train; a replan with the same order
	// adds none, and the files are served from the head-SHA cache.
	lists, gets := api.lists, api.gets
	_ = c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, prefilter)
	if n := strings.Count(logs.String(), "msg=\"merge train\""); n != 1 {
		t.Fatalf("transition lines = %d, want 1\n%s", n, logs.String())
	}
	if api.lists != lists {
		t.Fatalf("files re-listed on an unchanged head: %d -> %d", lists, api.lists)
	}
	if api.gets <= gets {
		t.Fatalf("mergeable state must be re-read each pass")
	}
	// A new front is a transition — and a head that went conflicting on
	// its own leaves the train and its count (hivecommons/hive#15395).
	api.mu.Lock()
	api.state[10] = "dirty"
	api.mu.Unlock()
	plan = c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, prefilter)
	if !plan.isFront(14) || plan.isSibling(10) || plan.isFront(10) {
		t.Fatalf("expected #14 to lead and #10 to drop out once #10 went dirty; groups %+v", plan.groups)
	}
	// #11 only ever overlapped #10, so it is no longer anyone's sibling.
	if g, _ := plan.group(14); !reflect.DeepEqual(g.Order, []int{14, 12}) || plan.isSibling(11) {
		t.Fatalf("order = %v (11 sibling=%v), want the conflicting #10 gone", g.Order, plan.isSibling(11))
	}
	if n := strings.Count(logs.String(), "msg=\"merge train\""); n != 2 {
		t.Fatalf("transition lines = %d, want 2", n)
	}
}

func TestPlanMergeTrainsDisabledAndSingletons(t *testing.T) {
	t0 := time.Now()
	api := &mergeTrainAPI{files: map[int][]string{1: {"a.go"}, 2: {"a.go"}}, state: map[int]string{1: "clean", 2: "clean"}}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, _ := newMergeTrainEngine(t, srv.URL, MergeTrainPolicy{Enabled: false}, nil)
	prs := []*gh.PullRequest{mtPR(1, t0, ""), mtPR(2, t0, "")}
	plan := c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) string { return "" })
	if len(plan.groups) != 0 || api.lists != 0 {
		t.Fatalf("disabled train planned %d groups and listed %d files", len(plan.groups), api.lists)
	}
	c, _ = newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	// A lone candidate on its base costs no API call at all.
	plan = c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs[:1], func(*gh.PullRequest) string { return "" })
	if len(plan.groups) != 0 || api.lists != 0 {
		t.Fatalf("singleton planned %d groups, listed %d", len(plan.groups), api.lists)
	}
	// Prefiltered-out PRs (draft, hold, …) never join.
	plan = c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) string { return "draft" })
	if len(plan.groups) != 0 {
		t.Fatalf("prefiltered PRs joined a train")
	}
	if mergeTrainCandidate("label:needs-rebase") != true || mergeTrainCandidate("label:hold") {
		t.Fatal("candidate gate: needs-rebase admits, hold does not")
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
	plan := c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) string { return "" })
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
	prs := []*gh.PullRequest{mtPR(30, t0, ""), mtPR(31, t0.Add(time.Minute), "")}
	// Two heads that arrived conflicting on their own are nobody's train:
	// the planner carries neither (hivecommons/hive#15395, #15307).
	if plan := plan0(c, prs); plan.isFront(30) || plan.isSibling(31) || len(plan.groups) != 0 {
		t.Fatalf("conflicting outsiders must not form a train: %+v", plan.groups)
	}
	// #29 merged earlier on this base and the train asked for #30's rebase
	// as its sibling: #30 is the train's own repair, so it stays a member
	// while the loop guard has attempts left.
	c.noteMergeTrainFrontMerged("acme", "widget", "v5", 29)
	c.bumpMergeTrainAttempts("acme", "widget", 30, 29)
	c.bumpMergeTrainAttempts("acme", "widget", 31, 29)
	plan := plan0(c, prs)
	if !plan.isFront(30) || !plan.isSibling(31) {
		t.Fatalf("members under repair must ride the train: %+v", plan.groups)
	}
	reason, handled := c.mergeTrainFront(context.Background(), plan, prs[0])
	if !handled {
		t.Fatal("dirty front under repair with a resolver must be handled")
	}
	mt, ok := prstage.ParseMergeTrain(reason)
	if !ok || mt.State+":"+strconv.Itoa(mt.Attempt)+"/"+strconv.Itoa(mt.MaxAttempts) != "rebasing:2/2" {
		t.Fatalf("reason %q parsed %+v", reason, mt)
	}
	// Guard spent on #30: it leaves the train, and #31 alone is no train —
	// the conflicting path for a person takes them, not a train slot.
	plan = plan0(c, prs)
	if len(plan.groups) != 0 {
		t.Fatalf("exhausted members must leave the train: %+v", plan.groups)
	}
	if _, handled := c.mergeTrainFront(context.Background(), plan, prs[0]); handled {
		t.Fatal("a PR outside every train is never handled as a front")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1 (loop guard)", calls)
	}
	if !strings.Contains(logs.String(), "rebase requested for the front") {
		t.Fatalf("missing rebase log\n%s", logs.String())
	}
	// The exhausted reason still classifies to a human stage with the action.
	g := mergeTrainGroup{Base: "v5", Order: []int{30, 31}, OverlapFiles: []string{"a.go"}}
	mt, _ = prstage.ParseMergeTrain(mergeTrainFrontReason(&g, prstage.MergeTrainStateRebaseExhausted, 2, 2))
	if mt.Sentence() == "" || !strings.Contains(mt.Sentence(), "after 2 rebase attempts") {
		t.Fatalf("exhausted sentence = %q", mt.Sentence())
	}
}

// TestPlanMergeTrainsExcludesReturnedAndConflicting: hive's own #15307
// (mergeable CONFLICTING, stage 072 returned-to-contributor) was counted
// "queued-behind#15424:4/6" and drew the train's idle comments
// (hivecommons/hive#15395). A PR at 072 — by label or by the stage store —
// or one GitHub says is conflicting on its own is neither a member nor
// part of the queue's size.
func TestPlanMergeTrainsExcludesReturnedAndConflicting(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{15424: {"a.go"}, 15307: {"a.go"}, 15430: {"a.go"}, 15431: {"a.go"}},
		state: map[int]string{15424: "clean", 15307: "dirty", 15430: "clean", 15431: "behind"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, RebaseResolverFunc(func(context.Context, RebaseRequest) error { return nil }))
	c.stageStore = stagelabels.NewStore("", func() time.Time { return t0 })
	c.stageStore.Observe(stagelabels.Key("acme/widget", 15431), stagelabels.ReturnedToContributor, "ci-red")
	// A front merged on this base and other members were rebased: #15307
	// is still an outsider — the train never asked for its rebase.
	c.noteMergeTrainFrontMerged("acme", "widget", "v5", 15300)
	returned := mtPR(15430, t0.Add(2*time.Minute), "")
	returned.Labels = []*gh.Label{{Name: gh.Ptr(stagelabels.ReturnedToContributor.Label())}}
	prs := []*gh.PullRequest{mtPR(15424, t0, ""), mtPR(15307, t0.Add(time.Minute), ""), returned, mtPR(15431, t0.Add(3*time.Minute), "")}
	plan := plan0(c, prs)
	for _, n := range []int{15307, 15430, 15431} {
		if plan.isSibling(n) || plan.isFront(n) {
			t.Errorf("#%d must be out of every train: %+v", n, plan.groups)
		}
	}
	if len(plan.groups) != 0 {
		t.Fatalf("only #15424 is left, and one PR is no train: %+v", plan.groups)
	}
	for _, want := range []string{"why=returned-to-contributor", "why=conflicting"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing skip log %q\n%s", want, logs.String())
		}
	}
	// With a clean partner the survivors form the train and the size counts
	// members only: 2, not 4.
	api.mu.Lock()
	api.state[15307] = "clean"
	api.mu.Unlock()
	plan = plan0(c, prs)
	g, ok := plan.group(15424)
	if !ok || !reflect.DeepEqual(g.Order, []int{15424, 15307}) {
		t.Fatalf("train = %+v", plan.groups)
	}
	mt, _ := prstage.ParseMergeTrain(c.mergeTrainSkip(plan, 15307))
	if mt.Size != 2 || mt.Position != 2 {
		t.Fatalf("size/position = %d/%d, want 2/2", mt.Size, mt.Position)
	}
}

func plan0(c *Engine, prs []*gh.PullRequest) *mergeTrainPlan {
	return c.planMergeTrains(context.Background(), "self", "acme/widget", "acme", "widget", prs, func(*gh.PullRequest) string { return "" })
}

func TestMergeTrainFrontNoResolverFallsThroughOnce(t *testing.T) {
	t0 := time.Now()
	api := &mergeTrainAPI{files: map[int][]string{40: {"a.go"}, 41: {"a.go"}}, state: map[int]string{40: "dirty", 41: "dirty"}}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	prs := []*gh.PullRequest{mtPR(40, t0, ""), mtPR(41, t0.Add(time.Minute), "")}
	// Without a resolver no dirty head is under repair, so two conflicting
	// heads are no train at all and the lane's conflicting path takes them.
	for i := 0; i < 2; i++ {
		plan := plan0(c, prs)
		if len(plan.groups) != 0 {
			t.Fatalf("no resolver: conflicting heads form no train: %+v", plan.groups)
		}
		if r, handled := c.mergeTrainFront(context.Background(), plan, prs[0]); handled || r != "" {
			t.Fatalf("no resolver: front must fall through, got handled=%v %q", handled, r)
		}
	}
	if n := strings.Count(logs.String(), "why=conflicting"); n != 2 {
		t.Fatalf("skip line logged %d times, want once per head\n%s", n, logs.String())
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
	c = &Engine{mergeTrainPolicy: func() MergeTrainPolicy { return MergeTrainPolicy{Enabled: true} }}
	if p := c.mergeTrainPolicyNow(); p.MaxRebaseAttempts != 2 {
		t.Fatalf("zero max attempts must default to 2, got %d", p.MaxRebaseAttempts)
	}
	var plan *mergeTrainPlan
	if plan.isFront(1) || plan.isSibling(1) {
		t.Fatal("nil plan has no members")
	}
}

func TestMergeTrainPolicyOptionLegacySwitch(t *testing.T) {
	if mergeTrainPolicyOption(Options{}) != nil {
		t.Fatal("no switch, no policy func")
	}
	full := func() MergeTrainPolicy { return MergeTrainPolicy{Enabled: false, MaxRebaseAttempts: 5} }
	if got := mergeTrainPolicyOption(Options{MergeTrain: full, MergeTrainEnabled: func() bool { return true }})(); got.MaxRebaseAttempts != 5 || got.Enabled {
		t.Fatalf("full policy must win: %+v", got)
	}
	off := false
	got := mergeTrainPolicyOption(Options{MergeTrainEnabled: func() bool { return off }})
	if p := got(); p.Enabled || p.MaxRebaseAttempts != DefaultMergeTrainPolicy.MaxRebaseAttempts {
		t.Fatalf("legacy off: %+v", p)
	}
	off = true
	if p := got(); !p.Enabled {
		t.Fatalf("legacy on: %+v", p)
	}
}

func blocked(m mergeTrainMember) mergeTrainMember {
	m.Blocked = true
	return m
}

// TestMergeTrainBlockedHeadMemory: the lane's verdict per head decides
// whether it may front; a red/terminal verdict marks the exact head, any
// other verdict clears it, and a new head starts clean. A conflict is not
// blocking: the train's own rebase step repairs it.
func TestMergeTrainBlockedHeadMemory(t *testing.T) {
	for reason, want := range map[string]bool{
		"":                                   false,
		"conflicting":                        false,
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
	c.noteMergeTrainOutcome("self", "acme", "widget", 7, "sha1", "not-mergeable")
	if !c.mergeTrainHeadBlocked("self", "acme", "widget", 7, "sha1") || c.mergeTrainHeadBlocked("self", "acme", "widget", 7, "sha2") {
		t.Fatal("the exact head is blocked, a new head is not")
	}
	if c.mergeTrainHeadBlocked("trusted", "acme", "widget", 7, "sha1") {
		t.Fatal("one lane's verdict is not another lane's")
	}
	c.noteMergeTrainOutcome("self", "acme", "widget", 7, "sha1", "")
	if c.mergeTrainHeadBlocked("self", "acme", "widget", 7, "sha1") {
		t.Fatal("a clean verdict clears the mark")
	}
	c.noteMergeTrainOutcome("self", "acme", "widget", 7, "", "not-mergeable")
	if c.mergeTrainHeadBlocked("self", "acme", "widget", 7, "") {
		t.Fatal("no sha, no mark")
	}
	var nilEngine *Engine
	nilEngine.noteMergeTrainOutcome("self", "acme", "widget", 7, "sha1", "not-mergeable")
	if nilEngine.mergeTrainHeadBlocked("self", "acme", "widget", 7, "sha1") {
		t.Fatal("nil engine blocks nothing")
	}
}

// TestMergeTrainHumanMergeNeverMember: a PR the lane leaves to a person
// (untrusted author, no linked issue, the title/label human-merge rule,
// a human-merge path) is not a train member at all — not a front, not a
// sibling — so the PRs behind it form their own train with a head that can
// merge. hive's #15425 (untrusted author, parked at 090) fronted a train of
// three and the two behind it waited on a merge that was never coming.
func TestMergeTrainHumanMergeNeverMember(t *testing.T) {
	for reason, want := range map[string]bool{
		"":                          false,
		"soaking":                   false,
		"check-pending":             false,
		"not-mergeable":             false,
		"untrusted-author-role":     true,
		"author-permission-missing": true,
		prstage.ContributorLaneReasonNoLinkedIssue: true,
		prstage.ContributorLaneReasonClaimTier:     true,
		prstage.AutoMergeReasonHumanMergeRequired:  true,
		prstage.ContributorLaneReasonWorkflowPath:  true,
		prstage.AutoMergeReasonMergeRefusedReview:  true,
	} {
		if got := mergeTrainHumanMergeReason(reason); got != want {
			t.Fatalf("mergeTrainHumanMergeReason(%q) = %v, want %v", reason, got, want)
		}
	}
	t0 := time.Now()
	api := &mergeTrainAPI{files: map[int][]string{60: {"a.go"}, 61: {"a.go"}, 62: {"a.go"}}, state: map[int]string{60: "clean", 61: "clean", 62: "clean"}}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	prs := []*gh.PullRequest{mtPR(60, t0, "clean"), mtPR(61, t0.Add(time.Minute), "clean"), mtPR(62, t0.Add(2*time.Minute), "clean")}
	// The oldest, cleanest PR is an untrusted author's: the lane said so.
	c.noteMergeTrainOutcome("self", "acme", "widget", 60, "sha60", prstage.ContributorLaneReasonNoLinkedIssue)
	plan := plan0(c, prs)
	if plan.isFront(60) || plan.isSibling(60) {
		t.Fatalf("a human-merge PR rides no train: front=%v sibling=%v", plan.isFront(60), plan.isSibling(60))
	}
	if !plan.isFront(61) || !plan.isSibling(62) {
		t.Fatalf("the PRs behind it form their own train: front61=%v sib62=%v groups=%d", plan.isFront(61), plan.isSibling(62), len(plan.groups))
	}
	// The same verdict from another lane does not touch this lane's train.
	c2, _ := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, nil)
	c2.noteMergeTrainOutcome("trusted", "acme", "widget", 60, "sha60", "untrusted-author-role")
	if plan := plan0(c2, prs); !plan.isFront(60) {
		t.Fatal("another lane's verdict must not evict the member from this lane's train")
	}
	// A new head starts clean: the lane re-judges it.
	c.noteMergeTrainOutcome("self", "acme", "widget", 60, "sha60", "")
	if plan := plan0(c, prs); !plan.isFront(60) {
		t.Fatal("a clean verdict re-admits the PR")
	}
}

// TestPlanMergeTrainsExcludesUnderRepair: hive's own #15405 led a train's
// header as "1st: #15405" while a maintainer's changes-requested had it at
// 070 fixing. A PR at 070 / 071 / 085 — by label or by the stage store —
// is somebody's to repair and takes no slot; the survivors form the train
// among themselves.
func TestPlanMergeTrainsExcludesUnderRepair(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	api := &mergeTrainAPI{
		files: map[int][]string{15405: {"operator-reference.md"}, 15406: {"operator-reference.md"}, 15407: {"operator-reference.md"}, 15408: {"operator-reference.md"}},
		state: map[int]string{15405: "clean", 15406: "clean", 15407: "clean", 15408: "clean"},
	}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	c, logs := newMergeTrainEngine(t, srv.URL, DefaultMergeTrainPolicy, RebaseResolverFunc(func(context.Context, RebaseRequest) error { return nil }))
	c.stageStore = stagelabels.NewStore("", func() time.Time { return t0 })
	c.stageStore.Observe(stagelabels.Key("acme/widget", 15407), stagelabels.NeedsDecision, "rebase-failed-twice")
	fixing := mtPR(15405, t0, "")
	fixing.Labels = []*gh.Label{{Name: gh.Ptr(stagelabels.Fixing.Label())}}
	needsAuthor := mtPR(15408, t0.Add(3*time.Minute), "")
	needsAuthor.Labels = []*gh.Label{{Name: gh.Ptr(stagelabels.NeedsAuthor.Label())}}
	prs := []*gh.PullRequest{fixing, mtPR(15406, t0.Add(time.Minute), ""), mtPR(15407, t0.Add(2*time.Minute), ""), needsAuthor}
	plan := plan0(c, prs)
	for _, n := range []int{15405, 15407, 15408} {
		if plan.isSibling(n) || plan.isFront(n) {
			t.Errorf("#%d is under repair and must be out of every train: %+v", n, plan.groups)
		}
	}
	if len(plan.groups) != 0 {
		t.Fatalf("only #15406 is left, and one PR is no train: %+v", plan.groups)
	}
	for _, want := range []string{"why=fixing", "why=needs-decision", "why=needs-author"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing skip log %q\n%s", want, logs.String())
		}
	}
	// Once the fixer is done (label gone) the two clean PRs form the train
	// and #15405 leads it by age.
	fixing.Labels = nil
	plan = plan0(c, prs)
	g, ok := plan.group(15405)
	if !ok || !reflect.DeepEqual(g.Order, []int{15405, 15406}) {
		t.Fatalf("train = %+v", plan.groups)
	}
}
