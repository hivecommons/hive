package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// chatAgentsFixture mirrors the owner's spoke (#14409): two BLOCKED agents
// with different reasons, a paused supervisor, one working agent, idle ones.
func chatAgentsFixture() []FrontendAgent {
	return []FrontendAgent{
		{Name: "adjudicator", State: "running", Busy: "idle", Enabled: true,
			LastKickAt: "2026-10-26T18:00:00Z", KickOutcomeAt: "2026-10-26T18:10:00Z", KickOutcome: "ended"},
		{Name: "reviewer-2", State: "running", Busy: "working", Enabled: true,
			StructuredStatus: "BLOCKED", StatusEvidence: "blocked: inference (rate_limit): 429 weekly cap reached",
			BlockedReason: "inference (rate_limit): 429 weekly cap reached", BlockedSince: "2026-10-26T17:30:00Z",
			LastKickAt: "2026-10-26T17:20:00Z", Restarts: 3},
		{Name: "scanner", State: "running", Busy: "working", Enabled: true,
			StructuredStatus: "BLOCKED", StatusEvidence: "blocked: policy stand-down: proxy request blocked",
			BlockedReason: "policy stand-down: proxy request blocked", BlockedSince: "2026-10-26T18:20:00Z",
			KickOutcome: "stand-down", KickOutcomeReason: "proxy request blocked", KickOutcomeAt: "2026-10-26T18:20:00Z",
			LastKickAt: "2026-10-26T18:15:00Z", Doing: "triaging hivecommons/hive#14000", BusySince: "2026-10-26T18:15:00Z",
			LastAction: &FrontendAgentLastAction{Repo: "hivecommons/hive", Number: 14000, Action: "commented on", At: "2026-10-26T18:18:00Z"},
			Restarts:   1},
		{Name: "supervisor", State: "stopped", Paused: true, PausedReason: "operator pause", PausedBy: "andy", PausedAt: "2026-10-25T12:00:00Z", Enabled: true},
		{Name: "worker", State: "running", Busy: "working", Enabled: true, Doing: "fixing #14100", BusySince: "2026-10-26T18:25:00Z",
			LastKickAt: "2026-10-26T18:25:00Z"},
		{Name: "janitor", State: "stopped", Enabled: true, Condition: "ProviderError", ConditionMessage: "exit 1", Restarts: 12},
	}
}

var chatAgentsNow = time.Date(2026, 10, 26, 18, 30, 0, 0, time.UTC)

func TestParseChatAgentIntent_Table(t *testing.T) {
	agents := chatAgentsFixture()
	for _, tc := range []struct {
		query   string
		inScope bool
		want    chatAgentIntent
		unknown string
	}{
		// Frontend forwards bare `/agents` as `agents: status` — the roster.
		{query: "agents: status", inScope: true, want: chatAgentIntent{Kind: chatAgentRoster}},
		{query: "AGENTS!!!", inScope: true, want: chatAgentIntent{Kind: chatAgentRoster}},
		{query: "list agents", inScope: true, want: chatAgentIntent{Kind: chatAgentRoster}},
		{query: "show all agents", inScope: true, want: chatAgentIntent{Kind: chatAgentRoster}},
		// Filters.
		{query: "agents: stuck", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterBlocked}},
		{query: "/agents blocked", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterBlocked}},
		{query: "which agents are stuck?", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterBlocked}},
		{query: "agents: paused", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterPaused}},
		{query: "agents: running", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterRunning}},
		{query: "which agents are idle", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterIdle}},
		{query: "agents: failed", inScope: true, want: chatAgentIntent{Kind: chatAgentFilter, Filter: chatAgentFilterFailed}},
		// Superlatives.
		{query: "Which agent completed last?", inScope: true, want: chatAgentIntent{Kind: chatAgentLastCompleted}},
		{query: "which agent finished most recently", inScope: true, want: chatAgentIntent{Kind: chatAgentLastCompleted}},
		{query: "agents: last", inScope: true, want: chatAgentIntent{Kind: chatAgentLastCompleted}},
		{query: "which agent ran last", inScope: true, want: chatAgentIntent{Kind: chatAgentLastRun}},
		// Per-agent detail, slash and natural language.
		{query: "agents: scanner status", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "status"}},
		{query: "agents: status scanner", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "status"}},
		{query: "agents: reviewer-2 why", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "reviewer-2", Aspect: "why"}},
		{query: "/agents scanner", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "status"}},
		{query: "scanner status", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "status"}},
		{query: "status of scanner", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "status"}},
		{query: "what is scanner doing?", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "status"}},
		{query: "why is reviewer-2 blocked", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "reviewer-2", Aspect: "why"}},
		{query: "when did scanner last run", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "last"}},
		{query: "agents: Scanner history", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "scanner", Aspect: "history"}},
		// Unknown name in a name-shaped query.
		{query: "agents: nosuch status", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "nosuch", Aspect: "status"}, unknown: "nosuch"},
		{query: "agents: nosuch", inScope: true, want: chatAgentIntent{Kind: chatAgentDetail, Agent: "nosuch"}, unknown: "nosuch"},
		// In scope but no local intent: left for the responder.
		{query: "agents: what should the hive work on next week", inScope: true, want: chatAgentIntent{}},
		// Not agent questions at all.
		{query: "status", inScope: false},
		{query: "prs?", inScope: false},
		{query: "beads", inScope: false},
	} {
		got, inScope, unknown := parseChatAgentIntent(tc.query, agents)
		if inScope != tc.inScope {
			t.Errorf("%q: inScope = %v, want %v", tc.query, inScope, tc.inScope)
			continue
		}
		if !inScope {
			continue
		}
		if got != tc.want {
			t.Errorf("%q: intent = %+v, want %+v", tc.query, got, tc.want)
		}
		if unknown != tc.unknown {
			t.Errorf("%q: unknown = %q, want %q", tc.query, unknown, tc.unknown)
		}
	}
}

