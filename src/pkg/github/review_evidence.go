package github

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/modelfamily"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// ReviewEvidenceDirName is the directory under the durable data dir that holds
// the review evidence bundles (pkg/evidence, hivecommons/hive#11060): one
// file per PR head at <owner>/<repo>/<number>/<head>.json.
//
// It sits beside review-links.json on /data rather than under
// /var/run/hive-metrics for the same reason the links and verdicts do: a
// bundle is the record an auditor asks for months later, and the container's
// ephemeral layer would lose it on the next restart.
const ReviewEvidenceDirName = "evidence"

// ReviewEvidenceRoot is a var so tests can point it at a temp dir.
var ReviewEvidenceRoot = filepath.Join(ReviewLinksDir, ReviewEvidenceDirName)

// reviewEvidenceMu serializes the read-modify-write of a bundle, for the same
// reason reviewLinksMu guards the link ledger.
var reviewEvidenceMu sync.Mutex

// reviewEvidenceGeneralPath stands in for a finding's file when the reviewer
// reported it against the PR as a whole: the schema requires a path, and
// dropping the finding would make the bundle say less than the review did.
const reviewEvidenceGeneralPath = "(pr)"

// ReviewEvidenceSettings is what the relay needs from config to write a
// bundle for one repo. Policy is the snapshot taken when a head's bundle is
// first written; an empty Policy.Perspectives is filled from the relay's own
// perspective set.
type ReviewEvidenceSettings struct {
	Enabled        bool
	SigningKeyFile string
	Policy         evidence.Policy
}

// SetReviewEvidence wires the evidence.* settings. Read through a func so a
// live config edit applies without rebuilding the client. Nil (the default)
// writes no bundles, so a client nothing configured stays off the data dir.
func (c *Client) SetReviewEvidence(fn func(repo string) ReviewEvidenceSettings) {
	if c == nil {
		return
	}
	c.reviewEvidence = fn
}

func (c *Client) reviewEvidenceSettings(repo string) (ReviewEvidenceSettings, bool) {
	if c == nil || c.reviewEvidence == nil {
		return ReviewEvidenceSettings{}, false
	}
	s := c.reviewEvidence(repo)
	return s, s.Enabled
}

// ReviewEvidencePath is where the bundle for repo#number@head lives under
// root (ReviewEvidenceRoot when empty). Every segment is agent- or
// forge-supplied, so each is reduced to a single safe path element.
func ReviewEvidencePath(root, repo string, number int, head string) string {
	if root == "" {
		root = ReviewEvidenceRoot
	}
	owner, name, _ := strings.Cut(strings.TrimSpace(repo), "/")
	return filepath.Join(root, evidencePathSegment(owner), evidencePathSegment(name),
		strconv.Itoa(number), evidencePathSegment(head)+".json")
}

// evidencePathSegment keeps the characters GitHub allows in owner and repo
// names and collapses everything else to "-", so no segment can carry a
// separator or be "." / "..".
func evidencePathSegment(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if strings.Trim(out, ".") == "" {
		return "unknown"
	}
	return out
}

// LoadReviewEvidence reads one bundle. For a bundle in a PR directory the
// newest copy across that directory and its <pr>.uid<N> fallback siblings is
// read. A missing file reports an error wrapping os.ErrNotExist.
func LoadReviewEvidence(path string) (*evidence.Bundle, error) {
	path = resolveReviewEvidencePath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b evidence.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &b, nil
}

// reviewEvidenceUpdate is what one relay or sentinel pass adds to one head's
// bundle.
type reviewEvidenceUpdate struct {
	verdicts []evidence.Verdict
	posted   []evidence.PostedReview
	sentinel []evidence.SentinelFinding
	ci       *evidence.CI
	signals  review.ConfidenceSignals
}

