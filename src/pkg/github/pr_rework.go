package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

const maxPRReviewPages = 3
const PRReworkCacheFile = "pr-rework-cache.json"
const prReworkCacheSchemaVersion = 2

var (
	PRReworkCachePath = filepath.Join(ReviewLinksDir, PRReworkCacheFile)
	prReworkCacheMu   sync.Mutex
)

var (
	fixAttemptRE        = regexp.MustCompile(`(?i)\battempt\s+(\d+)\s*(?:/|of)\s*\d+`)
	fixAttemptTrailerRE = regexp.MustCompile(`(?im)^Hive-Fix-Attempt:\s*(\d+)\s*(?:/|of)\s*\d+\s*$`)
)

type prReviewEvent struct {
	State       string
	Author      string
	AuthorType  string
	CommitID    string
	Body        string
	SubmittedAt time.Time
}

type prCommentEvent struct {
	Author     string
	AuthorType string
	Body       string
	CreatedAt  time.Time
}

func (c *Client) enrichAttributedPRRework(ctx context.Context, prs []PullRequest) {
	if c == nil {
		return
	}
	cache := loadPRReworkCache()
	changed := false
	for i := range prs {
		if !prs[i].HiveAttributed {
			continue
		}
		key := ReviewLinkKey(prs[i].Repo, prs[i].Number)
		if cached, ok := cache[key]; ok {
			prs[i].Rework = cached
			continue
		}
		rework, err := c.fetchPRRework(ctx, prs[i])
		if err != nil {
			continue
		}
		prs[i].Rework = rework
		if !prs[i].MergedAt.IsZero() || !prs[i].ClosedAt.IsZero() || strings.EqualFold(prs[i].State, "closed") {
			cache[key] = rework
			changed = true
		}
	}
	if changed {
		storePRReworkCache(cache)
	}
}

func (c *Client) fetchPRRework(ctx context.Context, pr PullRequest) (PRReworkStats, error) {
	reviews, err := c.listPRReviewEvents(ctx, pr.Repo, pr.Number)
	if err != nil {
		return PRReworkStats{}, err
	}
	commits, err := c.ListPRCommits(ctx, pr.Repo, pr.Number)
	if err != nil {
		return PRReworkStats{}, err
	}
	comments, err := c.listPRCommentEvents(ctx, pr.Repo, pr.Number)
	if err != nil {
		return PRReworkStats{}, err
	}
	return BuildPRReworkStatsWithComments(pr, reviews, comments, commits), nil
}

