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
	"github.com/hivecommons/hive/pkg/review"
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
	// HoldHeadSHA and HeldSince are the hold the release was found against:
	// the head of its latest blocking verdict and the time the hold began on
	// that verdict's content (ReviewHoldSince). They let the review pipeline
	// honour a release posted before a base-branch merge drew a fresh verdict
	// on the same content, as the sweep does (hivecommons/hive#13086).
	HoldHeadSHA string    `json:"hold_head_sha,omitempty"`
	HeldSince   time.Time `json:"held_since,omitzero"`
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

// ReleasesRecordedReviewHold is ReleasesReviewHold for a release read back
// from the ledger, against a hold whose latest blocking verdict was recorded
// on holdHeadSHA at heldSince. When the release was found against that same
// verdict, the earlier start the finder established for its content
// (r.HeldSince) is honoured, so the board agrees with the sweep that a
// base-branch merge did not undo the release (hivecommons/hive#13086).
func ReleasesRecordedReviewHold(holdHeadSHA string, heldSince time.Time, r ReviewHoldRelease) bool {
	since := heldSince
	head := strings.TrimSpace(holdHeadSHA)
	if head != "" && strings.EqualFold(strings.TrimSpace(r.HoldHeadSHA), head) && !r.HeldSince.IsZero() && r.HeldSince.Before(since) {
		since = r.HeldSince
	}
	return ReleasesReviewHold(since, r)
}

// ReviewVerdictHold is the review swarm's hold on one pull request: the
// latest decisive verdict asked for a person (requires_human) or rejected it.
// HeadSHA is the head the verdict was recorded on, which may be older than
// the PR's current head: a fix push does not clear the hold.
//
// Verdicts are the blocking verdicts behind the hold, oldest first, ending
// with the one on HeadSHA (review.VerdictHold.Verdicts).
type ReviewVerdictHold struct {
	Held       bool
	Verdict    string
	HeadSHA    string
	RecordedAt time.Time
	Verdicts   []review.HeldVerdict
}

// ReviewVerdictHoldFunc resolves the review verdict hold for repo#number.
// repo is owner/repo. It is consulted regardless of review.require_approval.
type ReviewVerdictHoldFunc func(repo string, number int) ReviewVerdictHold

// FindReviewHoldRelease returns the first review or comment on the PR that
// releases hold, by ReleasesReviewHold: a person's approving review or
// `/hive approve` reply posted after the first blocking verdict on the
// content the hold's verdict judged (ReviewHoldSince), so a base-branch
// merge that drew a fresh verdict does not undo it (hivecommons/hive#13086).
// isPerson tells a person from the App and other bots. releaser, when set,
// further limits who may release (a trusted merger on lanes where the PR
// author is not one); nil accepts any person. The returned release carries
// the hold it was found against (HoldHeadSHA, HeldSince).
func FindReviewHoldRelease(ctx context.Context, client *gh.Client, owner, repo string, number int, hold ReviewVerdictHold, isPerson func(*gh.User) bool, releaser func(login string) bool) (ReviewHoldRelease, bool, error) {
	if client == nil {
		return ReviewHoldRelease{}, false, errors.New("no GitHub client to list the PR's reviews and comments")
	}
	since := ReviewHoldSince(ctx, client, owner, repo, number, hold)
	accept := func(r ReviewHoldRelease) bool {
		return ReleasesReviewHold(since, r) && (releaser == nil || releaser(r.By))
	}
	found := func(r ReviewHoldRelease) ReviewHoldRelease {
		r.HoldHeadSHA, r.HeldSince = hold.HeadSHA, since
		return r
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
				return found(release), true, nil
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
				return found(release), true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return ReviewHoldRelease{}, false, nil
		}
		commentOpts.Page = resp.NextPage
	}
}

