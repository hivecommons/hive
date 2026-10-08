package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// dibsPublicCheckTTL is how long a definitive verdict (public or not) is
	// trusted before re-verification. Positive AND negative verdicts are
	// cached: a repo flipping visibility is picked up within one TTL, and the
	// unauthenticated GitHub rate limit (60/h) comfortably covers the fleet's
	// ~30 real repos re-checked once per hour.
	dibsPublicCheckTTL = time.Hour
	// dibsPublicCheckErrTTL is the retry-after for a TRANSIENT failure
	// (network error, 5xx, rate limit): the prior verdict — or the
	// excluded-by-default when there is none — is kept, and the next check
	// happens sooner than a full TTL so an outage does not pin a stale answer
	// for an hour.
	dibsPublicCheckErrTTL = 5 * time.Minute
	// dibsPublicCheckTimeout bounds one GitHub API round-trip. Checks are
	// background work; a hung connection must release its concurrency slot
	// promptly rather than pile up goroutines.
	dibsPublicCheckTimeout = 5 * time.Second
	// dibsPublicCheckParallel bounds concurrent GitHub API checks so a cold
	// cache over the whole fleet cannot open dozens of simultaneous
	// connections. Repos over the bound are retried on the next poll.
	dibsPublicCheckParallel = 4
)

// dibsPublicVerdict is one cached answer: whether the repo was public when
// last checked, and when the answer stops being trusted.
type dibsPublicVerdict struct {
	public  bool
	expires time.Time
}

// dibsPublicChecker is the mutex-guarded verdict cache plus the background
// refresh machinery. One per HubServer, created lazily by dibsChecker.
type dibsPublicChecker struct {
	mu       sync.Mutex
	verdicts map[string]dibsPublicVerdict
	inflight map[string]bool
	// sem bounds concurrent background checks (dibsPublicCheckParallel). A
	// slot is acquired under mu in isPublic and released by refresh.
	sem     chan struct{}
	apiBase string // test seam; production is https://api.github.com
	client  *http.Client
	logger  *slog.Logger
	now     func() time.Time // test seam for TTL expiry
}

func newDibsPublicChecker(logger *slog.Logger) *dibsPublicChecker {
	return &dibsPublicChecker{
		verdicts: map[string]dibsPublicVerdict{},
		inflight: map[string]bool{},
		sem:      make(chan struct{}, dibsPublicCheckParallel),
		apiBase:  "https://api.github.com",
		client:   hubGitHubHTTPClient(),
		logger:   logger,
		now:      time.Now,
	}
}

// dibsChecker returns the server's lazily-created public-repo checker. Tests
// pre-set s.dibsPublic (pointing apiBase at a fake GitHub API) before the
// first call and the Once respects it.
func (s *HubServer) dibsChecker() *dibsPublicChecker {
	s.dibsPublicOnce.Do(func() {
		if s.dibsPublic == nil {
			s.dibsPublic = newDibsPublicChecker(s.logger)
		}
	})
	return s.dibsPublic
}

// isPublic answers from the cache and NEVER blocks: a missing or expired
// verdict schedules one deduplicated background refresh (subject to the
// concurrency bound) and reports the current best answer — the stale verdict
// if one exists, otherwise excluded.
func (c *dibsPublicChecker) isPublic(repoID string) bool {
	c.mu.Lock()
	v, known := c.verdicts[repoID]
	fresh := known && c.now().Before(v.expires)
	launch := !fresh && !c.inflight[repoID]
	if launch {
		select {
		case c.sem <- struct{}{}:
			c.inflight[repoID] = true
		default:
			// Bound reached: skip this round; the next poll retries.
			launch = false
		}
	}
	c.mu.Unlock()
	if launch {
		go c.refresh(repoID)
	}
	return known && v.public
}

// refresh performs one GitHub API check and records the verdict. Runs in its
// own goroutine holding a sem slot acquired by isPublic.
func (c *dibsPublicChecker) refresh(repoID string) {
	public, definitive := c.check(repoID)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.inflight, repoID)
	<-c.sem
	now := c.now()
	if definitive {
		c.verdicts[repoID] = dibsPublicVerdict{public: public, expires: now.Add(dibsPublicCheckTTL)}
		return
	}
	// Transient failure: fall back to the prior verdict (zero value = excluded
	// when there is none) and retry sooner than a full TTL.
	prior := c.verdicts[repoID]
	c.verdicts[repoID] = dibsPublicVerdict{public: prior.public, expires: now.Add(dibsPublicCheckErrTTL)}
}

// check asks the GitHub API whether repoID ("owner/name") is a public repo.
// definitive=false means a transient failure the caller must not cache for a
// full TTL.
func (c *dibsPublicChecker) check(repoID string) (public, definitive bool) {
	owner, name, ok := strings.Cut(repoID, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		// Not an owner/repo pair at all — permanently excludable, no API call.
		return false, true
	}
	ctx, cancel := context.WithTimeout(hubGitHubCallerContext(context.Background(), "dibs_public_check"), dibsPublicCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.apiBase+"/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil)
	if err != nil {
		return false, true
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Warn("dibs public check: github unreachable", "repo", repoID, "error", err)
		return false, false
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var payload struct {
			Private *bool `json:"private"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil || payload.Private == nil {
			// A 200 without a parseable private field is not a verdict.
			return false, false
		}
		return !*payload.Private, true
	case http.StatusNotFound:
		// GitHub 404s both "private" and "nonexistent" to callers without
		// access — either way, not publicly listable.
		return false, true
	default:
		// 403/429 (rate limit), 5xx — transient; keep the prior verdict.
		return false, false
	}
}
