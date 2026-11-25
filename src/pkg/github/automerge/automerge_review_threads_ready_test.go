package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// readyThreadsTransport is the real client with the 090 thread resolution
// replaced by a recorder.
type readyThreadsTransport struct {
	*hgithub.Client
	mu      sync.Mutex
	calls   []string
	replies []bool
	outcome hgithub.OwnReviewThreadsOutcome
	err     error
}

func (t *readyThreadsTransport) ResolveOwnReviewThreadsForHumanMerge(_ context.Context, repo string, number int, base string, reply bool) (hgithub.OwnReviewThreadsOutcome, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, repo+"#"+strconv.Itoa(number)+"@"+base)
	t.replies = append(t.replies, reply)
	return t.outcome, t.err
}

func (t *readyThreadsTransport) replyList() []bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]bool(nil), t.replies...)
}

func (t *readyThreadsTransport) callList() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

// readyThreadsStageEngine is a contributor PR #9 on acme/widget (base main)
// whose only objection is trust: an approved, green head lands at 090.
func readyThreadsStageEngine(t *testing.T, outcome hgithub.OwnReviewThreadsOutcome) (*stageAPI, *readyThreadsTransport, func(reason, head string, labels ...string), *stagelabels.Store) {
	t.Helper()
	return readyThreadsStageEngineQuiet(t, outcome, false)
}

// readyThreadsStageEngineQuiet is readyThreadsStageEngine with
// github.notifications.mode quiet when quiet is set.
func readyThreadsStageEngineQuiet(t *testing.T, outcome hgithub.OwnReviewThreadsOutcome, quiet bool) (*stageAPI, *readyThreadsTransport, func(reason, head string, labels ...string), *stagelabels.Store) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	t.Cleanup(closeAPI)
	clock := &contributorLaneClock{at: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, 0)
	if quiet {
		api.statefulComments = true
		e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, QuietNotifications: true} }
	}
	client, ok := e.transport.(*hgithub.Client)
	if !ok {
		t.Fatalf("engine transport is %T, want *hgithub.Client", e.transport)
	}
	tr := &readyThreadsTransport{Client: client, outcome: outcome}
	e.transport = tr
	sweep := func(reason, head string, labels ...string) {
		t.Helper()
		api.reset()
		pr := stagePR(9, labels...)
		pr.Head.SHA = gh.Ptr(head)
		pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("main")}
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return api, tr, sweep, store
}

// The stage pass asks for the hive's own threads to be resolved when it
// writes 090, once per head: the same head again asks nothing, a new head
// asks again. The 090 entry comment is posted as before.
func TestReadyForHumanMergeResolvesOwnThreadsOncePerHead(t *testing.T) {
	api, tr, sweep, store := readyThreadsStageEngine(t, hgithub.OwnReviewThreadsOutcome{Unresolved: 2, Resolvable: 2, Resolved: 2})
	sweep(contributorLaneReasonClaimTier, "sha", "role/contributor", "author/human", "stage/050-review-pending")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("stage = %v, want 090", got)
	}
	if got := tr.callList(); !slices.Equal(got, []string{"acme/widget#9@main"}) {
		t.Fatalf("calls = %v, want one for the head on main", got)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], stagelabels.ReadyCommentMarker) {
		t.Fatalf("comments = %q, want the 090 entry comment", api.comments)
	}
	sweep(contributorLaneReasonClaimTier, "sha", "role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if got := tr.callList(); len(got) != 1 {
		t.Fatalf("same head asked again: calls = %v", got)
	}
	sweep(contributorLaneReasonClaimTier, "sha2", "role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if got := tr.callList(); len(got) != 2 {
		t.Fatalf("new head: calls = %v, want a second", got)
	}
}

