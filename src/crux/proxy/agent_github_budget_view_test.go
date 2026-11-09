package proxy

import (
	"net/http"
	"testing"
	"time"
)

// AgentGitHubCallsLastHour reports the same sliding-hour count the cap check
// reads, per agent, and reading it changes nothing the next check sees.
func TestAgentGitHubCallsLastHourMatchesTheCapWindow(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	b := newAgentGitHubBudget(func() time.Time { return now })
	p := &GitHubProxy{githubBudget: b}

	b.record("reviewer", http.StatusOK, 300)
	now = now.Add(30 * time.Minute)
	b.record("reviewer", http.StatusOK, 300)
	b.record("reviewer", http.StatusNotModified, 300) // a 304 is not charged
	b.record("scanner", http.StatusOK, 300)
	b.record("quality", http.StatusOK, 0) // uncapped: no window kept

	got := p.AgentGitHubCallsLastHour()
	if got["reviewer"] != 2 || got["scanner"] != 1 || len(got) != 2 {
		t.Fatalf("calls = %v, want reviewer 2, scanner 1, nothing else", got)
	}

	// 31 minutes on, the first reviewer call has left the window.
	now = now.Add(31 * time.Minute)
	got = p.AgentGitHubCallsLastHour()
	if got["reviewer"] != 1 || got["scanner"] != 1 {
		t.Fatalf("calls after the window slid = %v, want reviewer 1, scanner 1", got)
	}
	if count, _, _ := b.check("reviewer", 300); count != got["reviewer"] {
		t.Fatalf("cap check counts %d, view counts %d", count, got["reviewer"])
	}

	var nilProxy *GitHubProxy
	if got := nilProxy.AgentGitHubCallsLastHour(); len(got) != 0 {
		t.Fatalf("nil proxy = %v", got)
	}
}
