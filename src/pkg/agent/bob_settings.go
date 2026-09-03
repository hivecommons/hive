package agent

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hivecommons/hive/pkg/config"
)

// Unix permission bits for the agent-UID readability probe. Named rather than
// inlined so the intent of each mask is explicit at the call site.
const (
	// bobKeyGroupReadBit is `g+r` — the bit that lets an agent UID (2001+, in
	// group node) read a dev:node-owned key file.
	bobKeyGroupReadBit fs.FileMode = 0o040
	// bobKeyGroupExecBit is `g+x` — on a DIRECTORY this is traverse
	// permission, required to open a file inside it by name.
	bobKeyGroupExecBit fs.FileMode = 0o010
	// bobKeyOtherReadBit is `o+r`. Also sufficient for the agent to read, but
	// it means the secret is world-readable, which is worth flagging.
	bobKeyOtherReadBit fs.FileMode = 0o004
	// bobStateGroupWriteBit is `g+w` — the bit that lets an agent UID create
	// or replace files inside a dev:node-owned bob state directory.
	bobStateGroupWriteBit fs.FileMode = 0o020
)

// verifyBobKeyReadable checks that the resolved bob key file is readable BY
// THE AGENT UID, and logs an actionable error when it is not.
//
// This exists because the obvious check is a false positive. ResolveAPIKey
// runs in the hive process as dev, which owns the key file — so it succeeds,
// the Governor Bob tab renders "✓ Key set", and everything looks configured.
// But bobshell reads BOBSHELL_API_KEY while running as the agent UID, which on
// a dev:node 0600 file gets EACCES. The key resolves empty, bob silently falls
// back to the W3ID browser SSO flow that cannot complete in a pod, and the
// agent sits at the auth prompt with no error anywhere. That silent
// disagreement between "hive can read it" and "the agent can read it" is the
// whole bug, so it gets an explicit probe.
//
// Root agents (UID 0) and unset UIDs are skipped — they can read anything.
// Reports whether the key looks readable; callers log-and-continue rather than
// refusing to launch, since the Secret-mounted path may still work.
// Logs paths, modes and UIDs only — never the key value.
func (m *Manager) verifyBobKeyReadable(agentName, keyPath string, uid int) bool {
	if uid <= 0 || keyPath == "" {
		return true
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		m.logger.Warn("bob key file not statable; bob will fall back to SSO and hang at the auth prompt",
			"agent", agentName, "path", keyPath, "uid", uid, "error", err)
		return false
	}
	mode := info.Mode().Perm()

	// The directory must be group-traversable or the file cannot be opened at
	// all, regardless of its own mode.
	dir := filepath.Dir(keyPath)
	if dirInfo, derr := os.Stat(dir); derr == nil {
		if dirMode := dirInfo.Mode().Perm(); dirMode&bobKeyGroupExecBit == 0 {
			m.logger.Error("bob key directory is not traversable by the agent UID; bob cannot read the key and will hang at the SSO prompt",
				"agent", agentName, "dir", dir,
				"dir_mode", fs.FileMode(dirMode).String(), "uid", uid,
				"remedy", "chmod 710 "+dir+" (group execute lets the agent open the key by name without listing the dir)")
			return false
		}
	}

	if mode&bobKeyGroupReadBit == 0 && mode&bobKeyOtherReadBit == 0 {
		m.logger.Error("bob key file is not readable by the agent UID; bob cannot read the key and will hang at the SSO prompt",
			"agent", agentName, "path", keyPath,
			"mode", fs.FileMode(mode).String(), "uid", uid,
			"remedy", "chmod 440 "+keyPath+" and ensure it is group-owned by node")
		return false
	}
	if mode&bobKeyOtherReadBit != 0 {
		m.logger.Warn("bob key file is world-readable; group-read (440) is sufficient",
			"agent", agentName, "path", keyPath, "mode", fs.FileMode(mode).String())
	}
	return true
}

// bobSharedHome is the HOME every non-inference agent runs with (see
// agentEnvPairs). bob resolves its settings file from os.homedir(), so all bob
// agents on a hive share ONE /data/home/.bob/settings.json.
const bobSharedHome = "/data/home"

// bobSettingsPath returns the shared bob settings file for a given HOME.
func bobSettingsPath(home string) string {
	return filepath.Join(home, config.BobSettingsRelPath)
}