// An answer that read no thread (a paused repository, a rule that could not
// be read) or resolved nothing because every resolve call failed leaves the
// head to be asked again; any other answer is final for the head.
func TestReadyForHumanMergeOwnThreadsRetriedOnlyWhenNothingChanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome hgithub.OwnReviewThreadsOutcome
		err     error
		want    int
	}{
		{name: "rule unknown", outcome: hgithub.OwnReviewThreadsOutcome{Skipped: hgithub.OwnThreadsSkippedRuleUnknown}, want: 2},
		{name: "paused", outcome: hgithub.OwnReviewThreadsOutcome{Skipped: "paused"}, want: 2},
		{name: "rule not required", outcome: hgithub.OwnReviewThreadsOutcome{Skipped: hgithub.OwnThreadsSkippedRuleNotRequired}, want: 1},
		{name: "a thread needs a person", outcome: hgithub.OwnReviewThreadsOutcome{Unresolved: 2, Resolvable: 1, OwnBlocking: 1, Skipped: "not-every-thread-resolvable"}, want: 1},
		{name: "thread read failed", err: errors.New("graphql: boom"), want: 1},
		{name: "every resolve failed", outcome: hgithub.OwnReviewThreadsOutcome{Unresolved: 2, Resolvable: 2, Failed: 2}, want: 2},
		{name: "one of two resolves failed", outcome: hgithub.OwnReviewThreadsOutcome{Unresolved: 2, Resolvable: 2, Resolved: 1, Failed: 1}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, tr, sweep, _ := readyThreadsStageEngine(t, tc.outcome)
			tr.err = tc.err
			sweep(contributorLaneReasonClaimTier, "sha", "role/contributor", "author/human", "stage/050-review-pending")
			sweep(contributorLaneReasonClaimTier, "sha", "role/contributor", "author/human", "stage/090-ready-for-human-merge")
			if got := tr.callList(); len(got) != tc.want {
				t.Fatalf("calls = %v, want %d", got, tc.want)
			}
		})
	}
}

// A held 090 (a sentinel alert here) waits for the gate: nothing is asked
// on the held head, and the head is asked once the gate lifts.
func TestReadyForHumanMergeHeldHeadWaitsForTheGate(t *testing.T) {
	_, tr, sweep, store := readyThreadsStageEngine(t, hgithub.OwnReviewThreadsOutcome{})
	sweep(contributorLaneReasonClaimTier, "sha", "role/contributor", "author/human", "stage/050-review-pending")
	sweep(contributorLaneReasonSentinelBlocked, "sha2", "role/contributor", "author/human", "stage/090-ready-for-human-merge", "sentinel-alert")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("stage = %v, want the held head at 090", got)
	}
	if got := tr.callList(); len(got) != 1 {
		t.Fatalf("held head asked: calls = %v", got)
	}
	sweep(contributorLaneReasonClaimTier, "sha2", "role/contributor", "author/human", "stage/090-ready-for-human-merge")
	if got := tr.callList(); len(got) != 2 || got[1] != "acme/widget#9@main" {
		t.Fatalf("after the gate lifted: calls = %v, want the head asked", got)
	}
}

