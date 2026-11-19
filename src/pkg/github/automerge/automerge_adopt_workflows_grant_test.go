package automerge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// workflowsGrantTransport is the engine's client with a fixed answer to
// WorkflowsPushable: an installation that grants (or lacks) `workflows:
// write` with a lane that asks for it serving the repo.
type workflowsGrantTransport struct {
	*hgithub.Client
	pushable bool

	mu    sync.Mutex
	asked []string
}

func (t *workflowsGrantTransport) WorkflowsPushable(_ context.Context, repo string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asked = append(t.asked, repo)
	return t.pushable
}

func (t *workflowsGrantTransport) askedRepos() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.asked...)
}

func withWorkflowsGrant(t *testing.T, h *adoptHarness, pushable bool) *workflowsGrantTransport {
	t.Helper()
	client, ok := h.e.transport.(*hgithub.Client)
	if !ok {
		t.Fatalf("engine transport is %T, want *hgithub.Client", h.e.transport)
	}
	tr := &workflowsGrantTransport{Client: client, pushable: pushable}
	h.e.transport = tr
	return tr
}

// A base that changed .github/workflows/ supersedes a branch of the base
// repository only when the hive cannot push workflow files there. With the
// installation's `workflows: write` grant, the base-sync lands on the PR's
// own branch, so the PR is adopted in place and keeps its number and
// approvals (projectbluefin/common#1689 was closed and re-opened as #1708).
// A compare failure still supersedes.
func TestAdoptionInPlaceWorkflowGuardHonoursGrant(t *testing.T) {
	cases := []struct {
		name        string
		pushable    bool
		files       []string
		failCompare bool
		wantInPlace bool
		wantAsked   bool
	}{
		{name: "grant: same-repo head adopted in place", pushable: true, files: []string{".github/workflows/build.yml"}, wantInPlace: true, wantAsked: true},
		{name: "no grant: same-repo head superseded", files: []string{".github/workflows/build.yml"}, wantAsked: true},
		{name: "grant: compare error still supersedes", pushable: true, failCompare: true},
		{name: "grant: no workflow change needs no grant read", pushable: true, files: []string{"src/a.go"}, wantInPlace: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			tr := withWorkflowsGrant(t, h, tc.pushable)
			h.adopt.allowMaintainerEdits(7, true)
			h.adopt.compareFiles = tc.files
			h.adopt.failCompare = tc.failCompare
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			h.clock.advance(30 * time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
			comments := adoptionComments(h.api)
			if len(comments) != 1 {
				t.Fatalf("adoption comments: %v", h.api.comments)
			}
			if got := strings.Contains(comments[0], "mode=in-place"); got != tc.wantInPlace {
				t.Fatalf("in-place = %v, want %v: %s", got, tc.wantInPlace, comments[0])
			}
			if superseded := len(h.adopt.pulls) == 1; superseded == tc.wantInPlace {
				t.Fatalf("superseding PRs = %d, in-place want %v", len(h.adopt.pulls), tc.wantInPlace)
			}
			asked := tr.askedRepos()
			if (len(asked) > 0) != tc.wantAsked {
				t.Fatalf("grant read for %v, want asked=%v", asked, tc.wantAsked)
			}
			for _, repo := range asked {
				if repo != "acme/widget" {
					t.Fatalf("grant read for %q, want acme/widget", repo)
				}
			}
		})
	}
}

// A fork branch takes no workflow-file push from the App whatever the base
// installation grants (projectbluefin/utah-packages#1173, actions#752), so
// the grant never makes a fork PR adoptable in place across a base workflow
// change: it stays with its author and is not copied into the base repo.
func TestAdoptionForkWorkflowGuardIgnoresGrant(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	tr := withWorkflowsGrant(t, h, true)
	h.adopt.allowMaintainerEdits(7, true)
	h.adopt.compareFiles = []string{".github/workflows/build-stage.yml"}
	h.observe(forkPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(forkPR(7, "aaa", labels...), "check-failing:unit")
	if c := adoptionComments(h.api); len(c) != 0 {
		t.Fatalf("fork PR adopted across a base workflow change: %v", c)
	}
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("fork code copied into the base repository: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7)); !rec.ForkRehomeRefused("aaa") {
		t.Fatalf("fork head not recorded as staying with its author: %+v", rec)
	}
	if asked := tr.askedRepos(); len(asked) != 0 {
		t.Fatalf("fork head read the base installation's grant: %v", asked)
	}
}

// A refused update-branch on an adopted head is logged with the merge
// sweep's own reason, not the catch-all.
func TestAdoptionRecoveryUpdateBranchSkip(t *testing.T) {
	cases := []struct {
		name   string
		status int
		msg    string
		fork   bool
		want   string
	}{
		{name: "fork head, base changed workflows", status: http.StatusForbidden, msg: workflowPermissionRefusal, fork: true, want: "fork-workflow-update"},
		{name: "same-repo head, token lacks workflows", status: http.StatusForbidden, msg: workflowPermissionRefusal, want: "workflow-permission"},
		{name: "fork not editable", status: http.StatusUnprocessableEntity, msg: "maintainer edits are not allowed", fork: true, want: "fork-not-editable"},
		{name: "conflict", status: http.StatusUnprocessableEntity, msg: "merge conflict between base and head", want: "conflict"},
		{name: "server error", status: http.StatusBadGateway, msg: "bad gateway", want: "update-branch-failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &gh.ErrorResponse{Response: &http.Response{StatusCode: tc.status}, Message: tc.msg}
			if got := adoptionRecoveryUpdateBranchSkip(classifyUpdateBranchError(err, tc.fork)); got != tc.want {
				t.Fatalf("skip reason %q, want %q", got, tc.want)
			}
		})
	}
	if got := adoptionRecoveryUpdateBranchSkip(classifyUpdateBranchError(errors.New("network"), false)); got != "update-branch-failed" {
		t.Fatalf("plain error: %q", got)
	}
}
