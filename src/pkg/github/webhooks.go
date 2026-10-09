package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const webhookStatsWindow = time.Hour

type WebhookHealthSnapshot struct {
	Healthy         bool      `json:"healthy"`
	LastEventAt     time.Time `json:"last_event_at,omitzero"`
	Events1h        int       `json:"events_1h"`
	Invalidations1h int       `json:"invalidations_1h"`
	IntervalS       int       `json:"interval_s"`
}

type webhookHealthTracker struct {
	mu              sync.Mutex
	clock           func() time.Time
	lastEventByRepo map[string]time.Time
	events          []time.Time
	invalidations   []time.Time
	lastHealthy     *bool
}

func (t *webhookHealthTracker) now() time.Time {
	if t.clock != nil {
		return t.clock()
	}
	return time.Now()
}

func (t *webhookHealthTracker) setClockForTest(fn func() time.Time) {
	t.mu.Lock()
	t.clock = fn
	t.mu.Unlock()
}

func (t *webhookHealthTracker) record(repo string, invalidations int) {
	now := t.now()
	repo = canonicalPRDetailRepo(repo)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastEventByRepo == nil {
		t.lastEventByRepo = map[string]time.Time{}
	}
	if repo != "" {
		t.lastEventByRepo[repo] = now
	}
	t.events = append(t.events, now)
	for i := 0; i < invalidations; i++ {
		t.invalidations = append(t.invalidations, now)
	}
}

func pruneTimes(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for ; i < len(ts); i++ {
		if !ts[i].Before(cutoff) {
			break
		}
	}
	return ts[i:]
}

func (t *webhookHealthTracker) snapshot(interval time.Duration, logger *slog.Logger) WebhookHealthSnapshot {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	now := t.now()
	cutoff := now.Add(-webhookStatsWindow)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = pruneTimes(t.events, cutoff)
	t.invalidations = pruneTimes(t.invalidations, cutoff)
	var last time.Time
	for _, at := range t.lastEventByRepo {
		if at.After(last) {
			last = at
		}
	}
	healthy := !last.IsZero() && now.Sub(last) < 2*interval
	if t.lastHealthy == nil {
		v := healthy
		t.lastHealthy = &v
	} else if *t.lastHealthy != healthy {
		if logger != nil {
			if healthy {
				logger.Info("github webhook health recovered", "last_event_at", last, "interval_s", int(interval.Seconds()))
			} else {
				logger.Warn("github webhook health stale", "last_event_at", last, "interval_s", int(interval.Seconds()))
			}
		}
		*t.lastHealthy = healthy
	}
	return WebhookHealthSnapshot{Healthy: healthy, LastEventAt: last, Events1h: len(t.events), Invalidations1h: len(t.invalidations), IntervalS: int(interval.Seconds())}
}

func (c *Client) SetWebhookClockForTest(fn func() time.Time) {
	if c != nil {
		c.webhooks.setClockForTest(fn)
	}
}

func (c *Client) WebhookHealth(interval time.Duration) WebhookHealthSnapshot {
	if c == nil {
		return WebhookHealthSnapshot{IntervalS: int(interval.Seconds())}
	}
	s := c.webhooks.snapshot(interval, c.logger)
	c.webhookCacheHealthy.Store(s.Healthy)
	return s
}

type WebhookInvalidationResult struct {
	Repo          string
	Invalidations int
	Ignored       bool
}

type webhookRepo struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}
type webhookPRRef struct {
	Number int `json:"number"`
	Head   struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type webhookPayload struct {
	Repository  webhookRepo `json:"repository"`
	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int `json:"number"`
		PullRequest any `json:"pull_request"`
	} `json:"issue"`
	CheckSuite *struct {
		HeadSHA      string         `json:"head_sha"`
		PullRequests []webhookPRRef `json:"pull_requests"`
	} `json:"check_suite"`
	CheckRun *struct {
		HeadSHA      string         `json:"head_sha"`
		PullRequests []webhookPRRef `json:"pull_requests"`
	} `json:"check_run"`
	SHA          string         `json:"sha"`
	PullRequests []webhookPRRef `json:"pull_requests"`
	Ref          string         `json:"ref"`
}

func (c *Client) HandleWebhookInvalidation(_ context.Context, event string, body []byte) (WebhookInvalidationResult, error) {
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return WebhookInvalidationResult{}, err
	}
	repo := p.Repository.FullName
	if repo == "" && p.Repository.Owner.Login != "" && p.Repository.Name != "" {
		repo = p.Repository.Owner.Login + "/" + p.Repository.Name
	}
	if !c.webhookRepoKnown(repo) {
		return WebhookInvalidationResult{Repo: repo, Ignored: true}, nil
	}
	n := 0
	invalidate := func(num int) {
		if num > 0 {
			if c.Invalidate(repo, num) {
				n++
			}
		}
	}
	invalidatePRRefs := func(refs []webhookPRRef, sha string) {
		seen := map[int]bool{}
		for _, pr := range refs {
			if pr.Number > 0 {
				seen[pr.Number] = true
				invalidate(pr.Number)
			}
		}
		if len(seen) == 0 && strings.TrimSpace(sha) != "" {
			for _, num := range c.cachedPRNumbersByHeadSHA(repo, sha) {
				invalidate(num)
			}
		}
	}
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		if p.PullRequest != nil {
			invalidate(p.PullRequest.Number)
		}
	case "issue_comment":
		if p.Issue != nil && p.Issue.PullRequest != nil {
			invalidate(p.Issue.Number)
		}
	case "check_suite":
		if p.CheckSuite != nil {
			invalidatePRRefs(p.CheckSuite.PullRequests, p.CheckSuite.HeadSHA)
		}
	case "check_run":
		if p.CheckRun != nil {
			invalidatePRRefs(p.CheckRun.PullRequests, p.CheckRun.HeadSHA)
		}
	case "status":
		invalidatePRRefs(p.PullRequests, p.SHA)
	case "push":
		ref := strings.TrimPrefix(strings.TrimSpace(p.Ref), "refs/heads/")
		n += c.InvalidateRepoRefs(repo, ref, ref)
	}
	c.webhooks.record(repo, n)
	return WebhookInvalidationResult{Repo: repo, Invalidations: n}, nil
}

func (c *Client) webhookRepoKnown(repo string) bool {
	if c == nil {
		return false
	}
	repo = canonicalPRDetailRepo(repo)
	if repo == "" {
		return false
	}
	for _, r := range c.getRepos() {
		cr := canonicalPRDetailRepo(r)
		if cr == repo {
			return true
		}
		if !strings.Contains(cr, "/") && canonicalPRDetailRepo(c.org+"/"+cr) == repo {
			return true
		}
	}
	return false
}
