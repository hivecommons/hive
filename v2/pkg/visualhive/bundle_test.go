package visualhive

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestValidateAndImportBundle(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeTestBundle(t, root, false)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3, AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	store, err := beads.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := bundle.Import(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := bundle.Import(store)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || second.Skipped != 1 || store.Count() != 1 {
		t.Fatalf("import was not idempotent: first=%+v second=%+v count=%d", first, second, store.Count())
	}
}

func TestValidateBundleRejectsTampering(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeTestBundle(t, root, false)
	beadsPath := filepath.Join(root, "files", ".visual-hive", "hive", "beads.json")
	if err := os.WriteFile(beadsPath, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBundle(manifestPath, ValidationOptions{MaxACMM: 3, AllowLocal: true}); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}
}

func TestValidateBundleRejectsUntrustedByDefault(t *testing.T) {
	manifestPath := writeTestBundle(t, t.TempDir(), false)
	if _, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 3}); err == nil || !strings.Contains(err.Error(), "trusted") {
		t.Fatalf("expected provenance rejection, got %v", err)
	}
}

func writeTestBundle(t *testing.T, root string, trusted bool) string {
	t.Helper()
	relative := "files/.visual-hive/hive/beads.json"
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	projection := []Projection{{ID: "vh-1", Title: "Fix contrast", Type: "bug", Status: "open", Priority: 1, Actor: "quality", ExternalRef: "visual-hive:demo:1", Metadata: map[string]string{}, Notes: "deterministic failure", CreatedAt: "2026-07-09T11:00:00Z", UpdatedAt: "2026-07-09T11:00:00Z", DependsOn: []string{}}}
	data, _ := json.Marshal(projection)
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fileDigest := digest(data)
	entries := []string{fmt.Sprintf("%s\x00%s\x00%d", relative, fileDigest, len(data))}
	sort.Strings(entries)
	overall := digest([]byte(strings.Join(entries, "\n")))
	manifest := Manifest{
		SchemaVersion: ManifestSchema, BundleID: "test-bundle", GeneratedAt: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 7, 10, 11, 0, 0, 0, time.UTC),
		Producer: Producer{Name: "visual-hive", Version: "0.2.0", GitCommit: "abc123"}, Source: Source{Repository: "owner/repo", Ref: "refs/heads/main", CommitSHA: "abc123", Event: "local", Conclusion: "local", Trusted: trusted},
		Project: "demo", Mode: "measured", Verdict: "ready", ACMMRequest: 3,
		Files: []File{{Path: relative, SourcePath: ".visual-hive/hive/beads.json", SHA256: fileDigest, Size: int64(len(data)), MediaType: "application/json"}}, OverallDigest: overall,
		Provenance: Provenance{Kind: "local", SubjectDigest: overall}, Safety: Safety{AtomicWrite: true, PathsAreRelative: true, DigestsRequired: true, ProducerCountersAreAdvisory: true},
	}
	manifestData, _ := json.Marshal(manifest)
	manifestPath := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifestPath, manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}
