package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// #7232 recommendation 2: runEvalCycle's decision points move into seams that
// can be exercised without a process. This covers the pinned advisory issue
// retry — the #4167 behaviour whose absence silently wedged the digest for a
// whole process lifetime, and which had no test because it was inline in a
// 891-line function pinned to a real *github.Client.

func TestEnsurePinnedAdvisoryIssueSkipsWhenNothingToDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repo   string
		issues map[string]int
		ensure func(context.Context, string) (int, error)
	}{
		{
			name:   "no primary repo",
			repo:   "",
			issues: map[string]int{},
		},
		{
			// The caller expresses "no GitHub client" as a nil ensure.
			name:   "no github client",
			repo:   "o/r",
			issues: map[string]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := advisoryEnsureDeps{ensure: tc.ensure}
			if err := ensurePinnedAdvisoryIssue(context.Background(), tc.issues, tc.repo, deps, nil); err != nil {
				t.Fatalf("want nil error, got %v", err)
			}
		})
	}
}

func TestEnsurePinnedAdvisoryIssueDoesNotRetryWhenResolved(t *testing.T) {
	// The cheapness claim in the comment ("nothing once resolved") is load
	// bearing: this runs every eval cycle and a stray search per cycle is a
	// rate-limit cost on every hive.
	calls := 0
	deps := advisoryEnsureDeps{
		ensure: func(context.Context, string) (int, error) { calls++; return 7, nil },
	}
	issues := map[string]int{"o/r": 42}

	if err := ensurePinnedAdvisoryIssue(context.Background(), issues, "o/r", deps, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("resolved issue should not be re-ensured, got %d call(s)", calls)
	}
	if issues["o/r"] != 42 {
		t.Errorf("resolved number overwritten: %d", issues["o/r"])
	}
}

func TestEnsurePinnedAdvisoryIssueRecordsResolvedNumber(t *testing.T) {
	var gotKey, gotVal string
	deps := advisoryEnsureDeps{
		ensure: func(_ context.Context, repo string) (int, error) {
			if repo != "o/r" {
				t.Errorf("ensure called for %q", repo)
			}
			return 1234, nil
		},
		setenv: func(k, v string) error { gotKey, gotVal = k, v; return nil },
	}
	issues := map[string]int{}

	if err := ensurePinnedAdvisoryIssue(context.Background(), issues, "o/r", deps, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Mutated in place: later stages in the same cycle read this map.
	if issues["o/r"] != 1234 {
		t.Errorf("advisoryIssues[o/r] = %d, want 1234", issues["o/r"])
	}
	if gotKey != "HIVE_ADVISORY_ISSUE" || gotVal != "1234" {
		t.Errorf("env stamp = %q=%q, want HIVE_ADVISORY_ISSUE=1234", gotKey, gotVal)
	}
}

func TestEnsurePinnedAdvisoryIssueReturnsCauseOnFailure(t *testing.T) {
	// The returned error is the whole point of #4329: the digest post path
	// records the CAUSE (e.g. Issues disabled on a fork), not just "no issue".
	wantErr := errors.New("issues are disabled for this repository")
	var setenvCalls int
	deps := advisoryEnsureDeps{
		ensure: func(context.Context, string) (int, error) { return 0, wantErr },
		setenv: func(string, string) error { setenvCalls++; return nil },
	}
	issues := map[string]int{}

	err := ensurePinnedAdvisoryIssue(context.Background(), issues, "o/r", deps, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("want the ensure error back, got %v", err)
	}
	if _, ok := issues["o/r"]; ok {
		t.Errorf("failed ensure must not record a number: %v", issues)
	}
	if setenvCalls != 0 {
		t.Errorf("failed ensure must not stamp the environment, got %d call(s)", setenvCalls)
	}
}

func TestEnsurePinnedAdvisoryIssueRetriesUntilResolved(t *testing.T) {
	// The wedge #4167 describes: a transient boot failure must not disable the
	// retry for the rest of the process. Two failing cycles, then success.
	attempts := 0
	deps := advisoryEnsureDeps{
		ensure: func(context.Context, string) (int, error) {
			attempts++
			if attempts < 3 {
				return 0, errors.New("secondary rate limit")
			}
			return 99, nil
		},
		setenv: func(string, string) error { return nil },
	}
	issues := map[string]int{}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = ensurePinnedAdvisoryIssue(ctx, issues, "o/r", deps, nil)
	}
	if issues["o/r"] != 99 {
		t.Fatalf("want the issue resolved on the third cycle, got %v", issues)
	}
	// And it stops asking once resolved.
	_ = ensurePinnedAdvisoryIssue(ctx, issues, "o/r", deps, nil)
	if attempts != 3 {
		t.Errorf("want 3 attempts total, got %d", attempts)
	}
}

func TestEnsurePinnedAdvisoryIssueToleratesNilSetenvAndLogger(t *testing.T) {
	deps := advisoryEnsureDeps{
		ensure: func(context.Context, string) (int, error) { return 5, nil },
	}
	issues := map[string]int{}
	if err := ensurePinnedAdvisoryIssue(context.Background(), issues, "o/r", deps, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues["o/r"] != 5 {
		t.Errorf("want the number recorded even without a setenv, got %v", issues)
	}
}

func TestEnsurePinnedAdvisoryIssueLogsResolutionAndFailure(t *testing.T) {
	// Both log lines are the operator's only signal for this path, so they are
	// asserted rather than assumed — the same posture hub_boot_seams took.
	for _, tc := range []struct {
		name    string
		ensure  func(context.Context, string) (int, error)
		wantMsg string
	}{
		{
			name:    "resolved",
			ensure:  func(context.Context, string) (int, error) { return 3, nil },
			wantMsg: "advisory issue resolved on retry",
		},
		{
			name:    "still unresolved",
			ensure:  func(context.Context, string) (int, error) { return 0, errors.New("boom") },
			wantMsg: "advisory issue still unresolved — digest cannot be posted this cycle",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf capturingHandler
			logger := slog.New(&buf)
			deps := advisoryEnsureDeps{ensure: tc.ensure, setenv: func(string, string) error { return nil }}
			_ = ensurePinnedAdvisoryIssue(context.Background(), map[string]int{}, "o/r", deps, logger)
			if !buf.sawMessage(tc.wantMsg) {
				t.Errorf("want log %q, got %v", tc.wantMsg, buf.messages)
			}
		})
	}
}

// capturingHandler is a minimal slog.Handler that records message text.
type capturingHandler struct {
	messages []string
	attrs    []slog.Attr
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.messages = append(h.messages, r.Message)
	return nil
}
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.attrs = append(h.attrs, attrs...)
	return h
}
func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

func (h *capturingHandler) sawMessage(want string) bool {
	for _, m := range h.messages {
		if m == want {
			return true
		}
	}
	return false
}
