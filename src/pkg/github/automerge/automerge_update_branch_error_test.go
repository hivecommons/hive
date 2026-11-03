package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

// workflowPermissionRefusal is GitHub's 403 body when update-branch (or the
// merges API) would bring .github/workflows/ changes into a head and the App
// lacks the workflows permission (projectbluefin/testsuite#991, 2026-10-25).
const workflowPermissionRefusal = "refusing to allow a GitHub App to create or update workflow `.github/workflows/docs-validate.yml` without `workflows` permission"

func TestClassifyUpdateBranchError(t *testing.T) {
	resp := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	tests := []struct {
		name string
		err  error
		fork bool
		want updateBranchFailure
	}{
		{"merge conflict", resp(http.StatusUnprocessableEntity, "merge conflict between base and head"), false, updateBranchConflict},
		{"unrecognised 422 stays conflict", resp(http.StatusUnprocessableEntity, "Validation failed"), false, updateBranchConflict},
		{"already up to date", resp(http.StatusUnprocessableEntity, "There are no new commits on the base branch."), false, updateBranchUpToDate},
		{"already up to date legacy text", resp(http.StatusUnprocessableEntity, "branch is already up to date"), false, updateBranchUpToDate},
		{"403 on fork PR", resp(http.StatusForbidden, "Resource not accessible by integration"), true, updateBranchForkNotEditable},
		{"403 on same-repo PR", resp(http.StatusForbidden, "Resource not accessible by integration"), false, updateBranchOther},
		{"422 maintainer edits", resp(http.StatusUnprocessableEntity, "Maintainer edits are not allowed on this pull request"), false, updateBranchForkNotEditable},
		{"422 permission", resp(http.StatusUnprocessableEntity, "Insufficient permission to update head branch"), false, updateBranchForkNotEditable},
		{"403 workflow permission on same-repo PR", resp(http.StatusForbidden, workflowPermissionRefusal), false, updateBranchWorkflowPermission},
		// A fork head is a repository the App is not installed on: no grant
		// on the base repository lets it push workflow files there
		// (projectbluefin/utah#1001).
		{"403 workflow refusal on fork PR", resp(http.StatusForbidden, workflowPermissionRefusal), true, updateBranchForkWorkflowUpdate},
		{"422 workflow refusal on fork PR", resp(http.StatusUnprocessableEntity, workflowPermissionRefusal), true, updateBranchForkWorkflowUpdate},
		{"422 workflow refusal on same-repo PR", resp(http.StatusUnprocessableEntity, workflowPermissionRefusal), false, updateBranchWorkflowPermission},
		{"403 workflow refusal on fork PR wrapped by the mutation ledger", fmt.Errorf("updating branch: %w", resp(http.StatusForbidden, workflowPermissionRefusal)), true, updateBranchForkWorkflowUpdate},
		{"merge conflict on fork PR", resp(http.StatusUnprocessableEntity, "merge conflict between base and head"), true, updateBranchConflict},
		{"already up to date on fork PR", resp(http.StatusUnprocessableEntity, "There are no new commits on the base branch."), true, updateBranchUpToDate},
		{"403 workflow permission without backticks", resp(http.StatusForbidden, "refusing to allow a GitHub App to create or update workflow .github/workflows/ci.yml without workflows permission"), false, updateBranchWorkflowPermission},
		{"403 workflow permission wrapped by the mutation ledger", fmt.Errorf("updating branch: effect outcome uncertain, reconciliation required: %w", resp(http.StatusForbidden, workflowPermissionRefusal)), false, updateBranchWorkflowPermission},
		{"403 workflow permission marked not applied", effects.NotApplied(resp(http.StatusForbidden, workflowPermissionRefusal)), false, updateBranchWorkflowPermission},
		{"403 rate limit on same-repo PR", resp(http.StatusForbidden, "API rate limit exceeded for installation ID 1."), false, updateBranchOther},
		{"403 rate limit on fork PR", resp(http.StatusForbidden, "API rate limit exceeded for installation ID 1."), true, updateBranchForkNotEditable},
		{"server error", resp(http.StatusInternalServerError, "boom"), false, updateBranchOther},
		{"non-API error", errors.New("dial tcp"), false, updateBranchOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyUpdateBranchError(tt.err, tt.fork); got != tt.want {
				t.Fatalf("classifyUpdateBranchError() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUpdateBranchAndSkipWorkflowPermission: the sweep reports GitHub's
// workflows-permission refusal as its own reason with no error, which stages
// 085, instead of "update-branch" plus the error (a retry every tick).
func TestUpdateBranchAndSkipWorkflowPermission(t *testing.T) {
	const head = "sha-head"
	cases := []struct {
		name       string
		message    string
		fork       bool
		wantReason string
		wantErr    bool
	}{
		{name: "same-repo workflow refusal", message: workflowPermissionRefusal, wantReason: autoMergeReasonWorkflowPermission},
		{name: "fork workflow refusal", message: workflowPermissionRefusal, fork: true, wantReason: autoMergeReasonForkWorkflowUpdate},
		{name: "other same-repo 403 keeps update-branch", message: "Resource not accessible by integration", wantReason: "update-branch", wantErr: true},
		{name: "other fork 403 stays fork-not-editable", message: "Resource not accessible by integration", fork: true, wantReason: autoMergeReasonForkNotEditable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			updates := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
					updates++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprintf(w, `{"message":%q}`, tc.message)
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
					_, _ = fmt.Fprintf(w, `{"number":7,"head":{"sha":%q}}`, head)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			event, reason, err := c.updateBranchAndSkip(context.Background(), "acme/widget", "acme", "widget", 7, head, "sha-base", "behind", tc.fork)
			if reason != tc.wantReason || (err != nil) != tc.wantErr {
				t.Fatalf("reason, err = %q, %v; want %q, wantErr %v", reason, err, tc.wantReason, tc.wantErr)
			}
			if event.BranchUpdated {
				t.Fatal("a refused update must not report BranchUpdated")
			}
			mu.Lock()
			defer mu.Unlock()
			if updates != 1 {
				t.Fatalf("update-branch calls = %d, want 1", updates)
			}
		})
	}
}

// updateBranchRefusalServer answers every update-branch with a 403 carrying
// message and counts the calls.
func updateBranchRefusalServer(t *testing.T, message string, head *string, extra http.HandlerFunc) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	updates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			updates++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			_, _ = fmt.Fprintf(w, `{"number":7,"head":{"sha":%q}}`, *head)
		case extra != nil:
			extra(w, r)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return updates
	}
}

// TestUpdateBranchAndSkipWorkflowRefusalNotRetried replays
// projectbluefin/utah#1001: GitHub refused update-branch for workflow files
// and the sweep sent it again every tick, with the same head and base. A
// refusal is now sent once per head and base; the later ticks keep the
// reason, and a new base or a new head earns one fresh attempt.
func TestUpdateBranchAndSkipWorkflowRefusalNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fork       bool
		wantReason string
	}{
		{name: "same-repo", wantReason: autoMergeReasonWorkflowPermission},
		{name: "fork", fork: true, wantReason: autoMergeReasonForkWorkflowUpdate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := "sha-head"
			srv, updates := updateBranchRefusalServer(t, workflowPermissionRefusal, &head, nil)
			defer srv.Close()
			c := newAutoMergeSweepClient(srv.URL)
			tick := func(step, head, base string, wantUpdates int) {
				t.Helper()
				_, reason, err := c.updateBranchAndSkip(context.Background(), "acme/widget", "acme", "widget", 7, head, base, "behind", tc.fork)
				if reason != tc.wantReason || err != nil {
					t.Fatalf("%s: reason, err = %q, %v; want %q", step, reason, err, tc.wantReason)
				}
				if got := updates(); got != wantUpdates {
					t.Fatalf("%s: update-branch calls = %d, want %d", step, got, wantUpdates)
				}
			}
			tick("first sweep", head, "sha-base-1", 1)
			tick("second sweep, same head and base", head, "sha-base-1", 1)
			tick("third sweep, same head and base", head, "sha-base-1", 1)
			tick("the base moved", head, "sha-base-2", 2)
			tick("same head on the new base", head, "sha-base-2", 2)
			head = "sha-head-2"
			tick("the head moved", head, "sha-base-2", 3)
		})
	}
}

