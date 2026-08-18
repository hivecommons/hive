package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testAppID and testOtherAppID are arbitrary non-zero GitHub App IDs. Only
// their distinctness matters to these tests.
const (
	testAppID      = int64(123456)
	testOtherAppID = int64(999888)
)

// withTempClustersConfig redirects clustersConfigPath at a temp file seeded
// with the given cluster configs, so setClusterAppID never touches the real
// /data PVC.
func withTempClustersConfig(t *testing.T, configs []ClusterConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clusters.json")
	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture clusters: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture clusters: %v", err)
	}
	orig := clustersConfigPath
	clustersConfigPath = path
	t.Cleanup(func() { clustersConfigPath = orig })
	return path
}

// readClustersFixture reads the on-disk clusters.json back as raw configs.
func readClustersFixture(t *testing.T, path string) []ClusterConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read clusters config: %v", err)
	}
	var got []ClusterConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse clusters config: %v (data=%q)", err, data)
	}
	return got
}

// TestSetClusterAppIDPersists is the core contract: the App ID must be written
// to BOTH the in-memory map (so the running hub uses it immediately) and
// clusters.json (so it survives a restart).
func TestSetClusterAppIDPersists(t *testing.T) {
	path := withTempClustersConfig(t, []ClusterConfig{
		{ID: "pok-prod", Name: "pok"},
		{ID: "vllm-d", Name: "vllm"},
	})
	s := &HubServer{clusters: map[string]ClusterConfig{
		"pok-prod": {ID: "pok-prod", Name: "pok"},
		"vllm-d":   {ID: "vllm-d", Name: "vllm"},
	}}

	if err := s.setClusterAppID("pok-prod", testAppID); err != nil {
		t.Fatalf("setClusterAppID: %v", err)
	}

	if got := s.clusters["pok-prod"].GitHubAppID; got != testAppID {
		t.Errorf("in-memory GitHubAppID = %d, want %d", got, testAppID)
	}

	onDisk := readClustersFixture(t, path)
	var found bool
	for _, c := range onDisk {
		if c.ID == "pok-prod" {
			found = true
			if c.GitHubAppID != testAppID {
				t.Errorf("on-disk GitHubAppID = %d, want %d", c.GitHubAppID, testAppID)
			}
		}
	}
	if !found {
		t.Error("pok-prod is missing from the rewritten clusters.json")
	}
}

// TestSetClusterAppIDLeavesOtherClustersUntouched guards the documented
// data-loss rule: the rewrite reads the file fresh and edits one entry, so
// every other cluster — including one the in-memory map does not know about,
// which loadClusters would have silently dropped — must survive verbatim.
func TestSetClusterAppIDLeavesOtherClustersUntouched(t *testing.T) {
	path := withTempClustersConfig(t, []ClusterConfig{
		{ID: "pok-prod", Name: "pok"},
		{ID: "vllm-d", Name: "vllm", GitHubAppID: testOtherAppID},
		// Present on disk but absent from the in-memory map, standing in for an
		// entry loadClusters rejected. Re-serializing the map would delete it.
		{ID: "dropped-by-validation", Name: "ghost"},
	})
	s := &HubServer{clusters: map[string]ClusterConfig{
		"pok-prod": {ID: "pok-prod", Name: "pok"},
		"vllm-d":   {ID: "vllm-d", Name: "vllm", GitHubAppID: testOtherAppID},
	}}

	if err := s.setClusterAppID("pok-prod", testAppID); err != nil {
		t.Fatalf("setClusterAppID: %v", err)
	}

	onDisk := readClustersFixture(t, path)
	byID := make(map[string]ClusterConfig, len(onDisk))
	for _, c := range onDisk {
		byID[c.ID] = c
	}
	if len(onDisk) != 3 {
		t.Fatalf("clusters.json now has %d entries, want 3 (ids=%v)", len(onDisk), keysOf(byID))
	}
	if _, ok := byID["dropped-by-validation"]; !ok {
		t.Error("an entry absent from the in-memory map was deleted from clusters.json")
	}
	if got := byID["vllm-d"].GitHubAppID; got != testOtherAppID {
		t.Errorf("vllm-d GitHubAppID = %d, want %d (an unrelated cluster was modified)", got, testOtherAppID)
	}
	if got := byID["vllm-d"].Name; got != "vllm" {
		t.Errorf("vllm-d Name = %q, want %q", got, "vllm")
	}
}

