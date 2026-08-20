package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRegistryEntryForgeHostJSONKey pins the wire spelling the dashboard reads.
// RegistryEntry marshals the hive's forge host as "githubHost"; the spoke table
// used to read only h.github_host (the PROVISION-REQUEST spelling), which is
// undefined on a hive row, so every repo link and tooltip claimed github.com
// even for a hive whose default repo lives on github.ibm.com.
func TestRegistryEntryForgeHostJSONKey(t *testing.T) {
	b, err := json.Marshal(RegistryEntry{ID: "h1", GitHubHost: "github.ibm.com"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"githubHost":"github.ibm.com"`) {
		t.Fatalf("registry entry forge host key changed: %s", b)
	}
}

// TestSpokeTableUsesHiveForgeHost asserts the dashboard resolves a hive row's
// forge through hiveForgeHost (which accepts both spellings) rather than the
// bare h.github_host, and that the repo-link tooltip names that same forge.
func TestSpokeTableUsesHiveForgeHost(t *testing.T) {
	if !strings.Contains(dashboardHTML, "function hiveForgeHost(h)") {
		t.Fatal("hiveForgeHost is gone; hive rows must resolve their own forge host")
	}
	if !strings.Contains(dashboardHTML, "h.githubHost || h.github_host") {
		t.Error("hiveForgeHost no longer reads the registry's githubHost spelling")
	}
	if strings.Contains(dashboardHTML, "ghRepoURL(h.github_host") {
		t.Error("a repo link still reads h.github_host directly; hive rows spell it githubHost")
	}
	if strings.Contains(dashboardHTML, "escAttr((h.github_host || 'github.com'))") {
		t.Error("the repo-link tooltip still falls back to github.com from an undefined field")
	}
	if !strings.Contains(dashboardHTML, "Open repository on ' + escAttr((hiveForgeHost(h) || 'github.com'))") {
		t.Error("the repo-link tooltip must name the hive's resolved forge host")
	}
}
