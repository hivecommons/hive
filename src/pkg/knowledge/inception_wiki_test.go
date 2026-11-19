package knowledge

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func writeInceptionWikiFixture(t *testing.T, dir, wikiName string) string {
	t.Helper()
	vault := filepath.Join(dir, inceptionWikiDir)
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	fact := filepath.Join(vault, "vision.md")
	if err := os.WriteFile(fact, []byte("---\ntype: vision\n---\n# Vision\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if wikiName != "" {
		state, _ := json.Marshal(InceptionState{WikiName: wikiName})
		if err := os.MkdirAll(filepath.Join(dir, "inception"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, inceptionStateFile), state, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return fact
}

func vaultNames(api *KnowledgeAPI) map[string]bool {
	out := map[string]bool{}
	for _, v := range api.Vaults() {
		out[v.Name] = true
	}
	return out
}

// Past inception facts must stay served (and untouched) after the inception
// engine was retired.
func TestReconnectInceptionWikiKeepsPastFacts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range []struct{ saved, want string }{
		{"", inceptionWikiDefaultName},
		{"my-custom-wiki", "my-custom-wiki"},
	} {
		dir := t.TempDir()
		fact := writeInceptionWikiFixture(t, dir, c.saved)
		api := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)
		ReconnectInceptionWiki(dir, api, logger)
		if !vaultNames(api)[c.want] {
			t.Fatalf("vault %q not connected; vaults = %v", c.want, api.Vaults())
		}
		// A second call hits the already-connected branch quietly.
		ReconnectInceptionWiki(dir, api, nil)
		if _, err := os.Stat(fact); err != nil {
			t.Fatalf("past inception fact must stay on disk: %v", err)
		}
	}
}

func TestReconnectInceptionWikiNoops(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ReconnectInceptionWiki(t.TempDir(), nil, logger)

	missing := t.TempDir()
	api := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)
	ReconnectInceptionWiki(missing, api, logger)
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, inceptionWikiDir), 0o755); err != nil {
		t.Fatal(err)
	}
	ReconnectInceptionWiki(empty, api, logger)
	if vaultNames(api)[inceptionWikiDefaultName] {
		t.Fatal("no files: nothing must be connected")
	}
	if got := inceptionWikiName(missing); got != inceptionWikiDefaultName {
		t.Fatalf("inceptionWikiName with no state = %q", got)
	}
}
