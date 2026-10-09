package knowledge

import (
	"context"
	"strings"
	"testing"
)

func TestScheduledPromotionOnPromotedHook(t *testing.T) {
	tests := []struct {
		name       string
		confidence float64
		setHook    bool
		wantCalls  int
		wantCount  int
	}{
		{"promoted fires hook", 0.95, true, 1, 1},
		{"nothing promoted skips hook", 0.10, true, 0, 0},
		{"cleared hook is not called", 0.95, false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := newPromoteProbe("hook-fact", tt.confidence, "verified")
			defer probe.close()
			cfg := CuratorConfig{Enabled: boolPtr(true), Schedule: "daily", AutoPromoteThreshold: 0.9}
			s := NewPromotionScheduler(probe.promoter(cfg), cfg, schedTestLogger())
			calls, count := 0, 0
			s.OnPromoted(func(n int) { calls++; count = n })
			if !tt.setHook {
				s.OnPromoted(nil)
			}
			s.RunOnce(context.Background())
			if calls != tt.wantCalls || count != tt.wantCount {
				t.Fatalf("hook calls=%d count=%d, want %d/%d", calls, count, tt.wantCalls, tt.wantCount)
			}
		})
	}
}

func TestPromoteSourceCarriesPrefix(t *testing.T) {
	probe := newPromoteProbe("prov-fact", 0.95, "verified")
	defer probe.close()
	cfg := CuratorConfig{Enabled: boolPtr(true), AutoPromoteThreshold: 0.9}
	res := probe.promoter(cfg).Promote(context.Background(), PromoteRequest{
		Slug: "prov-fact", FromLayer: LayerProject, ToLayer: LayerOrg, Reason: "r", Promoter: "curator",
	})
	if !res.Success {
		t.Fatalf("promote failed: %s", res.Error)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if got := probe.ingests[0][0].SourcePR; !strings.HasPrefix(got, PromotedSourcePrefix) {
		t.Fatalf("SourcePR = %q, want prefix %q", got, PromotedSourcePrefix)
	}
}
