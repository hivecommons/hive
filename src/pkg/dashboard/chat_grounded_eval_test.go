package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Chat eval: the owner's questions must select the right path and dataset,
// and a question with arguments must never come back as the bare roster or
// the bare PR counts (hivecommons/hive: "chat answers with canned dumps").

// fakeChatGrounder records the prompts and answers from a canned script.
type fakeChatGrounder struct {
	calls  []fakeChatGrounderCall
	answer string
	err    error
}

type fakeChatGrounderCall struct {
	profile, system, user string
}

func (f *fakeChatGrounder) RunDeciderTurn(_ context.Context, profile, system, user string) (string, error) {
	f.calls = append(f.calls, fakeChatGrounderCall{profile: profile, system: system, user: user})
	if f.err != nil {
		return "", f.err
	}
	return f.answer, nil
}

func chatEvalStatus(now time.Time) *StatusPayload {
	hour := now.Add(-1 * time.Hour)
	day := now.Add(-26 * time.Hour)
	week := now.Add(-8 * 24 * time.Hour)
	agents := []FrontendAgent{
		{Name: "scanner", State: "running", Busy: "idle", LastKickAt: hour.Format(time.RFC3339), KickOutcome: "success",
			LastAction: &FrontendAgentLastAction{Repo: "hivecommons/hive", Number: 14001, Action: "opened PR", At: hour.Format(time.RFC3339)}},
		{Name: "reviewer-2", State: "running", Busy: "working", AwaitingCI: true, Condition: "stalled", ConditionMessage: "waiting on CI for #14002"},
		{Name: "fixer", State: "stopped", Paused: true, PausedReason: "inference budget exhausted"},
		{Name: "triage", State: "running", Busy: "idle", NeedsLogin: true},
	}
	for i := 0; i < 12; i++ {
		agents = append(agents, FrontendAgent{Name: "worker-" + string(rune('a'+i)), State: "running", Busy: "idle"})
	}
	prs := []any{
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 14001, Title: "feat: thing", Author: "hive-bot", Labels: []string{"stage/060-in-review"}, CreatedAt: day, UpdatedAt: hour, CIStatus: "success"}},
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 14002, Title: "fix: other", Author: "alice", Labels: []string{"stage/050-ci"}, CreatedAt: week, UpdatedAt: day, CIStatus: "failure", FailingChecks: []string{"go-test"}}},
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 14003, Title: "wip", Author: "bob", Draft: true, CreatedAt: day, UpdatedAt: day}},
		FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 14004, Title: "needs owner", Author: "carol", Labels: []string{config.PRNeedsDecisionStageLabel}, CreatedAt: week, UpdatedAt: week, ReviewState: "approved"}},
	}
	issues := []any{
		github.Issue{Repo: "hivecommons/hive", Number: 13990, Title: "bug one", Labels: []string{"stage/020-ready"}, Priority: "high", CreatedAt: day},
		github.Issue{Repo: "hivecommons/hive", Number: 13991, Title: "policy question", Labels: []string{config.DefaultNeedsDecisionLabel}, CreatedAt: week},
	}
	return &StatusPayload{
		HiveID:        "hive-test",
		Agents:        agents,
		Repos:         []FrontendRepo{{Name: "hive", Full: "hivecommons/hive", OpenPrs: prs, ActionableIssues: issues}},
		Governor:      FrontendGovernor{Active: true, Mode: "busy", NextKickIn: "4m", NextAgents: []string{"scanner"}},
		OpenItems:     FrontendOpenItems{Issues: 40, PRs: 12, Total: 52},
		ActionableNow: FrontendActionableNow{Issues: 7, PRs: 3, Total: 10},
		Beads:         FrontendBeads{Workers: 5, Supervisor: 1},
		SystemAlerts:  []SystemAlert{{ID: "budget", Severity: "warning", Message: "inference budget at 90%"}},
		DeepHealth:    map[string]any{"status": "ready"},
	}
}

func chatEvalServer(t *testing.T, grounder *fakeChatGrounder) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	if grounder != nil {
		deps.ChatGrounder = grounder
	}
	s.statusMu.Lock()
	s.status = chatEvalStatus(time.Now())
	s.statusMu.Unlock()
	return s, deps
}

