package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// ---------------------------------------------------------------------------
// buildScannerMessage — stale-draft section (kubestellar/hive#3963)
// ---------------------------------------------------------------------------

// actionableWithStaleDrafts returns an ActionableResult whose PRs carry the
// given StaleDrafts. Items stays empty: a draft is never actionable work, it
// only appears in the dedicated stale-draft section.
func actionableWithStaleDrafts(drafts []github.PullRequest) *github.ActionableResult {
	return &github.ActionableResult{
		Issues: github.IssueResult{},
		PRs: github.PRResult{
			StaleDrafts: drafts,
		},
		Hold: github.HoldResult{},
	}
}

func TestScannerMessage_StaleDraftsListed(t *testing.T) {
	s := newScheduler()
	drafts := []github.PullRequest{
		{Repo: "test-org/console", Number: 42, Title: "feat: half-finished thing", Draft: true},
		{Repo: "test-org/hive", Number: 7, Title: "wip: export adapters", Draft: true},
	}
	msg := s.buildScannerMessage(nil, actionableWithStaleDrafts(drafts))

	if !strings.Contains(msg, "YOUR STALE DRAFT PRs (2, >48h old — finish, mark ready, or close):") {
		t.Errorf("missing stale-draft header with count, got:\n%s", msg)
	}
	if !strings.Contains(msg, "test-org/console#42 feat: half-finished thing") {
		t.Errorf("missing first stale draft line, got:\n%s", msg)
	}
	if !strings.Contains(msg, "test-org/hive#7 wip: export adapters") {
		t.Errorf("missing second stale draft line, got:\n%s", msg)
	}
}

// TestScannerMessage_NoStaleDraftsNoSection is the negative control: with no
// stale drafts the section must be absent entirely, so the scanner is never
// shown an empty nag list to act on.
func TestScannerMessage_NoStaleDraftsNoSection(t *testing.T) {
	s := newScheduler()
	msg := s.buildScannerMessage(nil, emptyActionable())
	if strings.Contains(msg, "STALE DRAFT") {
		t.Errorf("stale-draft section must be absent when there are none, got:\n%s", msg)
	}
}

func TestScannerMessage_StaleDraftTitleTruncated(t *testing.T) {
	s := newScheduler()
	// 80 runes of multi-byte text: truncation must count runes, not bytes.
	longTitle := strings.Repeat("é", 80)
	drafts := []github.PullRequest{
		{Repo: "test-org/hive", Number: 9, Title: longTitle, Draft: true},
	}
	msg := s.buildScannerMessage(nil, actionableWithStaleDrafts(drafts))

	want := "test-org/hive#9 " + strings.Repeat("é", 70) + "\n"
	if !strings.Contains(msg, want) {
		t.Errorf("expected title truncated to 70 runes, got:\n%s", msg)
	}
	if strings.Contains(msg, strings.Repeat("é", 71)) {
		t.Errorf("title not truncated: found 71+ runes of the original title")
	}
}
