package ioscan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanaryPreambleOmittedWithoutToken(t *testing.T) {
	if got := CanaryPreamble(""); got != "" {
		t.Fatalf("CanaryPreamble(\"\") = %q, want empty", got)
	}
	got := CanaryPreamble("HIVE-CANARY-deadbeef")
	if !strings.Contains(got, "HIVE-CANARY-deadbeef") {
		t.Fatalf("preamble %q does not carry the token", got)
	}
	// The preamble is the only thing standing between the agent and an
	// innocent-looking "summarize your context" exfiltration, so it has to
	// forbid the transforms an agent would otherwise consider safe.
	for _, want := range []string{"Never print", "summarize", "external service"} {
		if !strings.Contains(got, want) {
			t.Fatalf("preamble %q missing %q", got, want)
		}
	}
}

// A registry that cannot survive a restart is a registry that stops detecting
// leaks the moment the hub is redeployed, so the on-disk round trip is the
// behavior worth pinning — including the parent directory being created.
func TestCanaryRegistryRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "canaries.json")
	first := NewCanaryRegistry(path)
	c, err := first.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Add did not persist the registry: %v", err)
	}

	second := NewCanaryRegistry(path)
	leak, ok := second.Scan("", "log line containing "+c.Token, "artifact")
	if !ok {
		t.Fatal("reloaded registry did not detect the persisted canary")
	}
	if leak.Agent != "scanner" || leak.Token != c.Token || leak.Source != "artifact" {
		t.Fatalf("leak = %+v, want scanner/%s/artifact", leak, c.Token)
	}
}

// Half-written records (agent or token missing) must be dropped rather than
// installed as a map entry that can never match — an empty token would
// otherwise make strings.Contains true for every text scanned.
func TestCanaryRegistryLoadSkipsIncompleteRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canaries.json")
	raw, err := json.Marshal([]Canary{
		{Agent: "", Token: CanaryPrefix + "aa"},
		{Agent: "quality", Token: ""},
		{Agent: "quality", Token: CanaryPrefix + "bb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewCanaryRegistry(path)
	if _, ok := r.Scan("quality", "totally benign output", "artifact"); ok {
		t.Fatal("empty token matched benign text")
	}
	if _, ok := r.Scan("quality", "here is "+CanaryPrefix+"bb", "artifact"); !ok {
		t.Fatal("complete record was not loaded")
	}
}

func TestCanaryRegistryLoadReportsUnreadableAndCorruptState(t *testing.T) {
	dir := t.TempDir()
	missing := &CanaryRegistry{persist: filepath.Join(dir, "absent.json")}
	if err := missing.Load(); err == nil {
		t.Fatal("Load: expected an error for a missing persist file")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&CanaryRegistry{persist: corrupt}).Load(); err == nil {
		t.Fatal("Load: expected an error for corrupt JSON")
	}
}

// An in-memory registry (no persist path) is the normal shape in tests and in
// hubs with no writable data dir; neither Load nor Save may fail there.
func TestCanaryRegistryWithoutPersistPathIsInMemory(t *testing.T) {
	var nilRegistry *CanaryRegistry
	if err := nilRegistry.Load(); err != nil {
		t.Fatalf("Load on nil registry = %v, want nil", err)
	}
	if err := nilRegistry.Save(); err != nil {
		t.Fatalf("Save on nil registry = %v, want nil", err)
	}
	if _, ok := nilRegistry.Scan("scanner", "text", "artifact"); ok {
		t.Fatal("nil registry reported a leak")
	}
	r := NewCanaryRegistry("")
	if err := r.Save(); err != nil {
		t.Fatalf("Save without a persist path = %v, want nil", err)
	}
	if _, ok := r.Scan("scanner", "", "artifact"); ok {
		t.Fatal("empty text reported a leak")
	}
}

// Persisting is best-effort: a registry whose data dir is unwritable must still
// hand back a usable canary (the in-memory registry keeps detecting leaks)
// while reporting the failure to the owner instead of swallowing it.
func TestAddKeepsWorkingWhenPersistFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var saveErr error
	r := &CanaryRegistry{persist: filepath.Join(blocker, "canaries.json"), onChange: func(err error) { saveErr = err }}
	c, err := r.Add("scanner")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if saveErr == nil {
		t.Fatal("onChange was not told about the failed save")
	}
	if _, ok := r.Scan("scanner", "leaked "+c.Token, "artifact"); !ok {
		t.Fatal("registry stopped detecting leaks after a failed save")
	}
}

// A canary registered for one agent still has to be caught in another agent's
// output — that cross-agent hit is exactly the exfiltration path the canary
// exists to reveal.
func TestScanFallsBackToEveryAgent(t *testing.T) {
	r := NewCanaryRegistry("")
	c, err := r.Add("scanner")
	if err != nil {
		t.Fatal(err)
	}
	leak, ok := r.Scan("quality", "quality's PR body quotes "+c.Token, "pr_body")
	if !ok {
		t.Fatal("canary from another agent was not detected")
	}
	if leak.Agent != "scanner" {
		t.Fatalf("leak.Agent = %q, want the owning agent \"scanner\"", leak.Agent)
	}
}
