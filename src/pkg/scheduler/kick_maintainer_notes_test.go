package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/maintainernotes"
)

// Dashboard maintainer notes ride the kick for the items it lists, under
// one "# Maintainer notes" header, capped per item; no lookup or no notes
// adds nothing.
func TestKickMaintainerNotesInjectedAndCapped(t *testing.T) {
	s := workTrackerScheduler(t, "linear", "BODY\n${ISSUE_LIST}\n${PR_LIST}")
	issues := []github.Issue{
		{SourceType: "linear", Repo: "acme/widget", ExternalID: "ENG-42", Number: 42, Title: "noted one"},
		{SourceType: "linear", Repo: "acme/widget", ExternalID: "ENG-43", Number: 43, Title: "quiet one"},
	}
	msg := s.BuildAgentMessage("quality", issues, &github.ActionableResult{})
	if strings.Contains(msg, maintainerNotesKickHeader) {
		t.Fatalf("no lookup must add no notes block:\n%s", msg)
	}

	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	s.SetMaintainerNotesLookup(func(repo string, number int) []maintainernotes.Note {
		if repo != "acme/widget" || number != 42 {
			return nil
		}
		return []maintainernotes.Note{
			{Author: "dave", Text: strings.Repeat("long context ", 200), CreatedAt: at.Add(-time.Hour)},
			{Author: "alice", Text: "owner wants this after the v6 cut", CreatedAt: at},
			{Author: "bob", Text: "waiting on legal", CreatedAt: at.Add(time.Hour)},
		}
	})
	msg = s.BuildAgentMessage("quality", issues, &github.ActionableResult{})
	i := strings.Index(msg, maintainerNotesKickHeader)
	if i < 0 || strings.Count(msg, maintainerNotesKickHeader) != 1 {
		t.Fatalf("notes block must appear exactly once:\n%s", msg)
	}
	block := msg[i:]
	if end := strings.Index(block, "\n\n"); end >= 0 {
		// The block runs to the next blank-line-separated seam; the
		// per-item section itself has one blank line after the header.
		rest := block[end+2:]
		if j := strings.Index(rest, "\n\n"); j >= 0 {
			block = block[:end+2+j]
		}
	}
	if !strings.Contains(block, "## acme/widget#42") || !strings.Contains(block, "- @alice (2026-11-08): owner wants this after the v6 cut") {
		t.Fatalf("noted item missing:\n%s", block)
	}
	if strings.Contains(block, "acme/widget#43") {
		t.Fatalf("an item without notes must not be listed:\n%s", block)
	}
	// Newest first; the oldest, oversized note is cut at the cap rather
	// than typed whole.
	if !(strings.Index(block, "@bob") < strings.Index(block, "@alice") && strings.Index(block, "@alice") < strings.Index(block, "@dave")) {
		t.Fatalf("notes must run newest first:\n%s", block)
	}
	if strings.Contains(block, strings.Repeat("long context ", 200)) || !strings.Contains(block, "…") {
		t.Fatalf("note over the cap must be elided with an ellipsis:\n%s", block)
	}
	if len(block) > maintainernotes.KickSectionCap+256 {
		t.Fatalf("per-item block %d bytes is not capped near %d", len(block), maintainernotes.KickSectionCap)
	}
	if !strings.Contains(block, "not on GitHub") {
		t.Fatalf("block must tell the agent the notes are private:\n%s", block)
	}
}
