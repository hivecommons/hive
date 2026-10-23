package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// hgithub.Client is the production queuer: approve-and-unhold must reach the
// same QueuePRAutoMerge path as the dashboard's queue button.
var _ autoMergeQueuer = (*hgithub.Client)(nil)

const (
	approveUnholdHead  = "head-current"
	approveUnholdStale = "head-stale"
	approveUnholdLabel = "lgtm"
)

// recordingQueuer is the real transport client with QueuePRAutoMerge
// replaced by a recorder, so the trigger is tested apart from the queue path
// (covered by TestQueuePRAutoMerge* in pkg/github).
type recordingQueuer struct {
	*hgithub.Client
	mu     sync.Mutex
	queued []string
	err    error
}

func (r *recordingQueuer) QueuePRAutoMerge(_ context.Context, repo string, number int, queuedBy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queued = append(r.queued, fmt.Sprintf("%s#%d by %s", repo, number, queuedBy))
	return r.err
}

func (r *recordingQueuer) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queued...)
}

// approveUnholdPR is the GitHub state of PR #7 for one scenario.
type approveUnholdPR struct {
	author   string
	draft    bool
	labels   []string
	reviews  []map[string]any
	events   []map[string]any
	comments []map[string]any
}

func approveUnholdReview(login, state, commit, at string) map[string]any {
	return map[string]any{"user": map[string]any{"login": login, "type": "User"}, "state": state, "commit_id": commit, "submitted_at": at}
}

func approveUnholdHoldEvent(kind, actor, actorType, at string) map[string]any {
	return map[string]any{"event": kind, "label": map[string]string{"name": "hold"}, "actor": map[string]any{"login": actor, "type": actorType}, "created_at": at}
}

func approveUnholdLevelNotice() map[string]any {
	return map[string]any{"body": `<!-- hive:level-hold {"agent":"scanner"} -->` + "\n> Held for human review", "user": map[string]any{"login": testHiveAppBotLogin, "type": "Bot"}}
}

// consentingPR is the one shape that counts as consent: alice, a trusted
// merger, approved the current head and removed the App's level hold.
func consentingPR() approveUnholdPR {
	return approveUnholdPR{
		author:  testHiveAppBotLogin,
		reviews: []map[string]any{approveUnholdReview("alice", "APPROVED", approveUnholdHead, "2026-10-22T10:05:00Z")},
		events: []map[string]any{
			approveUnholdHoldEvent("labeled", testHiveAppBotLogin, "Bot", "2026-10-22T09:00:00Z"),
			approveUnholdHoldEvent("unlabeled", "alice", "User", "2026-10-22T10:06:00Z"),
		},
		comments: []map[string]any{approveUnholdLevelNotice()},
	}
}

