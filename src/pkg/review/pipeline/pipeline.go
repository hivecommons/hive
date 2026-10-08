// Package pipeline derives where a pull request sits in the review pipeline
// (unreviewed → reviewing → changes requested → fixing → human hold →
// approved → merged/abandoned) from evidence the hive already records: the
// review verdict artifact, the review dispatch state, the review-links
// ledger, labels, the escalation ledger, the CI rollup and the merge state.
//
// Derive is pure: callers load the evidence and pass it in, so the same
// inputs always produce the same card and every rule is unit-testable
// (hivecommons/hive#11086).
package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// Stage is one column of the review pipeline. The zero value is
// StageUnreviewed.
type Stage int

const (
	StageUnreviewed Stage = iota
	StageReviewing
	StageChangesRequested
	StageFixing
	StageHumanHold
	StageApproved
	StageMerged
	StageAbandoned
)

var stageNames = [...]string{
	StageUnreviewed:       "unreviewed",
	StageReviewing:        "reviewing",
	StageChangesRequested: "changes_requested",
	StageFixing:           "fixing",
	StageHumanHold:        "human_hold",
	StageApproved:         "approved",
	StageMerged:           "merged",
	StageAbandoned:        "abandoned",
}

// Stages returns every stage in pipeline order.
func Stages() []Stage {
	out := make([]Stage, len(stageNames))
	for i := range stageNames {
		out[i] = Stage(i)
	}
	return out
}

func (s Stage) valid() bool { return s >= 0 && int(s) < len(stageNames) }

func (s Stage) String() string {
	if !s.valid() {
		return fmt.Sprintf("stage(%d)", int(s))
	}
	return stageNames[s]
}

// ParseStage reads a stage name case-insensitively.
func ParseStage(name string) (Stage, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	for i, n := range stageNames {
		if n == want {
			return Stage(i), nil
		}
	}
	return 0, fmt.Errorf("unknown review pipeline stage %q", name)
}

