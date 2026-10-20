package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

type outcomeStampCall struct {
	repo   string
	number int
	st     github.PRState
	now    time.Time
}

// fakeOutcomeResolver answers GetPRState from a table and records every
// outcome stamp the ledger pass asks for.
type fakeOutcomeResolver struct {
	states map[string]github.PRState
	stamps []outcomeStampCall
}

func (f *fakeOutcomeResolver) GetPRState(_ context.Context, repo string, number int) (github.PRState, error) {
	return f.states[repo+"#"+strconv.Itoa(number)], nil
}

func (f *fakeOutcomeResolver) RecordReviewEvidenceOutcome(repo string, number int, st github.PRState, now time.Time) {
	f.stamps = append(f.stamps, outcomeStampCall{repo: repo, number: number, st: st, now: now})
}

// Every PR the ledger resolves this cycle is handed to the evidence outcome
// recorder with the state the ledger fetched (no second fetch), including a
// PR GitHub still reports open; a client without the recorder is fine.
func TestRecordReviewOutcomesStampsEvidence(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Org = "o"
	path := filepath.Join(t.TempDir(), "review-outcomes.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)

	first := &github.ActionableResult{}
	first.PRs.Items = []github.PullRequest{{Repo: "r", Number: 1, Author: "alice"}, {Repo: "r", Number: 2, Author: "bob"}, {Repo: "r", Number: 3, Author: "cy"}}
	recordReviewOutcomesAt(context.Background(), cfg, &fakeOutcomeResolver{}, first, path, t0, logger)

	resolver := &fakeOutcomeResolver{states: map[string]github.PRState{
		"o/r#1": {State: "closed", MergedAt: t0.Add(30 * time.Minute), ClosedAt: t0.Add(30 * time.Minute), MergedBy: "alice"},
		"o/r#2": {State: "closed", ClosedAt: t0.Add(40 * time.Minute)},
		"o/r#3": {State: "open"},
	}}
	t1 := t0.Add(time.Hour)
	recordReviewOutcomesAt(context.Background(), cfg, resolver, &github.ActionableResult{}, path, t1, logger)

	if len(resolver.stamps) != 3 {
		t.Fatalf("stamps = %+v, want one per vanished PR", resolver.stamps)
	}
	for i, want := range []outcomeStampCall{
		{repo: "o/r", number: 1, st: resolver.states["o/r#1"], now: t1},
		{repo: "o/r", number: 2, st: resolver.states["o/r#2"], now: t1},
		{repo: "o/r", number: 3, st: resolver.states["o/r#3"], now: t1},
	} {
		if resolver.stamps[i] != want {
			t.Errorf("stamp[%d] = %+v, want %+v", i, resolver.stamps[i], want)
		}
	}

	// A state getter without the recorder still resolves the ledger.
	other := filepath.Join(t.TempDir(), "review-outcomes.json")
	recordReviewOutcomesAt(context.Background(), cfg, plainStateGetter{states: resolver.states}, first, other, t0, logger)
	recordReviewOutcomesAt(context.Background(), cfg, plainStateGetter{states: resolver.states}, &github.ActionableResult{}, other, t1, logger)
}

type plainStateGetter struct{ states map[string]github.PRState }

func (p plainStateGetter) GetPRState(_ context.Context, repo string, number int) (github.PRState, error) {
	return p.states[repo+"#"+strconv.Itoa(number)], nil
}
