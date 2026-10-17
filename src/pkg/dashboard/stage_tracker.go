package dashboard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The Hive activity monitor (GET /api/stage-tracker) is the dashboard's
// pizza-tracker view of the stage pipeline: one row per open issue / PR
// with the item's position on the stagelabels.Table, a human caption built
// from the stage and the lane's recorded reason, who must act next, the
// health modifiers riding alongside the stage, and the item's recent
// transition trail from the stage store. It reads only what the hive
// already has — the status snapshot (titles, authors, labels) and
// stagelabels.Shared() — so it costs no GitHub call, and it lists every
// open item the snapshot knows: the actionable and held sets plus the
// display-only records of what the enumeration sidelined (claimed,
// in-progress, covered, parked, filtered issues; draft and exempt PRs),
// which the first version dropped (hivecommons/hive#12682).
//
// The payload is built at most once per dashboard.stage_tracker.cache_ttl_s
// (default: the governor eval interval) and served from memory with an
// ETag and Cache-Control: max-age, so a dashboard full of pollers costs the
// process one JSON encode per TTL and GitHub nothing.

// StageTrackerResponse is the GET /api/stage-tracker payload.
type StageTrackerResponse struct {
	GeneratedAt time.Time `json:"generated_at"`
	// Source is "fresh" on the request that built the payload and "cache"
	// on every request served from memory until the TTL lapses.
	Source string `json:"source"`
	// CacheTTLSeconds is the effective dashboard.stage_tracker.cache_ttl_s.
	CacheTTLSeconds int64               `json:"cache_ttl_s"`
	HiveID          string              `json:"hive_id,omitempty"`
	Stages          []StageTrackerStage `json:"stages"`
	Items           []StageTrackerItem  `json:"items"`
	// AuthorKindLabels is true when at least one item carried an explicit
	// author/<kind> label, so the UI can say whether kinds are labelled or
	// inferred from the PR's hive markers.
	AuthorKindLabels bool `json:"author_kind_labels"`
}

// StageTrackerStage is one row of stagelabels.Table as the strip needs it.
type StageTrackerStage struct {
	Number      int    `json:"number"`
	Slug        string `json:"slug"`
	Label       string `json:"label"`
	Short       string `json:"short"`
	Description string `json:"description"`
	Issue       bool   `json:"issue"`
	PR          bool   `json:"pr"`
	// Main marks a segment of the strip; a stage that is not Main is a
	// detour rendered as a badge on the BranchFrom segment.
	Main       bool `json:"main"`
	BranchFrom int  `json:"branch_from,omitempty"`
	// DwellSeconds is the expected dwell after which the item is stalled
	// (0 = open-ended), after config overrides.
	DwellSeconds int64 `json:"dwell_seconds"`
}

// StageTrackerHop is one entry of an item's transition trail.
type StageTrackerHop struct {
	Number int       `json:"number"`
	Slug   string    `json:"slug"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// StageTrackerItem is one open issue or PR on the tracker.
type StageTrackerItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
	Author string `json:"author,omitempty"`
	// Labels are the item's GitHub labels as the snapshot saw them, so the
	// dashboard can offer a label filter without a second endpoint.
	Labels    []string   `json:"labels,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Bucket is the enumeration bucket of an item outside the actionable
	// set (claimed, in_progress, claimed_by_pr, filtered, hive_advisory,
	// reporter_triage, draft, …) and BucketReason the enumeration's note;
	// both empty for an actionable or held item.
	Bucket       string `json:"bucket,omitempty"`
	BucketReason string `json:"bucket_reason,omitempty"`
	// AuthorKind is human / clanker / hive ("" = unknown) and
	// AuthorKindSource says whether it came from an author/<kind> label
	// ("label") or was inferred from the PR's hive markers ("inferred").
	AuthorKind       string `json:"author_kind,omitempty"`
	AuthorKindSource string `json:"author_kind_source,omitempty"`
	// Tier is the author/<tier> admission label without its prefix.
	Tier string `json:"tier,omitempty"`
	// Origin is the origin/<source> provenance label without its prefix:
	// which hive component created the item ("" = none, e.g. human-filed).
	Origin string `json:"origin,omitempty"`
	// Lane is the pipeline route the item is on: issue, hive or contributor.
	Lane string `json:"lane"`
	// Stage is the stage number (0 = no stage known); StageSince is when
	// the item entered it (nil when only the label is known).
	Stage      int        `json:"stage"`
	StageSlug  string     `json:"stage_slug,omitempty"`
	StageLabel string     `json:"stage_label,omitempty"`
	StageSince *time.Time `json:"stage_since,omitempty"`
	// Mirrored is true for an issue whose stage is its covering PR's.
	Mirrored bool `json:"mirrored,omitempty"`
	// Reason is the lane's recorded reason token; Caption is the human
	// sentence built from stage + reason; Actor is who must act next.
	Reason  string `json:"reason,omitempty"`
	Caption string `json:"caption"`
	Actor   string `json:"actor,omitempty"`
	// Modifiers are the health and control markers in effect: stalled,
	// regressed, hold, needs-human, needs-decision (stage/085 carried as a
	// park signal), needs-rebase, blocked, draft.
	Modifiers     []string   `json:"modifiers"`
	RegressedFrom int        `json:"regressed_from,omitempty"`
	NudgeCount    int        `json:"nudge_count,omitempty"`
	LastNudgeAt   *time.Time `json:"last_nudge_at,omitempty"`
	AdoptedBy     int        `json:"adopted_by,omitempty"`
	// ClaimantKind is the author kind of whoever holds the live claim on a
	// claimed issue (human / clanker / hive), from the worker-claim ledger.
	ClaimantKind string `json:"claimant_kind,omitempty"`
	LinkedPR     int    `json:"linked_pr,omitempty"`
	// LinkedPRs lists the open pull requests covering an issue (the one
	// named in the stage reason first) with everything the tracker needs
	// to draw each as a sub-row nested under the issue.
	LinkedPRs []StageTrackerLinkedPR `json:"linked_prs,omitempty"`
	// NestedUnder is, on a PR, the issue in the same repo whose row nests
	// it (0 when the PR has no listed issue and stands on its own).
	NestedUnder int               `json:"nested_under,omitempty"`
	History     []StageTrackerHop `json:"history"`
}

