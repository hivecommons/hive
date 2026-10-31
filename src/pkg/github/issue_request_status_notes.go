package github

import (
	"strings"

	"github.com/hivecommons/hive/pkg/prstatus"
)

// Agent status notes the policies tell an agent to leave as a PR/issue
// comment. Each is a one-line state report — the dashboard already shows
// the same state — so in quiet notification mode it is a section of the
// item's status comment rather than an email to every watcher.
const (
	agentNoteAwaitingCI   = "awaiting-ci"   // "hive/awaiting-ci: CI pending — sweep will merge when green."
	agentNoteParking      = "parking"       // "Parking on stage/045-needs-decision: …"
	agentNoteAlreadyFixed = "already-fixed" // "Already fixed by #N …" before a close
)

// relayStatusNoteKey classifies a relayed comment body as one of the agent
// status notes, by the prefix the policies prescribe, or returns "" for a
// comment that carries content of its own (a finding, a triage verdict
// with evidence, a question) and must stay a comment.
func relayStatusNoteKey(body string) string {
	first := strings.ToLower(strings.TrimSpace(firstLine(body)))
	switch {
	case strings.HasPrefix(first, "hive/awaiting-ci"):
		return prstatus.KeyAgentNotePrefix + agentNoteAwaitingCI
	case strings.HasPrefix(first, "parking on "), strings.HasPrefix(first, "parked on "):
		return prstatus.KeyAgentNotePrefix + agentNoteParking
	case strings.HasPrefix(first, "already fixed"):
		return prstatus.KeyAgentNotePrefix + agentNoteAlreadyFixed
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimLeft(s, "\r\n\t ")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
