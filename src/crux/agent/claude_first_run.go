package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/config"
)

// Claude Code first-run onboarding, for per-UID interactive agents.
//
// The CLI runs its onboarding flow (theme, the "Security notes" screen ending
// in "Press Enter to continue…", terminal setup) whenever $HOME/.claude.json
// lacks hasCompletedOnboarding. Inference homes have always seeded that flag
// (inferenceUserConfigSeed); per-UID interactive homes never did. They relied
// on seedClaudeSessionForAgent, which is adopt-only and judges a source by
// oauthAccount alone — and a signed-in .claude.json the CLI writes for itself
// carries oauthAccount WITHOUT hasCompletedOnboarding (the same two-key split
// bin/contributor-agent.sh seed_claude_config documents). Agents configured at
// boot survived because their own file on the PVC finished onboarding long
// ago; an agent added at runtime (the meetings research pool) starts from a
// fresh home, adopted a sibling's identity minus the flag, and parked on the
// security notice for every kick.

const (
	// claudeProjectsKey / claudeProjectTrustKey are the .claude.json keys that
	// record per-cwd workspace trust ("Is this a project you created or one
	// you trust?"), keyed by the absolute project directory.
	claudeProjectsKey     = "projects"
	claudeProjectTrustKey = "hasTrustDialogAccepted"
	// jsonTrue is the raw JSON literal written for a seeded boolean flag.
	jsonTrue = "true"
)

// claudeFirstRunNoticeLabel is the BlockingPrompts label of Claude Code's
// first-run security notice.
const claudeFirstRunNoticeLabel = panesignal.ClaudeFirstRunNoticeLabel

// claudeFirstRunNoticeInjectionMarker is the notice's "Due to prompt injection
// risks" line, which the kick-refusal scanner must not read as a refusal.
const claudeFirstRunNoticeInjectionMarker = panesignal.ClaudeFirstRunNoticeInjectionMarker

const (
	// claudeFirstRunNoticeEnterAttempts bounds how many Enters one kick
	// readiness wait sends to the first-run notice before giving up on it. One
	// is enough on every CLI version seen; the second covers a keystroke that
	// landed while the screen was still painting.
	claudeFirstRunNoticeEnterAttempts = 2
	// claudeFirstRunNoticeEnterDelay lets the TUI move to the next onboarding
	// screen after an Enter before the pane is captured again.
	claudeFirstRunNoticeEnterDelay = 500 * time.Millisecond
	// kickBlockingScreenGiveUpAfter is how long a kick readiness wait tolerates
	// a known blocking screen it does not answer itself (folder trust, codex
	// update, …) before failing with a reason naming it, instead of running
	// out inputPromptTimeout. The lifetime prompt watcher answers those on a
	// trustPollInterval poll plus trustCooldown, well inside this window.
	kickBlockingScreenGiveUpAfter = 20 * time.Second
)

