package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestRepoItemHoldNoteNamesDashboardUserAndIsEditedInPlace: a hold release
// from the dashboard leaves one comment naming the signed-in user (GitHub
// records the Hive App as the label's actor), and the next hold action on
// the same item edits that comment instead of posting another
// (hivecommons/hive#19852).
func TestRepoItemHoldNoteNamesDashboardUserAndIsEditedInPlace(t *testing.T) {
	mock := &repoHoldMock{permission: map[string]string{"writer": "maintain"}, issueLabels: []string{"hold"}}
	srv := newRepoHoldServer(t, mock)

	if w := postRepoHold(t, srv, "writer", false, false); w.Code != http.StatusOK {
		t.Fatalf("release: code=%d body=%s", w.Code, w.Body.String())
	}
	if mock.commentPosts != 1 || mock.commentEdits != 0 {
		t.Fatalf("release: posts=%d edits=%d, want one new comment", mock.commentPosts, mock.commentEdits)
	}
	body := fmt.Sprint(mock.comments[0]["body"])
	if !strings.Contains(body, dashboardActorNoteMarker(dashboardActorKindHold)) ||
		!strings.Contains(body, "Hold released from the Hive dashboard by @writer: `hold` removed") {
		t.Fatalf("release note = %q, want the marker and the dashboard user", body)
	}

	if w := postRepoHold(t, srv, "", true, true); w.Code != http.StatusOK {
		t.Fatalf("owner add: code=%d body=%s", w.Code, w.Body.String())
	}
	if mock.commentPosts != 1 || mock.commentEdits != 1 || len(mock.comments) != 1 {
		t.Fatalf("second click: posts=%d edits=%d comments=%d, want the existing comment edited", mock.commentPosts, mock.commentEdits, len(mock.comments))
	}
	body = fmt.Sprint(mock.comments[0]["body"])
	added := strings.Index(body, "Hold added from the Hive dashboard by the hive owner: `hive-pause/h1` added")
	released := strings.Index(body, "Hold released from the Hive dashboard by @writer")
	if added < 0 || released < 0 || added > released {
		t.Fatalf("edited note = %q, want the add first and the earlier release kept", body)
	}

	off := false
	srv.deps.Config.Dashboard.AttributeLabelActions = &off
	if w := postRepoHold(t, srv, "writer", false, false); w.Code != http.StatusOK {
		t.Fatalf("release with notes off: code=%d body=%s", w.Code, w.Body.String())
	}
	if mock.commentPosts != 1 || mock.commentEdits != 1 {
		t.Fatalf("notes off: posts=%d edits=%d, want no comment write", mock.commentPosts, mock.commentEdits)
	}
	if len(mock.removed) != 2 {
		t.Fatalf("notes off: removed=%v, want the hold still released", mock.removed)
	}
}

// TestApprovalNoteNamesReleasedLabelsOnly: an approval's note lists the
// labels it took off for the person (needs-human, the sentinel label) and
// is skipped when it removed none.
func TestApprovalNoteNamesReleasedLabelsOnly(t *testing.T) {
	mock := &repoHoldMock{}
	srv := newRepoHoldServer(t, mock)
	ctx := context.Background()

	srv.noteApprovalReleased(ctx, "testorg/testrepo", 7, "writer", []string{"approval-recorded", "admitted"})
	if mock.commentPosts != 0 {
		t.Fatalf("approval without a released label posted %d comments, want none", mock.commentPosts)
	}
	srv.noteApprovalReleased(ctx, "testorg/testrepo", 7, "writer", []string{"approval-recorded", "needs-human"})
	if mock.commentPosts != 1 {
		t.Fatalf("approval lifting needs-human: posts=%d, want 1", mock.commentPosts)
	}
	if body := fmt.Sprint(mock.comments[0]["body"]); !strings.Contains(body, "Approved from the Hive dashboard by @writer: `needs-human` removed") {
		t.Fatalf("approval note = %q, want the user and the lifted label", body)
	}
}

func TestRenderDashboardActorNoteKeepsNewestEntries(t *testing.T) {
	marker := dashboardActorNoteMarker(dashboardActorKindHold)
	at := time.Date(2026, 11, 25, 15, 50, 0, 0, time.UTC)
	body := ""
	for i := range dashboardActorNoteMaxEntries + 3 {
		body = renderDashboardActorNote(marker, body, dashboardActorEntry(fmt.Sprintf("Hold %d", i), "writer", "", at))
	}
	if n := strings.Count(body, "\n- "); n != dashboardActorNoteMaxEntries {
		t.Fatalf("entries = %d, want %d:\n%s", n, dashboardActorNoteMaxEntries, body)
	}
	newest := fmt.Sprintf("- Hold %d from the Hive dashboard by @writer (2026-11-25 15:50 UTC).", dashboardActorNoteMaxEntries+2)
	if !strings.HasPrefix(body, marker+"\n") || !strings.Contains(body, newest) || strings.Contains(body, "- Hold 2 ") {
		t.Fatalf("note = %q, want the marker first, the newest entry kept and the oldest dropped", body)
	}
	if got := dashboardActorName("ibmid:5500087VJB"); got != "`ibmid:5500087VJB`" {
		t.Fatalf("hub identity named %q, want it in code without an @-mention", got)
	}
}
