package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14921 closed #14906 (the issue its request declared) and
// the scanner-filed sub-issues #14910 and #14911. Only #14906 moved to
// 040-covered-by-pr when the PR opened; the siblings stayed 010-actionable
// with the scanner as their actor for the whole CI run, because the relay
// covered only the request's declared issues and the webhook path recorded
// the claims in the ledger without a stage fact.

func TestPRCoveredIssues_UnionOfDeclaredAndBodyCloses(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:0")
	body := "Builds on #14912.\n\nCloses #14906\nCloses #14910\nCloses #14911\nRefs #14909\nFixes other/repo#5"
	got, partial := c.prCoveredIssues("o/r", body, []int{14906, 0, 14906})
	if want := []int{14906, 14910, 14911}; !reflect.DeepEqual(got, want) || partial != nil {
		t.Fatalf("prCoveredIssues = %v (partial %v), want %v", got, partial, want)
	}
	if got, partial := c.prCoveredIssues("o/r", "no closing keyword here, see #3", nil); got != nil || partial != nil {
		t.Fatalf("prCoveredIssues with no closes = %v / %v, want nil", got, partial)
	}
	if got, partial := c.prCoveredIssues("o/r", "", []int{7}); !reflect.DeepEqual(got, []int{7}) || partial != nil {
		t.Fatalf("declared-only = %v / %v, want [7]", got, partial)
	}
}

// hivecommons/hive#15008: the scanner's partial fix #14990 for the shared-CI
// incident #14976 declared the issue but its body read "Refs #14976
// (needs-human: …)" — the agent deliberately not claiming to finish it. The
// relay still staged the incident "open PR covers it". A declared issue the
// body references without a closing keyword is partial, never covered; a
// closing keyword on the same issue wins; a bare mention is nothing.
func TestPRCoveredIssues_DeclaredRefsIsPartialNotCovered(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:0")
	body := "Retry go mod download in the Docker builder stages.\n\nRefs #14976 (needs-human: the workflow steps need the same retry; the App token cannot edit workflows)"
	covered, partial := c.prCoveredIssues("o/r", body, []int{14976})
	if covered != nil || !reflect.DeepEqual(partial, []int{14976}) {
		t.Fatalf("declared + Refs: covered %v partial %v, want nil / [14976]", covered, partial)
	}
	covered, partial = c.prCoveredIssues("o/r", "Refs #14976\nFixes #14976", []int{14976})
	if !reflect.DeepEqual(covered, []int{14976}) || partial != nil {
		t.Fatalf("Refs + Fixes: covered %v partial %v, want [14976] / nil", covered, partial)
	}
	// The hive's own deferral comment text names the incident, but it is
	// neither a closing nor a reference keyword: no claim of any tier.
	deferral := "<!-- hive-shared-ci-14976 -->\nDeferred to shared-CI incident #14976 (Go module proxy stream resets); the failing checks are not this PR's fault."
	if got := ParseClaimedIssues(deferral, "o/r"); got != nil {
		t.Fatalf("deferral comment parsed as closing claims: %v", got)
	}
	if got := ParseReferencedIssues(deferral, "o/r"); got != nil {
		t.Fatalf("deferral comment parsed as reference claims: %v", got)
	}
	covered, partial = c.prCoveredIssues("o/r", deferral, nil)
	if covered != nil || partial != nil {
		t.Fatalf("deferral text covers %v / %v, want nothing", covered, partial)
	}
}

// Through the relay: the declared-but-only-referenced issue is an
// AddressedBy fact (040, "addresses part of it"), not CoveredBy.
func TestPRRequestWatcher_DeclaredRefsIssueIsAddressedNotCovered(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	enableInProgress(c, "hive-a", time.Hour)
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	c.coveredByPRLabel.mark("o/r")

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	body := "Retry go mod download.\n\nRefs #14976 (needs-human: workflow files need the same retry)"
	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-14976", Title: "🐛 fix(ci): retry", Body: body, Agent: "scanner", IssueN: []int{14976}}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	if created != 1 {
		t.Fatalf("expected 1 PR created, got %d", created)
	}
	var methods []string
	for _, f := range sink.All() {
		if f.Method != "CoveredBy" && f.Method != "AddressedBy" {
			continue
		}
		if f.Ref.Number != 14976 || f.PR == nil || f.PR.Number != 42 {
			t.Errorf("fact = %+v, want (issue #14976, PR #42)", f)
		}
		methods = append(methods, f.Method)
	}
	if !reflect.DeepEqual(methods, []string{"AddressedBy"}) {
		t.Fatalf("facts = %v, want exactly one AddressedBy (no CoveredBy for a Refs-only declared issue)", methods)
	}
}

