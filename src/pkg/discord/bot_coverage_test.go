package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// SetAgentNames
// ---------------------------------------------------------------------------

func TestSetAgentNames(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())
	names := []string{"scanner", "architect", "outreach"}
	b.SetAgentNames(names)

	b.mu.RLock()
	got := b.agentNames
	b.mu.RUnlock()

	if len(got) != len(names) {
		t.Fatalf("expected %d agent names, got %d", len(names), len(got))
	}
	for i, n := range names {
		if got[i] != n {
			t.Errorf("agentNames[%d] = %q, want %q", i, got[i], n)
		}
	}
}

// ---------------------------------------------------------------------------
// resolveAlias
// ---------------------------------------------------------------------------

func TestResolveAlias_KnownAliases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"s", "status"},
		{"st", "status"},
		{"g", "governor"},
		{"gov", "governor"},
		{"h", "help"},
		{"?", "help"},
		{"k", "kick"},
		{"p", "pause"},
		{"r", "resume"},
		{"sc", "scanner"},
		{"ar", "architect"},
		{"ou", "outreach"},
		{"su", "supervisor"},
		{"ci", "ci-maintainer"},
		{"se", "sec-check"},
		{"sg", "strategist"},
		{"te", "quality"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := resolveAlias(tt.input)
			if got != tt.want {
				t.Errorf("resolveAlias(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveAlias_NotAnAlias(t *testing.T) {
	got := resolveAlias("notanalias")
	if got != "notanalias" {
		t.Errorf("resolveAlias(%q) = %q, want pass-through", "notanalias", got)
	}
}

// ---------------------------------------------------------------------------
// getIdentity
// ---------------------------------------------------------------------------

func TestGetIdentity_KnownAgents(t *testing.T) {
	known := []string{"scanner", "ci-maintainer", "architect", "outreach", "supervisor", "sec-check", "strategist", "quality", "governor", "pipeline"}
	for _, name := range known {
		id := getIdentity(name)
		if id.Emoji == "" {
			t.Errorf("getIdentity(%q) returned empty emoji", name)
		}
		if id.Color == 0 {
			t.Errorf("getIdentity(%q) returned zero color", name)
		}
	}
}

func TestGetIdentity_UnknownAgent(t *testing.T) {
	id := getIdentity("unknown-agent")
	pipelineID := agentIdentities["pipeline"]
	if id.Emoji != pipelineID.Emoji || id.Color != pipelineID.Color {
		t.Errorf("unknown agent should fall back to pipeline identity")
	}
}

// ---------------------------------------------------------------------------
// isValidAgent
// ---------------------------------------------------------------------------

func TestIsValidAgent_True(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())
	b.SetAgentNames([]string{"scanner", "architect"})

	if !b.isValidAgent("scanner") {
		t.Error("expected scanner to be valid")
	}
	if !b.isValidAgent("architect") {
		t.Error("expected architect to be valid")
	}
}

func TestIsValidAgent_False(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())
	b.SetAgentNames([]string{"scanner"})

	if b.isValidAgent("nonexistent") {
		t.Error("expected nonexistent to be invalid")
	}
}

// ---------------------------------------------------------------------------
// cmdHelp
// ---------------------------------------------------------------------------

func TestCmdHelp_ContainsCommandInfo(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())
	b.SetAgentNames([]string{"scanner", "architect"})

	result := b.cmdHelp()

	if !strings.Contains(result, "!status") {
		t.Error("help should mention !status")
	}
	if !strings.Contains(result, "!governor") {
		t.Error("help should mention !governor")
	}
	if !strings.Contains(result, "!kick") {
		t.Error("help should mention !kick")
	}
	if !strings.Contains(result, "scanner") {
		t.Error("help should list agent names")
	}
	if !strings.Contains(result, "architect") {
		t.Error("help should list agent names")
	}
}

// ---------------------------------------------------------------------------
// cmdStatus — via mock dashboard
// ---------------------------------------------------------------------------