func TestRenderChatAgentRoster_InlineBlockReason(t *testing.T) {
	out := renderChatAgentRoster(chatAgentsFixture(), chatAgentsNow)
	for _, want := range []string{
		"Agents (6):",
		"- reviewer-2: running — BLOCKED: inference (rate_limit): 429 weekly cap reached (since 2026-10-26 17:30 UTC (1h00m ago))",
		"- scanner: running — BLOCKED: policy stand-down: proxy request blocked (since 2026-10-26 18:20 UTC (10m ago))",
		"- supervisor: stopped (paused)",
		"- worker: running — fixing #14100",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("roster missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "— BLOCKED\n") {
		t.Errorf("roster still prints a bare BLOCKED label:\n%s", out)
	}
}

func TestRenderChatAgentFilter_Table(t *testing.T) {
	agents := chatAgentsFixture()
	for _, tc := range []struct {
		filter  string
		want    []string
		exclude []string
	}{
		{
			filter:  chatAgentFilterBlocked,
			want:    []string{"Blocked agents (2 of 6):", "**reviewer-2** (running) — inference (rate_limit): 429 weekly cap reached; since 2026-10-26 17:30 UTC (1h00m ago)", "**scanner** (running) — policy stand-down: proxy request blocked; since"},
			exclude: []string{"adjudicator", "supervisor", "worker"},
		},
		{
			filter:  chatAgentFilterPaused,
			want:    []string{"Paused agents (1 of 6):", "**supervisor** — paused: operator pause (by andy); since 2026-10-25 12:00 UTC (1d ago)"},
			exclude: []string{"scanner"},
		},
		{
			filter:  chatAgentFilterRunning,
			want:    []string{"Running agents (3 of 6):", "**worker** — working: fixing #14100; since 2026-10-26 18:25 UTC (5m ago)"},
			exclude: []string{"adjudicator", "supervisor", "janitor"},
		},
		{
			filter:  chatAgentFilterIdle,
			want:    []string{"Idle agents (1 of 6):", "**adjudicator** — idle; last turn ended 2026-10-26 18:10 UTC (20m ago)"},
			exclude: []string{"scanner", "worker"},
		},
		{
			filter:  chatAgentFilterFailed,
			want:    []string{"Failed agents (1 of 6):", "**janitor** — stopped: exit 1 (12 restarts)"},
			exclude: []string{"supervisor", "scanner"},
		},
	} {
		out := renderChatAgentFilter(agents, tc.filter, chatAgentsNow)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q:\n%s", tc.filter, w, out)
			}
		}
		for _, x := range tc.exclude {
			if strings.Contains(out, x) {
				t.Errorf("%s: should not list %q:\n%s", tc.filter, x, out)
			}
		}
	}
	if out := renderChatAgentFilter(agents[:1], chatAgentFilterBlocked, chatAgentsNow); !strings.Contains(out, "No blocked agents right now (1 agents total)") {
		t.Errorf("empty filter = %q", out)
	}
}