// End to end through the relay: a request declaring one issue whose body
// closes three is a CoveredBy fact for each of the three.
func TestPRRequestWatcher_CoversEveryIssueTheBodyCloses(t *testing.T) {
	created := 0
	srv := newPRMockServer(t, "", &created)
	defer srv.Close()
	c := testClient(t, srv.URL)
	enableInProgress(c, "hive-a", time.Hour)
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	c.coveredByPRLabel.mark("o/r")

	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()

	body := "Inline the linked issue.\n\nCloses #14906\nCloses #14910\nCloses #14911"
	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "scanner/fix-1", Title: "✨ feature: thing", Body: body, Agent: "scanner", IssueN: []int{14906}}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
	if created != 1 {
		t.Fatalf("expected 1 PR created, got %d", created)
	}
	var covered []int
	for _, f := range sink.All() {
		if f.Method != "CoveredBy" {
			continue
		}
		if f.Ref.Kind != stagelabels.IssueItem || f.PR == nil || f.PR.Number != 42 {
			t.Errorf("fact = %+v, want CoveredBy(issue, PR #42)", f)
		}
		covered = append(covered, f.Ref.Number)
	}
	if want := []int{14906, 14910, 14911}; !reflect.DeepEqual(covered, want) {
		t.Fatalf("CoveredBy issues = %v, want %v (declared issue plus every body close)", covered, want)
	}
}

func TestWebhookPRCoverIssues(t *testing.T) {
	strong := func(n int) IssueClaim { return IssueClaim{Repo: "repo", Issue: n} }
	cases := []struct {
		name string
		d    PRClaimDelivery
		want []int
	}{
		{"opened hive PR closing three", PRClaimDelivery{Repo: "repo", Action: "opened", State: PRStateOpen, Claims: []IssueClaim{strong(14911), strong(14906), strong(14910), strong(14906)}}, []int{14906, 14910, 14911}},
		{"edited body", PRClaimDelivery{Repo: "repo", Action: "edited", State: PRStateOpen, Claims: []IssueClaim{strong(1)}}, []int{1}},
		{"synchronize is not a text change", PRClaimDelivery{Repo: "repo", Action: "synchronize", State: PRStateOpen, Claims: []IssueClaim{strong(1)}}, nil},
		{"closed PR covers nothing", PRClaimDelivery{Repo: "repo", Action: "closed", State: PRStateClosed, Claims: []IssueClaim{strong(1)}}, nil},
		{"merged PR covers nothing", PRClaimDelivery{Repo: "repo", Action: "closed", State: PRStateMerged, Claims: []IssueClaim{strong(1)}}, nil},
		{"external and reference claims are the sweep's", PRClaimDelivery{Repo: "repo", Action: "opened", State: PRStateOpen, Claims: []IssueClaim{
			{Repo: "repo", Issue: 1, ExternalAuthor: true}, {Repo: "repo", Issue: 2, Reference: true}, {Repo: "other", Issue: 3}, strong(4),
		}}, []int{4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookPRCoverIssues(tc.d); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("webhookPRCoverIssues = %v, want %v", got, tc.want)
			}
		})
	}
}

// A `pull_request` opened delivery for a hive-authored PR stages every
// issue it closes, off the handler's path; the issue-stage lane being off
// keeps the handler silent.
func TestHandleWebhookEventPullRequestCoversClosedIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	clock := &webhookTestClock{now: time.Date(2026, 10, 28, 12, 43, 0, 0, time.UTC)}
	t.Cleanup(resetPRDetailCacheForTest(clock.Now, 0))
	t.Cleanup(resetWebhookTrackerForTest(clock.Now))
	c := NewClientForTest(srv.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.appBotLogin = "hive-app[bot]"
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	SetPRClaimsHook(nil)
	// The label definition is the one write the cover makes before the fact.
	c.coveredByPRLabel.mark("org/repo")

	body := "Closes #14906\nCloses #14910\nCloses #14911\nRefs #14909"
	payload := webhookPRPayload("opened", webhookPRObject(14921, body, "hive-app[bot]", "open"))

	// Lane off: no facts.
	if _, err := c.HandleWebhookEvent("pull_request", payload); err != nil {
		t.Fatal(err)
	}
	c.WaitStageFacts()
	if facts := sink.All(); len(facts) != 0 {
		t.Fatalf("issue-stage lane off still submitted facts: %+v", facts)
	}

	c.SetIssueStageLabels(func() IssueStagePolicy { return IssueStagePolicy{Enabled: true} })
	if _, err := c.HandleWebhookEvent("pull_request", payload); err != nil {
		t.Fatal(err)
	}
	c.WaitStageFacts()
	var covered []int
	for _, f := range sink.All() {
		if f.Method != "CoveredBy" || f.PR == nil || f.PR.Number != 14921 || f.PR.FullRepo() != "org/repo" || f.Ref.Kind != stagelabels.IssueItem {
			t.Errorf("fact = %+v, want CoveredBy(issue, PR org/repo#14921)", f)
			continue
		}
		covered = append(covered, f.Ref.Number)
	}
	if want := []int{14906, 14910, 14911}; !reflect.DeepEqual(covered, want) {
		t.Fatalf("CoveredBy issues = %v, want %v (every closed issue, not the reference)", covered, want)
	}
}