func TestCmdStatus_Success(t *testing.T) {
	snap := statusSnapshot{
		Governor: governorSnapshot{Mode: "QUIET", Issues: 3, PRs: 2},
		Budget:   budgetSnapshot{WeeklyBudget: 100, Used: 50, PctUsed: 50.0},
		Agents: []agentSnapshot{
			{Name: "scanner", Busy: "working", Cadence: "15m", Doing: "scanning issues"},
			{Name: "architect", Busy: "idle", Cadence: "1h", Paused: true},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(snap)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, err := b.cmdStatus()
	if err != nil {
		t.Fatalf("cmdStatus error: %v", err)
	}
	if !strings.Contains(result, "QUIET") {
		t.Error("result should contain governor mode")
	}
	if !strings.Contains(result, "Budget") {
		t.Error("result should contain budget info")
	}
	if !strings.Contains(result, "scanner") {
		t.Error("result should list scanner agent")
	}
}

func TestCmdStatus_DashboardUnreachable(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: "http://127.0.0.1:1"}, discardLogger())
	b.client = &http.Client{Timeout: 1 * time.Second}

	result, err := b.cmdStatus()
	if err != nil {
		t.Fatalf("cmdStatus should not return error on dashboard failure: %v", err)
	}
	if !strings.Contains(result, "Could not reach") {
		t.Errorf("expected 'Could not reach' message, got: %q", result)
	}
}

func TestCmdStatus_InvalidJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, err := b.cmdStatus()
	if err != nil {
		t.Fatalf("cmdStatus error: %v", err)
	}
	if !strings.Contains(result, "Failed to parse") {
		t.Errorf("expected parse failure message, got: %q", result)
	}
}

func TestCmdStatus_LongDoingTruncated(t *testing.T) {
	longDoing := strings.Repeat("x", 100)
	snap := statusSnapshot{
		Agents: []agentSnapshot{
			{Name: "scanner", Busy: "working", Cadence: "15m", Doing: longDoing},
		},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(snap)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.cmdStatus()
	// Long doing (>80 chars) gets truncated to 80
	if !strings.Contains(result, "scanner") {
		t.Error("result should contain agent name")
	}
}

func TestCmdStatus_TruncatesLongResult(t *testing.T) {
	// Create enough agents to exceed discordMsgLimit (1900 chars)
	agents := make([]agentSnapshot, 50)
	for i := range agents {
		agents[i] = agentSnapshot{
			Name:    fmt.Sprintf("agent-%d", i),
			Busy:    "working",
			Cadence: "15m",
			Doing:   strings.Repeat("a", 80),
		}
	}
	snap := statusSnapshot{Agents: agents}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(snap)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.cmdStatus()
	if len(result) > discordMsgLimit+len("…") {
		t.Errorf("result length %d exceeds limit %d", len(result), discordMsgLimit)
	}
}

// ---------------------------------------------------------------------------
// cmdGovernor — via mock dashboard
// ---------------------------------------------------------------------------

func TestCmdGovernor_Success(t *testing.T) {
	snap := statusSnapshot{
		Governor: governorSnapshot{Mode: "BUSY", Issues: 12, PRs: 5},
		Budget:   budgetSnapshot{WeeklyBudget: 200, Used: 80, PctUsed: 40.0},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(snap)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, err := b.cmdGovernor()
	if err != nil {
		t.Fatalf("cmdGovernor error: %v", err)
	}
	if !strings.Contains(result, "BUSY") {
		t.Error("result should contain mode")
	}
	if !strings.Contains(result, "12") {
		t.Error("result should contain issue count")
	}
	if !strings.Contains(result, "Budget") {
		t.Error("result should contain budget")
	}
}

func TestCmdGovernor_NoBudget(t *testing.T) {
	snap := statusSnapshot{
		Governor: governorSnapshot{Mode: "IDLE", Issues: 0, PRs: 0},
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(snap)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.cmdGovernor()
	if strings.Contains(result, "Budget") {
		t.Error("result should not contain budget when weekly budget is 0")
	}
}

func TestCmdGovernor_DashboardUnreachable(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: "http://127.0.0.1:1"}, discardLogger())
	b.client = &http.Client{Timeout: 1 * time.Second}

	result, err := b.cmdGovernor()
	if err != nil {
		t.Fatalf("cmdGovernor error: %v", err)
	}
	if !strings.Contains(result, "Could not reach") {
		t.Errorf("expected 'Could not reach' message, got: %q", result)
	}
}

// ---------------------------------------------------------------------------
// cmdAgentAction
// ---------------------------------------------------------------------------

func TestCmdAgentAction_Kick(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()
	b.SetAgentNames([]string{"scanner"})

	result, err := b.cmdAgentAction("kick", "scanner fix the build")
	if err != nil {
		t.Fatalf("cmdAgentAction error: %v", err)
	}
	if !strings.Contains(result, "Sent to scanner") {
		t.Errorf("expected kick with prompt confirmation, got: %q", result)
	}
}

func TestCmdAgentAction_KickNoPrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()
	b.SetAgentNames([]string{"scanner"})

	result, _ := b.cmdAgentAction("kick", "scanner")
	if !strings.Contains(result, "Kicked scanner") {
		t.Errorf("expected kick confirmation, got: %q", result)
	}
}

