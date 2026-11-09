package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/config"
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
	nudged map[string]time.Time
	// refused is the last refusal of each agent's GitHub request by its
	// hourly cap or the shared reserve floor. The review relay reads it
	// (AgentGitHubRefusedSince) to tell a review the proxy cut short from
	// one that read the PR, and the review planner reads it
	// (AgentGitHubRefusal) to tell an expiry the cap caused from one it did
	// not, and to wait for the window to free before asking again.
	refused map[string]agentGitHubRefusal
}

// agentGitHubRefusal is one refusal: when it happened, and when the
// refusal's Retry-After said the agent could read again (the sliding hour
// freeing a slot, or the shared bucket's reset).
type agentGitHubRefusal struct {
	at     time.Time
	clears time.Time
}

func newAgentGitHubBudget(now func() time.Time) *agentGitHubBudget {
	if now == nil {
		now = time.Now
	}
	return &agentGitHubBudget{
		now:     now,
		events:  make(map[string][]time.Time),
		warned:  make(map[string]bool),
		nudged:  make(map[string]time.Time),
		refused: make(map[string]agentGitHubRefusal),
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
	if until, ok := b.nudged[agentName]; ok && now.Before(until) {
		return false
	}
	b.nudged[agentName] = now.Add(agentGitHubBudgetWindow)
	return true
}

// markRefused records that agentName was just refused a GitHub request
// whose Retry-After was retryAfter seconds.
func (b *agentGitHubBudget) markRefused(agentName string, retryAfter int) {
	if b == nil || strings.TrimSpace(agentName) == "" {
		return
	}
	now := b.now()
	if retryAfter < 0 {
		retryAfter = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refused == nil {
		b.refused = make(map[string]agentGitHubRefusal)
	}
	b.refused[agentName] = agentGitHubRefusal{at: now, clears: now.Add(time.Duration(retryAfter) * time.Second)}
}

// refusedSince reports whether agentName was refused a GitHub request at or
// after since and, if so, when the last refusal said it would clear.
func (b *agentGitHubBudget) refusedSince(agentName string, since time.Time) (clears time.Time, refused bool) {
	if b == nil || strings.TrimSpace(agentName) == "" {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	last, ok := b.refused[agentName]
	if !ok || last.at.Before(since) {
		return time.Time{}, false
	}
	return last.clears, true
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

// AgentGitHubRefusedSince reports whether the proxy refused one of
// agentName's GitHub requests at or after since, because the agent reached
// agents.github_api_hourly_cap or the shared bucket fell below
// github.agent_reserve_floor. The review relay uses it to set aside a
// requires_human from a review session the proxy cut short: the session
// could not read what it needed, so its verdict is no verdict
// (projectbluefin/utah-packages#959). The record lives in memory only; a
// restart forgets it, as it forgets the counts.
func (p *GitHubProxy) AgentGitHubRefusedSince(agentName string, since time.Time) bool {
	_, refused := p.AgentGitHubRefusal(agentName, since)
	return refused
}

// AgentGitHubRefusal is AgentGitHubRefusedSince plus when the last refusal
// said the agent could read again: its Retry-After, which is when the
// sliding hour frees a slot under the cap, or the shared bucket's reset
// under the reserve floor. The review planner uses it so an expiry the cap
// caused does not spend review.max_redispatch, and so the perspective is
// asked again only once the agent can read.
func (p *GitHubProxy) AgentGitHubRefusal(agentName string, since time.Time) (clears time.Time, refused bool) {
	if p == nil || p.githubBudget == nil {
		return time.Time{}, false
	}
	return p.githubBudget.refusedSince(agentName, since)
}

func (p *GitHubProxy) githubBudgetRefusal(agentName, method, path string, isRead bool) (status *agentGitHubBudgetStatus) {
	if p == nil || agentName == "" || agentName == internalCallerName {
		return nil
	}
	defer func() {
		if status != nil {
			p.githubBudget.markRefused(agentName, status.RetryAfter)
		}
	}()
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
	nudge, message := agentGitHubBudgetMessages(st, p.agentGitHubBudgetNow())
	first := p.githubBudget != nil && p.githubBudget.markNudged(st.Agent)
	if first && p.githubBudgetNudge != nil {
		p.githubBudgetNudge(st.Agent, nudge)
	}
	if p.logger != nil {
		// Warn once per agent per cap window; repeats are expected and self-limiting.
		level := slog.LevelDebug
		if first {
			level = slog.LevelWarn
		}
		p.logger.Log(context.Background(), level, "agent GitHub API hourly cap reached",
			"agent", st.Agent, "count", st.Count, "cap", st.Cap, "retry_after", st.RetryAfter, "reason", st.Reason)
	}
	payload, _ := json.Marshal(map[string]string{
		"message":           message,
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

func (p *GitHubProxy) agentGitHubBudgetNow() time.Time {
	if p != nil && p.githubBudget != nil && p.githubBudget.now != nil {
		return p.githubBudget.now()
	}
	return time.Now()
}

// agentGitHubBudgetMessages returns the nudge typed into the agent's pane and
// the 429 body gh prints for a budget refusal.
//
// The two refusals need different advice. "cap" is the agent's own
// agents.github_api_hourly_cap: it has polled too much, so it stops GitHub
// reads for the session. "reserve" is github.agent_reserve_floor: the App
// installation's shared core bucket, which the daemon also spends, is nearly
// empty. That is not the agent's doing and it ends at the bucket's reset, so
// the agent pauses until then rather than giving up. Saying "hourly cap
// reached ... for this session" there made sec-check abandon its whole audit
// on knuckle (2026-11-08, refused at 379/400).
//
// The reserve wording warns that git fetch and clone may be refused too: by
// default github.com is tunneled and never reaches this gate, but when it is
// intercepted (HIVE_PROXY_INJECT_GH_AUTH, or a lane under
// write_surface.enforce) agentGitHubRequestIsRead counts info/refs and
// git-upload-pack as reads, so the floor refuses them as well.
func agentGitHubBudgetMessages(st agentGitHubBudgetStatus, now time.Time) (nudge, message string) {
	if st.Reason != "reserve" {
		return "Stop polling GitHub. gh returned 429 hourly cap reached; stop all GitHub reads for this session and finish with local work.",
			"hive: agent GitHub API hourly cap reached (" + strconv.Itoa(st.Count) + "/" + strconv.Itoa(st.Cap) + "); stop polling and continue with local work"
	}
	retry := st.RetryAfter
	if retry <= 0 {
		retry = 1
	}
	until := now.Add(time.Duration(retry) * time.Second).UTC().Format(time.RFC3339)
	minutes := strconv.Itoa((retry + 59) / 60)
	left := strconv.Itoa(st.Count) + " left, reserve floor " + strconv.Itoa(st.Cap)
	nudge = "Pause GitHub reads until " + until + " (about " + minutes + " min), then try again. " +
		"gh returned 429 because the hive's shared GitHub API budget is low (" + left + "), not because you reached your own cap. " +
		"Until then gh and other GitHub API reads are refused, and git fetch or clone may be too; carry on with local work and do not end the task over this."
	message = "hive: the hive's shared GitHub API budget is low (" + left + "); this is not this agent's own cap. " +
		"GitHub API reads are paused until " + until + " (Retry-After " + strconv.Itoa(retry) + "s), and git fetch or clone may be too; " +
		"try again then and continue with local work meanwhile"
	return nudge, message
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
