package hotspot

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueshape"
)

// Agent is the agent lane a hotspot issue is filed for: the title's
// `[ci-maintainer]` tag and the agent/ci-maintainer label name the same
// agent, the one that works the split.
const Agent = "ci-maintainer"

// Labels applied to every modularize issue. Both exist on hivecommons/hive;
// the writer's label-ensure step creates them elsewhere.
var Labels = []string{config.AgentLaneLabel(Agent), "kind/cleanup"}

// Title is the issue title for a hotspot. It starts with the `[ci-maintainer]`
// tag every hive-filed issue title carries, matching its agent/ci-maintainer
// label (the governor's hotspot advisor files it on that lane's behalf).
func Title(h Hotspot, units int) string {
	var title string
	switch h.Recommendation {
	case RecommendSplitSource:
		title = fmt.Sprintf("refactor: %s is a merge hotspot — split its source %s into %d units", h.File, strings.TrimSuffix(h.GeneratedFrom, "/"), units)
	case RecommendSplitDoc:
		title = fmt.Sprintf("refactor: %s is a merge hotspot — split into %d sections", h.File, units)
	default:
		title = fmt.Sprintf("refactor: %s is a merge hotspot — split into %d units", h.File, units)
	}
	return issueshape.AgentIssueTitle(Agent, title)
}

