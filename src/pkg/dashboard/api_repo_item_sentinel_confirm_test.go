package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The human-gate pill's "Confirm safe" (hivecommons/hive#17470): a person
// confirms a sentinel finding from the dashboard — the confirmation is
// commented first, then the sentinel label comes off — behind the Hold
// authorisation, attributed to the dashboard user, idempotent when the
// label is already gone.

func postSentinelConfirm(t *testing.T, srv *Server, user, body string, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/7/sentinel-confirm", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if owner {
		markOwnerRequest(req)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestRepoItemSentinelConfirmAuthorizationCommentAndLabel(t *testing.T) {
	mock := &repoItemDecisionMock{issueLabels: []string{"stage/085-needs-decision", config.DefaultSentinelLabel, "lgtm"}}
	srv := newRepoItemDecisionServer(t, mock)
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"testrepo"}

	if w := postSentinelConfirm(t, srv, "reader", `{}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if w := postSentinelConfirm(t, srv, "", `{}`, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: code=%d want 401 body=%s", w.Code, w.Body.String())
	}
	if w := postSentinelConfirm(t, srv, "writer", `{"comment":"`+strings.Repeat("x", sentinelConfirmMaxComment+1)+`"}`, false); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized comment: code=%d want 400 body=%s", w.Code, w.Body.String())
	}
	if len(mock.comments)+len(mock.removed) != 0 {
		t.Fatalf("refused requests wrote to GitHub: comments=%v removed=%v", mock.comments, mock.removed)
	}

	w := postSentinelConfirm(t, srv, "writer", `{"comment":" the workflow edit is the pinned runner image, reviewed ","type":"pr"}`, false)
	if w.Code != http.StatusOK {
		t.Fatalf("writer: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoSentinelConfirmResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.AlreadyClear || resp.Label != config.DefaultSentinelLabel || strings.Join(resp.Removed, ",") != config.DefaultSentinelLabel || resp.Repo != "testorg/testrepo" || resp.Number != 7 {
		t.Fatalf("response = %+v", resp)
	}
	if len(mock.comments) != 1 || !strings.HasPrefix(mock.comments[0], sentinelConfirmMarker+"\n@writer confirmed the sentinel finding safe") || !strings.HasSuffix(mock.comments[0], "the workflow edit is the pinned runner image, reviewed") {
		t.Fatalf("comments = %q", mock.comments)
	}
	if got := strings.Join(mock.removed, ","); got != config.DefaultSentinelLabel {
		t.Fatalf("removed = %q, want only the sentinel label", got)
	}
	if resp.Result == nil || resp.Result.Action != rowActionSentinelConfirmed || resp.Result.Target.Repo != "testorg/testrepo" || resp.Result.Target.Number != 7 || !strings.Contains(resp.Result.Outcome, "@writer") || !strings.Contains(resp.Result.Outcome, config.DefaultSentinelLabel) {
		t.Fatalf("result = %+v", resp.Result)
	}

	var audited bool
	for _, entry := range srv.GetAudit().Recent(10) {
		if entry.Action == sentinelConfirmAuditAction {
			audited = true
			if entry.User != "writer" {
				t.Fatalf("audit user = %q, want the dashboard user", entry.User)
			}
			if !strings.Contains(entry.Detail, "removed="+config.DefaultSentinelLabel) || !strings.Contains(entry.Detail, "type=pr") || !strings.Contains(entry.Detail, "pinned runner image") {
				t.Fatalf("audit detail = %q", entry.Detail)
			}
		}
	}
	if !audited {
		t.Fatal("no audit entry for the sentinel confirmation")
	}

	// The label already gone: the confirmation is still commented so the
	// record names the person; nothing is removed; the owner role passes
	// without a permission lookup (a user session is still required so the
	// comment can name who confirmed).
	mock.comments, mock.removed = nil, nil
	mock.issueLabels = []string{"lgtm"}
	w = postSentinelConfirm(t, srv, "owner-ada", `{}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("owner, already clear: code=%d body=%s", w.Code, w.Body.String())
	}
	resp = repoSentinelConfirmResponse{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.AlreadyClear || len(resp.Removed) != 0 || len(mock.removed) != 0 || len(mock.comments) != 1 {
		t.Fatalf("already clear: response=%+v comments=%v removed=%v", resp, mock.comments, mock.removed)
	}
	if resp.Result == nil || !strings.Contains(resp.Result.Outcome, "no `"+config.DefaultSentinelLabel+"` label was on the item") {
		t.Fatalf("already-clear result = %+v", resp.Result)
	}
}

func TestSentinelConfirmPureHelpers(t *testing.T) {
	if got := sentinelConfirmComment("ada", "  "); got != sentinelConfirmMarker+"\n@ada confirmed the sentinel finding safe from the dashboard." {
		t.Fatalf("bare comment = %q", got)
	}
	if got := sentinelConfirmComment("ada", " reviewed the diff "); !strings.HasSuffix(got, "from the dashboard.\n\nreviewed the diff") {
		t.Fatalf("comment with text = %q", got)
	}
	if got := sentinelLabelsOn([]string{"lgtm", "Sentinel-Alert", " sentinel-alert ", "hold"}, "sentinel-alert"); strings.Join(got, ",") != "Sentinel-Alert,sentinel-alert" {
		t.Fatalf("sentinelLabelsOn = %v", got)
	}
	if got := sentinelLabelsOn([]string{"lgtm"}, "sentinel-alert"); len(got) != 0 {
		t.Fatalf("sentinelLabelsOn clean = %v", got)
	}
}
