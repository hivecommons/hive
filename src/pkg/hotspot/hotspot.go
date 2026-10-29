// Package hotspot finds files that concurrent pull requests keep touching
// and turns the worst of them into one modularize issue each for the
// ci-maintainer lane.
//
// It adds no GitHub polling of its own: the merge-train planner already
// fetches every candidate PR's file list to predict overlaps, and the
// rebase follow-through already records conflicting files. Both feed the
// Store here; Analyze reduces the window to per-file evidence and a
// recommendation; the Filer writes at most one open issue per file through
// the writeback gate.
package hotspot

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PRFiles is one pull request's file list as last observed.
type PRFiles struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	Files    []string  `json:"files"`
	OpenedAt time.Time `json:"opened_at"`
	// ClosedAt is zero while the PR is open. Merged and closed-unmerged
	// PRs both end the open interval; only Merged ones count as "merged".
	ClosedAt time.Time `json:"closed_at,omitempty"`
	Merged   bool      `json:"merged,omitempty"`
	// SeenAt is when the file list was last refreshed.
	SeenAt time.Time `json:"seen_at"`
}

// Conflict records one merge-conflict or rebase-failure event and the files
// GitHub (or the rebase follow-through) blamed for it.
type Conflict struct {
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Files  []string  `json:"files"`
	At     time.Time `json:"at"`
}

// Kind classifies a path for the recommendation rules.
type Kind string

const (
	KindCode      Kind = "code"
	KindDoc       Kind = "doc"
	KindLock      Kind = "lock"
	KindGenerated Kind = "generated"
	KindData      Kind = "data"
)

// Recommendation is what the advisor suggests for a hotspot.
type Recommendation string

const (
	// RecommendNone: below thresholds or guarded; reported only.
	RecommendNone Recommendation = "none"
	// RecommendSplitCode: file an issue to split the file along the symbol
	// groups that collided.
	RecommendSplitCode Recommendation = "split-code"
	// RecommendSplitDoc: a large document whose sections collide; suggest
	// splitting by section/area (like docs/api-reference/<area>.md).
	RecommendSplitDoc Recommendation = "split-doc"
	// RecommendSplitSource: a generated file; the recommendation points at
	// its source files instead.
	RecommendSplitSource Recommendation = "split-source"
	// RecommendReportOnly: lock/data files, docs below the size bar, or
	// generated files whose source tree is already split into more than
	// MaxUnits files.
	RecommendReportOnly Recommendation = "report-only"
)

// Hotspot is the per-file result.
type Hotspot struct {
	Repo      string `json:"repo"`
	File      string `json:"file"`
	Kind      Kind   `json:"kind"`
	PRs       int    `json:"prs"`
	PRNumbers []int  `json:"pr_numbers"`
	// Overlaps counts PR pairs that were open at the same time and both
	// touched File.
	Overlaps int `json:"overlaps"`
	// Conflicts counts conflict events attributable to File.
	Conflicts int `json:"conflicts"`
	// Lines is the file's line count, or 0 when the source was unreadable.
	Lines int     `json:"lines"`
	Score float64 `json:"score"`
	// SymbolGroups are the distinct top-level units of the file (funcs and
	// types for Go, declarations for JS/TS/py, `##` headings for md).
	SymbolGroups   []string       `json:"symbol_groups,omitempty"`
	Recommendation Recommendation `json:"recommendation"`
	// Reason explains why Recommendation is what it is, in one sentence.
	Reason string `json:"reason"`
	// GeneratedFrom names the source tree for generated files.
	GeneratedFrom string `json:"generated_from,omitempty"`
	// SourceFiles is how many files GeneratedFrom already holds (0 when
	// unknown); see Options.SourceFileCount.
	SourceFiles int `json:"source_files,omitempty"`
	// IssueNumber / IssueURL point at the open or most recent modularize
	// issue for this file, if any.
	IssueNumber int       `json:"issue_number,omitempty"`
	IssueURL    string    `json:"issue_url,omitempty"`
	LastUpdated time.Time `json:"last_updated"`
}

