package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
)

// forgeStateBinding memoizes, per "owner/repo", the advisory issue number
// the cache-backed lookup resolved, so the hot path is one cache read and
// never a list. The cache itself is the store SetForgeState attached
// (forgestate_reader.go).
type forgeStateBinding struct {
	mu           sync.RWMutex
	advisoryNums map[string]int
}

// forgeStateReader is the slice of *forgestate.Store the advisory lookup
// uses; an interface so tests can script the cache's answers.
type forgeStateReader interface {
	Snapshot(repo string) (forgestate.Snapshot, bool)
	ReadThrough(ctx context.Context, repo string, k forgestate.Kind, n int, bound time.Duration) (forgestate.Item, bool, error)
	WriteThrough(repo string, m forgestate.Mutation) error
}

// advisoryForgeStateForTest overrides the attached store for the advisory
// lookup only.
var advisoryForgeStateForTest forgestateReaderSeam

type forgestateReaderSeam struct {
	mu sync.RWMutex
	r  forgeStateReader
}

func (c *Client) advisoryForgeState() forgeStateReader {
	advisoryForgeStateForTest.mu.RLock()
	r := advisoryForgeStateForTest.r
	advisoryForgeStateForTest.mu.RUnlock()
	if r != nil {
		return r
	}
	if store := c.ForgeState(); store != nil {
		return store
	}
	return nil
}

func (c *Client) advisoryIssueMemo(repo string) (int, bool) {
	c.forgeState.mu.RLock()
	defer c.forgeState.mu.RUnlock()
	n, ok := c.forgeState.advisoryNums[repo]
	return n, ok && n > 0
}

func (c *Client) rememberAdvisoryIssue(repo string, n int) {
	c.forgeState.mu.Lock()
	defer c.forgeState.mu.Unlock()
	if n <= 0 {
		delete(c.forgeState.advisoryNums, repo)
		return
	}
	if c.forgeState.advisoryNums == nil {
		c.forgeState.advisoryNums = map[string]int{}
	}
	c.forgeState.advisoryNums[repo] = n
}

// errAdvisoryCacheMiss tells findAdvisoryIssue the cache could not answer and
// the legacy search/list path must run.
var errAdvisoryCacheMiss = errors.New("advisory issue not resolvable from forgestate cache")

// findAdvisoryIssueCached resolves the pinned advisory issue from the
// ForgeState cache (#13378). It returns (number, nil) for an open advisory
// issue, (0, nil) when the cache proves there is none to reuse, and
// errAdvisoryCacheMiss when the caller must fall back to the list path.
//
// Hot path: the memoized number is read through the cache within its
// freshness bound, so a healthy cycle costs zero GitHub calls. A memo or
// label hit whose cached copy is too old costs exactly one targeted GET. A
// closed advisory issue is reopened in place, as the list path does (#4167).
// Nothing here lists issues.
func (c *Client) findAdvisoryIssueCached(ctx context.Context, owner, repo string) (int, error) {
	store := c.advisoryForgeState()
	if store == nil {
		return 0, errAdvisoryCacheMiss
	}
	full := owner + "/" + repo
	if n, ok := c.advisoryIssueMemo(full); ok {
		return c.confirmAdvisoryIssue(ctx, store, owner, repo, n)
	}
	snap, ok := store.Snapshot(full)
	if !ok || !snap.Ready {
		return 0, errAdvisoryCacheMiss
	}
	// First discovery: the label narrows the candidates; one targeted GET
	// per candidate confirms the title before the number is memoized.
	for n, it := range snap.Issues {
		if !it.HasLabel(advisoryLabelName) || !it.Open() {
			continue
		}
		issue, _, err := c.client.Issues.Get(ctx, owner, repo, n)
		if err != nil || issue.IsPullRequest() || !isAdvisoryTitle(issue.GetTitle()) {
			continue
		}
		if issue.GetState() != "open" {
			continue
		}
		c.rememberAdvisoryIssue(full, n)
		c.logger.Info("found advisory issue via forgestate cache",
			slog.String("repo", full), slog.Int("number", n))
		return n, nil
	}
	// A ready snapshot with no labelled advisory issue: the issue may be
	// unlabelled or closed, which only the list path can tell apart from
	// "never created". That path runs once; the create or reopen memoizes.
	return 0, errAdvisoryCacheMiss
}

// confirmAdvisoryIssue checks a memoized advisory issue number against the
// cache and, when the cache cannot vouch for it, one targeted GET.
func (c *Client) confirmAdvisoryIssue(ctx context.Context, store forgeStateReader, owner, repo string, n int) (int, error) {
	full := owner + "/" + repo
	it, ok, err := store.ReadThrough(ctx, full, forgestate.KindIssue, n, 0)
	if err == nil && ok && it.Open() {
		return n, nil
	}
	// ok=false means the cache's confirming read saw it closed or gone;
	// err means the cache would not answer (not ready, stale, budget). Both
	// resolve with one targeted GET, never a list.
	issue, resp, getErr := c.client.Issues.Get(ctx, owner, repo, n)
	switch {
	case getErr != nil && resp != nil && resp.StatusCode == http.StatusNotFound:
		c.rememberAdvisoryIssue(full, 0)
		return 0, errAdvisoryCacheMiss
	case getErr != nil:
		return 0, fmt.Errorf("confirming advisory issue #%d: %w", n, getErr)
	case issue.IsPullRequest() || !isAdvisoryTitle(issue.GetTitle()):
		c.rememberAdvisoryIssue(full, 0)
		return 0, errAdvisoryCacheMiss
	case issue.GetState() == "open":
		return n, nil
	}
	// Closed: reuse it, as the list path's fallback 3 does.
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, n, &gh.IssueRequest{State: gh.Ptr("open")}); err != nil {
		c.logger.Warn("found closed advisory issue in cache but could not reopen it",
			slog.String("repo", full), slog.Int("number", n), slog.String("error", err.Error()))
		return 0, nil
	}
	_ = store.WriteThrough(full, forgestate.Mutation{Op: forgestate.OpReopen, Kind: forgestate.KindIssue, Number: n})
	c.logger.Info("reopened closed advisory issue (forgestate cache) instead of creating a duplicate",
		slog.String("repo", full), slog.Int("number", n))
	c.ensureAdvisoryLabel(ctx, owner, repo, n)
	return n, nil
}
