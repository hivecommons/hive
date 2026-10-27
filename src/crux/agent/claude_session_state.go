package agent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// claudeSessionState classifies $HOME/.claude.json for one agent.
type claudeSessionState int

const (
	// claudeSessionUnknown — not classified (not a claude agent, no resolvable
	// home, or the file could not be parsed). Never used to make a claim.
	claudeSessionUnknown claudeSessionState = iota

	// claudeSessionAbsent — no .claude.json at all. Ordinary for an agent that
	// has genuinely never run; a restart CAN help here, because the CLI writes
	// a fresh one and completes onboarding against a valid token.
	claudeSessionAbsent

	// claudeSessionUnreadable — the file exists but this process cannot read
	// it (permission). Distinct from absent: something IS there.
	claudeSessionUnreadable

	// claudeSessionSkeleton — the file parses but carries NO signed-in identity
	// (no oauthAccount). This is #4596's signature when it coexists with a
	// valid credential: the login state was overwritten, not lost with the
	// token. Restarting cannot fix it.
	claudeSessionSkeleton

	// claudeSessionSignedIn — the file carries an oauthAccount, i.e. the CLI
	// should consider itself signed in.
	claudeSessionSignedIn
)

// String renders the state for logs and operator-facing messages.
func (s claudeSessionState) String() string {
	switch s {
	case claudeSessionAbsent:
		return "absent"
	case claudeSessionUnreadable:
		return "unreadable"
	case claudeSessionSkeleton:
		return "no-signed-in-identity"
	case claudeSessionSignedIn:
		return "signed-in"
	default:
		return "unknown"
	}
}

// claudeSessionStateKeys are the keys a signed-in, onboarded Claude Code writes
// into .claude.json. oauthAccount is the identity itself (accountUuid,
// emailAddress, organizationUuid, …) and is the one that decides whether the
// CLI shows the login menu; hasCompletedOnboarding gates the first-run flow.
// Both are reported so a partially-written file is legible in the log rather
// than collapsing to a single bit.
const (
	claudeOAuthAccountKey = "oauthAccount"
	claudeOnboardingKey   = "hasCompletedOnboarding"
)

// claudeSessionFile returns the .claude.json path for an agent, which lives
// beside (not inside) the .claude directory that holds the credential.
func claudeSessionFile(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// claudeSessionInfo is the observation, kept as data so callers can build a
// message without re-reading the file.
type claudeSessionInfo struct {
	Path              string
	State             claudeSessionState
	HasOAuthAccount   bool
	HasCompletedSetup bool
}

// inspectClaudeSession classifies the .claude.json at path.
//
// Read-only by construction: it opens nothing for write and never repairs what
// it finds (see the file header for why repairing would be actively harmful).
// An unparseable file reports Unknown rather than Skeleton — claiming "the
// login identity is missing" about a file we failed to parse would be a guess,
// and this exists to replace guesses.
func inspectClaudeSession(path string) claudeSessionInfo {
	info := claudeSessionInfo{Path: path, State: claudeSessionUnknown}
	if path == "" {
		return info
	}

	data, err := os.ReadFile(path)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			info.State = claudeSessionAbsent
		case errors.Is(err, fs.ErrPermission):
			info.State = claudeSessionUnreadable
		default:
			info.State = claudeSessionUnknown
		}
		return info
	}
	return classifyClaudeSession(path, data)
}

// classifyClaudeSession is inspectClaudeSession's judgement applied to bytes
// that are already in hand. Split out because the same classification has to
// run over content this process could not open itself — the seeding path reads
// agent-owned files through su-exec (see claude_session_adopt.go) and must
// reach the identical verdict as a direct read.
func classifyClaudeSession(path string, data []byte) claudeSessionInfo {
	info := claudeSessionInfo{Path: path, State: claudeSessionUnknown}

	// Decode into a raw map: .claude.json's full schema is Claude Code's, it
	// changes between versions, and this must not fail closed on a key it has
	// never seen. Only the two keys above are interpreted.
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		return info
	}

	info.HasOAuthAccount = hasNonEmptyJSONValue(parsed[claudeOAuthAccountKey])
	info.HasCompletedSetup = jsonValueIsTrue(parsed[claudeOnboardingKey])
	if info.HasOAuthAccount {
		info.State = claudeSessionSignedIn
	} else {
		info.State = claudeSessionSkeleton
	}
	return info
}

// hasNonEmptyJSONValue reports whether raw is present and carries something
// more than null/{}/"" — a key written back as an empty object is exactly as
// unauthenticated as a key that is absent, and the clobber produces both.
func hasNonEmptyJSONValue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case nil:
		return false
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	case string:
		return t != ""
	default:
		return true
	}
}

// jsonValueIsTrue reports whether raw is the JSON literal true.
func jsonValueIsTrue(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}