// Which routes and engines ask. A 090 for a human-merge path (an
// auto-merge-on repository) and for an auto-merge-off repository ask like a
// trust-blocked one; a held 090, an 085, an observe-only engine, a merged
// PR and a head with no base or SHA do not.
func TestResolveOwnThreadsAtReadyGuards(t *testing.T) {
	ready := stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge}
	for _, tc := range []struct {
		name        string
		edit        func(*stageObservation)
		route       stagelabels.Route
		observeOnly bool
		want        int
	}{
		{name: "trust-blocked 090", route: ready, want: 1},
		{name: "human-merge path 090", edit: func(o *stageObservation) { o.Reason = autoMergeReasonHumanMergePath },
			route: stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge, ManualMerge: true, Reasons: []string{"it changes a path a person merges"}}, want: 1},
		{name: "auto-merge off 090", edit: func(o *stageObservation) { o.Reason = observeOnlyReasonRepoAutoMergeDisabled }, route: ready, want: 1},
		{name: "held 090", route: stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge, Gates: []string{"a sentinel alert is open on this PR"}}},
		{name: "085", route: stagelabels.Route{Stage: stagelabels.NeedsDecision}},
		{name: "observe-only engine", route: ready, observeOnly: true},
		{name: "merged", edit: func(o *stageObservation) { o.Merged = true }, route: ready},
		{name: "no base", edit: func(o *stageObservation) { o.BaseRef = "" }, route: ready},
		{name: "no head", edit: func(o *stageObservation) { o.HeadSHA = "" }, route: ready},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:1")
			client.SetAppBotLogin(testHiveAppBotLogin)
			tr := &readyThreadsTransport{Client: client}
			c := New(tr, Options{})
			c.observeOnly = tc.observeOnly
			obs := stageObservation{Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 5, HeadSHA: "h1", BaseRef: "main", Reason: "untrusted-author-role"}
			if tc.edit != nil {
				tc.edit(&obs)
			}
			for i := 0; i < 2; i++ {
				c.resolveOwnThreadsAtReady(context.Background(), obs, tc.route)
			}
			if got := tr.callList(); len(got) != tc.want {
				t.Fatalf("calls = %v, want %d", got, tc.want)
			}
		})
	}
}

// In quiet notification mode a plain 090 emails nobody, so the threads are
// resolved without replies; otherwise each resolved thread gets one.
func TestReadyForHumanMergeQuietResolvesWithoutReplies(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		_, tr, sweep, store := readyThreadsStageEngineQuiet(t, hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, Resolved: 1}, quiet)
		sweep(contributorLaneReasonClaimTier, "sha", "role/contributor", "author/human", "stage/050-review-pending")
		if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
			t.Fatalf("quiet=%v: stage = %v, want 090", quiet, got)
		}
		if got := tr.replyList(); !slices.Equal(got, []bool{!quiet}) {
			t.Fatalf("quiet=%v: reply flags = %v, want [%v]", quiet, got, !quiet)
		}
	}
}

// The resolve and the replies are unsolicited writes: the write-back gate
// decides them under the 090 / 085 entry comment's kind, before any GitHub
// call. A repository whose mode refuses comment writes (labels-only,
// issues-only, off) gets nothing read or written, even on the hive's own
// PR; a person's PR needs what that kind needs (the opt-in label, or mode
// full). A refusal is final for the head.
func TestResolveOwnThreadsAtReadyWritebackGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   writeback.RepoMode
		author string
		labels []string
		want   int
	}{
		{name: "labels-only, the hive's own PR", mode: writeback.RepoModeLabelsOnly, author: testHiveAppBotLogin},
		{name: "issues-only, the hive's own PR", mode: writeback.RepoModeIssuesOnly, author: testHiveAppBotLogin},
		{name: "off, the hive's own PR", mode: writeback.RepoModeOff, author: testHiveAppBotLogin},
		{name: "labels-only, a person's opted-in PR", mode: writeback.RepoModeLabelsOnly, author: "alice", labels: []string{writeback.DefaultOptInLabel}},
		{name: "a person's PR, not opted in", author: "alice"},
		{name: "the hive's own PR", author: testHiveAppBotLogin, want: 1},
		{name: "a person's opted-in PR", author: "alice", labels: []string{writeback.DefaultOptInLabel}, want: 1},
		{name: "full, a person's PR", mode: writeback.RepoModeFull, author: "alice", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "http://127.0.0.1:1")
			client.SetAppBotLogin(testHiveAppBotLogin)
			cfg := writeback.Config{}
			if tc.mode != "" {
				cfg.PerRepo = map[string]writeback.RepoOverride{"acme/widget": {Mode: tc.mode}}
			}
			client.SetWritebackGate(writeback.New(cfg, writeback.Deps{LevelFor: func(string) int { return 6 }}, ""))
			tr := &readyThreadsTransport{Client: client}
			c := New(tr, Options{})
			obs := stageObservation{Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 5, HeadSHA: "h1", BaseRef: "main", Author: tc.author, Labels: tc.labels}
			for i := 0; i < 2; i++ {
				c.resolveOwnThreadsAtReady(context.Background(), obs, stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge})
			}
			if got := tr.callList(); len(got) != tc.want {
				t.Fatalf("calls = %v, want %d", got, tc.want)
			}
		})
	}
}

