package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/settingsparity"
)

func TestSettingsDialogReferencesEveryHiveYAMLLeaf(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join("..", "..", ".."))
	leaves, err := settingsparity.ConfigLeaves(repoRoot)
	if err != nil {
		t.Fatalf("enumerate config leaves: %v", err)
	}
	allow, err := settingsparity.Allowlist(filepath.Join("settings_parity_allowlist.txt"))
	if err != nil {
		t.Fatalf("read settings parity allowlist: %v", err)
	}
	corpus := dashboardSettingsCorpus(t)
	leafSet := map[string]bool{}
	var missing []string
	for _, leaf := range leaves {
		leafSet[leaf.Path] = true
		if settingsParityReferenced(corpus, leaf.Path) || allow[leaf.Path] != "" {
			continue
		}
		missing = append(missing, leaf.Path)
	}
	if len(missing) > 0 {
		t.Fatalf("hive.yaml settings are not referenced by the Settings UI and are not allowlisted:\n%s", strings.Join(missing, "\n"))
	}
	for path := range allow {
		if !leafSet[path] {
			t.Fatalf("settings parity allowlist contains %q, but that is not a current Config leaf", path)
		}
		if settingsParityReferenced(corpus, path) {
			t.Fatalf("settings parity allowlist still contains %q, but the Settings UI now references it; remove the allowlist entry", path)
		}
	}
}

func dashboardSettingsCorpus(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	paths := []string{filepath.Join("static", "index.html")}
	if err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func settingsParityReferenced(corpus, path string) bool {
	if strings.Contains(corpus, path) {
		return true
	}
	return false
}