// Options are the thresholds; zero values take the documented defaults.
type Options struct {
	Window      time.Duration
	MinPRs      int
	MinOverlaps int
	MinLines    int
	// MinGroups is the don't-splinter guard: a split is only recommended
	// when at least this many distinct symbol groups exist. Default 2.
	MinGroups int
	// MaxUnits caps the proposed split. Default 4.
	MaxUnits int
	// MinUnitLines is the smallest unit the proposal may create. Default 150.
	MinUnitLines int
	// SourceFileCount, when set, returns how many files a generated file's
	// source tree already holds (0 when unknown). A tree already split into
	// more than MaxUnits files is not proposed for another split.
	SourceFileCount func(repo, dir string) int
}

// Defaults mirror governor.hotspot_* in pkg/config.
const (
	DefaultWindow       = 7 * 24 * time.Hour
	DefaultMinPRs       = 6
	DefaultMinOverlaps  = 3
	DefaultMinLines     = 800
	DefaultMinGroups    = 2
	DefaultMaxUnits     = 4
	DefaultMinUnitLines = 150
	// DefaultDocMinLines is the size bar above which a document is
	// recommended for a by-section split rather than merely reported.
	DefaultDocMinLines = 1500
)

func (o Options) withDefaults() Options {
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.MinPRs <= 0 {
		o.MinPRs = DefaultMinPRs
	}
	if o.MinOverlaps <= 0 {
		o.MinOverlaps = DefaultMinOverlaps
	}
	if o.MinLines <= 0 {
		o.MinLines = DefaultMinLines
	}
	if o.MinGroups <= 0 {
		o.MinGroups = DefaultMinGroups
	}
	if o.MaxUnits <= 0 {
		o.MaxUnits = DefaultMaxUnits
	}
	if o.MinUnitLines <= 0 {
		o.MinUnitLines = DefaultMinUnitLines
	}
	return o
}

// FileInfo is what Analyze needs to know about a file's current content.
// Source may be nil when the file could not be read; Lines is then 0 and
// no split is recommended (the guard needs a size).
type FileInfo struct {
	Lines  int
	Source []byte
}

// SourceReader resolves a repo-relative path to its current content. It may
// return (FileInfo{}, false) when the file is unavailable.
type SourceReader func(repo, path string) (FileInfo, bool)

// Analyze reduces the PR and conflict records of one repo to hotspots at or
// above the thresholds. Files below the thresholds are omitted. The result
// is sorted by Score descending, then by path.
//
// Hunk-level attribution (which lines each PR changed) is not retained by
// the merge train, so the weighting by "same region" the owner suggested is
// skipped; symbol groups come from the file's current top-level
// declarations instead — see Groups.
func Analyze(repo string, prs []PRFiles, conflicts []Conflict, read SourceReader, opts Options, now time.Time) []Hotspot {
	opts = opts.withDefaults()
	since := now.Add(-opts.Window)

	type agg struct {
		prs       []PRFiles
		conflicts int
	}
	byFile := map[string]*agg{}
	for _, pr := range prs {
		if pr.Repo != repo || !inWindow(pr, since) {
			continue
		}
		seen := map[string]bool{}
		for _, f := range pr.Files {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			a := byFile[f]
			if a == nil {
				a = &agg{}
				byFile[f] = a
			}
			a.prs = append(a.prs, pr)
		}
	}
	for _, c := range conflicts {
		if c.Repo != repo || c.At.Before(since) {
			continue
		}
		for _, f := range c.Files {
			if a := byFile[f]; a != nil {
				a.conflicts++
			}
		}
	}

	var out []Hotspot
	for file, a := range byFile {
		if len(a.prs) < opts.MinPRs {
			continue
		}
		overlaps := pairwiseOverlaps(a.prs)
		if overlaps < opts.MinOverlaps {
			continue
		}
		h := Hotspot{
			Repo:        repo,
			File:        file,
			PRs:         len(a.prs),
			Overlaps:    overlaps,
			Conflicts:   a.conflicts,
			LastUpdated: now,
		}
		for _, pr := range a.prs {
			h.PRNumbers = append(h.PRNumbers, pr.Number)
		}
		sort.Ints(h.PRNumbers)

		var info FileInfo
		if read != nil {
			info, _ = read(repo, file)
		}
		h.Lines = info.Lines
		h.Kind, h.GeneratedFrom = Classify(file, info.Source)
		if h.Kind == KindCode || h.Kind == KindDoc {
			h.SymbolGroups = Groups(file, info.Source)
		}
		h.Score = score(h)
		if h.Kind == KindGenerated && h.GeneratedFrom != "" && opts.SourceFileCount != nil {
			h.SourceFiles = opts.SourceFileCount(repo, h.GeneratedFrom)
		}
		h.Recommendation, h.Reason = recommend(h, opts)
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].File < out[j].File
	})
	return out
}

