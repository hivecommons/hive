package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// Per-session filesystem layout. A session never shares a HOME with another
// session or with an agent, so a vendor CLI's transcripts, caches and memory
// stay per-session. Vendor LOGINS are shared fleet-wide by bridging only the
// credential files a vendor CLI needs (the perAgentCLIHomes bridge lists the
// fleet already trusts) from the shared home, never the whole shared
// ~/.config (which holds gh's hosts.yml).
const (
	userSessionHomeDirName = "sessions"
	userSessionHomeMode    = 0o700
	userSessionWorkDirName = "work"
	// userSessionsDirEnv relocates the sessions data root (default
	// /data/sessions), shared with the dashboard store.
	userSessionsDirEnv     = "HIVE_SESSIONS_DIR"
	defaultUserSessionsDir = "/data/sessions"
)

// sharedConfigBridges are the ~/.config subtrees bridged from the shared home
// because a vendor stores its provider login there. gh is deliberately absent.
var sharedConfigBridges = []string{"goose", "opencode", "aider"}

// userSessionsDir is the data root holding sessions.json and per-session work
// dirs.
func userSessionsDir() string {
	if v := os.Getenv(userSessionsDirEnv); v != "" {
		return v
	}
	return defaultUserSessionsDir
}

func userSessionHome(id string) string {
	return filepath.Join(sharedAgentHome, userSessionHomeDirName, id)
}

func userSessionCodexHome(id string) string {
	return filepath.Join(userSessionHome(id), ".codex")
}

func userSessionWorkDir(id string) string {
	return filepath.Join(userSessionsDir(), id, userSessionWorkDirName)
}

// setupUserSessionHome creates the per-session HOME, XDG dirs, CODEX_HOME and
// work dir and bridges the shared vendor login files into it.
func setupUserSessionHome(id string) error {
	home := userSessionHome(id)
	dirs := []string{
		home,
		filepath.Join(home, ".config"),
		agentXDGDataHome(home),
		agentXDGStateHome(home),
		userSessionCodexHome(id),
		userSessionWorkDir(id),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, userSessionHomeMode); err != nil {
			return fmt.Errorf("creating session dir %s: %w", d, err)
		}
	}
	for _, spec := range perAgentCLIHomes {
		dir := filepath.Join(home, spec.dir)
		if err := os.MkdirAll(dir, userSessionHomeMode); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
		for _, name := range spec.bridges {
			bridgeSessionEntry(filepath.Join(dir, name), filepath.Join(sharedAgentHome, spec.dir, name))
		}
	}
	for _, name := range sharedConfigBridges {
		bridgeSessionEntry(filepath.Join(home, ".config", name), filepath.Join(sharedAgentHome, ".config", name))
	}
	// codex keeps its login in CODEX_HOME/auth.json.
	bridgeSessionEntry(filepath.Join(userSessionCodexHome(id), "auth.json"), filepath.Join(sharedAgentHome, ".codex", "auth.json"))
	return nil
}

// bridgeSessionEntry links link -> target when the shared target exists and
// link is absent. A missing target is skipped (the vendor was never signed in
// fleet-wide); an existing link is left alone so a re-launch keeps whatever the
// CLI wrote.
func bridgeSessionEntry(link, target string) {
	if _, err := os.Stat(target); err != nil {
		return
	}
	if _, err := os.Lstat(link); err == nil {
		return
	}
	_ = os.Symlink(target, link)
}

// userSessionEnvPairs are the per-session variables set on the tmux session
// after the inherited credentials were stripped.
func userSessionEnvPairs(id, scrubScript string) [][2]string {
	home := userSessionHome(id)
	pairs := [][2]string{
		{"HOME", home},
		{"CODEX_HOME", userSessionCodexHome(id)},
		{"XDG_CONFIG_HOME", filepath.Join(home, ".config")},
		{"XDG_DATA_HOME", agentXDGDataHome(home)},
		{"XDG_STATE_HOME", agentXDGStateHome(home)},
		{"HIVE_SESSION_ID", id},
	}
	if scrubScript != "" {
		// Sourced by every non-interactive bash the CLI spawns; unsets any
		// GitHub token the CLI re-exports into its tool shells (#4045).
		pairs = append(pairs, [2]string{"BASH_ENV", scrubScript}, [2]string{"ENV", scrubScript})
	}
	return pairs
}
