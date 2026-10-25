package github

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type issueStageWriterFunc func(context.Context, stageengine.Ref, []string, stagelabels.Desired) (stagelabels.Outcome, error)

func (f issueStageWriterFunc) Apply(ctx context.Context, ref stageengine.Ref, labels []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
	return f(ctx, ref, labels, want)
}

func TestIssueStageSweepCancellationDoesNotReportUnattemptedWrites(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	result := &ActionableResult{}
	for n := 1; n <= 3; n++ {
		result.Issues.Items = append(result.Issues.Items, Issue{Repo: "acme/widgets", Number: n})
	}
	fake.seed(result)
	path := filepath.Join(t.TempDir(), "stage-labels.json")
	store := stagelabels.NewStore(path, stageNow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	writer := issueStageWriterFunc(func(ctx context.Context, _ stageengine.Ref, _ []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
		calls++
		out := stagelabels.Outcome{Stage: want.Stage, Added: []string{want.Stage.Label()}}
		if calls == 1 {
			// A partial write succeeded before the sweep budget ran out.
			cancel()
			return out, ctx.Err()
		}
		return out, nil
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts := &stageengine.Options{Writer: writer}
	res := SyncIssueStageLabelsWith(ctx, c, result, store, stageNow(), logger, opts)
	if calls != 1 || res.Writes != 1 {
		t.Fatalf("calls=%d writes=%d; want one attempted, partially successful write", calls, res.Writes)
	}
	if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "write failed") {
		t.Fatalf("sweep cancellation must not generate item failures: %s", &logs)
	}
	if strings.Count(logs.String(), "sweep interrupted") != 1 {
		t.Fatalf("want one interruption diagnostic: %s", &logs)
	}
	// A restart must retain observations (and their dwell timers), even
	// though cancellation skipped the sweep's legacy-cleanup tail.
	reloaded := stagelabels.NewStore(path, stageNow)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		key := stagelabels.Key("acme/widgets", n)
		want, _ := store.Get(key)
		got, ok := reloaded.Get(key)
		if !ok || got.Stage != want.Stage || !got.EnteredAt.Equal(want.EnteredAt) {
			t.Fatalf("persisted observation %s: got %+v (present=%t), want %+v", key, got, ok, want)
		}
	}
	// Cancellation leaves the remaining issues eligible for the next pass.
	res = SyncIssueStageLabelsWith(context.Background(), c, result, store, stageNow(), logger, opts)
	if calls != 4 || res.Writes != 3 {
		t.Fatalf("retry: calls=%d writes=%d; want all three issues retried", calls, res.Writes)
	}
}

func TestIssueStageExpiredSweepDoesNotProvision(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "acme/widgets", Number: 1}}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	res := SyncIssueStageLabels(ctx, c, result, stagelabels.NewStore("", stageNow), stageNow(), logger)
	if res.Items != 0 || fake.lists != 0 || strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("expired sweep should stop before planning/provisioning: result=%+v lists=%d logs=%s", res, fake.lists, &logs)
	}
	if strings.Count(logs.String(), "sweep interrupted") != 1 {
		t.Fatalf("want one deadline diagnostic: %s", &logs)
	}
}

func TestIssueStageRequestDeadlineStillWarnsWhenSweepIsLive(t *testing.T) {
	fake := newIssueStageServer(t)
	c := issueStageTestClient(t, fake.srv, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "acme/widgets", Number: 1}, {Repo: "acme/widgets", Number: 2}}
	fake.seed(result)
	calls := 0
	writer := issueStageWriterFunc(func(context.Context, stageengine.Ref, []string, stagelabels.Desired) (stagelabels.Outcome, error) {
		calls++
		return stagelabels.Outcome{}, context.DeadlineExceeded
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	SyncIssueStageLabelsWith(context.Background(), c, result, stagelabels.NewStore("", stageNow), stageNow(), logger, &stageengine.Options{Writer: writer})
	if calls != 2 || strings.Count(logs.String(), "issue stage labels: write failed") != 2 {
		t.Fatalf("request-local timeouts must still warn and allow later items: calls=%d logs=%s", calls, &logs)
	}
}
