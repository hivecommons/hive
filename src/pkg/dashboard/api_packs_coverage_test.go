package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestHandlePacksListCurrentFlag verifies the list handler marks exactly one
// pack as current (the one matching the configured ACMM level).
func TestHandlePacksListCurrentFlag(t *testing.T) {
	srv := newFullServer(t) // level 2

	req := httptest.NewRequest("GET", "/api/packs", nil)
	w := httptest.NewRecorder()
	srv.handlePacksList(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var packs []struct {
		Level   int  `json:"level"`
		Current bool `json:"current"`
	}
	if err := json.NewDecoder(w.Body).Decode(&packs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(packs) == 0 {
		t.Fatal("expected at least one pack")
	}

	var currentCount int
	for _, p := range packs {
		if p.Current {
			currentCount++
			if p.Level != 2 {
				t.Errorf("current pack level = %d, want 2", p.Level)
			}
		}
	}
	if currentCount != 1 {
		t.Errorf("exactly 1 pack should be current, got %d", currentCount)
	}
}

// TestHandlePacksListIncludesAgents verifies the response includes the agents
// array and agent count for each pack.
func TestHandlePacksListIncludesAgents(t *testing.T) {
	srv := newFullServer(t)

	req := httptest.NewRequest("GET", "/api/packs", nil)
	w := httptest.NewRecorder()
	srv.handlePacksList(w, req)

	var packs []struct {
		Name       string `json:"name"`
		AgentCount int    `json:"agentCount"`
		Agents     []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&packs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, p := range packs {
		if p.AgentCount != len(p.Agents) {
			t.Errorf("pack %q: agentCount=%d but len(agents)=%d", p.Name, p.AgentCount, len(p.Agents))
		}
	}
}

// TestHandlePackApplyResponseBody validates the response structure from a
// successful pack apply.
func TestHandlePackApplyResponseBody(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	req := httptest.NewRequest("POST", "/api/packs/1/apply", nil)
	req.SetPathValue("level", "1")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["ok"] != true {
		t.Error("response missing ok=true")
	}
	if resp["status"] != "applied" {
		t.Errorf("status = %v, want applied", resp["status"])
	}
	if _, ok := resp["created"]; !ok {
		t.Error("response missing created field")
	}
	if _, ok := resp["tombstoned"]; !ok {
		t.Error("response missing tombstoned field")
	}
}

// TestHandlePackApplyInvalidLevelZero verifies that level 0 is rejected as
// invalid (packs are 1-indexed).
func TestHandlePackApplyInvalidLevelZero(t *testing.T) {
	srv := newFullServer(t)

	req := httptest.NewRequest("POST", "/api/packs/0/apply", nil)
	req.SetPathValue("level", "0")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code == 200 {
		t.Errorf("expected error for level 0, got 200")
	}
}

// TestApplyPackTombstonedAgents verifies that a deliberately removed agent is
// NOT re-created by ApplyPack, and is reported in the Tombstoned slice.
func TestApplyPackTombstonedAgents(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	// Mark scanner as removed so the pack will skip it.
	srv.deps.Config.RemovedAgents = append(srv.deps.Config.RemovedAgents, "scanner")
	// Remove from live agents so it would need to be re-created.
	delete(srv.deps.Config.Agents, "scanner")

	pack, err := config.ACMMPackByLevel(2)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(2): %v", err)
	}
	hasScannerInPack := false
	for _, a := range pack.Agents {
		if a.Name == "scanner" {
			hasScannerInPack = true
		}
	}
	if !hasScannerInPack {
		t.Skip("scanner not in L2 pack, cannot test tombstone")
	}

	result, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	found := false
	for _, name := range result.Tombstoned {
		if name == "scanner" {
			found = true
		}
	}
	if !found {
		t.Errorf("scanner not in tombstoned list: %v", result.Tombstoned)
	}
	if _, ok := srv.deps.Config.Agents["scanner"]; ok {
		t.Error("tombstoned scanner was re-created in config")
	}
}

// TestApplyPackPreservesOperatorModelOnReapply verifies that a model set by the
// operator (ModelOwner != pack) is not overwritten on re-apply of the same level.
func TestApplyPackPreservesOperatorModelOnReapply(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Model = "operator-chosen-model"
	scanner.ModelOwner = "operator"
	srv.deps.Config.Agents["scanner"] = scanner

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	got := srv.deps.Config.Agents["scanner"]
	if got.Model != "operator-chosen-model" {
		t.Errorf("operator model overwritten: got %q, want %q", got.Model, "operator-chosen-model")
	}
}

// TestApplyPackFillsEmptyBackend verifies that ApplyPack fills a missing
// backend from the pack definition but does not overwrite an existing one.
func TestApplyPackFillsEmptyBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	// Clear scanner backend so the pack can fill it.
	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = ""
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner

	pack, err := config.ACMMPackByLevel(2)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(2): %v", err)
	}
	var packBackend string
	for _, a := range pack.Agents {
		if a.Name == "scanner" {
			packBackend = a.Backend
		}
	}

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	got := srv.deps.Config.Agents["scanner"]
	if packBackend != "" && got.Backend != packBackend {
		t.Errorf("empty backend not filled: got %q, want %q", got.Backend, packBackend)
	}
}

// TestApplyPackDoesNotOverwriteExistingBackend verifies that an existing
// backend is preserved even when the pack defines a different one.
func TestApplyPackDoesNotOverwriteExistingBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = "user-chosen-backend"
	srv.deps.Config.Agents["scanner"] = scanner

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	got := srv.deps.Config.Agents["scanner"]
	if got.Backend != "user-chosen-backend" {
		t.Errorf("existing backend overwritten: got %q, want %q", got.Backend, "user-chosen-backend")
	}
}

// TestHandlePackSetLevelZero verifies that level 0 is rejected.
func TestHandlePackSetLevelZero(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":0}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for level 0", w.Code)
	}
}

// TestHandlePackSetLevelNegative verifies that a negative level is rejected.
func TestHandlePackSetLevelNegative(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":-1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for negative level", w.Code)
	}
}

// TestDetectACMMLevelDefault verifies the fallback when no level is configured.
func TestDetectACMMLevelDefault(t *testing.T) {
	got := detectACMMLevel(&config.Config{})
	if got != 1 {
		t.Errorf("detectACMMLevel(nil ACMMLevel) = %d, want 1", got)
	}
}

// TestDetectACMMLevelExplicit verifies that a set level is returned as-is.
func TestDetectACMMLevelExplicit(t *testing.T) {
	level := 4
	got := detectACMMLevel(&config.Config{ACMMLevel: &level})
	if got != 4 {
		t.Errorf("detectACMMLevel(4) = %d, want 4", got)
	}
}

// TestApplyPackNoAgentsDir verifies that ApplyPack fails cleanly when
// agents_dir is not configured.
func TestApplyPackNoAgentsDir(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = ""
	// Remove existing agents so the pack tries to create new ones.
	srv.deps.Config.Agents = map[string]config.AgentConfig{}

	_, err := srv.ApplyPack(2)
	if err == nil {
		t.Error("expected error when agents_dir is empty, got nil")
	}
	if !strings.Contains(err.Error(), "agents_dir") {
		t.Errorf("error = %q, want mention of agents_dir", err)
	}
}
