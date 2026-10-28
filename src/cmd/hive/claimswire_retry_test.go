package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

func TestClaimEffectRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		succeed  bool
		attempts int
	}{
		{"deadline then success", context.DeadlineExceeded, true, 2},
		{"uncertain then success", fmt.Errorf("stage: %w", effects.ErrNeedsReconciliation), true, 2},
		{"exhausted", effects.ErrNeedsReconciliation, false, claimsGitHubAttempts},
		{"permanent", errors.New("forbidden"), false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &claimEffectQueue{}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			attempts := 0
			var contexts []context.Context
			fn := q.retryEffect(context.Background(), "o/r#1", "claimed stage", logger, 5*time.Millisecond, time.Millisecond, func(ctx context.Context) error {
				if ctx.Err() != nil {
					t.Error("attempt started with expired context")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Error("attempt has no deadline")
				}
				for _, previous := range contexts {
					if previous == ctx || previous.Err() == nil {
						t.Error("attempt did not receive a fresh context or previous attempt was not canceled")
					}
				}
				contexts = append(contexts, ctx)
				attempts++
				if tc.succeed && attempts > 1 {
					return nil
				}
				if tc.err == context.DeadlineExceeded {
					<-ctx.Done()
					return ctx.Err()
				}
				return tc.err
			})
			done := make(chan struct{})
			q.run("o/r#1", fn)
			q.run("o/r#1", func() { close(done) })
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("retry queue never drained")
			}
			if attempts != tc.attempts {
				t.Fatalf("attempts = %d, want %d", attempts, tc.attempts)
			}
			if got := strings.Count(logs.String(), "retrying GitHub side effect"); got != tc.attempts-1 {
				t.Errorf("retry logs = %d, want %d", got, tc.attempts-1)
			}
			wantWarnings := 1
			if tc.succeed {
				wantWarnings = 0
			}
			if got := strings.Count(logs.String(), "GitHub side effect failed"); got != wantWarnings {
				t.Errorf("final warnings = %d, want %d", got, wantWarnings)
			}
		})
	}
}

func TestClaimEffectRetryCancellation(t *testing.T) {
	q := &claimEffectQueue{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	attempts := 0
	q.run("issue", q.retryEffect(ctx, "issue", "claim", nil, time.Second, time.Hour, func(context.Context) error {
		attempts++
		close(started)
		return context.DeadlineExceeded
	}))
	<-started
	cancel()
	done := make(chan struct{})
	q.run("issue", func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt retry backoff")
	}
	if attempts != 1 {
		t.Fatalf("attempts after shutdown = %d, want 1", attempts)
	}
}

// Embedding the rest of the interface keeps this focused on the two facts
// emitted by the claim hooks, without exercising the mutation ledger itself.
type retryClaimFactSink struct {
	stageengine.FactSink
	order   []string
	started chan struct{}
	unblock chan struct{}
	done    chan struct{}
}

func (s *retryClaimFactSink) Claimed(context.Context, stageengine.Ref, stageengine.ClaimFact) error {
	s.order = append(s.order, "claim")
	if len(s.order) == 1 {
		close(s.started)
		<-s.unblock
		return fmt.Errorf("stage write: %w", effects.ErrNeedsReconciliation)
	}
	return nil
}

func (s *retryClaimFactSink) ClaimReleased(context.Context, stageengine.Ref, string) error {
	s.order = append(s.order, "release")
	close(s.done)
	return nil
}

func TestClaimHooks_RetryBeforeRelease(t *testing.T) {
	sink := &retryClaimFactSink{started: make(chan struct{}), unblock: make(chan struct{}), done: make(chan struct{})}
	gh := github.NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	gh.SetStageFactSink(sink)
	cfg := &config.Config{}
	cfg.Governor.Claims.Label = boolPtr(true)
	cfg.Governor.Claims.Comment = boolPtr(false)
	hooks := githubClaimHooks(context.Background(), cfg, func() *github.Client { return gh }, nil)
	claim := claims.Claim{Repo: "o/r", Issue: 1, Holder: "ann", Kind: claims.KindHuman}
	hooks.OnClaimed(claim, claims.OutcomeClaimed)
	<-sink.started
	hooks.OnReleased(claim, claims.ReasonExpired)
	close(sink.unblock)
	select {
	case <-sink.done:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not follow retried claim")
	}
	if want := []string{"claim", "claim", "release"}; !reflect.DeepEqual(sink.order, want) {
		t.Fatalf("effects = %v, want %v", sink.order, want)
	}
}

func TestClaimEffectRetryRetainsRemainingEffects(t *testing.T) {
	q := &claimEffectQueue{}
	var order []string
	attempts := 0
	retry := q.retryEffect(context.Background(), "issue", "claim stage", nil, time.Second, time.Millisecond, func(context.Context) error {
		order = append(order, "stage")
		attempts++
		if attempts == 1 {
			return effects.ErrNeedsReconciliation
		}
		return nil
	})
	q.run("issue", func() {
		q.prepend("issue",
			func() { order = append(order, "comment") },
			retry,
			func() { order = append(order, "label") },
		)
	})
	done := make(chan struct{})
	q.run("issue", func() {
		order = append(order, "release")
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry lost remaining effects")
	}
	if want := []string{"comment", "stage", "stage", "label", "release"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("effects = %v, want %v", order, want)
	}
}