// recordReviewEvidence adds what the relay just recorded to the evidence
// bundle for each head it concerns: the verdicts that passed validation and
// dispatch binding, and the review it posted (nil when it posted none).
//
// Like the verdict and link writes it sits beside, every failure is logged
// and swallowed: the review is already on GitHub, and a missing bundle must
// never turn it into a retry that posts it again.
func (c *Client) recordReviewEvidence(ctx context.Context, req ReviewRequest, reports []review.PerspectiveReport, posted *gh.PullRequestReview, now time.Time) {
	settings, ok := c.reviewEvidenceSettings(req.Repo)
	if !ok {
		return
	}
	postedURL := strings.TrimSpace(posted.GetHTMLURL())
	if len(reports) == 0 && postedURL == "" {
		return
	}
	owner, name := c.splitRepo(strings.TrimSpace(req.Repo))
	repo := owner + "/" + name
	now = now.UTC()

	var (
		pr      *gh.PullRequest
		fetched bool
	)
	// The PR is fetched at most once per pass, and only when a bundle has to
	// be started (author and base live nowhere else on the relay's path) or a
	// head is unknown.
	fetchPR := func() *gh.PullRequest {
		if fetched {
			return pr
		}
		fetched = true
		if c.client == nil {
			return nil
		}
		got, _, err := c.client.PullRequests.Get(ctx, owner, name, req.Number)
		if err != nil {
			c.logger.Warn("review evidence: could not fetch PR",
				slog.String("repo", repo), slog.Int("number", req.Number), slog.String("error", err.Error()))
			return nil
		}
		pr = got
		return pr
	}

	meta := c.attributionMeta(req.Agent)
	updates := map[string]*reviewEvidenceUpdate{}
	var heads []string
	updateFor := func(head string) *reviewEvidenceUpdate {
		head = strings.TrimSpace(head)
		if head == "" {
			head = fetchPR().GetHead().GetSHA()
		}
		if head == "" {
			return nil
		}
		u, ok := updates[head]
		if !ok {
			u = &reviewEvidenceUpdate{}
			updates[head] = u
			heads = append(heads, head)
		}
		return u
	}
	for _, r := range reports {
		u := updateFor(r.HeadSHA)
		if u == nil {
			c.logger.Warn("review evidence: verdict head unknown, not recorded",
				slog.String("repo", repo), slog.Int("number", req.Number),
				slog.String("perspective", string(r.Perspective)))
			continue
		}
		u.verdicts = append(u.verdicts, evidenceVerdict(r, meta, now))
	}
	if postedURL != "" {
		head := strings.TrimSpace(posted.GetCommitID())
		if u := updateFor(head); u != nil {
			if head == "" {
				head = fetchPR().GetHead().GetSHA()
			}
			u.posted = append(u.posted, evidence.PostedReview{URL: postedURL, Head: head, At: now})
		}
	}
	if len(heads) == 0 {
		return
	}

	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		// Written unsigned rather than not at all: "signed": false is stated
		// in the bundle, so nobody can mistake it for a signed one.
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	for _, head := range heads {
		u := updates[head]
		if len(u.verdicts) > 0 {
			existing, _ := LoadVerifiedReviewEvidence(ReviewEvidencePath("", repo, req.Number, head), reviewEvidenceVerifierForKey(key))
			needsSignals := existing == nil
			for _, v := range u.verdicts {
				if existing != nil && !containsEvidenceVerdict(existing.Verdicts, v) {
					needsSignals = true
				}
			}
			if needsSignals {
				base := ""
				if existing != nil {
					base = existing.BaseSHA
				} else {
					base = fetchPR().GetBase().GetSHA()
				}
				u.signals = c.reviewConfidenceDiff(ctx, owner, name, base, head)
			}
		}
		c.upsertReviewEvidence(repo, req.Number, head, *updates[head], settings.Policy, key, fetchPR, now)
	}
}

