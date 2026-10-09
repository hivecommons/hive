package github

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
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

// LoadReviewEvidence reads one bundle. A missing file reports an error
// wrapping os.ErrNotExist.
func LoadReviewEvidence(path string) (*evidence.Bundle, error) {
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
		c.upsertReviewEvidence(repo, req.Number, head, *updates[head], settings.Policy, key, fetchPR, now)
	}
}

// upsertReviewEvidence merges u into the bundle for repo#number@head,
// creating it (with the policy snapshot and a link to the previous head's
// bundle) on first write. A pass that adds nothing new leaves the file
// untouched, so re-running the relay never duplicates entries or churns the
// hash.
func (c *Client) upsertReviewEvidence(repo string, number int, head string, u reviewEvidenceUpdate, policy evidence.Policy, key ed25519.PrivateKey, fetchPR func() *gh.PullRequest, now time.Time) {
	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()

	path := ReviewEvidencePath("", repo, number, head)
	b, err := LoadReviewEvidence(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		pr := fetchPR()
		if pr == nil {
			c.logger.Warn("review evidence: PR metadata unavailable, bundle not started",
				slog.String("repo", repo), slog.Int("number", number), slog.String("head", head))
			return
		}
		b = &evidence.Bundle{
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
	default:
		// An unreadable bundle is still evidence. Overwriting it with a fresh
		// one would silently replace the record, so it is left for a person.
		c.logger.Warn("review evidence: existing bundle unreadable, left untouched",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	changed := b.UpdatedAt.IsZero()
	for _, v := range u.verdicts {
		if !containsEvidenceVerdict(b.Verdicts, v) {
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
		c.logger.Warn("review evidence: could not write bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	c.logger.Info("review evidence: bundle written",
		slog.String("id", b.ID), slog.Int("verdicts", len(b.Verdicts)),
		slog.Int("posted_reviews", len(b.PostedReviews)), slog.Int("sentinel", len(b.Sentinel)),
		slog.Bool("signed", b.Signed),
		slog.String("path", path))
}

// recordReviewEvidenceSentinel adds the findings the sentinel sweep just
// flagged pr for to the bundle for its head. pr is the listing the sweep
// already holds, so a head the relay has not written yet gets its bundle
// started from it (author, base, policy) without another fetch. Like the
// relay's writes, every failure is logged and swallowed: the alert label and
// comment are already on GitHub.
func (c *Client) recordReviewEvidenceSentinel(repo string, pr *gh.PullRequest, findings []sentinel.Finding, now time.Time) {
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

// writeReviewEvidence commits b with write-then-rename so a reader never
// observes a half-written bundle.
func writeReviewEvidence(path string, b *evidence.Bundle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
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
// App bot as its author; any other bot account is "bot".
func evidenceAuthor(repo string, number int, user *gh.User) evidence.Author {
	login := strings.TrimSpace(user.GetLogin())
	switch {
	case auditAuthorAgentForPR(repo, number, reviewVerdictAuditPath, time.Now().UTC().Add(-reviewVerdictAuditLookback)) != "":
		return evidence.Author{Login: login, Kind: evidence.AuthorAgent}
	case strings.EqualFold(user.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]"):
		return evidence.Author{Login: login, Kind: evidence.AuthorBot}
	default:
		return evidence.Author{Login: login, Kind: evidence.AuthorHuman}
	}
}

// evidenceVerdict maps a recorded perspective report onto the bundle schema.
// Confidence is that perspective's own review.ScoreConfidence score scaled to
// [0,1], so it means the same thing the comment's Confidence line does.
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
func containsEvidenceVerdict(have []evidence.Verdict, v evidence.Verdict) bool {
	v.RecordedAt = time.Time{}
	for _, h := range have {
		h.RecordedAt = time.Time{}
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best *evidence.Bundle
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := LoadReviewEvidence(filepath.Join(dir, e.Name()))
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