// MarshalText implements encoding.TextMarshaler, so Stage encodes as its
// name in JSON.
func (s Stage) MarshalText() ([]byte, error) {
	if !s.valid() {
		return nil, fmt.Errorf("invalid review pipeline stage %d", int(s))
	}
	return []byte(stageNames[s]), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Stage) UnmarshalText(text []byte) error {
	parsed, err := ParseStage(string(text))
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// Merge states accepted in MergeState.State. They mirror GitHub's PR state
// with "merged" split out of "closed".
const (
	MergeStateOpen   = "open"
	MergeStateClosed = "closed"
	MergeStateMerged = "merged"
)

// MergeState is the PR's lifecycle outside review.
type MergeState struct {
	State    string    `json:"state,omitempty"`
	MergedAt time.Time `json:"merged_at,omitzero"`
	ClosedAt time.Time `json:"closed_at,omitzero"`
}

// Inputs is everything Derive reads. Every field is optional; absent evidence
// simply does not move the PR forward.
type Inputs struct {
	// PR is the review-queue enumeration entry: identity, head, labels, held
	// flag and CI rollup (CIState).
	PR ghpkg.ReviewQueueEntry
	// Merge is the merge/close state. The review queue holds only open PRs,
	// so it is zero there.
	Merge MergeState
	// Verdicts are the verdict artifact entries for this PR, any head.
	// Entries for other PRs are ignored.
	Verdicts []review.Aggregate
	// Dispatch is the review dispatch state; entries for other PRs are ignored.
	Dispatch review.DispatchState
	// ReviewLink is the PR's review-links ledger entry, if any.
	ReviewLink *ghpkg.ReviewLink
	// Escalation is the PR's escalation ledger entry, if any.
	Escalation *escalation.Entry
	// FixCycleCap overrides the loop cap when the verdict does not carry one.
	// Zero means review.DefaultFixCycleCap.
	FixCycleCap int
	// BotThreads are the review-bot threads the hive has replied in, with how
	// many replies each has. Empty when the caller does not load them.
	BotThreads []BotThread
	// BotMaxAttempts is review_bots.max_attempts_per_thread. Zero means
	// config.DefaultReviewBotMaxAttempts.
	BotMaxAttempts int
}

// BotThread is one review-bot thread and the hive's reply count in it.
type BotThread struct {
	Reviewer string `json:"reviewer"`
	Thread   string `json:"thread"`
	Attempts int    `json:"attempts"`
}

// LoopWarning says why a PR is about to leave the automatic review/fix loop.
type LoopWarning struct {
	Reason   string `json:"reason"`
	Reviewer string `json:"reviewer,omitempty"`
	Thread   string `json:"thread,omitempty"`
}

// Reviewer is one perspective that reviewed (or is reviewing) the PR.
type Reviewer struct {
	Perspective review.Perspective `json:"perspective"`
	Model       string             `json:"model,omitempty"`
}

// SeverityCounts are the in-scope findings on the current head, bucketed on
// the P0–P3 scale review_bots.min_priority uses: critical → P0, high → P1,
// medium → P2, low/info/unrated → P3.
type SeverityCounts struct {
	P0 int `json:"p0"`
	P1 int `json:"p1"`
	P2 int `json:"p2"`
	P3 int `json:"p3"`
}

// Next action kinds.
const (
	ActionReview = "review"
	ActionWait   = "wait"
	ActionFix    = "fix"
	ActionHuman  = "human"
	ActionFixCI  = "fix_ci"
	ActionWaitCI = "wait_ci"
	ActionMerge  = "merge"
	ActionNone   = "none"
	ActionReopen = "reopen"
)

// NextAction is the single step that moves the PR to its next stage.
type NextAction struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// Card is one PR on the review pipeline board.
type Card struct {
	Repo         string         `json:"repo"`
	Number       int            `json:"number"`
	Title        string         `json:"title,omitempty"`
	Author       string         `json:"author,omitempty"`
	URL          string         `json:"url,omitempty"`
	HeadSHA      string         `json:"head_sha,omitempty"`
	HiveAuthored bool           `json:"hive_authored"`
	CIState      string         `json:"ci_state,omitempty"`
	Stage        Stage          `json:"stage"`
	Since        time.Time      `json:"since,omitzero"`
	Reviewers    []Reviewer     `json:"reviewers"`
	Severity     SeverityCounts `json:"severity"`
	LoopCount    int            `json:"loop_count"`
	LoopCap      int            `json:"loop_cap"`
	NextAction   NextAction     `json:"next_action"`
	Reasons      []string       `json:"reasons"`
	// LoopWarning is set while the PR is one cycle from the loop cap, or a
	// review-bot thread is at its attempt cap, and no human has the PR yet.
	// The dashboard offers "Send to human" while it is set.
	LoopWarning *LoopWarning `json:"loop_warning,omitempty"`
}

// Review-link states, as recorded by the review relay.
const (
	linkStateApproved         = "approved"
	linkStateChangesRequested = "changes_requested"
)

// approvalLabels mark a PR a maintainer or the hive has already cleared.
var approvalLabels = []string{ghpkg.AutoMergeQueuedLabel, "approved", escalation.ReviewerPassedLabel}

// Derive places the PR in exactly one stage. Rules, first match wins:
//
//  1. merged — merge state is merged or MergedAt is set.
//  2. abandoned — merge state is closed or ClosedAt is set.
//  3. human_hold — a hold or needs-human label, the queue's held flag, a
//     current-head verdict that requires a human or rejects, a dispatch-state
//     human hold for the head, or an escalated escalation entry.
//  4. approved — a merge-eligible approve verdict on the head, an approval
//     label (lgtm, approved, reviewer-passed), the escalation ledger's
//     reviewer-passed SHA equal to the head, or an approving hive review on
//     the head in the review-links ledger.
//  5. fixing — a pending fix dispatched for the head.
//  6. changes_requested — a changes_requested verdict (or, with no verdict, a
//     changes-requested hive review) on the head.
//  7. reviewing — a review perspective dispatched for the head and not yet
//     aggregated.
//  8. unreviewed — otherwise.
//
// A PR that would be fixing or changes_requested with its loop counter at or
// over the cap is moved to human_hold: another automatic cycle is not coming.
func Derive(in Inputs) Card {
	pr := in.PR
	card := Card{
		Repo:         pr.Repo,
		Number:       pr.Number,
		Title:        pr.Title,
		Author:       pr.Author,
		URL:          pr.URL,
		HeadSHA:      pr.HeadSHA,
		HiveAuthored: pr.HiveAuthored,
		CIState:      pr.CIState,
		Reasons:      []string{},
	}

	current, stale := headVerdict(in.Verdicts, pr)
	pendingReviews := pendingReviewsFor(in.Dispatch, pr)
	pendingFix := pendingFixFor(in.Dispatch, pr)
	link := headLink(in.ReviewLink, pr.HeadSHA)

	card.Reviewers = reviewers(current, pendingReviews)
	if current != nil {
		card.Severity = severity(current.Findings)
	}
	card.LoopCount, card.LoopCap = loop(in, current, pendingFix)

	reviewURL := pr.URL
	if in.ReviewLink != nil && strings.TrimSpace(in.ReviewLink.URL) != "" {
		reviewURL = in.ReviewLink.URL
	}

	switch {
	case strings.EqualFold(in.Merge.State, MergeStateMerged) || !in.Merge.MergedAt.IsZero():
		card.Stage = StageMerged
		card.Since = in.Merge.MergedAt
		card.Reasons = append(card.Reasons, "pull request merged")
		card.NextAction = NextAction{Kind: ActionNone, Label: "Nothing to do", URL: pr.URL}
		return card
	case strings.EqualFold(in.Merge.State, MergeStateClosed) || !in.Merge.ClosedAt.IsZero():
		card.Stage = StageAbandoned
		card.Since = in.Merge.ClosedAt
		card.Reasons = append(card.Reasons, "pull request closed without merging")
		card.NextAction = NextAction{Kind: ActionReopen, Label: "Reopen if still wanted", URL: pr.URL}
		return card
	}

	if reasons, since := holdReasons(in, current, humanHoldFor(in.Dispatch, pr)); len(reasons) > 0 {
		card.Stage = StageHumanHold
		card.Since = since
		card.Reasons = append(card.Reasons, reasons...)
		card.NextAction = NextAction{Kind: ActionHuman, Label: "Human decision needed", URL: pr.URL}
		return card
	}

	if reasons, since := approvalReasons(in, current, link); len(reasons) > 0 {
		card.Stage = StageApproved
		card.Since = since
		card.Reasons = append(card.Reasons, reasons...)
		card.NextAction = approvedAction(pr)
		return card
	}

	switch {
	case pendingFix != nil:
		card.Stage = StageFixing
		card.Since = pendingFix.Dispatched
		card.Reasons = append(card.Reasons, fmt.Sprintf("fix dispatched to %s (attempt %d)", nonEmpty(pendingFix.Agent, "an agent"), pendingFix.Attempts))
		card.NextAction = NextAction{Kind: ActionWait, Label: "Wait for the fix push", URL: pr.URL}
	case current != nil && current.Verdict == review.VerdictChangesRequested:
		card.Stage = StageChangesRequested
		card.Since = current.RecordedAt
		card.Reasons = append(card.Reasons, "review verdict on the current head requests changes")
		card.Reasons = append(card.Reasons, current.Reasons...)
		card.NextAction = NextAction{Kind: ActionFix, Label: "Dispatch a fix", URL: reviewURL}
	case current == nil && link != nil && strings.EqualFold(link.State, linkStateChangesRequested):
		card.Stage = StageChangesRequested
		card.Since = link.At
		card.Reasons = append(card.Reasons, "hive review on the current head requests changes")
		card.NextAction = NextAction{Kind: ActionFix, Label: "Dispatch a fix", URL: reviewURL}
	case len(pendingReviews) > 0:
		card.Stage = StageReviewing
		card.Since = earliestDispatch(pendingReviews)
		card.Reasons = append(card.Reasons, fmt.Sprintf("%d review perspective(s) dispatched for the current head", len(pendingReviews)))
		card.NextAction = NextAction{Kind: ActionWait, Label: "Wait for reviewers", URL: pr.URL}
	default:
		card.Stage = StageUnreviewed
		card.Since = pr.CreatedAt
		switch {
		case current != nil:
			card.Reasons = append(card.Reasons, fmt.Sprintf("review verdict %q on the current head is not actionable", current.Verdict))
		case stale != nil:
			card.Reasons = append(card.Reasons, "head moved since the last review verdict")
		default:
			card.Reasons = append(card.Reasons, "no review verdict for the current head")
		}
		card.NextAction = NextAction{Kind: ActionReview, Label: "Dispatch review", URL: pr.URL}
	}

	if (card.Stage == StageFixing || card.Stage == StageChangesRequested) && card.LoopCount >= card.LoopCap {
		card.Reasons = append([]string{fmt.Sprintf("fix-cycle cap reached (%d/%d) while %s", card.LoopCount, card.LoopCap, card.Stage)}, card.Reasons...)
		card.Stage = StageHumanHold
		card.NextAction = NextAction{Kind: ActionHuman, Label: "Human decision needed", URL: pr.URL}
	}
	card.LoopWarning = loopWarning(in, card)
	return card
}

// loopWarning reports the loop-safety state of an open PR that no human has
// taken yet: the fix-cycle counter at cap-1 or over, or a review-bot thread at
// review_bots.max_attempts_per_thread.
func loopWarning(in Inputs, card Card) *LoopWarning {
	if card.Stage == StageMerged || card.Stage == StageAbandoned {
		return nil
	}
	for _, l := range in.PR.Labels {
		if strings.EqualFold(l, escalation.NeedsHumanLabel) {
			return nil
		}
	}
	maxAttempts := in.BotMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = config.DefaultReviewBotMaxAttempts
	}
	for _, t := range in.BotThreads {
		if t.Attempts >= maxAttempts {
			return &LoopWarning{
				Reason:   fmt.Sprintf("review-bot thread reached its attempt cap (%d/%d)", t.Attempts, maxAttempts),
				Reviewer: t.Reviewer,
				Thread:   t.Thread,
			}
		}
	}
	if card.LoopCap > 0 && card.LoopCount >= card.LoopCap-1 {
		w := &LoopWarning{Reason: fmt.Sprintf("fix cycles at %d of %d", card.LoopCount, card.LoopCap)}
		names := make([]string, 0, len(card.Reviewers))
		for _, r := range card.Reviewers {
			names = append(names, string(r.Perspective))
		}
		w.Reviewer = strings.Join(names, ", ")
		return w
	}
	return nil
}