// Body renders the issue body: plain-language summary (≤ 300 words),
// evidence table, the symbol groups, a concrete split proposal and the
// don't-splinter rule. The dedupe Marker is the first line.
func Body(h Hotspot, opts Options, window time.Duration) string {
	opts = opts.withDefaults()
	units := Units(h, opts)
	days := int(window.Hours() / 24)
	if days <= 0 {
		days = int(DefaultWindow.Hours() / 24)
	}
	var b strings.Builder
	b.WriteString(Marker(h.File))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "`%s` keeps being touched by pull requests that are open at the same time. In the last %d days %d distinct PRs changed it, %d pairs of them were open concurrently, and %d merge conflicts or rebase failures were blamed on it. Every concurrent pair is a rebase someone (or the merge train) has to redo, and a %d-line file makes each of those rebases slower and riskier.\n\n",
		h.File, days, h.PRs, h.Overlaps, h.Conflicts, h.Lines)

	switch h.Recommendation {
	case RecommendSplitSource:
		fmt.Fprintf(&b, "This file is **generated** — do not edit it by hand. The collisions come from its source under `%s`; split *there* so that unrelated changes stop regenerating the same output hunks, then regenerate once per PR.\n\n", h.GeneratedFrom)
	case RecommendSplitDoc:
		b.WriteString("This is a document, so the split is by **section**: each top-level `##` area becomes its own file under a directory named after this one, with a short index page that links them (the way `docs/api-reference.md` became `docs/api-reference/<area>.md`). Keep every heading anchor reachable from the index.\n\n")
	default:
		fmt.Fprintf(&b, "Proposed fix: a **mechanical split** into %d files in the same package. No behaviour change, exported API unchanged, tests move with the code they cover. Land it when no other PR is open against this file (check the merge-train overlap first) so the split itself does not become one more collision.\n\n", units)
	}

	b.WriteString("## Evidence\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n| window | %d days |\n| distinct PRs | %d |\n| concurrent PR pairs | %d |\n| conflicts attributed | %d |\n| lines | %d |\n| kind | %s |\n| score | %.1f |\n\n", days, h.PRs, h.Overlaps, h.Conflicts, h.Lines, h.Kind, h.Score)
	if len(h.PRNumbers) > 0 {
		b.WriteString("PRs in window: ")
		for i, n := range h.PRNumbers {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "#%d", n)
		}
		b.WriteString("\n\n")
	}

	if len(h.SymbolGroups) > 0 {
		if h.Kind == KindDoc {
			b.WriteString("## Sections\n\n")
		} else {
			b.WriteString("## Top-level units\n\n")
		}
		for _, g := range h.SymbolGroups {
			fmt.Fprintf(&b, "- `%s`\n", g)
		}
		b.WriteString("\n")
	}

	if h.Recommendation != RecommendSplitSource {
		b.WriteString("## Split proposal\n\n")
		for i, bucket := range proposeBuckets(h, units) {
			fmt.Fprintf(&b, "%d. `%s` — %s\n", i+1, bucket.file, bucket.desc)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Don't splinter\n\n")
	fmt.Fprintf(&b, "- At most %d resulting files, each at least %d lines; fewer is better.\n", opts.MaxUnits, opts.MinUnitLines)
	b.WriteString("- No behaviour change; the package, its exported API and every import path stay the same.\n")
	b.WriteString("- Tests move with the code they cover; no new test helpers.\n")
	b.WriteString("- One PR; land it when the file has no open PRs against it.\n")
	b.WriteString("\n_Filed by the hive's merge-hotspot advisor (`GET /api/hotspots`). It will not file another issue for this file while this one is open._\n")
	return b.String()
}

type bucket struct {
	file string
	desc string
}

// proposeBuckets spreads the symbol groups across `units` files named after
// the original, grouped by common name prefix where that helps.
func proposeBuckets(h Hotspot, units int) []bucket {
	ext := path.Ext(h.File)
	stem := strings.TrimSuffix(h.File, ext)
	stem = strings.TrimSuffix(stem, "_test")
	groups := append([]string(nil), h.SymbolGroups...)
	sort.Strings(groups)
	if h.Kind == KindDoc {
		dir := stem + "/"
		out := []bucket{{file: dir + "README.md", desc: "index linking every section"}}
		for _, g := range groups {
			out = append(out, bucket{file: dir + slug(g) + ".md", desc: "section “" + g + "”"})
			if len(out) >= units+1 {
				break
			}
		}
		if len(groups) > len(out)-1 {
			out[len(out)-1].desc += fmt.Sprintf(" and the %d remaining sections", len(groups)-(len(out)-1))
		}
		return out
	}
	out := make([]bucket, units)
	out[0] = bucket{file: h.File, desc: "core types and the entry points other files call"}
	for i := 1; i < units; i++ {
		out[i] = bucket{file: fmt.Sprintf("%s_%s%s", stem, partName(i, groups, units), ext)}
	}
	// Round-robin the groups so every bucket names what moves into it.
	perBucket := make([][]string, units)
	for i, g := range groups {
		perBucket[i%units] = append(perBucket[i%units], g)
	}
	for i := range out {
		if len(perBucket[i]) == 0 {
			if out[i].desc == "" {
				out[i].desc = "remaining helpers"
			}
			continue
		}
		var names []string
		for j, g := range perBucket[i] {
			if j >= 4 {
				names = append(names, fmt.Sprintf("… %d more", len(perBucket[i])-j))
				break
			}
			names = append(names, "`"+g+"`")
		}
		if out[i].desc != "" {
			out[i].desc += "; keeps "
		} else {
			out[i].desc = "moves "
		}
		out[i].desc += strings.Join(names, ", ")
	}
	return out
}

// partName picks a file suffix for bucket i from the first group it will
// hold, falling back to partN.
func partName(i int, groups []string, units int) string {
	if i < len(groups) {
		if s := slug(groups[i]); s != "" {
			return s
		}
	}
	return fmt.Sprintf("part%d", i+1)
}

func slug(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('_')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if len(out) > 24 {
		out = strings.Trim(out[:24], "_")
	}
	return out
}

// Gate is the writeback decision the Filer consults before creating an
// issue. pkg/github.Client.WritebackAllowed(repo, item, kind, preview)
// satisfies it through a small adapter in cmd/hive.
type Gate interface {
	IssueFileAllowed(repo, file string) bool
}

// IssueCreator creates the issue. It is satisfied by an adapter around
// github.Client.CreateIssue so that this package never imports pkg/github.
type IssueCreator interface {
	CreateHotspotIssue(ctx context.Context, repo, title, body string, labels []string) (number int, url string, err error)
}

// IssueStater reports whether a previously filed issue is still open and,
// when closed, when it closed.
type IssueStater interface {
	HotspotIssueState(ctx context.Context, repo string, number int) (open bool, closedAt time.Time, err error)
}

// Filer turns actionable hotspots into issues with the dedupe, refile and
// open-issue cap rules applied.
type Filer struct {
	Store   *Store
	Gate    Gate
	Creator IssueCreator
	Stater  IssueStater
	Opts    Options
	Refile  time.Duration
	// MaxOpen caps the modularize issues that may be open for a repo at
	// once; no new one is filed while that many are open (0 = default).
	MaxOpen int
	// MaxPerPass caps the issues one File call may open (0 = default).
	MaxPerPass int
	Now        func() time.Time
	Logf       func(format string, args ...any)
}

// Outcome is what File decided for one hotspot.
type Outcome struct {
	File   string
	Action string // "filed", "open", "refile-wait", "capped", "gated", "skip", "error"
	Number int
	Err    error
}

// File walks hotspots (highest score first) and files at most one new
// issue per actionable file, at most MaxPerPass per call, and none while
// MaxOpen issues it filed earlier are still open. It is safe to call every
// eval cycle: existing issues are checked, closed ones wait Refile before a
// new one may be opened, and the next-ranked hotspot is filed when a slot
// frees.
func (f *Filer) File(ctx context.Context, repo string, hs []Hotspot) []Outcome {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	refile := f.Refile
	if refile <= 0 {
		refile = time.Duration(DefaultHotspotRefileDays) * 24 * time.Hour
	}
	maxOpen := f.MaxOpen
	if maxOpen <= 0 {
		maxOpen = DefaultHotspotMaxOpen
	}
	maxPerPass := f.MaxPerPass
	if maxPerPass <= 0 {
		maxPerPass = DefaultHotspotMaxPerPass
	}
	logf := f.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	filedThisPass := 0
	cappedLogged := false
	var out []Outcome
	for _, h := range hs {
		if !h.Actionable() {
			out = append(out, Outcome{File: h.File, Action: "skip"})
			continue
		}
		if rec, ok := f.Store.Issue(repo, h.File); ok {
			if rec.ClosedAt.IsZero() && f.Stater != nil {
				open, closedAt, err := f.Stater.HotspotIssueState(ctx, repo, rec.Number)
				if err != nil {
					out = append(out, Outcome{File: h.File, Action: "error", Number: rec.Number, Err: err})
					continue
				}
				if !open {
					if closedAt.IsZero() {
						closedAt = now()
					}
					f.Store.MarkIssueClosed(repo, h.File, closedAt)
					rec.ClosedAt = closedAt
				}
			}
			if rec.ClosedAt.IsZero() {
				out = append(out, Outcome{File: h.File, Action: "open", Number: rec.Number})
				continue
			}
			if now().Sub(rec.ClosedAt) < refile {
				out = append(out, Outcome{File: h.File, Action: "refile-wait", Number: rec.Number})
				continue
			}
		}
		if open := f.Store.OpenIssues(repo); open >= maxOpen || filedThisPass >= maxPerPass {
			if !cappedLogged {
				logf("hotspot: %s: not filing for %s — %d modularize issue(s) open (cap %d), %d filed this pass (cap %d)", repo, h.File, open, maxOpen, filedThisPass, maxPerPass)
				cappedLogged = true
			}
			out = append(out, Outcome{File: h.File, Action: "capped"})
			continue
		}
		if f.Gate != nil && !f.Gate.IssueFileAllowed(repo, h.File) {
			out = append(out, Outcome{File: h.File, Action: "gated"})
			continue
		}
		if f.Creator == nil {
			out = append(out, Outcome{File: h.File, Action: "skip"})
			continue
		}
		units := Units(h, f.Opts)
		num, url, err := f.Creator.CreateHotspotIssue(ctx, repo, Title(h, units), Body(h, f.Opts, f.Opts.withDefaults().Window), Labels)
		if err != nil {
			logf("hotspot: file issue for %s %s: %v", repo, h.File, err)
			out = append(out, Outcome{File: h.File, Action: "error", Err: err})
			continue
		}
		f.Store.RecordIssue(IssueRecord{Repo: repo, File: h.File, Number: num, URL: url, FiledAt: now()})
		filedThisPass++
		logf("hotspot: filed %s#%d for %s", repo, num, h.File)
		out = append(out, Outcome{File: h.File, Action: "filed", Number: num})
	}
	return out
}

// Defaults mirroring config.DefaultHotspot*.
const (
	DefaultHotspotRefileDays = 30
	// DefaultHotspotMaxOpen caps open modularize issues per repo so the
	// advisor cannot flood the work queue (hivecommons/hive: five filed in
	// one pass while three older ones still had PRs in flight).
	DefaultHotspotMaxOpen = 2
	// DefaultHotspotMaxPerPass caps new issues per File call.
	DefaultHotspotMaxPerPass = 1
)
