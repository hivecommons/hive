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
	"time"

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
		got := runEscalationSweep(context.Background(), cfg, client, actionable, nil, nil, logger)
		if len(got) != 0 {
			t.Fatalf("escalated = %v, want empty when escalation is disabled", got)
		}
	})

	t.Run("nil github client returns empty", func(t *testing.T) {
		got := runEscalationSweep(context.Background(), escalationTestConfig(), nil, actionable, nil, nil, logger)
		if len(got) != 0 {
			t.Fatalf("escalated = %v, want empty with nil client", got)
		}
	})

	t.Run("nil actionable returns empty", func(t *testing.T) {
		got := runEscalationSweep(context.Background(), escalationTestConfig(), client, nil, nil, nil, logger)
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
		got := runEscalationSweep(context.Background(), cfg, client, pr(sha), nil, nil, logger)
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
	got := runEscalationSweep(context.Background(), cfg, client, pr("sha-3"), notifier, nil, logger)
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
	got = runEscalationSweep(context.Background(), cfg, client, pr("sha-4"), nil, nil, logger)
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
			actionableWith(redPR("widgets", 9, "helper[bot]", sha)), nil, nil, logger)
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
			actionableWith(redPR("widgets", 4, "hive-agent", sha)), nil, nil, logger)
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

	// Once the forge accepts labels again the sweep retries the LABEL only —
	// never the comment — and stops once it is confirmed on the PR.
	fake.failLabels = false
	sweep("sha-3")
	if len(fake.labels) != 1 || fake.labels[0] != escalation.NeedsHumanLabel {
		t.Fatalf("labels = %v, want the label retried once the forge recovers", fake.labels)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want the label retry to post no comment", len(fake.comments))
	}
	labeled := redPR("widgets", 4, "hive-agent", "sha-3")
	labeled.Labels = []string{escalation.NeedsHumanLabel}
	runEscalationSweep(context.Background(), cfg, client, actionableWith(labeled), nil, nil, logger)
	if len(fake.labels) != 1 {
		t.Fatalf("labels = %v, want no further label calls once confirmed", fake.labels)
	}
}

// The forge label is the record of escalation: a PR that already wears
// needs-human is hands-off and NEVER gets a second comment, even when the
// ledger has no memory of it (a wiped or unwritable /data). This is the
// tuna-os incident: corral#268 collected fifteen identical escalation
// comments in 32 hours.
func TestRunEscalationSweepLabeledPRIsEscalatedWithoutSideEffects(t *testing.T) {
	newTestEscalationStore(t) // empty ledger
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	labeled := redPR("widgets", 5, "hive-agent", "sha-1")
	labeled.Labels = []string{"needs-human"}
	key := escalation.Key("acme/widgets", 5)
	for i := 0; i < 3; i++ {
		got := runEscalationSweep(context.Background(), cfg, client, actionableWith(labeled), nil, nil, logger)
		if !got[key] {
			t.Fatalf("pass %d: escalated = %v, want %q true from the label alone", i+1, got, key)
		}
	}
	if len(fake.paths) != 0 {
		t.Fatalf("a labeled PR must not trigger any API call, saw %v", fake.paths)
	}
}

// A pass that cannot conclude CI (checks re-running, or the check-run fetch
// failed — both surface as CIStatus "pending") must neither forget an
// escalation nor count as a fix attempt.
func TestRunEscalationSweepPendingPassKeepsEscalation(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	sweep := func(pr github.PullRequest) map[string]bool {
		return runEscalationSweep(context.Background(), cfg, client, actionableWith(pr), nil, nil, logger)
	}
	for _, sha := range []string{"a", "b", "c"} {
		sweep(redPR("widgets", 6, "hive-agent", sha))
	}
	key := escalation.Key("acme/widgets", 6)
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want the escalation comment", len(fake.comments))
	}

	pending := redPR("widgets", 6, "hive-agent", "c")
	pending.CIStatus = "pending"
	pending.FailingChecks = nil
	pending.Labels = []string{escalation.NeedsHumanLabel}
	if got := sweep(pending); !got[key] {
		t.Fatalf("escalated = %v, want %q to survive a pending pass", got, key)
	}
	// Red again on the same head, label still on: quiet.
	red := redPR("widgets", 6, "hive-agent", "c")
	red.Labels = []string{escalation.NeedsHumanLabel}
	if got := sweep(red); !got[key] {
		t.Fatalf("escalated = %v, want %q still true", got, key)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("comments = %d, want no second escalation comment", len(fake.comments))
	}
}

// Dependency bots carry the "[bot]" suffix but are not hive agents: their red
// PRs are not fix loops and must never be escalated.
func TestRunEscalationSweepIgnoresDependencyBots(t *testing.T) {
	newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	for _, author := range []string{"renovate[bot]", "dependabot[bot]", "mergeraptor[bot]"} {
		for _, sha := range []string{"1", "2", "3", "4"} {
			got := runEscalationSweep(context.Background(), cfg, client,
				actionableWith(redPR("widgets", 9, author, author+sha)), nil, nil, logger)
			if len(got) != 0 {
				t.Fatalf("%s: escalated = %v, want empty", author, got)
			}
		}
	}
	if len(fake.paths) != 0 {
		t.Fatalf("dependency-bot PRs must not touch the API, saw %v", fake.paths)
	}
}

// Budget exhaustion on a never-moving head words the comment for what it is.
func TestRunEscalationSweepExhaustedWording(t *testing.T) {
	store, clock := newTestEscalationStore(t)
	client, fake := newEscalationSweepClient(t)
	cfg := escalationTestConfig()
	logger := discardLogger()

	pr := redPR("widgets", 10, "hive-agent", "frozen")
	runEscalationSweep(context.Background(), cfg, client, actionableWith(pr), nil, nil, logger)
	*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
	for i := 0; i < escalation.MaxReEngagements; i++ {
		if !store.TryReEngage("acme/widgets", 10, "frozen") {
			t.Fatalf("re-engage %d should be allowed", i+1)
		}
	}
	got := runEscalationSweep(context.Background(), cfg, client, actionableWith(pr), nil, nil, logger)
	if !got[escalation.Key("acme/widgets", 10)] {
		t.Fatalf("escalated = %v, want exhausted PR escalated", got)
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], "no new commit pushed") {
		t.Fatalf("comments = %q, want exhausted wording", fake.comments)
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
		got := runEscalationSweep(context.Background(), cfg, client, actionableWith(human), nil, nil, logger)
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
	runEscalationSweep(context.Background(), cfg, client, actionableWith(withExcerpt), nil, nil, logger)
	withExcerpt.HeadSHA = "sha-2"
	runEscalationSweep(context.Background(), cfg, client, actionableWith(withExcerpt), nil, nil, logger)

	bare := redPR("widgets", 12, "hive-agent", "sha-3") // no excerpt this pass
	got := runEscalationSweep(context.Background(), cfg, client, actionableWith(bare), nil, nil, logger)
	if !got[escalation.Key("acme/widgets", 12)] {
		t.Fatalf("escalated = %v, want acme/widgets#12 true", got)
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], "panic: index out of range") {
		t.Fatalf("comment must carry the ledger's stored excerpt, got: %v", fake.comments)
	}
}
