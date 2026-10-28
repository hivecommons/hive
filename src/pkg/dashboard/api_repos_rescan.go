package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// repoRescanDebounce is the server-side floor between two operator-initiated
// repository rescans. The Rescan button in the REPOSITORIES header is a
// one-click GitHub enumeration of every watched repo (issues + PRs + CI
// status), so — exactly like the ACMM Re-evaluate button it is modelled on —
// holding it down must not be able to spend the hive's API budget. A request
// inside the window is answered from the last scan's counts instead of
// re-hitting GitHub.
const repoRescanDebounce = 30 * time.Second

// repoRescanTimeout bounds one enumeration. A hive watching many repos, or a
// GitHub that is rate-limiting, must not leave the button spinning forever.
const repoRescanTimeout = 90 * time.Second

// ReposRescanResult is the response body of POST /api/repos/rescan.
//
// Status distinguishes the three outcomes the caller must render differently:
//
//	rescanned   — GitHub was queried; the counts below are from this scan.
//	debounced   — a scan finished less than repoRescanDebounce ago; the counts
//	              are that scan's, and RetryAfterS says when a new one is
//	              allowed. NOT an error: the data really is that fresh.
//	in-progress — another rescan is running right now; the counts are the
//	              previous scan's.
type ReposRescanResult struct {
	Status      string `json:"status"`
	Issues      int    `json:"issues"`
	PRs         int    `json:"prs"`
	Hold        int    `json:"hold"`
	Repos       int    `json:"repos"`
	ScannedAt   string `json:"scanned_at,omitempty"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
}

// handleReposRescan re-enumerates the watched repositories' open issues and
// pull requests on demand and republishes the dashboard status snapshot, so
// the REPOSITORIES cards show the current tickets without waiting out the
// governor's eval interval (up to several minutes in idle/quiet mode).
//
// It runs the READ-ONLY half of the governor's eval cycle — enumeration, CI
// enrichment and the duplicate-PR claim guard — and none of the side effects:
// no mode change, no agent kicks, no advisory posting, no escalation sweep.
// Pressing the button can therefore never start work; it only refreshes what
// the operator is looking at.
//
// Deliberately not owner-gated, matching the ACMM `?refresh=1` precedent it
// mirrors: this is a refresh of read-only data that any dashboard viewer can
// already see, and the far more expensive ACMM re-evaluation is ungated too.
// The API budget is protected by repoRescanDebounce, not by a role.
func (s *Server) handleReposRescan(w http.ResponseWriter, r *http.Request) {
	// Activity Monitor retries identify their rows; other rescan callers keep
	// the existing empty-body contract.
	var body struct {
		Items []struct {
			Repo   string `json:"repo"`
			Number int    `json:"number"`
		} `json:"items"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	if len(body.Items) > 500 {
		jsonError(w, "too many retry items", http.StatusBadRequest)
		return
	}
	seen := map[string]bool{}
	for _, item := range body.Items {
		if item.Number <= 0 || !s.prQueueRepoAllowed(item.Repo) {
			jsonError(w, "invalid retry item", http.StatusBadRequest)
			return
		}
	}
	var finishes []func()
	for _, item := range body.Items {
		key := item.Repo + "#" + strconv.Itoa(item.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		var finish func()
		w, finish = s.observeOperatorAction(w, r, item.Repo, item.Number, "retry", "")
		finishes = append(finishes, finish)
	}
	defer func() {
		for _, finish := range finishes {
			finish()
		}
	}()

	if s.deps == nil || s.deps.RescanReposFunc == nil {
		jsonError(w, "repository rescan is not available on this hive", http.StatusServiceUnavailable)
		return
	}

	s.repoRescanMu.Lock()
	// A rescan already in flight: answer immediately with the previous scan's
	// counts rather than queueing a second enumeration behind the first. Two
	// operators (or a double-click) must cost one GitHub sweep, not two.
	if s.repoRescanInFlight {
		last := s.repoRescanLast
		s.repoRescanMu.Unlock()
		last.Status = "in-progress"
		last.RetryAfterS = 0
		jsonResponse(w, last)
		return
	}
	if !s.repoRescanAt.IsZero() {
		if age := time.Since(s.repoRescanAt); age < repoRescanDebounce {
			last := s.repoRescanLast
			s.repoRescanMu.Unlock()
			last.Status = "debounced"
			last.RetryAfterS = int(math.Ceil((repoRescanDebounce - age).Seconds()))
			jsonResponse(w, last)
			return
		}
	}
	s.repoRescanInFlight = true
	s.repoRescanMu.Unlock()

	defer func() {
		s.repoRescanMu.Lock()
		s.repoRescanInFlight = false
		s.repoRescanMu.Unlock()
	}()

	// Bounded, and deliberately NOT derived from r.Context(): a browser tab
	// closed mid-scan would otherwise cancel the enumeration after the API
	// calls had already been spent, leaving the cards stale and the budget
	// gone. The scan finishes and publishes; only the response is lost.
	base := context.Background()
	if s.deps.Ctx != nil {
		base = s.deps.Ctx
	}
	ctx, cancel := context.WithTimeout(base, repoRescanTimeout)
	defer cancel()

	actionable, err := s.deps.RescanReposFunc(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("manual repository rescan failed", "error", err)
		}
		jsonError(w, "repository rescan failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	result := ReposRescanResult{Status: "rescanned", ScannedAt: time.Now().UTC().Format(time.RFC3339)}
	if actionable != nil {
		result.Issues = actionable.Issues.Count
		result.PRs = actionable.PRs.Count
		result.Hold = actionable.Hold.Total
		result.Repos = len(actionable.TotalByRepo)
	}
	if result.Repos == 0 && s.deps.Config != nil {
		result.Repos = len(s.deps.Config.Project.Repos)
	}

	s.repoRescanMu.Lock()
	s.repoRescanAt = time.Now()
	s.repoRescanLast = result
	s.repoRescanMu.Unlock()

	s.auditFromRequest(r, "repos_rescan", auditDetail(
		"issues", strconv.Itoa(result.Issues),
		"prs", strconv.Itoa(result.PRs),
		"repos", strconv.Itoa(result.Repos),
	), "")

	jsonResponse(w, result)
}
