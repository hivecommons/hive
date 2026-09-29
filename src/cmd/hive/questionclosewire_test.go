package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/scheduler"
)

func TestWireQuestionAutoclose_RespectsToggle(t *testing.T) {
	t.Setenv(config.QuestionAutocloseEnvVar, "")
	questionAutoclosePath = filepath.Join(t.TempDir(), "question-autoclose.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := func() *github.Client { return nil }

	off := &config.Config{}
	if m := wireQuestionAutoclose(ctx, off, scheduler.New(off, logger), client, logger); m != nil {
		t.Fatal("question_autoclose off still built a manager")
	}
	if m := wireQuestionAutoclose(ctx, nil, nil, client, logger); m != nil {
		t.Fatal("nil config must build nothing")
	}

	on := &config.Config{Governor: config.GovernorConfig{QuestionAutoclose: config.QuestionAutocloseConfig{Enabled: true, Hours: 2}}}
	m := wireQuestionAutoclose(ctx, on, scheduler.New(on, logger), client, logger)
	if m == nil || !m.Enabled() {
		t.Fatal("enabled config did not build a manager")
	}
	// A nil GitHub client makes a tick a no-op rather than a panic.
	if rep := m.Tick(ctx); rep.Scheduled != 0 || rep.Closed != 0 {
		t.Fatalf("tick without a client did work: %+v", rep)
	}

	// The env override turns it on for a config-off hive.
	t.Setenv(config.QuestionAutocloseEnvVar, "true")
	if m := wireQuestionAutoclose(ctx, off, scheduler.New(off, logger), nil, logger); m == nil {
		t.Fatal("HIVE_QUESTION_AUTOCLOSE=true did not enable the feature")
	} else if rep := m.Tick(ctx); rep.Scheduled != 0 {
		t.Fatalf("nil client provider did work: %+v", rep)
	}
}

// TestQuestionAutocloseView_ReadsManagerLazily pins the dashboard adapter:
// before bootSupervision stores a manager (and whenever the feature is off)
// it reports disabled with an empty schedule, and after the store it reflects
// the live manager without re-registering the dashboard dependencies.
func TestQuestionAutocloseView_ReadsManagerLazily(t *testing.T) {
	t.Setenv(config.QuestionAutocloseEnvVar, "")
	questionAutoclosePath = filepath.Join(t.TempDir(), "question-autoclose.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := &boot{}
	view := questionAutocloseView{b: b}
	if view.Enabled() || len(view.ScheduledQuestions()) != 0 {
		t.Fatal("view before any manager must be disabled and empty")
	}
	if (questionAutocloseView{}).Enabled() {
		t.Fatal("zero view must be disabled")
	}

	on := &config.Config{Governor: config.GovernorConfig{QuestionAutoclose: config.QuestionAutocloseConfig{Enabled: true, Hours: 2}}}
	b.questionAutoclose.Store(wireQuestionAutoclose(ctx, on, scheduler.New(on, logger), func() *github.Client { return nil }, logger))
	if !view.Enabled() {
		t.Fatal("view did not pick up the stored manager")
	}
	if got := view.ScheduledQuestions(); got == nil || len(got) != 0 {
		t.Fatalf("fresh manager schedule = %#v, want empty non-nil slice", got)
	}
}
