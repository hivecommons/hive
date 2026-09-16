package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// runEvalCycle takes 17 collaborators and is 0% covered because constructing
// them is the whole problem (#7232). One path needs none of them: a hive with
// no GitHub credentials must bail out before the first API call.
//
// That guard is worth pinning on its own. Without it the hive logs an
// enumeration failure once per eval interval forever, which reads as a GitHub
// outage rather than as "this hive was never given credentials" — the
// misleading-symptom bug the guard's own comment describes.
//
// These tests deliberately make NO production change. They are the reachable
// part of #7232 step 1; the seam extraction that makes the rest of the
// function testable is still outstanding.

func evalCycleTestLogger() (*slog.Logger, func() string) {
	var mu sync.Mutex
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(&lockedWriter{mu: &mu, buf: buf}, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// callEvalCycleWithoutCredentials invokes runEvalCycle with a nil GitHub
// client and nil collaborators. It is safe precisely because the credential
// guard returns first; if that guard is ever removed this panics, which is the
// point.
func callEvalCycleWithoutCredentials(ctx context.Context, cfg *config.Config, logger *slog.Logger) {
	runEvalCycle(ctx, cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, logger)
}

func TestRunEvalCycleWithoutCredentialsReturnsBeforeEnumerating(t *testing.T) {
	logger, logs := evalCycleTestLogger()
	cfg := &config.Config{HiveID: "test-hive"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		callEvalCycleWithoutCredentials(context.Background(), cfg, logger)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runEvalCycle did not return with a nil GitHub client; the credential guard is not short-circuiting")
	}

	if !strings.Contains(logs(), "skipping eval cycle") {
		t.Fatalf("a hive without credentials must say why it skipped, got:\n%s", logs())
	}
}

// TestRunEvalCycleWithoutCredentialsStatesTheCause guards the wording, not
// just the early return. The whole reason this branch exists is that the
// alternative — a recurring enumeration failure — points the operator at
// GitHub instead of at their own missing config.
func TestRunEvalCycleWithoutCredentialsStatesTheCause(t *testing.T) {
	logger, logs := evalCycleTestLogger()
	callEvalCycleWithoutCredentials(context.Background(), &config.Config{HiveID: "test-hive"}, logger)

	out := logs()
	if !strings.Contains(out, "credentials") {
		t.Fatalf("the skip reason must name credentials as the cause, got:\n%s", out)
	}
	for _, misleading := range []string{"failed to enumerate", "enumeration failed", "level=ERROR", "level=WARN"} {
		if strings.Contains(out, misleading) {
			t.Fatalf("a hive with no credentials is misconfigured, not broken; must not log %q:\n%s", misleading, out)
		}
	}
}

// TestRunEvalCycleWithoutCredentialsHonorsCancelledContext proves the guard is
// reached independently of context state, so a shutting-down hive cannot be
// pushed down the enumeration path.
func TestRunEvalCycleWithoutCredentialsHonorsCancelledContext(t *testing.T) {
	logger, logs := evalCycleTestLogger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		callEvalCycleWithoutCredentials(ctx, &config.Config{HiveID: "test-hive"}, logger)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runEvalCycle blocked on a cancelled context with no credentials")
	}
	if !strings.Contains(logs(), "skipping eval cycle") {
		t.Fatalf("cancelled context must still take the credential guard, got:\n%s", logs())
	}
}