func TestCmdAgentAction_Pause(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()
	b.SetAgentNames([]string{"scanner"})

	result, _ := b.cmdAgentAction("pause", "scanner")
	if !strings.Contains(result, "Paused scanner") {
		t.Errorf("expected pause confirmation, got: %q", result)
	}
}

func TestCmdAgentAction_Resume(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()
	b.SetAgentNames([]string{"scanner"})

	result, _ := b.cmdAgentAction("resume", "scanner")
	if !strings.Contains(result, "Resumed scanner") {
		t.Errorf("expected resume confirmation, got: %q", result)
	}
}

func TestCmdAgentAction_UnknownAgent(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())
	b.SetAgentNames([]string{"scanner"})

	result, _ := b.cmdAgentAction("kick", "nonexistent")
	if !strings.Contains(result, "Unknown agent") {
		t.Errorf("expected unknown agent error, got: %q", result)
	}
}

func TestCmdAgentAction_UnknownAction(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())
	b.SetAgentNames([]string{"scanner"})

	result, _ := b.cmdAgentAction("badaction", "scanner")
	if !strings.Contains(result, "Unknown action") {
		t.Errorf("expected unknown action error, got: %q", result)
	}
}

func TestCmdAgentAction_AliasResolution(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()
	b.SetAgentNames([]string{"scanner"})

	// "sc" is an alias for "scanner"
	result, _ := b.cmdAgentAction("kick", "sc")
	if !strings.Contains(result, "Kicked scanner") {
		t.Errorf("expected alias resolution to scanner, got: %q", result)
	}
}

// ---------------------------------------------------------------------------
// dashboardGet / dashboardPost
// ---------------------------------------------------------------------------

func TestDashboardGet_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	data, err := b.dashboardGet("/api/status")
	if err != nil {
		t.Fatalf("dashboardGet error: %v", err)
	}
	if !strings.Contains(string(data), "ok") {
		t.Errorf("unexpected data: %s", string(data))
	}
}

func TestDashboardGet_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	_, err := b.dashboardGet("/api/status")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestDashboardPost_Success(t *testing.T) {
	var gotPath string
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	payload, _ := json.Marshal(map[string]string{"prompt": "do stuff"})
	err := b.dashboardPost("/api/kick/scanner", payload)
	if err != nil {
		t.Fatalf("dashboardPost error: %v", err)
	}
	if gotPath != "/api/kick/scanner" {
		t.Errorf("path = %q, want /api/kick/scanner", gotPath)
	}
	if !strings.Contains(gotBody, "do stuff") {
		t.Errorf("body should contain prompt, got: %s", gotBody)
	}
}

func TestDashboardPost_NilBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	err := b.dashboardPost("/api/pause/scanner", nil)
	if err != nil {
		t.Fatalf("dashboardPost with nil body error: %v", err)
	}
}

func TestDashboardPost_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	err := b.dashboardPost("/api/kick/scanner", nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// dashboardKick / dashboardPause / dashboardResume
// ---------------------------------------------------------------------------

func TestDashboardKick_WithPrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, err := b.dashboardKick("scanner", "fix the build")
	if err != nil {
		t.Fatalf("dashboardKick error: %v", err)
	}
	if !strings.Contains(result, "Sent to scanner") {
		t.Errorf("expected prompt confirmation, got: %q", result)
	}
	if !strings.Contains(result, "fix the build") {
		t.Errorf("expected prompt text in result, got: %q", result)
	}
}