// seedClaudeOnboardingForAgent asserts hasCompletedOnboarding (and workspace
// trust for the agent's own work dir) in a per-agent .claude.json that already
// exists, so the CLI starts at its input prompt instead of the onboarding flow.
//
// MERGE-ONLY, and never a fabrication: an absent file stays absent (the login
// menu is the honest state when no signed-in source exists — see
// seedClaudeSessionForAgent), every existing key is preserved byte for byte,
// and a file that does not parse as a JSON object is left alone. Runs after
// adoption on every fresh tmux session, so it also repairs an agent whose own
// file was written without the flag by an earlier, failed first launch. The
// CLI is not running yet at this point, so there is no rewrite to race.
func (m *Manager) seedClaudeOnboardingForAgent(agent *AgentProcess, backend, home string) {
	if backend != config.ClaudeBackend {
		return
	}
	target := claudeSessionFile(home)
	// Lstat, never Stat: the reads and writes below follow symlinks, and a
	// link planted at .claude.json must not redirect them (audit F12 posture).
	info, err := os.Lstat(target)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			m.logger.Warn("failed to inspect claude session for onboarding seed",
				"agent", agent.Name, "path", target, "error", err)
		}
		return
	}
	if !info.Mode().IsRegular() {
		m.logger.Warn("refusing to seed claude onboarding (session file is not a regular file)",
			"agent", agent.Name, "path", target, "mode", info.Mode().String())
		return
	}
	data, err := m.readClaudeSessionForAdoption(target)
	if err != nil {
		m.logger.Warn("failed to read claude session for onboarding seed",
			"agent", agent.Name, "path", target, "error", err)
		return
	}
	merged, changed, err := mergeClaudeOnboardingSeed(data, filepath.Join(m.workDir, agent.Name))
	if err != nil {
		m.logger.Warn("claude session unparseable; leaving it for the CLI and not seeding onboarding",
			"agent", agent.Name, "path", target, "error", err)
		return
	}
	if !changed {
		return
	}
	if err := m.writeClaudeSessionForAgent(target, merged, m.agentExecUserSpec(agent)); err != nil {
		m.logger.Warn("failed to seed claude onboarding",
			"agent", agent.Name, "path", target, "error", err)
		return
	}
	if agent.UID > 0 {
		// Best-effort, as in seedClaudeSessionForAgent: unprivileged
		// deployments cannot chown, and the seed mode keeps the file usable.
		_ = os.Chown(target, agent.UID, -1)
	}
	m.logger.Info("seeded claude onboarding into agent session so the CLI skips its first-run screens",
		"agent", agent.Name, "path", target)
}

// mergeClaudeOnboardingSeed returns data with hasCompletedOnboarding set and
// projectDir marked trusted, and whether anything changed. Values are handled
// as raw JSON so every key the CLI owns (numeric ids included) round-trips
// unchanged. hasCompletedOnboarding is set whenever it is not already true —
// the flag's only job is to skip onboarding, so an explicit false is the
// condition being repaired. A trust entry that already exists is kept as is.
func mergeClaudeOnboardingSeed(data []byte, projectDir string) ([]byte, bool, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, false, err
	}
	if top == nil {
		return nil, false, errors.New("claude session is not a JSON object")
	}
	changed := false
	if !jsonValueIsTrue(top[claudeOnboardingKey]) {
		top[claudeOnboardingKey] = json.RawMessage(jsonTrue)
		changed = true
	}
	if projectDir != "" {
		if projects, ok := seedProjectTrust(top[claudeProjectsKey], projectDir); ok {
			top[claudeProjectsKey] = projects
			changed = true
		}
	}
	if !changed {
		return data, false, nil
	}
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false, fmt.Errorf("marshal claude session: %w", err)
	}
	return out, true, nil
}

// seedProjectTrust returns the projects object with projectDir trusted, and
// whether it changed. An unparseable projects value (or project entry) is left
// untouched: trust is a convenience — the prompt watcher answers the dialog —
// and never worth rewriting state this code does not understand.
func seedProjectTrust(raw json.RawMessage, projectDir string) (json.RawMessage, bool) {
	projects := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &projects); err != nil || projects == nil {
			return nil, false
		}
	}
	project := map[string]json.RawMessage{}
	if existing, ok := projects[projectDir]; ok {
		if err := json.Unmarshal(existing, &project); err != nil || project == nil {
			return nil, false
		}
	}
	if _, ok := project[claudeProjectTrustKey]; ok {
		return nil, false
	}
	project[claudeProjectTrustKey] = json.RawMessage(jsonTrue)
	projectJSON, err := json.Marshal(project)
	if err != nil {
		return nil, false
	}
	projects[projectDir] = projectJSON
	out, err := json.Marshal(projects)
	if err != nil {
		return nil, false
	}
	return out, true
}

// noteClaudeFirstRunNotice logs, once per agent per process, that its pane hit
// Claude Code's first-run security notice and is being answered. Once, because
// both the lifetime prompt watcher and every kick readiness wait can see the
// same screen; the line exists to say "this agent's HOME never finished CLI
// onboarding", which is a per-agent fact, not a per-sighting one.
func (m *Manager) noteClaudeFirstRunNotice(agent *AgentProcess, path string) {
	if _, loaded := m.firstRunNoticeLogged.LoadOrStore(agent.Name, struct{}{}); loaded {
		return
	}
	m.logger.Warn("Claude Code first-run security notice on screen; pressing Enter to continue (the agent HOME never completed CLI onboarding)",
		"agent", agent.Name, "path", path, "home", AgentHome(agent.Name, agent.UID, agent.effectiveBackend()))
}

