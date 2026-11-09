package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAgentGitHubBudgetWindowCountingAndIsolation(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	b := newAgentGitHubBudget(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if count, warn := b.record("scanner", http.StatusOK, 3); count != i+1 || (i < 2 && warn) {
			t.Fatalf("record %d count=%d warn=%v", i, count, warn)
		}
	}
	if count, retry, over := b.check("scanner", 3); !over || count != 3 || retry != 3600 {
		t.Fatalf("check scanner = count %d retry %d over %v, want 3/3600/true", count, retry, over)
	}
	if count, _, over := b.check("quality", 3); over || count != 0 {
		t.Fatalf("quality isolated count=%d over=%v", count, over)
	}
	now = now.Add(time.Hour + time.Second)
	if count, _, over := b.check("scanner", 3); over || count != 0 {
		t.Fatalf("window did not slide: count=%d over=%v", count, over)
	}
}

func TestAgentGitHubBudgetDoesNotCount304(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	b := newAgentGitHubBudget(func() time.Time { return now })
	if count, warn := b.record("scanner", http.StatusNotModified, 1); count != 0 || warn {
		t.Fatalf("304 counted: count=%d warn=%v", count, warn)
	}
	if _, _, over := b.check("scanner", 1); over {
		t.Fatal("304-only agent was capped")
	}
}

func TestAgentGitHubBudgetReserveFloorGuardReadOnly(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	p := &GitHubProxy{
		githubBudget:       newAgentGitHubBudget(func() time.Time { return now }),
		githubBudgetCap:    func(string) int { return 300 },
		githubReserveFloor: func() int { return 400 },
		githubReserveSnapshot: func() (int, time.Time, bool) {
			return 399, now.Add(2 * time.Minute), true
		},
	}
	if st := p.githubBudgetRefusal("scanner", http.MethodGet, "/repos/o/r/issues", true); st == nil || st.Reason != "reserve" || st.RetryAfter != 120 {
		t.Fatalf("reserve read refusal = %+v, want reserve retry 120", st)
	}
	if st := p.githubBudgetRefusal("scanner", http.MethodPost, "/repos/o/r/issues", false); st != nil {
		t.Fatalf("reserve blocked write: %+v", st)
	}
}

func TestAgentGitHubBudget429ShapeAndNudge(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	nudges := 0
	p := &GitHubProxy{
		logger:       slog.Default(),
		githubBudget: newAgentGitHubBudget(func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }),
		githubBudgetNudge: func(agentName, message string) {
			nudges++
			if agentName != "scanner" || !strings.Contains(message, "Stop polling GitHub") {
				t.Fatalf("nudge = %q %q", agentName, message)
			}
		},
	}
	done := make(chan bool, 1)
	go func() {
		done <- p.writeAgentGitHubBudget429(server, agentGitHubBudgetStatus{
			Agent: "scanner", Count: 3, Cap: 3, RetryAfter: 17, Reason: "cap",
		})
	}()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "17" {
		t.Fatalf("Retry-After = %q, want 17", got)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.Contains(body["message"], "hourly cap reached (3/3)") {
		t.Fatalf("message = %q", body["message"])
	}
	if !strings.Contains(body["documentation_url"], "docs/operator-reference.md#github-api-quota") {
		t.Fatalf("documentation_url = %q", body["documentation_url"])
	}
	_ = resp.Body.Close()
	if ok := <-done; !ok {
		t.Fatal("writeAgentGitHubBudget429 returned false")
	}
	if nudges != 1 {
		t.Fatalf("nudges = %d, want 1", nudges)
	}
	if p.githubBudget.markNudged("scanner") {
		t.Fatal("markNudged allowed a second nudge in the same window")
	}
	if nudges != 1 {
		t.Fatalf("nudge repeated in same window: %d", nudges)
	}
}

// readAgentGitHubBudget429 writes st through writeAgentGitHubBudget429 and
// returns the nudge typed into the pane and the 429 body's message.
func readAgentGitHubBudget429(t *testing.T, now time.Time, st agentGitHubBudgetStatus) (nudge, message string, resp *http.Response) {
	t.Helper()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	p := &GitHubProxy{
		githubBudget:      newAgentGitHubBudget(func() time.Time { return now }),
		githubBudgetNudge: func(_, msg string) { nudge = msg },
	}
	done := make(chan bool, 1)
	go func() { done <- p.writeAgentGitHubBudget429(server, st) }()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_ = resp.Body.Close()
	if ok := <-done; !ok {
		t.Fatal("writeAgentGitHubBudget429 returned false")
	}
	return nudge, body["message"], resp
}

