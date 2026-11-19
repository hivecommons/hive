package knowledge

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	// inceptionWikiDir is where past inception runs wrote their ideation
	// facts (vision, constitution, requirements, ...) as Markdown files,
	// relative to the data dir.
	inceptionWikiDir = "inception-wiki"
	// inceptionWikiDefaultName is the vault name those runs registered.
	inceptionWikiDefaultName = "inception-wiki"
	// inceptionStateFile carried an optional custom vault name (WikiName).
	inceptionStateFile = "inception/state.json"
)

// ReconnectInceptionWiki keeps the knowledge facts produced by past
// inception runs available after the inception engine was retired in favour
// of Inception (spek) sessions. Those facts live as Markdown files under
// <dataDir>/inception-wiki; the engine used to reconnect that directory as a
// knowledge vault on every boot, so this does the same. It only reads: no
// fact, vault file or state file is written, moved or deleted.
func ReconnectInceptionWiki(dataDir string, api *KnowledgeAPI, logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if api == nil {
		return
	}
	vaultDir := filepath.Join(dataDir, inceptionWikiDir)
	entries, err := os.ReadDir(vaultDir)
	if err != nil || len(entries) == 0 {
		return
	}
	name := inceptionWikiName(dataDir)
	if err := api.ConnectVault(vaultDir, name); err != nil {
		if !strings.Contains(err.Error(), "already connected") {
			logger.Warn("failed to reconnect inception wiki on startup", "error", err)
		}
		return
	}
	logger.Info("inception wiki reconnected on startup", "dir", vaultDir, "name", name, "files", len(entries))
}

// inceptionWikiName returns the vault name a past run saved in its state
// file (a renamed wiki), or the default.
func inceptionWikiName(dataDir string) string {
	data, err := os.ReadFile(filepath.Join(dataDir, inceptionStateFile))
	if err != nil {
		return inceptionWikiDefaultName
	}
	var state InceptionState
	if json.Unmarshal(data, &state) != nil || strings.TrimSpace(state.WikiName) == "" {
		return inceptionWikiDefaultName
	}
	return state.WikiName
}