func holdReasons(in Inputs, current *review.Aggregate, hold *review.HumanReviewHold) ([]string, time.Time) {
	var reasons []string
	var since time.Time
	for _, l := range in.PR.Labels {
		if strings.EqualFold(l, escalation.NeedsHumanLabel) || isHoldLabel(l) {
			reasons = append(reasons, fmt.Sprintf("label %q", l))
		}
	}
	if in.PR.Held && len(reasons) == 0 {
		reasons = append(reasons, "held in the review queue")
	}
	if current != nil {
		switch {
		case current.Verdict == review.VerdictReject:
			reasons = append(reasons, "review verdict rejects the current head")
			since = current.RecordedAt
		case current.RequiresHuman || current.Verdict == review.VerdictRequiresHuman:
			reasons = append(reasons, "review verdict requires a human")
			since = current.RecordedAt
		}
	}
	if hold != nil {
		reasons = append(reasons, "review dispatch parked for a human: "+nonEmpty(hold.Reason, "no reason recorded"))
		if since.IsZero() {
			since = hold.UpdatedAt
		}
	}
	if e := in.Escalation; e != nil && e.Escalated {
		reasons = append(reasons, "escalated after repeated CI failures")
		if since.IsZero() {
			since = e.LabelAppliedAt
		}
	}
	return reasons, since
}

