package effects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestClaimValidate(t *testing.T) {
	cases := []struct {
		name    string
		claim   Claim
		wantErr string
	}{
		{"valid", Claim{Repo: "o/r", Kind: KindIssueComment, Target: "issue/1"}, ""},
		{"missing repo", Claim{Kind: KindIssueComment, Target: "t"}, "repo"},
		{"repo without slash", Claim{Repo: "justname", Kind: KindIssueComment, Target: "t"}, "repo"},
		{"whitespace repo", Claim{Repo: "   ", Kind: KindIssueComment, Target: "t"}, "repo"},
		{"missing kind", Claim{Repo: "o/r", Target: "t"}, "kind"},
		{"whitespace kind", Claim{Repo: "o/r", Kind: " ", Target: "t"}, "kind"},
		{"missing target", Claim{Repo: "o/r", Kind: KindIssueComment}, "target"},
		{"whitespace target", Claim{Repo: "o/r", Kind: KindIssueComment, Target: "\t"}, "target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.claim.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestExecuteNilBoundaryFallsBackToNoop(t *testing.T) {
	called := false
	res, err := Execute(context.Background(), nil, Claim{}, func(context.Context) (Result, error) {
		called = true
		return Result{Provenance: "p"}, nil
	})
	if err != nil || !called || res.Provenance != "p" {
		t.Fatalf("Execute(nil boundary) = %+v, %v, called=%v", res, err, called)
	}
}

func TestNoopBoundaryPassesThroughError(t *testing.T) {
	want := errors.New("boom")
	_, err := NoopBoundary{}.Execute(context.Background(), Claim{}, func(context.Context) (Result, error) {
		return Result{}, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestStableDigestDeterministicAndBoundaryAware(t *testing.T) {
	first := StableDigest("a", "b")
	second := StableDigest("a", "b")
	if first != second {
		t.Fatal("digest not deterministic")
	}
	if StableDigest("a", "b") == StableDigest("b", "a") {
		t.Fatal("digest ignores part order")
	}
	if StableDigest("ab", "c") == StableDigest("a", "bc") {
		t.Fatal("digest collides across part boundaries")
	}
	if len(StableDigest()) != 64 {
		t.Fatal("digest is not hex sha256")
	}
}

func TestStableInputsSortedAndEmpty(t *testing.T) {
	if StableInputs(nil) != "" || StableInputs(map[string]string{}) != "" {
		t.Fatal("empty inputs must produce empty string")
	}
	got := StableInputs(map[string]string{"b": "2", "a": "1"})
	want := "a=1\nb=2\n"
	if got != want {
		t.Fatalf("StableInputs = %q, want %q", got, want)
	}
}

func TestRecorderCountsAndNilSafety(t *testing.T) {
	var nilRec *Recorder
	nilRec.SetMode("x")
	nilRec.IncJournaled()
	nilRec.IncFenced()
	nilRec.IncDenied()
	if got := nilRec.Snapshot(); got != (Stats{}) {
		t.Fatalf("nil recorder snapshot = %+v", got)
	}

	rec := &Recorder{}
	rec.SetMode("journal")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.IncJournaled()
			rec.IncFenced()
			rec.IncDenied()
		}()
	}
	wg.Wait()
	got := rec.Snapshot()
	want := Stats{Mode: "journal", Journaled: 10, Fenced: 10, Denied: 10}
	if got != want {
		t.Fatalf("Snapshot = %+v, want %+v", got, want)
	}
}

type stubBoundary struct{ err error }

func (s stubBoundary) Execute(ctx context.Context, _ Claim, effect func(context.Context) (Result, error)) (Result, error) {
	if s.err != nil {
		return Result{}, s.err
	}
	return effect(ctx)
}

func TestLoggingBoundarySuccessIncrementsJournaled(t *testing.T) {
	rec := &Recorder{}
	b := LoggingBoundary{Next: stubBoundary{}, Recorder: rec}
	res, err := b.Execute(context.Background(), Claim{Repo: "o/r", Kind: KindBranchPush, Target: "b"},
		func(context.Context) (Result, error) { return Result{Provenance: "ok"}, nil })
	if err != nil || res.Provenance != "ok" {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if got := rec.Snapshot().Journaled; got != 1 {
		t.Fatalf("Journaled = %d, want 1", got)
	}
}

func TestLoggingBoundaryDeniedLogsAndSkipsJournal(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))
	rec := &Recorder{}
	b := LoggingBoundary{Next: stubBoundary{err: fmt.Errorf("wrapped: %w", ErrDenied)}, Logger: logger, Recorder: rec}
	_, err := b.Execute(context.Background(), Claim{Repo: "o/r", Kind: KindPullRequestMerge, Target: "pr/1"},
		func(context.Context) (Result, error) { t.Fatal("effect must not run"); return Result{}, nil })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if !strings.Contains(sb.String(), "mutation boundary denied") {
		t.Fatalf("denial not logged: %q", sb.String())
	}
	if got := rec.Snapshot().Journaled; got != 0 {
		t.Fatalf("Journaled = %d, want 0 on denial", got)
	}
}

func TestLoggingBoundaryNonDeniedErrorNotLogged(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))
	b := LoggingBoundary{Next: stubBoundary{err: errors.New("transient")}, Logger: logger}
	_, err := b.Execute(context.Background(), Claim{Repo: "o/r", Kind: KindIssueCreate, Target: "t"},
		func(context.Context) (Result, error) { return Result{}, nil })
	if err == nil {
		t.Fatal("want error")
	}
	if sb.Len() != 0 {
		t.Fatalf("unexpected log output: %q", sb.String())
	}
}

func TestLoggingBoundaryNilNextRunsEffect(t *testing.T) {
	res, err := LoggingBoundary{}.Execute(context.Background(), Claim{Repo: "o/r", Kind: KindLabelMutation, Target: "t"},
		func(context.Context) (Result, error) { return Result{Provenance: "direct"}, nil })
	if err != nil || res.Provenance != "direct" {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
}