// The agent's own hourly cap keeps its wording: agents (and the lane policies
// that quote "hourly cap reached") stop GitHub reads for the session.
func TestAgentGitHubBudgetCapRefusalWordingUnchanged(t *testing.T) {
	now := time.Date(2026, 11, 8, 11, 35, 20, 0, time.UTC)
	nudge, message, _ := readAgentGitHubBudget429(t, now, agentGitHubBudgetStatus{
		Agent: "scanner", Count: 300, Cap: 300, RetryAfter: 900, Reason: "cap",
	})
	if want := "Stop polling GitHub. gh returned 429 hourly cap reached; stop all GitHub reads for this session and finish with local work."; nudge != want {
		t.Fatalf("cap nudge = %q, want %q", nudge, want)
	}
	if want := "hive: agent GitHub API hourly cap reached (300/300); stop polling and continue with local work"; message != want {
		t.Fatalf("cap message = %q, want %q", message, want)
	}
}

// A refusal under github.agent_reserve_floor is the hive's shared budget, not
// the agent's cap. On knuckle (2026-11-08 11:35:22Z) sec-check was refused at
// 379/400, read "hourly cap reached ... stop all GitHub reads for this
// session", and gave up its whole audit. The wording must say whose budget it
// is, when it resets, and that the agent pauses rather than stops.
func TestAgentGitHubBudgetReserveRefusalNamesSharedBudgetAndReset(t *testing.T) {
	now := time.Date(2026, 11, 8, 11, 35, 22, 0, time.UTC)
	nudge, message, resp := readAgentGitHubBudget429(t, now, agentGitHubBudgetStatus{
		Agent: "sec-check", Count: 379, Cap: 400, RetryAfter: 1260, Reason: "reserve",
		Method: http.MethodGet, Path: "/repos/o/r/contents/x",
	})
	if got := resp.Header.Get("Retry-After"); got != "1260" {
		t.Fatalf("Retry-After = %q, want 1260", got)
	}
	for name, text := range map[string]string{"nudge": nudge, "message": message} {
		for _, want := range []string{
			"shared GitHub API budget is low (379 left, reserve floor 400)",
			"2026-11-08T11:56:22Z",
			"git fetch or clone may be too",
			"local work",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("reserve %s = %q, want it to contain %q", name, text, want)
			}
		}
		for _, bad := range []string{"hourly cap reached", "for this session", "Stop polling"} {
			if strings.Contains(text, bad) {
				t.Errorf("reserve %s = %q, must not contain %q", name, text, bad)
			}
		}
	}
	if !strings.Contains(nudge, "about 21 min") {
		t.Errorf("reserve nudge = %q, want the wait in minutes", nudge)
	}
	if !strings.Contains(nudge, "do not end the task") {
		t.Errorf("reserve nudge = %q, want it to say the task goes on", nudge)
	}
	if !strings.Contains(message, "Retry-After 1260s") {
		t.Errorf("reserve message = %q, want the Retry-After seconds", message)
	}
}

func TestAgentGitHubBudgetCapWarnOncePerWindow(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	p := &GitHubProxy{
		logger:       slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		githubBudget: newAgentGitHubBudget(func() time.Time { return now }),
	}
	refuse := func() {
		server, client := net.Pipe()
		defer server.Close()
		defer client.Close()
		go func() { _, _ = io.Copy(io.Discard, client) }()
		p.writeAgentGitHubBudget429(server, agentGitHubBudgetStatus{Agent: "scanner", Count: 3, Cap: 3, RetryAfter: 5, Reason: "cap"})
	}
	warns := func() int { return strings.Count(buf.String(), "level=WARN") }
	for i := 0; i < 5; i++ {
		refuse()
	}
	if got := warns(); got != 1 {
		t.Fatalf("WARNs in first window = %d, want 1\n%s", got, buf.String())
	}
	if got := strings.Count(buf.String(), "level=DEBUG"); got != 4 {
		t.Fatalf("DEBUG repeats = %d, want 4", got)
	}
	now = now.Add(time.Hour + time.Second)
	refuse()
	if got := warns(); got != 2 {
		t.Fatalf("WARNs after window reset = %d, want 2", got)
	}
}