func approvalReasons(in Inputs, current *review.Aggregate, link *ghpkg.ReviewLink) ([]string, time.Time) {
	var reasons []string
	var since time.Time
	if current != nil && current.Verdict == review.VerdictApprove && current.MergeEligible {
		reasons = append(reasons, "merge-eligible approve verdict on the current head")
		since = current.RecordedAt
	}
	for _, l := range in.PR.Labels {
		for _, want := range approvalLabels {
			if strings.EqualFold(l, want) {
				reasons = append(reasons, fmt.Sprintf("label %q", l))
			}
		}
	}
	if e := in.Escalation; e != nil && e.ReviewerPassedSHA != "" && e.ReviewerPassedSHA == in.PR.HeadSHA {
		reasons = append(reasons, "reviewer passed the current head")
		if since.IsZero() {
			since = e.ReviewerPassedAt
		}
	}
	if link != nil && strings.EqualFold(link.State, linkStateApproved) {
		reasons = append(reasons, "hive review approved the current head")
		if since.IsZero() {
			since = link.At
		}
	}
	return reasons, since
}

func approvedAction(pr ghpkg.ReviewQueueEntry) NextAction {
	switch pr.CIState {
	case ghpkg.ReviewQueueCIGreen:
		return NextAction{Kind: ActionMerge, Label: "Merge", URL: pr.URL}
	case ghpkg.ReviewQueueCIRed:
		return NextAction{Kind: ActionFixCI, Label: "Fix failing CI", URL: pr.URL}
	default:
		return NextAction{Kind: ActionWaitCI, Label: "Wait for CI", URL: pr.URL}
	}
}

func isHoldLabel(l string) bool {
	for _, h := range ghpkg.HoldLabels {
		if strings.EqualFold(l, h) {
			return true
		}
	}
	return false
}

// SameRepo matches repository names in either spelling: the review queue
// carries full "owner/repo" names while some ledgers record bare ones.
func SameRepo(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	if strings.Contains(a, "/") == strings.Contains(b, "/") {
		return false
	}
	return strings.EqualFold(bare(a), bare(b))
}

