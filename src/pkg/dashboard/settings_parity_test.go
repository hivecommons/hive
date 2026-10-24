package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
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
	controlCorpus := settingHelpMarkRE.ReplaceAllString(corpus, "")
	leafSet := map[string]bool{}
	var missing []string
	for _, leaf := range leaves {
		leafSet[leaf.Path] = true
		if settingsParityReferenced(corpus, controlCorpus, leaf.Path) || allow[leaf.Path] != "" {
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
		if settingsParityReferenced(corpus, controlCorpus, path) {
			t.Fatalf("settings parity allowlist still contains %q, but the Settings UI now references it; remove the allowlist entry", path)
		}
	}
}

func dashboardSettingsCorpus(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	paths := []string{filepath.Join("static", "index.html")}
	if err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		// This file names control-only leaves literally; it is not UI coverage.
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".go") && filepath.Base(path) != "settings_parity_test.go" {
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

// settingHelpMarkRE matches a settingHelpMark(...) call so its help text can be
// ignored for keys that need an editable control rather than a prose mention.
var settingHelpMarkRE = regexp.MustCompile("settingHelpMark\\((?:'[^']*'|\"[^\"]*\"),\\s*(?:'[^']*'|\"[^\"]*\"),\\s*(?:'(?:[^'\\\\]|\\\\.)*'|\"[^\"]*\"),\\s*`[^`]*`\\)")

// controlOnlyLeaves are keys whose mention in help text must not count as UI
// coverage; only a reference outside settingHelpMark(...) does.
var controlOnlyLeaves = map[string]bool{
	"data.nous_dir": true,
}

func settingsParityReferenced(corpus, controlCorpus, path string) bool {
	if controlOnlyLeaves[path] {
		return containsLeaf(controlCorpus, path)
	}
	return containsLeaf(corpus, path)
}

// containsLeaf reports whether path occurs in corpus as a whole dotted leaf:
// a match preceded by '.' or an identifier character is a longer key that
// merely ends in path (ioscan.classifier.backend vs classifier.backend) and
// does not count as a reference.
func containsLeaf(corpus, path string) bool {
	for start := 0; ; {
		i := strings.Index(corpus[start:], path)
		if i < 0 {
			return false
		}
		i += start
		if i == 0 || !isLeafChar(corpus[i-1]) {
			return true
		}
		start = i + 1
	}
}

func isLeafChar(c byte) bool {
	return c == '.' || c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
