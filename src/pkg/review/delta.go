package review

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Delta re-review (hivecommons/hive: reviewer-cost analysis, Oct 13-27 2026).
//
// A re-review is a review of a PR the hive has already judged at an earlier
// head SHA. Across three hosted hives re-reviews were 28% of attributed
// reviewer spend, and 53% on the busiest one, because each one re-read the
// whole PR: the diff against the base, the touched files, their callers.
// Almost all of that was already read and judged at the previous head. The
// delta mode hands the reviewer its previous verdict and asks it to read the
// diff between the two heads instead, re-checking what it flagged before;
// a delta that is empty (a force-push with the same tree) or trivial skips
// the model call and re-stamps the previous verdict onto the new head.

// DefaultDeltaTrivialLines and DefaultDeltaMaxAge mirror the config defaults
// (config.DefaultReviewDeltaTrivialLines, config.DefaultReviewDeltaMaxAge) for
// callers that build DispatchOptions without a config.
const (
	DefaultDeltaTrivialLines = 20
	DefaultDeltaMaxAge       = 72 * time.Hour
)

// DeltaFile is one file in the compare between the reviewed head and the
// current head.
type DeltaFile struct {
	Path      string `json:"path"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
}

// DeltaDiff is the compare between a PR's previously reviewed head and its
// current head, fetched by the caller (pkg/github ReviewDeltaDiff) for a PR
// the planner may delta-review. The planner never calls GitHub itself.
type DeltaDiff struct {
	// PreviousHead is the base of the compare: the head the prior verdict
	// stands against. It must match the prior aggregate the planner found,
	// or the diff is ignored.
	PreviousHead string
	// Known is true when the compare succeeded and the file list is
	// complete. Unknown means no trivial skip: the reviewer still gets the
	// delta instruction, since that needs no file list.
	Known bool
	Files []DeltaFile
	// BaseSync is true when every commit between the two heads is a merge of
	// the base branch into the PR that GitHub made for the hive App
	// (update-branch for review.contributor_prs.base_sync,
	// auto_merge.require_current_base and the hold guard's mergeability upkeep;
	// the merges API for merge-forward), proven by pkg/github ReviewDeltaDiff
	// from the compare's commits (projectbluefin/fsdk-containers#482). When it is true
	// PRFiles and BaseFiles are complete lists; when it is false both are nil.
	BaseSync bool
	// PRFiles are the PR's own changed paths at the current head (the base
	// branch's merge base with the head, three-dot, like the PR's Files tab).
	PRFiles []string
	// BaseFiles are the paths the base branch changed between the reviewed
	// head's merge base and the base commit the sync merged in.
	BaseFiles []string
}

// ChangedLines is the delta size the trivial threshold is judged against.
func (d DeltaDiff) ChangedLines() int {
	n := 0
	for _, f := range d.Files {
		n += f.Additions + f.Deletions
	}
	return n
}

// Identical reports an empty known delta: the new head's tree matches the
// reviewed head's (a rebase or an amended message).
func (d DeltaDiff) Identical() bool {
	return d.Known && len(d.Files) == 0
}

// BaseSyncOnly reports a known base-sync delta that brought in nothing but
// the base's own changes, none of them in a file the PR changes:
//
//   - the delta's files are exactly the files the base changed (BaseFiles), so
//     no base change was dropped and nothing beyond the base was added; and
//   - no delta file is one of the PR's own files (PRFiles), so each delta file
//     now holds the base's content, and no PR file moved.
//
// Together these mean every file at the new head is either byte-for-byte the
// reviewed head's or the base's, which is what a clean merge of a base that
// touched none of the PR's files produces. GitHub's update-branch and merges
// API refuse a conflicting merge, so a hive base-sync never carries a conflict
// resolution, and a merge whose result differs from that (a hand-resolved or
// fabricated tree) fails one of the two checks unless all it did was take
// the base's version of a file both sides changed: the same result as a PR
// whose change already landed on the base, which is base code, reviewed and
// merged there.
func (d DeltaDiff) BaseSyncOnly() bool {
	if !d.Known || !d.BaseSync || len(d.Files) == 0 {
		return false
	}
	delta := map[string]bool{}
	for _, f := range d.Files {
		delta[strings.TrimSpace(f.Path)] = true
	}
	base := map[string]bool{}
	for _, p := range d.BaseFiles {
		base[strings.TrimSpace(p)] = true
	}
	if len(base) != len(delta) {
		return false
	}
	for p := range delta {
		if !base[p] {
			return false
		}
	}
	for _, p := range d.PRFiles {
		if delta[strings.TrimSpace(p)] {
			return false
		}
	}
	return true
}

// DeltaContext is what a delta re-review kick carries about the previous
// verdict. It goes into the prompt, so it holds only what the planner wrote
// or the reviewer itself reported before: never the PR author's text.
type DeltaContext struct {
	PreviousHead       string
	PreviousVerdict    Verdict
	PreviousRecordedAt time.Time
	// PreviousPerspectives is the per-perspective verdict at the previous
	// head, so a reviewer covering several perspectives knows which ones
	// withheld approval.
	PreviousPerspectives map[Perspective]Verdict
	// PreviousFindings are the findings recorded at the previous head, which
	// the reviewer must re-check at the new head.
	PreviousFindings []PerspectiveFinding
	// Files are the paths the delta touches when the compare is known;
	// empty when it is not, and the reviewer reads the diff itself.
	Files []string
	// ChangedLines is the known delta size, 0 when unknown.
	ChangedLines int
	DiffKnown    bool
	// BaseSync is set when the planner carries an approve across a hive
	// base-sync (DeltaDiff.BaseSyncOnly) rather than a trivial delta.
	BaseSync bool
	// BaseDiffEmpty is set when the PR itself has no diff against its base
	// (hivecommons/hive#15499); the delta is trivial whatever the compare
	// from the previous head shows.
	BaseDiffEmpty bool `json:"base_diff_empty,omitempty"`
}

// DeltaReviewInfo is recorded on an aggregate produced by a delta re-review,
// so the cost dashboard and the verdict reader can tell a delta from a full
// review and find the head it was judged against.
type DeltaReviewInfo struct {
	PreviousHead string `json:"previous_head"`
	// Trivial is true when no model call was made: the previous verdict was
	// re-stamped onto the new head because the delta was empty or under
	// review.delta_trivial_lines in files the previous verdict passed.
	Trivial bool `json:"trivial,omitempty"`
	// BaseSync is true when the re-stamp crossed a hive merge of the base
	// into the PR whose changes miss every PR file (projectbluefin/fsdk-containers#482),
	// at any delta size.
	BaseSync     bool   `json:"base_sync,omitempty"`
	ChangedLines int    `json:"changed_lines,omitempty"`
	Note         string `json:"note,omitempty"`
}

// DeltaRestamp is a verdict the planner carried from a previous head onto the
// current head without a review session. The caller records it in the
// verdict artifact (RecordDeltaRestamps); the planner itself only plans.
type DeltaRestamp struct {
	Repo         string
	Number       int
	HeadSHA      string
	PreviousHead string
	Aggregate    Aggregate
}

// deltaMode is the planner's decision for one PR head.
type deltaMode int

const (
	deltaModeFull deltaMode = iota
	deltaModeDelta
	deltaModeTrivial
)

// PriorAggregateFor returns the most recently recorded aggregate for this PR
// at a head OTHER than currentHead: the verdict a delta re-review is judged
// against. ok is false when the PR has never been reviewed at another head
// within the artifact's retention.
func (a Artifact) PriorAggregateFor(repo string, number int, currentHead string) (Aggregate, bool) {
	wantRepo := strings.TrimSpace(repo)
	var match Aggregate
	var ok bool
	for _, item := range a.Items {
		if item.Number != number || strings.TrimSpace(item.Repo) != wantRepo {
			continue
		}
		if strings.TrimSpace(item.HeadSHA) == "" || SameHeadSHA(item.HeadSHA, currentHead) {
			continue
		}
		if !ok || item.RecordedAt.After(match.RecordedAt) {
			match = item.Normalize()
			ok = true
		}
	}
	return match, ok
}

func (o DispatchOptions) effectiveDeltaTrivialLines() int {
	if o.DeltaTrivialLines < 0 {
		return 0
	}
	if o.DeltaTrivialLines == 0 {
		return DefaultDeltaTrivialLines
	}
	return o.DeltaTrivialLines
}

func (o DispatchOptions) effectiveDeltaMaxAge() time.Duration {
	if o.DeltaMaxAge <= 0 {
		return DefaultDeltaMaxAge
	}
	return o.DeltaMaxAge
}

// planDelta decides whether this head is reviewed in full, as a delta against
// the prior verdict, or re-stamped without a session. The rules:
//
//   - delta_rereview off, no prior verdict at another head, or a prior verdict
//     that is incomplete or reject: full review. A first review has nothing
//     to diff against; a reject judged the whole approach, not lines.
//   - a prior changes_requested with findings older than delta_max_age:
//     full review. The flagged code has had time to move on and the
//     reviewer's record of it is stale.
//   - otherwise delta. With a known compare that is empty, or under
//     delta_trivial_lines and touching no file the prior verdict flagged, the
//     prior verdict is re-stamped instead of asked again.
//   - a prior approve across a hive base-sync that brought in only base
//     changes outside the PR's files (DeltaDiff.BaseSyncOnly) is re-stamped
//     at any delta size (projectbluefin/fsdk-containers#482). The hive's own update-branch
//     used to drop an approved PR back to review-gate:no-review-bundle, and
//     then pay a model to "delta review" base code that was reviewed and
//     merged upstream. CI still re-runs on the merge head (the point of
//     auto_merge.require_current_base, hivecommons/hive#13634), so only the
//     review verdict is carried. Only approve: a changes_requested or
//     requires_human verdict keeps its delta review, so a base-sync never
//     settles what a reviewer or a person still has open. A negative
//     delta_trivial_lines, which opts out of re-stamps, opts out of this too.
func planDelta(pr PullRequest, artifact Artifact, opts DispatchOptions, now time.Time) (deltaMode, *DeltaContext, Aggregate) {
	if !opts.DeltaRereview {
		return deltaModeFull, nil, Aggregate{}
	}
	prior, ok := artifact.PriorAggregateFor(pr.Repo, pr.Number, pr.HeadSHA)
	if !ok {
		return deltaModeFull, nil, Aggregate{}
	}
	switch prior.Verdict {
	case VerdictApprove, VerdictChangesRequested, VerdictRequiresHuman:
	default:
		return deltaModeFull, nil, Aggregate{}
	}
	if prior.Verdict == VerdictChangesRequested && len(prior.Findings) > 0 && !prior.RecordedAt.IsZero() && now.Sub(prior.RecordedAt) > opts.effectiveDeltaMaxAge() {
		return deltaModeFull, nil, Aggregate{}
	}
	ctx := &DeltaContext{
		PreviousHead:         prior.HeadSHA,
		PreviousVerdict:      prior.Verdict,
		PreviousRecordedAt:   prior.RecordedAt,
		PreviousPerspectives: prior.Perspectives,
		PreviousFindings:     prior.Findings,
	}
	if pr.BaseDiffEmpty {
		// The PR has no diff against its base: the new head only absorbed
		// base commits (a merge-from-base shows them in the compare from
		// the previous head, so the delta looks non-trivial). Nothing new
		// to read; the prior verdict stands (hivecommons/hive#15499).
		ctx.DiffKnown = true
		ctx.BaseDiffEmpty = true
		return deltaModeTrivial, ctx, prior
	}
	diff := pr.DeltaDiff
	if diff == nil || !diff.Known || !SameHeadSHA(diff.PreviousHead, prior.HeadSHA) {
		return deltaModeDelta, ctx, prior
	}
	ctx.DiffKnown = true
	ctx.ChangedLines = diff.ChangedLines()
	for _, f := range diff.Files {
		ctx.Files = append(ctx.Files, f.Path)
	}
	sort.Strings(ctx.Files)
	if prior.Verdict == VerdictApprove && opts.DeltaTrivialLines >= 0 && diff.BaseSyncOnly() {
		ctx.BaseSync = true
		return deltaModeTrivial, ctx, prior
	}
	if deltaTrivial(*diff, prior, opts.effectiveDeltaTrivialLines()) {
		return deltaModeTrivial, ctx, prior
	}
	return deltaModeDelta, ctx, prior
}

// deltaTrivial reports whether the delta can carry the prior verdict without
// a review: an empty delta always can; a delta under threshold can when none
// of its files holds a prior finding, since a change in a file the reviewer
// flagged may be the fix (or not) and has to be read.
func deltaTrivial(diff DeltaDiff, prior Aggregate, threshold int) bool {
	if !diff.Known {
		return false
	}
	if diff.Identical() {
		return true
	}
	if threshold <= 0 || diff.ChangedLines() >= threshold {
		return false
	}
	flagged := map[string]bool{}
	for _, f := range prior.Findings {
		if p := strings.TrimSpace(f.Finding.File); p != "" {
			flagged[p] = true
		}
	}
	for _, f := range diff.Files {
		if flagged[strings.TrimSpace(f.Path)] {
			return false
		}
	}
	return true
}

// restampAggregate copies the prior verdict onto the new head. The verdict,
// perspectives, findings and confidence are the prior's; the head, body
// digest and timestamp are the new head's; Delta says why.
func restampAggregate(prior Aggregate, pr PullRequest, ctx DeltaContext, now time.Time) Aggregate {
	agg := prior
	agg.HeadSHA = pr.HeadSHA
	agg.BodyDigest = pr.BodyDigest
	agg.RecordedAt = now
	note := fmt.Sprintf("re-stamped from %s: delta touches %d file(s), %d line(s), none previously flagged", shortSHA(ctx.PreviousHead), len(ctx.Files), ctx.ChangedLines)
	switch {
	case ctx.BaseSync:
		note = fmt.Sprintf("re-stamped from %s: hive base-sync; the base's changes (%d file(s), %d line(s)) are disjoint from the PR's files", shortSHA(ctx.PreviousHead), len(ctx.Files), ctx.ChangedLines)
	case ctx.DiffKnown && len(ctx.Files) == 0:
		note = fmt.Sprintf("re-stamped from %s: identical tree", shortSHA(ctx.PreviousHead))
	}
	if ctx.BaseDiffEmpty {
		note = fmt.Sprintf("re-stamped from %s: PR has no diff against its base", shortSHA(ctx.PreviousHead))
	}
	agg.Delta = &DeltaReviewInfo{PreviousHead: ctx.PreviousHead, Trivial: true, BaseSync: ctx.BaseSync, ChangedLines: ctx.ChangedLines, Note: note}
	agg.Reasons = append(append([]string(nil), prior.Reasons...), "delta re-review: "+note)
	return agg
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// deltaSection is the prompt block for a delta re-review. It names the
// previous head and verdict, lists the previous findings for re-checking,
// and scopes the reading to the diff between the two heads.
func deltaSection(pr PullRequest, ctx *DeltaContext) string {
	if ctx == nil || strings.TrimSpace(ctx.PreviousHead) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nDELTA RE-REVIEW. ")
	fmt.Fprintf(&b, "You (the hive) already reviewed this PR at head %s", ctx.PreviousHead)
	if !ctx.PreviousRecordedAt.IsZero() {
		fmt.Fprintf(&b, " on %s", ctx.PreviousRecordedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	fmt.Fprintf(&b, "; the verdict was %s", ctx.PreviousVerdict)
	if len(ctx.PreviousPerspectives) > 0 {
		names := make([]string, 0, len(ctx.PreviousPerspectives))
		for p := range ctx.PreviousPerspectives {
			names = append(names, string(p))
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s=%s", n, ctx.PreviousPerspectives[Perspective(n)]))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	b.WriteString(".\n")
	b.WriteString("Review ONLY what changed since that head, not the whole PR again:\n")
	fmt.Fprintf(&b, "  gh api repos/%s/compare/%s...%s --jq '.files[] | \"\\(.status) \\(.filename) +\\(.additions) -\\(.deletions)\"'\n", pr.Repo, ctx.PreviousHead, pr.HeadSHA)
	fmt.Fprintf(&b, "  gh api repos/%s/compare/%s...%s -H 'Accept: application/vnd.github.diff'\n", pr.Repo, ctx.PreviousHead, pr.HeadSHA)
	if ctx.DiffKnown {
		fmt.Fprintf(&b, "The delta is %d changed line(s) across %d file(s)", ctx.ChangedLines, len(ctx.Files))
		if len(ctx.Files) > 0 {
			fmt.Fprintf(&b, ": %s", strings.Join(ctx.Files, ", "))
		}
		b.WriteString(".\n")
	}
	b.WriteString("Read the new code in those hunks and enough of their surroundings to judge them. Code outside the delta was judged at the previous head and stands unless the delta changes what it does.\n")
	if len(ctx.PreviousFindings) > 0 {
		b.WriteString("Re-check each finding from the previous review at the new head and say whether it is fixed, still present, or no longer applies:\n")
		for _, f := range ctx.PreviousFindings {
			loc := strings.TrimSpace(f.Finding.File)
			if loc != "" && f.Finding.Line > 0 {
				loc = fmt.Sprintf("%s:%d", loc, f.Finding.Line)
			}
			title := strings.TrimSpace(f.Finding.Title)
			if title == "" {
				title = strings.TrimSpace(f.Finding.Summary)
			}
			if loc == "" && title == "" {
				continue
			}
			fmt.Fprintf(&b, "  - [%s] %s %s\n", f.Perspective, loc, title)
		}
	} else {
		b.WriteString("The previous review recorded no findings, so there is nothing to re-check beyond the delta itself.\n")
	}
	b.WriteString("Your verdict is for the WHOLE PR at the new head: a previous finding that is still present keeps its verdict; a clean delta on a previously approved PR is approve. Record it with the usual head_sha above.\n")
	b.WriteString("Say fixed / still present / no longer applies in your summary. Do not list a still-present out-of-scope finding again under findings: it is already in the backlog, and a re-report (\"Previous … still present\") is discarded, not filed. Cite its backlog issue with `<!-- hive-refers-to: #N -->` if you need to point at it. Only in-scope findings that still bear on the verdict, and genuinely new findings in the delta, go under findings.\n")
	return b.String()
}