// StageTrackerLinkedPR is one covering PR nested under an issue row.
type StageTrackerLinkedPR struct {
	Number     int        `json:"number"`
	Title      string     `json:"title,omitempty"`
	URL        string     `json:"url,omitempty"`
	Author     string     `json:"author,omitempty"`
	AuthorKind string     `json:"author_kind,omitempty"`
	Stage      int        `json:"stage,omitempty"`
	StageSince *time.Time `json:"stage_since,omitempty"`
	Caption    string     `json:"caption,omitempty"`
	Modifiers  []string   `json:"modifiers,omitempty"`
}

// Stage numbers the tracker lays out by. Pinned to stagelabels.Table in
// stage_tracker_test.go.
const (
	trackerNumberReviewed       = 60
	trackerNumberAwaitingMerge  = 80
	trackerNumberReviewPending  = 50
	trackerNumberFixing         = 70
	trackerNumberNeedsAuthor    = 71
	trackerNumberReturned       = 72
	trackerNumberAdopted        = 74
	trackerNumberNeedsDecision  = 85
	trackerNumberReadyForHuman  = 90
	trackerNumberCoveredByPR    = 40
	trackerNumberClaimed        = 20
	trackerNumberPRInProgress   = 30
	trackerNumberActionable     = 10
	stageTrackerHistoryMaxItems = stagelabels.HistoryLimit
)

// Modifier names (also the CSS hooks in index.html).
const (
	modStalled       = "stalled"
	modRegressed     = "regressed"
	modHold          = "hold"
	modNeedsHuman    = "needs-human"
	modNeedsDecision = "needs-decision"
	modNeedsRebase   = "needs-rebase"
	modBlocked       = "blocked"
	modDraft         = "draft"
)

// Lane names.
const (
	laneIssue       = "issue"
	laneHive        = "hive"
	laneContributor = "contributor"
)

// Author kinds, mirrored by value from pkg/authorkind (hivecommons/hive#12573)
// so this file works whether or not that package has landed.
const (
	authorKindHuman    = "human"
	authorKindClanker  = "clanker"
	authorKindHive     = "hive"
	authorKindLabelPfx = "author/"
	authorKindSrcLabel = "label"
	authorKindSrcInfer = "inferred"
	botLoginSuffix     = "[bot]"
)

// stageTrackerBranchFrom says which main segment a detour stage hangs off.
// The PR strip collapses 050 and 060 into one "CI / Review" chevron (lit during
// 050, ticked from 060 on), so review-side detours hang off 050; 060 itself
// lights the "Merge admission" chevron.
func stageTrackerBranchFrom(number int) int {
	switch number {
	case trackerNumberFixing, trackerNumberNeedsAuthor, trackerNumberReturned, trackerNumberAdopted:
		return trackerNumberReviewPending
	case trackerNumberNeedsDecision, trackerNumberReadyForHuman:
		return trackerNumberAwaitingMerge
	}
	return 0
}

