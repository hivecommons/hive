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
)

const maxPRReviewPages = 3
const PRReworkCacheFile = "pr-rework-cache.json"

var (
	PRReworkCachePath = filepath.Join(ReviewLinksDir, PRReworkCacheFile)
	prReworkCacheMu   sync.Mutex
)

var fixAttemptRE = regexp.MustCompile(`(?i)\battempt\s+(\d+)\s*(?:/|of)\s*\d+`)

type prReviewEvent struct {
	State       string
	Author      string
	AuthorType  string
	CommitID    string
	SubmittedAt time.Time
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
	return BuildPRReworkStats(pr, reviews, commits), nil
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
		if r.State != "CHANGES_REQUESTED" {
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
	stats.FirstPass = !pr.MergedAt.IsZero() && stats.ReviewRounds == 0 && stats.FixAttempts == 0
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
	GeneratedAt time.Time                `json:"generated_at"`
	Items       map[string]PRReworkStats `json:"items"`
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
	return file.Items
}

func storePRReworkCache(items map[string]PRReworkStats) {
	prReworkCacheMu.Lock()
	defer prReworkCacheMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(PRReworkCachePath), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(prReworkCacheFile{GeneratedAt: time.Now().UTC(), Items: items}, "", "  ")
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
	m := fixAttemptRE.FindStringSubmatch(message)
	if len(m) != 2 {
		return 0
	}
	n := 0
	for _, r := range m[1] {
		n = n*10 + int(r-'0')
	}
	return n
}
