// Package buildcontains answers "does the build this hive is running contain
// PR #N (or commit X)?" from git ancestry, and from nothing else.
//
// Image tags move, builds can be untagged, and a digest says nothing about
// history, so "merged" is routinely mistaken for "deployed". The only sound
// answer is ancestry: the PR's merge commit is an ancestor of (or equal to)
// the commit the hive reports running, or it is not.
//
// The package is deliberately small and pure. The repository history lives
// behind the Source interface so the hub and a hive's own dashboard can each
// supply their own GitHub client, and so tests can assert the invariants
// without a network:
//
//   - A commit that cannot be resolved is Unknown, never Yes and never No.
//   - A PR whose merge commit is not in the running build's history is No.
//   - Any lookup failure is Unknown. A guess is never returned.
package buildcontains

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Verdict is the three-valued answer to "does this build contain it?".
type Verdict string

const (
	// Yes means the queried commit is the running commit or one of its ancestors.
	Yes Verdict = "yes"
	// No means ancestry was resolved and the queried commit is not in the
	// running build's history (or the PR was never merged).
	No Verdict = "no"
	// Unknown means ancestry could not be established. It is the only
	// permitted answer to a failed, partial or unresolvable lookup.
	Unknown Verdict = "unknown"
)

// QueryKind says how a Query was interpreted.
type QueryKind string

const (
	KindPR     QueryKind = "pr"
	KindCommit QueryKind = "commit"
)

const (
	// maxPRDigits bounds what is read as a PR number. A string of seven or
	// more digits is read as a commit SHA instead: the repository will not
	// reach a million PRs, while a 7-character SHA made only of digits is
	// ordinary.
	maxPRDigits = 6
	// minSHALen and maxSHALen bound an accepted hex commit reference: the
	// canonical 7-character short form up to a full 40-character SHA-1.
	minSHALen = 7
	maxSHALen = 40
	// MaxCompares caps how many distinct running commits one lookup compares.
	// Each is a remote request, so a fleet of hives on many different builds
	// must not fan out without limit; the rest answer Unknown.
	MaxCompares = 40
	// compareConcurrency bounds in-flight compares for one lookup.
	compareConcurrency = 4
	// cacheMax bounds a CachingSource. Dropped wholesale on overflow: an
	// entry is one cheap request to re-derive.
	cacheMax = 1024
)

// Reasons attached to Unknown/No results. They are display text for operators
// and part of the read API, so they are constants.
const (
	ReasonNoRunningCommit  = "the hive reports no resolvable commit"
	ReasonCommitNotFound   = "the commit is not in the repository history this hub can read (unpublished, force-pushed or from a fork)"
	ReasonCompareFailed    = "the history lookup failed; try again"
	ReasonCompareStatus    = "the history lookup returned an unrecognised status"
	ReasonPRNotFound       = "pull request not found"
	ReasonPRLookupFailed   = "the pull request lookup failed; try again"
	ReasonPRNotMerged      = "the pull request is not merged, so no build of the base branch contains it"
	ReasonPRNoMergeCommit  = "the pull request is merged but reports no merge commit"
	ReasonBudgetExceeded   = "too many distinct builds in one lookup; narrow the fleet or retry"
	ReasonNoQueryCommit    = "no commit to look up"
	reasonBadQueryTemplate = "not a PR number or a commit SHA: %q"
)

// Query is a parsed lookup: a PR number or a commit SHA.
type Query struct {
	Kind QueryKind `json:"kind"`
	// PR is set when Kind is KindPR.
	PR int `json:"pr,omitempty"`
	// Commit is set when Kind is KindCommit (lowercased, as typed).
	Commit string `json:"commit,omitempty"`
}

// ParseQuery reads a PR number ("123", "#123", "PR 123", "pr#123") or a commit
// SHA (7 to 40 hex characters). Digits-only input of up to six digits is a PR
// number; anything else must be a hex SHA. Everything else is an error.
func ParseQuery(raw string) (Query, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return Query{}, fmt.Errorf(reasonBadQueryTemplate, raw)
	}
	digits := strings.TrimLeft(strings.TrimPrefix(s, "pr"), " #-:")
	if strings.HasPrefix(s, "#") {
		digits = strings.TrimLeft(s, "# ")
	}
	if isDigits(digits) && len(digits) <= maxPRDigits {
		n, err := strconv.Atoi(digits)
		if err == nil && n > 0 {
			return Query{Kind: KindPR, PR: n}, nil
		}
		return Query{}, fmt.Errorf(reasonBadQueryTemplate, raw)
	}
	if IsCommitRef(s) {
		return Query{Kind: KindCommit, Commit: s}, nil
	}
	return Query{}, fmt.Errorf(reasonBadQueryTemplate, raw)
}