// readyThreadsGraphQL serves the review-thread query and the two thread
// mutations for acme/widget#7, whose threads are the hive's own.
type readyThreadsGraphQL struct {
	mu       sync.Mutex
	bodies   map[string]string // thread id -> first comment body
	order    []string
	resolved []string
	replies  map[string][]string
	ops      []string
	queries  int
}

func (g *readyThreadsGraphQL) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queries
}

func (g *readyThreadsGraphQL) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &req); err != nil {
		t.Errorf("bad graphql body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queries++
	w.Header().Set("Content-Type", "application/json")
	id, _ := req.Variables["id"].(string)
	var data any
	switch {
	case strings.Contains(req.Query, "resolveReviewThread("):
		g.resolved = append(g.resolved, id)
		g.ops = append(g.ops, "resolve:"+id)
		data = map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"id": id, "isResolved": true}}}
	case strings.Contains(req.Query, "addPullRequestReviewThreadReply("):
		body, _ := req.Variables["body"].(string)
		g.replies[id] = append(g.replies[id], body)
		g.ops = append(g.ops, "reply:"+id)
		data = map[string]any{"addPullRequestReviewThreadReply": map[string]any{"comment": map[string]any{"id": "PRRC_new"}}}
	case strings.Contains(req.Query, "reviewThreads("):
		nodes := []map[string]any{}
		for _, tid := range g.order {
			nodes = append(nodes, map[string]any{
				"id": tid, "isResolved": slices.Contains(g.resolved, tid), "isOutdated": false, "path": "a.go", "line": 1,
				"comments": map[string]any{"nodes": []map[string]any{{
					"author":     map[string]any{"__typename": "Bot", "login": strings.TrimSuffix(testHiveAppBotLogin, "[bot]")},
					"body":       g.bodies[tid],
					"databaseId": 1,
				}}},
			})
		}
		data = map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"headRefName":   "feature",
			"reviewThreads": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": nodes},
		}}}
	default:
		t.Errorf("unexpected graphql operation: %s", req.Query)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// projectbluefin/actions#779 end to end: a repository whose auto-merge is