// inWindow reports whether the PR was open at any point since `since`.
func inWindow(pr PRFiles, since time.Time) bool {
	return pr.ClosedAt.IsZero() || !pr.ClosedAt.Before(since)
}

// pairwiseOverlaps counts PR pairs whose open intervals intersect.
func pairwiseOverlaps(prs []PRFiles) int {
	n := 0
	for i := 0; i < len(prs); i++ {
		for j := i + 1; j < len(prs); j++ {
			if intervalsIntersect(prs[i], prs[j]) {
				n++
			}
		}
	}
	return n
}

func intervalsIntersect(a, b PRFiles) bool {
	aEnd, bEnd := a.ClosedAt, b.ClosedAt
	// Open PRs extend to +inf.
	if aEnd.IsZero() {
		aEnd = time.Unix(1<<40, 0)
	}
	if bEnd.IsZero() {
		bEnd = time.Unix(1<<40, 0)
	}
	return !a.OpenedAt.After(bEnd) && !b.OpenedAt.After(aEnd)
}

// score weights overlaps by file size (bigger files are costlier to
// collide on and to rebase) and counts a conflict as two overlaps.
func score(h Hotspot) float64 {
	sizeWeight := 1.0
	switch {
	case h.Lines >= 4000:
		sizeWeight = 2.0
	case h.Lines >= 1500:
		sizeWeight = 1.5
	case h.Lines >= 800:
		sizeWeight = 1.25
	}
	return float64(h.Overlaps)*sizeWeight + 2*float64(h.Conflicts)
}

func recommend(h Hotspot, opts Options) (Recommendation, string) {
	switch h.Kind {
	case KindGenerated:
		if h.GeneratedFrom != "" && h.SourceFiles > opts.MaxUnits {
			return RecommendReportOnly, fmt.Sprintf("generated file; its source tree %s is already split into %d files — regeneration makes every source change touch this output; nothing to split", h.GeneratedFrom, h.SourceFiles)
		}
		if h.GeneratedFrom != "" {
			return RecommendSplitSource, "generated file; the collisions are in " + h.GeneratedFrom + " — look there, never edit the output by hand"
		}
		return RecommendReportOnly, "generated file with no known source; reported only"
	case KindLock, KindData:
		return RecommendReportOnly, "lock/data file; collisions resolve by regenerating, not by splitting"
	case KindDoc:
		if h.Lines < DefaultDocMinLines {
			return RecommendReportOnly, "document below the size bar; reported only"
		}
		if len(h.SymbolGroups) < opts.MinGroups {
			return RecommendReportOnly, "document has fewer than two top-level sections; nothing to split along"
		}
		return RecommendSplitDoc, "large document whose sections collide; split by section/area like docs/api-reference/<area>.md"
	}
	if h.Lines == 0 {
		return RecommendNone, "source unreadable; cannot size the file"
	}
	if h.Lines < opts.MinLines {
		return RecommendNone, "below governor.hotspot_min_lines; splitting would splinter the code"
	}
	if len(h.SymbolGroups) < opts.MinGroups {
		return RecommendNone, "fewer than two top-level symbol groups; the file is already one unit"
	}
	return RecommendSplitCode, "large file with several top-level groups hit by concurrent PRs"
}

// Units proposes how many files the hotspot should become, honouring the
// don't-splinter rule: at most opts.MaxUnits, each at least
// opts.MinUnitLines lines, and never more units than symbol groups.
func Units(h Hotspot, opts Options) int {
	opts = opts.withDefaults()
	n := h.Lines / opts.MinUnitLines
	if n > opts.MaxUnits {
		n = opts.MaxUnits
	}
	if g := len(h.SymbolGroups); g > 0 && n > g {
		n = g
	}
	if n < 2 {
		n = 2
	}
	return n
}

// Actionable reports whether a hotspot warrants an issue.
func (h Hotspot) Actionable() bool {
	switch h.Recommendation {
	case RecommendSplitCode, RecommendSplitDoc, RecommendSplitSource:
		return true
	}
	return false
}

// Marker is the HTML comment that dedupes issues per file.
func Marker(path string) string {
	return "<!-- hive:hotspot file=" + strings.TrimSpace(path) + " -->"
}
