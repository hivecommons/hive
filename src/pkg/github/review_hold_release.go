package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ReviewHoldReleaseCommand is the reply a person posts to release a review
// verdict hold without submitting an approving review.
const ReviewHoldReleaseCommand = "/hive approve"

// How a person released a review verdict hold.
const (
	ReviewHoldReleaseViaReview  = "review"
	ReviewHoldReleaseViaComment = "comment"
)

// ReviewHoldRelease is one GitHub action that may release the review swarm's
// requires_human or reject hold on a pull request (#12374): a review (State is
// its state) or a conversation comment (Body is its text). Person is false for
// the App and every other bot.
type ReviewHoldRelease struct {
	Via    string    `json:"via"`
	State  string    `json:"state,omitempty"`
	Body   string    `json:"body,omitempty"`
	By     string    `json:"by"`
	Person bool      `json:"person"`
	At     time.Time `json:"at"`
}

// ReleasesReviewHold reports whether r releases a review verdict hold whose
// verdict was recorded at heldSince: an APPROVED review or a `/hive approve`
// reply, by a person, after the verdict. A plain comment does not release:
// "do not merge this" is a comment too. A newer verdict moves heldSince past
// the release, so it holds again.
//
// The self-merge sweep (automerge reviewVerdictGate) and the review pipeline's
// human_hold, which the dashboard's your-turn decision reads, both answer the
// question here so the board and the merger cannot drift apart (#12962).
func ReleasesReviewHold(heldSince time.Time, r ReviewHoldRelease) bool {
	if !r.Person || strings.TrimSpace(r.By) == "" || !r.At.After(heldSince) {
		return false
	}
	switch r.Via {
	case ReviewHoldReleaseViaReview:
		return strings.EqualFold(r.State, "APPROVED")
	case ReviewHoldReleaseViaComment:
		return IsReviewHoldReleaseCommand(r.Body)
	}
	return false
}

// ReviewVerdictHold is the review swarm's hold on one pull request: the
// latest decisive verdict asked for a person (requires_human) or rejected it.
// HeadSHA is the head the verdict was recorded on, which may be older than
// the PR's current head: a fix push does not clear the hold.
type ReviewVerdictHold struct {
	Held       bool
	Verdict    string
	HeadSHA    string
	RecordedAt time.Time
}

// ReviewVerdictHoldFunc resolves the review verdict hold for repo#number.
// repo is owner/repo. It is consulted regardless of review.require_approval.
type ReviewVerdictHoldFunc func(repo string, number int) ReviewVerdictHold

// FindReviewHoldRelease returns the first review or comment on the PR that
// releases a hold whose verdict was recorded at since, by ReleasesReviewHold:
// a person's approving review or `/hive approve` reply posted after the
// verdict. isPerson tells a person from the App and other bots. releaser,
// when set, further limits who may release (a trusted merger on lanes where
// the PR author is not one); nil accepts any person.
func FindReviewHoldRelease(ctx context.Context, client *gh.Client, owner, repo string, number int, since time.Time, isPerson func(*gh.User) bool, releaser func(login string) bool) (ReviewHoldRelease, bool, error) {
	if client == nil {
		return ReviewHoldRelease{}, false, errors.New("no GitHub client to list the PR's reviews and comments")
	}
	accept := func(r ReviewHoldRelease) bool {
		return ReleasesReviewHold(since, r) && (releaser == nil || releaser(r.By))
	}
	reviewOpts := &gh.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, reviewOpts)
		if err != nil {
			return ReviewHoldRelease{}, false, err
		}
		for _, r := range reviews {
			release := ReviewHoldRelease{
				Via:    ReviewHoldReleaseViaReview,
				State:  r.GetState(),
				By:     SafeGetLogin(r.GetUser()),
				Person: isPerson != nil && isPerson(r.GetUser()),
				At:     r.GetSubmittedAt().Time,
			}
			if accept(release) {
				return release, true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		reviewOpts.Page = resp.NextPage
	}
	commentOpts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	if !since.IsZero() {
		commentOpts.Since = gh.Ptr(since)
	}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, commentOpts)
		if err != nil {
			return ReviewHoldRelease{}, false, err
		}
		for _, comment := range comments {
			release := ReviewHoldRelease{
				Via:    ReviewHoldReleaseViaComment,
				Body:   comment.GetBody(),
				By:     SafeGetLogin(comment.GetUser()),
				Person: isPerson != nil && isPerson(comment.GetUser()),
				At:     comment.GetCreatedAt().Time,
			}
			if accept(release) {
				return release, true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return ReviewHoldRelease{}, false, nil
		}
		commentOpts.Page = resp.NextPage
	}
}

