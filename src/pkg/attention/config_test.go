package attention

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/findingidentity"
)

func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{
		{FlapWindow: -time.Second},
		{Admitter: map[string]string{"repo": "owner"}},
		{Admitter: map[string]string{"o/r": ""}},
		{Admitter: map[string]string{"o/r": "one", "O/R": "two"}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted ambiguous config: %+v", cfg)
		}
	}
	if err := testConfig.Validate(); err != nil {
		t.Fatal(err)
	}
	if (Config{}).Window() != 12*time.Hour || (Config{FlapWindow: 6 * time.Hour}).Window() != 6*time.Hour {
		t.Fatal("flap defaults")
	}
	if testConfig.Owner("ORG/REPO") != "owner" || testConfig.Owner("repo") != "" {
		t.Fatal("owner must match full repo")
	}
}

func TestFindingIdentityUsesExistingKey(t *testing.T) {
	r := findingidentity.Record{SubjectDigest: "subject", Predicate: "security", Location: "pkg/file.go:12"}
	want := findingidentity.Key(r)
	if FindingSubject(r) != want {
		t.Fatal("finding identity changed")
	}
	r.Location = "pkg/file.go:25"
	if FindingSubject(r) != want {
		t.Fatal("new head line number split episode")
	}
	r.Predicate = "urgent-new-risk"
	if FindingSubject(r) == want {
		t.Fatal("new risk collapsed")
	}
}

func TestUnknownVersionDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte(`{"version":2,"items":{},"episodes":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("future ledger version accepted")
	}
}