// stageTrackerShort is the segment caption: short enough for a ~26px strip.
func stageTrackerShort(s stagelabels.Stage) string {
	switch s {
	case stagelabels.Actionable:
		return "Actionable"
	case stagelabels.Claimed:
		return "Claimed"
	case stagelabels.PRInProgress:
		return "PR in progress"
	case stagelabels.CoveredByPR:
		return "Covered by PR"
	case stagelabels.ReviewPending:
		return "CI / Review"
	case stagelabels.Reviewed:
		return "Reviewed"
	case stagelabels.Fixing:
		return "Fixing"
	case stagelabels.NeedsAuthor:
		return "Needs author"
	case stagelabels.ReturnedToContributor:
		return "Returned to contributor"
	case stagelabels.AdoptedByHive:
		return "Adopted by hive"
	case stagelabels.AwaitingMergeAdmission:
		return "Merge gate"
	case stagelabels.NeedsDecision:
		return "Needs human decision"
	case stagelabels.ReadyForHumanMerge:
		return "Ready for human merge"
	case stagelabels.MergeQueue:
		return "Merge queue"
	}
	return s.Slug()
}

func stageTrackerStages(overrides map[string]time.Duration) []StageTrackerStage {
	out := make([]StageTrackerStage, 0, len(stagelabels.Table))
	for _, d := range stagelabels.Table {
		branch := stageTrackerBranchFrom(d.Number)
		out = append(out, StageTrackerStage{
			Number: d.Number, Slug: d.Slug, Label: d.Stage.Label(), Short: stageTrackerShort(d.Stage),
			Description: d.Description, Issue: d.Issue, PR: d.PR, Main: branch == 0, BranchFrom: branch,
			DwellSeconds: int64(stagelabels.Dwell(d.Stage, overrides) / time.Second),
		})
	}
	return out
}

// stageByNumber resolves a table number to its stage (None when absent).
func stageByNumber(number int) stagelabels.Stage {
	for _, d := range stagelabels.Table {
		if d.Number == number {
			return d.Stage
		}
	}
	return stagelabels.None
}

// labelStage returns the stage carried by a `stage/NNN-slug` label, or None.
func labelStage(labels []string) stagelabels.Stage {
	for _, l := range labels {
		if n := stageNumber(l); n > 0 {
			if s := stageByNumber(n); s != stagelabels.None {
				return s
			}
		}
	}
	return stagelabels.None
}

var trackerPRNumberRE = regexp.MustCompile(`#(\d+)`)

// stageTrackerAuthorKind classifies the author from an explicit
// author/<kind> label first, else from the hive markers the snapshot kept.
func stageTrackerAuthorKind(labels []string, author string, appAuthored, hiveAttributed bool) (kind, source string) {
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case authorKindLabelPfx + authorKindHive:
			return authorKindHive, authorKindSrcLabel
		case authorKindLabelPfx + authorKindClanker:
			if kind != authorKindHive {
				kind = authorKindClanker
			}
		case authorKindLabelPfx + authorKindHuman:
			if kind == "" {
				kind = authorKindHuman
			}
		}
	}
	if kind != "" {
		return kind, authorKindSrcLabel
	}
	switch {
	case appAuthored:
		return authorKindHive, authorKindSrcInfer
	case hiveAttributed:
		return authorKindClanker, authorKindSrcInfer
	}
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), "agent/") {
			return authorKindHive, authorKindSrcInfer
		}
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(author)), botLoginSuffix) {
		return "", ""
	}
	if strings.TrimSpace(author) == "" {
		return "", ""
	}
	return authorKindHuman, authorKindSrcInfer
}

// stageTrackerTier is the author/<tier> admission label without its prefix.
func stageTrackerTier(labels []string) string {
	for _, l := range labels {
		lower := strings.ToLower(strings.TrimSpace(l))
		rest, ok := strings.CutPrefix(lower, stagelabels.AuthorPrefix)
		if !ok {
			continue
		}
		switch rest {
		case "trusted", "contributor", "newcomer", "untrusted":
			return rest
		}
	}
	return ""
}

// stageTrackerModifiers lists the control/health markers on the item.
func stageTrackerModifiers(labels []string, hiveID string, rec stagelabels.Record, hasRec bool, overrides map[string]time.Duration, now time.Time, draft bool) []string {
	set := map[string]bool{}
	for _, l := range labels {
		switch lower := strings.ToLower(strings.TrimSpace(l)); lower {
		case stagelabels.StalledLabel:
			set[modStalled] = true
		case stagelabels.RegressedLabel:
			set[modRegressed] = true
		case "needs-human":
			set[modNeedsHuman] = true
		case needsDecisionLabel, "needs-decision":
			// The stage label is the park signal (hivecommons/hive#12683);
			// the retired spelling is read until migrated.
			set[modNeedsDecision] = true
		case "needs-rebase":
			set[modNeedsRebase] = true
		case "blocked":
			set[modBlocked] = true
		}
	}
	if len(holdLabels(labels, hiveID)) > 0 {
		set[modHold] = true
	}
	if hasRec {
		if stagelabels.Stalled(rec, overrides, now) {
			set[modStalled] = true
		}
		if rec.Regressed() {
			set[modRegressed] = true
		}
	}
	if draft {
		set[modDraft] = true
	}
	out := make([]string, 0, len(set))
	for _, m := range []string{modStalled, modRegressed, modHold, modNeedsHuman, modNeedsDecision, modNeedsRebase, modBlocked, modDraft} {
		if set[m] {
			out = append(out, m)
		}
	}
	return out
}

