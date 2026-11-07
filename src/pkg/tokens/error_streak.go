package tokens

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Per-agent model-call error streaks (#5577; the demotion-signal half of
// #5338).
//
// The signal: an agent whose CLI keeps RUNNING turns while EVERY model call
// dies produces no usage and no output, yet reports green — the bobshell
// opus-4-6 crash loop shape, where the hive's only write-capable agent was
// dead for 4+ days behind a generic "no write" chip. The strongest artifact
// the spoke already records for this is the bobshell chat recording itself:
// every kick appends a user turn, and a SUCCESSFUL call appends an assistant
// reply carrying a usage block (tokens.input/output) — so a run of trailing
// user turns with no usage-bearing reply IS the consecutive-failure streak,
// with per-turn granularity and per-agent attribution via the same
// trustedFolders projectHash mapping the token scanner uses.
//
// Why not the alternatives:
//   - .bob-errors/ logs are direct evidence but free-form text the spoke has
//     never parsed; counting lines conflates one multi-line stack trace with
//     N failures, and the format is version-dependent.
//   - CLI exit tracking misses this class entirely — the CLI stays up and the
//     agent stays green while every call inside it dies (#5338's exact shape).

const (
	// errorStreakGrace ignores the newest UNANSWERED turn(s) younger than
	// this: a kick delivered moments ago legitimately has no reply yet, and
	// counting it would flag every agent mid-turn. An assistant reply that
	// explicitly recorded zero usage is a completed failure and is counted
	// regardless of age.
	errorStreakGrace = 10 * time.Minute

	// errorStreakMaxAge bounds how stale a streak may be and still be
	// reported: if the newest failed turn is older than this, the agent is
	// simply idle (or was fixed) and the streak is history, not a live fault.
	errorStreakMaxAge = 24 * time.Hour

	// maxErrorStreakPerAgent caps the reported count. The hub only needs
	// "N consecutive ≥ threshold"; an unbounded number from a week-long crash
	// loop adds nothing but payload risk.
	maxErrorStreakPerAgent = 10_000
)

// bobTurn is one user-initiated model turn reconstructed from a chat
// recording: when it happened and whether a usage-bearing assistant reply
// ever answered it.
type bobTurn struct {
	at time.Time
	// answered is true once ANY assistant reply followed the user turn
	// (successful or not); success is true only when that reply carried
	// real token usage — the proof the model call actually completed.
	answered bool
	success  bool
}

// BobAgentErrorStreaks scans bobshell chat recordings under bobHomeDir
// (the same tmp/*/chats/*.json files the token scanner reads) and returns,
// per agent, the count of CONSECUTIVE most-recent turns whose model call
// produced no usage — either an assistant reply that recorded zero tokens,
// or a user turn (older than errorStreakGrace) that never received a reply
// at all, which is what a call that crashes before responding leaves behind.
//
// Agents whose recordings never carry an explicit usage block anywhere in the
// window are skipped: an estimation-era format that records no usage for
// SUCCESSFUL calls either would make every turn look failed. Streaks whose
// newest failure is older than errorStreakMaxAge are dropped as stale. The
// result is always non-nil (an empty map means "measured, no streaks") so
// callers can tell a clean fleet from a scan that never ran.
func BobAgentErrorStreaks(bobHomeDir string, now time.Time) map[string]int {
	return BobAgentErrorStreaksForTree(bobHomeDir, "", now)
}

// BobAgentErrorStreaksForTree is BobAgentErrorStreaks for one tree whose
// owner is already known (see ScanBobSessionTree).
func BobAgentErrorStreaksForTree(bobHomeDir, defaultAgent string, now time.Time) map[string]int {
	out := make(map[string]int)
	if bobHomeDir == "" {
		return out
	}

	matches, err := filepath.Glob(filepath.Join(bobHomeDir, "tmp", "*", "chats", "*.json"))
	if err != nil {
		return out
	}
	agentsByHash := bobTrustedAgentsByProjectHash(bobHomeDir)
	cutoff := now.Add(-maxBobSessionAge)

	type agentTrace struct {
		turns []bobTurn
		// usageCapable is true once any message in any of this agent's
		// sessions carried an explicit usage structure (even all-zero):
		// the format proves usage WOULD be recorded on success.
		usageCapable bool
	}
	traces := make(map[string]*agentTrace)

	// Sessions sort by first-activity so cross-session turn order is
	// chronological per agent (one agent's sessions are sequential in
	// practice; recordings within a file are already ordered).
	type sessRec struct {
		agent    string
		start    int64
		turns    []bobTurn
		hasUsage bool
	}
	var sessions []sessRec

	for _, path := range matches {
		sess, err := parseBobChatFile(path)
		if err != nil || sess == nil || len(sess.Messages) == 0 {
			continue
		}
		last := bobSessionLastActive(sess)
		if last > 0 && time.UnixMilli(last).Before(cutoff) {
			continue
		}
		fallbackTS := time.UnixMilli(last)

		projectHash := strings.TrimSpace(sess.ProjectHash)
		if projectHash == "" {
			projectHash = extractBobProjectHash(path)
		}
		agentName := bobAgentName(projectHash, agentsByHash, defaultAgent)

		rec := sessRec{agent: agentName, start: bobSessionFirstActive(sess)}
		for i := range sess.Messages {
			msg := &sess.Messages[i]
			ts := fallbackTS
			if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(msg.Timestamp)); err == nil {
				ts = t
			}
			switch msg.effectiveType() {
			case "user":
				rec.turns = append(rec.turns, bobTurn{at: ts})
			case "bob-shell", "assistant":
				if msg.Tokens != nil || msg.Usage != nil {
					rec.hasUsage = true
				}
				in, out2, _ := msg.effectiveTokensReal()
				if len(rec.turns) > 0 {
					t := &rec.turns[len(rec.turns)-1]
					t.answered = true
					if in > 0 || out2 > 0 {
						t.success = true
					}
					// A reply's own timestamp is the better "when did this
					// turn conclude" marker than the kick's.
					if !ts.IsZero() {
						t.at = ts
					}
				}
			}
		}
		if len(rec.turns) > 0 {
			sessions = append(sessions, rec)
		}
	}

	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].start < sessions[j].start })
	for _, rec := range sessions {
		tr := traces[rec.agent]
		if tr == nil {
			tr = &agentTrace{}
			traces[rec.agent] = tr
		}
		tr.turns = append(tr.turns, rec.turns...)
		tr.usageCapable = tr.usageCapable || rec.hasUsage
	}

	for agentName, tr := range traces {
		if !tr.usageCapable {
			continue
		}
		streak, newest := trailingFailureStreak(tr.turns, now)
		if streak == 0 {
			continue
		}
		if now.Sub(newest) > errorStreakMaxAge {
			continue
		}
		if streak > maxErrorStreakPerAgent {
			streak = maxErrorStreakPerAgent
		}
		out[agentName] = streak
	}
	return out
}

// trailingFailureStreak counts consecutive FAILED turns from the tail of the
// timeline, stopping at the first success. Unanswered turns younger than
// errorStreakGrace are skipped (still in flight, not evidence either way).
// Returns the streak and the timestamp of its newest counted failure.
func trailingFailureStreak(turns []bobTurn, now time.Time) (int, time.Time) {
	streak := 0
	var newest time.Time
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.success {
			break
		}
		if !t.answered && now.Sub(t.at) < errorStreakGrace {
			continue // in-flight turn: not a failure yet
		}
		streak++
		if t.at.After(newest) {
			newest = t.at
		}
	}
	return streak, newest
}