// IsCommitRef reports whether s is a plausible hex commit reference. A hive
// that reports "unknown", "dev" or an empty string is therefore not resolvable.
func IsCommitRef(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < minSHALen || len(s) > maxSHALen {
		return false
	}
	for _, c := range s {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// PullRequest is the slice of a pull request the lookup needs.
type PullRequest struct {
	Merged      bool
	MergeCommit string
}

// Source supplies repository history. Implementations must report "not found"
// through the found flag and reserve err for transport and server failures, so
// that a missing commit (Unknown, reason says why) is distinguishable from a
// lookup that failed (Unknown, try again).
type Source interface {
	// Compare returns GitHub's compare status of base...head: "identical",
	// "ahead" (head has commits base lacks, base is an ancestor), "behind" or
	// "diverged". found is false when either commit cannot be resolved.
	Compare(ctx context.Context, base, head string) (status string, found bool, err error)
	// PullRequest looks one pull request up. found is false when it does not exist.
	PullRequest(ctx context.Context, number int) (pr PullRequest, found bool, err error)
}

// Result is one hive's answer.
type Result struct {
	Contains Verdict `json:"contains"`
	Reason   string  `json:"reason,omitempty"`
}

// Target is a resolved Query: the commit whose presence is being tested, or an
// answer that holds for every hive without any comparison.
type Target struct {
	// Query is what was asked.
	Query Query `json:"query"`
	// Commit is the commit tested for presence ("" when Final).
	Commit string `json:"commit,omitempty"`
	// Final is true when the answer is the same for every hive and needs no
	// comparison (an unmerged or unknown PR).
	Final bool `json:"-"`
	// Answer holds the verdict when Final.
	Answer Result `json:"-"`
}

// Resolve turns a Query into the commit to test. A PR resolves to its merge
// commit; a PR that is not merged resolves to a final No, and one that cannot
// be read resolves to a final Unknown.
func Resolve(ctx context.Context, src Source, q Query) Target {
	t := Target{Query: q}
	switch q.Kind {
	case KindCommit:
		if !IsCommitRef(q.Commit) {
			t.Final, t.Answer = true, Result{Contains: Unknown, Reason: ReasonNoQueryCommit}
			return t
		}
		t.Commit = q.Commit
	case KindPR:
		pr, found, err := src.PullRequest(ctx, q.PR)
		switch {
		case err != nil:
			t.Final, t.Answer = true, Result{Contains: Unknown, Reason: ReasonPRLookupFailed}
		case !found:
			t.Final, t.Answer = true, Result{Contains: Unknown, Reason: ReasonPRNotFound}
		case !pr.Merged:
			t.Final, t.Answer = true, Result{Contains: No, Reason: ReasonPRNotMerged}
		case !IsCommitRef(pr.MergeCommit):
			t.Final, t.Answer = true, Result{Contains: Unknown, Reason: ReasonPRNoMergeCommit}
		default:
			t.Commit = pr.MergeCommit
		}
	default:
		t.Final, t.Answer = true, Result{Contains: Unknown, Reason: ReasonNoQueryCommit}
	}
	return t
}

// Check answers whether running contains the target commit.
func (t Target) Check(ctx context.Context, src Source, running string) Result {
	running = strings.TrimSpace(running)
	if !IsCommitRef(running) {
		return Result{Contains: Unknown, Reason: ReasonNoRunningCommit}
	}
	if t.Final {
		return t.Answer
	}
	if t.Commit == "" {
		return Result{Contains: Unknown, Reason: ReasonNoQueryCommit}
	}
	status, found, err := src.Compare(ctx, t.Commit, running)
	if err != nil {
		return Result{Contains: Unknown, Reason: ReasonCompareFailed}
	}
	if !found {
		return Result{Contains: Unknown, Reason: ReasonCommitNotFound}
	}
	switch status {
	case "identical", "ahead":
		return Result{Contains: Yes}
	case "behind", "diverged":
		return Result{Contains: No}
	default:
		return Result{Contains: Unknown, Reason: ReasonCompareStatus}
	}
}

// CheckAll answers for many running commits at once, keyed by the commit as
// given. Each distinct commit is compared once, at most MaxCompares of them,
// with bounded concurrency; the remainder are Unknown.
func (t Target) CheckAll(ctx context.Context, src Source, running []string) map[string]Result {
	out := make(map[string]Result, len(running))
	var todo []string
	for _, r := range running {
		if _, seen := out[r]; seen {
			continue
		}
		if t.Final || !IsCommitRef(r) {
			out[r] = t.Check(ctx, src, r)
			continue
		}
		if len(todo) >= MaxCompares {
			out[r] = Result{Contains: Unknown, Reason: ReasonBudgetExceeded}
			continue
		}
		out[r] = Result{Contains: Unknown, Reason: ReasonCompareFailed}
		todo = append(todo, r)
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, compareConcurrency)
	)
	for _, r := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func(r string) {
			defer wg.Done()
			defer func() { <-sem }()
			res := t.Check(ctx, src, r)
			mu.Lock()
			out[r] = res
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	return out
}

type compareKey struct{ base, head string }

type compareValue struct {
	status string
}

// CachingSource memoises definitive compare answers. Ancestry between two
// fixed commits never changes, so a found result is cached for good; failures
// and not-found results are never cached, so a transient error cannot stick.
type CachingSource struct {
	inner Source
	mu    sync.Mutex
	cache map[compareKey]compareValue
}

// NewCachingSource wraps inner with the compare cache.
func NewCachingSource(inner Source) *CachingSource {
	return &CachingSource{inner: inner, cache: map[compareKey]compareValue{}}
}

// Compare implements Source.
func (c *CachingSource) Compare(ctx context.Context, base, head string) (string, bool, error) {
	key := compareKey{base: strings.ToLower(base), head: strings.ToLower(head)}
	c.mu.Lock()
	v, ok := c.cache[key]
	c.mu.Unlock()
	if ok {
		return v.status, true, nil
	}
	status, found, err := c.inner.Compare(ctx, base, head)
	if err != nil || !found {
		return status, found, err
	}
	c.mu.Lock()
	if len(c.cache) >= cacheMax {
		c.cache = map[compareKey]compareValue{}
	}
	c.cache[key] = compareValue{status: status}
	c.mu.Unlock()
	return status, true, nil
}

// PullRequest implements Source. Pull request state changes (open, then
// merged), so it is never cached.
func (c *CachingSource) PullRequest(ctx context.Context, number int) (PullRequest, bool, error) {
	return c.inner.PullRequest(ctx, number)
}
