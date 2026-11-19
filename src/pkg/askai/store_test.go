package askai

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAllowedSourceURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://docs.hivecommons.dev/settings/#features":                            "https://docs.hivecommons.dev/settings/#features",
		"https://DOCS.hivecommons.dev/a?utm=x":                                       "https://docs.hivecommons.dev/a",
		"https://github.com/hivecommons/hive/blob/v5/src/docs/dashboard-sections.md": "https://github.com/hivecommons/hive/blob/v5/src/docs/dashboard-sections.md",
		"https://github.com/hivecommons/hive/tree/v5/src/docs/design":                "https://github.com/hivecommons/hive/tree/v5/src/docs/design",
	} {
		got, ok := AllowedSourceURL(raw)
		if !ok || got != want {
			t.Errorf("AllowedSourceURL(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{
		"http://docs.hivecommons.dev/a",
		"https://docs.hivecommons.dev.evil.example/a",
		"https://user:pw@docs.hivecommons.dev/a",
		"https://docs.hivecommons.dev:8443/a",
		"https://github.com/hivecommons/hive/blob/v5/src/pkg/hub/ask.go",
		"https://github.com/someone/hive/blob/v5/src/docs/a.md",
		"https://github.com/hivecommons/hive/issues/1",
		"https://github.com/hivecommons/hive/blob/v5/src/docs/../../README.md",
		"javascript:alert(1)",
		"",
	} {
		if got, ok := AllowedSourceURL(raw); ok {
			t.Errorf("AllowedSourceURL(%q) admitted %q", raw, got)
		}
	}
}

func TestFilterSourcesCapsAndSanitizes(t *testing.T) {
	var in []UpstreamSource
	for i := 0; i < MaxSources+3; i++ {
		in = append(in, UpstreamSource{URL: "https://docs.hivecommons.dev/p" + strings.Repeat("x", i), Title: strings.Repeat("t", maxSourceTitleRunes*2)})
	}
	out := FilterSources(in)
	if len(out) != MaxSources {
		t.Fatalf("kept %d sources, want %d", len(out), MaxSources)
	}
	if n := len([]rune(out[0].Title)); n != maxSourceTitleRunes {
		t.Fatalf("title length %d", n)
	}
	if got := FilterSources([]UpstreamSource{{URL: "https://docs.hivecommons.dev/a", Title: " "}}); got[0].Title != "https://docs.hivecommons.dev/a" {
		t.Fatalf("empty title fallback: %+v", got)
	}
	long := SanitizeAnswer(strings.Repeat("a", MaxAnswerRunes+10))
	if !strings.HasSuffix(long, answerTruncatedSuffix) || len([]rune(long)) != MaxAnswerRunes+len([]rune(answerTruncatedSuffix)) {
		t.Fatalf("answer cap: %d runes", len([]rune(long)))
	}
}

func TestSectionVocabularyIsClosed(t *testing.T) {
	if _, ok := SectionLabel("settings-features"); !ok {
		t.Fatal("settings-features must be in the vocabulary")
	}
	for _, id := range []string{"", "acme/private-repo", "<script>", "Settings, Features"} {
		if _, ok := SectionLabel(id); ok {
			t.Fatalf("%q must not be a section id", id)
		}
	}
	for _, id := range SectionIDs() {
		label, _ := SectionLabel(id)
		if label == "" || CredentialShaped(label) {
			t.Fatalf("bad label for %q", id)
		}
	}
}

func TestConsentStoreFileIsPrivateAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "consents")
	store := &ConsentStore{Dir: dir}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if _, err := store.Grant("alice", "old", now); !errors.Is(err, ErrStaleConsentVersion) {
		t.Fatalf("stale version: %v", err)
	}
	c, err := store.Grant("alice", ConsentTextVersion, now)
	if err != nil || !c.Current() || c.Person != "alice" {
		t.Fatalf("grant: %+v %v", c, err)
	}
	info, err := os.Stat(filepath.Join(dir, "alice.json"))
	if err != nil || info.Mode().Perm() != consentFileMode {
		t.Fatalf("file mode: %v %v", info, err)
	}
	if dinfo, err := os.Stat(dir); err != nil || dinfo.Mode().Perm() != consentDirMode {
		t.Fatalf("dir mode: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
	if changed, err := store.Withdraw("alice", now); err != nil || !changed || store.Has("alice") {
		t.Fatalf("withdraw: %v %v", changed, err)
	}
	if changed, _ := store.Withdraw("alice", now); changed {
		t.Fatal("a second withdrawal must be a no-op")
	}
	if changed, err := store.Withdraw("nobody", now); err != nil || changed {
		t.Fatal("withdrawing without a record must be a no-op")
	}
	if err := store.Delete("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alice.json")); !os.IsNotExist(err) {
		t.Fatal("delete left the record")
	}
	for _, bad := range []string{"", "../alice", "a/b", ".hidden", strings.Repeat("a", 200)} {
		if _, err := store.Grant(bad, ConsentTextVersion, now); !errors.Is(err, ErrInvalidPerson) {
			t.Errorf("person %q accepted: %v", bad, err)
		}
	}
}
