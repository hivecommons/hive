// Package agent: credential watchdog — periodic verification that each in-use
// CLI backend's durable credential file (Copilot device-flow token, Claude
// OAuth credentials) is present and usable. Pure move from manager.go
// (refs #7303); no behavior change.
package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

const (
	// defaultCredentialWatchdogInterval is how often the credential watchdog
	// checks that each in-use backend's durable credential file still exists
	// and is usable. It is a slow health check (a missing/expired credential is
	// a standing condition until an operator re-logs in, not a fast-moving one),
	// so a coarse interval keeps the Audit Log signal-not-noise while still
	// catching a post-upgrade-roll loss within a few minutes.
	defaultCredentialWatchdogInterval = 5 * time.Minute
	// CredentialWatchdogIntervalEnv overrides the watchdog interval with a Go
	// duration string. Invalid or non-positive values fall back to the default;
	// a value of "0" does NOT disable the watchdog (use the parse-failure path
	// only for overrides) — disabling is intentionally not offered so the
	// safety net cannot be silently turned off.
	CredentialWatchdogIntervalEnv = "HIVE_CREDENTIAL_WATCHDOG_INTERVAL"
)

// credentialWatchdogInterval resolves the watchdog interval from
// CredentialWatchdogIntervalEnv, falling back to the default.
func credentialWatchdogInterval() time.Duration {
	if v := os.Getenv(CredentialWatchdogIntervalEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultCredentialWatchdogInterval
}

// credentialWatch describes one CLI backend whose usable credential lives in a
// durable file on the hive PVC that the hive relies on but does NOT itself keep
// alive. Only backends of this shape belong here: bob resolves its key at
// launch and fails loudly, and gemini/codex/goose/pi/inference backends keep
// their creds under the agent's own $HOME (or do no CLI login at all), so there
// is no hive-managed file for a presence check to watch.
//
// probe reports a credentialProbe: ok=true means the credential is usable;
// when false, reason is a short human string ("missing" / "login expired") for
// the audit detail and log. It must only stat/parse the file — never emit,
// mutate, or return token material.
type credentialWatch struct {
	backend     string
	path        string
	auditAction string
	probe       func(path string) credentialProbe
}

// credentialProbe is one probe's verdict on a durable credential.
//
// recovery exists because the watchdog used to hardcode "operator dashboard
// device-flow login" for every unusable credential, and that is wrong for the
// failure #5730 describes: the shared Claude credential rewritten 0600 by a
// token refresh holds a live access token and a valid refresh grant, and no
// number of re-logins fixes it — the next refresh re-tightens the file. An
// operator sent to redo an OAuth flow reads that as "hive needs a daily
// re-login", which is exactly how #5454 stayed misdiagnosed for so long. The
// probe knows which condition it found, so the probe names the recovery.
//
// fields carries extra structured log context (mode, owner) and must never
// carry token material.
type credentialProbe struct {
	ok       bool
	reason   string
	recovery string
	fields   []any
}

// credentialOK is the usable verdict.
func credentialOK() credentialProbe { return credentialProbe{ok: true} }

// defaultCredentialRecovery is the recovery for a credential that genuinely
// needs a human to authenticate — the only case the watchdog used to know.
const defaultCredentialRecovery = "operator dashboard device-flow login"

// credentialReadable reports whether this process can actually OPEN the file,
// separating "the credential is spent" from "the credential is fine and we
// cannot read it". os.Stat is not enough: stat succeeds on a 0600 file owned by
// another uid as long as the directory is traversable, which is precisely the
// #5730 state — so every check built on Stat plus a parse reported a perfectly
// healthy credential as an expired login.
func credentialReadable(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	_ = f.Close()
	return true, nil
}

// credentialModeAndOwner reports the file's permission bits and owning uid for
// the operator-facing log. Best-effort: an unreadable stat yields zero values
// and the caller simply logs less.
func credentialModeAndOwner(path string) (mode string, ownerUID uint32) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0
	}
	mode = fi.Mode().Perm().String()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ownerUID = st.Uid
	}
	return mode, ownerUID
}

// copilotTokenUsable reports whether the durable Copilot device-flow token file
// is present and non-empty. It reads only the file's presence and size — never
// its contents.
func copilotTokenUsable(path string) credentialProbe {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return credentialProbe{reason: "missing", recovery: defaultCredentialRecovery}
	}
	return credentialOK()
}