func chatEvalAsk(t *testing.T, s *Server, query string) map[string]any {
	t.Helper()
	rec := doPost(s, "/api/chat", map[string]interface{}{"query": query})
	if rec.Code != http.StatusOK {
		t.Fatalf("%q: status = %d, want 200", query, rec.Code)
	}
	return decodeJSON(t, rec)
}

// chatEvalCase is one owner question with the route it must take.
type chatEvalCase struct {
	query    string
	kind     chatRouteKind
	scope    string
	question string
	// snapshotHas are substrings the grounded snapshot must carry so the
	// model can actually answer; snapshotLacks must be absent (scope filter).
	snapshotHas   []string
	snapshotLacks []string
}

var chatEvalCases = []chatEvalCase{
	// The four failures observed on hive's own spoke, 2026-10-26.
	{query: "prs: waiting on review", kind: chatRouteGrounded, scope: "prs", question: "waiting on review", snapshotHas: []string{"hivecommons/hive#14001", "review_state", "\"draft\":true"}, snapshotLacks: []string{"\"name\":\"scanner\""}},
	{query: "agents: stuck", kind: chatRouteGrounded, scope: "agents", question: "stuck", snapshotHas: []string{"reviewer-2", "awaiting_ci", "inference budget exhausted", "needs_login"}, snapshotLacks: []string{"\"open_prs\":[", "\"actionable_issues\":["}},
	{query: "Which agent completed last?", kind: chatRouteGrounded, scope: "agents", question: "Which agent completed last?", snapshotHas: []string{"last_completed", "opened PR hivecommons/hive#14001"}},
	{query: "agents: scanner status", kind: chatRouteGrounded, scope: "agents", question: "scanner status", snapshotHas: []string{"\"name\":\"scanner\"", "last_kick_outcome"}},
	// Typed slash form (not forwarded by the frontend) routes identically.
	{query: "/agents stuck", kind: chatRouteGrounded, scope: "agents", question: "stuck"},
	{query: "/prs waiting on review", kind: chatRouteGrounded, scope: "prs", question: "waiting on review"},
	// More owner questions.
	{query: "why is reviewer-2 blocked", kind: chatRouteGrounded, scope: "agents", question: "why is reviewer-2 blocked", snapshotHas: []string{"blocked_reason", "stalled: waiting on CI for #14002"}},
	{query: "what's waiting on my decision", kind: chatRouteGrounded, scope: "", question: "what's waiting on my decision", snapshotHas: []string{"awaiting_decision", "hivecommons/hive#14004", "hivecommons/hive#13991"}},
	{query: "which PRs have failing CI", kind: chatRouteGrounded, scope: "prs", question: "which PRs have failing CI", snapshotHas: []string{"\"ci\":\"failure\"", "go-test"}},
	{query: "what version are we on", kind: chatRouteGrounded, scope: "", question: "what version are we on", snapshotHas: []string{"\"version\""}},
	{query: "when was the last upgrade", kind: chatRouteGrounded, scope: "", question: "when was the last upgrade"},
	{query: "how many issues are actionable", kind: chatRouteGrounded, scope: "beads", question: "how many issues are actionable", snapshotHas: []string{"\"actionable_issues\":7", "hivecommons/hive#13990"}},
	{query: "what did the scanner do last", kind: chatRouteGrounded, scope: "agents", question: "what did the scanner do last", snapshotHas: []string{"opened PR hivecommons/hive#14001"}},
	{query: "prs: older than a week", kind: chatRouteGrounded, scope: "prs", question: "older than a week", snapshotHas: []string{"\"age\":\"8d\""}},
	{query: "beads: high priority", kind: chatRouteGrounded, scope: "beads", question: "high priority", snapshotHas: []string{"\"priority\":\"high\""}},
	{query: "governor: next kick", kind: chatRouteGrounded, scope: "governor", question: "next kick", snapshotHas: []string{"\"next_kick_in\":\"4m\"", "\"next_agents\":[\"scanner\"]"}},
	{query: "is anything alerting right now", kind: chatRouteGrounded, scope: "", question: "is anything alerting right now", snapshotHas: []string{"inference budget at 90%"}},
	{query: "which PRs did the fixer open", kind: chatRouteGrounded, scope: "", question: "which PRs did the fixer open"},
	// Bare commands keep their deterministic answers.
	{query: "agents: status", kind: chatRouteBare, scope: "agents"},
	{query: "/agents", kind: chatRouteBare, scope: "agents"},
	{query: "AGENTS!!!", kind: chatRouteBare, scope: "agents"},
	{query: "prs: status", kind: chatRouteBare, scope: "prs"},
	{query: "prs?", kind: chatRouteBare, scope: "prs"},
	{query: "beads?", kind: chatRouteBare, scope: "beads"},
	{query: "status", kind: chatRouteBare, scope: "status"},
	{query: "help", kind: chatRouteHelp},
	{query: "/help", kind: chatRouteHelp},
	{query: "jam: who is online?", kind: chatRoutePresence},
	{query: "who is here", kind: chatRoutePresence},
}

