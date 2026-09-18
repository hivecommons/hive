// Package recommend turns a repository's open pull request queue into a short
// list of things a maintainer can actually do next.
//
// A hive that reviews pull requests produces per-PR comments. On a repository
// with a handful of open PRs that is enough. On one with forty, it is not: the
// comments are spread across forty pages, nothing says which PRs are ready,
// and the maintainer still has to open every one to find out. Measured on a
// bluefin spoke, 71% of open PRs were cleanly mergeable and 21% were
// conflicting -- facts the hive already knew on every cycle and never told
// anyone.
//
// This package answers one question: given everything the hive knows, what
// should a human do next? It sorts the queue into buckets that map to
// distinct actions -- merge it, rebase it, fix its CI, decide it, close it --
// and renders them as a single Markdown body meant to live in one pinned
// issue that is rewritten in place rather than posted again and again.
package recommend

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Bucket is a recommended action, not a status. Each one names something a
// maintainer can do, because a digest sorted by GitHub's vocabulary
// ("unstable", "blocked") tells a reader what GitHub thinks rather than what
// is being asked of them.
type Bucket string

const (
	// BucketReady is the point of the whole digest: cleanly mergeable, CI
	// green, not a draft. Nothing is stopping these but a decision.
	BucketReady Bucket = "ready"
	// BucketConflicts needs a rebase. It is usually the second-largest
	// bucket and the cheapest to shrink, because the answer is often
	// "close it" rather than "rebase it".
	BucketConflicts Bucket = "conflicts"
	// BucketRedCI is blocked on a failing required check.
	BucketRedCI Bucket = "red-ci"
	// BucketDecision is waiting on a judgment only a human can make --
	// carrying the hive's human-decision label.
	BucketDecision Bucket = "decision"
	// BucketStaleDraft is a draft that has not moved in a long time. Drafts
	// are not review work, but they are queue weight, and a maintainer
	// deciding to close ten of them gets the count down immediately.
	BucketStaleDraft Bucket = "stale-draft"
)

// bucketOrder is the order sections are rendered in: what can be finished
// first, then what is cheap to clear, then what needs real work.
var bucketOrder = []Bucket{BucketReady, BucketConflicts, BucketRedCI, BucketDecision, BucketStaleDraft}

var bucketTitle = map[Bucket]string{
	BucketReady:      "✅ Ready to merge",
	BucketConflicts:  "🔀 Needs a rebase",
	BucketRedCI:      "🔴 Failing CI",
	BucketDecision:   "🧑 Waiting on a human decision",
	BucketStaleDraft: "💤 Stale drafts",
}

// bucketLead is the one line under each heading that says what to DO. A
// section header alone leaves the reader to infer the action.
var bucketLead = map[Bucket]string{
	BucketReady:      "Cleanly mergeable with green CI. Nothing is blocking these but a decision.",
	BucketConflicts:  "Conflicts with the base branch. Each needs a rebase — or a decision to close it.",
	BucketRedCI:      "A required check is failing. These cannot merge until it is fixed.",
	BucketDecision:   "The hive reviewed these and found something only a human can settle.",
	BucketStaleDraft: "Drafts with no recent movement. Closing the abandoned ones shrinks the queue immediately.",
}

// Item is one pull request as it appears in the digest.
type Item struct {
	Number    int
	Title     string
	URL       string
	Author    string
	AgeDays   int
	Class     github.ReviewClass
	FromFork  bool
	Note      string
	CreatedAt time.Time
}

// Report is the rendered-ready view of one repository's queue.
type Report struct {
	Repo        string
	GeneratedAt time.Time
	// TotalOpen counts every open PR considered, including ones that landed
	// in no bucket. The digest must never imply the queue is only as big as
	// the part it chose to show.
	TotalOpen int
	Buckets   map[Bucket][]Item
	// Unclassified counts PRs whose state the hive could not determine.
	// Reported openly: a digest that silently drops what it did not
	// understand invites a maintainer to trust a number that is wrong.
	Unclassified int
	// MaxPerBucket is stamped by Build so rendering needs no second options
	// argument.
	MaxPerBucket int
	// HumanDecisionLabel is carried through so the decision section can link
	// the exact label query.
	HumanDecisionLabel string
}

// Options tunes the digest for one repository.
type Options struct {
	// Now is the clock, injectable for tests.
	Now time.Time
	// HumanDecisionLabel is the label the hive applies when a review needs a
	// human. It is configuration rather than a constant because it differs
	// per hive -- bluefin uses "3-human-queue", which means nothing
	// elsewhere.
	HumanDecisionLabel string
	// StaleDraftDays is how long a draft sits before it is worth listing.
	// Zero means DefaultStaleDraftDays.
	StaleDraftDays int
	// MaxPerBucket caps how many PRs each section lists. Zero means
	// DefaultMaxPerBucket. A digest that lists all forty conflicting PRs is
	// just the queue again, which is the thing the reader was trying to
	// escape.
	MaxPerBucket int
}

const (
	// DefaultStaleDraftDays is deliberately generous. A draft is someone's
	// work in progress, and listing it after a week would be nagging.
	DefaultStaleDraftDays = 30
	// DefaultMaxPerBucket keeps each section skimmable.
	DefaultMaxPerBucket = 10
)

