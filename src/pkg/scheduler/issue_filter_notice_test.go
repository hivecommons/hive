package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The issue-filter NOTICE is prompt text only — enforcement lives at
// enumeration (pkg/github fetchIssues), tested there. These tests pin that
// kick prompts state the active policy so agents are never told to go look at
// excluded issues, and that an unconfigured hive's prompts are unchanged.

func newSchedulerWithFilter(f config.IssueFilterConfig) *Scheduler {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "test-org",
			Repos:       []string{"test-org/common"},
			IssueFilter: f,
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func TestScannerMessage_IssueFilterNoticePresent(t *testing.T) {
	s := newSchedulerWithFilter(config.IssueFilterConfig{
		RequireLabels: []string{"approved-for-agents"},
	})
	msg := s.buildScannerMessage(nil, emptyActionable())
	if !strings.Contains(msg, "ISSUE FILTER") {
		t.Errorf("scanner kick missing issue-filter notice:\n%s", msg)
	}
	if !strings.Contains(msg, "approved-for-agents") {
		t.Errorf("scanner kick does not name the required label:\n%s", msg)
	}
}

// TestScannerMessage_NoFilterNoNotice is the regression pin: an unconfigured
// hive's kick prompt carries no filter text at all.
func TestScannerMessage_NoFilterNoNotice(t *testing.T) {
	s := newScheduler()
	msg := s.buildScannerMessage(nil, emptyActionable())
	if strings.Contains(msg, "ISSUE FILTER") {
		t.Errorf("unconfigured hive's scanner kick mentions an issue filter:\n%s", msg)
	}
}

// TestFormatIssueList_CarriesNotice: templates render issues via
// ${ISSUE_LIST}; the notice must ride it so template-driven kicks state the
// policy too — including when the filter leaves the list empty, which is
// exactly when an agent is most tempted to go find work by itself.
func TestFormatIssueList_CarriesNotice(t *testing.T) {
	s := newSchedulerWithFilter(config.IssueFilterConfig{
		RequireLabels: []string{"approved-for-agents"},
	})
	out := s.formatIssueList(nil)
	if !strings.Contains(out, "ISSUE FILTER") {
		t.Errorf("empty ${ISSUE_LIST} missing issue-filter notice: %q", out)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("empty ${ISSUE_LIST} lost its (none) marker: %q", out)
	}

	// Unconfigured: exact legacy output, byte for byte.
	legacy := newScheduler().formatIssueList(nil)
	if legacy != "(none)" {
		t.Errorf("unconfigured empty ${ISSUE_LIST} changed: %q, want %q", legacy, "(none)")
	}
}
