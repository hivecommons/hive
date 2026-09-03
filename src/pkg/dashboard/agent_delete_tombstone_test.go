package dashboard

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// bluefinLevel is the ACMM level of the hive that produced the report. Its
// level-3 pack defines both agents the operator deleted.
const bluefinLevel = 3

// TestApplyPackDoesNotResurrectDeletedAgent is the regression test for the
// reported bug: "I deleted brainstorm and guide agents and they always come
// back". ApplyPack runs on every hive restart, and it re-created any pack
// agent missing from the roster — so a deletion never survived a restart.
func TestApplyPackDoesNotResurrectDeletedAgent(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.ApplyPack(bluefinLevel); err != nil {
		t.Fatalf("ApplyPack(%d): %v", bluefinLevel, err)
	}
	for _, name := range []string{"brainstorm", "guide"} {
		if _, ok := srv.deps.Config.Agents[name]; !ok {
			t.Fatalf("precondition: L%d pack should define %q", bluefinLevel, name)
		}
	}

	// Operator deletes both, exactly as the audit log on the live hive shows.
	for _, name := range []string{"guide", "brainstorm"} {
		rec := doDelete(srv, "/api/config/governor/agents/"+name, nil)
		if rec.Code != 200 {
			t.Fatalf("delete %s: status %d", name, rec.Code)
		}
	}

	// Restart. The pack is re-applied ("merging pack updates").
	result, err := srv.ApplyPack(bluefinLevel)
	if err != nil {
		t.Fatalf("ApplyPack(%d) re-apply: %v", bluefinLevel, err)
	}

	for _, name := range []string{"brainstorm", "guide"} {
		if _, ok := srv.deps.Config.Agents[name]; ok {
			t.Errorf("deleted agent %q was re-created by ApplyPack", name)
		}
	}
	if len(result.Tombstoned) != 2 {
		t.Errorf("ApplyPack should report both deleted agents as tombstoned, got %v", result.Tombstoned)
	}

	// The user reported deleting them repeatedly. Four more restarts.
	for i := 0; i < 4; i++ {
		if _, err := srv.ApplyPack(bluefinLevel); err != nil {
			t.Fatalf("ApplyPack restart %d: %v", i, err)
		}
	}
	for _, name := range []string{"brainstorm", "guide"} {
		if _, ok := srv.deps.Config.Agents[name]; ok {
			t.Errorf("deleted agent %q came back after repeated restarts", name)
		}
	}
}

// TestMergeAgentOverridesSkipsTombstoned covers the FASTER of the two
// resurrection paths, and the one that matches the user's "a minute or so
// later": a leftover /data/agent-configs/<name>.yaml is re-merged by every
// config reload, because MergeAgentOverrides only ever adds.
func TestMergeAgentOverridesSkipsTombstoned(t *testing.T) {
	cfg := &config.Config{
		Agents:        map[string]config.AgentConfig{},
		RemovedAgents: []string{"guide"},
	}
	cfg.MergeAgentOverrides(map[string]config.AgentConfig{
		"guide":   {Model: "claude-sonnet-4-6"},
		"scanner": {Model: "claude-sonnet-4-6"},
	})
	if _, ok := cfg.Agents["guide"]; ok {
		t.Error("tombstoned agent was re-merged from its overlay file")
	}
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Error("a non-deleted overlay agent must still merge")
	}
}

// TestMergeAgentOverridesPrunesSeedAgent covers the OTHER source: the
// ConfigMap seed, which the dashboard cannot rewrite. Skipping the overlay is
// not enough on its own — a tombstoned agent already present in the base map
// must be evicted too.
func TestMergeAgentOverridesPrunesSeedAgent(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"guide": {Model: "from-seed"},
		},
		RemovedAgents: []string{"guide"},
	}
	cfg.MergeAgentOverrides(nil)
	if _, ok := cfg.Agents["guide"]; ok {
		t.Error("tombstoned agent survived from the ConfigMap seed")
	}
}