// RecordDeltaRestamps writes the planner's re-stamped verdicts into the
// durable artifact at path ("" for the default), so the next cycle, the merge
// gate and the dashboard see the new head as judged. An existing entry for the
// same head and body digest is replaced; nothing else in the artifact moves.
func RecordDeltaRestamps(path string, restamps []DeltaRestamp, now time.Time) (int, error) {
	if len(restamps) == 0 {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	target := path
	if target == "" {
		target = ReviewVerdictsPath
	}
	artifact, err := LoadArtifact(path)
	if err != nil {
		artifact = Artifact{}
	}
	written := 0
	for _, r := range restamps {
		agg := r.Aggregate
		if agg.RecordedAt.IsZero() {
			agg.RecordedAt = now
		}
		key := reviewKey(agg.Repo, agg.Number, agg.HeadSHA, agg.BodyDigest)
		replaced := false
		for i, item := range artifact.Items {
			if reviewKey(item.Repo, item.Number, item.HeadSHA, item.BodyDigest) == key {
				artifact.Items[i] = agg
				replaced = true
				break
			}
		}
		if !replaced {
			artifact.Items = append(artifact.Items, agg)
		}
		written++
	}
	artifact.GeneratedAt = now
	if err := WriteArtifact(target, artifact); err != nil {
		return 0, err
	}
	return written, nil
}
