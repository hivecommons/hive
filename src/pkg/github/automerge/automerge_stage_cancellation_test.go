package automerge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type interruptedStageWriter struct {
	err   error
	calls int
}

func (w *interruptedStageWriter) Apply(context.Context, stageengine.Ref, []string, stagelabels.Desired) (stagelabels.Outcome, error) {
	w.calls++
	return stagelabels.Outcome{Added: []string{"partial-write"}}, w.err
}

// A writer deadline can truncate the batch while the sweep context is live.
// Unattempted PRs must not post regression notes (or other public side effects).
func TestApplyStageLabelsSkipsUnattemptedTail(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			api := &stageAPI{labels: map[string]string{}}
			for _, d := range stagelabels.AllLabelDefs() {
				api.labels[d.Name] = d.Color
			}
			srv := newStageAPI(t, api)
			defer srv.Close()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
			e, _, store := newStageEngine(t, srv.URL, clock)
			writer := &interruptedStageWriter{err: fmt.Errorf("request: %w", cause)}
			e.stageEngine = &stageengine.Options{Mode: stageengine.ModeEnforce, Writer: writer}
			for n := 1; n <= 3; n++ {
				store.Observe(stageKey("acme", "widget", n), stagelabels.MergeQueue, "head-changed-since-eval")
				e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", stagePR(n, "stage/100-merge-queue", "role/trusted", "author/human"), "check-failing:unit", false, nil)
				e.noteStageRole("acme", "widget", n, stagelabels.RoleTrusted)
			}
			ctx := context.Background()
			res, err := e.ApplyStageLabels(ctx)
			if !errors.Is(err, cause) || ctx.Err() != nil || writer.calls != 1 || res.Writes != 1 {
				t.Fatalf("err=%v ctx=%v calls=%d result=%+v", err, ctx.Err(), writer.calls, res)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if len(api.comments) != 1 || res.Comments != 1 {
				t.Fatalf("only the attempted PR may post its regression note: comments=%v result=%+v", api.comments, res)
			}
		})
	}
}
