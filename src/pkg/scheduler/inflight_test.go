package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestInflight_HeldItemsWithheldAndNoted(t *testing.T) {
	s := workTrackerScheduler(t, "linear", "BODY\n${ISSUE_LIST}\n${PR_LIST}")
	s.SetInflightLookup(func(issue github.Issue) (string, bool) {
		if issue.ExternalID == "ENG-42" {
			return "agent quality via Linear session sess-1", true
		}
		return "", false
	})
	issues := []github.Issue{
		{SourceType: "linear", Repo: "acme/widget", ExternalID: "ENG-42", Title: "held one"},
		{SourceType: "linear", Repo: "acme/widget", ExternalID: "ENG-43", Title: "free one"},
	}
	msg := s.BuildAgentMessage("quality", issues, &github.ActionableResult{})
	if strings.Contains(msg, "held one") {
		t.Errorf("held item leaked into the work list:\n%s", msg)
	}
	if !strings.Contains(msg, "free one") {
		t.Errorf("free item missing from the work list:\n%s", msg)
	}
	if !strings.Contains(msg, inflightNoteHeader) || !strings.Contains(msg, "acme/widget!ENG-42 — agent quality via Linear session sess-1") {
		t.Errorf("in-flight note missing or wrong:\n%s", msg)
	}
	if strings.Count(msg, inflightNoteHeader) != 1 {
		t.Errorf("note should appear once:\n%s", msg)
	}

	// Kick refs follow the same filter.
	kicks := s.BuildKickMessages(&github.ActionableResult{Issues: github.IssueResult{Items: issues}}, []string{"quality"})
	if len(kicks) != 1 {
		t.Fatalf("kicks = %d", len(kicks))
	}
	for _, ref := range kicks[0].IssueRefs {
		if strings.Contains(ref, "ENG-42") {
			t.Errorf("held ref in IssueRefs: %v", kicks[0].IssueRefs)
		}
	}
}

func TestInflight_ExplicitPlacementAndNoLookup(t *testing.T) {
	s := workTrackerScheduler(t, "linear", "HEAD\n${IN_FLIGHT}\nTAIL\n${ISSUE_LIST}")
	issues := []github.Issue{{SourceType: "linear", Repo: "acme/widget", ExternalID: "ENG-42", Title: "x"}}
	// No lookup: nothing held, placeholder resolves empty, no note appended.
	msg := s.BuildAgentMessage("quality", issues, &github.ActionableResult{})
	if strings.Contains(msg, inflightNoteHeader) || strings.Contains(msg, "${IN_FLIGHT}") {
		t.Errorf("unexpected note with no lookup:\n%s", msg)
	}
	s.SetInflightLookup(func(github.Issue) (string, bool) { return "someone", true })
	msg = s.BuildAgentMessage("quality", issues, &github.ActionableResult{})
	if strings.Count(msg, inflightNoteHeader) != 1 || strings.Index(msg, inflightNoteHeader) > strings.Index(msg, "TAIL") {
		t.Errorf("note should be placed once where ${IN_FLIGHT} was:\n%s", msg)
	}
}