// nearestExistingDir walks up from dir and returns the first ancestor that
// exists, or "" if none does. Used to locate the directory whose permissions
// actually govern a recursive mkdir of a not-yet-existing path.
func nearestExistingDir(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		// filepath.Dir is its own fixed point at the root ("/" or "."), which
		// is the loop's only termination condition.
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// verifyBobStateDirsWritable probes, AS THE AGENT UID, every directory bob
// writes to during startup, and logs an actionable error for each one that is
// not writable.
//
// This exists because bob reports these failures only inside its own TUI —
// "There was an error saving your latest settings changes." and "Failed to
// initialize logger:" — which nobody sees unless they are attached to that
// pane. Nothing reaches the hive log, so a genuinely unwritable directory is
// invisible to an operator reading `kubectl logs` and gets misdiagnosed as a
// read-only filesystem. The same false-positive trap as verifyBobKeyReadable
// applies: the hive process runs as dev and OWNS these directories, so any
// check it performs on its own behalf succeeds while the agent UID still gets
// EACCES. The probe therefore tests the agent's group access explicitly rather
// than calling os.Access as dev.
//
// Advisory only — it never blocks a launch. bob degrades rather than dying
// when these writes fail (ephemeral installation ID, no chat recording), so a
// probe failure is a reason to log loudly, not to refuse to start the agent.
// Logs paths, modes and UIDs only — never any file contents.
//
// Returns the directories found unwritable, so callers and tests can assert on
// the outcome rather than scraping log output. A nil result means every
// existing state dir is writable by the agent UID.
func (m *Manager) verifyBobStateDirsWritable(agentName, home, workDir string, uid int) []string {
	if uid <= 0 {
		return nil
	}
	var unwritable []string

	// The directories bob creates or writes during startup:
	//   - $HOME/.bob            settings.json, trustedFolders.json,
	//                           installation_id, tmp/ (chat recordings)
	//   - <workdir>/.bob        per-agent .bob-errors/ logger target, which is
	//                           what "Failed to initialize logger:" refers to
	probeDirs := []string{
		filepath.Dir(bobSettingsPath(home)),
		filepath.Join(workDir, config.BobStateDirName),
	}

	for _, dir := range probeDirs {
		info, err := os.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				// A missing dir is not itself a failure — bob creates these
				// with mkdirSync({recursive:true}) on first use. But that
				// mkdir runs as the AGENT UID, so it succeeds only if the
				// PARENT is group-writable. Probing the parent here is the
				// whole point on a fresh hive: this is precisely the
				// first-run EACCES in #2284, and the original probe returned
				// "all clear" for it because it skipped missing dirs
				// entirely. Check the nearest existing ancestor instead.
				if parent := nearestExistingDir(filepath.Dir(dir)); parent != "" {
					if pInfo, pErr := os.Stat(parent); pErr == nil {
						pMode := pInfo.Mode().Perm()
						if pMode&bobStateGroupWriteBit == 0 || pMode&bobKeyGroupExecBit == 0 {
							m.logger.Error("bob state dir does not exist and its parent is not writable by the agent UID; bob's first-run mkdir will fail with EACCES",
								"agent", agentName, "dir", dir, "parent", parent,
								"parent_mode", fs.FileMode(pMode).String(), "uid", uid,
								"remedy", "chmod 2775 "+parent+" and ensure it is group-owned by node")
							unwritable = append(unwritable, dir)
						}
					}
				}
				continue
			}
			m.logger.Warn("bob state dir not statable; bob may fail to persist settings or initialize its logger",
				"agent", agentName, "dir", dir, "uid", uid, "error", err)
			continue
		}
		if !info.IsDir() {
			m.logger.Warn("bob state path exists but is not a directory",
				"agent", agentName, "path", dir, "uid", uid)
			continue
		}

		// The agent UID reaches these dev-owned dirs through group `node`, so
		// group write+execute is what actually decides whether bob can create
		// or replace a file inside. Write alone is not enough — without
		// execute the path cannot be traversed to open the file by name.
		mode := info.Mode().Perm()
		if mode&bobStateGroupWriteBit == 0 || mode&bobKeyGroupExecBit == 0 {
			m.logger.Error("bob state dir is not writable by the agent UID; bob will report \"error saving your latest settings changes\" and \"Failed to initialize logger\"",
				"agent", agentName, "dir", dir,
				"mode", fs.FileMode(mode).String(), "uid", uid,
				"remedy", "chmod 770 "+dir+" and ensure it is group-owned by node")
			unwritable = append(unwritable, dir)
		}
	}
	return unwritable
}

