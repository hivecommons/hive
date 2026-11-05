package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
)

const withdrawApp = "hive-app[bot]"

// park177 is the shape of Danathar/openaudible-autoupdate#177: the
// architect's park in the body of the issue it filed.
const park177 = "Option A: pin the job to Fedora 44.\nOption B: add --bash-method=DEBUG.\n\nRecommended (owner confirms): A.\n\n— hive: agent=architect backend=claude model=claude-opus-5-5\n\n<!-- hive:owner-step -->"

type withdrawFake struct {
	mu       sync.Mutex
	labels   []string
	author   string
	body     string
	comments []string // JSON objects
	writes   []string
}

func (f *withdrawFake) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == "GET" && strings.HasSuffix(p, "/issues/177"):
		var labels []string
		for _, l := range f.labels {
			labels = append(labels, fmt.Sprintf(`{"name":%q}`, l))
		}
		_, _ = fmt.Fprintf(w, `{"number":177,"state":"open","user":{"login":%q},"body":%q,"labels":[%s]}`, f.author, f.body, strings.Join(labels, ","))
	case r.Method == "GET" && strings.HasSuffix(p, "/issues/177/comments"):
		_, _ = io.WriteString(w, "["+strings.Join(f.comments, ",")+"]")
	case r.Method == "POST" && strings.HasSuffix(p, "/issues/177/comments"):
		var body struct{ Body string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.writes = append(f.writes, "comment:"+body.Body)
		_, _ = io.WriteString(w, `{"id":99}`)
	case r.Method == "DELETE" && strings.Contains(p, "/issues/comments/"):
		f.writes = append(f.writes, "delete:"+p[strings.LastIndex(p, "/")+1:])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "DELETE" && strings.Contains(p, "/labels/"):
		f.writes = append(f.writes, "unlabel:"+p[strings.LastIndex(p, "/")+1:])
		_, _ = io.WriteString(w, `[]`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func withdrawHiveComment(id int, body string) string {
	return fmt.Sprintf(`{"id":%d,"user":{"login":%q},"body":%q}`, id, withdrawApp, body)
}

func runWithdraw(t *testing.T, f *withdrawFake, agent string) (IssueResponse, []string, []stageenginetest.Recorded) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = withdrawApp
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 177, Agent: agent, WithdrawDecision: true,
		Body: "No decision is needed here any more: PR #176 carries the fix and closes this issue. Please do not apply option A.",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	return readIssueResultFile(t, reqPath), append([]string(nil), f.writes...), sink.All()
}

func newWithdrawFake() *withdrawFake {
	return &withdrawFake{
		labels: []string{NeedsDecisionStageLabel, "agent/architect"},
		author: withdrawApp,
		body:   park177,
		comments: []string{
			withdrawHiveComment(11, unparkNoticeMarker+"\n> **What to reply**\n> - **`/hive approve` → option A (recommended)**"),
		},
	}
}

// The agent that parked #177 takes the ask back: the comment carries the
// marker, the park lifts through the stage engine, and the notice that
// still recommended option A is deleted.
func TestWithdrawDecisionLiftsOwnPark(t *testing.T) {
	f := newWithdrawFake()
	resp, writes, facts := runWithdraw(t, f, "architect")
	if !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "comment:No decision is needed") || !strings.HasSuffix(writes[0], parkWithdrawnMarker("architect")) || writes[1] != "delete:11" {
		t.Fatalf("writes = %q, want the marked comment, then the notice deleted", writes)
	}
	if len(facts) != 1 || facts[0].Method != "Unparked" || facts[0].Ref.Number != 177 || facts[0].Who != "architect" {
		t.Fatalf("facts = %+v, want one Unparked by architect", facts)
	}
}

// A park the agent placed in a later comment counts as its own too, and an
// operator-renamed decision label comes off with it.
func TestWithdrawDecisionCommentParkAndRenamedLabel(t *testing.T) {
	f := newWithdrawFake()
	f.author, f.body = "someone", "A person's report."
	f.labels = []string{"owner-call"}
	f.comments = []string{withdrawHiveComment(12, "Pick A or B.\n\n— hive: agent=quality backend=claude\n\n<!-- hive:owner-step -->")}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = withdrawApp
	c.SetIssueFilter(needsDecisionFilter{label: "owner-call"})
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	dir := withIssueDir(t)
	reqPath, _ := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 177, Agent: "quality", WithdrawDecision: true, Body: "Not needed: the cause was the test harness."})
	c.ProcessIssueRequestsOnce(context.Background())
	if resp := readIssueResultFile(t, reqPath); !resp.OK {
		t.Fatalf("result = %+v, want ok", resp)
	}
	if fmt.Sprint(f.writes[1:]) != "[unlabel:owner-call]" || len(sink.All()) != 1 {
		t.Fatalf("writes = %q facts = %+v, want the renamed label removed and one Unparked", f.writes, sink.All())
	}
}

func TestWithdrawDecisionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
		edit  func(*withdrawFake)
		want  string
	}{
		{"another lane's park", "scanner", func(*withdrawFake) {}, "architect parked"},
		{"not parked", "architect", func(f *withdrawFake) { f.labels = []string{"agent/architect"} }, "nothing to withdraw"},
		{"needs-human stays", "architect", func(f *withdrawFake) { f.labels = append(f.labels, "needs-human") }, "needs-human"},
		{"park text by a person", "architect", func(f *withdrawFake) { f.author = "someone" }, "no hive park text"},
		{"a maintainer already answered", "architect", func(f *withdrawFake) {
			f.comments = append(f.comments, withdrawHiveComment(13, unparkAcceptedMarker+"\nUn-parked by @owner."))
		}, "no hive park text"},
		{"a person's comment cannot name the parker", "architect", func(f *withdrawFake) {
			f.author = "someone"
			f.comments = []string{`{"id":14,"user":{"login":"someone"},"body":"x\n— hive: agent=architect\n<!-- hive:owner-step -->"}`}
		}, "no hive park text"},
		{"a newer park by another lane", "architect", func(f *withdrawFake) {
			f.comments = append(f.comments, withdrawHiveComment(15, "Also pick C or D.\n\n— hive: agent=quality\n\n<!-- hive:owner-step -->"))
		}, "quality parked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWithdrawFake()
			tc.edit(f)
			resp, writes, facts := runWithdraw(t, f, tc.agent)
			if resp.OK || !strings.Contains(resp.Error, tc.want) {
				t.Fatalf("result = %+v, want refused with %q", resp, tc.want)
			}
			if len(writes) != 0 || len(facts) != 0 {
				t.Fatalf("writes = %q facts = %+v, want nothing written", writes, facts)
			}
		})
	}
}

// A retry after the comment went up but the lift failed finishes the lift
// without posting the comment again.
func TestWithdrawDecisionRetryOnlyLifts(t *testing.T) {
	f := newWithdrawFake()
	f.comments = append(f.comments, withdrawHiveComment(16, "No decision is needed.\n\n"+parkWithdrawnMarker("architect")))
	resp, writes, facts := runWithdraw(t, f, "architect")
	if !resp.OK || fmt.Sprint(writes) != "[delete:11]" || len(facts) != 1 || facts[0].Method != "Unparked" {
		t.Fatalf("result = %+v writes = %q facts = %+v, want only the lift", resp, writes, facts)
	}
}

func TestWithdrawDecisionShape(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)
	for _, req := range []IssueRequest{
		{Kind: "comment", Repo: "o/r", Number: 1, Agent: "a", Body: "x", WithdrawDecision: true, NeedsDecision: true},
		{Kind: "label", Repo: "o/r", Number: 1, Agent: "a", Labels: []string{"x"}, WithdrawDecision: true},
	} {
		p, _ := WriteIssueRequest(dir, req)
		c.ProcessIssueRequestsOnce(context.Background())
		if resp := readIssueResultFile(t, p); resp.OK || !strings.Contains(resp.Error, "withdraw_decision") {
			t.Fatalf("%s request = %+v, want a shape refusal", req.Kind, resp)
		}
	}
}

// After a withdrawal the "What to reply" menu no longer offers the options
// the agent took back.
func TestUnparkMenuDropsWithdrawnOptions(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	c.appBotLogin = withdrawApp
	app := &gh.User{Login: gh.Ptr(withdrawApp)}
	before := c.unparkMenuFor(park177, app, nil)
	if len(before.options) == 0 || before.recommended != "A" {
		t.Fatalf("menu before = %+v, want options A/B with A recommended", before)
	}
	comments := []*gh.IssueComment{{User: app, Body: gh.Ptr("No decision is needed.\n\n" + parkWithdrawnMarker("architect"))}}
	m := c.unparkMenuFor(park177, app, comments)
	if len(m.options) != 0 || m.recommended != "" || m.ownerStep {
		t.Fatalf("menu = %+v, want the withdrawn options gone", m)
	}
}

// Withdrawing lifts a hive-controlled park, so like parking it needs the
// label op in the lane's allowlist.
func TestWithdrawDecisionRequiresLabelOp(t *testing.T) {
	f := newWithdrawFake()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	c.appBotLogin = withdrawApp
	c.SetWriteAllowlistFunc(allowOnly("architect", WriteOpComment))
	sink := &stageenginetest.Recorder{}
	c.SetStageFactSink(sink)
	dir := withIssueDir(t)
	reqPath, _ := WriteIssueRequest(dir, IssueRequest{Kind: "comment", Repo: "o/r", Number: 177, Agent: "architect", WithdrawDecision: true, Body: "Not needed."})
	c.ProcessIssueRequestsOnce(context.Background())
	if resp := readIssueResultFile(t, reqPath); resp.OK || !strings.Contains(resp.Error, WriteOpLabel) {
		t.Fatalf("result = %+v, want a refusal naming %q", resp, WriteOpLabel)
	}
	if len(f.writes) != 0 || len(sink.All()) != 0 {
		t.Fatalf("writes = %q facts = %+v, want nothing", f.writes, sink.All())
	}
}