// humanizeStageReason turns a lane reason token into a short clause with
// no leading capital and no trailing period, or "" when the token adds
// nothing to the stage description.
func humanizeStageReason(reason string) string {
	r := strings.TrimSpace(reason)
	if r == "" {
		return ""
	}
	lower := strings.ToLower(r)
	switch lower {
	case "actionable", "live agent claim", "live issue claim", "open pr covers it":
		return ""
	case "held":
		return "held by a hold label"
	case "label:needs-rebase":
		return "needs a rebase"
	case "untrusted-author-role":
		return "author's role is not trusted for unattended merge"
	case "contributor-lane:no-verdict":
		return "no review verdict yet"
	case "contributor-lane:no-claim":
		return "no verified claim links this PR to an issue"
	case "contributor-lane:claim-tier-untrusted":
		return "author's claim tier is below the auto-merge tiers"
	case "ci-red", "check-runs":
		return "CI is red"
	}
	if strings.HasPrefix(lower, "mirrors pr #") {
		return "mirrors PR " + r[len("mirrors PR "):]
	}
	if rest, ok := strings.CutPrefix(r, "required-check-pending:"); ok {
		return "check " + rest + " still running"
	}
	if rest, ok := strings.CutPrefix(r, "required-check-failing:"); ok {
		return "check " + rest + " failing"
	}
	if rest, ok := strings.CutPrefix(r, "optional-check-failing:"); ok {
		return "optional check " + rest + " failing"
	}
	if rest, ok := strings.CutPrefix(lower, "reviewer-verdict:"); ok {
		switch rest {
		case "changes_requested", "request_changes":
			return "review swarm requested changes"
		case "reject", "rejected":
			return "review swarm rejected the head"
		}
		return "review verdict " + strings.ReplaceAll(rest, "_", " ")
	}
	if rest, ok := strings.CutPrefix(r, "excluded-label:"); ok {
		return "label " + rest + " excludes it"
	}
	if rest, ok := strings.CutPrefix(r, "label:"); ok {
		return "label " + rest
	}
	if rest, ok := strings.CutPrefix(lower, "pending: "); ok {
		if i := strings.Index(rest, " (< min_head_age)"); i >= 0 {
			return rest[:i] + "; waiting for the minimum head age"
		}
		return rest
	}
	if strings.HasPrefix(lower, "contributor-lane:soaking") {
		return "soaking"
	}
	if strings.HasPrefix(lower, "release-hold") {
		return "release hold"
	}
	if strings.HasPrefix(lower, "status-") || strings.HasPrefix(lower, "check-") {
		return "CI is red (" + r + ")"
	}
	if rest, ok := strings.CutPrefix(lower, "contributor-lane:"); ok {
		return strings.ReplaceAll(strings.ReplaceAll(rest, "-", " "), "_", " ")
	}
	return strings.NewReplacer("-", " ", "_", " ", ":", ": ").Replace(r)
}

func joinClause(head, clause string) string {
	if clause == "" {
		return head
	}
	return head + " — " + clause
}

