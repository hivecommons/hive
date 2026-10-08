package proxy

import (
	"bufio"
	"encoding/json"
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
