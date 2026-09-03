package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
)

// escalationSweepServer is a fake GitHub API capturing the two escalation
// side effects runEscalationSweep performs: the evidence comment and the
// needs-human label. Per-path status overrides let a test fail one effect.
type escalationSweepServer struct {
	mu       sync.Mutex
	comments []string // comment bodies, in order
	labels   []string // labels added, flattened, in order
	paths    []string // request paths, in order

	failComments bool
	failLabels   bool
}

func (s *escalationSweepServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.paths = append(s.paths, r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if s.failComments {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("bad comment payload: %v", err)
			}
			s.comments = append(s.comments, payload.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if s.failLabels {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			var names []string
			if err := json.Unmarshal(body, &names); err != nil {
				// go-github may send {"labels": [...]} depending on version.
				var wrapped struct {
					Labels []string `json:"labels"`
				}
				if err2 := json.Unmarshal(body, &wrapped); err2 != nil {
					t.Errorf("bad labels payload %q: %v / %v", body, err, err2)
				}
				names = wrapped.Labels
			}
			s.labels = append(s.labels, names...)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func newEscalationSweepClient(t *testing.T) (*github.Client, *escalationSweepServer) {
	t.Helper()
	fake := &escalationSweepServer{}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return github.NewClientForTest(server.URL, "acme", []string{"widgets"}, logger), fake
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunEscalationSweepGates(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	logger := discardLogger()
	actionable := actionableWith(redPR("widgets", 7, "hive-agent", "sha-1"))

	t.Run("disabled config returns empty and calls nothing", func(t *testing.T) {
		cfg := escalationTestConfig()
		cfg.Escalation.Disabled = true
		got := runEscalationSweep(context.Background(), cfg, client, actionable, nil, logger)
		if len(got) != 0 {
			t.Fatalf("escalated = %v, want empty when escalation is disabled", got)
		}
	})

	t.Run("nil github client returns empty", func(t *testing.T) {
		got := runEscalationSweep(context.Background(), escalationTestConfig(), nil, actionable, nil, logger)
		if len(got) != 0 {
			t.Fatalf("escalated = %v, want empty with nil client", got)
		}
	})

	t.Run("nil actionable returns empty", func(t *testing.T) {
		got := runEscalationSweep(context.Background(), escalationTestConfig(), client, nil, nil, logger)
		if len(got) != 0 {
			t.Fatalf("escalated = %v, want empty with nil actionable set", got)
		}
	})

	if len(fake.paths) != 0 {
		t.Fatalf("gated sweeps must not touch the GitHub API, saw %v", fake.paths)
	}
}

// Three distinct red head SHAs cross the default threshold: the sweep must
// post the evidence comment, add the needs-human label, notify, and mark the
// PR escalated so the side effects never repeat.
func TestRunEscalationSweepEscalatesAtThresholdOnce(t *testing.T) {
	store, _ := newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	pr := func(sha string) *github.ActionableResult {
		p := redPR("widgets", 7, "hive-agent", sha)
		p.CIFailureExcerpt = "TestFoo: want 2, got 3"
		return actionableWith(p)
	}

	// Passes 1 and 2: below threshold, no side effects, nothing escalated.
	for i, sha := range []string{"sha-1", "sha-2"} {
		got := runEscalationSweep(context.Background(), cfg, client, pr(sha), nil, logger)
		if len(got) != 0 {
			t.Fatalf("pass %d: escalated = %v, want empty below threshold", i+1, got)
		}
	}
	if len(fake.paths) != 0 {
		t.Fatalf("below threshold the sweep must not call GitHub, saw %v", fake.paths)
	}

	// Pass 3: third distinct red SHA crosses DefaultThreshold. A notifier
	// with no channels configured exercises the notify branch as a no-op.
	notifier := notify.New(config.NotificationsConfig{}, logger)
	got := runEscalationSweep(context.Background(), cfg, client, pr("sha-3"), notifier, logger)
	key := escalation.Key("acme/widgets", 7)
	if !got[key] {
		t.Fatalf("escalated = %v, want %q true at threshold", got, key)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want exactly one escalation comment", len(fake.comments))
	}
	comment := fake.comments[0]
	for _, want := range []string{"3 distinct fix attempts", "test", "TestFoo: want 2, got 3"} {
		if !strings.Contains(comment, want) {
			t.Errorf("escalation comment missing %q:\n%s", want, comment)
		}
	}
	if len(fake.labels) != 1 || fake.labels[0] != escalation.NeedsHumanLabel {
		t.Fatalf("labels = %v, want exactly [%q]", fake.labels, escalation.NeedsHumanLabel)
	}
	for _, p := range fake.paths {
		if !strings.HasPrefix(p, "/repos/acme/widgets/issues/7/") {
			t.Errorf("side effect hit %q, want the org-qualified repo acme/widgets PR 7", p)
		}
	}
	if store.Attempts("acme/widgets", 7) != 3 {
		t.Errorf("store attempts = %d, want 3", store.Attempts("acme/widgets", 7))
	}

	// Pass 4: already escalated — still reported, but no repeat side effects.
	got = runEscalationSweep(context.Background(), cfg, client, pr("sha-4"), nil, logger)
	if !got[key] {
		t.Fatalf("escalated = %v, want %q to stay true after escalation", got, key)
	}
	if len(fake.comments) != 1 || len(fake.labels) != 1 {
		t.Fatalf("side effects repeated: %d comments, %d labels, want 1 and 1",
			len(fake.comments), len(fake.labels))
	}
}

// A failed comment must NOT mark the PR escalated: the whole point is that
// the evidence reaches a human, so the sweep retries on the next pass.
func TestRunEscalationSweepRetriesCommentNextPass(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	sweep := func(sha string) map[string]bool {
		return runEscalationSweep(context.Background(), cfg, client,
			actionableWith(redPR("widgets", 9, "helper[bot]", sha)), nil, logger)
	}
	sweep("sha-1")
	sweep("sha-2")

	fake.failComments = true
	got := sweep("sha-3")
	key := escalation.Key("acme/widgets", 9)
	if !got[key] {
		t.Fatalf("escalated = %v, want %q true even when the comment fails", got, key)
	}
	if len(fake.labels) != 0 {
		t.Fatalf("labels = %v, want none until the comment lands", fake.labels)
	}

	// Next pass: comment succeeds, label lands, PR is finally marked.
	fake.failComments = false
	got = sweep("sha-3")
	if !got[key] {
		t.Fatalf("escalated = %v, want %q true on the retry pass", got, key)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want the retried comment to land exactly once", len(fake.comments))
	}
	if len(fake.labels) != 1 || fake.labels[0] != escalation.NeedsHumanLabel {
		t.Fatalf("labels = %v, want [%q] after the retry", fake.labels, escalation.NeedsHumanLabel)
	}

	// And once marked, a further pass repeats nothing.
	sweep("sha-3")
	if len(fake.comments) != 1 || len(fake.labels) != 1 {
		t.Fatalf("side effects repeated after MarkEscalated: %d comments, %d labels",
			len(fake.comments), len(fake.labels))
	}
}

// A label failure is logged but non-fatal: the comment carried the evidence,
// so the PR is still marked escalated and never re-commented.
func TestRunEscalationSweepLabelFailureIsNonFatal(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	sweep := func(sha string) map[string]bool {
		return runEscalationSweep(context.Background(), cfg, client,
			actionableWith(redPR("widgets", 4, "hive-agent", sha)), nil, logger)
	}
	sweep("sha-1")
	sweep("sha-2")

	fake.failLabels = true
	got := sweep("sha-3")
	key := escalation.Key("acme/widgets", 4)
	if !got[key] {
		t.Fatalf("escalated = %v, want %q true despite the label failure", got, key)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want the evidence comment to have landed", len(fake.comments))
	}

	sweep("sha-3")
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want no repeat after a label-only failure", len(fake.comments))
	}
}

// Human-authored PRs are never escalation candidates, and a stored excerpt
// backfills a crossing pass that observed none.
func TestRunEscalationSweepAuthorGateAndExcerptFallback(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	// Human-authored red PR: ignored entirely, forever.
	human := redPR("widgets", 11, "jane-dev", "sha-h1")
	for _, sha := range []string{"sha-h1", "sha-h2", "sha-h3", "sha-h4"} {
		human.HeadSHA = sha
		got := runEscalationSweep(context.Background(), cfg, client, actionableWith(human), nil, logger)
		if len(got) != 0 {
			t.Fatalf("escalated = %v, want empty for a human-authored PR", got)
		}
	}
	if len(fake.paths) != 0 {
		t.Fatalf("human PRs must not trigger API calls, saw %v", fake.paths)
	}

	// Agent PR carries an excerpt on early passes but not on the crossing
	// pass: the comment must fall back to the excerpt stored in the ledger.
	withExcerpt := redPR("widgets", 12, "hive-agent", "sha-1")
	withExcerpt.CIFailureExcerpt = "panic: index out of range"
	runEscalationSweep(context.Background(), cfg, client, actionableWith(withExcerpt), nil, logger)
	withExcerpt.HeadSHA = "sha-2"
	runEscalationSweep(context.Background(), cfg, client, actionableWith(withExcerpt), nil, logger)

	bare := redPR("widgets", 12, "hive-agent", "sha-3") // no excerpt this pass
	got := runEscalationSweep(context.Background(), cfg, client, actionableWith(bare), nil, logger)
	if !got[escalation.Key("acme/widgets", 12)] {
		t.Fatalf("escalated = %v, want acme/widgets#12 true", got)
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], "panic: index out of range") {
		t.Fatalf("comment must carry the ledger's stored excerpt, got: %v", fake.comments)
	}
}

// #5617 item 3: a PR that re-escalates AFTER a reviewer-lane pass must reach
// its human with the structured hand-off note — what the reviewer left on the
// branch, and that no second automated pass is coming — rather than the
// generic first-escalation body. One reviewer pass per PR is the whole ladder,
// so this comment is the last thing the machinery will ever say about the PR.
func TestRunEscalationSweepHandsOffAfterReviewerPass(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	red := func(sha string, labels ...string) *github.ActionableResult {
		p := redPR("widgets", 7, "hive-agent", sha)
		p.CIFailureExcerpt = "TestFoo: want 2, got 3"
		p.Labels = labels
		return actionableWith(p)
	}

	// First escalation: three distinct red SHAs cross the threshold.
	for _, sha := range []string{"sha-1", "sha-2", "sha-3"} {
		runEscalationSweep(context.Background(), cfg, client, red(sha), nil, logger)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want the first escalation comment", len(fake.comments))
	}
	if strings.Contains(fake.comments[0], "A reviewer already adjudicated") {
		t.Errorf("a first escalation must not claim a prior reviewer pass:\n%s", fake.comments[0])
	}

	// The reviewer repairs, drops needs-human and adds reviewer-passed; its
	// pushed fix is red. Sweep reconciles the verdict and restarts the ledger.
	runEscalationSweep(context.Background(), cfg, client,
		red("reviewer-fix", escalation.ReviewerPassedLabel), nil, logger)
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want no comment for the reviewer-verdict reconciliation", len(fake.comments))
	}

	// Two more distinct red SHAs re-cross the threshold on the fresh ledger.
	for _, sha := range []string{"sha-4", "sha-5"} {
		runEscalationSweep(context.Background(), cfg, client,
			red(sha, escalation.ReviewerPassedLabel, escalation.NeedsHumanLabel), nil, logger)
	}
	if len(fake.comments) != 2 {
		t.Fatalf("comments = %d, want a second escalation comment after the reviewer pass", len(fake.comments))
	}
	handoff := fake.comments[1]
	for _, want := range []string{
		"A reviewer already adjudicated this PR",
		"`reviewer-fix`",
		"No further automated pass is coming",
		"TestFoo: want 2, got 3",
	} {
		if !strings.Contains(handoff, want) {
			t.Errorf("re-escalation comment missing %q:\n%s", want, handoff)
		}
	}
}
