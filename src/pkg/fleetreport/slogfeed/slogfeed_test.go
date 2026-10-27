package slogfeed

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

type observed struct {
	class, key, message, diagnostics string
}

func newFeed(t *testing.T) (*slog.Logger, *[]observed, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	var seen []observed
	h := NewWithObserver(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}), func(class, key, message, diagnostics string) {
		seen = append(seen, observed{class, key, message, diagnostics})
	})
	return slog.New(h), &seen, &out
}

func TestHandlerForwardsAndObservesWarnPlus(t *testing.T) {
	logger, seen, out := newFeed(t)
	logger.Info("issue-request watcher started", "dir", "/var/run/x")
	logger.Warn("relay response write failed, will retry with backoff", "repo", "owner/repo", "error", "boom 42")
	logger.Error("kick failed: tmux send-keys exit 1")
	if n := strings.Count(out.String(), "\n"); n != 3 {
		t.Fatalf("wrapped handler saw %d records, want 3: %s", n, out.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("observed = %+v, want exactly the WARN (the ERROR matches kick_failed_repeatedly keywords)", *seen)
	}
	got := (*seen)[0]
	if got.class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("class = %q", got.class)
	}
	if got.key != fleetreport.NormalizeMessage("relay response write failed, will retry with backoff") {
		t.Fatalf("key = %q", got.key)
	}
	if !strings.Contains(got.diagnostics, "WARN") || !strings.Contains(got.diagnostics, "repo=owner/repo") || !strings.Contains(got.diagnostics, "error=boom 42") {
		t.Fatalf("diagnostics = %q", got.diagnostics)
	}
}

func TestHandlerSkipsBenignAndExplicitClasses(t *testing.T) {
	logger, seen, _ := newFeed(t)
	logger.Warn("read tcp 10.0.0.1:443: i/o timeout")
	logger.Warn("relay fetch aborted: context canceled")
	logger.Warn("restart storm detected for agent quality")
	logger.Error("advisory post failed: 502")
	// The relay watcher's own retry line is covered by issue_request_stuck.
	logger.Warn("issue-request watcher: request failed, will retry with backoff")
	logger.Warn("")
	if len(*seen) != 0 {
		t.Fatalf("observed = %+v, want none", *seen)
	}
}

func TestHandlerWithAttrsCarriesBoundAttrsIntoDiagnostics(t *testing.T) {
	logger, seen, _ := newFeed(t)
	logger.With("agent", "quality").WithGroup("req").Warn("ledger write failed", "path", "/data/x.json")
	if len(*seen) != 1 {
		t.Fatalf("observed = %+v", *seen)
	}
	d := (*seen)[0].diagnostics
	if !strings.Contains(d, "agent=quality") || !strings.Contains(d, "path=/data/x.json") {
		t.Fatalf("diagnostics = %q", d)
	}
}

func TestKeyBoundsAndNormalises(t *testing.T) {
	a := Key("request failed for owner/repo-1 after 3 attempts (id 0123456789abcdef)")
	b := Key("request failed for owner/other-9 after 17 attempts (id fedcba9876543210)")
	if a == "" || a != b {
		t.Fatalf("keys differ: %q vs %q", a, b)
	}
	long := Key(strings.Repeat("x", 10*maxKeyBytes))
	if len(long) != maxKeyBytes {
		t.Fatalf("key not bounded: %d", len(long))
	}
	if Key("   ") != "" {
		t.Fatal("blank message produced a key")
	}
}

func TestFeedIntoTrackerQualifiesAtThresholdAndIdleClears(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	tr := fleetreport.NewRuntimeTracker(clock)
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	for i := 0; i < 4; i++ {
		logger.Warn("relay response write failed, will retry with backoff", "attempt", i)
		now = now.Add(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("4 repeats produced evidence: %+v", ev)
	}
	logger.Warn("relay response write failed, will retry with backoff", "attempt", 99)
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassUnclassifiedRepeated || ev[0].Count != 5 {
		t.Fatalf("evidence = %+v", ev)
	}
	// Silence for 2× the window clears the condition and records it so the
	// publisher posts exactly one "cleared" comment.
	now = now.Add(21 * time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("idle condition still reported: %+v", ev)
	}
	cleared := tr.TakeCleared()
	if len(cleared) != 1 || cleared[0].Class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("TakeCleared = %+v", cleared)
	}
}

func TestHandlerEnabledDefersToWrapped(t *testing.T) {
	var out bytes.Buffer
	h := New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("INFO enabled on a WARN-level wrapped handler")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("ERROR not enabled")
	}
}