// The review relay sets aside a requires_human from a session the proxy cut
// short (projectbluefin/utah-packages#959). It can only do that if the proxy
// remembers, per agent, when it last refused a request: for the hourly cap
// and for the reserve floor, and for nobody else.
func TestAgentGitHubRefusedSinceRecordsCapAndReserveRefusals(t *testing.T) {
	now := time.Date(2026, 11, 9, 1, 33, 0, 0, time.UTC)
	remaining := 5000
	p := &GitHubProxy{
		githubBudget:       newAgentGitHubBudget(func() time.Time { return now }),
		githubBudgetCap:    func(string) int { return 3 },
		githubReserveFloor: func() int { return 400 },
		githubReserveSnapshot: func() (int, time.Time, bool) {
			return remaining, now.Add(10 * time.Minute), true
		},
	}
	dispatched := now.Add(-20 * time.Minute)
	for i := 0; i < 3; i++ {
		if st := p.githubBudgetRefusal("reviewer", http.MethodGet, "/repos/o/r/pulls/1", true); st != nil {
			t.Fatalf("request %d refused under the cap: %+v", i, st)
		}
		p.recordAgentGitHubBudget("reviewer", "/repos/o/r/pulls/1", http.StatusOK, 3)
	}
	if p.AgentGitHubRefusedSince("reviewer", dispatched) {
		t.Fatal("an agent under its cap reads as refused")
	}
	if st := p.githubBudgetRefusal("reviewer", http.MethodGet, "/repos/o/r/pulls/1/files", true); st == nil || st.Reason != "cap" {
		t.Fatalf("fourth request = %+v, want a cap refusal", st)
	}
	if !p.AgentGitHubRefusedSince("reviewer", dispatched) {
		t.Fatal("the cap refusal was not recorded against the session that was running")
	}
	if !p.AgentGitHubRefusedSince("reviewer", now) {
		t.Fatal("a refusal at the session start must count")
	}
	// The planner waits for the cap window to free before asking again, so
	// the refusal carries its Retry-After: here the first of the three
	// requests (all at now) leaves the sliding hour at now+1h.
	if clears, refused := p.AgentGitHubRefusal("reviewer", dispatched); !refused || !clears.Equal(now.Add(agentGitHubBudgetWindow)) {
		t.Fatalf("AgentGitHubRefusal = %v, %v, want refused until %v", clears, refused, now.Add(agentGitHubBudgetWindow))
	}
	if p.AgentGitHubRefusedSince("reviewer", now.Add(time.Second)) {
		t.Fatal("a session asked after the refusal reads as refused")
	}
	if p.AgentGitHubRefusedSince("scanner", dispatched) {
		t.Fatal("another agent's refusal leaked")
	}

	remaining = 100
	if st := p.githubBudgetRefusal("scanner", http.MethodGet, "/repos/o/r/issues", true); st == nil || st.Reason != "reserve" {
		t.Fatalf("scanner read below the floor = %+v, want a reserve refusal", st)
	}
	if !p.AgentGitHubRefusedSince("scanner", dispatched) {
		t.Fatal("the reserve-floor refusal was not recorded")
	}
	if clears, _ := p.AgentGitHubRefusal("scanner", dispatched); !clears.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("reserve refusal clears at %v, want the bucket reset %v", clears, now.Add(10*time.Minute))
	}
	if p.AgentGitHubRefusedSince(internalCallerName, dispatched) {
		t.Fatal("the hive's own calls are never refused or recorded")
	}
	var nilProxy *GitHubProxy
	if nilProxy.AgentGitHubRefusedSince("reviewer", dispatched) {
		t.Fatal("a nil proxy reads as refused")
	}
	if _, refused := nilProxy.AgentGitHubRefusal("reviewer", dispatched); refused {
		t.Fatal("a nil proxy's AgentGitHubRefusal reads as refused")
	}
}
