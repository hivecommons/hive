package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

type recordingFixAuditor struct {
	entries []string
}

func (r *recordingFixAuditor) AuditLog(user, action, detail, agent string) {
	r.entries = append(r.entries, user+"|"+action+"|"+detail+"|"+agent)
}

// TestAuditWithheldReviewFixes: every fix the planner refused this cycle
// lands on the audit trail naming the PR, its author and the setting that
// would have allowed the push (hivecommons/hive#8421).
func TestAuditWithheldReviewFixes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plan := review.DispatchPlan{WithheldFixes: []review.WithheldFix{
		{Repo: "acme/chairlift", Number: 302, HeadSHA: "6e12c4d", Author: "contributor", Setting: review.WithheldFixSetting, Reason: "PR was not opened by a hive agent; review published, fix not pushed", Withheld: time.Now().UTC()},
		{Repo: "acme/dakota-iso", Number: 217, HeadSHA: "9253c5d", Author: "maintainer", Setting: review.WithheldFixSetting, Withheld: time.Now().UTC()},
	}}
	auditor := &recordingFixAuditor{}
	auditWithheldReviewFixes(plan, auditor, logger)

	if len(auditor.entries) != 2 {
		t.Fatalf("got %d audit entries, want 2: %v", len(auditor.entries), auditor.entries)
	}
	first := auditor.entries[0]
	for _, want := range []string{"governor|review_fix_withheld|", "pr=acme/chairlift#302", "author=contributor", "setting=review.fix_human_prs=false", "head=6e12c4d"} {
		if !strings.Contains(first, want) {
			t.Fatalf("audit entry lacks %q: %s", want, first)
		}
	}
	if !strings.Contains(auditor.entries[1], "pr=acme/dakota-iso#217") || !strings.Contains(auditor.entries[1], "author=maintainer") {
		t.Fatalf("second audit entry wrong: %s", auditor.entries[1])
	}
}

func TestAuditWithheldReviewFixesNoEntriesNoAuditor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auditor := &recordingFixAuditor{}
	auditWithheldReviewFixes(review.DispatchPlan{}, auditor, logger)
	if len(auditor.entries) != 0 {
		t.Fatalf("audited with nothing withheld: %v", auditor.entries)
	}
	// A nil auditor (no dashboard) must not panic; the log line still fires.
	auditWithheldReviewFixes(review.DispatchPlan{WithheldFixes: []review.WithheldFix{{Repo: "acme/x", Number: 1, Setting: review.WithheldFixSetting}}}, nil, logger)
}
