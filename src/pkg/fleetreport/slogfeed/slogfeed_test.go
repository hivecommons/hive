package slogfeed

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
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

// TestDiagnosticsKeepsBothPaneTimeoutExcerpts regression-tests
// hivecommons/hive#14586 and #15311: the "pane content at timeout" WARN
// (src/crux/agent/manager.go) logs head_500 before tail_500, each up to 500
// runes. At the old 512-byte cap, head_500 alone (worse with multi-byte
// box-drawing characters) filled the budget and diagnostics() dropped
// tail_500 — the pane's current state and the only field useful for
// diagnosing a stuck CLI — before it was ever written.
func TestDiagnosticsKeepsBothPaneTimeoutExcerpts(t *testing.T) {
	logger, seen, _ := newFeed(t)
	head := strings.Repeat("❯", 500) // worst case: 500 runes, 3 bytes each
	tail := strings.Repeat("z", 480) + "TAIL-MARKER-PRESENT"
	logger.Warn("pane content at timeout",
		"agent", "quality",
		"len", 28517,
		"has_goose_ready", false,
		"has_enter", false,
		"has_arrow", true,
		"has_question_form", false,
		"has_bob_placeholder", false,
		"has_codex_ready", false,
		"head_500", head,
		"tail_500", tail,
	)
	if len(*seen) != 1 {
		t.Fatalf("observed = %+v, want exactly one", *seen)
	}
	got := (*seen)[0]
	if got.class != fleetreport.ClassAgentPaneTimeout {
		t.Fatalf("class = %q, want %q", got.class, fleetreport.ClassAgentPaneTimeout)
	}
	d := got.diagnostics
	if !strings.Contains(d, "has_codex_ready=false") {
		t.Fatalf("diagnostics dropped a flag before head_500: %q", d)
	}
	if !strings.Contains(d, "head_500=") {
		t.Fatalf("diagnostics missing head_500 entirely: %q", d)
	}
	if !strings.Contains(d, "TAIL-MARKER-PRESENT") {
		t.Fatalf("diagnostics dropped tail_500 (the pane's current state): %q", d)
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

func TestHandlerSkipsContributorTierLimits(t *testing.T) {
	for _, msg := range []string{
		"[contribute-ws] refusing task: concurrency limit reached",
		"[contribute-ws] refusing task: hourly rate limit reached",
		"[contribute-ws] refusing task: daily rate limit reached",
		"  [CONTRIBUTE-WS] REFUSING TASK: HOURLY RATE LIMIT REACHED  ",
	} {
		t.Run(msg, func(t *testing.T) {
			logger, seen, out := newFeed(t)
			for i := 0; i < 5; i++ {
				logger.Warn(msg, "tier", "newcomer", "assigned_last_hour", 3, "max_per_hour", 3)
			}
			if len(*seen) != 0 {
				t.Fatalf("policy refusals observed as runtime errors: %+v", *seen)
			}
			if n := strings.Count(out.String(), "\n"); n != 5 {
				t.Fatalf("wrapped handler saw %d records, want 5", n)
			}
		})
	}
	for _, msg := range []string{
		"[contribute-ws] refusing task: lease could not be persisted — try again",
		"[contribute-ws] hourly rate limit store write failed",
		"[contribute-ws] refusing task: hourly rate limit reached: enforcement failed",
	} {
		logger, seen, _ := newFeed(t)
		logger.Warn(msg)
		if len(*seen) != 1 || (*seen)[0].class != fleetreport.ClassUnclassifiedRepeated {
			t.Errorf("failure %q should remain reportable: %+v", msg, *seen)
		}
	}
}

func TestClassifierErrorsStayInLocalLogs(t *testing.T) {
	const host = "private-inference.internal"
	err := &url.Error{Op: "Post", URL: "https://" + host + "/v1/chat/completions", Err: errors.New("dial tcp: lookup " + host + ": no such host")}
	for _, msg := range []string{
		"ioscan classifier failed closed; text withheld",
		"ioscan classifier failed open; text passes on unclassified",
		"ioscan fail-closed blocked kick",
	} {
		for _, key := range []string{"error", "classifier_error"} {
			for _, placement := range []string{"record", "bound", "group"} {
				t.Run(msg+"/"+key+"/"+placement, func(t *testing.T) {
					now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
					tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
					var out bytes.Buffer
					logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
					attr := slog.Any(key, err)
					if key == "classifier_error" {
						attr = slog.String(key, err.Error())
					}
					args := []any{"agent", "reviewer", "fix", "check ioscan.classifier.model"}
					switch placement {
					case "record":
						args = append(args, attr)
					case "bound":
						logger = logger.With(attr)
					case "group":
						logger = logger.With(slog.Group("classifier", slog.Group("transport", attr)))
					}
					logger.Warn(msg, args...)
					if !strings.Contains(out.String(), host) || !strings.Contains(out.String(), "no such host") {
						t.Fatalf("local log lost the original error: %s", out.String())
					}
					ev := tr.Evidence()
					if len(ev) != 1 || ev[0].Class != fleetreport.ClassIOScanClassifierUnavail {
						t.Fatalf("classifier warning lost its report: %+v", ev)
					}
					report := fleetreport.BuildRuntimeReport(fleetreport.Observation{}, ev[0], "fp", "inst", "hive", "v5", "commit", now)
					if strings.Contains(report.Body, host) || strings.Contains(report.Body, "no such host") || strings.Contains(report.Body, "/v1/chat/completions") {
						t.Fatalf("public report leaked classifier error: %s", report.Body)
					}
					if !strings.Contains(report.Body, msg) || !strings.Contains(report.Body, "agent=reviewer") || !strings.Contains(report.Body, "fix=check ioscan.classifier.model") {
						t.Fatalf("public report lost actionable context: %s", report.Body)
					}
				})
			}
		}
	}
}

// One GitHub rate-limit episode made every caller log its own WARN, and each
// distinct message was filed as its own unclassified_error_repeated issue
// (hivecommons/hive#15106 and siblings). The WARNs stay in local logs; none
// is observed by the generic feed.
func TestGitHubRateLimitStormLogsWithoutFleetReport(t *testing.T) {
	const secondary = ": 403 API secondary rate limit exceeded until 2026-10-28 17:42:28 -0400 EDT, not making remote request."
	const shed = ": github pacing: request shed while rate-limit caution window is engaged (class low, queue 8.209s ahead, limit 5s)"
	cases := []struct {
		issue int
		msg   string
		args  []any
	}{
		{15110, "stage labels: check digest failed", []any{"repo", "utah-packages", "pr", 797, "error", "GET https://api.github.com/repos/o/r/commits/abc/status?per_page=100" + secondary}},
		{15111, "GitHub refused repo enumeration transiently; serving last-good snapshot for this repo (marked stale)", []any{"repo", "website", "side", "issues", "error", "listing issues for o/website: GET https://api.github.com/repos/o/website/issues?page=1" + secondary}},
		{15112, "failed to fetch check runs", []any{"repo", "testsuite", "pr", 1037, "error", "GET https://api.github.com/repos/o/r/commits/abc/check-runs?per_page=100" + secondary}},
		{15113, "could not check watched repo has_issues", []any{"repo", "o/website", "error", `Get "https://api.github.com/repos/o/website"` + shed}},
		{15114, "advisory: issue state lookup failed", []any{"ref", "o/bluefin#961", "error", `reading o/bluefin#961: Get "https://api.github.com/repos/o/bluefin/issues/961"` + shed}},
		{15115, "stage labels: could not read the author's repository permission; adoption deferred", []any{"repo", "utah-packages", "pr", 798, "error", "GET https://api.github.com/repos/o/r/collaborators/u/permission" + secondary}},
		{15107, "github pacing: low-priority work shed while rate-limit caution window is engaged", []any{"caller", "review-request-watcher", "queue_eta", "6.882s", "budget", "5s"}},
		{15106, "stage labels: could not read maintainer_can_modify; keeping in-place adoption this sweep", []any{"repo", "utah-packages", "pr", 798, "error", "GET https://api.github.com/repos/o/r/pulls/798" + secondary}},
		{15109, "advisory: path existence check failed", []any{"path", "a/b.sh", "repo", "common", "error", `checking o/common/a/b.sh@abc: Get "https://api.github.com/repos/o/common/contents/a/b.sh?ref=abc"` + shed}},
		{0, "failed to fetch check runs", []any{"error", "GET https://api.github.com/repos/o/r/commits/abc/check-runs: 429 Too Many Requests"}},
		{0, "failed to fetch check runs", []any{"error", "GET https://api.github.com/repos/o/r/commits/abc/check-runs: 403 API rate limit of 5000 still exceeded until 2026-10-28 17:42:28 -0400 EDT, not making remote request."}},
	}
	for _, tc := range cases {
		logger, seen, out := newFeed(t)
		for i := 0; i < 8; i++ {
			logger.Warn(tc.msg, tc.args...)
		}
		if len(*seen) != 0 {
			t.Errorf("#%d %q: rate-limited WARN produced observations: %+v", tc.issue, tc.msg, *seen)
		}
		if n := strings.Count(out.String(), "\n"); n != 8 {
			t.Errorf("#%d %q: wrapped handler saw %d records, want 8", tc.issue, tc.msg, n)
		}
	}

	// The same callers failing for any other reason must remain reportable,
	// including a plain 403 permission refusal.
	logger, seen, _ := newFeed(t)
	logger.Warn("failed to fetch check runs", "error", "GET https://api.github.com/repos/o/r/commits/abc/check-runs: 403 Resource not accessible by integration")
	logger.Warn("stage labels: check digest failed", "error", "GET https://api.github.com/repos/o/r/commits/abc/status: 500 Internal Server Error")
	if len(*seen) != 2 {
		t.Fatalf("non-rate-limit failures must remain reportable: %+v", *seen)
	}
}

// #15365: a GitHub 502/503/504 on a fetch is transient and retried next tick.
func TestGitHubUpstreamUnavailableLogsWithoutFleetReport(t *testing.T) {
	cases := []struct {
		msg  string
		args []any
	}{
		{"fetch failed: GET https://api.github.com/repos/o/r/pulls/1336: 503  []", nil},
		{"fetch failed: GET https://api.github.com/repos/o/r: 502 Bad Gateway []", nil},
		{"fetch failed: GET https://api.github.com/repos/o/r: 504 Gateway Timeout []", nil},
		{"self-authored automerge sweep skipped PR", []any{"repo", "kubestellar-mcp", "pr", 1336, "reason", "fetch-pr", "error", "GET https://api.github.com/repos/o/r/pulls/1336: 503  []"}},
		{"failed to fetch check runs", []any{"error", "GET https://api.github.com/repos/o/r/commits/abc/check-runs: 502 Bad Gateway []"}},
		{"failed to fetch check runs", []any{"error", "GET https://api.github.com/repos/o/r/commits/abc/check-runs: 504 Gateway Timeout []"}},
	}
	for _, tc := range cases {
		logger, seen, out := newFeed(t)
		for i := 0; i < 8; i++ {
			logger.Warn(tc.msg, tc.args...)
		}
		if len(*seen) != 0 {
			t.Errorf("%q: upstream-unavailable WARN produced observations: %+v", tc.msg, *seen)
		}
		if n := strings.Count(out.String(), "\n"); n != 8 {
			t.Errorf("%q: wrapped handler saw %d records, want 8", tc.msg, n)
		}
	}

	logger, seen, _ := newFeed(t)
	logger.Warn("self-authored automerge sweep skipped PR", "error", "GET https://api.github.com/repos/o/r/pulls/1336: 500 Internal Server Error []")
	logger.Warn("self-authored automerge sweep skipped PR", "error", "GET https://api.github.com/repos/o/r/pulls/1336: 403 Resource not accessible by integration")
	logger.Warn("fetch failed: GET https://api.github.com/repos/o/r/pulls/503: 404 Not Found")
	logger.Warn("fetch failed: GET https://api.github.com/repos/o/r: 500 Internal Server Error")
	if len(*seen) != 4 {
		t.Fatalf("non-gateway failures must remain reportable: %+v", *seen)
	}
}

// #15785, #15786: fail-closed gates hitting a GitHub 500 on the status read
// are expected and retried, so they are not reported as unclassified.
func TestFailClosedStatus500LogsWithoutFleetReport(t *testing.T) {
	const statusErr = "GET https://api.github.com/repos/o/r/commits/abc/status?per_page=100: 500  []"
	cases := []struct {
		msg  string
		args []any
	}{
		{"merge-eligible lane gate could not evaluate PR; leaving it listed", []any{"repo", "o/r", "number", 3929, "error", "o/r#3929 lane evidence unavailable (status-check; fail closed): " + statusErr}},
		{"trusted-author automerge sweep skipped PR", []any{"repo", "hive", "pr", 15763, "reason", "release-hold-check", "error", statusErr}},
	}
	for _, tc := range cases {
		logger, seen, out := newFeed(t)
		for i := 0; i < 8; i++ {
			logger.Warn(tc.msg, tc.args...)
		}
		if len(*seen) != 0 {
			t.Errorf("%q: produced observations: %+v", tc.msg, *seen)
		}
		if n := strings.Count(out.String(), "\n"); n != 8 {
			t.Errorf("%q: wrapped handler saw %d records, want 8", tc.msg, n)
		}
	}

	logger, seen, _ := newFeed(t)
	logger.Warn("trusted-author automerge sweep skipped PR", "error", "GET https://api.github.com/repos/o/r/pulls/1: 404 Not Found")
	logger.Warn("merge-eligible lane gate could not evaluate PR; leaving it listed", "error", "boom")
	if len(*seen) != 2 {
		t.Fatalf("other failures must remain reportable: %+v", *seen)
	}
}

// #15789: a hub that is restarting answers the heartbeat 5xx and the spoke
// retries on the next beat; auth rejections stay reportable.
func TestHubHeartbeatUnavailableLogsWithoutFleetReport(t *testing.T) {
	body := `{"ok":false,"error":"hive unavailable"}`
	for _, status := range []int{502, 503, 504} {
		logger, seen, out := newFeed(t)
		for i := 0; i < 8; i++ {
			logger.Warn("hub heartbeat rejected", "status", status, "body", body)
		}
		if len(*seen) != 0 {
			t.Errorf("status %d: produced observations: %+v", status, *seen)
		}
		if n := strings.Count(out.String(), "\n"); n != 8 {
			t.Errorf("status %d: wrapped handler saw %d records, want 8", status, n)
		}
	}

	for _, status := range []int{401, 403} {
		logger, seen, _ := newFeed(t)
		logger.Warn("hub heartbeat rejected", "status", status, "body", "unauthorized")
		if len(*seen) != 1 {
			t.Errorf("status %d must remain reportable: %+v", status, *seen)
		}
	}
}

// #15906: the per-head review cap suppressing a duplicate comment is expected.
func TestReviewCommentCappedLogsWithoutFleetReport(t *testing.T) {
	logger, seen, out := newFeed(t)
	for i := 0; i < 8; i++ {
		logger.Warn("review-request watcher: comment suppressed by per-head cap, verdict recorded",
			"agent", "reviewer", "repo", "hivecommons/hive", "number", 15895,
			"reason", "head 20854a8 already carries 1 hive review(s) (limit 1)")
	}
	if len(*seen) != 0 {
		t.Errorf("produced observations: %+v", *seen)
	}
	if n := strings.Count(out.String(), "\n"); n != 8 {
		t.Errorf("wrapped handler saw %d records, want 8", n)
	}
}

// #15333: keep repeated translator identity refusals visible under their own
// gateway class, without treating them as backend credential failures.
func TestInferenceCallerUnidentifiedFeedThresholdAndClear(t *testing.T) {
	now := time.Date(2026, 10, 29, 6, 26, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	var out bytes.Buffer
	logger := slog.New(NewWithObserver(slog.NewJSONHandler(&out, nil), tr.Observe))
	const msg = "inference request from unidentified caller refused"
	warn := func() {
		logger.Warn(msg, "remote", "127.0.0.1:47724", "method", "POST", "path", "/v1/messages")
	}
	for i := 0; i < 4; i++ {
		warn()
		now = now.Add(time.Minute)
		if ev := tr.Evidence(); len(ev) != 0 {
			t.Fatalf("below threshold produced evidence: %+v", ev)
		}
	}
	warn()
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassInferenceCallerUnknown || ev[0].Count != 5 || ev[0].Severity != "medium" || ev[0].Component != "inference-gateway" {
		t.Fatalf("repeated identity refusals = %+v", ev)
	}
	for _, want := range []string{"remote=127.0.0.1:47724", "method=POST", "path=/v1/messages"} {
		if !strings.Contains(ev[0].Diagnostics, want) {
			t.Errorf("diagnostics %q missing %q", ev[0].Diagnostics, want)
		}
	}
	if n := strings.Count(out.String(), "\n"); n != 5 {
		t.Fatalf("wrapped handler saw %d records, want 5", n)
	}
	now = now.Add(21 * time.Minute)
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("idle identity refusals still reported: %+v", ev)
	}
	cleared := tr.TakeCleared()
	if len(cleared) != 1 || cleared[0].Class != fleetreport.ClassInferenceCallerUnknown {
		t.Fatalf("cleared = %+v", cleared)
	}
	// Other translator failures must still reach their existing classes.
	logger2, seen, _ := newFeed(t)
	logger2.Warn("inference upstream failed")
	logger2.Warn("inference no route", "agent", "scanner")
	if len(*seen) != 2 || (*seen)[0].class != fleetreport.ClassInferenceUpstreamError || (*seen)[1].class != fleetreport.ClassUnclassifiedRepeated {
		t.Fatalf("unrelated inference failures = %+v", *seen)
	}
}

// #15986: a watchdog stall caused by a provider inference block (429
// budget_exceeded) is upstream backpressure, not an unclassified defect.
func TestInferenceBlockedStallLogsWithoutFleetReport(t *testing.T) {
	logger, seen, out := newFeed(t)
	for i := 0; i < 10; i++ {
		logger.Warn("watchdog: agent not producing", "agent", "dedupe",
			"detail", `blocked: inference (rate_limit): API Error: Request rejected (429) {"error":{"type":"budget_exceeded"}}`)
	}
	if len(*seen) != 0 {
		t.Errorf("produced observations: %+v", *seen)
	}
	if n := strings.Count(out.String(), "\n"); n != 10 {
		t.Errorf("wrapped handler saw %d records, want 10", n)
	}

	logger, seen, _ = newFeed(t)
	logger.Warn("watchdog: agent not producing", "agent", "dedupe", "detail", "no production evidence for 2h0m0s")
	if len(*seen) != 1 {
		t.Fatalf("other stalls must remain reportable: %+v", *seen)
	}
}