// stageTrackerCaption builds the human sentence and the actor for an item.
func stageTrackerCaption(stage stagelabels.Stage, reason, author string, kind string, mirrored bool, rec stagelabels.Record, hasRec bool, linkedPR int) (caption, actor string) {
	clause := humanizeStageReason(reason)
	who := strings.TrimSpace(author)
	if who == "" {
		who = "the author"
	} else {
		who = "@" + who
	}
	prRef := ""
	if linkedPR > 0 {
		prRef = " #" + strconv.Itoa(linkedPR)
	}
	if kind == "issue" && mirrored {
		return "Covered by PR" + prRef + " — " + strings.ToLower(stageTrackerShort(stage)) + " on the PR side", "its PR"
	}
	switch stage {
	case stagelabels.Actionable:
		return joinClause("Triaged and eligible; waiting for the scanner to pick it up", clause), "scanner"
	case stagelabels.Claimed:
		return joinClause("Claimed; work is about to start", clause), "claimant"
	case stagelabels.PRInProgress:
		return joinClause("An agent kick is running; no PR yet", clause), "hive"
	case stagelabels.CoveredByPR:
		return joinClause("An open PR"+prRef+" covers this issue", clause), "its PR"
	case stagelabels.ReviewPending:
		return joinClause("Review swarm requested; waiting for a verdict on the current head", clause), "review swarm"
	case stagelabels.Reviewed:
		return joinClause("Review swarm approved; a gate is still pending", clause), "CI"
	case stagelabels.Fixing:
		return joinClause("Red or changes requested; a hive agent is fixing it", clause), "hive"
	case stagelabels.NeedsAuthor:
		return joinClause("Needs "+who, clause) + " — " + who + " must act", strings.TrimPrefix(who, "@")
	case stagelabels.ReturnedToContributor:
		c := joinClause("Returned to "+who, clause) + " — " + who + " must act"
		if hasRec && rec.NudgeCount > 0 {
			c += " (" + plural(rec.NudgeCount, "nudge") + " sent)"
		}
		return c, strings.TrimPrefix(who, "@")
	case stagelabels.AdoptedByHive:
		c := "Hive adopted this PR"
		if hasRec && rec.AdoptedBy > 0 {
			c += " (superseded by #" + strconv.Itoa(rec.AdoptedBy) + ")"
		}
		return joinClause(c, clause), "hive"
	case stagelabels.AwaitingMergeAdmission:
		return joinClause("Green and approved; waiting for the automerge lane to admit it", clause), "automerge lane"
	case stagelabels.NeedsDecision:
		return joinClause("A human decision is owed", clause), "maintainer"
	case stagelabels.ReadyForHumanMerge:
		return joinClause("Ready for a maintainer's /lgtm /approve; the hive cannot merge it itself", clause), "maintainer"
	case stagelabels.MergeQueue:
		return joinClause("In the merge queue; waiting for soak, tide or the merge window", clause), "tide"
	}
	return joinClause("No stage recorded yet", clause), ""
}

func stageTrackerHistory(rec stagelabels.Record) []StageTrackerHop {
	out := make([]StageTrackerHop, 0, len(rec.History))
	for _, h := range rec.History {
		out = append(out, StageTrackerHop{Number: h.Stage.Number(), Slug: h.Stage.Slug(), At: h.At, Reason: h.Reason})
	}
	if len(out) > stageTrackerHistoryMaxItems {
		out = out[len(out)-stageTrackerHistoryMaxItems:]
	}
	return out
}

// Enumeration buckets (pkg/github work details) and the stage each implies
// for an issue that carries no stage label and has no store record yet.
const (
	trackerBucketClaimed     = "claimed"
	trackerBucketInProgress  = "in_progress"
	trackerBucketClaimedByPR = "claimed_by_pr"
)

func bucketStage(bucket string) stagelabels.Stage {
	switch bucket {
	case trackerBucketClaimed:
		return stagelabels.Claimed
	case trackerBucketInProgress:
		return stagelabels.PRInProgress
	case trackerBucketClaimedByPR:
		return stagelabels.CoveredByPR
	}
	return stagelabels.None
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t
	return &u
}

// stageStoreOrShared returns the injected stage store (tests) or the
// process-wide one the reconcilers write.
func (s *Server) stageStoreOrShared() *stagelabels.Store {
	if s != nil && s.stageStore != nil {
		return s.stageStore
	}
	return stagelabels.Shared()
}

func (s *Server) stageDwellOverrides() map[string]time.Duration {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	return s.deps.Config.Governor.Labels.Stage.DwellOverrides()
}

