package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// #8023 added `reviewer` to the L5/L6 packs as `mode: ADVISORY` +
// `converse: true` — the first pack agent whose roster entry carries
// `converse` at all. These pin the two halves of the contract: the pack SEEDS
// converse on an agent that has no value for it, and it never overrides one
// that does. ApplyPack runs on every restart, so a replace-on-diff here would
// re-grant a revoked opt-in on every pod roll, the #7503/#5632 family of bug.

func TestApplyPackSeedsConverseOnCreatedReviewer(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	delete(srv.deps.Config.Agents, "reviewer")

	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	got, ok := srv.deps.Config.Agents["reviewer"]
	if !ok {
		t.Fatal("L5 pack did not create `reviewer`")
	}
	if got.Converse == nil || !*got.Converse {
		t.Errorf("created reviewer Converse = %v, want true", got.Converse)
	}
	if got.Mode != "ADVISORY" {
		t.Errorf("created reviewer Mode = %q, want ADVISORY", got.Mode)
	}
}

func TestApplyPackSeedsConverseOnPreexistingReviewer(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	// The hive that created `reviewer` by hand before it joined the roster:
	// it predates `converse`, so the field is unset and the pack may fill it.
	srv.deps.Config.Agents["reviewer"] = config.AgentConfig{
		ID: "reviewer", Role: "reviewer", Backend: "claude", Enabled: true,
		PauseOwner: config.FieldOwnerOperator,
	}

	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	got := srv.deps.Config.Agents["reviewer"]
	if got.Converse == nil || !*got.Converse {
		t.Errorf("pre-existing reviewer Converse = %v, want the pack's true", got.Converse)
	}
}

func TestApplyPackDoesNotReGrantRevokedConverse(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	revoked := false
	srv.deps.Config.Agents["reviewer"] = config.AgentConfig{
		ID: "reviewer", Role: "reviewer", Backend: "claude", Enabled: true,
		Converse: &revoked,
	}

	// Every restart re-applies the pack; the revocation must survive all of them.
	for i := 0; i < 4; i++ {
		if _, err := srv.ApplyPack(5); err != nil {
			t.Fatalf("ApplyPack(5) restart %d: %v", i, err)
		}
		got := srv.deps.Config.Agents["reviewer"]
		if got.Converse == nil || *got.Converse {
			t.Fatalf("restart %d: operator `converse: false` was re-granted by the pack: got %v", i, got.Converse)
		}
	}
}
