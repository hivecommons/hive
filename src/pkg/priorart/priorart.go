// Package priorart is the deterministic half of the triage prior-art gate
// (hivecommons/hive#13575): given a feature / RFC / epic issue it derives the
// issue's key terms, asks a set of Sources (knowledge beads, merged PRs and
// closed issues, docs, settings, code) for candidates, ranks them by how much
// of the issue's weighted vocabulary each covers, and reports the findings
// with a confidence. The scheduler parks the issue at
// stage/017-possible-existing when the confidence reaches the configured
// threshold; the author's reply un-parks it (pkg/github, prior_art_gate_resume).
//
// Like pkg/rfccoverage the package is stdlib-only and makes no model call:
// the product is a ranked, bounded, cached list a person reviews. Every
// Source is optional and every Source error is soft — a hive without
// knowledge or without code search still gets findings from what it has.
package priorart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Marker opens the gate's park comment. Its presence on an issue means the
// author has been asked once; the gate never asks again.
const Marker = "<!-- hive:prior-art-gate -->"

// Kind is where a candidate came from.
type Kind string

const (
	KindKnowledge   Kind = "knowledge"
	KindMergedPR    Kind = "merged_pr"
	KindClosedIssue Kind = "closed_issue"
	KindOpenIssue   Kind = "open_issue"
	KindDoc         Kind = "doc"
	KindSetting     Kind = "setting"
	KindCode        Kind = "code"
)

// Query is what a Source is asked.
type Query struct {
	Repo string
	// Terms are the issue's key terms, highest weight first.
	Terms []Term
	// Identifiers are the Terms that look like code identifiers, config
	// keys or flags (a subset of Terms); code and docs sources search by
	// these first.
	Identifiers []string
}

// TopTerms returns up to n term strings, highest weight first.
func (q Query) TopTerms(n int) []string {
	out := make([]string, 0, n)
	for _, t := range q.Terms {
		if len(out) == n {
			break
		}
		out = append(out, t.Text)
	}
	return out
}

// Candidate is one thing a Source found that may already cover the issue.
type Candidate struct {
	Kind  Kind
	Title string
	// Text is the searchable body (PR body, bead body, doc excerpt, code
	// line). Only the first SnippetMax runes of the best-matching line are
	// quoted.
	Text string
	URL  string
	// Ref is a short human handle: "#123", "docs/triage.md", "bead slug".
	Ref string
	// Terms are extra matched terms the source knows the file contains but
	// Text does not show; the ranker counts them, the quote never does.
	Terms []string
}

// Source is one place prior art may live.
type Source interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Candidate, error)
}

// Finding is a ranked Candidate.
type Finding struct {
	Candidate
	// Confidence is 0-100: the share of the issue's weighted key terms the
	// candidate covers (title terms and identifiers weigh more).
	Confidence int
	// Matched are the terms shared with the issue, sorted.
	Matched []string
	// Quote is the one line of the candidate that matched best.
	Quote string
}

// Report is the result of one search.
type Report struct {
	Findings []Finding
	// Confidence is the best finding's confidence (0 when none).
	Confidence int
	Terms      []Term
	// Sources names the sources that answered; Errors counts those that
	// failed (their candidates are simply absent).
	Sources []string
	Errors  int
	// Coverage lists the gaps sources reported (e.g. "checkout missing for
	// org/repo"): the findings were searched without that evidence, so an
	// empty or weak report is not proof that the feature is absent.
	Coverage []string
	Cached   bool
}

// CoverageError is returned by a Source that answered from incomplete
// evidence. Unlike any other error, its candidates are still ranked and the
// source counts as answered; Gaps are copied into Report.Coverage.
type CoverageError struct {
	Gaps []string
	// Transient marks gaps that clear on their own (an index still
	// building); a report carrying one is not cached.
	Transient bool
}

func (e *CoverageError) Error() string {
	return "prior art: incomplete coverage: " + strings.Join(e.Gaps, "; ")
}

// Issue is the gate's view of an issue.
type Issue struct {
	Repo   string
	Number int
	Title  string
	Body   string
	Labels []string
}