// IsPersonLogin reports whether u is a person: not appBotLogin, nor any
// other bot.
func IsPersonLogin(u *gh.User, appBotLogin string) bool {
	login := strings.TrimSpace(SafeGetLogin(u))
	if login == "" || strings.EqualFold(u.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if app := strings.TrimSpace(appBotLogin); app != "" && strings.EqualFold(login, app) {
		return false
	}
	return true
}

// IsReviewHoldReleaseCommand reports whether a comment body carries the
// `/hive approve` release command at the start of one of its lines.
func IsReviewHoldReleaseCommand(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(strings.ToLower(line))
		if len(fields) >= 2 && fields[0]+" "+fields[1] == ReviewHoldReleaseCommand {
			return true
		}
	}
	return false
}

// ReviewHoldReleasesFile records the person's release the self-merge sweep
// found for each held pull request. The sweep already lists the PR's reviews
// and comments to decide whether it may merge; recording what it found lets
// the review pipeline and the dashboard's your-turn decision see the same
// release without an API call per PR per render (#12962).
const ReviewHoldReleasesFile = "review-hold-releases.json"

// ReviewHoldReleasesPath is a var so tests can point it at a temp dir. It
// lives on the durable data dir next to the review-links ledger.
var ReviewHoldReleasesPath = filepath.Join(ReviewLinksDir, ReviewHoldReleasesFile)

// reviewHoldReleasesMu serializes the read-modify-write in
// RecordReviewHoldRelease.
var reviewHoldReleasesMu sync.Mutex

// maxReviewHoldReleases bounds the ledger; the oldest releases are pruned
// first.
const maxReviewHoldReleases = 2000

type reviewHoldReleaseLedger struct {
	GeneratedAt time.Time                    `json:"generated_at"`
	Releases    map[string]ReviewHoldRelease `json:"releases"`
}

func reviewHoldReleaseKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// LoadReviewHoldReleases reads the ledger. A missing file is not an error:
// no person has released a held PR yet.
func LoadReviewHoldReleases(path string) (map[string]ReviewHoldRelease, error) {
	if path == "" {
		path = ReviewHoldReleasesPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]ReviewHoldRelease{}, nil
		}
		return nil, err
	}
	var ledger reviewHoldReleaseLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if ledger.Releases == nil {
		return map[string]ReviewHoldRelease{}, nil
	}
	return ledger.Releases, nil
}

// ReviewHoldReleaseFor returns the recorded release for repo#number. repo is
// owner/repo and matches case-insensitively.
func ReviewHoldReleaseFor(releases map[string]ReviewHoldRelease, repo string, number int) (ReviewHoldRelease, bool) {
	r, ok := releases[reviewHoldReleaseKey(repo, number)]
	return r, ok
}

// RecordReviewHoldRelease stores the release the sweep found for repo#number,
// replacing an earlier one. Anything ReleasesReviewHold would never accept is
// ignored, and a comment's text is not kept beyond the command itself.
func RecordReviewHoldRelease(path, repo string, number int, r ReviewHoldRelease) error {
	if path == "" {
		path = ReviewHoldReleasesPath
	}
	if !ReleasesReviewHold(time.Time{}, r) {
		return nil
	}
	if r.Via == ReviewHoldReleaseViaComment {
		r.Body = ReviewHoldReleaseCommand
	}
	r.At = r.At.UTC()
	reviewHoldReleasesMu.Lock()
	defer reviewHoldReleasesMu.Unlock()

	releases, err := LoadReviewHoldReleases(path)
	if err != nil {
		releases = map[string]ReviewHoldRelease{}
	}
	key := reviewHoldReleaseKey(repo, number)
	if prev, ok := releases[key]; ok && sameReviewHoldRelease(prev, r) {
		return nil
	}
	releases[key] = r
	pruneReviewHoldReleases(releases)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reviewHoldReleaseLedger{GeneratedAt: time.Now().UTC(), Releases: releases}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sameReviewHoldRelease(a, b ReviewHoldRelease) bool {
	return a.Via == b.Via && a.State == b.State && a.Body == b.Body && a.By == b.By && a.Person == b.Person && a.At.Equal(b.At)
}

func pruneReviewHoldReleases(releases map[string]ReviewHoldRelease) {
	if len(releases) <= maxReviewHoldReleases {
		return
	}
	keys := make([]string, 0, len(releases))
	for k := range releases {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := releases[keys[i]].At, releases[keys[j]].At
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.Before(b)
	})
	for _, k := range keys[:len(releases)-maxReviewHoldReleases] {
		delete(releases, k)
	}
}
