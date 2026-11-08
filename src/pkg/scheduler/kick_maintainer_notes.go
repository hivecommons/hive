package scheduler

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/maintainernotes"
)

// MaintainerNotesLookup returns the live maintainer notes on repo#number
// (maintainernotes.Store.List).
type MaintainerNotesLookup func(repo string, number int) []maintainernotes.Note

// SetMaintainerNotesLookup installs (or with nil, removes) the notes lookup
// the kick seam reads.
func (s *Scheduler) SetMaintainerNotesLookup(fn MaintainerNotesLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maintainerNotes = fn
}

// maintainerNotesKickHeader opens the seam; the seam is skipped when the
// kick already carries it.
const maintainerNotesKickHeader = maintainernotes.KickSectionHeader

// addMaintainerNotes appends, per kicked issue that has any, the notes its
// maintainers left on the dashboard (maintainernotes.KickSection, capped at
// maintainernotes.KickSectionCap bytes per item). Private context: the
// block tells the agent not to quote it on GitHub. Nothing is appended when
// no lookup is installed or no listed item has a note.
func (s *Scheduler) addMaintainerNotes(message string, issues []github.Issue) string {
	if message == "" || strings.Contains(message, maintainerNotesKickHeader) {
		return message
	}
	s.mu.RLock()
	fn := s.maintainerNotes
	s.mu.RUnlock()
	if fn == nil {
		return message
	}
	var b strings.Builder
	for _, issue := range issues {
		notes := fn(issue.Repo, issue.Number)
		if len(notes) == 0 {
			continue
		}
		section := maintainernotes.KickSection(notes, maintainernotes.KickSectionCap)
		if section == "" {
			continue
		}
		// One header for the block; each item's notes under its own ref.
		if b.Len() == 0 {
			b.WriteString(maintainerNotesKickHeader + "\n")
		}
		b.WriteString("\n## " + issueDisplayRef(issue) + "\n")
		b.WriteString(strings.TrimPrefix(section, maintainerNotesKickHeader+"\n"))
	}
	if b.Len() == 0 {
		return message
	}
	return appendKickSection(message, b.String())
}
