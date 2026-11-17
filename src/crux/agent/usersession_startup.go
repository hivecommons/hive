package agent

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/config"
)

// Startup auto-answer for user sessions. A new session used to open on the
// vendor CLI's first-run questions, and a person had to answer them before the
// session's first request could be typed. Agents get past the same screens
// unattended; sessions reuse that detector (panesignal.BlockingPrompts and the
// agents' input-prompt and login checks) under a much narrower rule, because a
// session runs with the CLI's permission prompts ON:
//
//   - only screens classified as STARTUP screens are answered
//     (panesignal.SessionStartupScreen): a first-run notice, the trust question
//     for the directory the session was opened in, onboarding, an update menu;
//   - a tool or command permission prompt is reported as refused, never
//     answered;
//   - a login or OAuth screen is reported as sign-in, never answered;
//   - nothing is sent once a person has typed into the session.
//
// The runtime only reads one screen and, when asked, answers that one screen
// (StartupStep). The bounded loop around it, the status shown in the session
// tab and the audit entry live in the dashboard (api_sessions_autostart.go).
const (
	// userSessionStartupKeyDelay separates the keystrokes of one answer (the
	// option, then Enter), as the agents' prompt watcher does.
	userSessionStartupKeyDelay = 300 * time.Millisecond
	// userSessionTypedFormat lists each attached client as
	// "<readonly> <created> <last activity>" (epoch seconds).
	userSessionTypedFormat = "#{client_readonly} #{client_created} #{client_activity}"
	userSessionTypedFields = 3
	// userSessionTypedGraceSec is how far a client's last activity must be
	// past its creation to count as a person typing. tmux only advances a
	// client's activity on input from that client's terminal (keys the hive
	// sends with send-keys do not touch it); the grace covers the terminal's
	// own replies to tmux's capability queries in the first moments of an
	// attach.
	userSessionTypedGraceSec = 2
	// userSessionClaudeConfigName is Claude Code's per-HOME state file.
	userSessionClaudeConfigName = ".claude.json"
	// userSessionClaudeProjectsKey / userSessionClaudeTrustKey are the keys of
	// that file recording workspace trust per project directory.
	userSessionClaudeProjectsKey = "projects"
	userSessionClaudeTrustKey    = "hasTrustDialogAccepted"
)

// userSessionSignInMarkers are lower-cased phrases of a vendor's sign-in
// menu, in addition to the agents' login-prompt patterns
// (paneShowsLoginPrompt). A pane carrying one is a sign-in screen.
var userSessionSignInMarkers = []string{
	"select login method",
	"sign in with",
	"log in with",
	"login with",
	"sign in to continue",
	"log in to continue",
}

// userSessionSeedOnceScript writes stdin to $1 only when nothing is there
// (set -C refuses to overwrite and to follow a symlink), private to the
// session uid. An existing file is the CLI's own state and is left alone.
const userSessionSeedOnceScript = `umask 077; if [ -e "$1" ] || [ -L "$1" ]; then exit 0; fi; set -C; cat > "$1"`