func TestDashboardKick_WithoutPrompt(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.dashboardKick("scanner", "")
	if !strings.Contains(result, "Kicked scanner") {
		t.Errorf("expected kick confirmation, got: %q", result)
	}
}

func TestDashboardKick_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.dashboardKick("scanner", "")
	if !strings.Contains(result, "Failed to kick") {
		t.Errorf("expected failure message, got: %q", result)
	}
}

func TestDashboardPause_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.dashboardPause("scanner")
	if !strings.Contains(result, "Failed to pause") {
		t.Errorf("expected failure message, got: %q", result)
	}
}

func TestDashboardResume_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()

	result, _ := b.dashboardResume("scanner")
	if !strings.Contains(result, "Failed to resume") {
		t.Errorf("expected failure message, got: %q", result)
	}
}

// ---------------------------------------------------------------------------
// diffAgents
// ---------------------------------------------------------------------------

func TestDiffAgents_IdleToWorking(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "idle", Cadence: "15m"}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working", Cadence: "15m", Doing: "scanning"}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Working") {
		t.Errorf("expected Working transition, got: %q", sent[0])
	}
}

func TestDiffAgents_WorkingToIdle(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working", Cadence: "15m"}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "idle", Cadence: "15m", Doing: "done scanning"}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Completed") {
		t.Errorf("expected Completed transition, got: %q", sent[0])
	}
}

