package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#18866: an issue held by the SHA-hold gate shows why on its
// row (pill, hover, chevron note) and a maintainer can waive the requirement
// from the row menu; the waiver is gated like releasing a `hold`.

func TestStageTrackerSHAHoldPayload(t *testing.T) {
	srv := newFullServer(t)
	if got := srv.stageTrackerSHAHold("o/r", 1, []string{"kind/bug"}); got != nil {
		t.Fatalf("plain issue: sha_hold = %+v, want nil", got)
	}
	held := srv.stageTrackerSHAHold("o/r", 1, []string{"kind/bug", "needs-sha"})
	if held == nil || held.State != "held" || held.Pill != "Held: needs commit SHA" ||
		held.Tip != "Post a commit SHA in the issue, or waive this requirement." || held.Note == "" {
		t.Fatalf("held: %+v", held)
	}
	waived := srv.stageTrackerSHAHold("o/r", 1, []string{"kind/bug", "needs-sha", "sha-waived"})
	if waived == nil || waived.State != "waived" || waived.Pill != "SHA waived" {
		t.Fatalf("waived with nobody known: %+v", waived)
	}
	srv.deps.GHClient = github.NewClient("t", "o", []string{"r"}, slog.Default(), "")
	srv.deps.GHClient.RecordSHAWaiver("o/r", 1, "alice")
	waived = srv.stageTrackerSHAHold("o/r", 1, []string{"sha-waived"})
	if waived == nil || waived.Pill != "SHA waived by alice" || waived.By != "alice" {
		t.Fatalf("waived by alice: %+v", waived)
	}
}

func postSHAWaiver(t *testing.T, srv *Server, user string, owner, waived bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"waived":false}`
	if waived {
		body = `{"waived":true}`
	}
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/7/sha-waiver", strings.NewReader(body))
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

func TestRepoItemSHAWaiverAuthorizationAndLabels(t *testing.T) {
	mock := &acceptTriageMock{permission: "write", issueLabels: []string{"kind/bug", "needs-sha"}}
	srv := newAcceptTriageServer(t, mock)

	// Not privileged: a reader and an anonymous caller cannot override.
	if w := postSHAWaiver(t, srv, "reader", false, true); w.Code != http.StatusForbidden {
		t.Fatalf("reader: code=%d want 403 body=%s", w.Code, w.Body.String())
	}
	if w := postSHAWaiver(t, srv, "", false, true); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: code=%d want 401 body=%s", w.Code, w.Body.String())
	}
	if len(mock.added)+len(mock.removed) != 0 {
		t.Fatalf("refused requests wrote labels: added=%v removed=%v", mock.added, mock.removed)
	}

	w := postSHAWaiver(t, srv, "writer", false, true)
	if w.Code != http.StatusOK {
		t.Fatalf("writer: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoSHAWaiverResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || !resp.Waived || resp.Result == nil || resp.Result.Action != rowActionSHAWaived {
		t.Fatalf("response = %+v", resp)
	}
	if got := strings.Join(mock.added, ","); got != "sha-waived" {
		t.Fatalf("added = %q, want sha-waived", got)
	}
	if got := strings.Join(mock.removed, ","); got != "needs-sha" {
		t.Fatalf("removed = %q, want needs-sha released at once", got)
	}
	if got := srv.deps.GHClient.SHAWaivedBy("testorg/testrepo", 7); got != "writer" {
		t.Fatalf("waived by = %q, want writer", got)
	}
	var audited bool
	for _, entry := range srv.GetAudit().Recent(10) {
		if entry.Action == shaWaiverAuditAction && entry.User == "writer" {
			audited = true
		}
	}
	if !audited {
		t.Fatal("no audit entry for the waiver")
	}

	// Waiving again is a no-op.
	mock.added, mock.removed = nil, nil
	if w := postSHAWaiver(t, srv, "writer", false, true); w.Code != http.StatusOK || len(mock.added)+len(mock.removed) != 0 {
		t.Fatalf("repeat waive: code=%d added=%v removed=%v", w.Code, mock.added, mock.removed)
	}

	// Restore removes only sha-waived; the sweep re-holds it later.
	w = postSHAWaiver(t, srv, "writer", false, false)
	if w.Code != http.StatusOK || strings.Join(mock.removed, ",") != "sha-waived" {
		t.Fatalf("restore: code=%d removed=%v body=%s", w.Code, mock.removed, w.Body.String())
	}
	if got := srv.deps.GHClient.SHAWaivedBy("testorg/testrepo", 7); got != "" {
		t.Fatalf("waived by after restore = %q, want empty", got)
	}

	// The verified owner role passes without a permission lookup.
	mock.added, mock.removed = nil, nil
	mock.issueLabels = []string{"kind/bug", "needs-sha"}
	if w := postSHAWaiver(t, srv, "", true, true); w.Code != http.StatusOK || strings.Join(mock.added, ",") != "sha-waived" {
		t.Fatalf("owner: code=%d added=%v", w.Code, mock.added)
	}
}

func TestRepoItemSHAWaiverRefusesPullRequests(t *testing.T) {
	mock := &acceptTriageMock{permission: "write", isPullRequest: true, issueLabels: []string{"needs-sha"}}
	srv := newAcceptTriageServer(t, mock)
	if w := postSHAWaiver(t, srv, "writer", false, true); w.Code != http.StatusNotFound || len(mock.added) != 0 {
		t.Fatalf("PR: code=%d added=%v, want 404 and no write", w.Code, mock.added)
	}
}

func TestStageTrackerSHAHoldPillAndMenu(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{"${shaHoldNote}", "stageTrackerSHAHoldChipHtml(item)", "/sha-waiver"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerSHAHoldOf")+"\n"+
		jsFunc(t, html, "stageTrackerSHAHoldChipHtml")+"\n"+
		jsFunc(t, html, "stageTrackerSHAHoldNoteHtml")+`