func TestChatParseRoute_Eval(t *testing.T) {
	for _, tc := range chatEvalCases {
		got := chatParseRoute(tc.query)
		if got.Kind != tc.kind {
			t.Errorf("%q: kind = %d, want %d", tc.query, got.Kind, tc.kind)
			continue
		}
		if got.Scope != tc.scope {
			t.Errorf("%q: scope = %q, want %q", tc.query, got.Scope, tc.scope)
		}
		if tc.kind == chatRouteGrounded && got.Question != tc.question {
			t.Errorf("%q: question = %q, want %q", tc.query, got.Question, tc.question)
		}
	}
}

// With a grounder wired, every question with arguments reaches the model
// with its question intact and a snapshot scoped to the command's dataset,
// and the answer is the model's — not the roster.
func TestHandleChat_GroundedEval(t *testing.T) {
	for _, tc := range chatEvalCases {
		if tc.kind != chatRouteGrounded {
			continue
		}
		t.Run(tc.query, func(t *testing.T) {
			g := &fakeChatGrounder{answer: "reviewer-2 is waiting on CI.\nSources: reviewer-2"}
			s, _ := chatEvalServer(t, g)
			res := chatEvalAsk(t, s, tc.query)
			answer, _ := res["answer"].(string)
			if res["status"] != "ok" {
				t.Fatalf("result = %#v, want ok", res)
			}
			if strings.Contains(answer, "Agents (16)") || strings.Contains(answer, "open PR(s)") {
				t.Fatalf("answer = %q is the canned dump", answer)
			}
			if res["grounded"] != true {
				// The deterministic /agents matcher (#14409) recognised the
				// question and answered without a model turn.
				if len(g.calls) != 0 || answer == "" {
					t.Fatalf("result = %#v, want grounded ok or a deterministic agent answer", res)
				}
				return
			}
			if answer != g.answer {
				t.Fatalf("answer = %q, want the model's answer", answer)
			}
			if len(g.calls) != 1 {
				t.Fatalf("grounder calls = %d, want 1", len(g.calls))
			}
			call := g.calls[0]
			if call.profile != chatGroundedProfile {
				t.Errorf("profile = %q", call.profile)
			}
			if !strings.Contains(call.system, "I don't have that data") || !strings.Contains(call.system, "Sources:") || !strings.Contains(call.system, "cannot take actions") {
				t.Errorf("system prompt lacks the grounding contract: %q", call.system)
			}
			if !strings.Contains(call.user, "Question: "+tc.question+"\n") {
				t.Errorf("user prompt lacks the question %q: %q", tc.question, call.user[:200])
			}
			wantScope := tc.scope
			if wantScope == "" {
				wantScope = "all"
			}
			if !strings.Contains(call.user, "Scope: "+wantScope+"\n") {
				t.Errorf("user prompt scope: want %q in %q", wantScope, call.user[:120])
			}
			for _, want := range tc.snapshotHas {
				if !strings.Contains(call.user, want) {
					t.Errorf("snapshot lacks %q", want)
				}
			}
			for _, lacks := range tc.snapshotLacks {
				if strings.Contains(call.user, lacks) {
					t.Errorf("snapshot for scope %q must not carry %q", tc.scope, lacks)
				}
			}
			if len(call.user) > chatGroundedMaxSnapshotBytes+1024 {
				t.Errorf("prompt is %d bytes, over the cap", len(call.user))
			}
		})
	}
}