// userSessionShowsSignIn reports whether the visible pane is a login or OAuth
// screen.
func userSessionShowsSignIn(visible string) bool {
	if paneShowsLoginPrompt(strings.Split(visible, "\n")) || extractLoginURL(visible) != "" {
		return true
	}
	lower := strings.ToLower(paneTail(visible, blockingPromptTailLines))
	for _, m := range userSessionSignInMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// classifyUserSessionStartup reads one visible pane of a session's CLI and
// returns what it shows plus, for a startup screen, the keys that answer it.
// The order is the safety argument:
//
//  1. a sign-in screen is reported before anything could match it as a menu;
//  2. a refused prompt or a known startup screen;
//  3. a CLI that is already working is past startup. This comes after the
//     screens on purpose: the busy-footer test also matches the "Esc to
//     cancel" hint under a trust or login menu, and reading such a menu as a
//     working CLI would release the queued request into it;
//  4. the input prompt, by the agents' readiness test, with any pending menu
//     ruled out.
func classifyUserSessionStartup(vendor, visible string) (crux.UserSessionStartupStep, []string) {
	loading := crux.UserSessionStartupStep{Kind: crux.UserSessionStartupLoading}
	if strings.TrimSpace(visible) == "" {
		return loading, nil
	}
	ready := crux.UserSessionStartupStep{Kind: crux.UserSessionStartupReady}
	if userSessionShowsSignIn(visible) {
		return crux.UserSessionStartupStep{Kind: crux.UserSessionStartupSignIn}, nil
	}
	if scr, ok := panesignal.SessionStartupScreen(vendor, visible); ok {
		if scr.Refused {
			return crux.UserSessionStartupStep{Kind: crux.UserSessionStartupRefused, Screen: scr.Label}, nil
		}
		return crux.UserSessionStartupStep{Kind: crux.UserSessionStartupScreen, Screen: scr.Label}, scr.Keys
	}
	if paneShowsActiveWork(visible) {
		return ready, nil
	}
	if paneShowsInputPrompt(visible) && !paneShowsConsentScreen(visible) && !panesignal.ShowsNumberedSelection(visible) {
		return ready, nil
	}
	return loading, nil
}

// userSessionClientsTyped reads `list-clients -F userSessionTypedFormat` and
// reports whether a writable client has had input since it attached. A
// read-only client cannot type, so its activity is ignored.
func userSessionClientsTyped(out []byte) bool {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != userSessionTypedFields || fields[0] != "0" {
			continue
		}
		created, err1 := strconv.ParseInt(fields[1], 10, 64)
		activity, err2 := strconv.ParseInt(fields[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if activity-created >= userSessionTypedGraceSec {
			return true
		}
	}
	return false
}

// StartupStep reads the session's visible screen once. When answer is true,
// the screen is a known startup screen and nobody has typed into the session,
// it sends that screen's answer. It never answers anything else, and it fails
// closed: when the attached clients cannot be listed the step reports Typed.
func (r *UserSessionRuntime) StartupStep(id, vendor string, answer bool) crux.UserSessionStartupStep {
	gone := crux.UserSessionStartupStep{Kind: crux.UserSessionStartupGone}
	if checkUserSessionID(id) != nil {
		return gone
	}
	uid, err := r.sessionUID(id)
	if err != nil {
		return gone
	}
	if p := r.probeAs(uid, id); !p.Exists || p.Dead {
		return gone
	}
	session := UserSessionTmuxName(id)
	// The visible screen only: an answered menu lingers in scrollback.
	out, err := r.tmuxAs(uid, id, "", "capture-pane", "-p", "-J", "-t", session)
	if err != nil {
		return crux.UserSessionStartupStep{Kind: crux.UserSessionStartupLoading}
	}
	step, keys := classifyUserSessionStartup(vendor, string(out))
	clients, err := r.tmuxAs(uid, id, "", "list-clients", "-t", session, "-F", userSessionTypedFormat)
	step.Typed = err != nil || userSessionClientsTyped(clients)
	if !answer || step.Typed || step.Kind != crux.UserSessionStartupScreen || len(keys) == 0 {
		return step
	}
	for i, key := range keys {
		if i > 0 {
			time.Sleep(userSessionStartupKeyDelay)
		}
		if _, err := r.tmuxAs(uid, id, "", "send-keys", "-t", session, key); err != nil {
			r.logger.Warn("user session startup answer failed", "id", id, "screen", step.Screen, "error", err)
			return step
		}
	}
	step.Answered = true
	r.logger.Info("user session startup screen answered", "id", id, "vendor", vendor, "screen", step.Screen)
	return step
}

// userSessionClaudeOnboardingSeed is the .claude.json a fresh Claude Code
// session HOME starts with: onboarding done and the session's own working
// directory trusted. It carries no account and no credential (the login is
// the separate read-only copy, userSessionLoginFiles), so a CLI that is not
// signed in still shows its login screen.
func userSessionClaudeOnboardingSeed(cwd string) ([]byte, error) {
	seed := map[string]any{claudeOnboardingKey: true}
	if cwd != "" {
		seed[userSessionClaudeProjectsKey] = map[string]any{
			cwd: map[string]any{userSessionClaudeTrustKey: true},
		}
	}
	return json.Marshal(seed)
}

// seedOnboarding is the first line against first-run screens: a HOME that
// already says onboarding is done never shows them, so nothing has to be
// answered. Only Claude Code has such a file today, and only a HOME without
// one is seeded: an existing file is the CLI's own (a resumed session) and is
// never rewritten. Best effort, since the auto-answer handles whatever still
// appears.
func (r *UserSessionRuntime) seedOnboarding(spec crux.UserSessionSpec, uid int, cwd string) {
	if !spec.SeedOnboarding || spec.Vendor != config.ClaudeBackend {
		return
	}
	data, err := userSessionClaudeOnboardingSeed(cwd)
	if err != nil {
		return
	}
	target := filepath.Join(userSessionHome(spec.ID), userSessionClaudeConfigName)
	if out, err := r.privRun(userSessionSpec(uid), strings.NewReader(string(data)), "sh", "-c", userSessionSeedOnceScript, "sh", target); err != nil {
		r.logger.Warn("user session onboarding seed failed", "id", spec.ID, "error", outputErr("seed", err, out))
	}
}
