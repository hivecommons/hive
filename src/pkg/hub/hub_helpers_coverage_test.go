package hub

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for partially covered pkg/hub helpers:
//   - defaultHostForKind (forge.go)
//   - generationPinSeverity.String (hub_generations_retire.go)
//   - readClustersFile (clusters_registry.go)
//   - clampQuadrantSpend (server.go)

func TestDefaultHostForKind(t *testing.T) {
	cases := []struct {
		kind ForgeKind
		want string
	}{
		{ForgeGitLab, "gitlab.com"},
		{ForgeGitea, "codeberg.org"},
		{ForgeGitHub, "github.com"},
		{ForgeKind("unknown"), "github.com"},
	}
	for _, tc := range cases {
		if got := defaultHostForKind(tc.kind); got != tc.want {
			t.Errorf("defaultHostForKind(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestGenerationPinSeverityString(t *testing.T) {
	cases := []struct {
		sev  generationPinSeverity
		want string
	}{
		{pinSeverityNone, "none"},
		{pinSeverityWarn, "warn"},
		{pinSeverityStranded, "stranded"},
		{generationPinSeverity(99), "none"},
	}
	for _, tc := range cases {
		if got := tc.sev.String(); got != tc.want {
			t.Errorf("severity %d String() = %q, want %q", tc.sev, got, tc.want)
		}
	}
}

func TestReadClustersFile_Loaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.json")
	if err := os.WriteFile(path, []byte(`{"local":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, outcome := readClustersFile(path, quietLogger())
	if outcome != clustersLoaded {
		t.Fatalf("outcome = %v, want clustersLoaded", outcome)
	}
	if string(data) != `{"local":{}}` {
		t.Errorf("data = %q, want file contents", data)
	}
}

func TestReadClustersFile_Absent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	data, outcome := readClustersFile(path, quietLogger())
	if outcome != clustersAbsent {
		t.Fatalf("outcome = %v, want clustersAbsent", outcome)
	}
	if data != nil {
		t.Errorf("data = %q, want nil for absent file", data)
	}
}

func TestReadClustersFile_UntrustedAfterRetries(t *testing.T) {
	// A directory path makes os.ReadFile fail with a non-ENOENT error
	// (EISDIR) on every attempt, exercising the retry loop and the
	// clustersUntrusted terminal outcome.
	dir := t.TempDir()
	data, outcome := readClustersFile(dir, quietLogger())
	if outcome != clustersUntrusted {
		t.Fatalf("outcome = %v, want clustersUntrusted", outcome)
	}
	if data != nil {
		t.Errorf("data = %q, want nil for unreadable registry", data)
	}
}

func TestClampQuadrantSpend(t *testing.T) {
	if got := clampQuadrantSpend(nil); got != nil {
		t.Errorf("clampQuadrantSpend(nil) = %v, want nil", got)
	}

	neg := int64(-1)
	if got := clampQuadrantSpend(&neg); got != nil {
		t.Errorf("clampQuadrantSpend(-1) = %v, want nil (not reported)", got)
	}

	zero := int64(0)
	if got := clampQuadrantSpend(&zero); got == nil || *got != 0 {
		t.Errorf("clampQuadrantSpend(0) = %v, want 0", got)
	}

	normal := int64(123456)
	if got := clampQuadrantSpend(&normal); got == nil || *got != 123456 {
		t.Errorf("clampQuadrantSpend(123456) = %v, want 123456", got)
	}

	over := maxQuadrantSpend + 1
	if got := clampQuadrantSpend(&over); got == nil || *got != maxQuadrantSpend {
		t.Errorf("clampQuadrantSpend(max+1) = %v, want clamped to %d", got, maxQuadrantSpend)
	}
}