// Options configures a Searcher.
type Options struct {
	Sources []Source
	// MaxFindings caps Report.Findings. Zero means DefaultMaxFindings.
	MaxFindings int
	// MinConfidence drops findings under it. Zero means DefaultMinConfidence.
	MinConfidence int
	// Budget bounds one Find across all sources. Zero means DefaultBudget.
	Budget time.Duration
	// CacheTTL is how long a report is reused for an unchanged title+body.
	// Zero means DefaultCacheTTL.
	CacheTTL time.Duration
	// Now is the clock (tests inject one). Nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

const (
	DefaultMaxFindings   = 5
	DefaultMinConfidence = 35
	DefaultBudget        = 20 * time.Second
	DefaultCacheTTL      = 24 * time.Hour
	// maxCacheEntries bounds process memory on a busy hive.
	maxCacheEntries = 2048
	// SnippetMax is the longest quote rendered per finding.
	SnippetMax = 160
)

// Searcher runs the sources and ranks what they return.
type Searcher struct {
	opts  Options
	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	at     time.Time
	report Report
}

// NewSearcher returns a Searcher over opts.
func NewSearcher(opts Options) *Searcher {
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = DefaultMaxFindings
	}
	if opts.MinConfidence <= 0 {
		opts.MinConfidence = DefaultMinConfidence
	}
	if opts.Budget <= 0 {
		opts.Budget = DefaultBudget
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Searcher{opts: opts, cache: map[string]cacheEntry{}}
}

// CacheKey is the per-issue cache key: a hash of repo, title and body, so an
// edited issue is searched again and an unchanged one is not.
func CacheKey(issue Issue) string {
	h := sha256.Sum256([]byte(issue.Repo + "\x00" + issue.Title + "\x00" + issue.Body))
	return hex.EncodeToString(h[:8])
}

// Find searches every source for issue and returns the ranked report. It
// never returns an error: a source that fails is counted in Report.Errors
// and the rest still answer. With no usable terms the report is empty.
func (s *Searcher) Find(ctx context.Context, issue Issue) Report {
	key := CacheKey(issue)
	now := s.opts.Now()
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && now.Sub(e.at) < s.opts.CacheTTL {
		s.mu.Unlock()
		r := e.report
		r.Cached = true
		return r
	}
	s.mu.Unlock()

	terms := ExtractTerms(issue.Title, issue.Body)
	report := Report{Terms: terms}
	if len(terms) == 0 || len(s.opts.Sources) == 0 {
		s.remember(key, now, report)
		return report
	}
	q := Query{Repo: issue.Repo, Terms: terms, Identifiers: identifiers(terms)}
	ctx, cancel := context.WithTimeout(ctx, s.opts.Budget)
	defer cancel()

	var findings []Finding
	transient := false
	seen := map[string]bool{}
	for _, src := range s.opts.Sources {
		if ctx.Err() != nil {
			report.Errors++
			continue
		}
		cands, err := src.Search(ctx, q)
		var gap *CoverageError
		if errors.As(err, &gap) {
			for _, g := range gap.Gaps {
				if !slices.Contains(report.Coverage, g) {
					report.Coverage = append(report.Coverage, g)
				}
			}
			transient = transient || gap.Transient
			s.opts.Logger.Info("prior art: source coverage incomplete", "source", src.Name(), "repo", issue.Repo, "issue", issue.Number, "gaps", gap.Gaps)
			err = nil
		}
		if err != nil {
			report.Errors++
			s.opts.Logger.Debug("prior art: source failed", "source", src.Name(), "repo", issue.Repo, "issue", issue.Number, "error", err)
			continue
		}
		report.Sources = append(report.Sources, src.Name())
		for _, c := range cands {
			if sameItem(c, issue) {
				continue
			}
			f, ok := Rank(terms, c)
			if !ok || f.Confidence < s.opts.MinConfidence {
				continue
			}
			id := string(c.Kind) + "\x00" + c.URL + "\x00" + c.Ref + "\x00" + c.Title
			if seen[id] {
				continue
			}
			seen[id] = true
			findings = append(findings, f)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Confidence != findings[j].Confidence {
			return findings[i].Confidence > findings[j].Confidence
		}
		if findings[i].Kind != findings[j].Kind {
			return kindRank(findings[i].Kind) < kindRank(findings[j].Kind)
		}
		return findings[i].Ref < findings[j].Ref
	})
	if len(findings) > s.opts.MaxFindings {
		findings = findings[:s.opts.MaxFindings]
	}
	report.Findings = findings
	if len(findings) > 0 {
		report.Confidence = findings[0].Confidence
	}
	// A report built while a source was still warming up, or cut short by
	// the budget, would otherwise stand in for a clean answer for the TTL.
	if !transient && ctx.Err() == nil {
		s.remember(key, now, report)
	}
	return report
}

func (s *Searcher) remember(key string, now time.Time, r Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) >= maxCacheEntries {
		for k, e := range s.cache {
			if now.Sub(e.at) >= s.opts.CacheTTL {
				delete(s.cache, k)
			}
		}
		if len(s.cache) >= maxCacheEntries {
			s.cache = map[string]cacheEntry{}
		}
	}
	s.cache[key] = cacheEntry{at: now, report: r}
}

// sameItem drops the issue itself when a forge source echoes it back.
func sameItem(c Candidate, issue Issue) bool {
	if issue.Number <= 0 {
		return false
	}
	if c.Kind != KindOpenIssue && c.Kind != KindClosedIssue && c.Kind != KindMergedPR {
		return false
	}
	n := strconv.Itoa(issue.Number)
	return c.Ref == "#"+n || strings.HasSuffix(c.URL, "/"+n)
}

// kindRank orders equal-confidence findings: shipped work first, then the
// places a person can read, then code.
func kindRank(k Kind) int {
	switch k {
	case KindMergedPR:
		return 0
	case KindSetting:
		return 1
	case KindDoc:
		return 2
	case KindKnowledge:
		return 3
	case KindClosedIssue:
		return 4
	case KindOpenIssue:
		return 5
	case KindCode:
		return 6
	}
	return 7
}

// SourceFunc adapts a function to Source.
type SourceFunc struct {
	SourceName string
	Fn         func(ctx context.Context, q Query) ([]Candidate, error)
}

// Name implements Source.
func (s SourceFunc) Name() string { return s.SourceName }

// Search implements Source.
func (s SourceFunc) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if s.Fn == nil {
		return nil, nil
	}
	return s.Fn(ctx, q)
}
