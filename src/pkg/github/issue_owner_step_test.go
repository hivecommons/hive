package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func testReleaseStep() *IssueOwnerStep {
	return &IssueOwnerStep{Kind: "release", Tag: "v0.12.0", Target: strings.Repeat("a", 40)}
}

func TestIssueOwnerStepRoundTrip(t *testing.T) {
	step := testReleaseStep()
	body := issueOwnerStepBody("Remaining step: cut the release.", true, step)
	wantMarker := "<!-- hive:owner-step kind=release tag=v0.12.0 target=" + step.Target + " -->"
	if !strings.HasSuffix(body, wantMarker) {
		t.Fatalf("body = %q, want marker %q", body, wantMarker)
	}
	if got := parseIssueOwnerStep(body); !reflect.DeepEqual(got, step) {
		t.Fatalf("round trip = %+v, want %+v", got, step)
	}
	if got := ownerStepCommentBody(body, true); got != body {
		t.Fatalf("legacy helper duplicated typed marker: %q", got)
	}
	if got := issueOwnerStepBody("plain note", false, step); got != "plain note" {
		t.Fatalf("metadata without park must not mark body: %q", got)
	}
	for _, bad := range []string{
		unparkOwnerStepMarker,
		step.marker() + "\n" + step.marker(),
		step.marker() + "\n" + unparkOwnerStepMarker,
		strings.Replace(step.marker(), "kind=release", "kind=settings", 1),
		strings.Replace(step.marker(), "target="+step.Target, "target=main", 1),
		strings.Replace(step.marker(), "tag=v0.12.0", "tag=v0.12.0 extra=value", 1),
		strings.Replace(step.marker(), "tag=v0.12.0", "v0.12.0", 1),
	} {
		if got := parseIssueOwnerStep(bad); got != nil {
			t.Errorf("ambiguous or invalid marker %q parsed as %+v", bad, got)
		}
	}
}

func TestIssueOwnerStepValidation(t *testing.T) {
	for _, tag := range []string{"", "@", "../v1", "v1..2", "v1.lock", "v1/.hidden", "v1/", "/v1", "v1//x", "v1.", "v1@{x", "v1 x", "v1\nx", "v1>x", "v1?", "v1\\x", "v1\x7f", "v1`x"} {
		s := *testReleaseStep()
		s.Tag = tag
		if s.validate() == nil {
			t.Errorf("accepted invalid tag %q", tag)
		}
	}
	for _, tag := range []string{"v0.12.0", "releases/v1.2.3-rc.1", "1.0+build"} {
		s := *testReleaseStep()
		s.Tag = tag
		if err := s.validate(); err != nil {
			t.Errorf("valid tag %q: %v", tag, err)
		}
	}
	for _, target := range []string{"main", "abc1234", strings.Repeat("A", 40), strings.Repeat("a", 39), strings.Repeat("g", 40)} {
		s := *testReleaseStep()
		s.Target = target
		if s.validate() == nil {
			t.Errorf("accepted mutable or invalid target %q", target)
		}
	}
}

func TestReleaseStepMenuPolicy(t *testing.T) {
	c := menuTestClient()
	step := testReleaseStep()
	body := issueOwnerStepBody("Remaining step: cut the release.", true, step)
	for _, m := range []unparkMenu{unparkMenuFromBody(body), c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", body)})} {
		if !m.ownerStep || !reflect.DeepEqual(m.releaseStep, step) {
			t.Fatalf("lost release metadata: %+v", m)
		}
		if got, want := m.bullets(), unparkMenuFromBody(ownerStepCommentBody("owner step", true)).bullets(); got != want {
			t.Fatalf("release metadata must not enable execution; menu = %q, want %q", got, want)
		}
	}
	enabled := renderUnparkNoticeForPolicy(unparkMenuFromBody(body), true)
	for _, want := range []string{"`/hive approve`: Hive creates release `v0.12.0`", "`" + step.Target + "`", "`/hive done`", "`/hive decision <your instructions>`"} {
		if !strings.Contains(enabled, want) {
			t.Errorf("enabled release notice missing %q:\n%s", want, enabled)
		}
	}
	disabled := renderUnparkNoticeForPolicy(unparkMenuFromBody(body), false)
	if strings.Contains(disabled, "/hive approve") || strings.Contains(disabled, step.Target) {
		t.Fatalf("default-off release notice changed:\n%s", disabled)
	}
	if m := c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("someone", body)}); m.ownerStep || m.releaseStep != nil {
		t.Fatal("non-Hive comment supplied a release proposal")
	}
	accepted := ghComment("hive[bot]", unparkReplyMarkerPrefix+"5 -->\n"+renderUnparkAcceptedReply("owner", unparkDoneDecision, nil))
	if m := c.unparkMenuFor(body, menuTestHiveUser, []*gh.IssueComment{accepted}); m.ownerStep || m.releaseStep != nil {
		t.Fatal("accepted reply must clear release proposal")
	}
	dashboardAccepted := ghComment("hive[bot]", renderUnparkAcceptedReply("owner", "create release", nil))
	if m := c.unparkMenuFor(body, menuTestHiveUser, []*gh.IssueComment{dashboardAccepted}); m.ownerStep || m.releaseStep != nil {
		t.Fatal("dashboard acceptance without a command-reply marker must clear release proposal")
	}
	manual := ghComment("hive[bot]", ownerStepCommentBody("Change settings instead.", true))
	if m := c.unparkMenuFor(body, menuTestHiveUser, []*gh.IssueComment{manual}); !m.ownerStep || m.releaseStep != nil {
		t.Fatal("newest manual step must supersede release proposal")
	}
	choice := issueOwnerStepBody("Option A: release\nOption B: wait", true, step)
	if m := c.unparkMenuFor("body", menuTestHiveUser, []*gh.IssueComment{ghComment("hive[bot]", choice)}); m.ownerStep || m.releaseStep != nil {
		t.Fatal("lettered options must not become a release action")
	}
}