// upsertReviewEvidence merges u into the bundle for repo#number@head,
// creating it (with the policy snapshot and a link to the previous head's
// bundle) on first write. A pass that adds nothing new leaves the file
// untouched, so re-running the relay never duplicates entries or churns the
// hash.
func (c *Client) upsertReviewEvidence(repo string, number int, head string, u reviewEvidenceUpdate, policy evidence.Policy, key ed25519.PrivateKey, fetchPR func() *gh.PullRequest, now time.Time) {
	path := ReviewEvidencePath("", repo, number, head)
	// fetchPR is a GitHub GET and memoized, so a bundle that has to be started
	// fetches here, before reviewEvidenceMu is taken: the merge-eligibility
	// poll loop (recordReviewEvidenceCI) takes the same lock and must not wait
	// on the network. A bundle that appears meanwhile only costs a spare fetch.
	if _, statErr := os.Stat(resolveReviewEvidencePath(path)); errors.Is(statErr, os.ErrNotExist) {
		fetchPR()
	}

	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()
	defer lockReviewEvidenceDir(path)()

	b, err := LoadReviewEvidence(path)
	switch {
	case err == nil:
		if err := reviewEvidenceVerifierForKey(key).Verify(b); err != nil {
			c.logger.Warn("review evidence: existing bundle failed integrity verification, left untouched",
				slog.String("path", path), slog.String("error", err.Error()))
			return
		}
	case errors.Is(err, os.ErrNotExist):
		pr := fetchPR()
		if pr == nil {
			c.logger.Warn("review evidence: PR metadata unavailable, bundle not started",
				slog.String("repo", repo), slog.Int("number", number), slog.String("head", head))
			return
		}
		b = c.newReviewEvidenceBundle(repo, number, head, pr, policy, now)
		// A bundle started after the PR already merged (the merge path ran
		// before the first review wrote one, or someone else merged) would
		// otherwise never gain a merge record and read as "outcome unknown".
		// Record what the fetched PR already knows. Record-only: nothing
		// here is read back into merge eligibility.
		b.Merge = reviewEvidenceMergeFromPR(pr, head, now)
	default:
		// An unreadable bundle is still evidence. Overwriting it with a fresh
		// one would silently replace the record, so it is left for a person.
		c.logger.Warn("review evidence: existing bundle unreadable, left untouched",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	changed := b.UpdatedAt.IsZero()
	signals := u.signals
	// Read only CI already captured before this verdict. Later CI/merge writes
	// never recompute scores, so calibration cannot learn the outcome in advance.
	if !b.CI.CapturedAt.IsZero() && !b.CI.CapturedAt.After(now) {
		signals.CIState = b.CI.Status
	}
	for _, v := range b.Verdicts {
		signals.Peers = append(signals.Peers, evidenceConfidenceReport(v, repo, number, head))
	}
	for _, v := range u.verdicts {
		signals.Peers = append(signals.Peers, evidenceConfidenceReport(v, repo, number, head))
	}
	for _, v := range u.verdicts {
		v = stampEvidenceVerdictFamily(v, b.Author)
		if !containsEvidenceVerdict(b.Verdicts, v) {
			derived := review.DerivedConfidence(evidenceConfidenceReport(v, repo, number, head), signals)
			v.ConfidenceDerived = &derived
			b.Verdicts = append(b.Verdicts, v)
			changed = true
		}
	}
	for _, p := range u.posted {
		if !containsPostedReview(b.PostedReviews, p.URL) {
			b.PostedReviews = append(b.PostedReviews, p)
			changed = true
		}
	}
	for _, f := range u.sentinel {
		if !containsSentinelFinding(b.Sentinel, f) {
			b.Sentinel = append(b.Sentinel, f)
			changed = true
		}
	}
	if u.ci != nil && b.Merge == nil &&
		(b.CI.CapturedAt.IsZero() || b.CI.Status != u.ci.Status || !sameEvidenceChecks(b.CI.Checks, u.ci.Checks)) {
		b.CI = *u.ci
		changed = true
	}
	if !changed {
		return
	}
	b.UpdatedAt = now
	if err := b.Validate(); err != nil {
		c.logger.Warn("review evidence: bundle invalid, not written",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := evidence.Seal(b, key); err != nil {
		c.logger.Warn("review evidence: could not seal bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := writeReviewEvidence(path, b); err != nil {
		level := slog.LevelWarn
		// Permission failures are an ownership problem that repeats every poll; warn once per PR directory.
		if errors.Is(err, fs.ErrPermission) {
			if _, seen := evidenceWriteDenied.LoadOrStore(reviewEvidenceCanonicalDir(filepath.Dir(path)), struct{}{}); seen {
				level = slog.LevelDebug
			}
		}
		c.logger.Log(context.Background(), level, "review evidence: could not write bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	c.logger.Info("review evidence: bundle written",
		slog.String("id", b.ID), slog.Int("verdicts", len(b.Verdicts)),
		slog.Int("posted_reviews", len(b.PostedReviews)), slog.Int("sentinel", len(b.Sentinel)),
		slog.Bool("signed", b.Signed),
		slog.String("path", path))
}

// newReviewEvidenceBundle starts the (unsealed) bundle for repo#number@head
// from the PR snapshot: author, base and the policy in force.
func (c *Client) newReviewEvidenceBundle(repo string, number int, head string, pr *gh.PullRequest, policy evidence.Policy, now time.Time) *evidence.Bundle {
	path := ReviewEvidencePath("", repo, number, head)
	return &evidence.Bundle{
		SchemaVersion:    evidence.SchemaVersion,
		ID:               evidence.BundleID(repo, number, head),
		Repo:             repo,
		Number:           number,
		Author:           evidenceAuthor(repo, number, pr.GetUser()),
		BaseSHA:          pr.GetBase().GetSHA(),
		HeadSHA:          head,
		PreviousBundleID: previousReviewEvidenceID(filepath.Dir(path), head),
		Policy:           c.reviewEvidencePolicy(policy),
		CreatedAt:        now,
	}
}

// StartReviewEvidence starts the evidence bundle for repo#number@head from pr
// when none exists yet, so a shadow observation, the CI snapshot and the merge
// recorded on the self-merge lane always have a bundle to land on even when
// the swarm has not written one (hivecommons/hive#12592). An existing bundle
// is left untouched. Record-only; failures are logged and swallowed.
func (c *Client) StartReviewEvidence(repo string, number int, head string, pr *gh.PullRequest) {
	if c == nil || pr == nil || head == "" || number <= 0 {
		return
	}
	settings, ok := c.reviewEvidenceSettings(repo)
	if !ok {
		return
	}
	if _, err := os.Stat(resolveReviewEvidencePath(ReviewEvidencePath("", repo, number, head))); err == nil || !errors.Is(err, os.ErrNotExist) {
		return
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, starting bundle unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	c.upsertReviewEvidence(repo, number, head, reviewEvidenceUpdate{}, settings.Policy, key,
		func() *gh.PullRequest { return pr }, time.Now().UTC())
}

// reviewEvidenceMergeObservedMethod marks a merge event taken from the PR
// as GitHub reports it: the API says who merged and when, not how. It is the
// same value `hivectl evidence backfill` writes, so readers treat both alike.
const reviewEvidenceMergeObservedMethod = "unrecorded"

// reviewEvidenceMergeFromPR returns the merge event GitHub already reports
// for pr, or nil when it has not merged. The merge SHA falls back to head and
// the time to now so the event always validates.
func reviewEvidenceMergeFromPR(pr *gh.PullRequest, head string, now time.Time) *evidence.MergeEvent {
	if pr == nil || !pr.GetMerged() {
		return nil
	}
	m := &evidence.MergeEvent{
		Actor:  strings.TrimSpace(pr.GetMergedBy().GetLogin()),
		Method: reviewEvidenceMergeObservedMethod,
		At:     pr.GetMergedAt().Time.UTC(),
		SHA:    strings.TrimSpace(pr.GetMergeCommitSHA()),
	}
	if m.Actor == "" {
		m.Actor = reviewEvidenceMergeObservedMethod
	}
	if m.At.IsZero() {
		m.At = now
	}
	if m.SHA == "" {
		m.SHA = head
	}
	if opened := pr.GetCreatedAt().Time.UTC(); !opened.IsZero() && !opened.After(m.At) {
		m.OpenedAt = opened
	}
	return m
}

// recordReviewEvidenceSentinel adds the findings the sentinel sweep just
// flagged pr for to the bundle for its head. pr is the listing the sweep
// already holds, so a head the relay has not written yet gets its bundle
// started from it (author, base, policy) without another fetch. Like the
// relay's writes, every failure is logged and swallowed: the alert label and
// comment are already on GitHub. blocked reports that the sweep applied the
// alert label for these findings; a notice-only pass (trusted author,
// comment but no label) records them with Notice set so readers know the
// label never carried this alert.
func (c *Client) recordReviewEvidenceSentinel(repo string, pr *gh.PullRequest, findings []sentinel.Finding, blocked bool, now time.Time) {
	settings, ok := c.reviewEvidenceSettings(repo)
	if !ok || pr == nil || len(findings) == 0 {
		return
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" || pr.GetNumber() <= 0 {
		return
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	var u reviewEvidenceUpdate
	for _, f := range findings {
		if ef, ok := evidenceSentinelFinding(f); ok {
			ef.Notice = !blocked
			u.sentinel = append(u.sentinel, ef)
		}
	}
	if len(u.sentinel) == 0 {
		return
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	c.upsertReviewEvidence(owner+"/"+name, pr.GetNumber(), head, u, settings.Policy, key,
		func() *gh.PullRequest { return pr }, now.UTC())
}

// evidenceSentinelFinding maps a sentinel finding onto the bundle schema.
// Paths are sorted so the same finding re-flagged always compares equal.
func evidenceSentinelFinding(f sentinel.Finding) (evidence.SentinelFinding, bool) {
	rule := strings.TrimSpace(f.Rule)
	if rule == "" {
		return evidence.SentinelFinding{}, false
	}
	var paths []string
	for _, p := range f.Paths {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return evidence.SentinelFinding{Rule: rule, Summary: strings.TrimSpace(f.Summary), Paths: paths}, true
}

func containsSentinelFinding(have []evidence.SentinelFinding, f evidence.SentinelFinding) bool {
	for _, h := range have {
		if reflect.DeepEqual(h, f) {
			return true
		}
	}
	return false
}

// evidenceWriteDenied remembers PR directories whose write already failed with a permission
// error; every new head SHA is a new path in the same unwritable directory.
var evidenceWriteDenied sync.Map

// evidenceDirMode is group-writable and setgid so writers running under
// different uids that share a group can all add bundles to one directory.
const evidenceDirMode = os.FileMode(0o775) | os.ModeSetgid

// evidenceFallbackDirMarker separates a PR directory's name from the uid of
// the writer that owns its fallback sibling: <pr>.uid<N>.
const evidenceFallbackDirMarker = ".uid"

// writeReviewEvidence commits b with write-then-rename so a reader never
// observes a half-written bundle. When the PR directory exists but this uid
// may not write into it (created by another writer, #13836), the bundle goes
// to the writer-owned sibling <pr>.uid<N>/ instead; the readers
// (ListReviewEvidence, LoadReviewEvidence) consult those siblings too.
func writeReviewEvidence(path string, b *evidence.Bundle) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	err = writeEvidenceBundleFile(path, data)
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	fallback := reviewEvidenceFallbackPath(path)
	ferr := writeEvidenceBundleFile(fallback, data)
	// The sibling may be new: the memoised listing of its parent is stale.
	forgetEvidenceSiblings(reviewEvidenceCanonicalDir(filepath.Dir(fallback)))
	if ferr != nil {
		return fmt.Errorf("%w (fallback %s: %v)", err, fallback, ferr)
	}
	return nil
}

func writeEvidenceBundleFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := mkdirEvidenceDirs(dir); err != nil {
		return err
	}
	widenOwnedEvidenceDir(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// reviewEvidenceFallbackPath is where this process writes the bundle at path
// when its directory is not writable: <pr>.uid<N>/<head>.json. path may
// itself already be inside another writer's <pr>.uid<M>/ sibling (an
// Entry.Path handed back by ListReviewEvidence); the sibling is always
// derived from the canonical PR directory so the readers, which only glob
// <pr>.uid<N> next to <pr>/, still find the bundle.
func reviewEvidenceFallbackPath(path string) string {
	dir := reviewEvidenceCanonicalDir(filepath.Dir(path))
	return filepath.Join(dir+evidenceFallbackDirMarker+strconv.Itoa(os.Getuid()), filepath.Base(path))
}

// reviewEvidenceCanonicalDir is the PR directory for dir, which is either the
// PR directory itself or one of its <pr>.uid<N> fallback siblings.
func reviewEvidenceCanonicalDir(dir string) string {
	base := filepath.Base(dir)
	i := strings.LastIndex(base, evidenceFallbackDirMarker)
	if i <= 0 {
		return dir
	}
	if _, err := strconv.Atoi(base[i+len(evidenceFallbackDirMarker):]); err != nil {
		return dir
	}
	if _, err := strconv.Atoi(base[:i]); err != nil {
		return dir
	}
	return filepath.Join(filepath.Dir(dir), base[:i])
}

// reviewEvidenceDirs is dir followed by its <dir>.uid<N> fallback siblings.
//
// The siblings are found by listing dir's parent (the repo directory), which
// in a hosted hive holds one entry per PR ever reviewed — thousands, on NFS.
// A Glob per lookup made every tracker build, which looks up one bundle per
// open PR, re-read that directory once per PR (hivecommons/hive#14681). The
// listing is memoised per parent for evidenceSiblingListTTL and dropped when
// this process creates a sibling itself; a sibling another writer creates
// becomes visible within the TTL.
func reviewEvidenceDirs(dir string) []string {
	dirs := []string{dir}
	base := filepath.Base(dir)
	for _, name := range evidenceSiblingNames(filepath.Dir(dir)) {
		rest, ok := strings.CutPrefix(name, base+evidenceFallbackDirMarker)
		if !ok {
			continue
		}
		if _, err := strconv.Atoi(rest); err != nil {
			continue
		}
		m := filepath.Join(filepath.Dir(dir), name)
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			dirs = append(dirs, m)
		}
	}
	return dirs
}

// evidenceSiblingListTTL bounds how long a repo directory's list of
// <pr>.uid<N> fallback siblings is reused before it is read again.
const evidenceSiblingListTTL = 60 * time.Second

// evidenceSiblingListMaxParents caps the memo; past it the whole memo is
// dropped rather than evicted entry by entry (a hive has tens of repos).
const evidenceSiblingListMaxParents = 512

type evidenceSiblingList struct {
	names []string // entries containing evidenceFallbackDirMarker, sorted
	at    time.Time
}

var (
	evidenceSiblingMu    sync.Mutex
	evidenceSiblingLists = map[string]evidenceSiblingList{}
	// evidenceSiblingNow is the clock the memo ages by; tests replace it.
	evidenceSiblingNow = time.Now
)

// evidenceSiblingNames is the sorted names under parent that contain the
// fallback marker, from the memo when it is younger than the TTL.
func evidenceSiblingNames(parent string) []string {
	now := evidenceSiblingNow()
	evidenceSiblingMu.Lock()
	if l, ok := evidenceSiblingLists[parent]; ok && now.Sub(l.at) < evidenceSiblingListTTL {
		evidenceSiblingMu.Unlock()
		return l.names
	}
	evidenceSiblingMu.Unlock()
	var names []string
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			if strings.Contains(e.Name(), evidenceFallbackDirMarker) {
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	evidenceSiblingMu.Lock()
	if len(evidenceSiblingLists) >= evidenceSiblingListMaxParents {
		evidenceSiblingLists = map[string]evidenceSiblingList{}
	}
	evidenceSiblingLists[parent] = evidenceSiblingList{names: names, at: now}
	evidenceSiblingMu.Unlock()
	return names
}

// forgetEvidenceSiblings drops the memoised sibling list for the parent of
// dir; called when this process creates a fallback sibling.
func forgetEvidenceSiblings(dir string) {
	evidenceSiblingMu.Lock()
	delete(evidenceSiblingLists, filepath.Dir(dir))
	evidenceSiblingMu.Unlock()
}

// resolveReviewEvidencePath is the most recently written copy of the bundle
// at path across the PR directory and its fallback siblings, or path itself
// when no copy exists (so callers still see os.ErrNotExist).
func resolveReviewEvidencePath(path string) string {
	dir, name := filepath.Dir(path), filepath.Base(path)
	if _, err := strconv.Atoi(filepath.Base(dir)); err != nil {
		return path
	}
	best := path
	var bestMod time.Time
	found := false
	for _, d := range reviewEvidenceDirs(dir) {
		p := filepath.Join(d, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		if !found || info.ModTime().After(bestMod) {
			best, bestMod, found = p, info.ModTime(), true
		}
	}
	return best
}

// widenOwnedEvidenceDir makes an existing evidence directory this uid owns
// and can write (e.g. 0o755, created before #13836) group-writable
// (evidenceDirMode), so it stops locking out other writers sharing its group.
// A directory deliberately made read-only for its owner is left alone.
func widenOwnedEvidenceDir(dir string) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	uid := os.Getuid()
	perm := info.Mode().Perm()
	if uid < 0 || fileOwnerUID(info) != uid || perm&0o200 == 0 || perm&0o020 != 0 {
		return
	}
	_ = os.Chmod(dir, evidenceDirMode)
}

// mkdirEvidenceDirs creates dir and any missing parents, applying
// evidenceDirMode to each component it created (MkdirAll is subject to umask
// and ignores setgid). Pre-existing directories are handled by
// widenOwnedEvidenceDir.
func mkdirEvidenceDirs(dir string) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, evidenceDirMode); err != nil {
		return err
	}
	for _, d := range missing {
		_ = os.Chmod(d, evidenceDirMode)
	}
	return nil
}

func (c *Client) reviewEvidencePolicy(p evidence.Policy) evidence.Policy {
	if len(p.Perspectives) == 0 {
		for _, name := range c.perspectives.List() {
			p.Perspectives = append(p.Perspectives, string(name))
		}
	}
	return p
}

// evidenceAuthor classifies the PR author. A PR the audit log records as
// opened by one of this hive's agents is "agent" even though GitHub shows the
// App bot as its author, and carries the agent, backend and model that audit
// entry recorded at launch; any other bot account is "bot".
func evidenceAuthor(repo string, number int, user *gh.User) evidence.Author {
	login := strings.TrimSpace(user.GetLogin())
	if m := auditAuthorMetaForPR(repo, number, reviewVerdictAuditPath, time.Now().UTC().Add(-reviewVerdictAuditLookback)); m.Agent != "" {
		return evidence.Author{Login: login, Kind: evidence.AuthorAgent, Agent: m.Agent, Backend: m.Backend, Model: m.Model}
	}
	switch {
	case strings.EqualFold(user.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]"):
		return evidence.Author{Login: login, Kind: evidence.AuthorBot}
	default:
		return evidence.Author{Login: login, Kind: evidence.AuthorHuman}
	}
}

// evidenceVerdict maps a recorded perspective report onto the bundle schema.
// Confidence is the binned self-report, with the historical verdict-derived
// score retained only for reports that omitted confidence. Gating is unchanged.
func evidenceVerdict(r review.PerspectiveReport, meta InvocationMeta, now time.Time) evidence.Verdict {
	model := strings.TrimSpace(r.ReviewModel)
	if model == "" {
		model = strings.TrimSpace(meta.Model)
	}
	if model == "" {
		model = "unknown"
	}
	score := review.ScoreConfidence([]review.PerspectiveReport{r}, 0).Score
	v := evidence.Verdict{
		Perspective: string(r.Perspective),
		Model:       model,
		Backend:     strings.TrimSpace(meta.Backend),
		Verdict:     string(r.Verdict),
		Confidence:  float64(score) / float64(review.ConfidenceMax),
		RecordedAt:  now,
		AuthorModel: strings.TrimSpace(r.AuthorModel),
	}
	if r.Confidence != nil {
		raw := *r.Confidence
		if r.ConfidenceRaw != nil {
			raw = *r.ConfidenceRaw
		}
		v.Confidence = review.SnapConfidence(raw)
		v.ConfidenceRaw = &raw
	}
	for _, f := range r.Findings {
		path := strings.TrimSpace(f.File)
		if path == "" {
			path = reviewEvidenceGeneralPath
		}
		summary := strings.TrimSpace(f.Summary)
		if summary == "" {
			summary = strings.TrimSpace(f.Title)
		}
		severity := strings.TrimSpace(string(f.Severity))
		if severity == "" {
			severity = "unknown"
		}
		if summary == "" {
			summary = severity + " finding"
		}
		v.Findings = append(v.Findings, evidence.Finding{Path: path, Line: f.Line, Severity: severity, Summary: summary})
	}
	return v
}

// containsEvidenceVerdict reports whether have already holds v. RecordedAt is
// ignored: the same verdict relayed twice is one verdict, while a changed
// verdict for the same perspective is new evidence and is kept beside the old.
// stampEvidenceVerdictFamily sets SameFamily once the bundle's author is
// known: the report's own author_model when it carried one, else the model
// the audit log recorded for the PR's agent. Unknown on either side leaves
// it nil.
func stampEvidenceVerdictFamily(v evidence.Verdict, author evidence.Author) evidence.Verdict {
	authorModel := v.AuthorModel
	if authorModel == "" {
		authorModel = author.Model
	}
	if same, known := modelfamily.SameFamily(v.Model, authorModel); known {
		v.SameFamily = &same
	}
	return v
}

func containsEvidenceVerdict(have []evidence.Verdict, v evidence.Verdict) bool {
	// Family stamps are derived, not observed: a verdict recorded before
	// they existed must still match its re-relayed self. Derived scores
	// snapshot the first observation: a retry must not become a second
	// verdict just because more peers or a newer CI state arrived.
	v.RecordedAt, v.AuthorModel, v.SameFamily, v.ConfidenceDerived = time.Time{}, "", nil, nil
	for _, h := range have {
		h.RecordedAt, h.AuthorModel, h.SameFamily, h.ConfidenceDerived = time.Time{}, "", nil, nil
		if reflect.DeepEqual(h, v) {
			return true
		}
	}
	return false
}

func containsPostedReview(have []evidence.PostedReview, url string) bool {
	for _, h := range have {
		if h.URL == url {
			return true
		}
	}
	return false
}

// previousReviewEvidenceID is the id of the newest bundle in dir (one PR's
// bundles) for a head other than head, or "" when there is none. A new push
// starts a new bundle; this is what keeps the chain of heads traceable.
func previousReviewEvidenceID(dir, head string) string {
	names, err := reviewEvidenceFileNames(dir)
	if err != nil {
		return ""
	}
	var best *evidence.Bundle
	for _, name := range names {
		b, err := LoadReviewEvidence(filepath.Join(dir, name))
		if err != nil || b.HeadSHA == head || b.ID == "" {
			continue
		}
		if best == nil || b.CreatedAt.After(best.CreatedAt) ||
			(b.CreatedAt.Equal(best.CreatedAt) && b.ID > best.ID) {
			best = b
		}
	}
	if best == nil {
		return ""
	}
	return best.ID
}

// Compare immutable SHAs rather than PR files, which may already describe a
// newer push. GitHub caps comparison files at 300; conservatively treat a full
// page as unknown rather than claiming a truncated diff is small/test-free.
func (c *Client) reviewConfidenceDiff(ctx context.Context, owner, name, base, head string) review.ConfidenceSignals {
	var s review.ConfidenceSignals
	if c.client == nil || base == "" || head == "" {
		return s
	}
	comparison, _, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:review_evidence"), owner, name, base, head, &gh.ListOptions{PerPage: 1})
	const comparisonFileLimit = 300
	if err != nil || comparison == nil || comparison.Files == nil || len(comparison.Files) >= comparisonFileLimit {
		return s
	}
	s.DiffKnown = true
	for _, f := range comparison.Files {
		s.ChangedLines += f.GetAdditions() + f.GetDeletions()
		s.TouchesTests = s.TouchesTests || review.ConfidenceTestPath(f.GetFilename())
	}
	return s
}

func evidenceConfidenceReport(v evidence.Verdict, repo string, number int, head string) review.PerspectiveReport {
	r := review.PerspectiveReport{Repo: repo, Number: number, HeadSHA: head, Perspective: review.Perspective(v.Perspective), Verdict: review.Verdict(v.Verdict)}
	for _, f := range v.Findings {
		r.Findings = append(r.Findings, outputschema.Finding{File: f.Path, Line: f.Line})
	}
	return r
}