func TestRenderChatAgentDetail_Table(t *testing.T) {
	byName := map[string]FrontendAgent{}
	for _, a := range chatAgentsFixture() {
		byName[a.Name] = a
	}
	for _, tc := range []struct {
		agent, aspect string
		want          []string
		exclude       []string
	}{
		{
			agent: "scanner", aspect: "status",
			want: []string{
				"**scanner** — running, BLOCKED",
				"- Blocked: policy stand-down: proxy request blocked (since 2026-10-26 18:20 UTC (10m ago))",
				"- Doing: triaging hivecommons/hive#14000 (since 2026-10-26 18:15 UTC (15m ago))",
				"- Last completed turn: 2026-10-26 18:20 UTC (10m ago) — stand-down: proxy request blocked",
				"- Last kick: 2026-10-26 18:15 UTC (15m ago)",
				"- Last PR/issue: commented on hivecommons/hive#14000 at 2026-10-26 18:18 UTC (12m ago)",
				"- Restarts: 1",
			},
			exclude: []string{"reviewer-2", "adjudicator", "Agents ("},
		},
		{
			agent: "reviewer-2", aspect: "why",
			want:    []string{"**reviewer-2** — running, BLOCKED", "- Blocked: inference (rate_limit): 429 weekly cap reached (since 2026-10-26 17:30 UTC (1h00m ago))"},
			exclude: []string{"Restarts", "scanner"},
		},
		{
			agent: "worker", aspect: "why",
			want: []string{"**worker** — running, working", "- Not blocked."},
		},
		{
			agent: "scanner", aspect: "last",
			want:    []string{"- Last completed turn:", "- Last kick:", "- Last PR/issue:"},
			exclude: []string{"Restarts"},
		},
		{
			agent: "reviewer-2", aspect: "history",
			want: []string{"- Restarts: 3"},
		},
		{
			agent: "supervisor", aspect: "status",
			want: []string{"**supervisor** — stopped (paused)", "- Paused: yes — operator pause (by andy); since 2026-10-25 12:00 UTC (1d ago)"},
		},
		{
			agent: "adjudicator", aspect: "status",
			want: []string{"**adjudicator** — running, idle", "- Last completed turn: 2026-10-26 18:10 UTC (20m ago) — ended"},
		},
	} {
		out := renderChatAgentDetail(byName[tc.agent], tc.aspect, chatAgentsNow)
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s/%s: missing %q:\n%s", tc.agent, tc.aspect, w, out)
			}
		}
		for _, x := range tc.exclude {
			if strings.Contains(out, x) {
				t.Errorf("%s/%s: should not contain %q:\n%s", tc.agent, tc.aspect, x, out)
			}
		}
	}
}

func TestRenderChatAgentLastCompleted(t *testing.T) {
	out := renderChatAgentLastCompleted(chatAgentsFixture(), chatAgentsNow)
	if !strings.HasPrefix(out, "**scanner** completed last, at 2026-10-26 18:20 UTC (10m ago) — outcome stand-down: proxy request blocked; last PR/issue commented on hivecommons/hive#14000") {
		t.Errorf("last completed = %q", out)
	}
	if !strings.Contains(out, "Before that: adjudicator (2026-10-26 18:10 UTC (20m ago)).") {
		t.Errorf("missing runner-up: %q", out)
	}
	if out := renderChatAgentLastCompleted([]FrontendAgent{{Name: "x"}}, chatAgentsNow); out != "No agent has a recorded completed run yet." {
		t.Errorf("empty = %q", out)
	}
	if out := renderChatAgentLastRun(chatAgentsFixture(), chatAgentsNow); !strings.HasPrefix(out, "**worker** ran last — kicked 2026-10-26 18:25 UTC (5m ago), still working.") {
		t.Errorf("last run = %q", out)
	}
}

func TestRenderChatAgentUnknown(t *testing.T) {
	out := renderChatAgentUnknown("nosuch", chatAgentsFixture())
	if out != "No agent named `nosuch`; agents: adjudicator, janitor, reviewer-2, scanner, supervisor, worker" {
		t.Errorf("unknown = %q", out)
	}
}