// ensureBobAuthSettings makes hive the owner of the `security.auth` block in
// bob's persisted settings file.
//
// Why this is needed at all: BOBSHELL_DEFAULT_AUTH_TYPE is only a fallback
// DEFAULT. Per bundle/bob.js, a persisted security.auth.selectedType is
// consulted FIRST and wins over the env var, so a single agent that once
// picked "Log in with IBM" at the prompt writes selectedType:"sso" into the
// SHARED file and parks every bob agent on the hive at the SSO/API-key prompt.
//
// What it writes:
//   - selectedType = "api-key" — the choice itself.
//   - enforcedType = "api-key" — strictly stronger. The bundle filters the
//     option list down to enforcedType
//     (`enforcedType && (a=a.filter(p=>p.value===enforcedType))`), so SSO stops
//     being selectable, and eEr() turns any other active type into an error.
//     Together these mean a stray write cannot re-break the fleet.
//
// It is called on EVERY bob launch, so an out-of-band write self-heals on the
// next launch rather than persisting until someone notices.
//
// The merge is deliberately key-scoped: the file also holds instanceId,
// teamId, licenseConsent, isNotFirstTime, prev_version and ibm_secrets, and
// clobbering licenseConsent alone would make bob hard-error on startup. Unknown
// keys are decoded into a generic map and re-encoded untouched.
//
// Errors are logged and swallowed: a settings file we cannot write is not a
// reason to abort a launch that may still succeed via the env var.
// The API key is never read or written here — only the auth TYPE.
func (m *Manager) ensureBobAuthSettings(agentName, home string) {
	path := bobSettingsPath(home)

	if err := os.MkdirAll(filepath.Dir(path), config.BobSettingsDirMode); err != nil {
		m.logger.Warn("bob settings: mkdir failed",
			"agent", agentName, "path", filepath.Dir(path), "error", err)
		return
	}

	settings := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// A corrupt/truncated file must not wedge every bob agent forever.
		// Start from an empty object instead: the keys we would have preserved
		// are already unreadable, and bob re-derives them (licenseConsent comes
		// back via --accept-license on the launch command).
		if uerr := json.Unmarshal(data, &settings); uerr != nil {
			m.logger.Warn("bob settings: unparseable, rewriting auth block from empty",
				"agent", agentName, "path", path, "error", uerr)
			settings = map[string]any{}
		}
	case !os.IsNotExist(err):
		m.logger.Warn("bob settings: read failed",
			"agent", agentName, "path", path, "error", err)
		return
	}

	changed := setBobAuthBlock(settings)
	if !changed {
		return
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		m.logger.Warn("bob settings: marshal failed", "agent", agentName, "error", err)
		return
	}
	if err := os.WriteFile(path, out, config.BobSettingsFileMode); err != nil {
		m.logger.Warn("bob settings: write failed",
			"agent", agentName, "path", path, "error", err)
		return
	}
	// Re-chmod explicitly: WriteFile's mode only applies at CREATE time, so an
	// existing file created with a tighter umask would stay non-group-writable
	// and the next agent UID could not update it.
	if err := os.Chmod(path, config.BobSettingsFileMode); err != nil {
		m.logger.Warn("bob settings: chmod failed",
			"agent", agentName, "path", path, "error", err)
	}
	// Log the TYPE and PATH only — never the key.
	m.logger.Info("bob settings: asserted api-key auth",
		"agent", agentName, "path", path,
		"selected_type", config.BobAuthTypeAPIKey,
		"enforced_type", config.BobAuthTypeAPIKey)
}

// setBobAuthBlock merges the hive-owned auth values into settings in place,
// creating the intermediate `security` / `security.auth` objects as needed and
// preserving every sibling key. It reports whether anything actually changed,
// so a already-correct file is not rewritten on every launch (which would
// churn mtime and race concurrent launches for no benefit).
//
// Intermediate values of the wrong JSON type (e.g. `"security": "yes"`) are
// replaced rather than merged — there is no sane merge, and bob itself would
// fail to read them.
func setBobAuthBlock(settings map[string]any) bool {
	security, ok := settings[config.BobSettingsSecurityKey].(map[string]any)
	if !ok {
		security = map[string]any{}
		settings[config.BobSettingsSecurityKey] = security
	}
	auth, ok := security[config.BobSettingsAuthKey].(map[string]any)
	if !ok {
		auth = map[string]any{}
		security[config.BobSettingsAuthKey] = auth
	}

	changed := false
	for _, key := range []string{
		config.BobSettingsSelectedTypeKey,
		config.BobSettingsEnforcedTypeKey,
	} {
		if cur, _ := auth[key].(string); cur != config.BobAuthTypeAPIKey {
			auth[key] = config.BobAuthTypeAPIKey
			changed = true
		}
	}
	return changed
}