func TestReviewPendingExpiryLogsWithoutFleetReport(t *testing.T) {
	now := time.Date(2026, 10, 23, 17, 39, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	// Five unrelated first expiries in one cycle meet the generic feed's
	// threshold, even when the governor warns only once per PR (#13850).
	for i := 0; i < 5; i++ {
		logger.Warn("review pending expired", "repo", "owner/repo", "pr", 90+i,
			"perspective", "docs-currency", "agent", "reviewer", "age", "49m30s", "expiries", 1)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("bounded review retries produced fleet evidence: %+v", ev)
	}
	if n := strings.Count(out.String(), "\n"); n != 5 {
		t.Fatalf("wrapped handler saw %d expiry records, want 5: %s", n, out.String())
	}
	if !strings.Contains(out.String(), `"msg":"review pending expired"`) || !strings.Contains(out.String(), `"expiries":1`) {
		t.Fatalf("expiry log lost its message or diagnostics: %s", out.String())
	}
	// Exhausting the retry budget is still actionable, and must still cross
	// the report threshold instead of being swallowed by the retry filter.
	const exhausted = "review dispatch exhausted; holding PR for a human"
	for i := 0; i < 5; i++ {
		logger.Warn(exhausted, "repo", "owner/repo", "pr", 90+i)
	}
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassUnclassifiedRepeated || ev[0].Message != exhausted || ev[0].Count != 5 {
		t.Fatalf("exhausted reviews should still produce fleet evidence: %+v", ev)
	}
}

func TestPolicySeedDivergenceLogsWithoutFleetReport(t *testing.T) {
	now := time.Date(2026, 10, 24, 17, 9, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	const msg = "policies: seeded policy differs from the embedded default with no seed record; leaving it in place — resave it from the dashboard to take a new edit, or delete it to pick up the embedded default"
	// A hive whose /data/policies predates the seed-manifest tracking
	// (#9428) has every embedded template diverge on its first reconcile:
	// up to one WARN per template within a single boot, all carrying this
	// same constant message text (#14005).
	names := []string{"supervisor-nogithub.md", "scanner.md", "guide.md", "quality.md", "reviewer-queue.md"}
	for _, name := range names {
		logger.Warn(msg, "name", name, "path", "/data/policies/"+name)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("burst across unrelated policy templates produced fleet evidence: %+v", ev)
	}
	if n := strings.Count(out.String(), "\n"); n != len(names) {
		t.Fatalf("wrapped handler saw %d records, want %d: %s", n, len(names), out.String())
	}
	if !strings.Contains(out.String(), `"name":"supervisor-nogithub.md"`) || !strings.Contains(out.String(), `"path":"/data/policies/quality.md"`) {
		t.Fatalf("seed-divergence log lost its diagnostics: %s", out.String())
	}
}

func TestCopilotModelFallbackLogsWithoutFleetReport(t *testing.T) {
	now := time.Date(2026, 10, 24, 17, 37, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	const msg = "copilot CLI substituted the configured model"
	// Each copilot agent warns once per substitution tuple (#13659), so a
	// hive whose agents all share a model the account cannot use emits one
	// WARN per agent in the same boot, all with this constant text (#14020).
	agents := []string{"guide", "scanner", "quality", "reviewer", "supervisor", "outreach"}
	for _, name := range agents {
		logger.Warn(msg, "agent", name, "requested_model", "claude-sonnet-4.6", "running_model", "auto")
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("burst across copilot agents produced fleet evidence: %+v", ev)
	}
	if n := strings.Count(out.String(), "\n"); n != len(agents) {
		t.Fatalf("wrapped handler saw %d records, want %d: %s", n, len(agents), out.String())
	}
	if !strings.Contains(out.String(), `"agent":"guide"`) || !strings.Contains(out.String(), `"running_model":"auto"`) {
		t.Fatalf("model-substitution log lost its diagnostics: %s", out.String())
	}
}

func TestRequiredChecksOverrideMismatchLogsWithoutFleetReport(t *testing.T) {
	now := time.Date(2026, 10, 26, 6, 22, 45, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	const msg = "automerge required_checks override does not match repo protection"
	// Reproduce the reported five warnings in ten minutes. The notice must
	// stay visible to operators without qualifying as a runtime defect.
	for i := 0; i < 5; i++ {
		logger.Warn(msg, "repo", "kubestellar/console-kb", "declared", "[build-gate]",
			"actual", "[]", "using", "actual", "mismatch", "[build-gate]")
		now = now.Add(time.Minute)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("configuration drift produced fleet evidence: %+v", ev)
	}
	if n := strings.Count(out.String(), "\n"); n != 5 {
		t.Fatalf("wrapped handler saw %d records, want 5: %s", n, out.String())
	}
	if !strings.Contains(out.String(), `"msg":"`+msg+`"`) || !strings.Contains(out.String(), `"using":"actual"`) || !strings.Contains(out.String(), `"mismatch":"[build-gate]"`) {
		t.Fatalf("configuration warning lost its diagnostics: %s", out.String())
	}
	const failure = "automerge failed to read repo protection"
	for i := 0; i < 5; i++ {
		logger.Warn(failure)
	}
	if ev := tr.Evidence(); len(ev) != 1 || ev[0].Class != fleetreport.ClassUnclassifiedRepeated || ev[0].Message != failure || ev[0].Count != 5 {
		t.Fatalf("protection lookup failure should still produce fleet evidence: %+v", ev)
	}
}

func TestFeedLogFedPath(t *testing.T) {
	tests := []struct {
		name      string
		level     slog.Level
		message   string
		wantClass string
		wantKey   string
	}{
		{
			name:    "benign message is dropped",
			level:   slog.LevelWarn,
			message: "github REST 404 cached; suppressing repeated fetches",
		},
		{
			name:    "message matching no class is dropped by the log-fed path",
			level:   slog.LevelWarn,
			message: "advisory post failed: 502",
		},
		{
			name:      "log-fed keyword observes under its class",
			level:     slog.LevelWarn,
			message:   "tmux has-session failed for session 42",
			wantClass: fleetreport.ClassTmuxSessionCheckFailed,
			wantKey:   fleetreport.NormalizeMessage("tmux has-session failed for session 42"),
		},
		{
			name:      "feedback issue refresh failure observes under its class",
			level:     slog.LevelWarn,
			message:   "feedback: issue refresh failed",
			wantClass: fleetreport.ClassFeedbackIssueRefreshFailed,
			wantKey:   fleetreport.NormalizeMessage("feedback: issue refresh failed"),
		},
		{
			name:      "surrounding whitespace is trimmed before matching",
			level:     slog.LevelError,
			message:   "  tmux has-session failed  ",
			wantClass: fleetreport.ClassTmuxSessionCheckFailed,
			wantKey:   fleetreport.NormalizeMessage("tmux has-session failed"),
		},
		{
			name:      "very long log-fed message key is truncated",
			level:     slog.LevelWarn,
			message:   "tmux has-session failed " + strings.Repeat("because the socket is gone ", 10*maxKeyBytes),
			wantClass: fleetreport.ClassTmuxSessionCheckFailed,
			wantKey:   fleetreport.NormalizeMessage("tmux has-session failed " + strings.Repeat("because the socket is gone ", 10*maxKeyBytes))[:maxKeyBytes],
		},
		{
			name:      "unmatched message still observes as unclassified",
			level:     slog.LevelWarn,
			message:   "relay response write failed, will retry with backoff",
			wantClass: fleetreport.ClassUnclassifiedRepeated,
			wantKey:   fleetreport.NormalizeMessage("relay response write failed, will retry with backoff"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, seen, _ := newFeed(t)
			logger.Log(context.Background(), tc.level, tc.message)
			if tc.wantClass == "" {
				if len(*seen) != 0 {
					t.Fatalf("observed = %+v, want none", *seen)
				}
				return
			}
			if len(*seen) != 1 {
				t.Fatalf("observed = %+v, want exactly one", *seen)
			}
			got := (*seen)[0]
			if got.class != tc.wantClass {
				t.Fatalf("class = %q, want %q", got.class, tc.wantClass)
			}
			if got.key != tc.wantKey {
				t.Fatalf("key = %q, want %q", got.key, tc.wantKey)
			}
			if len(got.key) > maxKeyBytes {
				t.Fatalf("key not bounded: %d", len(got.key))
			}
		})
	}
}

func TestStartupKickInterruptedFeedThresholdAndClear(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	const msg = "startup kick dropped: agent restarted or stopped while waiting"
	// One normal stop must not report a fault; repeated interruptions should
	// carry their own attribution rather than a timeout or restart storm.
	for i := 0; i < 4; i++ {
		logger.Warn(msg, "name", "scanner", "trigger", "startup")
		now = now.Add(time.Minute)
		if ev := tr.Evidence(); len(ev) != 0 {
			t.Fatalf("below threshold produced evidence: %+v", ev)
		}
	}
	logger.Warn(msg, "name", "scanner", "trigger", "startup")
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassStartupKickInterrupted || ev[0].Count != 5 || ev[0].Severity != "low" {
		t.Fatalf("repeated interruptions = %+v", ev)
	}
	if !strings.Contains(ev[0].Diagnostics, "scanner") || !strings.Contains(ev[0].Diagnostics, "startup") {
		t.Fatalf("interruption diagnostics = %q", ev[0].Diagnostics)
	}
	now = now.Add(21 * time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("idle interruptions still reported: %+v", ev)
	}
	cleared := tr.TakeCleared()
	if len(cleared) != 1 || cleared[0].Class != fleetreport.ClassStartupKickInterrupted {
		t.Fatalf("cleared interruptions = %+v", cleared)
	}
}

func TestAgentGitHubBudgetLogsWithoutFleetReport(t *testing.T) {
	logger, seen, out := newFeed(t)
	for _, msg := range []string{
		"agent GitHub API hourly cap reached",
		"agent GitHub API hourly budget above soft threshold",
	} {
		for i := 0; i < 8; i++ {
			logger.Warn(msg, "agent", "reviewer", "count", 300, "cap", 300, "retry_after", 83, "reason", "cap")
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("expected quota backpressure produced observations: %+v", *seen)
	}
	if n := strings.Count(out.String(), "\n"); n != 16 {
		t.Fatalf("wrapped handler saw %d budget records, want 16: %s", n, out.String())
	}
	if !strings.Contains(out.String(), `"retry_after":83`) || !strings.Contains(out.String(), `"reason":"cap"`) {
		t.Fatalf("budget log lost its diagnostics: %s", out.String())
	}
	// Suppress only the known enforcement notifications, not budget failures.
	logger.Error("agent GitHub API hourly budget accounting failed")
	if len(*seen) != 1 || (*seen)[0].class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("budget accounting failure must remain reportable: %+v", *seen)
	}
}

func TestPostRestartKickHoldLogsWithoutFleetReport(t *testing.T) {
	logger, seen, out := newFeed(t)
	for i := 0; i < 8; i++ {
		logger.Warn("failed to send kick", "agent", "reviewer-2", "error", "kick held after restart: agent reviewer-2 was restarted while producing a turn; kicks are held for 40s more (#7363)")
	}
	if len(*seen) != 0 {
		t.Fatalf("expected post-restart kick hold produced observations: %+v", *seen)
	}
	if n := strings.Count(out.String(), "\n"); n != 8 {
		t.Fatalf("wrapped handler saw %d hold records, want 8", n)
	}
	// Other kick send failures must remain reportable.
	logger.Warn("failed to send kick", "agent", "reviewer-2", "error", "tmux send-keys exit 1")
	if len(*seen) != 1 || (*seen)[0].class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("other kick failures must remain reportable: %+v", *seen)
	}
}

func TestMergeCIRefusalLogsWithoutFleetReport(t *testing.T) {
	now := time.Date(2026, 10, 26, 14, 30, 19, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	const msg = "merge-request watcher: REFUSED to merge, CI not confirmed"
	const reason = "ci gate: required status check has not succeeded (required-check-pending:build-gate)"
	// Reproduce the issue's twelve refusals in ten minutes without changing
	// the gate decision or losing its operator-facing diagnostics.
	for i := 0; i < 12; i++ {
		logger.Warn(msg, "repo", "kubestellar/console", "number", 24617,
			"agent", "scanner", "verdict", "red", "reason", reason)
		now = now.Add(30 * time.Second)
	}
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("CI safety refusals produced fleet evidence: %+v", ev)
	}
	if n := strings.Count(out.String(), "\n"); n != 12 {
		t.Fatalf("wrapped handler saw %d records, want 12: %s", n, out.String())
	}
	if !strings.Contains(out.String(), `"msg":"`+msg+`"`) ||
		!strings.Contains(out.String(), `"verdict":"red"`) ||
		!strings.Contains(out.String(), `"reason":"`+reason+`"`) {
		t.Fatalf("CI refusal warning lost its diagnostics: %s", out.String())
	}
	const failure = "merge-request watcher: failed to read request"
	for i := 0; i < 5; i++ {
		logger.Warn(failure)
	}
	if ev := tr.Evidence(); len(ev) != 1 || ev[0].Class != fleetreport.ClassUnclassifiedRepeated || ev[0].Message != failure || ev[0].Count != 5 {
		t.Fatalf("watcher failure should still produce fleet evidence: %+v", ev)
	}
}