// keysOf returns map keys for readable failure messages.
func keysOf(m map[string]ClusterConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSetClusterAppIDUnknownCluster checks an ID the hub does not run is
// refused before any file is touched.
func TestSetClusterAppIDUnknownCluster(t *testing.T) {
	path := withTempClustersConfig(t, []ClusterConfig{{ID: "pok-prod"}})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	s := &HubServer{clusters: map[string]ClusterConfig{"pok-prod": {ID: "pok-prod"}}}
	err = s.setClusterAppID("no-such-cluster", testAppID)

	if err == nil {
		t.Fatal("setClusterAppID() = nil, want an error for an unknown cluster")
	}
	if !strings.Contains(err.Error(), "unknown cluster") {
		t.Errorf("error = %q, want it to report an unknown cluster", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Error("clusters.json was rewritten despite the cluster being unknown")
	}
}

// TestSetClusterAppIDMissingFromConfigFile covers the in-memory/on-disk skew
// case: the hub knows the cluster but clusters.json does not list it, so the
// write cannot be persisted and must be reported rather than silently lost.
func TestSetClusterAppIDMissingFromConfigFile(t *testing.T) {
	withTempClustersConfig(t, []ClusterConfig{{ID: "some-other-cluster"}})
	s := &HubServer{clusters: map[string]ClusterConfig{"pok-prod": {ID: "pok-prod"}}}

	err := s.setClusterAppID("pok-prod", testAppID)

	if err == nil {
		t.Fatal("setClusterAppID() = nil, want an error when the cluster is absent from clusters.json")
	}
	if !strings.Contains(err.Error(), "not present") {
		t.Errorf("error = %q, want it to report the cluster is not present in the config file", err)
	}
}

// TestSetClusterAppIDUnreadableConfig checks a missing clusters.json surfaces
// as an error rather than a panic or a silent no-op.
func TestSetClusterAppIDUnreadableConfig(t *testing.T) {
	orig := clustersConfigPath
	clustersConfigPath = filepath.Join(t.TempDir(), "does-not-exist.json")
	t.Cleanup(func() { clustersConfigPath = orig })

	s := &HubServer{clusters: map[string]ClusterConfig{"pok-prod": {ID: "pok-prod"}}}
	err := s.setClusterAppID("pok-prod", testAppID)

	if err == nil {
		t.Fatal("setClusterAppID() = nil, want an error when clusters.json cannot be read")
	}
	if !strings.Contains(err.Error(), "read clusters config") {
		t.Errorf("error = %q, want it to report the read failure", err)
	}
}

// TestSetClusterAppIDMalformedConfig checks unparseable JSON is reported
// instead of being overwritten — clobbering a hand-edited clusters.json the
// hub merely failed to parse would strand every cluster.
func TestSetClusterAppIDMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.json")
	const malformed = `{not valid json`
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	orig := clustersConfigPath
	clustersConfigPath = path
	t.Cleanup(func() { clustersConfigPath = orig })

	s := &HubServer{clusters: map[string]ClusterConfig{"pok-prod": {ID: "pok-prod"}}}
	err := s.setClusterAppID("pok-prod", testAppID)

	if err == nil {
		t.Fatal("setClusterAppID() = nil, want an error for malformed clusters.json")
	}
	if !strings.Contains(err.Error(), "parse clusters config") {
		t.Errorf("error = %q, want it to report the parse failure", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != malformed {
		t.Error("an unparseable clusters.json was overwritten, want it left intact")
	}
}