// buildStageTracker assembles the response from status and store at now.
func (s *Server) buildStageTracker(status *StatusPayload, store *stagelabels.Store, now time.Time) StageTrackerResponse {
	overrides := s.stageDwellOverrides()
	resp := StageTrackerResponse{GeneratedAt: now, Stages: stageTrackerStages(overrides), Items: []StageTrackerItem{}}
	if status == nil {
		return resp
	}
	resp.HiveID = status.HiveID
	lookup := func(repo string, number int) (stagelabels.Record, bool) {
		if store == nil {
			return stagelabels.Record{}, false
		}
		return store.Get(stagelabels.Key(repo, number))
	}
	finish := func(it *StageTrackerItem, labels []string, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, draft bool) {
		if hasRec && rec.Stage != stagelabels.None {
			stage = rec.Stage
			t := rec.EnteredAt
			it.StageSince = &t
			it.Reason = rec.Reason
			it.RegressedFrom = rec.RegressedFrom.Number()
			it.NudgeCount = rec.NudgeCount
			if !rec.LastNudgeAt.IsZero() {
				t2 := rec.LastNudgeAt
				it.LastNudgeAt = &t2
			}
			it.AdoptedBy = rec.AdoptedBy
		}
		it.Stage = stage.Number()
		it.StageSlug = stage.Slug()
		it.StageLabel = stage.Label()
		if m := trackerPRNumberRE.FindStringSubmatch(it.Reason); m != nil && it.Kind == "issue" {
			it.LinkedPR, _ = strconv.Atoi(m[1])
		}
		it.Mirrored = it.Kind == "issue" && stage != stagelabels.None && stage.Number() > trackerNumberCoveredByPR
		it.Modifiers = stageTrackerModifiers(labels, status.HiveID, rec, hasRec, overrides, now, draft)
		// 085 has its own chevron word ("Needs human decision"); the chip
		// only adds information when needs-decision modifies another stage.
		if stage == stagelabels.NeedsDecision {
			it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsDecision })
		}
		it.Caption, it.Actor = stageTrackerCaption(stage, it.Reason, it.Author, it.Kind, it.Mirrored, rec, hasRec, it.LinkedPR)
		it.History = stageTrackerHistory(rec)
	}
	seen := map[string]bool{}
	// Keyed on (repo, kind, number): the cross-kind collision (a PR echoed by
	// the issues feed) is resolved before add, with the PR record winning.
	add := func(it StageTrackerItem) {
		key := it.Repo + "/" + it.Kind + "#" + strconv.Itoa(it.Number)
		if seen[key] {
			return
		}
		seen[key] = true
		resp.Items = append(resp.Items, it)
	}
	addIssue := func(repoName string, issue github.Issue, bucket, bucketReason string) {
		issue.Repo = nonEmpty(issue.Repo, repoName)
		rec, ok := lookup(repoName, issue.Number)
		it := StageTrackerItem{Repo: repoName, Number: issue.Number, Kind: "issue", Title: issue.Title, Author: issue.Author, Lane: laneIssue,
			Labels: append([]string(nil), issue.Labels...), CreatedAt: timePtr(issue.CreatedAt), UpdatedAt: timePtr(issue.UpdatedAt),
			Bucket: bucket, BucketReason: bucketReason,
			URL: overviewItemURL(issue.URL, issue.Repo, issue.Number, "issue", status.GitHubBaseURL)}
		if issue.AuthorIsHuman {
			it.AuthorKind, it.AuthorKindSource = authorKindHuman, authorKindSrcInfer
		} else if strings.HasSuffix(strings.ToLower(issue.Author), botLoginSuffix) {
			it.AuthorKind, it.AuthorKindSource = authorKindHive, authorKindSrcInfer
		}
		if l, ok := config.FirstOriginLabel(issue.Labels); ok {
			it.Origin = config.OriginSourceOf(l)
		} else {
			for _, l := range issue.Labels {
				if canonical, ok := config.CanonicalAgentLaneLabel(l); ok {
					it.Origin = config.OriginSourceOf(canonical)
					break
				}
			}
		}
		stage := labelStage(issue.Labels)
		if stage == stagelabels.None && !(ok && rec.Stage != stagelabels.None) {
			stage = bucketStage(bucket)
		}
		finish(&it, issue.Labels, stage, rec, ok, false)
		if it.Stage == trackerNumberClaimed {
			it.ClaimantKind = s.stageTrackerClaimantKind(repoName, issue.Number)
		}
		for _, lp := range issue.LinkedPRs {
			if lp.Number > 0 && !lp.Merged && !strings.EqualFold(lp.State, "closed") {
				it.LinkedPRs = append(it.LinkedPRs, StageTrackerLinkedPR{Number: lp.Number, URL: lp.URL})
			}
		}
		add(it)
	}
	addPR := func(repoName string, pr github.PullRequest, verdict *github.MergeVerdict, bucket, bucketReason string) {
		pr.Repo = nonEmpty(pr.Repo, repoName)
		rec, ok := lookup(repoName, pr.Number)
		it := StageTrackerItem{Repo: repoName, Number: pr.Number, Kind: "pr", Title: pr.Title, Author: pr.Author,
			Labels: append([]string(nil), pr.Labels...), CreatedAt: timePtr(pr.CreatedAt), UpdatedAt: timePtr(pr.UpdatedAt),
			Bucket: bucket, BucketReason: bucketReason,
			URL: overviewItemURL(pr.URL, pr.Repo, pr.Number, "pr", status.GitHubBaseURL)}
		it.AuthorKind, it.AuthorKindSource = stageTrackerAuthorKind(pr.Labels, pr.Author, pr.AppAuthored, pr.HiveAttributed)
		if it.AuthorKindSource == authorKindSrcLabel {
			resp.AuthorKindLabels = true
		}
		it.Tier = stageTrackerTier(pr.Labels)
		if verdict != nil && it.Tier == "" {
			it.Tier = strings.ToLower(strings.TrimSpace(verdict.Tier))
		}
		it.Lane = laneContributor
		if it.AuthorKind == authorKindHive || pr.AppAuthored {
			it.Lane = laneHive
		}
		finish(&it, pr.Labels, labelStage(pr.Labels), rec, ok, pr.Draft)
		add(it)
	}
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		// GitHub's issues feed also returns pull requests. A PR is never an
		// issue in the tracker: when a number is in the repo's PR set, the PR
		// record wins and the issue-feed copy is dropped, otherwise the PR
		// would pair with itself and render as its own parent issue.
		prNumbers := map[int]bool{}
		for _, held := range []bool{false, true} {
			for _, fp := range frontendRepoPRs(repo, held) {
				prNumbers[fp.pr.Number] = true
			}
		}
		for _, raw := range repo.NonActionablePrs {
			if w, ok := repoWorkPR(raw); ok {
				prNumbers[w.Number] = true
			}
		}
		for _, held := range []bool{false, true} {
			for _, issue := range frontendRepoIssues(repo, held) {
				if prNumbers[issue.Number] || strings.Contains(issue.URL, "/pull/") {
					continue
				}
				addIssue(repoName, issue, "", "")
			}
			for _, fp := range frontendRepoPRs(repo, held) {
				addPR(repoName, fp.pr, fp.verdict, "", "")
			}
		}
		// The enumeration's sidelined items: issues it parked in a
		// work-detail bucket and the drafts / exempt PRs it dropped. They
		// are the open items the first tracker never showed.
		for _, raw := range repo.NonActionableIssues {
			if w, ok := repoWorkIssue(raw); ok {
				if prNumbers[w.Number] || strings.Contains(w.URL, "/pull/") {
					continue
				}
				addIssue(repoName, github.Issue{Repo: w.Repo, Number: w.Number, Title: w.Title, Author: w.Author, Labels: w.Labels, Assignees: w.Assignees,
					CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL, ClaimKind: w.ClaimKind, ClaimedBy: w.ClaimedBy, LinkedPRs: w.LinkedPRs, ClaimContext: w.ClaimContext}, w.Bucket, w.Reason)
			}
		}
		for _, raw := range repo.NonActionablePrs {
			if w, ok := repoWorkPR(raw); ok {
				addPR(repoName, github.PullRequest{Repo: w.Repo, Number: w.Number, Title: w.Title, Author: w.Author, Labels: w.Labels, Draft: w.Draft,
					AppAuthored: w.AppAuthored, HiveAttributed: w.HiveAttributed, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL}, nil, w.Bucket, w.Reason)
			}
		}
	}
	stageTrackerResolveLinkedPRs(resp.Items)
	sort.SliceStable(resp.Items, func(i, j int) bool {
		a, b := resp.Items[i], resp.Items[j]
		if a.Stage != b.Stage {
			return a.Stage > b.Stage
		}
		// Within a stage the item that has waited longest comes first: it is
		// the one most likely to need a human.
		at, bt := trackerSince(a), trackerSince(b)
		if at.IsZero() != bt.IsZero() {
			return !at.IsZero()
		}
		if !at.Equal(bt) {
			return at.Before(bt)
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return a.Number < b.Number
	})
	return resp
}