// TestUpdateBranchAndSkipRetriesOtherRefusals: only a workflow-file refusal
// is remembered. A fork head the App cannot edit can be fixed by the author
// ticking "Allow edits by maintainers" without moving the head or the base,
// so that refusal is tried again on the next tick.
func TestUpdateBranchAndSkipRetriesOtherRefusals(t *testing.T) {
	head := "sha-head"
	srv, updates := updateBranchRefusalServer(t, "Resource not accessible by integration", &head, nil)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	for i := 1; i <= 2; i++ {
		if _, reason, err := c.updateBranchAndSkip(context.Background(), "acme/widget", "acme", "widget", 7, head, "sha-base", "behind", true); reason != autoMergeReasonForkNotEditable || err != nil {
			t.Fatalf("tick %d: reason, err = %q, %v; want %q", i, reason, err, autoMergeReasonForkNotEditable)
		}
		if got := updates(); got != i {
			t.Fatalf("tick %d: update-branch calls = %d, want %d", i, got, i)
		}
	}
}

// TestCurrentBaseGateWorkflowRefusalKeyedOnBaseTip drives the path the
// projectbluefin hive looped on (utah#1001: "automerge head is behind the
// current base; updating before merge", then the refusal, every ~3 minutes).
// The memo is keyed on the compared base tip, so two sweeps against the same
// base make one update-branch call and a moved base makes one more.
func TestCurrentBaseGateWorkflowRefusalKeyedOnBaseTip(t *testing.T) {
	head := "sha-head"
	var baseMu sync.Mutex
	baseTip := "sha-base-1"
	compare := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/compare/main...sha-head" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		baseMu.Lock()
		defer baseMu.Unlock()
		_, _ = fmt.Fprintf(w, `{"behind_by":12,"ahead_by":1,"base_commit":{"sha":%q}}`, baseTip)
	}
	srv, updates := updateBranchRefusalServer(t, workflowPermissionRefusal, &head, compare)
	defer srv.Close()
	c := newAutoMergeSweepClient(srv.URL)
	c.requireCurrentBase = func(string) bool { return true }
	sweep := func(step string, wantUpdates int) {
		t.Helper()
		_, reason, stop, err := c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 7, "main", head, true, "behind", true)
		if !stop || reason != autoMergeReasonForkWorkflowUpdate || err != nil {
			t.Fatalf("%s: stop, reason, err = %v, %q, %v; want true, %q", step, stop, reason, err, autoMergeReasonForkWorkflowUpdate)
		}
		if got := updates(); got != wantUpdates {
			t.Fatalf("%s: update-branch calls = %d, want %d", step, got, wantUpdates)
		}
	}
	sweep("first sweep", 1)
	sweep("second sweep", 1)
	baseMu.Lock()
	baseTip = "sha-base-2"
	baseMu.Unlock()
	sweep("the base moved", 2)
}
