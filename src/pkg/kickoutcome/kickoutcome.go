// Package kickoutcome keeps a durable, bounded record of what each kick of an
// agent came to (hivecommons/hive#18916).
//
// "Kick sent" says the agent was told; it does not say whether a pull request
// opened, a commit was pushed, the agent stood down, the hive refused the kick,
// or the turn ended with nothing visible. The Store records, per kick, who or
// what triggered it, when it started and ended, and the raw signals the hive
// already has about it: the turn-end classification, a kick refusal, a pull
// request opened through the relay, a branch pushed or an item commented on
// through the relay. The outcome is DERIVED from those signals when it is read
// (Derive), so a signal that arrives late (the relay opens a pull request after
// the turn ends) upgrades the outcome, and a kick no signal speaks for is
// reported as ended_no_visible_action, never guessed.
//
// Security model: the store is hive-side only. Agents cannot query it, and no
// text it holds is ever put into an agent prompt. The text an agent wrote (a
// stand-down line, a refusal line) is untrusted: it is redacted for tokens,
// stripped of control characters and length-capped on the way in (Quote), and
// the dashboard escapes it on the way out. Log lines carry ids and kinds only.
package kickoutcome

import (
	"strings"
	"time"
	"unicode"

	"github.com/hivecommons/hive/crux/logscrub"
)

// Outcome kinds: what a kick came to.
const (
	KindOpenedPR             = "opened_pr"
	KindPushedCommit         = "pushed_commit"
	KindCommented            = "commented"
	KindStoodDown            = "stood_down"
	KindRefused              = "refused"
	KindStillRunning         = "still_running"
	KindEndedNoVisibleAction = "ended_no_visible_action"
)

// Trigger kinds: who or what started a kick.
const (
	TriggerDashboard        = "dashboard"
	TriggerGovernor         = "governor"
	TriggerWorkflowDispatch = "workflow_dispatch"
	// TriggerOther is a kick no caller declared (a meeting or inbox prompt, a
	// canary retry): only the runtime's own trigger word is known.
	TriggerOther = "other"
)

// Turn-end kinds the runtime reports (crux.KickOutcome*), mirrored here so the
// package needs no import edge to the runtime.
const (
	turnStandDown = "stand-down"
)

// Bounds. Every record, string and list the store keeps is capped.
const (
	// MaxRecordsPerAgent is how many kicks are kept per agent: the dashboard
	// shows the last ten.
	MaxRecordsPerAgent = 10
	// MaxAgents bounds the agents the store tracks.
	MaxAgents = 256
	// MaxQuoteRunes caps agent-written text kept in a record.
	MaxQuoteRunes = 200
	// MaxActorRunes caps a trigger actor (a login).
	MaxActorRunes = 64
	// MaxRefRunes caps a URL, branch or sha.
	MaxRefRunes = 300
	// ActionGrace is how long after a turn ended a pull request, push or
	// comment is still attributed to it: the relay acts after the turn.
	ActionGrace = 15 * time.Minute
	// RunningCeiling is how long a kick with no turn-end signal is still
	// called running; past it the hive cannot tell and says
	// ended_no_visible_action (a restart forgets turns).
	RunningCeiling = 2 * time.Hour
	// IntentTTL is how long a declared trigger waits for its delivery.
	IntentTTL = 2 * time.Minute
)

// Record is one kick and the raw signals about it. It is what is persisted.
type Record struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	// TriggerKind is one of the Trigger* kinds; TriggerActor the dashboard
	// user's login for a dashboard kick, empty otherwise; TriggerWord the
	// runtime's own trigger word.
	TriggerKind  string    `json:"trigger_kind"`
	TriggerActor string    `json:"trigger_actor,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	// EndedAt and TurnKind are the turn-end signal; TurnReason the agent's
	// own line for it (sanitized).
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	TurnKind   string     `json:"turn_kind,omitempty"`
	TurnReason string     `json:"turn_reason,omitempty"`
	// RefusalCode is a refusal reason code from the kick-refusals docs;
	// RefusalReason the quoted text behind it (sanitized).
	RefusalCode   string `json:"refusal_code,omitempty"`
	RefusalReason string `json:"refusal_reason,omitempty"`
	// PR, push and comment signals, each with the time it was seen.
	PRURL      string     `json:"pr_url,omitempty"`
	PRNumber   int        `json:"pr_number,omitempty"`
	PRAt       *time.Time `json:"pr_at,omitempty"`
	PushBranch string     `json:"push_branch,omitempty"`
	PushSHA    string     `json:"push_sha,omitempty"`
	PushAt     *time.Time `json:"push_at,omitempty"`
	CommentAt  *time.Time `json:"comment_at,omitempty"`
}

// Outcome is what Derive concludes about a record.
type Outcome struct {
	Kind string
	// At is when the outcome happened (the signal's time, else the turn end,
	// else the start).
	At time.Time
	// Quote is the agent's or the hive's own words behind stood_down and
	// refused; empty otherwise. Untrusted.
	Quote string
}

// Derive concludes what a kick came to from its signals alone. latest says it
// is the agent's most recent kick (only that one can still be running). The
// order is the strength of the evidence: a refusal means nothing ran; a pull
// request, push or comment is visible GitHub activity; a stand-down is the
// agent's own statement; a turn that ended with none of those, or a kick that
// can no longer be told apart from one that ended, is ended_no_visible_action.
func Derive(r Record, now time.Time, latest bool) Outcome {
	switch {
	case r.RefusalCode != "":
		return Outcome{Kind: KindRefused, At: r.StartedAt, Quote: r.RefusalReason}
	case r.PRAt != nil:
		return Outcome{Kind: KindOpenedPR, At: *r.PRAt}
	case r.PushAt != nil:
		return Outcome{Kind: KindPushedCommit, At: *r.PushAt}
	case r.CommentAt != nil:
		return Outcome{Kind: KindCommented, At: *r.CommentAt}
	case r.EndedAt != nil && r.TurnKind == turnStandDown:
		return Outcome{Kind: KindStoodDown, At: *r.EndedAt, Quote: r.TurnReason}
	case r.EndedAt != nil:
		return Outcome{Kind: KindEndedNoVisibleAction, At: *r.EndedAt}
	case latest && now.Sub(r.StartedAt) < RunningCeiling:
		return Outcome{Kind: KindStillRunning, At: r.StartedAt}
	}
	return Outcome{Kind: KindEndedNoVisibleAction, At: r.StartedAt}
}

// Quote makes text an agent (or a pane) wrote safe to store and show: tokens
// redacted, control characters dropped, whitespace collapsed, length capped.
// It does not escape for HTML: the dashboard does that where it renders.
func Quote(s string) string {
	s = logscrub.RedactPaneTokens(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case unicode.IsControl(r) || r == ' ' || r == ' ':
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	return capRunes(b.String(), MaxQuoteRunes)
}

// capRunes shortens s to at most n runes, marking a cut with an ellipsis.
func capRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

// cleanRef bounds an id-like string (login, URL, branch, sha) to printable
// text without whitespace.
func cleanRef(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return capRunes(logscrub.RedactPaneTokens(s), n)
}