// Without a grounder (or with it disabled, or failing) a scoped question
// gets the command's usage — never the bare dump.
func TestHandleChat_ScopedQuestionNeverFallsBackToDump(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *Server, deps *Dependencies)
		want  string
	}{
		{name: "no grounder", setup: func(*Server, *Dependencies) {}, want: "no inference backend is wired"},
		{name: "disabled", setup: func(s *Server, deps *Dependencies) {
			deps.ChatGrounder = &fakeChatGrounder{answer: "never"}
			off := false
			deps.Config.Chat.GroundedAnswers.Enabled = &off
		}, want: "chat.grounded_answers.enabled: false"},
		{name: "backend error", setup: func(s *Server, deps *Dependencies) {
			deps.ChatGrounder = &fakeChatGrounder{err: errors.New("endpoint returned 503")}
		}, want: "endpoint returned 503"},
		{name: "timeout", setup: func(s *Server, deps *Dependencies) {
			deps.ChatGrounder = &fakeChatGrounder{err: context.DeadlineExceeded}
		}, want: "did not answer in time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, deps := chatEvalServer(t, nil)
			tc.setup(s, deps)
			for _, query := range []string{"agents: what should the hive do next", "/agents what should the hive do next", "prs: waiting on review", "which PRs have failing CI"} {
				res := chatEvalAsk(t, s, query)
				answer, _ := res["answer"].(string)
				if res["status"] != "grounding_unavailable" {
					t.Errorf("%q: status = %v, want grounding_unavailable", query, res["status"])
				}
				if strings.Contains(answer, "Agents (16)") || strings.Contains(answer, "worker-a") || strings.Contains(answer, "open PR(s)") {
					t.Errorf("%q: answer is the bare dump: %q", query, answer)
				}
				if !strings.Contains(answer, tc.want) {
					t.Errorf("%q: answer = %q, want reason %q", query, answer, tc.want)
				}
				if _, hasErr := res["error"]; hasErr {
					t.Errorf("%q: usage reply must not be an error: %#v", query, res)
				}
			}
		})
	}
}

// Unscoped free text without a grounder still reaches the configured
// responder, and the final fallback names why grounding did not run.
func TestHandleChat_UnscopedFallsThroughToResponder(t *testing.T) {
	s, deps := chatEvalServer(t, nil)
	deps.ChatResponder = func(_ context.Context, q string, _ []any) (string, error) { return "responder: " + q, nil }
	res := chatEvalAsk(t, s, "what changed in the last release notes?")
	if res["answer"] != "responder: what changed in the last release notes?" {
		t.Fatalf("result = %#v, want responder answer", res)
	}
	deps.ChatResponder = nil
	res = chatEvalAsk(t, s, "what changed in the last release notes?")
	answer, _ := res["answer"].(string)
	if res["status"] != "responder_unavailable" || !strings.Contains(answer, "no inference backend is wired") || strings.Contains(answer, "worker-a") {
		t.Fatalf("result = %#v, want unavailable message naming grounding, not the roster", res)
	}
	// An agent-scoped question the deterministic matcher does not know gets
	// the agents usage rather than the responder fallback -- never the roster.
	res = chatEvalAsk(t, s, "agents: what should the hive do next")
	answer, _ = res["answer"].(string)
	if res["status"] != "grounding_unavailable" || res["scope"] != "agents" || strings.Contains(answer, "worker-a") {
		t.Fatalf("result = %#v, want agents usage, not the roster", res)
	}
}

// Bare commands are unchanged with a grounder wired: no model call.
func TestHandleChat_BareCommandsStayDeterministic(t *testing.T) {
	g := &fakeChatGrounder{answer: "never"}
	s, _ := chatEvalServer(t, g)
	for query, want := range map[string]string{
		"agents: status": "Agents (16):",
		"/agents":        "- fixer: stopped (paused)",
		"prs: status":    "hivecommons/hive: 4 open PR(s)",
		"status":         "Hive status: ready",
		"help":           "/agents stuck",
	} {
		res := chatEvalAsk(t, s, query)
		answer, _ := res["answer"].(string)
		if res["status"] != "ok" || !strings.Contains(answer, want) {
			t.Errorf("%q: result = %#v, want %q", query, res, want)
		}
	}
	if len(g.calls) != 0 {
		t.Fatalf("bare commands made %d grounder calls", len(g.calls))
	}
}