func approveUnholdServer(t *testing.T, pr approveUnholdPR, listedPulls *atomic.Bool) *httptest.Server {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write %s %s: the trigger itself must only read", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widget/pulls":
			if listedPulls != nil {
				listedPulls.Store(true)
			}
			labels := make([]map[string]string, 0, len(pr.labels))
			for _, l := range pr.labels {
				labels = append(labels, map[string]string{"name": l})
			}
			json.NewEncoder(w).Encode([]map[string]any{{
				"number": 7,
				"state":  "open",
				"draft":  pr.draft,
				"user":   map[string]any{"login": pr.author},
				"labels": labels,
				"head":   map[string]string{"sha": approveUnholdHead, "ref": "fix"},
			}})
		case "/repos/acme/widget/pulls/7/reviews":
			json.NewEncoder(w).Encode(orEmpty(pr.reviews))
		case "/repos/acme/widget/issues/7/events":
			json.NewEncoder(w).Encode(orEmpty(pr.events))
		case "/repos/acme/widget/issues/7/comments":
			json.NewEncoder(w).Encode(orEmpty(pr.comments))
		case "/repos/acme/widget/issues":
			json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func orEmpty(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

func approveUnholdEngine(t *testing.T, pr approveUnholdPR, optedIn bool, listedPulls *atomic.Bool) (*Engine, *recordingQueuer) {
	t.Helper()
	api := approveUnholdServer(t, pr, listedPulls)
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	q := &recordingQueuer{Client: client}
	e := New(q, Options{
		MergerAuthorizer:        func(login string) bool { return login == "alice" || login == "carol" },
		QueueOnApproveAndUnhold: func(repo string) bool { return optedIn && repo == "acme/widget" },
	})
	return e, q
}

func TestApproveAndUnholdQueuesWithTheApprovingMerger(t *testing.T) {
	e, q := approveUnholdEngine(t, consentingPR(), true, nil)
	if got := e.queueApprovedUnheldLevelHolds(context.Background(), "widget", "acme", "widget", approveUnholdLabel); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	if calls := q.calls(); len(calls) != 1 || calls[0] != "acme/widget#7 by alice" {
		t.Fatalf("QueuePRAutoMerge calls = %v, want one for acme/widget#7 by alice", calls)
	}
}

func TestApproveAndUnholdApprovalBeforeOrAfterUnholdBothCount(t *testing.T) {
	pr := consentingPR()
	pr.reviews = []map[string]any{approveUnholdReview("alice", "APPROVED", approveUnholdHead, "2026-10-22T11:00:00Z")}
	e, q := approveUnholdEngine(t, pr, true, nil)
	if got := e.queueApprovedUnheldLevelHolds(context.Background(), "widget", "acme", "widget", approveUnholdLabel); got != 1 || len(q.calls()) != 1 {
		t.Fatalf("queued = %d, calls = %v; approval after the unhold must also queue", got, q.calls())
	}
}

// TestApproveAndUnholdNothingElseCountsAsConsent pins the maintainer ruling
// on #13617: an APPROVED review on the current head plus removing the level
// hold is queued; nothing else counts as consent.
func TestApproveAndUnholdNothingElseCountsAsConsent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*approveUnholdPR)
		reason string
	}{
		{"comment review is not an approval", func(pr *approveUnholdPR) {
			pr.reviews = []map[string]any{approveUnholdReview("alice", "COMMENTED", approveUnholdHead, "2026-10-22T10:05:00Z")}
			pr.comments = append(pr.comments, map[string]any{"body": "/lgtm ship it", "user": map[string]any{"login": "alice", "type": "User"}})
		}, approveUnholdReasonNoApproval},
		{"approval on a stale head", func(pr *approveUnholdPR) {
			pr.reviews = []map[string]any{approveUnholdReview("alice", "APPROVED", approveUnholdStale, "2026-10-22T10:05:00Z")}
		}, approveUnholdReasonNoApproval},
		{"hold removal alone", func(pr *approveUnholdPR) {
			pr.reviews = nil
		}, approveUnholdReasonNoApproval},
		{"approval alone, hold still on", func(pr *approveUnholdPR) {
			pr.labels = []string{"hold"}
			pr.events = pr.events[:1]
		}, approveUnholdReasonHeld},
		{"approval alone, hold never removed by a person", func(pr *approveUnholdPR) {
			pr.events = pr.events[:1]
		}, approveUnholdReasonNoUnhold},
		{"untrusted actor approves and unholds", func(pr *approveUnholdPR) {
			pr.reviews = []map[string]any{approveUnholdReview("mallory", "APPROVED", approveUnholdHead, "2026-10-22T10:05:00Z")}
			pr.events[1] = approveUnholdHoldEvent("unlabeled", "mallory", "User", "2026-10-22T10:06:00Z")
		}, approveUnholdReasonUntrusted},
		{"approval and unhold by different mergers", func(pr *approveUnholdPR) {
			pr.events[1] = approveUnholdHoldEvent("unlabeled", "carol", "User", "2026-10-22T10:06:00Z")
		}, approveUnholdReasonNoApproval},
		{"latest review by the merger requests changes", func(pr *approveUnholdPR) {
			pr.reviews = append(pr.reviews, approveUnholdReview("alice", "CHANGES_REQUESTED", approveUnholdHead, "2026-10-22T10:07:00Z"))
		}, approveUnholdReasonNoApproval},
		{"hold removed by a bot", func(pr *approveUnholdPR) {
			pr.events[1] = approveUnholdHoldEvent("unlabeled", "some-bot[bot]", "Bot", "2026-10-22T10:06:00Z")
		}, approveUnholdReasonNoUnhold},
		{"hold re-applied after removal (push after review)", func(pr *approveUnholdPR) {
			pr.labels = []string{"hold"}
			pr.events = append(pr.events, approveUnholdHoldEvent("labeled", testHiveAppBotLogin, "Bot", "2026-10-22T10:30:00Z"))
		}, approveUnholdReasonHeld},
		{"hold was a person's, not the App's level hold", func(pr *approveUnholdPR) {
			pr.events[0] = approveUnholdHoldEvent("labeled", "carol", "User", "2026-10-22T09:00:00Z")
		}, approveUnholdReasonNoLevelHold},
		{"no level-hold notice", func(pr *approveUnholdPR) {
			pr.comments = nil
		}, approveUnholdReasonNoNotice},
		{"level-hold notice forged by a person", func(pr *approveUnholdPR) {
			pr.comments = []map[string]any{{"body": `<!-- hive:level-hold {"agent":"scanner"} -->`, "user": map[string]any{"login": "mallory", "type": "User"}}}
		}, approveUnholdReasonNoNotice},
		{"sentinel blocks", func(pr *approveUnholdPR) {
			pr.labels = []string{defaultSentinelLabel}
		}, approveUnholdReasonSentinel},
		{"already queued", func(pr *approveUnholdPR) {
			pr.labels = []string{approveUnholdLabel}
		}, approveUnholdReasonQueued},
		{"queue already approved on this head and since cancelled", func(pr *approveUnholdPR) {
			pr.reviews = append(pr.reviews, map[string]any{
				"user": map[string]any{"login": testHiveAppBotLogin, "type": "Bot"}, "state": "APPROVED", "commit_id": approveUnholdHead,
				"body": "Approved by @alice for Hive auto-merge on green CI.", "submitted_at": "2026-10-22T10:10:00Z",
			})
		}, approveUnholdReasonQueuedOnHead},
		{"draft", func(pr *approveUnholdPR) { pr.draft = true }, approveUnholdReasonDraft},
		{"not an agent PR", func(pr *approveUnholdPR) { pr.author = "dave" }, approveUnholdReasonNotAppAuthored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := consentingPR()
			tc.mutate(&pr)
			e, q := approveUnholdEngine(t, pr, true, nil)
			prs, err := e.listOpenPullRequests(context.Background(), "acme", "widget")
			if err != nil || len(prs) != 1 {
				t.Fatalf("listOpenPullRequests = %v, %v", prs, err)
			}
			by, reason, err := e.approveAndUnholdConsent(context.Background(), "acme", "widget", prs[0], approveUnholdLabel)
			if err != nil || by != "" || reason != tc.reason {
				t.Fatalf("approveAndUnholdConsent = (%q, %q, %v), want (\"\", %q, nil)", by, reason, err, tc.reason)
			}
			if got := e.queueApprovedUnheldLevelHolds(context.Background(), "widget", "acme", "widget", approveUnholdLabel); got != 0 || len(q.calls()) != 0 {
				t.Fatalf("queued = %d, calls = %v; want nothing queued", got, q.calls())
			}
		})
	}
}

