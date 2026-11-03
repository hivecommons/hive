package hub

import (
	"strings"
	"testing"
)

func TestEvidenceAuthorKindPooling(t *testing.T) {
	v := buildEvidenceView([]RegistryEntry{
		{ID: "old", Evidence: &HeartbeatEvidence{Merged90d: 100}},
		{ID: "a", Evidence: &HeartbeatEvidence{ByAuthorKind: map[string]EvidenceBucket{
			"agent": {N: 6, Bad: 1}, "human": {N: 2},
		}}},
		{ID: "b", Evidence: &HeartbeatEvidence{ByAuthorKind: map[string]EvidenceBucket{
			"agent": {N: 4, Bad: 1}, "human": {N: 1}, "bot": {},
		}}},
	})
	a := v.RevertRateByAuthorKind["agent"]
	if a.N != 10 || a.Rate == nil || *a.Rate != 0.2 || a.Lo == nil || a.Hi == nil {
		t.Fatalf("pooled agent rate = %+v", a)
	}
	if h := v.RevertRateByAuthorKind["human"]; h.N != 3 || h.Rate != nil || h.Lo != nil {
		t.Fatalf("small sample = %+v", h)
	}
	if b, ok := v.RevertRateByAuthorKind["bot"]; !ok || b.N != 0 || b.Rate != nil {
		t.Fatalf("measured empty bot sample = %+v, present=%v", b, ok)
	}
	old := buildEvidenceView([]RegistryEntry{{Evidence: &HeartbeatEvidence{Merged90d: 100}}})
	if len(old.RevertRateByAuthorKind) != 0 {
		t.Fatalf("old spoke fabricated author counts: %+v", old.RevertRateByAuthorKind)
	}
}

func TestSanitizeEvidenceAuthorKinds(t *testing.T) {
	in := &HeartbeatEvidence{Merged90d: 12, ByAuthorKind: map[string]EvidenceBucket{
		"human": {N: -1, Bad: 3}, "agent": {N: 10, Bad: 99},
		"bot": {N: maxEvidenceCount + 1, Bad: -1}, "untrusted": {N: 10, Bad: 10},
	}}
	got := sanitizeEvidence(in)
	if len(got.ByAuthorKind) != 3 || got.ByAuthorKind["human"] != (EvidenceBucket{}) ||
		got.ByAuthorKind["agent"] != (EvidenceBucket{N: 10, Bad: 10}) ||
		got.ByAuthorKind["bot"] != (EvidenceBucket{N: 2}) {
		t.Fatalf("unbounded author counts: %+v", got.ByAuthorKind)
	}
	got.ByAuthorKind["agent"] = EvidenceBucket{}
	if in.ByAuthorKind["agent"].N != 10 {
		t.Fatal("sanitizer aliased payload")
	}
	if sanitizeEvidence(&HeartbeatEvidence{}).ByAuthorKind != nil {
		t.Fatal("old spoke must retain absent author-kind data")
	}
}

// TestEvidenceAuthorKindPerHiveCounts: per-hive counts carry the author-kind
// buckets so a filtered selection can re-pool them; old spokes carry none.
func TestEvidenceAuthorKindPerHiveCounts(t *testing.T) {
	v := buildEvidenceView([]RegistryEntry{
		{ID: "old", Evidence: &HeartbeatEvidence{Merged90d: 5}},
		{ID: "new", Evidence: &HeartbeatEvidence{Merged90d: 12, ByAuthorKind: map[string]EvidenceBucket{"agent": {N: 12, Bad: 1}}}},
	})
	for _, h := range v.Hives {
		switch h.ID {
		case "old":
			if h.Counts == nil || h.Counts.ByAuthorKind != nil {
				t.Errorf("old spoke fabricated author-kind counts: %+v", h.Counts)
			}
		case "new":
			if h.Counts == nil || h.Counts.ByAuthorKind["agent"] != (EvidenceBucket{N: 12, Bad: 1}) {
				t.Errorf("per-hive author-kind counts missing: %+v", h.Counts)
			}
		}
	}
	for _, s := range evidenceWaiting(v) {
		if strings.Contains(s, "Revert rate by author kind") {
			t.Errorf("waiting still lists author-kind reverts although a hive reports them: %q", s)
		}
	}
	old := buildEvidenceView([]RegistryEntry{{ID: "old", Evidence: &HeartbeatEvidence{Merged90d: 5}}})
	if ws := strings.Join(evidenceWaiting(old), "\n"); !strings.Contains(ws, "Revert rate by author kind — hives report this once upgraded") {
		t.Errorf("old-only selection must wait on author-kind reverts: %q", ws)
	}
}