func TestIssueOwnerStepSnapshotUsesCommentAwareSweepResult12635(t *testing.T) {
	c := menuTestClient()
	step := testReleaseStep()
	issue := &gh.Issue{
		Number: gh.Ptr(42),
		Body:   gh.Ptr(issueOwnerStepBody("release", true, step)),
		User:   menuTestHiveUser,
	}
	if got := c.issueOwnerStepForSnapshot("Org/Repo", issue); !reflect.DeepEqual(got, step) {
		t.Fatalf("trusted body step = %+v", got)
	}

	commentOnly := &gh.Issue{Number: gh.Ptr(43), Body: gh.Ptr("body"), User: gh.Ptr(gh.User{Login: gh.Ptr("someone")})}
	c.rememberIssueOwnerStep("org/repo", 43, step)
	got := c.issueOwnerStepForSnapshot("ORG/REPO", commentOnly)
	if !reflect.DeepEqual(got, step) {
		t.Fatalf("comment-resolved step = %+v", got)
	}
	got.Tag = "mutated"
	if again := c.issueOwnerStepForSnapshot("org/repo", commentOnly); again.Tag != step.Tag {
		t.Fatalf("snapshot leaked mutable cache pointer: %+v", again)
	}

	c.rememberIssueOwnerStep("org/repo", 42, nil)
	if got := c.issueOwnerStepForSnapshot("org/repo", issue); got != nil {
		t.Fatalf("answered comment must suppress stale body marker: %+v", got)
	}
}

func TestIssueRequestReleaseStepCreate(t *testing.T) {
	step := testReleaseStep()
	got := runNeedsDecisionCreatePayload(t, nil, IssueRequest{
		Repo: "o/r", Title: "Release needed", Body: "Remaining step: cut the release.",
		Agent: "scanner", NeedsDecision: true, OwnerStep: step,
	})
	if !hasLabelFold(got.Labels, NeedsDecisionStageLabel) || !reflect.DeepEqual(parseIssueOwnerStep(got.Body), step) {
		t.Fatalf("release proposal must be parked with round-trippable metadata: %+v", got)
	}
}

func TestIssueRequestReleaseStepComment(t *testing.T) {
	var mu sync.Mutex
	var writes []string
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues/5"):
			_, _ = io.WriteString(w, `{"number":5}`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"needs-decision"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues/5/labels"):
			writes = append(writes, "park")
			_, _ = io.WriteString(w, `[{"name":"needs-decision"}]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			writes = append(writes, "comment")
			var payload struct{ Body string }
			_ = json.NewDecoder(r.Body).Decode(&payload)
			body = payload.Body
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)
	step := testReleaseStep()
	path, err := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 5, Agent: "scanner", Body: "Cut the release.", NeedsDecision: true, OwnerStep: step})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if result := readIssueResultFile(t, path); !result.OK {
		t.Fatalf("comment failed: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(writes, ",") != "park,comment" || !reflect.DeepEqual(parseIssueOwnerStep(body), step) {
		t.Fatalf("want park before typed comment; writes=%v body=%q", writes, body)
	}
}

func TestIssueRequestInvalidReleaseStepMakesNoAPICalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*IssueRequest)
	}{
		{"no park", func(r *IssueRequest) { r.NeedsDecision = false }},
		{"wrong request kind", func(r *IssueRequest) { r.Kind = "claim" }},
		{"unknown step kind", func(r *IssueRequest) { r.OwnerStep.Kind = "settings" }},
		{"mutable target", func(r *IssueRequest) { r.OwnerStep.Target = "main" }},
		{"invalid tag", func(r *IssueRequest) { r.OwnerStep.Tag = "v1 -->" }},
		{"conflicting body marker", func(r *IssueRequest) { r.Body += "\n" + unparkOwnerStepMarker }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("invalid metadata reached API: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)
			req := IssueRequest{Kind: "comment", Repo: "o/r", Number: 5, Agent: "scanner", Body: "Release needed.", NeedsDecision: true, OwnerStep: testReleaseStep()}
			tc.edit(&req)
			path, err := WriteIssueRequest(dir, req)
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if result := readIssueResultFile(t, path); result.OK || !strings.Contains(result.Error, "owner_step") {
				t.Fatalf("want metadata rejection, got %+v", result)
			}
		})
	}
}

func TestIssueOwnerStepNeverDowngradedByDecisionPolicy(t *testing.T) {
	c := menuTestClient()
	c.SetAgentIssueDecisionPolicy("resolve")
	step := testReleaseStep()
	req := IssueRequest{Repo: "o/r", Title: "Release needed", Body: "Cut the release.", Agent: "scanner", NeedsDecision: true, OwnerStep: step}
	if d := c.issueDecisionDowngradeForCreate(req, req.Body); d.Active {
		t.Fatalf("create with owner step must park under resolve: %+v", d)
	}
	req.Kind, req.Number = "comment", 5
	if d := c.issueDecisionDowngradeForComment(req, req.Body); d.Active {
		t.Fatalf("comment with owner step must park under resolve: %+v", d)
	}
	// Without the owner step the same request is a plain engineering
	// question, which the resolve policy still downgrades. The body names an
	// engineering concern explicitly: since hivecommons/hive#12972 the
	// classifier fails closed and an unclassified park stays a human decision.
	req.OwnerStep = nil
	req.Body = "Should the release notes live top-level or under docs/? Option A or option B."
	if d := c.issueDecisionDowngradeForComment(req, req.Body); !d.Active {
		t.Fatal("plain engineering question must still be downgraded under resolve")
	}
}