func trackerSince(it StageTrackerItem) time.Time {
	if it.StageSince != nil {
		return *it.StageSince
	}
	return time.Time{}
}

// stageTrackerClaimantKind maps the live claim holder's ledger kind onto
// the tracker's author kinds: a person is human, one of the hive's own
// agents is hive, a relay contributor or outside bot is clanker. Empty when
// the ledger is off or holds no claim.
func (s *Server) stageTrackerClaimantKind(repo string, number int) string {
	l := s.claimsLedger()
	if l == nil {
		return ""
	}
	c, ok := l.Lookup(repo, number)
	if !ok {
		return ""
	}
	switch c.Kind {
	case claims.KindHuman:
		return authorKindHuman
	case claims.KindAgent:
		return authorKindHive
	case claims.KindContributor, claims.KindExternal:
		return authorKindClanker
	}
	return ""
}

func repoWorkIssue(raw any) (github.RepoWorkIssue, bool) {
	switch v := raw.(type) {
	case github.RepoWorkIssue:
		return v, true
	case *github.RepoWorkIssue:
		if v != nil {
			return *v, true
		}
	}
	return github.RepoWorkIssue{}, false
}

func repoWorkPR(raw any) (github.RepoWorkPR, bool) {
	switch v := raw.(type) {
	case github.RepoWorkPR:
		return v, true
	case *github.RepoWorkPR:
		if v != nil {
			return *v, true
		}
	}
	return github.RepoWorkPR{}, false
}

// stageTrackerRESTCaller tags the endpoint's requests in the REST
// accounting (/api/gh-rate-limits) so an operator can see them listed with
// Charged 0: the handler never reaches GitHub.
const stageTrackerRESTCaller = "dashboard:stage-tracker"

// stageTrackerCache is the memoised payload: the encoded body, its ETag and
// when it was built.
type stageTrackerCache struct {
	body        []byte
	etag        string
	generatedAt time.Time
}