func (c *Client) listPRReviewEvents(ctx context.Context, repo string, number int) ([]prReviewEvent, error) {
	owner, repoName := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: 100}
	var out []prReviewEvent
	for page := 0; page < maxPRReviewPages; page++ {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing reviews for %s/%s#%d: %w", owner, repoName, number, err)
		}
		for _, r := range reviews {
			if r == nil {
				continue
			}
			user := r.GetUser()
			out = append(out, prReviewEvent{
				State:       strings.ToUpper(strings.TrimSpace(r.GetState())),
				Author:      safeGetLogin(user),
				AuthorType:  strings.TrimSpace(user.GetType()),
				CommitID:    strings.TrimSpace(r.GetCommitID()),
				Body:        strings.TrimSpace(r.GetBody()),
				SubmittedAt: r.GetSubmittedAt().Time,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func BuildPRReworkStats(pr PullRequest, reviews []prReviewEvent, commits []PRCommit) PRReworkStats {
	return BuildPRReworkStatsWithComments(pr, reviews, nil, commits)
}

func BuildPRReworkStatsWithComments(pr PullRequest, reviews []prReviewEvent, comments []prCommentEvent, commits []PRCommit) PRReworkStats {
	changeHeads := map[string]bool{}
	var firstReview time.Time
	humanChanges := 0
	for _, r := range reviews {
		if r.SubmittedAt.IsZero() {
			continue
		}
		if firstReview.IsZero() || r.SubmittedAt.Before(firstReview) {
			firstReview = r.SubmittedAt
		}
		if !reviewEventRequestsChanges(r) {
			continue
		}
		if isHumanReviewAuthor(r.Author, r.AuthorType) {
			humanChanges++
			continue
		}
		head := r.CommitID
		if head == "" {
			head = r.SubmittedAt.Format(time.RFC3339Nano)
		}
		changeHeads[head] = true
	}
	for _, c := range comments {
		if c.CreatedAt.IsZero() || !commentLooksLikeReviewSignal(c.Body) {
			continue
		}
		if firstReview.IsZero() || c.CreatedAt.Before(firstReview) {
			firstReview = c.CreatedAt
		}
		if commentRequestsChanges(c.Body) {
			if isHumanReviewAuthor(c.Author, c.AuthorType) {
				humanChanges++
				continue
			}
			changeHeads["comment:"+c.CreatedAt.Format(time.RFC3339Nano)] = true
		}
	}
	fixAttempts := 0
	followUpCommits := 0
	fixerModels := map[string]bool{}
	fixerBackends := map[string]bool{}
	for _, c := range commits {
		if commitAfterFirstReview(firstReview, pr.MergedAt, c.AuthoredAt) {
			followUpCommits++
		}
		attempt := commitFixAttempt(c.Message)
		if attempt > fixAttempts {
			fixAttempts = attempt
		}
		if attempt > 0 {
			meta, ok := ParseAttributionTrailer(c.Message)
			if !ok {
				continue
			}
			model := NormalizeAttributionModel(meta.Model)
			if model != "unknown" {
				fixerModels[model] = true
			}
			backend := NormalizeAttributionValue(meta.Backend)
			if backend != "unknown" {
				fixerBackends[backend] = true
			}
		}
	}
	if followUpCommits > fixAttempts {
		fixAttempts = followUpCommits
	}
	if dispatched := dispatchedFixAttemptsForPR(pr.Repo, pr.Number); dispatched > fixAttempts {
		fixAttempts = dispatched
	}
	var models []string
	for model := range fixerModels {
		models = append(models, model)
	}
	sort.Strings(models)
	var backends []string
	for backend := range fixerBackends {
		backends = append(backends, backend)
	}
	sort.Strings(backends)
	stats := PRReworkStats{
		ReviewRounds:        len(changeHeads),
		FixAttempts:         fixAttempts,
		HumanChangeRequests: humanChanges,
		FollowUpCommits:     followUpCommits,
		FirstReviewAt:       firstReview,
		FixerModels:         models,
		FixerBackends:       backends,
	}
	if !pr.MergedAt.IsZero() && !pr.CreatedAt.IsZero() {
		stats.TimeToMergeMinutes = int(pr.MergedAt.Sub(pr.CreatedAt).Minutes())
	}
	stats.FirstPass = !pr.MergedAt.IsZero() && stats.ReviewRounds == 0 && stats.FixAttempts == 0 && stats.FollowUpCommits == 0
	return stats
}

func commitAfterFirstReview(firstReview, mergedAt, authoredAt time.Time) bool {
	if firstReview.IsZero() || authoredAt.IsZero() || !authoredAt.After(firstReview) {
		return false
	}
	if !mergedAt.IsZero() && authoredAt.After(mergedAt) {
		return false
	}
	return true
}

type prReworkCacheFile struct {
	SchemaVersion int                      `json:"schema_version"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Items         map[string]PRReworkStats `json:"items"`
}

func loadPRReworkCache() map[string]PRReworkStats {
	prReworkCacheMu.Lock()
	defer prReworkCacheMu.Unlock()
	data, err := os.ReadFile(PRReworkCachePath)
	if err != nil {
		return map[string]PRReworkStats{}
	}
	var file prReworkCacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.Items == nil {
		return map[string]PRReworkStats{}
	}
	if file.SchemaVersion != prReworkCacheSchemaVersion {
		return map[string]PRReworkStats{}
	}
	return file.Items
}

func storePRReworkCache(items map[string]PRReworkStats) {
	prReworkCacheMu.Lock()
	defer prReworkCacheMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(PRReworkCachePath), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(prReworkCacheFile{SchemaVersion: prReworkCacheSchemaVersion, GeneratedAt: time.Now().UTC(), Items: items}, "", "  ")
	if err != nil {
		return
	}
	tmp := PRReworkCachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, PRReworkCachePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
	}
}

func isHumanReviewAuthor(login, userType string) bool {
	if strings.EqualFold(userType, "Bot") {
		return false
	}
	return !strings.HasSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
}

func commitFixAttempt(message string) int {
	m := fixAttemptTrailerRE.FindStringSubmatch(message)
	if len(m) != 2 {
		m = fixAttemptRE.FindStringSubmatch(message)
	}
	if len(m) != 2 {
		return 0
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	return n
}

func (c *Client) listPRCommentEvents(ctx context.Context, repo string, number int) ([]prCommentEvent, error) {
	owner, repoName := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		return nil, err
	}
	out := make([]prCommentEvent, 0, len(comments))
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		user := comment.GetUser()
		out = append(out, prCommentEvent{
			Author:     safeGetLogin(user),
			AuthorType: strings.TrimSpace(user.GetType()),
			Body:       strings.TrimSpace(comment.GetBody()),
			CreatedAt:  comment.GetCreatedAt().Time,
		})
	}
	return out, nil
}

func reviewEventRequestsChanges(r prReviewEvent) bool {
	switch strings.ToUpper(strings.TrimSpace(r.State)) {
	case "CHANGES_REQUESTED":
		return true
	case "COMMENTED":
		return !isHumanReviewAuthor(r.Author, r.AuthorType) && commentRequestsChanges(r.Body)
	default:
		return false
	}
}

func commentLooksLikeReviewSignal(body string) bool {
	return commentRequestsChanges(body) || strings.Contains(strings.ToLower(body), "**reviewed**")
}

func commentRequestsChanges(body string) bool {
	lower := strings.ToLower(strings.TrimSpace(body))
	if lower == "" {
		return false
	}
	markers := []string{
		`"verdict":"changes_requested"`,
		`"verdict": "changes_requested"`,
		`"verdict":"requires_human"`,
		`"verdict": "requires_human"`,
		`"verdict":"reject"`,
		`"verdict": "reject"`,
		"verdict: changes_requested",
		"verdict: requires_human",
		"changes_requested",
		"request changes",
		"requested changes",
		"**human decision needed**",
		"blocking ci failure",
		"blocking failure",
		"severity: high",
		"severity: critical",
		`"severity":"high"`,
		`"severity": "high"`,
		`"severity":"critical"`,
		`"severity": "critical"`,
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func dispatchedFixAttemptsForPR(repo string, number int) int {
	state, err := review.LoadDispatchState("")
	if err != nil {
		return 0
	}
	maxAttempt := 0
	for _, f := range state.FixAttempts {
		if strings.EqualFold(strings.TrimSpace(f.Repo), strings.TrimSpace(repo)) && f.Number == number && f.Attempts > maxAttempt {
			maxAttempt = f.Attempts
		}
	}
	return maxAttempt
}