// off, a ruleset whose pull_request rule requires thread resolution, and an
// approved, green PR whose only open threads are two of the hive's own low
// notes. The observe-only lanes write nothing; the stage pass that writes
// 090 resolves both threads and replies once in each, and a second pass on
// the same head writes nothing more. A medium (blocking) note beside them
// leaves every thread open.
func TestReadyForHumanMergeAutoMergeOffRepoResolvesOwnThreads(t *testing.T) {
	low := "**correctness (low):** rename this\n\n<!-- hive-finding-severity: low -->"
	for _, tc := range []struct {
		name     string
		threads  map[string]string
		resolved []string
	}{
		{name: "two own low notes", threads: map[string]string{"T-1": low, "T-2": low}, resolved: []string{"T-1", "T-2"}},
		{name: "plus an own blocking note", threads: map[string]string{"T-1": low, "T-2": low, "T-3": "**correctness (medium):** x may be nil\n\n<!-- hive-finding-severity: medium -->"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gql := &readyThreadsGraphQL{bodies: tc.threads, replies: map[string][]string{}}
			for id := range tc.threads {
				gql.order = append(gql.order, id)
			}
			slices.Sort(gql.order)
			lane := &observeOnlyLaneAPI{}
			api := &stageAPI{labels: map[string]string{}, extra: func(w http.ResponseWriter, r *http.Request) bool {
				switch {
				case r.URL.Path == "/graphql" || r.URL.Path == "/api/graphql":
					gql.serve(t, w, r)
					return true
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/rules/branches/main":
					_, _ = w.Write([]byte(`[{"type":"pull_request","parameters":{"required_approving_review_count":0,"required_review_thread_resolution":true}}]`))
					return true
				}
				return lane.serve(w, r)
			}}
			for _, d := range stagelabels.AllLabelDefs() {
				api.labels[d.Name] = d.Color
			}
			srv := newStageAPI(t, api)
			defer srv.Close()
			setStagePRTruth(7)

			clock := &contributorLaneClock{at: time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)}
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			store := stagelabels.NewStore(filepath.Join(t.TempDir(), "stage.json"), clock.now)
			e := New(client, Options{
				Now:                  clock.now,
				StageLabelPolicy:     func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
				StageTracker:         NewStageTracker(),
				StageStore:           store,
				CheckSummary:         greenChecks,
				BasePreparation:      NewBasePreparationStore(""),
				RepoAutoMergeEnabled: func(string) bool { return false },
				RepoAutoMergeSwitchedOff: func(repo string) bool {
					return strings.EqualFold(repo, "widget") || strings.EqualFold(repo, "acme/widget")
				},
				TrustedAuthorPolicy: func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true} },
				TrustedAuthorizer: func(login, requireRole string) TrustedAuthorDecision {
					return TrustedAuthorDecision{Allowed: login == "alice", Role: requireRole}
				},
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
					return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
				},
			})
			ctx := context.Background()
			pass := func() {
				t.Helper()
				before := gql.count()
				if _, err := e.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{}); err != nil {
					t.Fatalf("self-authored lane: %v", err)
				}
				if _, err := e.SweepTrustedAuthorAutoMerges(ctx, AutoMergeSweepOptions{SelfAuthoredLaneActive: true}); err != nil {
					t.Fatalf("trusted-author lane: %v", err)
				}
				if len(lane.writes) != 0 || gql.count() != before {
					t.Fatalf("observe-only lanes wrote: %v (graphql %d)", lane.writes, gql.count()-before)
				}
				if _, err := e.ApplyStageLabels(ctx); err != nil {
					t.Fatalf("ApplyStageLabels: %v", err)
				}
			}
			pass()
			if rec, ok := store.Get(stageKey("acme", "widget", 7)); !ok || rec.Stage != stagelabels.ReadyForHumanMerge {
				t.Fatalf("stage record = %+v (ok %v), want 090", rec, ok)
			}
			if lane.merges != 0 || lane.updates != 0 {
				t.Fatalf("merges=%d update-branch=%d, want none", lane.merges, lane.updates)
			}
			gql.mu.Lock()
			resolved := append([]string(nil), gql.resolved...)
			replies := len(gql.replies)
			gql.mu.Unlock()
			slices.Sort(resolved)
			if !slices.Equal(resolved, tc.resolved) {
				t.Fatalf("resolved %v, want %v", resolved, tc.resolved)
			}
			if replies != len(tc.resolved) {
				t.Fatalf("replies in %d threads, want %d: %v", replies, len(tc.resolved), gql.replies)
			}
			for _, id := range tc.resolved {
				if r := gql.replies[id]; len(r) != 1 || !strings.Contains(r[0], "is resolving its own non-blocking note") {
					t.Fatalf("replies in %s = %q, want one", id, r)
				}
			}
			gql.mu.Lock()
			ops := append([]string(nil), gql.ops...)
			gql.mu.Unlock()
			var want []string
			for _, id := range tc.resolved {
				want = append(want, "reply:"+id, "resolve:"+id)
			}
			if !slices.Equal(ops, want) {
				t.Fatalf("thread writes = %v, want each reply before its resolve %v", ops, want)
			}

			before := gql.count()
			pass()
			if after := gql.count(); after != before {
				t.Fatalf("second pass on the same head sent %d more graphql requests", after-before)
			}
		})
	}
}
