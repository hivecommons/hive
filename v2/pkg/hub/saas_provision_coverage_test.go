package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// saas_provision.go pure-function and file-IO coverage tests
//
// Targets the 0%/50% functions that are testable without a live HubServer:
//   - forgeHostKey (0% → 100%)
//   - forgesForCluster (0% → 100%)
//   - clusterDefaultForge (0% → 100%)
//   - effectiveGitHubBaseURL (50% → 100%)
//   - effectiveGitHubAPIURL (50% → 100%)
//   - spokeAppAuthFailing (0% → 100%)
//   - generateHiveID (0% → validated)
//   - loadSaaSHive / saveSaaSHive / listSaaSHives / countUserHives (file IO)
// ============================================================

// --- forgeHostKey ---

func TestForgeHostKey(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", publicForgeHost},
		{"github.com", publicForgeHost},
		{"https://github.com", publicForgeHost},
		{"https://github.com/", publicForgeHost},
		{"api.github.com", publicForgeHost},
		{"https://api.github.com", publicForgeHost},
		{"https://github.ibm.com", "github.ibm.com"},
		{"https://github.ibm.com/api/v3", "github.ibm.com"},
		{"https://github.ibm.com/api/v3/", "github.ibm.com"},
		{"https://github.cisco.com/", "github.cisco.com"},
	}
	for _, c := range cases {
		got := forgeHostKey(c.input)
		if got != c.want {
			t.Errorf("forgeHostKey(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// --- forgesForCluster ---

func TestForgesForCluster_Nil(t *testing.T) {
	out := forgesForCluster(nil)
	if len(out) != 0 {
		t.Errorf("expected empty map for nil cluster, got %+v", out)
	}
}

func TestForgesForCluster_LegacyFlat(t *testing.T) {
	c := &ClusterConfig{
		GitHubAppID:   12345,
		GitHubAppSlug: "my-app",
		GitHubBaseURL: "https://github.ibm.com",
	}
	out := forgesForCluster(c)
	id, ok := out["github.ibm.com"]
	if !ok {
		t.Fatalf("expected forge entry for github.ibm.com, got keys: %v", keys(out))
	}
	if id.AppID != 12345 || id.AppSlug != "my-app" {
		t.Errorf("forge identity = %+v", id)
	}
}

func TestForgesForCluster_ExplicitOverridesLegacy(t *testing.T) {
	c := &ClusterConfig{
		GitHubAppID:   111,
		GitHubAppSlug: "legacy",
		GitHubBaseURL: "https://github.ibm.com",
		Forges: map[string]ClusterForgeIdentity{
			"github.ibm.com": {AppID: 222, AppSlug: "explicit"},
		},
	}
	out := forgesForCluster(c)
	if out["github.ibm.com"].AppID != 222 {
		t.Errorf("explicit forge entry should override legacy, got %+v", out["github.ibm.com"])
	}
}

func TestForgesForCluster_NoAppIDOrSlug(t *testing.T) {
	c := &ClusterConfig{
		GitHubBaseURL: "https://github.ibm.com",
	}
	out := forgesForCluster(c)
	// No AppID and no slug → no legacy entry synthesised
	if len(out) != 0 {
		t.Errorf("expected empty map when no app_id/slug, got %+v", out)
	}
}

// --- clusterDefaultForge ---

func TestClusterDefaultForge_AllBranches(t *testing.T) {
	cases := []struct {
		name    string
		cluster *ClusterConfig
		want    string
	}{
		{"nil cluster", nil, publicForgeHost},
		{"explicit DefaultForge", &ClusterConfig{DefaultForge: "github.ibm.com"}, "github.ibm.com"},
		{"from GitHubBaseURL", &ClusterConfig{GitHubBaseURL: "https://github.cisco.com"}, "github.cisco.com"},
		{"from GitHubAPIURL", &ClusterConfig{GitHubAPIURL: "https://github.ibm.com/api/v3"}, "github.ibm.com"},
		{"empty everything", &ClusterConfig{}, publicForgeHost},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clusterDefaultForge(c.cluster)
			if got != c.want {
				t.Errorf("clusterDefaultForge = %q, want %q", got, c.want)
			}
		})
	}
}

// --- effectiveGitHubBaseURL ---

func TestEffectiveGitHubBaseURL(t *testing.T) {
	cluster := &ClusterConfig{GitHubBaseURL: "https://github.ibm.com"}
	cases := []struct {
		name string
		hive *SaaSHive
		want string
	}{
		{"empty inherits cluster", &SaaSHive{}, "https://github.ibm.com"},
		{"public sentinel returns empty", &SaaSHive{GitHubBaseURL: "public"}, ""},
		{"explicit github.com returns empty", &SaaSHive{GitHubBaseURL: "https://github.com"}, ""},
		{"explicit GHE wins", &SaaSHive{GitHubBaseURL: "https://github.cisco.com"}, "https://github.cisco.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveGitHubBaseURL(c.hive, cluster)
			if got != c.want {
				t.Errorf("effectiveGitHubBaseURL = %q, want %q", got, c.want)
			}
		})
	}
}

