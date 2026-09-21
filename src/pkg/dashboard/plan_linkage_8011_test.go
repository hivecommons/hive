package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

// hivecommons/hive#8011: epic↔issue linkage in the dashboard. The status
// payload carries issue→plan links and the waiting-on-human list; approve
// mirrors a checklist to the source issue when the knob is on; the static UI
// renders a state chip on the pill and claimant/PR in the review modal.

type recordingCommenter struct {
	repo   string
	number int
	body   string
	calls  int
	err    error
}

func (r *recordingCommenter) CreateIssueComment(_ context.Context, repo string, number int, body string) error {
	r.calls++
	r.repo, r.number, r.body = repo, number, body
	return r.err
}

func issuePlanServer(t *testing.T) (*Server, *beads.Store, *beads.Bead) {
	t.Helper()
	srv, store, _ := planServer(t)
	issue := github.Issue{Repo: "acme/widgets", Number: 42, Title: "ship widgets v2", URL: "https://github.com/acme/widgets/issues/42", Labels: []string{"plan"}}
	epic, err := planning.EpicFromIssue(store, issue, "")
	if err != nil {
		t.Fatal(err)
	}
	return srv, store, epic
}

func TestBuildPlanning_IssueLinksAndWaitingOnHuman(t *testing.T) {
	_, store, epic := issuePlanServer(t)
	stores := map[string]*beads.Store{"architect": store}

	fp := buildPlanningAt(stores, false, 6, time.Now())
	var link *PlanIssueLink
	for i := range fp.Issues {
		if fp.Issues[i].EpicID == epic.ID {
			link = &fp.Issues[i]
		}
	}
	if link == nil {
		t.Fatalf("issue epic missing from planning.issues: %+v", fp.Issues)
	}
	if link.IssueRepo != "acme/widgets" || link.IssueNumber != "42" || link.State != planning.PlanStateQueued || link.Agent != "architect" {
		t.Fatalf("link = %+v, want acme/widgets#42 queued in architect store", *link)
	}
	// The bd-created epic from planServer is a draft awaiting review: it has
	// no issue link but IS waiting on a human.
	var reviewItems, issueLinksForBdEpic int
	for _, w := range fp.WaitingOnHuman {
		if w.Reason == planning.PlanStateReview {
			reviewItems++
			if w.Issue != "" {
				issueLinksForBdEpic++
			}
		}
	}
	if reviewItems != 1 || issueLinksForBdEpic != 0 {
		t.Fatalf("waiting_on_human = %+v, want exactly one review item with no issue ref", fp.WaitingOnHuman)
	}

	// Decompose the issue epic and it moves to review, with the issue ref.
	if _, err := planning.DecomposeFromOutput(store, epic, "1. [T1] a [agent_suitable]\n", planning.Options{}); err != nil {
		t.Fatal(err)
	}
	fp = buildPlanningAt(stores, false, 6, time.Now())
	found := false
	for _, w := range fp.WaitingOnHuman {
		if w.EpicID == epic.ID && w.Issue == "acme/widgets#42" && w.Reason == planning.PlanStateReview {
			found = true
		}
	}
	if !found {
		t.Fatalf("decomposed issue epic not listed as waiting for review: %+v", fp.WaitingOnHuman)
	}
}

func TestMirrorPlan_PostsChecklistWhenEnabled(t *testing.T) {
	srv, store, epic := issuePlanServer(t)
	if _, err := planning.DecomposeFromOutput(store, epic, "1. [T1] build it [agent_suitable]\n2. [T2] sign off [human_required]\n", planning.Options{}); err != nil {
		t.Fatal(err)
	}
	tree, err := planning.GetPlanTree(store, epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/approve", nil)
	req.Host = "hive.example"
	rec := &recordingCommenter{}

	if srv.mirrorPlanWith(req, rec, store, tree, "architect") != true || rec.calls != 1 {
		t.Fatalf("mirror not posted: calls=%d", rec.calls)
	}
	if rec.repo != "acme/widgets" || rec.number != 42 {
		t.Fatalf("posted to %s#%d, want acme/widgets#42", rec.repo, rec.number)
	}
	for _, want := range []string{planning.MirrorMarker, "- [ ] **T1** build it", "- [ ] **T2** sign off — human", "#plan=" + epic.ID} {
		if !strings.Contains(rec.body, want) {
			t.Errorf("mirror body missing %q:\n%s", want, rec.body)
		}
	}

	// A forge failure is swallowed: approval already happened.
	failing := &recordingCommenter{err: errors.New("boom")}
	if srv.mirrorPlanWith(req, failing, store, tree, "architect") {
		t.Fatal("failed post must report false")
	}

	// A bd-created epic (no issue) has nowhere to mirror to.
	bare, err := store.Create("bare epic", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	bareTree := &planning.PlanTree{EpicID: bare.ID, EpicTitle: bare.Title}
	quiet := &recordingCommenter{}
	if srv.mirrorPlanWith(req, quiet, store, bareTree, "architect") || quiet.calls != 0 {
		t.Fatalf("bd-created epic must not be mirrored: calls=%d", quiet.calls)
	}
}

func TestHandlePlanApprove_MirrorOffByDefault(t *testing.T) {
	srv, store, epic := issuePlanServer(t)
	if _, err := planning.DecomposeFromOutput(store, epic, "1. [T1] a [agent_suitable]\n", planning.Options{}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/plan/"+epic.ID+"/approve", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK       bool `json:"ok"`
		Mirrored bool `json:"mirrored"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Mirrored {
		t.Fatalf("resp=%+v, want ok and mirrored=false with planning.mirror_to_issue unset", resp)
	}
}

func TestStaticPlanLinkageWiring8011(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		// Pill chip replaces the Plan button for issues that already have a plan.
		`const linked = planLinks[repoFull + '#' + i.number];`,
		`function planIssueChip(l)`,
		`data-action="openPlanReview" data-arg0="${esc(l.epicId)}"`,
		`.repo-issue-plan.plan-review { --pill-c: var(--yellow); }`,
		`.repo-issue-plan.plan-stuck { --pill-c: var(--red); }`,
		// Status payload join keys.
		`window._lastStatus.planning.issues`,
		`plan.waiting_on_human`,
		// Review modal: claimant and PR per task.
		`const who = c.claimedBy`,
		`const pr = c.prUrl`,
		// Tile names what needs you; deep link from the mirrored comment.
		`Needs you:`,
		`function handlePlanHash()`,
		`hash.startsWith('#plan=')`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}
