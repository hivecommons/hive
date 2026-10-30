package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
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

// TestAuditWithheldReviewFixesRecordsSettingOn: with fix_human_prs on the
// push is still withheld (hivecommons/hive#12897), and the audit entry must
// say the setting was on rather than claim it was off.
func TestAuditWithheldReviewFixesRecordsSettingOn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plan := review.DispatchPlan{WithheldFixes: []review.WithheldFix{
		{Repo: "acme/chairlift", Number: 303, HeadSHA: "b91fee3", Author: "contributor", Setting: review.WithheldFixSetting, SettingEnabled: true, Withheld: time.Now().UTC()},
	}}
	auditor := &recordingFixAuditor{}
	auditWithheldReviewFixes(plan, auditor, logger)
	if len(auditor.entries) != 1 || !strings.Contains(auditor.entries[0], "setting=review.fix_human_prs=true") {
		t.Fatalf("audit entry must record the setting as on: %v", auditor.entries)
	}
}

func TestAuditWithheldReviewFixesNotHiveAuthoredExclusionIsNotWarn(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	plan := review.DispatchPlan{FixExclusions: []review.FixDispatchExclusion{
		{Repo: "acme/x", Number: 1, HeadSHA: "abc", Author: "human", Reason: review.FixDispatchExcludedNotHiveAuthored},
	}}
	auditWithheldReviewFixes(plan, nil, logger)
	out := buf.String()
	if !strings.Contains(out, "review fix dispatch excluded") || strings.Contains(out, "level=WARN") {
		t.Fatalf("not-hive-authored exclusion must log below WARN, got: %s", out)
	}
}

type levelRecorder struct {
	levels []slog.Level
}

func (r *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *levelRecorder) Handle(_ context.Context, rec slog.Record) error {
	if rec.Message == "review fix dispatch excluded" {
		r.levels = append(r.levels, rec.Level)
	}
	return nil
}
func (r *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *levelRecorder) WithGroup(string) slog.Handler      { return r }

// TestAuditWithheldReviewFixesWarnsOncePerHead: the dispatcher re-derives
// its exclusions every cycle, so the same fix-attempts-exhausted line
// fired every ~5 min for 13h on #15308 (hivecommons/hive#15744). WARN
// once per (pr, head, reason); Debug while it persists; WARN again on a
// new head or after the exclusion cleared and came back.
func TestAuditWithheldReviewFixesWarnsOncePerHead(t *testing.T) {
	reviewFixExcludedWarn = prWarnOnce{}
	t.Cleanup(func() { reviewFixExcludedWarn = prWarnOnce{} })
	rec := &levelRecorder{}
	logger := slog.New(rec)
	exhausted := review.FixDispatchExclusion{Repo: "hivecommons/hive", Number: 15308, HeadSHA: "5b62304", Author: "hivecommons-hive[bot]", Reason: review.FixDispatchExcludedAttemptsExhausted, Attempts: 6}
	plan := review.DispatchPlan{FixExclusions: []review.FixDispatchExclusion{exhausted}}

	auditWithheldReviewFixes(plan, nil, logger)
	auditWithheldReviewFixes(plan, nil, logger)
	auditWithheldReviewFixes(plan, nil, logger)
	if want := []slog.Level{slog.LevelWarn, slog.LevelDebug, slog.LevelDebug}; !slices.Equal(rec.levels, want) {
		t.Fatalf("levels across three cycles = %v, want %v", rec.levels, want)
	}

	moved := exhausted
	moved.HeadSHA = "9f0e1d2"
	rec.levels = nil
	auditWithheldReviewFixes(review.DispatchPlan{FixExclusions: []review.FixDispatchExclusion{moved}}, nil, logger)
	if want := []slog.Level{slog.LevelWarn}; !slices.Equal(rec.levels, want) {
		t.Fatalf("new head levels = %v, want %v", rec.levels, want)
	}

	// Cleared for a cycle, then back: the old key was retained away.
	rec.levels = nil
	auditWithheldReviewFixes(review.DispatchPlan{}, nil, logger)
	auditWithheldReviewFixes(plan, nil, logger)
	if want := []slog.Level{slog.LevelWarn}; !slices.Equal(rec.levels, want) {
		t.Fatalf("recurrence levels = %v, want %v", rec.levels, want)
	}

	// Policy skips stay Info the first time, Debug after.
	rec.levels = nil
	policy := review.DispatchPlan{FixExclusions: []review.FixDispatchExclusion{{Repo: "acme/x", Number: 1, HeadSHA: "a", Reason: review.FixDispatchExcludedNotHiveAuthored}}}
	auditWithheldReviewFixes(policy, nil, logger)
	auditWithheldReviewFixes(policy, nil, logger)
	if want := []slog.Level{slog.LevelInfo, slog.LevelDebug}; !slices.Equal(rec.levels, want) {
		t.Fatalf("policy skip levels = %v, want %v", rec.levels, want)
	}
}
