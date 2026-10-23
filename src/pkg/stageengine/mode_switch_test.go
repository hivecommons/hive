package stageengine

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func newSwitchReconciler(t *testing.T, mode Mode, w StageWriter) (Reconciler, *Counters, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	m := NewCounters()
	r := New(Options{
		Mode:    mode,
		Store:   stagelabels.NewStore("", nil),
		Writer:  w,
		Issue:   tableDeriver(stagelabels.IssueItem),
		PR:      tableDeriver(stagelabels.PRItem),
		Now:     func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		Metrics: m,
	})
	return r, m, &logs
}

// The reconciler New returns is a ModeSwitch: a flip takes effect on the
// next Reconcile, is logged once per transition and ignores garbage.
func TestModeSwitchHotReload(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, m, logs := newSwitchReconciler(t, ModeShadow, w)
	sw, ok := r.(ModeSwitch)
	if !ok {
		t.Fatal("New must return a ModeSwitch")
	}
	if sw.Mode() != ModeShadow {
		t.Fatalf("mode = %s, want shadow", sw.Mode())
	}
	f := prFacts(1, "pending") // engine: 050
	if _, err := r.Reconcile(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got := w.stageLabels(f.Ref); len(got) != 0 || w.Writes != 0 {
		t.Fatalf("shadow wrote %v", got)
	}
	if m.Total(MetricDerivations) != 1 {
		t.Fatal("shadow must still count the derivation")
	}

	if !sw.SetMode(ModeEnforce) || sw.SetMode(ModeEnforce) || sw.SetMode(Mode("bogus")) || sw.SetMode(Mode("off")) {
		t.Fatal("SetMode must report only real changes and reject invalid modes (off included)")
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

	// Back to shadow: the label already on the item is left alone.
	sw.SetMode(ModeShadow)
	if _, err := r.Reconcile(ctx, prFacts(1, "soaking")); err != nil {
		t.Fatal(err)
	}
	if got := w.stageLabels(f.Ref); len(got) != 1 || got[0] != stagelabels.ReviewPending.Label() || w.Writes != 1 {
		t.Fatalf("shadow after enforce must not write, labels %v writes %d", got, w.Writes)
	}
	if n := strings.Count(logs.String(), "stage engine: mode changed"); n != 2 {
		t.Fatalf("logged %d mode transitions, want 2 (shadow→enforce, enforce→shadow):\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "from=shadow to=enforce") {
		t.Fatalf("mode transition must be a WARN naming both modes:\n%s", logs.String())
	}
}

func TestStatusOfCounts(t *testing.T) {
	ctx := context.Background()
	r, m, _ := newSwitchReconciler(t, ModeEnforce, newFakeWriter())
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(ctx, prFacts(3, "pending")); err != nil {
			t.Fatal(err)
		}
	}
	st := StatusOf(r, m)
	if st.Mode != "enforce" || st.Derivations != 3 || st.Anomalies != 0 || st.Healed != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusOfNilInputs(t *testing.T) {
	if st := StatusOf(nil, nil); st.Mode != "" {
		t.Fatalf("nil status = %+v", st)
	}
}