// --- effectiveGitHubAPIURL ---

func TestEffectiveGitHubAPIURL(t *testing.T) {
	cluster := &ClusterConfig{GitHubAPIURL: "https://github.ibm.com/api/v3"}
	cases := []struct {
		name string
		hive *SaaSHive
		want string
	}{
		{"empty + no base inherits cluster", &SaaSHive{}, "https://github.ibm.com/api/v3"},
		{"empty + public base returns empty", &SaaSHive{GitHubBaseURL: "public"}, ""},
		{"empty + github.com base returns empty", &SaaSHive{GitHubBaseURL: "https://github.com"}, ""},
		{"public sentinel returns empty", &SaaSHive{GitHubAPIURL: "public"}, ""},
		{"api.github.com returns empty", &SaaSHive{GitHubAPIURL: "https://api.github.com"}, ""},
		{"explicit GHE wins", &SaaSHive{GitHubAPIURL: "https://github.cisco.com/api/v3"}, "https://github.cisco.com/api/v3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectiveGitHubAPIURL(c.hive, cluster)
			if got != c.want {
				t.Errorf("effectiveGitHubAPIURL = %q, want %q", got, c.want)
			}
		})
	}
}

// --- spokeAppAuthFailing ---

func TestSpokeAppAuthFailing_AllStates(t *testing.T) {
	cases := []struct {
		name    string
		payload *HeartbeatPayload
		want    bool
	}{
		{"nil payload", nil, false},
		{"not required", &HeartbeatPayload{GitHubAppRequired: false}, false},
		{"required but healthy", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "ok"}, false},
		{"required empty state", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: ""}, false},
		{"not-installed", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "not-installed"}, true},
		{"wrong-installation", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "wrong-installation"}, true},
		{"key-invalid", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "key-invalid"}, true},
		{"key-missing", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "key-missing"}, true},
		{"whitespace trimmed", &HeartbeatPayload{GitHubAppRequired: true, GitHubAppState: "  not-installed  "}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := spokeAppAuthFailing(c.payload)
			if got != c.want {
				t.Errorf("spokeAppAuthFailing = %v, want %v", got, c.want)
			}
		})
	}
}

// --- generateHiveID ---

func TestGenerateHiveID_Format(t *testing.T) {
	id := generateHiveID("MyOrg", "my-awesome-repository")
	// Repo truncated to 12 chars: "my-awesome-r", then sanitized (unchanged).
	if !strings.HasPrefix(id, "hosted-myorg-my-awesome-r-") {
		t.Errorf("unexpected format: %q", id)
	}
	if !strings.HasPrefix(id, "hosted-") {
		t.Errorf("must start with 'hosted-', got %q", id)
	}
}

func TestGenerateHiveID_ShortRepo(t *testing.T) {
	id := generateHiveID("org", "repo")
	if !strings.HasPrefix(id, "hosted-org-repo-") {
		t.Errorf("short repo should not be truncated: %q", id)
	}
}

func TestGenerateHiveID_Uniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateHiveID("org", "repo")
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
}

// --- loadSaaSHive / saveSaaSHive / listSaaSHives / countUserHives ---

func TestLoadSaveSaaSHive(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	h := &SaaSHive{
		ID:    "test-hive-1",
		Owner: "alice",
		Org:   "my-org",
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}

	loaded := loadSaaSHive("test-hive-1")
	if loaded == nil {
		t.Fatal("loadSaaSHive returned nil")
	}
	if loaded.Owner != "alice" || loaded.Org != "my-org" {
		t.Errorf("loaded = %+v", loaded)
	}
}

func TestLoadSaaSHive_PathTraversal(t *testing.T) {
	if h := loadSaaSHive("../etc/passwd"); h != nil {
		t.Error("path traversal should return nil")
	}
	if h := loadSaaSHive("foo/bar"); h != nil {
		t.Error("slash in ID should return nil")
	}
	if h := loadSaaSHive("foo\\bar"); h != nil {
		t.Error("backslash in ID should return nil")
	}
}

func TestLoadSaaSHive_BadJSON(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	hiveDir := filepath.Join(dir, "bad-hive")
	os.MkdirAll(hiveDir, 0o755)
	os.WriteFile(filepath.Join(hiveDir, "meta.json"), []byte("{not json"), 0o644)

	if h := loadSaaSHive("bad-hive"); h != nil {
		t.Error("bad JSON should return nil")
	}
}