func (s *Server) stageTrackerCacheTTL() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerCacheTTL()
	}
	return (*config.Config)(nil).StageTrackerCacheTTL()
}

// stageTrackerPayload returns the cached body when it is younger than the
// TTL, else builds, encodes and caches a new one. fresh reports a rebuild.
func (s *Server) stageTrackerPayload(status *StatusPayload, now time.Time) (body []byte, etag string, generatedAt time.Time, fresh bool) {
	ttl := s.stageTrackerCacheTTL()
	s.stageTrackerCacheMu.Lock()
	defer s.stageTrackerCacheMu.Unlock()
	c := &s.stageTrackerCache
	if len(c.body) > 0 && now.Sub(c.generatedAt) < ttl {
		return c.body, c.etag, c.generatedAt, false
	}
	resp := s.buildStageTracker(status, s.stageStoreOrShared(), now)
	resp.CacheTTLSeconds = int64(ttl / time.Second)
	// The encoded body carries source=cache: that is what every later hit
	// serves verbatim, and what the ETag validates. The building request
	// alone rewrites the marker to fresh on its way out (handleStageTracker).
	resp.Source = "cache"
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		slogWarnOverviewEncode(err)
		return nil, "", now, true
	}
	c.body, c.etag, c.generatedAt = buf.Bytes(), webstatic.ETagFor(buf.Bytes()), now
	return c.body, c.etag, c.generatedAt, true
}

var stageTrackerSourceCache = []byte(`"source":"cache"`)
var stageTrackerSourceFresh = []byte(`"source":"fresh"`)

// handleStageTracker serves GET /api/stage-tracker from the memoised payload.
func (s *Server) handleStageTracker(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if status == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
		return
	}
	now := time.Now().UTC()
	body, etag, generatedAt, fresh := s.stageTrackerPayload(status, now)
	if body == nil {
		jsonError(w, "stage tracker unavailable", http.StatusInternalServerError)
		return
	}
	remaining := s.stageTrackerCacheTTL() - now.Sub(generatedAt)
	if remaining < 0 {
		remaining = 0
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(int64(remaining/time.Second), 10))
	w.Header().Set("X-Hive-Stage-Tracker-Source", map[bool]string{true: "fresh", false: "cache"}[fresh])
	if match := r.Header.Get("If-None-Match"); match != "" && !fresh && strings.Contains(match, etag) {
		github.RecordLocalServe(stageTrackerRESTCaller, r.URL.Path, http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	github.RecordLocalServe(stageTrackerRESTCaller, r.URL.Path, http.StatusOK)
	if fresh {
		body = bytes.Replace(body, stageTrackerSourceCache, stageTrackerSourceFresh, 1)
	}
	if _, err := w.Write(body); err != nil {
		slogWarnOverviewEncode(err)
	}
}

// stageTrackerResolveLinkedPRs fills each issue's linked PR entries from the
// PR items in the same response (title, author, stage, since, modifiers),
// puts the PR the stage reason names first, drops duplicates and marks each
// such PR as nested under the lowest-numbered issue that lists it.
func stageTrackerResolveLinkedPRs(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	for i := range items {
		if items[i].Kind == "pr" {
			prs[items[i].Repo+"#"+strconv.Itoa(items[i].Number)] = &items[i]
		}
	}
	for i := range items {
		it := &items[i]
		if it.Kind != "issue" {
			continue
		}
		seen := map[int]bool{}
		out := make([]StageTrackerLinkedPR, 0, len(it.LinkedPRs)+1)
		add := func(lp StageTrackerLinkedPR) {
			// Pairing is keyed on (repo, kind, number): an issue never links
			// to its own number, so nothing can become its own parent.
			if lp.Number == 0 || lp.Number == it.Number || seen[lp.Number] {
				return
			}
			seen[lp.Number] = true
			if pr := prs[it.Repo+"#"+strconv.Itoa(lp.Number)]; pr != nil {
				lp.Title, lp.Author, lp.AuthorKind, lp.Stage = pr.Title, pr.Author, pr.AuthorKind, pr.Stage
				lp.StageSince, lp.Caption = pr.StageSince, pr.Caption
				if len(pr.Modifiers) > 0 {
					lp.Modifiers = append([]string(nil), pr.Modifiers...)
				}
				if lp.URL == "" {
					lp.URL = pr.URL
				}
				if pr.NestedUnder == 0 || it.Number < pr.NestedUnder {
					pr.NestedUnder = it.Number
				}
			}
			out = append(out, lp)
		}
		if it.LinkedPR > 0 {
			add(StageTrackerLinkedPR{Number: it.LinkedPR})
		}
		for _, lp := range it.LinkedPRs {
			add(lp)
		}
		if len(out) == 0 {
			out = nil
		}
		it.LinkedPRs = out
	}
}