func TestBlockedReasonFromEvidence(t *testing.T) {
	for _, tc := range []struct{ status, evidence, want string }{
		{"BLOCKED", "blocked: inference (auth): 401", "inference (auth): 401"},
		{"BLOCKED", "Blocked:   crash-looping (9 restarts in 24h)", "crash-looping (9 restarts in 24h)"},
		{"BLOCKED", "", "reason not recorded"},
		{"BLOCKED", "no prefix here", "no prefix here"},
		{"NEEDS_CONTEXT", "blocked: whatever", ""},
		{"", "blocked: whatever", ""},
	} {
		if got := blockedReasonFromEvidence(tc.status, tc.evidence); got != tc.want {
			t.Errorf("(%q,%q) = %q, want %q", tc.status, tc.evidence, got, tc.want)
		}
	}
}

// The owner's three repro queries, end to end through /api/chat: none of
// them may come back as the roster.
func TestHandleChat_AgentQuestionsAreNotTheRoster(t *testing.T) {
	s := newFullServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{Agents: chatAgentsFixture()}
	s.statusMu.Unlock()

	for _, tc := range []struct {
		query   string
		want    string
		exclude string
	}{
		{query: "agents: stuck", want: "Blocked agents (2 of 6):", exclude: "adjudicator"},
		{query: "Which agent completed last?", want: "**scanner** completed last", exclude: "Agents ("},
		{query: "agents: scanner status", want: "**scanner** — running, BLOCKED", exclude: "reviewer-2"},
		{query: "agents: status scanner", want: "**scanner** — running, BLOCKED", exclude: "reviewer-2"},
		{query: "agents: reviewer-2 why", want: "- Blocked: inference (rate_limit): 429 weekly cap reached", exclude: "scanner"},
		{query: "scanner status", want: "**scanner** — running, BLOCKED", exclude: "Hive status"},
		{query: "status of scanner", want: "**scanner** — running, BLOCKED", exclude: "Hive status"},
		{query: "agents: nosuch status", want: "No agent named `nosuch`; agents: adjudicator", exclude: "**"},
		{query: "agents: status", want: "Agents (6):"},
		{query: "agents: what should the hive do next", want: "couldn't match that agent question", exclude: "Agents ("},
		{query: "status", want: "Hive status:"},
	} {
		rec := doPost(s, "/api/chat", map[string]interface{}{"query": tc.query})
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: status = %d", tc.query, rec.Code)
		}
		answer, _ := decodeJSON(t, rec)["answer"].(string)
		if !strings.Contains(answer, tc.want) {
			t.Errorf("%q: answer = %q, want to contain %q", tc.query, answer, tc.want)
		}
		if tc.exclude != "" && strings.Contains(answer, tc.exclude) {
			t.Errorf("%q: answer = %q, must not contain %q", tc.query, answer, tc.exclude)
		}
	}
}

func TestHandleChat_UnmatchedAgentQuestionGoesToResponderWithState(t *testing.T) {
	s := newFullServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{Agents: chatAgentsFixture()}
	s.statusMu.Unlock()
	var got string
	s.deps.ChatResponder = func(ctx context.Context, query string, history []any) (string, error) {
		got = query
		return "responder answer", nil
	}

	rec := doPost(s, "/api/chat", map[string]interface{}{"query": "agents: what should the hive do next"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	result := decodeJSON(t, rec)
	if result["answer"] != "responder answer" {
		t.Fatalf("answer = %v, want responder answer", result["answer"])
	}
	if !strings.HasPrefix(got, "agents: what should the hive do next\n\nHive agent state for reference:\nAgents (6):") {
		t.Fatalf("responder query = %q, want original question plus agent state", got)
	}
	if !strings.Contains(got, "scanner: running — BLOCKED: policy stand-down") {
		t.Fatalf("responder query lacks block reasons: %q", got)
	}

	// A non-agent question reaches the responder untouched.
	rec = doPost(s, "/api/chat", map[string]interface{}{"query": "what changed?"})
	if rec.Code != http.StatusOK || got != "what changed?" {
		t.Fatalf("non-agent query = %q (code %d), want passthrough", got, rec.Code)
	}
}