// continueFirstRunNoticeForKick presses Enter on the first-run security notice
// a kick found in front of the CLI, at most claudeFirstRunNoticeEnterAttempts
// times, and reports whether the notice is gone. Gone is enough: whatever
// onboarding step follows is the readiness wait's to handle, and it is not a
// reason to restart into the same notice. Caller must NOT hold m.mu.
func (m *Manager) continueFirstRunNoticeForKick(agent *AgentProcess) bool {
	for attempt := 0; attempt < claudeFirstRunNoticeEnterAttempts; attempt++ {
		m.noteClaudeFirstRunNotice(agent, "kick")
		m.tmuxSendKeysForAgent(agent, "Enter")
		m.sleepDuringPromptDismiss(claudeFirstRunNoticeEnterDelay)
		if !panesignal.ShowsClaudeFirstRunNotice(m.captureVisiblePaneForAgent(agent)) {
			return true
		}
	}
	m.logger.Warn("first-run security notice did not close after Enter",
		"agent", agent.Name, "enters", claudeFirstRunNoticeEnterAttempts)
	return false
}

// kickBlockingScreen tracks the known blocking screen one kick readiness wait
// is looking at, across poll ticks.
type kickBlockingScreen struct {
	label  string
	since  time.Time
	enters int
}

// blockingScreenNotReadyReason is the undeliverable reason for a kick whose
// pane is parked on a known blocking screen, carried into logs and the
// dashboard busy reason. kickPromptNotReadyErr prefixes it with "CLI did not
// reach input prompt", which pkg/research matches for its retry.
func blockingScreenNotReadyReason(label string) string {
	return fmt.Sprintf("prompt-not-ready: pane blocked on %s", label)
}

// handleBlockingScreenForKick deals with a known blocking screen (see
// panesignal.BlockingPrompts) in front of a kick. handled reports whether one
// is on screen; a non-empty reason means the wait should give up now.
//
// The first-run security notice is answered here with Enter, bounded by
// claudeFirstRunNoticeEnterAttempts: it has no options, so Enter is the whole
// answer, and waiting on the watcher's trustReanswerAfter cadence is what let
// a kick burn its full inputPromptTimeout. Every other blocking screen is left
// to watchForTrustPromptForAgent — some take a typed digit, and a second
// answerer would type it into the prompt that replaces the menu — and the
// wait only gives up once one has outlasted kickBlockingScreenGiveUpAfter.
// Caller must NOT hold m.mu.
func (m *Manager) handleBlockingScreenForKick(agent *AgentProcess, backend, visible string, s *kickBlockingScreen) (handled bool, reason string) {
	_, label, ok := blockingPromptKey(backend, visible)
	if !ok {
		*s = kickBlockingScreen{}
		return false, ""
	}
	if label != s.label {
		*s = kickBlockingScreen{label: label, since: time.Now()}
	}
	if label == claudeFirstRunNoticeLabel {
		if s.enters >= claudeFirstRunNoticeEnterAttempts {
			reason = blockingScreenNotReadyReason(label)
			m.logger.Warn("kick undeliverable: first-run security notice did not close after Enter",
				"agent", agent.Name, "reason", reason, "enters", s.enters)
			return true, reason
		}
		s.enters++
		m.noteClaudeFirstRunNotice(agent, "kick")
		m.tmuxSendKeysForAgent(agent, "Enter")
		m.sleepDuringPromptDismiss(claudeFirstRunNoticeEnterDelay)
		return true, ""
	}
	if time.Since(s.since) >= kickBlockingScreenGiveUpAfter {
		reason = blockingScreenNotReadyReason(label)
		m.logger.Warn("kick undeliverable: pane stayed on a known blocking screen",
			"agent", agent.Name, "reason", reason, "waited", time.Since(s.since))
		return true, reason
	}
	return true, ""
}
