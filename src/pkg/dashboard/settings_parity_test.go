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
		if !precededByPath(corpus, i) && !followedByPath(corpus, i+len(path)) {
			return true
		}
		start = i + 1
	}
}

// precededByPath reports whether the byte before i continues a dotted
// settings path into a match starting at i: a letter, digit, underscore,
// or dot means the match is embedded in (e.g. a suffix of) a longer path,
// not standing alone.
func precededByPath(corpus string, i int) bool {
	if i <= 0 {
		return false
	}
	b := corpus[i-1]
	return b == '.' || pathByte(b)
}

// followedByPath reports whether the byte(s) at i continue a dotted
// settings path out of a match ending at i. A letter, digit, or
// underscore always does. A dot does too, but only when another such
// byte follows it -- i.e. it leads into a further path segment. A dot
// with nothing path-shaped after it (end of sentence, end of string, a
// quote, a closing tag, ...) is punctuation, not a longer path, and does
// not disqualify the match: "...documented at dashboard.stage_tracker.
// webhook_refresh_replay_window_s." still references that leaf even
// though the mention ends the sentence.
func followedByPath(corpus string, i int) bool {
	if i >= len(corpus) {
		return false
	}
	if corpus[i] == '.' {
		return i+1 < len(corpus) && pathByte(corpus[i+1])
	}
	return pathByte(corpus[i])
}

// pathByte reports whether b can appear inside a single dotted-path
// segment (as opposed to '.', the segment separator).
func pathByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func TestSettingsParityReferenced(t *testing.T) {
	tests := []struct {
		name   string
		corpus string
		path   string
		want   bool
	}{
		{
			name:   "exact standalone match",
			corpus: `data-setting-key="ioscan.enabled"`,
			path:   "ioscan.enabled",
			want:   true,
		},
		{
			name:   "no occurrence",
			corpus: `data-setting-key="ioscan.enabled"`,
			path:   "retro.enabled",
			want:   false,
		},
		{
			name:   "suffix of a longer, unrelated path does not count",
			corpus: `data-setting-key="ioscan.classifier.backend"`,
			path:   "classifier.backend",
			want:   false,
		},
		{
			name:   "prefix of a longer, unrelated path does not count",
			corpus: `data-setting-key="classifier.backend_mode"`,
			path:   "classifier.backend",
			want:   false,
		},
		{
			name:   "embedded match plus a genuine standalone match elsewhere still counts",
			corpus: `data-setting-key="ioscan.classifier.backend" ... data-setting-key="classifier.backend"`,
			path:   "classifier.backend",
			want:   true,
		},
		{
			name:   "match bordered only by punctuation counts",
			corpus: `<code>classifier.backend</code>`,
			path:   "classifier.backend",
			want:   true,
		},
		{
			name:   "a sentence-ending period after the match does not disqualify it",
			corpus: `// webhookPatchReplayWindow is dashboard.stage_tracker.webhook_refresh_replay_window_s.`,
			path:   "dashboard.stage_tracker.webhook_refresh_replay_window_s",
			want:   true,
		},
		{
			name:   "a period into a further path segment still disqualifies it",
			corpus: `data-setting-key="classifier.backend.model"`,
			path:   "classifier.backend",
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := settingsParityReferenced(tc.corpus, "", tc.path); got != tc.want {
				t.Fatalf("settingsParityReferenced(%q, \"\", %q) = %v, want %v", tc.corpus, tc.path, got, tc.want)
			}
		})
	}
}