func TestSaveSaaSHive_PathTraversal(t *testing.T) {
	h := &SaaSHive{ID: "../escape"}
	if err := saveSaaSHive(h); err == nil {
		t.Error("path traversal in save should error")
	}
}

func TestListSaaSHives_WithInvalid(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	// Create two valid hives and one invalid
	for _, id := range []string{"hive-a", "hive-b"} {
		h := &SaaSHive{ID: id, Owner: "bob"}
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
	}
	// Invalid entry (not a dir)
	os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("x"), 0o644)

	hives := listSaaSHives()
	if len(hives) != 2 {
		t.Errorf("expected 2 hives, got %d", len(hives))
	}
}

func TestCountUserHives_MultiOwner(t *testing.T) {
	dir := t.TempDir()
	oldDir := saasHivesDir
	saasHivesDir = dir
	defer func() { saasHivesDir = oldDir }()

	for i, owner := range []string{"alice", "alice", "bob"} {
		h := &SaaSHive{ID: "h" + string(rune('0'+i)), Owner: owner}
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
	}

	if got := countUserHives("alice"); got != 2 {
		t.Errorf("countUserHives(alice) = %d, want 2", got)
	}
	if got := countUserHives("bob"); got != 1 {
		t.Errorf("countUserHives(bob) = %d, want 1", got)
	}
	if got := countUserHives("nobody"); got != 0 {
		t.Errorf("countUserHives(nobody) = %d, want 0", got)
	}
}

// --- resolveProvisionAppID ---

func TestResolveProvisionAppID_NilCluster(t *testing.T) {
	got := resolveProvisionAppID("12345", &SaaSHive{}, nil)
	if got != "12345" {
		t.Errorf("nil cluster should pass through reqAppID, got %q", got)
	}
}

func TestResolveProvisionAppID_ZeroAppID(t *testing.T) {
	got := resolveProvisionAppID("99999", &SaaSHive{}, &ClusterConfig{GitHubAppID: 0})
	if got != "99999" {
		t.Errorf("zero AppID cluster should pass through reqAppID, got %q", got)
	}
}

func TestResolveProvisionAppID_ClusterAppUsed(t *testing.T) {
	cluster := &ClusterConfig{
		GitHubAppID:   77777,
		GitHubAppSlug: "hive-ghe",
		GitHubBaseURL: "https://github.ibm.com",
	}
	got := resolveProvisionAppID("12345", &SaaSHive{}, cluster)
	if got != "77777" {
		t.Errorf("expected cluster app_id, got %q", got)
	}
}

func TestResolveProvisionAppID_SanitizesInput(t *testing.T) {
	got := resolveProvisionAppID("12<script>345", &SaaSHive{}, nil)
	if strings.Contains(got, "<") {
		t.Errorf("should sanitize HTML, got %q", got)
	}
}

// --- backfillGitHubHostFromCluster ---

func TestBackfillGitHubHostFromCluster_NilInputs(t *testing.T) {
	if got := backfillGitHubHostFromCluster(nil, nil); got != "" {
		t.Errorf("nil hive should return empty, got %q", got)
	}
	if got := backfillGitHubHostFromCluster(&SaaSHive{}, nil); got != "" {
		t.Errorf("nil cluster should return empty, got %q", got)
	}
}

func TestBackfillGitHubHostFromCluster_AlreadyHasHost(t *testing.T) {
	h := &SaaSHive{GitHubHost: "github.ibm.com"}
	c := &ClusterConfig{GitHubBaseURL: "https://github.cisco.com"}
	if got := backfillGitHubHostFromCluster(h, c); got != "" {
		t.Errorf("hive with host should not be backfilled, got %q", got)
	}
}

func TestBackfillGitHubHostFromCluster_ExplicitPublicBaseURL(t *testing.T) {
	h := &SaaSHive{GitHubBaseURL: "public"}
	c := &ClusterConfig{GitHubBaseURL: "https://github.ibm.com"}
	if got := backfillGitHubHostFromCluster(h, c); got != "" {
		t.Errorf("explicit public pin should not be backfilled, got %q", got)
	}
}

// --- helpers ---

func keys(m map[string]ClusterForgeIdentity) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Verify SaaSHive has the GitHubBaseURL/GitHubAPIURL fields we use.
func TestSaaSHiveJSONRoundtrip(t *testing.T) {
	h := SaaSHive{
		ID:            "test",
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
	}
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var h2 SaaSHive
	if err := json.Unmarshal(data, &h2); err != nil {
		t.Fatal(err)
	}
	if h2.GitHubBaseURL != h.GitHubBaseURL || h2.GitHubAPIURL != h.GitHubAPIURL {
		t.Errorf("roundtrip mismatch: %+v", h2)
	}
}
