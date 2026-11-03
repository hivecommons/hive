package automerge

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type prStageWriterFunc func(context.Context, stageengine.Ref, []string, stagelabels.Desired) (stagelabels.Outcome, error)

func (f prStageWriterFunc) Apply(ctx context.Context, ref stageengine.Ref, labels []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
	return f(ctx, ref, labels, want)
}

func TestApplyStageLabelsDoesNotFollowUpUnattemptedTail(t *testing.T) {
	for _, stop := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(stop.Error(), func(t *testing.T) {
			api := &stageAPI{labels: map[string]string{}}
			srv := newStageAPI(t, api)
			defer srv.Close()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
			e, _, _ := newStageEngine(t, srv.URL, clock)
			path := filepath.Join(t.TempDir(), "stage.json")
			e.stageStore = stagelabels.NewStore(path, clock.now)
			var attempted []int
			e.stageEngine = &stageengine.Options{Writer: prStageWriterFunc(func(_ context.Context, ref stageengine.Ref, _ []string, want stagelabels.Desired) (stagelabels.Outcome, error) {
				attempted = append(attempted, ref.Number)
				out := stagelabels.Outcome{Stage: want.Stage, Added: []string{want.Stage.Label()}}
				if len(attempted) == 2 {
					// A partial write and a wrapped request-local error: the
					// sweep context is live, but the batch must stop here.
					return out, fmt.Errorf("request: %w", stop)
				}
				return out, nil
			})}
			observe := func() {
				for n := 1; n <= 3; n++ {
					e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", trustedHuman071PR(n, "aaa"), "conflicting", false, nil)
					e.noteStageRole("acme", "widget", n, stagelabels.RoleTrusted)
				}
			}
			observe()
			ctx := context.Background()
			res, err := e.ApplyStageLabels(ctx)
			if !errors.Is(err, stop) || ctx.Err() != nil || len(attempted) != 2 || res.Writes != 2 {
				t.Fatalf("result=%+v err=%v attempted=%v ctx.Err=%v", res, err, attempted, ctx.Err())
			}
			if notes := humanNoteComments(api); len(notes) != 2 || res.Comments != 2 {
				t.Fatalf("unattempted PR must not receive a public note: result=%+v notes=%q", res, notes)
			}
			// The unattempted PR is still observed and persisted for retry;
			// saving must not mark its public follow-up as completed.
			reloaded := stagelabels.NewStore(path, clock.now)
			if err := reloaded.Load(); err != nil {
				t.Fatal(err)
			}
			if _, ok := reloaded.Get(stageKey("acme", "widget", 3)); !ok {
				t.Fatal("unattempted PR observation was not persisted")
			}
			observe()
			res, err = e.ApplyStageLabels(ctx)
			if err != nil || len(attempted) != 5 || res.Writes != 3 || res.Comments != 1 || len(humanNoteComments(api)) != 3 {
				t.Fatalf("retry: result=%+v err=%v attempted=%v notes=%q", res, err, attempted, humanNoteComments(api))
			}
		})
	}
}
