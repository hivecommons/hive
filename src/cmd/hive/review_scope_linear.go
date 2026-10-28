package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/worksource"
)

// reviewScopeIssueReader reads a Linear scope-contract issue read-only for
// a review kick (hivecommons/hive#14906). *worksource.LinearSource is the
// production reader; the fetch happens hive-side so reviewers never hold a
// Linear token.
type reviewScopeIssueReader interface {
	ScopeContractIssue(ctx context.Context, identifier string) (worksource.LinearScopeIssue, error)
}

// reviewScopeLinear keeps one Linear source across eval cycles so its
// per-issue cache spans kicks. Rebuilt when the work-source config changes.
var reviewScopeLinear struct {
	mu  sync.Mutex
	key string
	src *worksource.LinearSource
}

// reviewScopeLinearReader returns the Linear reader for scope contracts, or
// nil when no Linear team is configured or the source cannot be built (the
// prompt then names the identifier it could not fetch).
func reviewScopeLinearReader(cfg *config.Config, logger *slog.Logger) reviewScopeIssueReader {
	if cfg == nil || len(cfg.Governor.WorkSource.Linear.Teams) == 0 {
		return nil
	}
	ws := cfg.Governor.WorkSource
	key := reviewScopeLinearKey(ws.Linear)
	reviewScopeLinear.mu.Lock()
	defer reviewScopeLinear.mu.Unlock()
	if reviewScopeLinear.src != nil && reviewScopeLinear.key == key {
		return reviewScopeLinear.src
	}
	src, err := worksource.LinearScopeSource(ws, logger)
	if err != nil {
		if logger != nil {
			logger.Debug("review scope contract: Linear source unavailable", "error", err)
		}
		return nil
	}
	reviewScopeLinear.key, reviewScopeLinear.src = key, src
	return src
}

// reviewScopeLinearKey identifies the Linear settings the cached source was
// built from, so a team or key change rebuilds it.
func reviewScopeLinearKey(l config.LinearSourceConfig) string {
	b, _ := json.Marshal(l)
	return string(b)
}

// reviewScopeFailTTL bounds how long a failed Linear read is not retried, so
// an outage costs one timeout per issue per window, not per eval cycle.
const reviewScopeFailTTL = 2 * time.Minute

var reviewScopeFailures struct {
	mu sync.Mutex
	at map[string]time.Time
}

func reviewScopeRecentlyFailed(id string) bool {
	reviewScopeFailures.mu.Lock()
	defer reviewScopeFailures.mu.Unlock()
	t, ok := reviewScopeFailures.at[id]
	if ok && time.Since(t) >= reviewScopeFailTTL {
		delete(reviewScopeFailures.at, id)
		return false
	}
	return ok
}

func reviewScopeMarkFailed(id string) {
	reviewScopeFailures.mu.Lock()
	defer reviewScopeFailures.mu.Unlock()
	if reviewScopeFailures.at == nil {
		reviewScopeFailures.at = map[string]time.Time{}
	}
	reviewScopeFailures.at[id] = time.Now()
}

// reviewScopeContractFor fills the Linear issue text for a PR whose scope
// contract is a Linear issue. A failed read leaves the text empty, and the
// prompt falls back to naming the identifier.
func reviewScopeContractFor(ctx context.Context, reader reviewScopeIssueReader, item *review.PullRequest, logger *slog.Logger) {
	id, ok := review.LinearScopeContractID(item.ScopeContract)
	if !ok || reader == nil {
		return
	}
	if reviewScopeRecentlyFailed(id) {
		return
	}
	issue, err := reader.ScopeContractIssue(ctx, id)
	if err != nil {
		reviewScopeMarkFailed(id)
		if logger != nil {
			logger.Debug("review scope contract: Linear issue not fetched", "repo", item.Repo, "pr", item.Number, "issue", id, "error", err)
		}
		return
	}
	item.ScopeContractState, item.ScopeContractText = issue.State, issue.Text
}