// TestTombstoneSurvivesACMMLevelChange pins the design decision: a tombstone is
// scoped to the agent NAME and is NOT cleared by a level change. An operator
// who deletes `guide` at L3 does not get it back by moving to L5.
func TestTombstoneSurvivesACMMLevelChange(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.ApplyPack(bluefinLevel); err != nil {
		t.Fatalf("ApplyPack(%d): %v", bluefinLevel, err)
	}
	if rec := doDelete(srv, "/api/config/governor/agents/guide", nil); rec.Code != 200 {
		t.Fatalf("delete guide: status %d", rec.Code)
	}

	const higherLevel = 5
	if _, err := srv.ApplyPack(higherLevel); err != nil {
		t.Fatalf("ApplyPack(%d): %v", higherLevel, err)
	}
	if _, ok := srv.deps.Config.Agents["guide"]; ok {
		t.Error("a deleted agent must not return on an ACMM level increase")
	}
	// A genuinely NEW agent the higher pack introduces — one never deleted
	// here — must still be added. Without this the tombstone would be
	// indistinguishable from a broken level upgrade.
	pack, err := config.ACMMPackByLevel(higherLevel)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(%d): %v", higherLevel, err)
	}
	added := 0
	for _, pa := range pack.Agents {
		if pa.Name == "guide" {
			continue
		}
		if _, ok := srv.deps.Config.Agents[pa.Name]; ok {
			added++
		}
	}
	if added == 0 {
		t.Error("the level increase added no agents at all — tombstone over-applied")
	}
}

// TestExplicitReAddLiftsTombstone: adding the agent back from the Governor grid
// is the one unambiguous signal that the operator changed their mind.
func TestExplicitReAddLiftsTombstone(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.ApplyPack(bluefinLevel); err != nil {
		t.Fatalf("ApplyPack(%d): %v", bluefinLevel, err)
	}
	if rec := doDelete(srv, "/api/config/governor/agents/guide", nil); rec.Code != 200 {
		t.Fatalf("delete guide: status %d", rec.Code)
	}
	if !srv.deps.Config.IsAgentRemoved("guide") {
		t.Fatal("precondition: guide should be tombstoned")
	}

	rec := doPost(srv, "/api/config/governor/agents", map[string]string{
		"name":    "guide",
		"backend": "copilot",
	})
	if rec.Code != 200 {
		t.Fatalf("re-add guide: status %d body %s", rec.Code, rec.Body.String())
	}
	if srv.deps.Config.IsAgentRemoved("guide") {
		t.Error("an explicit re-add must lift the tombstone")
	}

	// And the re-added agent must now survive a pack apply rather than being
	// pruned straight back out.
	if _, err := srv.ApplyPack(bluefinLevel); err != nil {
		t.Fatalf("ApplyPack after re-add: %v", err)
	}
	if _, ok := srv.deps.Config.Agents["guide"]; !ok {
		t.Error("re-added agent was pruned again")
	}
}

// TestDeleteResponseNamesThePack covers legibility. Silently undoing the
// operator's action a minute later is what turned this into a bug report; the
// delete response now says up front that the agent belongs to an ACMM pack and
// that the deletion has been recorded.
func TestDeleteResponseNamesThePack(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.ApplyPack(bluefinLevel); err != nil {
		t.Fatalf("ApplyPack(%d): %v", bluefinLevel, err)
	}

	rec := doDelete(srv, "/api/config/governor/agents/guide", nil)
	if rec.Code != 200 {
		t.Fatalf("delete guide: status %d", rec.Code)
	}
	var resp struct {
		OK         bool   `json:"ok"`
		Tombstoned bool   `json:"tombstoned"`
		Note       string `json:"note"`
		PackLevels []int  `json:"packLevels"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	if !resp.OK || !resp.Tombstoned {
		t.Errorf("delete response must report ok+tombstoned, got %+v", resp)
	}
	if resp.Note == "" {
		t.Error("delete response must explain what happens next")
	}
	if len(resp.PackLevels) == 0 {
		t.Error("guide is defined by ACMM packs; the response must say which levels")
	}
}

// TestRepeatDeleteIsIdempotent: a second delete of the same name must not
// append a duplicate tombstone.
func TestRepeatDeleteIsIdempotent(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{}}
	if !cfg.MarkAgentRemoved("guide") {
		t.Fatal("first mark should report a new tombstone")
	}
	if cfg.MarkAgentRemoved("guide") {
		t.Error("second mark should be a no-op")
	}
	if len(cfg.RemovedAgents) != 1 {
		t.Errorf("expected 1 tombstone, got %v", cfg.RemovedAgents)
	}
	if !cfg.ClearAgentRemoved("guide") {
		t.Error("clear should report a lifted tombstone")
	}
	if cfg.ClearAgentRemoved("guide") {
		t.Error("clearing an absent tombstone should be a no-op")
	}
}