const ids = as => as.map(a => a.id);
const heldHold = { state: 'held', label: 'needs-sha', pill: 'Held: needs commit SHA', tip: 'Post a commit SHA in the issue, or waive this requirement.', note: 'held: post a commit SHA' };
const item = { repo: 'o/r', kind: 'issue', number: 18740, url: 'u', stage: 0, modifiers: [], sha_hold: heldHold };

// The pill names the reason and carries how to lift it.
const chip = stageTrackerSHAHoldChipHtml(item);
assert.ok(chip.includes('Held: needs commit SHA'), chip);
assert.ok(chip.includes('title="Post a commit SHA in the issue, or waive this requirement."'), chip);
assert.ok(chip.includes('ham-sha-held'), chip);
// The note rides the Open chevron.
assert.ok(stageTrackerSHAHoldNoteHtml(item).includes('held: post a commit SHA'));
// Nothing for an ordinary issue, a PR, or an unknown state.
assert.strictEqual(stageTrackerSHAHoldChipHtml({ kind: 'issue', number: 1 }), '');
assert.strictEqual(stageTrackerSHAHoldChipHtml({ kind: 'pr', sha_hold: heldHold }), '');
assert.strictEqual(stageTrackerSHAHoldNoteHtml({ kind: 'issue', sha_hold: { state: 'x', pill: 'p' } }), '');

// Held: the menu offers Waive; it is gated like Hold (canHold).
let as = stageTrackerRowActions(item, { user: true, canHold: true, canHuman: true });
assert.ok(ids(as).includes('sha-waive') && !ids(as).includes('sha-restore'), ids(as).join());
assert.strictEqual(as.find(a => a.id === 'sha-waive').label, 'Waive SHA requirement');
assert.strictEqual(as.find(a => a.id === 'sha-waive').enabled, true);
as = stageTrackerRowActions(item, { user: true, canHold: false, canHuman: true });
const waive = as.find(a => a.id === 'sha-waive');
assert.strictEqual(waive.enabled, false, 'a non-privileged viewer sees it disabled');
assert.ok(waive.why, 'with the reason');

// Waived: the pill reads who, the item flips to Restore.
const waived = { ...item, sha_hold: { state: 'waived', label: 'sha-waived', by: 'alice', pill: 'SHA waived by alice', tip: 't', note: 'SHA waived' } };
assert.ok(stageTrackerSHAHoldChipHtml(waived).includes('SHA waived by alice'));
as = stageTrackerRowActions(waived, { user: true, canHold: true, canHuman: true });
assert.ok(ids(as).includes('sha-restore') && !ids(as).includes('sha-waive'));
assert.strictEqual(as.find(a => a.id === 'sha-restore').label, 'Restore SHA requirement');

// No SHA state: no entry.
assert.ok(!ids(stageTrackerRowActions({ ...item, sha_hold: null }, { user: true, canHold: true })).some(i => i.indexOf('sha-') === 0));

// The request goes to the waiver endpoint.
assert.deepStrictEqual(stageTrackerActionRequest(item, 'sha-waive'), { url: '/api/repos/o/r/items/18740/sha-waiver', body: { waived: true } });
assert.deepStrictEqual(stageTrackerActionRequest(waived, 'sha-restore'), { url: '/api/repos/o/r/items/18740/sha-waiver', body: { waived: false } });
`)
}