// claudeTokenUsable reports whether the Claude credentials file can still put
// agents to work. Unlike copilot's, a Claude credential can be PRESENT but
// unusable, so a bare presence check is insufficient — it delegates to
// claude.HasUsableToken. It distinguishes an absent file ("missing") from one
// that is genuinely spent ("login expired") for a more actionable alert.
//
// An access token that has merely aged out is NOT unusable: the refresh grant
// beside it mints a new one on the next CLI start, with no operator involved.
// Reporting that state as unusable is what made this watchdog prescribe an
// interactive login every time a hive ran longer than a Claude access token
// lives — roughly once a day, for a credential that was fine.
func claudeTokenUsable(path string) credentialProbe {
	if _, err := os.Stat(path); err != nil {
		return credentialProbe{reason: "missing", recovery: defaultCredentialRecovery}
	}
	// Readability BEFORE usability. claude.HasUsableToken reports positive
	// evidence only, so it cannot distinguish a spent credential from one it was
	// not allowed to open — and on the shared CLI home those are opposite
	// conditions with opposite recoveries (#5730). Asking first is what turns
	// "login expired (no usable refresh grant) -> operator device-flow login"
	// into the truth: the grant is live, the file is 0600, and one chmod fixes
	// it without touching the login at all.
	if _, err := credentialReadable(path); errors.Is(err, fs.ErrPermission) {
		mode, ownerUID := credentialModeAndOwner(path)
		return credentialProbe{
			reason:   "unreadable by the hive process (permission denied)",
			recovery: "chmod g+r " + path + " — the credential itself is fine; a re-login will not help",
			fields: []any{
				"mode", mode,
				"owner_uid", ownerUID,
				"reader_uid", os.Geteuid(),
				"cause", "a CLI token refresh rewrote the shared credential owner-only; the fleet reaches it through the node group",
			},
		}
	}
	if !claude.HasUsableToken(path) {
		return credentialProbe{
			reason:   "login expired (no usable refresh grant)",
			recovery: defaultCredentialRecovery,
		}
	}
	return credentialOK()
}

// credentialWatches is the set of durable-credential files the watchdog guards,
// keyed by backend. Adding a new CLI backend of the same shape is a one-line
// entry here — nothing else in the loop changes.
func credentialWatches() []credentialWatch {
	return []credentialWatch{
		{backend: "copilot", path: copilotUserTokenWatchPath, auditAction: AuditCopilotTokenMissing, probe: copilotTokenUsable},
		{backend: "claude", path: claude.CredentialsPath, auditAction: AuditClaudeTokenMissing, probe: claudeTokenUsable},
	}
}

// StartCredentialWatchdog periodically verifies that each in-use CLI backend's
// durable credential file (Copilot device-flow token, Claude OAuth credentials)
// is present and usable, and emits an audit event + logs a warning when it is
// not. It is a SAFETY NET, not a fixer: it never reads, writes, or otherwise
// touches token material (recovery is an operator dashboard device-flow login,
// per the manual-login rule). It exists because the loudest failure mode we
// see is silent: a fresh agent pod after an upgrade roll finds no durable
// credential, every agent CLI hangs at its login prompt, and — absent this —
// the only signal is agents quietly ceasing work for hours. Turning that into
// an immediate, queryable Audit Log entry is the whole point.
//
// Gating: each backend's check only fires when at least one configured agent
// uses that backend. A Claude-only hive never alerts on a missing Copilot
// token, and vice versa; gateway/inference-only hives alert on neither.
//
// Safe to start unconditionally at boot: like StartAgentTokenRefresh it tracks
// live state each tick rather than a boot-time snapshot, so a hive that gains
// or loses a backend via config reload is evaluated correctly.
func (m *Manager) StartCredentialWatchdog(ctx context.Context) {
	ticker := time.NewTicker(credentialWatchdogInterval())
	defer ticker.Stop()
	// Per-backend transition tracker so we log/audit on TRANSITIONS (usable ->
	// unusable and back) rather than every tick — a standing condition would
	// otherwise flood the Audit Log at the tick rate.
	lastUnusable := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, w := range credentialWatches() {
				m.evalCredentialWatch(w, lastUnusable)
			}
		}
	}
}

// evalCredentialWatch runs one backend's probe (only if that backend is in
// use) and emits a transition-edge log + audit event. lastUnusable carries the
// previous observation per backend across ticks.
func (m *Manager) evalCredentialWatch(w credentialWatch, lastUnusable map[string]bool) {
	if !m.backendInUse(w.backend) {
		// Not in use: nothing to watch. Reset the tracker so a later config
		// change that adds this backend with a bad credential alerts on its
		// first miss rather than being masked as "no transition".
		delete(lastUnusable, w.backend)
		return
	}
	res := w.probe(w.path)
	unusable := !res.ok
	if unusable && !lastUnusable[w.backend] {
		recovery := res.recovery
		if recovery == "" {
			recovery = defaultCredentialRecovery
		}
		args := []any{
			"backend", w.backend,
			"path", w.path,
			"reason", res.reason,
			"impact", "agents on this backend hang at login; new pods cannot start work",
			"recovery", recovery,
		}
		m.logger.Warn("credential watchdog: durable credential unusable", append(args, res.fields...)...)
		m.audit(w.auditAction, "", auditFields(
			"outcome", res.reason,
			"backend", w.backend,
			"path", w.path,
			"trigger", "watchdog",
		))
	} else if !unusable && lastUnusable[w.backend] {
		m.logger.Info("credential watchdog: durable credential restored",
			"backend", w.backend, "path", w.path)
	}
	lastUnusable[w.backend] = unusable
}
