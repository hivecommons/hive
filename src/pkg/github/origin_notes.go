package github

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
)

// OriginNotesMarker tags the one comment per item that collects an agent's
// free-text notes — the relayed `gh pr comment` bodies whose only marker
// is the identity footer's `<!-- hive:origin … -->` (hivecommons/hive#17067).
// Each new note edits that comment in place instead of posting another.
const OriginNotesMarker = "<!-- hive:origin-notes -->"

// originNotesHeading opens the collected-notes comment.
const originNotesHeading = "**Agent notes** — updated in place; edits do not notify. Full history is in the Activity Monitor."

// originNotesKeep caps how many notes the collected comment retains; the
// oldest drop off first.
const originNotesKeep = 10

// originNoteStatusKey is the status-comment section the notes fold into in
// quiet notification mode.
const originNoteStatusKey = prstatus.KeyAgentNotePrefix + "origin"

// originNoteEntry renders one note as a list item. Continuation lines are
// indented so a multi-paragraph note stays inside its bullet.
func originNoteEntry(agent, note string, now time.Time) string {
	head := "- "
	if agent = strings.TrimSpace(agent); agent != "" {
		head += "**" + agent + "** "
	}
	head += "(" + now.UTC().Format("2006-01-02 15:04 UTC") + "): "
	lines := strings.Split(strings.TrimSpace(note), "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return head + strings.Join(lines, "\n")
}

// renderOriginNotes returns the collected-notes comment body with entry
// appended to the notes already in previous (or a fresh comment when
// previous is empty), keeping at most originNotesKeep entries.
func renderOriginNotes(previous, entry string) string {
	var entries []string
	if i := strings.Index(previous, "\n- "); i >= 0 {
		for _, block := range strings.Split(previous[i+1:], "\n- ") {
			block = strings.TrimRight(block, "\n")
			if strings.TrimSpace(block) == "" {
				continue
			}
			entries = append(entries, "- "+strings.TrimPrefix(block, "- "))
		}
	}
	entries = append(entries, entry)
	if len(entries) > originNotesKeep {
		entries = entries[len(entries)-originNotesKeep:]
	}
	return OriginNotesMarker + "\n" + originNotesHeading + "\n\n" + strings.Join(entries, "\n")
}

// postOriginNote delivers an agent's free-text note without a new
// notification per note: in quiet mode it is the agent-note section of the
// item's status comment; otherwise it is appended to the one
// OriginNotesMarker comment on the item, created on first use. Reports
// whether a comment was created.
func (c *Client) postOriginNote(ctx context.Context, repo string, number int, agent, note string, now time.Time) (bool, error) {
	entry := originNoteEntry(agent, note, now)
	if c.notificationsQuiet() {
		return c.UpsertPRStatusSection(ctx, repo, number, originNoteStatusKey, entry)
	}
	res, err := c.EditIssueMarkerComment(ctx, repo, number, OriginNotesMarker, func(previous string) string {
		return renderOriginNotes(previous, entry)
	})
	return res.Created, err
}