func TestApproveAndUnholdFailsClosedWithoutMergerAuthorizer(t *testing.T) {
	e, q := approveUnholdEngine(t, consentingPR(), true, nil)
	e.SetMergerAuthorizer(nil)
	if got := e.queueApprovedUnheldLevelHolds(context.Background(), "widget", "acme", "widget", approveUnholdLabel); got != 0 || len(q.calls()) != 0 {
		t.Fatalf("queued = %d, calls = %v; no trusted-merger authorizer must queue nothing", got, q.calls())
	}
}

func TestApproveAndUnholdQueueErrorIsNotCounted(t *testing.T) {
	e, q := approveUnholdEngine(t, consentingPR(), true, nil)
	q.err = errors.New("sentinel-alert label blocks Hive approval and auto-merge")
	if got := e.queueApprovedUnheldLevelHolds(context.Background(), "widget", "acme", "widget", approveUnholdLabel); got != 0 {
		t.Fatalf("queued = %d, want 0 when QueuePRAutoMerge refuses", got)
	}
	if len(q.calls()) != 1 {
		t.Fatalf("calls = %v, want exactly one attempt", q.calls())
	}
}

// TestSweepQueuedAutoMergesApproveAndUnholdOptIn: off (the default) the sweep
// does not even list open PRs; on, it queues the consenting PR before it
// reads the queue.
func TestSweepQueuedAutoMergesApproveAndUnholdOptIn(t *testing.T) {
	for _, optedIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("opted_in=%v", optedIn), func(t *testing.T) {
			var listed atomic.Bool
			e, q := approveUnholdEngine(t, consentingPR(), optedIn, &listed)
			if _, err := e.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
				t.Fatalf("SweepQueuedAutoMerges: %v", err)
			}
			calls := q.calls()
			if !optedIn {
				if listed.Load() || len(calls) != 0 {
					t.Fatalf("flag off: listed open PRs=%v, calls=%v; want no behaviour change", listed.Load(), calls)
				}
				return
			}
			if len(calls) != 1 || !strings.HasSuffix(calls[0], "#7 by alice") {
				t.Fatalf("flag on: calls = %v, want #7 queued by alice", calls)
			}
		})
	}
}

func TestApproveAndUnholdNilOptionIsOff(t *testing.T) {
	if (&Engine{}).queueOnApproveAndUnholdActive("acme/widget") {
		t.Fatal("an engine without the option must leave approve-and-unhold off")
	}
	var nilEngine *Engine
	if nilEngine.queueOnApproveAndUnholdActive("acme/widget") {
		t.Fatal("nil engine must leave approve-and-unhold off")
	}
}
