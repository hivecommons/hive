package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/prstatus"
)

// Quiet mode: the post-merge evidence pointer is a section of the PR's
// status comment. With a status comment already there, nothing is created;
// a replayed merge finds the marker inside it and neither edits nor posts.
func TestRecordReviewEvidenceMergeQuietFoldsIntoStatusComment(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	root := withEvidenceRoot(t)
	writeTestEvidence(t, root, "h1", "", t0)
	f := evidenceMergeFixture{head: "h1", comments: []string{prstatus.Render(nil)}}
	c := evidenceMergeClient(t, &f, &ReviewEvidenceSettings{Enabled: true})
	c.SetNotificationsQuiet(func() bool { return true })

	c.RecordPRMergedAudit("o/r", 7, "", "m1", PRAuditPathSweep)
	if f.posts != 0 || f.edits != 1 {
		t.Fatalf("posts=%d edits=%d, want the status comment edited and nothing created", f.posts, f.edits)
	}
	section, ok := prstatus.Get(f.comments[0], prstatus.KeyEvidence)
	if !ok || !strings.Contains(section, ReviewEvidenceMarker) || !strings.Contains(section, "Evidence bundle") {
		t.Fatalf("evidence section = %q, %v", section, ok)
	}
	_, _, suppressed := c.NotificationCounts()
	if suppressed != 1 {
		t.Fatalf("suppressed=%d, want 1", suppressed)
	}

	c.RecordPRMergedAudit("o/r", 7, "", "m1", PRAuditPathSweep)
	if f.posts != 0 || f.edits != 1 {
		t.Fatalf("replay: posts=%d edits=%d, want no further write", f.posts, f.edits)
	}
}

// Quiet mode without a status comment yet: the one comment created is the
// status comment, carrying the evidence section. Normal mode posts the
// standalone marker comment as before.
func TestRecordReviewEvidenceMergeQuietCreatesStatusCommentOnce(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, quiet := range []bool{true, false} {
		root := withEvidenceRoot(t)
		writeTestEvidence(t, root, "h1", "", t0)
		f := evidenceMergeFixture{head: "h1"}
		c := evidenceMergeClient(t, &f, &ReviewEvidenceSettings{Enabled: true})
		c.SetNotificationsQuiet(func() bool { return quiet })
		c.RecordPRMergedAudit("o/r", 7, "", "m1", PRAuditPathSweep)
		if f.posts != 1 || len(f.comments) != 1 {
			t.Fatalf("quiet=%v: posts=%d comments=%d, want exactly one", quiet, f.posts, len(f.comments))
		}
		if prstatus.IsStatusComment(f.comments[0]) != quiet {
			t.Fatalf("quiet=%v: comment %q", quiet, f.comments[0])
		}
		if !strings.Contains(f.comments[0], ReviewEvidenceMarker) {
			t.Fatalf("quiet=%v: marker missing from %q", quiet, f.comments[0])
		}
	}
}

// An unblocked note is posted once per cause: the second sweep that sees
// the same issue and the same settled PR finds the note and posts nothing
// (hivecommons/hive#15536 ×2 within a minute). In quiet mode the note is a
// section of the issue's status comment, so a second, different cause edits
// that comment rather than creating another.
func TestPostUnblockedNoteIdempotentPerCause(t *testing.T) {
	note := blockedUnblockedMarker + "\nUnblocked: o/r#9 is no longer open. This issue is actionable again."
	other := blockedUnblockedMarker + "\nUnblocked: o/r#12 is no longer open. This issue is actionable again."
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctx := context.Background()

	t.Run("normal", func(t *testing.T) {
		s := &statusCommentServer{}
		c := testClient(t, s.start(t).URL)
		c.postUnblockedNote(ctx, "o/r", 5, note, logger)
		c.postUnblockedNote(ctx, "o/r", 5, note, logger)
		if s.creates != 1 || s.edits != 0 {
			t.Fatalf("creates=%d edits=%d, want one comment", s.creates, s.edits)
		}
		if _, _, suppressed := c.NotificationCounts(); suppressed != 1 {
			t.Fatalf("suppressed=%d, want 1", suppressed)
		}
	})
	t.Run("quiet", func(t *testing.T) {
		s := &statusCommentServer{}
		c := testClient(t, s.start(t).URL)
		c.SetNotificationsQuiet(func() bool { return true })
		c.postUnblockedNote(ctx, "o/r", 5, note, logger)
		c.postUnblockedNote(ctx, "o/r", 5, note, logger)
		c.postUnblockedNote(ctx, "o/r", 5, other, logger)
		if s.creates != 1 || s.edits != 1 {
			t.Fatalf("creates=%d edits=%d, want one status comment, edited once", s.creates, s.edits)
		}
		body, _ := s.comments[0]["body"].(string)
		section, ok := prstatus.Get(body, prstatus.KeyUnblocked)
		if !ok || !strings.Contains(section, "o/r#12") || strings.Contains(section, "o/r#9") {
			t.Fatalf("unblocked section = %q, %v", section, ok)
		}
	})
	t.Run("issue being closed by the merge", func(t *testing.T) {
		s := &statusCommentServer{comments: []map[string]any{{"id": 1, "body": CloseOnMergeMarker + "\nFixed by #9 (merged abc). Closing."}}}
		c := testClient(t, s.start(t).URL)
		c.postUnblockedNote(ctx, "o/r", 5, note, logger)
		if s.creates != 0 || s.edits != 0 {
			t.Fatalf("creates=%d edits=%d, want nothing on an issue the merge closes", s.creates, s.edits)
		}
	})
}

