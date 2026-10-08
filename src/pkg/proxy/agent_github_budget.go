package proxy

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	agentGitHubBudgetWindow = time.Hour
	githubAPICapDocsURL     = "https://github.com/hivecommons/hive/blob/main/src/docs/operator-reference.md#github-api-quota"
)

type agentGitHubBudget struct {
	mu     sync.Mutex
	now    func() time.Time
	events map[string][]time.Time
	warned map[string]bool
	nudged map[string]bool
}

func newAgentGitHubBudget(now func() time.Time) *agentGitHubBudget {
	if now == nil {
		now = time.Now
	}
	return &agentGitHubBudget{
		now:    now,
		events: make(map[string][]time.Time),
		warned: make(map[string]bool),
		nudged: make(map[string]bool),
	}
}

func (b *agentGitHubBudget) check(agentName string, cap int) (count int, retryAfter int, over bool) {
	if b == nil || cap <= 0 || strings.TrimSpace(agentName) == "" {
		return 0, 0, false
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	events := b.pruneLocked(agentName, now)
	count = len(events)
	if count < cap {
		return count, 0, false
	}
	return count, retryAfterSeconds(now, events[0].Add(agentGitHubBudgetWindow)), true
}

func (b *agentGitHubBudget) record(agentName string, status, cap int) (count int, warn bool) {
	if b == nil || cap <= 0 || strings.TrimSpace(agentName) == "" || status == http.StatusNotModified {
		return 0, false
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	events := append(b.pruneLocked(agentName, now), now)
	b.events[agentName] = events
	count = len(events)
	threshold := int(math.Ceil(float64(cap) * 0.8))
	if threshold < 1 {
		threshold = 1
	}
	if count >= threshold && !b.warned[agentName] {
		b.warned[agentName] = true
		warn = true
	}
	return count, warn
}

func (b *agentGitHubBudget) markNudged(agentName string) bool {
	if b == nil || strings.TrimSpace(agentName) == "" {
		return false
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = b.pruneLocked(agentName, now)
	if b.nudged[agentName] {
		return false
	}
	b.nudged[agentName] = true
	return true
}

func (b *agentGitHubBudget) pruneLocked(agentName string, now time.Time) []time.Time {
	events := b.events[agentName]
	cutoff := now.Add(-agentGitHubBudgetWindow)
	idx := 0
	for idx < len(events) && !events[idx].After(cutoff) {
		idx++
	}
	if idx > 0 {
		events = append([]time.Time(nil), events[idx:]...)
		b.events[agentName] = events
	}
	if len(events) == 0 {
		delete(b.events, agentName)
		delete(b.warned, agentName)
		delete(b.nudged, agentName)
	}
	return events
}

func retryAfterSeconds(now, then time.Time) int {
	if !then.After(now) {
		return 1
	}
	secs := int(math.Ceil(then.Sub(now).Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

func (p *GitHubProxy) agentGitHubCap(agentName string) int {
	if p != nil && p.githubBudgetCap != nil {
		return p.githubBudgetCap(agentName)
	}
	return config.DefaultAgentsGitHubAPIHourlyCap
}

func (p *GitHubProxy) agentGitHubReserveFloor() int {
	if p != nil && p.githubReserveFloor != nil {
		return p.githubReserveFloor()
	}
	return config.DefaultGitHubAgentReserveFloor
}

func (p *GitHubProxy) agentGitHubReserveSnapshot() (remaining int, reset time.Time, ok bool) {
	if p != nil && p.githubReserveSnapshot != nil {
		return p.githubReserveSnapshot()
	}
	return 0, time.Time{}, false
}

func (p *GitHubProxy) githubBudgetRefusal(agentName, method, path string, isRead bool) (status *agentGitHubBudgetStatus) {
	if p == nil || agentName == "" || agentName == internalCallerName {
		return nil
	}
	cap := p.agentGitHubCap(agentName)
	if cap > 0 && p.githubBudget != nil {
		count, retryAfter, over := p.githubBudget.check(agentName, cap)
		if over {
			return &agentGitHubBudgetStatus{Agent: agentName, Count: count, Cap: cap, RetryAfter: retryAfter, Reason: "cap"}
		}
	}
	floor := p.agentGitHubReserveFloor()
	if isRead && floor > 0 {
		remaining, reset, ok := p.agentGitHubReserveSnapshot()
		if ok && remaining < floor {
			now := time.Now()
			if p.githubBudget != nil && p.githubBudget.now != nil {
				now = p.githubBudget.now()
			}
			return &agentGitHubBudgetStatus{
				Agent: agentName, Count: remaining, Cap: floor,
				RetryAfter: retryAfterSeconds(now, reset), Reason: "reserve",
				Method: method, Path: path,
			}
		}
	}
	return nil
}

func (p *GitHubProxy) recordAgentGitHubBudget(agentName, endpoint string, status, cap int) {
	if p == nil || p.githubBudget == nil || agentName == "" || agentName == internalCallerName {
		return
	}
	count, warn := p.githubBudget.record(agentName, status, cap)
	if warn && p.logger != nil {
		p.logger.Warn("agent GitHub API hourly budget above soft threshold",
			"agent", agentName, "count", count, "cap", cap, "endpoint", endpoint)
	}
}

type agentGitHubBudgetStatus struct {
	Agent      string
	Count      int
	Cap        int
	RetryAfter int
	Reason     string
	Method     string
	Path       string
}

func (p *GitHubProxy) writeAgentGitHubBudget429(client netWriterConn, st agentGitHubBudgetStatus) bool {
	if st.RetryAfter <= 0 {
		st.RetryAfter = 1
	}
	if p.githubBudget != nil && p.githubBudget.markNudged(st.Agent) && p.githubBudgetNudge != nil {
		p.githubBudgetNudge(st.Agent, "Stop polling GitHub. gh returned 429 hourly cap reached; stop all GitHub reads for this session and finish with local work.")
	}
	if p.logger != nil {
		p.logger.Warn("agent GitHub API hourly cap reached",
			"agent", st.Agent, "count", st.Count, "cap", st.Cap, "retry_after", st.RetryAfter, "reason", st.Reason)
	}
	payload, _ := json.Marshal(map[string]string{
		"message":           "hive: agent GitHub API hourly cap reached (" + strconv.Itoa(st.Count) + "/" + strconv.Itoa(st.Cap) + "); stop polling and continue with local work",
		"documentation_url": githubAPICapDocsURL,
	})
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(payload) + "\n")),
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Retry-After", strconv.Itoa(st.RetryAfter))
	resp.Header.Set("X-Hive-Proxy-Blocked", "true")
	_ = client.SetWriteDeadline(time.Now().Add(httpWriteTimeout))
	writeErr := resp.Write(client)
	_ = client.SetWriteDeadline(time.Time{})
	if writeErr != nil && p.logger != nil {
		p.logTimeout("proxy GitHub budget response write timed out", writeErr, "agent", st.Agent)
		return false
	}
	return true
}

type netWriterConn interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}

func agentGitHubRequestIsRead(method, path string, graphQLMutation bool) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return true
	}
	if method == http.MethodPost && (gitUploadPackPath.MatchString(path) || (IsGraphQLPath(path) && !graphQLMutation)) {
		return true
	}
	return false
}
