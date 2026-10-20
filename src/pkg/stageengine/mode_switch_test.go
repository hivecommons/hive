package stageengine

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/stagelabels/shadowdiff"
)

func newSwitchReconciler(t *testing.T, mode Mode, w StageWriter, legacy func(Facts) Decision, shadow *shadowdiff.Recorder) (Reconciler, *Counters, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	m := NewCounters()
	r := New(Options{
		Mode:    mode,
		Store:   stagelabels.NewStore("", nil),
		Writer:  w,
		Issue:   tableDeriver(stagelabels.IssueItem),
		PR:      tableDeriver(stagelabels.PRItem),
		Legacy:  legacy,
		Now:     func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		Metrics: m,
		Shadow:  shadow,
	})
	return r, m, &logs
}

// The reconciler New returns is a ModeSwitch: a flip takes effect on the
// next Reconcile, is logged once per transition and ignores garbage.
func TestModeSwitchHotReload(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	legacy := func(Facts) Decision { return Decision{Stage: stagelabels.Reviewed, Reason: "legacy"} }
	r, m, logs := newSwitchReconciler(t, ModeShadow, w, legacy, nil)
	sw, ok := r.(ModeSwitch)
	if !ok {
		t.Fatal("New must return a ModeSwitch")
	}
	if sw.Mode() != ModeShadow {
		t.Fatalf("mode = %s, want shadow", sw.Mode())
	}
	f := prFacts(1, "pending") // engine: 050; legacy: 060
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got := w.stageLabels(f.Ref); len(got) != 1 || got[0] != stagelabels.Reviewed.Label() {
		t.Fatalf("shadow wrote %v", got)
	}

	if !sw.SetMode(ModeEnforce) || sw.SetMode(ModeEnforce) || sw.SetMode(Mode("bogus")) {
		t.Fatal("SetMode must report only real changes and reject invalid modes")
	}
	if sw.Mode() != ModeEnforce {
		t.Fatalf("mode = %s, want enforce", sw.Mode())
	}
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got := w.stageLabels(f.Ref); len(got) != 1 || got[0] != stagelabels.ReviewPending.Label() {
		t.Fatalf("enforce wrote %v, want the engine's %s", got, stagelabels.ReviewPending.Label())
	}

	sw.SetMode(ModeOff)
	derivations := m.Total(MetricDerivations)
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got := w.stageLabels(f.Ref); len(got) != 1 || got[0] != stagelabels.Reviewed.Label() {
		t.Fatalf("off must pass the legacy decision through, wrote %v", got)
	}
	if m.Total(MetricDerivations) != derivations {
		t.Fatal("off mode must not derive")
	}
	if n := strings.Count(logs.String(), "stage engine: mode changed"); n != 2 {
		t.Fatalf("logged %d mode transitions, want 2 (shadow→enforce, enforce→off):\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "from=shadow to=enforce") {
		t.Fatalf("mode transition must be a WARN naming both modes:\n%s", logs.String())
	}
}

// Off without a comparator stays inert (rollback with nothing to write).
func TestModeOffWithoutLegacyStaysInert(t *testing.T) {
	w := newFakeWriter()
	r, _, _ := newSwitchReconciler(t, ModeOff, w, nil, nil)
	if _, err := r.Reconcile(context.Background(), prFacts(2, "soaking")); err != nil || w.Writes != 0 {
		t.Fatalf("off without legacy: err=%v writes=%d", err, w.Writes)
	}
}

// With a shadow recorder configured the recorder owns the log-once and the
// example ring; the reconciler's counter still ticks per mismatch and its
// own log line stays silent.
func TestShadowRecorderOwnsLogOnce(t *testing.T) {
	ctx := context.Background()
	var recLogs strings.Builder
	rec := shadowdiff.NewRecorder(shadowdiff.WithLogger(slog.New(slog.NewTextHandler(&recLogs, nil))))
	legacy := func(f Facts) Decision {
		if f.Ref.Number == 4 {
			return Decision{Stage: stagelabels.NeedsDecision} // what tableDeriver says for "agree"
		}
		return Decision{Stage: stagelabels.Reviewed, Reason: "legacy"}
	}
	r, m, logs := newSwitchReconciler(t, ModeShadow, newFakeWriter(), legacy, rec)
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, prFacts(3, "pending")); err != nil {
			t.Fatal(err)
		}
	}
	// An agreeing item is recorded too, so the recorder's Agreed count is honest.
	if _, err := r.Reconcile(ctx, prFacts(4, "agree")); err != nil {
		t.Fatal(err)
	}
	if m.Total(MetricShadowMismatch) != 3 {
		t.Fatalf("mismatch counter = %d, want 3", m.Total(MetricShadowMismatch))
	}
	if strings.Contains(logs.String(), "shadow mismatch") {
		t.Fatalf("reconciler must not log its own mismatch line when a recorder is set:\n%s", logs.String())
	}
	if n := strings.Count(recLogs.String(), "legacy and engine disagree"); n != 2 {
		t.Fatalf("recorder logged %d lines, want 2 (one stage bucket, one reason bucket):\n%s", n, recLogs.String())
	}
	snap := rec.Snapshot()
	if snap.Items != 4 || snap.StageMismatches != 3 {
		t.Fatalf("recorder snapshot = items %d stage %d", snap.Items, snap.StageMismatches)
	}

	st := StatusOf(r, m, rec)
	if st.Mode != "shadow" || st.Mismatches != 3 || st.Derivations != 4 || st.Anomalies != 0 {
		t.Fatalf("status = %+v", st)
	}
	if len(st.LastMismatchExamples) != StatusExampleLimit {
		t.Fatalf("examples = %d, want capped at %d", len(st.LastMismatchExamples), StatusExampleLimit)
	}
	if ex := st.LastMismatchExamples[0]; ex.Ref != "acme/widgets#3" || ex.At.IsZero() {
		t.Fatalf("newest example first: %+v", ex)
	}
	kinds := map[string]bool{}
	for _, ex := range st.LastMismatchExamples {
		kinds[ex.Kind] = true
		if ex.Kind == shadowdiff.KindStage && (ex.Legacy != stagelabels.Reviewed.String() || ex.Engine != stagelabels.ReviewPending.String()) {
			t.Fatalf("stage example renders stages: %+v", ex)
		}
	}
	if !kinds[shadowdiff.KindStage] || !kinds[shadowdiff.KindReason] {
		t.Fatalf("examples should cover both kinds: %+v", st.LastMismatchExamples)
	}
}

func TestStatusOfNilInputs(t *testing.T) {
	st := StatusOf(nil, nil, nil)
	if st.Mode != "" || st.LastMismatchExamples == nil || len(st.LastMismatchExamples) != 0 {
		t.Fatalf("nil status = %+v (examples must marshal as [] not null)", st)
	}
}
