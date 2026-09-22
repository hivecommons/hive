package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The L5/L6 packs moved `reviewer` from an on-demand, PR-triggered definition
// to the cadenced queue design (#8023). ApplyPack reconciled kick_template to
// the new template but not on_demand, so a hive that had applied the older
// pack kept `on_demand: true` forever: the new template was in place and the
// governor never kicked it. on_demand is a pack-behavior field and must follow
// the pack — unless the operator set it (#7446), which owns it from then on.

func TestApplyPackTakesReviewerOffOnDemandWhenPackSaysSo(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	// The stuck shape: a reviewer created by the earlier pack definition.
	srv.deps.Config.Agents["reviewer"] = config.AgentConfig{
		ID: "reviewer", Role: "reviewer", Backend: "claude", Enabled: true,
		KickTemplate: "reviewer-advisory.md", OnDemand: true,
	}

	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	got := srv.deps.Config.Agents["reviewer"]
	if got.OnDemand {
		t.Fatal("reviewer is still on_demand after the L5 pack (on_demand: false) was applied")
	}
	if got.OnDemandOwner != config.FieldOwnerPack {
		t.Errorf("OnDemandOwner = %q, want %q", got.OnDemandOwner, config.FieldOwnerPack)
	}
	if got.KickTemplate != "reviewer-queue.md" {
		t.Errorf("KickTemplate = %q, want reviewer-queue.md", got.KickTemplate)
	}
}

func TestApplyPackKeepsOperatorOnDemandChoice(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	srv.deps.Config.Agents["reviewer"] = config.AgentConfig{
		ID: "reviewer", Role: "reviewer", Backend: "claude", Enabled: true,
		OnDemand: true, OnDemandOwner: config.FieldOwnerOperator,
	}

	// Every restart re-applies the pack; the operator's choice must survive.
	for i := 0; i < 3; i++ {
		if _, err := srv.ApplyPack(5); err != nil {
			t.Fatalf("ApplyPack(5) restart %d: %v", i, err)
		}
		got := srv.deps.Config.Agents["reviewer"]
		if !got.OnDemand || got.OnDemandOwner != config.FieldOwnerOperator {
			t.Fatalf("restart %d: operator on_demand=true was reverted by the pack: %+v", i, got)
		}
	}
}