// Every grounded attempt writes one audit line, answered or not.
func TestHandleChat_GroundedAudited(t *testing.T) {
	s, deps := chatEvalServer(t, &fakeChatGrounder{answer: "ok\nSources: scanner"})
	chatEvalAsk(t, s, "agents: what should the hive do next")
	deps.ChatGrounder = &fakeChatGrounder{err: errors.New("boom")}
	chatEvalAsk(t, s, "prs: waiting on review")
	entries := s.audit.Recent(0)
	var seen []string
	for _, e := range entries {
		if e.Action == chatGroundedAuditAction {
			seen = append(seen, e.Detail)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("audit lines = %d (%v), want 2", len(seen), seen)
	}
	if !strings.Contains(seen[0], "outcome=fallback: boom") || !strings.Contains(seen[0], "scope=prs") {
		t.Errorf("newest audit line = %q", seen[0])
	}
	if !strings.Contains(seen[1], "outcome=answered") || !strings.Contains(seen[1], "question=what should the hive do next") {
		t.Errorf("older audit line = %q", seen[1])
	}
}

// The snapshot is capped: lists stop at chatGroundedMaxItems and the JSON
// is shrunk under the byte cap, with the truncation recorded.
func TestChatBuildSnapshot_Caps(t *testing.T) {
	now := time.Now()
	status := chatEvalStatus(now)
	for i := 0; i < 200; i++ {
		status.Agents = append(status.Agents, FrontendAgent{Name: "bulk-" + strings.Repeat("x", 40) + string(rune('a'+i%26)), State: "running", Doing: strings.Repeat("y", 300)})
		status.Repos[0].OpenPrs = append(status.Repos[0].OpenPrs, FrontendPR{PullRequest: github.PullRequest{Repo: "hivecommons/hive", Number: 20000 + i, Title: strings.Repeat("t", 300), CreatedAt: now}})
	}
	snap := chatBuildSnapshot(status, "", now, nil)
	if len(snap.Agents) != chatGroundedMaxItems || len(snap.PRs) != chatGroundedMaxItems {
		t.Fatalf("agents = %d, prs = %d, want %d each", len(snap.Agents), len(snap.PRs), chatGroundedMaxItems)
	}
	if len(snap.Truncated) < 2 {
		t.Fatalf("truncated = %v, want agents and open_prs noted", snap.Truncated)
	}
	for _, a := range snap.Agents {
		if len(a.Doing) > chatGroundedTextCap+3 {
			t.Fatalf("doing not capped: %d", len(a.Doing))
		}
	}
	raw, items := chatEncodeSnapshot(snap, 4096)
	if len(raw) > 4096 {
		t.Fatalf("encoded = %d bytes, over 4096", len(raw))
	}
	if items == 0 || items >= 2*chatGroundedMaxItems {
		t.Fatalf("items = %d, want shrunk but non-empty", items)
	}
	var back chatSnapshot
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
}

// Decision parking is derived from stage labels on both PRs and issues.
func TestChatBuildSnapshot_Decisions(t *testing.T) {
	now := time.Now()
	snap := chatBuildSnapshot(chatEvalStatus(now), "prs", now, &chatSnapshotVersion{Release: "5.4.0", LastUpgrade: "2026-10-20T10:00:00Z"})
	if snap.Totals.AwaitingDecision != 2 || len(snap.Decisions) != 2 {
		t.Fatalf("decisions = %+v", snap.Decisions)
	}
	if snap.Version == nil || snap.Version.Release != "5.4.0" {
		t.Fatalf("version = %+v", snap.Version)
	}
	if len(snap.Issues) != 0 || len(snap.Agents) != 0 {
		t.Fatalf("prs scope must not carry issues/agents: %d/%d", len(snap.Issues), len(snap.Agents))
	}
}

func TestChatConfig_GroundedAnswersDefaultOn(t *testing.T) {
	var c config.ChatConfig
	if !c.GroundedAnswersEnabled() {
		t.Fatal("default must be on")
	}
	off := false
	c.GroundedAnswers.Enabled = &off
	if c.GroundedAnswersEnabled() {
		t.Fatal("enabled: false must turn it off")
	}
	c.GroundedAnswers.Model = " gpt-x "
	if c.GroundedAnswersModel() != "gpt-x" {
		t.Fatalf("model = %q", c.GroundedAnswersModel())
	}
}