// ReviewHoldSince returns when hold began on the content its latest verdict
// judged: the first-recorded time of the earliest verdict in the unbroken run
// of blocking verdicts, ending with the latest, whose heads carry the same
// non-merge commits and the same patch. A head the latest verdict's head
// reaches only through clean base-branch merges (a `Merge branch 'main'`
// update that leaves the PR's patch against its merge-base unchanged, compared
// like a patch-id) is the same content. Any other commit, a merge that carries
// conflict resolution, a merge whose cleanliness cannot be read, a rewritten
// history, or a new verdict on the same head (a changed description) starts a
// new hold. It lists the PR's commits only when there is an earlier verdict to
// consider, and falls back to the latest verdict's time (hold.RecordedAt)
// whenever the commits cannot be read.
func ReviewHoldSince(ctx context.Context, client *gh.Client, owner, repo string, number int, hold ReviewVerdictHold) time.Time {
	if client == nil || len(hold.Verdicts) < 2 || hold.RecordedAt.IsZero() {
		return hold.RecordedAt
	}
	var commits []*gh.RepositoryCommit
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListCommits(ctx, owner, repo, number, opts)
		if err != nil {
			return hold.RecordedAt
		}
		commits = append(commits, page...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	type patch struct {
		id string
		ok bool
	}
	patches := map[string]patch{}
	patchID := func(base, head string) (string, bool) {
		key := base + "..." + head
		if p, ok := patches[key]; ok {
			return p.id, p.ok
		}
		var p patch
		cmp, _, err := client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
		if err == nil && cmp != nil {
			p.id, p.ok = comparePatchID(cmp.Files)
		}
		patches[key] = p
		return p.id, p.ok
	}
	clean := func(merge, prParent, baseParent string) bool {
		before, ok := patchID(baseParent, prParent)
		if !ok {
			return false
		}
		after, ok := patchID(baseParent, merge)
		return ok && before == after
	}
	return reviewHoldSinceForCommits(hold, commits, clean)
}

// cleanMergeFunc reports whether merge, a merge of the base-branch commit
// baseParent into the PR commit prParent, leaves the PR's patch against its
// merge-base unchanged. It returns false when that cannot be determined.
type cleanMergeFunc func(merge, prParent, baseParent string) bool

// comparePatchID returns a patch-id-like digest of a compare's files: per
// file, its name and status and the added and removed lines with whitespace
// and line numbers dropped, so a clean merge of base-branch changes elsewhere
// keeps it. ok is false when the patch is missing or may be truncated.
func comparePatchID(files []*gh.CommitFile) (string, bool) {
	// GitHub's compare lists at most 300 files.
	if len(files) >= 300 {
		return "", false
	}
	entries := make([]string, 0, len(files))
	for _, f := range files {
		var b strings.Builder
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00", f.GetFilename(), f.GetPreviousFilename(), f.GetStatus())
		patch := f.GetPatch()
		if patch == "" {
			// Binary, too large, or a rename without changes: compare the blob.
			if f.GetSHA() == "" && f.GetChanges() > 0 {
				return "", false
			}
			b.WriteString("blob:" + f.GetSHA())
		}
		for _, line := range strings.Split(patch, "\n") {
			if line == "" || (line[0] != '+' && line[0] != '-') {
				continue
			}
			b.WriteByte(line[0])
			b.WriteString(strings.Join(strings.Fields(line[1:]), ""))
			b.WriteByte('\n')
		}
		entries = append(entries, b.String())
	}
	sort.Strings(entries)
	return strings.Join(entries, "\x01"), true
}

// reviewHoldSinceForCommits is ReviewHoldSince over the PR's commits; clean
// decides whether a base-branch merge commit kept the PR's patch (nil treats
// every merge as changing it).
func reviewHoldSinceForCommits(hold ReviewVerdictHold, commits []*gh.RepositoryCommit, clean cleanMergeFunc) time.Time {
	since := hold.RecordedAt
	n := len(hold.Verdicts)
	if n < 2 || since.IsZero() {
		return since
	}
	parents := make(map[string][]string, len(commits))
	for _, c := range commits {
		var ps []string
		for _, p := range c.Parents {
			ps = append(ps, strings.ToLower(p.GetSHA()))
		}
		parents[strings.ToLower(c.GetSHA())] = ps
	}
	latest := strings.ToLower(strings.TrimSpace(hold.Verdicts[n-1].HeadSHA))
	reach := prCommitAncestors(parents, latest)
	if reach == nil {
		return since
	}
	for i := n - 2; i >= 0; i-- {
		v := hold.Verdicts[i]
		head := strings.ToLower(strings.TrimSpace(v.HeadSHA))
		if v.RecordedAt.IsZero() || head == latest || !sameContent(parents, reach, head, clean) {
			break
		}
		since = v.RecordedAt
	}
	return since
}

// prCommitAncestors returns head and the PR commits it reaches through their
// parents, or nil when head is not one of the PR's commits. parents holds the
// PR's commits only, so the walk stops at the base branch.
func prCommitAncestors(parents map[string][]string, head string) map[string]bool {
	if _, ok := parents[head]; !ok {
		return nil
	}
	seen := map[string]bool{}
	stack := []string{head}
	for len(stack) > 0 {
		sha := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ps, ok := parents[sha]
		if !ok || seen[sha] {
			continue
		}
		seen[sha] = true
		stack = append(stack, ps...)
	}
	return seen
}

// sameContent reports whether head, an earlier verdict's head, is one of the
// commits in reach (the later head and its PR ancestors) and every commit
// reach adds over head's own ancestors is a clean merge of a base-branch
// commit: two parents, the first a PR commit and the second not, and clean
// reports the PR's patch unchanged across it. A merge that carries conflict
// resolution changes the PR's content like any other commit.
func sameContent(parents map[string][]string, reach map[string]bool, head string, clean cleanMergeFunc) bool {
	if !reach[head] {
		return false
	}
	earlier := prCommitAncestors(parents, head)
	var merges []string
	for sha := range reach {
		if earlier[sha] {
			continue
		}
		ps := parents[sha]
		if len(ps) != 2 {
			return false
		}
		if _, ok := parents[ps[0]]; !ok {
			return false
		}
		if _, ok := parents[ps[1]]; ok {
			return false
		}
		merges = append(merges, sha)
	}
	if len(merges) > 0 && clean == nil {
		return false
	}
	sort.Strings(merges)
	for _, sha := range merges {
		if !clean(sha, parents[sha][0], parents[sha][1]) {
			return false
		}
	}
	return true
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
	if !r.HeldSince.IsZero() {
		r.HeldSince = r.HeldSince.UTC()
	}
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
	return a.Via == b.Via && a.State == b.State && a.Body == b.Body && a.By == b.By && a.Person == b.Person && a.At.Equal(b.At) &&
		a.HoldHeadSHA == b.HoldHeadSHA && a.HeldSince.Equal(b.HeldSince)
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