func bare(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// sameHead treats an unknown head on either side as matching: an entry
// recorded without a SHA, or a PR whose head is unknown, cannot be told apart.
func sameHead(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a == "" || b == "" || a == b
}

func samePR(repo string, number int, pr ghpkg.ReviewQueueEntry) bool {
	return number == pr.Number && SameRepo(repo, pr.Repo)
}

// headVerdict returns the most recent verdict on the PR's head, or (when
// there is none) the most recent verdict on an older head.
func headVerdict(items []review.Aggregate, pr ghpkg.ReviewQueueEntry) (current, stale *review.Aggregate) {
	for i := range items {
		a := &items[i]
		if !samePR(a.Repo, a.Number, pr) {
			continue
		}
		if sameHead(a.HeadSHA, pr.HeadSHA) {
			if current == nil || a.RecordedAt.After(current.RecordedAt) {
				current = a
			}
		} else if stale == nil || a.RecordedAt.After(stale.RecordedAt) {
			stale = a
		}
	}
	if current != nil {
		stale = nil
	}
	return current, stale
}

func pendingReviewsFor(state review.DispatchState, pr ghpkg.ReviewQueueEntry) []review.PendingReview {
	var out []review.PendingReview
	for _, p := range state.Pending {
		if samePR(p.Repo, p.Number, pr) && sameHead(p.HeadSHA, pr.HeadSHA) {
			out = append(out, p)
		}
	}
	return out
}

func pendingFixFor(state review.DispatchState, pr ghpkg.ReviewQueueEntry) *review.PendingFix {
	for i := range state.Fixes {
		f := &state.Fixes[i]
		if samePR(f.Repo, f.Number, pr) && sameHead(f.HeadSHA, pr.HeadSHA) {
			return f
		}
	}
	return nil
}

func humanHoldFor(state review.DispatchState, pr ghpkg.ReviewQueueEntry) *review.HumanReviewHold {
	for i := range state.Human {
		h := &state.Human[i]
		if samePR(h.Repo, h.Number, pr) && sameHead(h.HeadSHA, pr.HeadSHA) {
			return h
		}
	}
	return nil
}

func headLink(link *ghpkg.ReviewLink, head string) *ghpkg.ReviewLink {
	if link == nil || !sameHead(link.HeadSHA, head) {
		return nil
	}
	return link
}

func reviewers(current *review.Aggregate, pending []review.PendingReview) []Reviewer {
	seen := map[review.Perspective]bool{}
	out := []Reviewer{}
	if current != nil {
		keys := make([]string, 0, len(current.Perspectives))
		for p := range current.Perspectives {
			keys = append(keys, string(p))
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := review.Perspective(k)
			seen[p] = true
			out = append(out, Reviewer{Perspective: p, Model: current.ReviewModel})
		}
	}
	for _, p := range pending {
		if seen[p.Perspective] {
			continue
		}
		seen[p.Perspective] = true
		out = append(out, Reviewer{Perspective: p.Perspective})
	}
	return out
}

func severity(findings []review.PerspectiveFinding) SeverityCounts {
	var c SeverityCounts
	for _, f := range findings {
		if !review.InScopeFinding(f.Finding) {
			continue
		}
		switch f.Finding.Severity {
		case outputschema.SeverityCritical:
			c.P0++
		case outputschema.SeverityHigh:
			c.P1++
		case outputschema.SeverityMedium:
			c.P2++
		default:
			c.P3++
		}
	}
	return c
}

// loop is how many automatic fix/re-engagement cycles the PR has used and the
// cap after which the hive stops trying on its own.
func loop(in Inputs, current *review.Aggregate, pendingFix *review.PendingFix) (count, limit int) {
	if current != nil {
		count = current.FixAttempts
		limit = current.MaxFixAttempts
	}
	if pendingFix != nil && pendingFix.Attempts > count {
		count = pendingFix.Attempts
	}
	for _, a := range in.Dispatch.FixAttempts {
		if samePR(a.Repo, a.Number, in.PR) && a.Attempts > count {
			count = a.Attempts
		}
	}
	if in.Escalation != nil && in.Escalation.ReEngagements > count {
		count = in.Escalation.ReEngagements
	}
	if limit <= 0 {
		limit = in.FixCycleCap
	}
	if limit <= 0 {
		limit = review.DefaultFixCycleCap
	}
	return count, limit
}

func earliestDispatch(pending []review.PendingReview) time.Time {
	var t time.Time
	for _, p := range pending {
		if !p.Dispatched.IsZero() && (t.IsZero() || p.Dispatched.Before(t)) {
			t = p.Dispatched
		}
	}
	return t
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