func TestDiffAgents_WorkingToIdleWithSummary(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working", Cadence: "15m"}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "idle", Cadence: "15m", LiveSummary: "Fixed 3 issues\nClosed 2 PRs\nDone"}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "```") {
		t.Errorf("expected code block with summary, got: %q", sent[0])
	}
}

func TestDiffAgents_WorkingToIdleWithLongSummary(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	longSummary := "line1\nline2\nline3\nline4\nline5"
	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working"}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "idle", LiveSummary: longSummary}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	// Summary should be truncated to 3 lines
	if !strings.Contains(sent[0], "line3") {
		t.Errorf("expected first 3 lines of summary, got: %q", sent[0])
	}
}

func TestDiffAgents_WorkingToIdleLongDoing(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	longDoing := strings.Repeat("x", 150)
	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working"}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "idle", Doing: longDoing}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	// Should be truncated to 100 chars
	if strings.Contains(sent[0], strings.Repeat("x", 101)) {
		t.Error("doing text should be truncated to 100 chars")
	}
}

func TestDiffAgents_PauseResume(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working", Paused: false}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Busy: "working", Paused: true}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Paused") {
		t.Errorf("expected Paused transition, got: %q", sent[0])
	}

	// Now test resume
	b.diffAgents(cur, prev)
	var sent2 []string
	drainQueue(b, &sent2)
	if len(sent2) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent2))
	}
	if !strings.Contains(sent2[0], "Resumed") {
		t.Errorf("expected Resumed transition, got: %q", sent2[0])
	}
}

func TestDiffAgents_CadenceOff(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Cadence: "15m"}},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "scanner", Cadence: "off"}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Off") {
		t.Errorf("expected Off transition, got: %q", sent[0])
	}
}

func TestDiffAgents_NewAgentIgnored(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents: []agentSnapshot{},
	}
	cur := &statusSnapshot{
		Agents: []agentSnapshot{{Name: "new-agent", Busy: "working"}},
	}

	b.diffAgents(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	// New agents not in prev should be ignored (no notification)
	if len(sent) != 0 {
		t.Errorf("expected no messages for new agent, got %d", len(sent))
	}
}

// ---------------------------------------------------------------------------
// diffGovernor
// ---------------------------------------------------------------------------

func TestDiffGovernor_ModeChange(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Governor: governorSnapshot{Mode: "QUIET", Issues: 3, PRs: 2},
	}
	cur := &statusSnapshot{
		Governor: governorSnapshot{Mode: "BUSY", Issues: 12, PRs: 5},
	}

	b.diffGovernor(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "QUIET") || !strings.Contains(sent[0], "BUSY") {
		t.Errorf("expected mode change from QUIET to BUSY, got: %q", sent[0])
	}
}

func TestDiffGovernor_NoChange(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	snap := &statusSnapshot{
		Governor: governorSnapshot{Mode: "QUIET"},
	}

	b.diffGovernor(snap, snap)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 0 {
		t.Errorf("expected no messages when mode unchanged, got %d", len(sent))
	}
}

func TestDiffGovernor_EmptyModeIgnored(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{Governor: governorSnapshot{Mode: ""}}
	cur := &statusSnapshot{Governor: governorSnapshot{Mode: "QUIET"}}

	b.diffGovernor(prev, cur)

	var sent []string
	drainQueue(b, &sent)
	// Empty prev mode should not trigger a diff
	if len(sent) != 0 {
		t.Errorf("expected no messages when prev mode is empty, got %d", len(sent))
	}
}

// ---------------------------------------------------------------------------
// onSSEEvent
// ---------------------------------------------------------------------------

func TestOnSSEEvent_NoPrevState(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	snap := &statusSnapshot{
		Agents:   []agentSnapshot{{Name: "scanner", Busy: "working"}},
		Governor: governorSnapshot{Mode: "IDLE"},
	}

	// First event — no previous state, should not produce messages
	b.onSSEEvent(snap)

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 0 {
		t.Errorf("expected no messages for first SSE event, got %d", len(sent))
	}
}

func TestOnSSEEvent_WithPrevState(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	prev := &statusSnapshot{
		Agents:   []agentSnapshot{{Name: "scanner", Busy: "idle"}},
		Governor: governorSnapshot{Mode: "IDLE"},
	}
	cur := &statusSnapshot{
		Agents:   []agentSnapshot{{Name: "scanner", Busy: "working"}},
		Governor: governorSnapshot{Mode: "QUIET"},
	}

	// Seed previous state
	b.onSSEEvent(prev)

	// Now trigger diff
	b.onSSEEvent(cur)

	var sent []string
	drainQueue(b, &sent)
	// Should have at least 2 messages: agent transition + governor mode change
	if len(sent) < 2 {
		t.Errorf("expected at least 2 messages, got %d: %v", len(sent), sent)
	}
}

// ---------------------------------------------------------------------------
// updateTopic
// ---------------------------------------------------------------------------

func TestUpdateTopic_GeneratesTopic(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	snap := &statusSnapshot{
		Agents: []agentSnapshot{
			{Name: "scanner", Busy: "working"},
			{Name: "architect", Busy: "idle", Paused: true},
			{Name: "outreach", Cadence: "off"},
		},
		Governor: governorSnapshot{Mode: "BUSY", Issues: 5, PRs: 3},
	}

	b.updateTopic(snap)

	b.mu.Lock()
	topic := b.lastTopic
	b.mu.Unlock()

	if topic == "" {
		t.Error("expected non-empty topic")
	}
	if !strings.Contains(topic, "BUSY") {
		t.Error("topic should contain governor mode")
	}
	if !strings.Contains(topic, "5i") {
		t.Error("topic should contain issue count")
	}
}

func TestUpdateTopic_UnchangedNoRepost(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	snap := &statusSnapshot{
		Agents:   []agentSnapshot{{Name: "scanner", Busy: "idle"}},
		Governor: governorSnapshot{Mode: "IDLE", Issues: 0, PRs: 0},
	}

	b.updateTopic(snap)
	b.mu.Lock()
	firstTopic := b.lastTopic
	b.mu.Unlock()

	// Call again with same state — should not trigger a new topic update
	b.updateTopic(snap)
	b.mu.Lock()
	secondTopic := b.lastTopic
	b.mu.Unlock()

	if firstTopic != secondTopic {
		t.Error("topic should not change for identical state")
	}
}

// ---------------------------------------------------------------------------
// setChannelTopic
// ---------------------------------------------------------------------------

func TestSetChannelTopic_CorrectRequest(t *testing.T) {
	var gotMethod string
	var gotPath string
	var gotBody string
	var gotAuth string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch123")
	err := b.setChannelTopic("test topic")
	if err != nil {
		t.Fatalf("setChannelTopic error: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if !strings.Contains(gotPath, "ch123") {
		t.Errorf("path should contain channel ID, got: %s", gotPath)
	}
	if gotAuth != "Bot test-token" {
		t.Errorf("auth = %q, want 'Bot test-token'", gotAuth)
	}
	if !strings.Contains(gotBody, "test topic") {
		t.Errorf("body should contain topic, got: %s", gotBody)
	}
}

func TestSetChannelTopic_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("rate limited"))
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	err := b.setChannelTopic("topic")
	if err == nil {
		t.Fatal("expected error for rate-limited response")
	}
}

// ---------------------------------------------------------------------------
// registerBuiltinCommands
// ---------------------------------------------------------------------------

func TestRegisterBuiltinCommands_AllPresent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(statusSnapshot{})
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())
	b.client = ts.Client()
	b.registerBuiltinCommands()

	expected := []string{"status", "governor", "help", "kick", "pause", "resume"}
	b.mu.RLock()
	for _, cmd := range expected {
		if _, ok := b.commands[cmd]; !ok {
			t.Errorf("expected builtin command %q not registered", cmd)
		}
	}
	b.mu.RUnlock()
}

// ---------------------------------------------------------------------------
// routeMessage — agent-as-command pattern
// ---------------------------------------------------------------------------

func TestRouteMessage_AgentAsCommand_Kick(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	b.dashboardURL = ts.URL
	b.SetAgentNames([]string{"scanner"})

	b.routeMessage(context.Background(), makeMsg("1", "!scanner do something", false))

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	// "!scanner do something" should kick scanner with "do something"
	if !strings.Contains(sent[0], "scanner") {
		t.Errorf("expected scanner in reply, got: %q", sent[0])
	}
}

func TestRouteMessage_AgentAsCommand_Pause(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	b.dashboardURL = ts.URL
	b.SetAgentNames([]string{"scanner"})

	b.routeMessage(context.Background(), makeMsg("1", "!scanner pause", false))

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Paused") {
		t.Errorf("expected Paused, got: %q", sent[0])
	}
}

func TestRouteMessage_AgentAsCommand_Resume(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	b.dashboardURL = ts.URL
	b.SetAgentNames([]string{"scanner"})

	b.routeMessage(context.Background(), makeMsg("1", "!scanner resume", false))

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Resumed") {
		t.Errorf("expected Resumed, got: %q", sent[0])
	}
}

func TestRouteMessage_AgentAsCommand_KickExplicit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")
	b.dashboardURL = ts.URL
	b.SetAgentNames([]string{"scanner"})

	b.routeMessage(context.Background(), makeMsg("1", "!scanner kick fix tests", false))

	var sent []string
	drainQueue(b, &sent)
	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	if !strings.Contains(sent[0], "Sent to scanner") {
		t.Errorf("expected kick with prompt, got: %q", sent[0])
	}
}

// ---------------------------------------------------------------------------
// consumeSSE
// ---------------------------------------------------------------------------

func TestConsumeSSE_ParsesEvents(t *testing.T) {
	snap := statusSnapshot{
		Agents:   []agentSnapshot{{Name: "scanner", Busy: "working"}},
		Governor: governorSnapshot{Mode: "IDLE"},
	}
	snapJSON, _ := json.Marshal(snap)

	sseData := fmt.Sprintf("data:%s\n\n", string(snapJSON))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseData))
		// Flush and close to end the stream
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// consumeSSE will return EOF when the server closes the connection
	_ = b.consumeSSE(ctx)

	// Verify state was captured
	b.mu.Lock()
	state := b.lastState
	b.mu.Unlock()

	if state == nil {
		t.Fatal("expected lastState to be set after SSE event")
	}
	if len(state.Agents) != 1 || state.Agents[0].Name != "scanner" {
		t.Errorf("unexpected state: %+v", state)
	}
}

func TestConsumeSSE_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())

	err := b.consumeSSE(context.Background())
	if err == nil {
		t.Fatal("expected error for non-200 SSE response")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status code, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sseLoop — context cancellation
// ---------------------------------------------------------------------------

func TestSSELoop_StopsOnCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	b := NewBot(Config{Token: "t", ChannelID: "c", DashboardURL: ts.URL}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.sseLoop(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sseLoop did not stop after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// heartbeatLoop — context cancellation
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_StopsOnCancel(t *testing.T) {
	b := NewBot(Config{Token: "t", ChannelID: "c"}, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.heartbeatLoop(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeatLoop did not stop after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// drainLoop — context cancellation
// ---------------------------------------------------------------------------

func TestDrainLoop_StopsOnCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	b := newTestBot(ts, "ch")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.drainLoop(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainLoop did not stop after context cancellation")
	}
}