// Relayed agent status notes fold into the status comment in quiet mode;
// anything with content of its own is still a comment.
func TestRelayStatusNoteKey(t *testing.T) {
	tests := map[string]string{
		"hive/awaiting-ci: CI pending — sweep will merge when green.":                 prstatus.KeyAgentNotePrefix + agentNoteAwaitingCI,
		"  hive/awaiting-ci: held for human review. Green CI will not merge this.":    prstatus.KeyAgentNotePrefix + agentNoteAwaitingCI,
		"Parking on stage/045-needs-decision: Risk: schema change\n\nDetails follow.": prstatus.KeyAgentNotePrefix + agentNoteParking,
		"Already fixed by #14 (merged). Closing as not planned.":                      prstatus.KeyAgentNotePrefix + agentNoteAlreadyFixed,
		"Verified: the bug reproduces on v5 at abc123.":                               "",
		"": "",
	}
	for body, want := range tests {
		if got := relayStatusNoteKey(body); got != want {
			t.Errorf("relayStatusNoteKey(%q) = %q, want %q", body, got, want)
		}
	}
}

// The relay comment path in quiet mode: an awaiting-ci note becomes a
// status section; a second one for a new head edits it.
func TestPostNoteOrFoldQuietEditsNormalCreates(t *testing.T) {
	ctx := context.Background()
	s := &statusCommentServer{}
	c := testClient(t, s.start(t).URL)
	c.SetNotificationsQuiet(func() bool { return true })
	key := prstatus.KeyAgentNotePrefix + agentNoteAwaitingCI
	if created, err := c.PostNoteOrFold(ctx, "o/r", 3, key, "hive/awaiting-ci: CI pending — sweep will merge when green."); err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if created, err := c.PostNoteOrFold(ctx, "o/r", 3, key, "hive/awaiting-ci: held for human review."); err != nil || created {
		t.Fatalf("second: created=%v err=%v", created, err)
	}
	if s.creates != 1 || s.edits != 1 {
		t.Fatalf("creates=%d edits=%d", s.creates, s.edits)
	}
	created, edited, suppressed := c.NotificationCounts()
	if created != 1 || edited != 1 || suppressed != 1 {
		t.Fatalf("counts created=%d edited=%d suppressed=%d", created, edited, suppressed)
	}

	s2 := &statusCommentServer{}
	c2 := testClient(t, s2.start(t).URL)
	for i := 0; i < 2; i++ {
		if _, err := c2.PostNoteOrFold(ctx, "o/r", 3, key, "hive/awaiting-ci: CI pending"); err != nil {
			t.Fatal(err)
		}
	}
	if s2.creates != 2 || s2.edits != 0 {
		t.Fatalf("normal mode: creates=%d edits=%d, want two comments", s2.creates, s2.edits)
	}
}

// The hourly line: the first bump logs the baseline, bumps within the hour
// stay silent, the first bump after an hour logs again with the totals.
func TestNotificationTallyLogsHourly(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	orig := notificationTallyNow
	notificationTallyNow = func() time.Time { return now }
	t.Cleanup(func() { notificationTallyNow = orig })

	var buf bytes.Buffer
	c := &Client{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	c.noteCommentCreated()
	c.noteCommentEdited()
	c.noteCommentSuppressed()
	if n := strings.Count(buf.String(), "github notifications"); n != 1 {
		t.Fatalf("log lines within the hour = %d, want 1:\n%s", n, buf.String())
	}
	now = now.Add(notificationTallyLogEvery)
	c.noteCommentSuppressed()
	out := buf.String()
	if n := strings.Count(out, "github notifications"); n != 2 {
		t.Fatalf("log lines after an hour = %d, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "created=1 edited=1 suppressed=2") {
		t.Fatalf("totals missing:\n%s", out)
	}
}

// The issue-stage lane already left its "Fixed by #42" closing note; the
// close-on-merge sweep closes the issue without a second one
// (hivecommons/hive#15727 vs #15843).
func TestCloseOnMergeSkipsSecondClosingComment(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	var patched bool
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "state": "open", "labels": []map[string]string{{"name": CoveredByPRLabel}}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"body": "covered by PR #42"},
				{"body": mergedPRClosedStageMarkerFor(42) + "\nFixed by #42 (merged). Closing because Hive's PR claim metadata says this PR was the full fix."},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "labels": []map[string]string{}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "unrelated", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/other", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			posts++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			patched = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	results := c.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}})
	if len(results) != 1 || results[0].Action != CloseOnMergeClosed {
		t.Fatalf("results = %+v, want issue closed", results)
	}
	if !patched || posts != 0 {
		t.Fatalf("patched=%v posts=%d, want closed with no second closing comment", patched, posts)
	}
}