func (o Options) staleDraftDays() int {
	if o.StaleDraftDays <= 0 {
		return DefaultStaleDraftDays
	}
	return o.StaleDraftDays
}

func (o Options) maxPerBucket() int {
	if o.MaxPerBucket <= 0 {
		return DefaultMaxPerBucket
	}
	return o.MaxPerBucket
}

func (o Options) now() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now
}

// Build sorts an open-PR list into recommended actions.
//
// Every PR lands in at most one bucket, and the order of the checks is the
// order of the questions a maintainer would ask: is a human already needed,
// is it a draft, is it conflicting, is it red, is it ready. A PR that is both
// conflicting and red is reported as conflicting, because the rebase has to
// happen first and telling them to fix CI on a branch that will not merge is
// advice that wastes their time.
func Build(repo string, prs []github.PullRequest, opts Options) Report {
	now := opts.now()
	rep := Report{
		Repo:               repo,
		GeneratedAt:        now,
		Buckets:            map[Bucket][]Item{},
		MaxPerBucket:       opts.maxPerBucket(),
		HumanDecisionLabel: opts.HumanDecisionLabel,
	}

	for _, pr := range prs {
		if pr.Number <= 0 {
			continue
		}
		rep.TotalOpen++
		item := newItem(pr, now)

		switch {
		case hasLabel(pr.Labels, opts.HumanDecisionLabel):
			rep.add(BucketDecision, item)
		case pr.Draft:
			if item.AgeDays >= opts.staleDraftDays() {
				rep.add(BucketStaleDraft, item)
			}
			// A fresh draft is active work, not queue debt. Counted in
			// TotalOpen, listed nowhere.
		case isConflicting(pr):
			item.Note = rebaseNote(pr)
			rep.add(BucketConflicts, item)
		case pr.HasFailingRequiredCheck():
			item.Note = failingNote(pr)
			rep.add(BucketRedCI, item)
		case isReady(pr):
			rep.add(BucketReady, item)
		default:
			rep.Unclassified++
		}
	}

	for b := range rep.Buckets {
		sortItems(rep.Buckets[b])
	}
	return rep
}

func (r *Report) add(b Bucket, item Item) {
	r.Buckets[b] = append(r.Buckets[b], item)
}

func newItem(pr github.PullRequest, now time.Time) Item {
	age := 0
	if !pr.CreatedAt.IsZero() {
		age = int(now.Sub(pr.CreatedAt).Hours() / 24)
		if age < 0 {
			age = 0
		}
	}
	return Item{
		Number:    pr.Number,
		Title:     pr.Title,
		URL:       pr.URL,
		Author:    pr.Author,
		AgeDays:   age,
		Class:     pr.ReviewClass,
		FromFork:  pr.FromFork,
		CreatedAt: pr.CreatedAt,
	}
}

// isReady is deliberately strict. "unstable" counts as mergeable for the
// merge gate because non-required checks are ignored by policy, but this
// digest is a recommendation to a human: putting a PR with red checks of any
// kind under "ready to merge" is how a digest loses a reader's trust the
// first time they click one.
func isReady(pr github.PullRequest) bool {
	if pr.Draft || pr.Mergeable != github.MergeableYes {
		return false
	}
	if pr.CIStatus == "failure" || pr.CIStatus == "pending" {
		return false
	}
	return pr.MergeableState == "clean" || pr.MergeableState == "has_hooks"
}

func isConflicting(pr github.PullRequest) bool {
	return pr.MergeableState == "dirty" || (pr.Mergeable == github.MergeableNo && pr.MergeableState != "blocked")
}

func rebaseNote(pr github.PullRequest) string {
	base := pr.BaseRef
	if base == "" {
		base = "the base branch"
	}
	if pr.FromFork {
		// Saying "needs a rebase" about a fork PR implies the hive could do
		// it. It cannot push to a fork, so the action belongs to the author.
		return fmt.Sprintf("conflicts with %s — only the author can rebase (fork)", base)
	}
	return fmt.Sprintf("conflicts with %s", base)
}

func failingNote(pr github.PullRequest) string {
	checks := append([]string(nil), pr.FailingChecks...)
	if len(checks) == 0 {
		return "a required check failed"
	}
	sort.Strings(checks)
	if len(checks) > 3 {
		return fmt.Sprintf("failing: %s (+%d more)", strings.Join(checks[:3], ", "), len(checks)-3)
	}
	return "failing: " + strings.Join(checks, ", ")
}

func hasLabel(labels []string, want string) bool {
	if strings.TrimSpace(want) == "" {
		return false
	}
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// sortItems ranks by triage class first, then oldest first. Class before age
// because a two-week-old bug fix should outrank a two-month-old docs tweak,
// and age within class because the oldest PR in a class is the one whose
// author has been waiting longest.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := classRank(items[i].Class), classRank(items[j].Class)
		if ri != rj {
			return ri < rj
		}
		if items[i].AgeDays != items[j].AgeDays {
			return items[i].AgeDays > items[j].AgeDays
		}
		return items[i].Number < items[j].Number
	})
}

func classRank(c github.ReviewClass) int {
	switch c {
	case github.ReviewClassFix:
		return 0
	case github.ReviewClassTests:
		return 1
	case github.ReviewClassRefactorDocs:
		return 2
	default:
		return 3
	}
}
