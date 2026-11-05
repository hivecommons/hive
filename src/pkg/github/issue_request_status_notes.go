package github

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/stagelabels"
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

// Awaiting-CI notes on an in-place adopted PR whose head the hive itself
// pushed (projectbluefin/utah#1091): "sweep will merge when green" is only
// true once the stage pass has checked that the push keeps the PR's own
// change (automerge checkAdoptedPushIntent). Until then the note says the
// check is pending; after a refusal it says the sweep will not merge.
const (
	awaitingCIIntentPendingNote = "hive/awaiting-ci: CI pending — the hive first checks that this push keeps the PR's own change; the sweep merges only after that check passes."
	awaitingCIIntentRefusedNote = "hive/awaiting-ci: CI pending, but the sweep will not merge this head: the hive's push undoes this PR's own change (see the hive's note on this PR)."
)

// adoptedAwaitingCINote rewrites an awaiting-ci note on an in-place adopted
// PR whose current head the adoption record has not taken yet (its intent
// check is pending) or has refused. Any other body is returned unchanged; an
// unreadable PR keeps the agent's text.
func (c *Client) adoptedAwaitingCINote(ctx context.Context, repo string, number int, body string) string {
	if relayStatusNoteKey(body) != prstatus.KeyAgentNotePrefix+agentNoteAwaitingCI {
		return body
	}
	owner, name := c.splitRepo(repo)
	rec, ok := stagelabels.Shared().Get(stagelabels.Key(owner+"/"+name, number))
	if !ok || !rec.AdoptedInPlace() {
		return body
	}
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return body
	}
	head := pr.GetHead().GetSHA()
	switch {
	case head == "":
		return body
	case rec.AdoptionEscalated(head) && rec.AdoptEscalatedReason == stagelabels.LaneReasonAdoptionFixerReverts:
		return awaitingCIIntentRefusedNote
	case head != rec.AdoptedHead && rec.YieldedHead != head:
		return awaitingCIIntentPendingNote
	}
	return body
}
