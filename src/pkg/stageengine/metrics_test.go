package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestCountersSnapshot(t *testing.T) {
	c := NewCounters()
	c.Derivation(stagelabels.PRItem, stagelabels.MergeQueue)
	c.Derivation(stagelabels.PRItem, stagelabels.MergeQueue)
	c.Derivation(stagelabels.IssueItem, stagelabels.None)
	c.ShadowMismatch(stagelabels.PRItem, stagelabels.Reviewed, stagelabels.ReviewPending)
	c.TransitionAnomaly(stagelabels.IssueItem, stagelabels.PRInProgress, stagelabels.Actionable)
	c.WriterWrite(stagelabels.PRItem, OpAdd, 2)
	c.WriterWrite(stagelabels.PRItem, OpRemove, 0)
	c.DuplicateHealed(stagelabels.PRItem)

	snap := c.Snapshot()
	if len(snap) != 6 {
		t.Fatalf("%d samples: %+v", len(snap), snap)
	}
	if snap[0].Name != MetricDerivations || snap[0].Labels["kind"] != "issue" || snap[0].Labels["stage"] != "none" || snap[0].Value != 1 {
		t.Fatalf("first sample %+v", snap[0])
	}
	if snap[1].Labels["stage"] != "merge-queue" || snap[1].Value != 2 {
		t.Fatalf("second sample %+v", snap[1])
	}
	if c.Total(MetricWriterWrites) != 2 || c.Total(MetricDuplicateHealed) != 1 || c.Total(MetricShadowMismatch) != 1 || c.Total(MetricAnomaly) != 1 {
		t.Fatalf("totals %+v", snap)
	}
	for _, s := range snap {
		if s.Name == MetricWriterWrites && s.Labels["op"] == OpRemove {
			t.Fatal("a zero write must not create a series")
		}
	}
	def := DefaultCounters()
	if def == nil || def != DefaultCounters() {
		t.Fatal("DefaultCounters must be a stable singleton")
	}
}
