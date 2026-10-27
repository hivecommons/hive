package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
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
	// OpenIssues and OpenPRs count Items by kind. Items is the canonical
	// open-items set (open_items.go), so these equal the Overview tile's
	// total open issues and PRs; the SPA prints them in the section header
	// and the node-driven tests pin the equality.
	OpenIssues int `json:"open_issues"`
	OpenPRs    int `json:"open_prs"`
	// AuthorKindLabels is true when at least one item carried an explicit
	// author/<kind> label, so the UI can say whether kinds are labelled or
	// inferred from the PR's hive markers.
	AuthorKindLabels bool `json:"author_kind_labels"`
	// NeedsHumanLabels is the "a person must act" label set
	// (needsHumanPresetLabels) behind the tracker's synthetic Needs human
	// facet, so the page and the Projects preset cannot disagree.
	NeedsHumanLabels []string `json:"needs_human_labels,omitempty"`
	// Engine is the ADR-0025 central stage engine's mode and counters for
	// the tracker's status strip; absent when the hive has no engine.
	Engine *stageengine.Status `json:"engine,omitempty"`
	// RecentlyMerged lists the pull requests the hive saw merge within
	// stageTrackerMergedWindow, from the audit log's pr_merged entries
	// (sweep, queue, relay and human merges observed by the PR poller
	// alike). Items only ever lists open work, so a merged PR simply
	// drops out of Items on the next build; this side list is what lets
	// the SPA tell "merged" from "closed" between two polls and play its
	// opt-in merge sound for the right rows.
	RecentlyMerged []StageTrackerMerged `json:"recently_merged,omitempty"`
}

// StageTrackerMerged is one recently merged pull request.
type StageTrackerMerged struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	MergedAt time.Time `json:"merged_at"`
}

// stageTrackerMergedWindow bounds RecentlyMerged. It is deliberately much
// longer than the cache TTL plus the SPA's 30 s poll: a tab that missed a
// few polls (throttled in the background) still sees the merge it was
// watching for, and the SPA dedupes per PR so a longer list never replays.
const stageTrackerMergedWindow = 30 * time.Minute

var stageTrackerAuditNumberRe = regexp.MustCompile(`(?:^|[,\s])number=(\d+)`)

// stageTrackerRepoQualifier maps the short repo name a configured repo
// goes by in audit entries (the PR poller's terminal observations record
// `repo=hive`) to the owner/name Items carry (`hivecommons/hive`), so the
// SPA's merge detector can match the two. Names already qualified, and
// short names no configured repo accounts for, pass through unchanged.
func stageTrackerRepoQualifier(status *StatusPayload) func(string) string {
	full := map[string]string{}
	if status != nil {
		for _, repo := range status.Repos {
			name := overviewRepoName(repo)
			if short := name[strings.LastIndex(name, "/")+1:]; short != "" && short != name {
				if prev, dup := full[short]; dup && prev != name {
					full[short] = "" // ambiguous across owners: leave as is
					continue
				}
				full[short] = name
			}
		}
	}
	return func(repo string) string {
		if strings.Contains(repo, "/") {
			return repo
		}
		if q := full[repo]; q != "" {
			return q
		}
		return repo
	}
}

// stageTrackerRecentlyMerged reads the pr_merged audit entries newer than
// now-stageTrackerMergedWindow from the in-memory ring: newest first, one
// entry per (repo, number) after qualify has mapped short repo names onto
// the owner/name Items use. The ring holds auditRingCap entries and is the
// only source consulted — a window this short never needs the on-disk log,
// and reading files on every tracker build would defeat its cache.
func (s *Server) stageTrackerRecentlyMerged(now time.Time, qualify func(string) string) []StageTrackerMerged {
	if s == nil || s.audit == nil {
		return nil
	}
	if qualify == nil {
		qualify = func(repo string) string { return repo }
	}
	seen := map[string]bool{}
	var out []StageTrackerMerged
	for _, e := range s.audit.RecentWithPrefixSince(now.Add(-stageTrackerMergedWindow), github.AuditActionPRMerged) {
		if e.Action != github.AuditActionPRMerged {
			continue
		}
		repo, ok := collect.AuditEntryRepo(e)
		if !ok {
			continue
		}
		repo = qualify(repo)
		number := e.Target
		if number == 0 {
			if m := stageTrackerAuditNumberRe.FindStringSubmatch(e.Detail); m != nil {
				number, _ = strconv.Atoi(m[1])
			}
		}
		if number <= 0 {
			continue
		}
		key := repo + "#" + strconv.Itoa(number)
		if seen[key] {
			continue
		}
		seen[key] = true
		at, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			at = now
		}
		out = append(out, StageTrackerMerged{Repo: repo, Number: number, MergedAt: at.UTC()})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].MergedAt.Equal(out[j].MergedAt) {
			return out[i].MergedAt.After(out[j].MergedAt)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out
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
	// From is the stage number left (0 on the first observation);
	// Regressed marks the hop as a backward move.
	From      int  `json:"from,omitempty"`
	Regressed bool `json:"regressed,omitempty"`
}

// StageTrackerItem is one open issue or PR on the tracker.
type StageTrackerItem struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
	Author string `json:"author,omitempty"`
	// Head is the PR head SHA the lane last evaluated ("" for issues), so
	// a "waiting for a verdict" caption can name the revision it waits on.
	Head           string                `json:"head,omitempty"`
	BaseRef        string                `json:"base_ref,omitempty"`
	ClaimReference bool                  `json:"claim_reference,omitempty"`
	IsTracker      bool                  `json:"is_tracker,omitempty"`
	Children       *github.IssueChildren `json:"children,omitempty"`
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
	// regressed, hold, needs-human, needs-decision (stage/045 on an issue or
	// stage/085 on a PR carried as a park signal), needs-rebase, blocked,
	// draft.
	Modifiers []string `json:"modifiers"`
	// RegressedFrom / RegressedTo / RegressedAt / RegressedReason narrate
	// the unresolved regression behind the `regressed` modifier: the
	// high-water stage the item fell from, the stage that fall landed on
	// (the item may have fallen further since), when, and the reason
	// recorded with the fall.
	RegressedFrom   int        `json:"regressed_from,omitempty"`
	RegressedTo     int        `json:"regressed_to,omitempty"`
	RegressedAt     *time.Time `json:"regressed_at,omitempty"`
	RegressedReason string     `json:"regressed_reason,omitempty"`
	// SoakRemainingSeconds is, at 095 pre-merge-soak, how much of the lane's
	// soak window was left as of the last sweep (the lane's
	// `soaking(remaining=…)` reason aged by the time since it was recorded);
	// 0 when unknown or not soaking.
	SoakRemainingSeconds int64 `json:"soak_remaining_seconds,omitempty"`
	// SentinelAlert is true on a PR the suspicious-activity sentinel flagged:
	// it wears the configured alert label (sentinel.label, default
	// sentinel-alert) or the review evidence bundle for its current head
	// records sentinel findings. A webhook label patch flips it without
	// waiting for the next governor cycle. SentinelFindings counts the
	// findings in that bundle (0 when only the label says so) and
	// SentinelReason is the first finding's summary, so the lane text can
	// name why a person must look (hivecommons/hive: "Sentinel alert ·
	// needs human").
	SentinelAlert    bool   `json:"sentinel_alert,omitempty"`
	SentinelFindings int    `json:"sentinel_findings,omitempty"`
	SentinelReason   string `json:"sentinel_reason,omitempty"`
	// HumanApprover is the trusted merger whose GitHub Approve review on
	// the current head settled a requires_human verdict or a manual-merge
	// lane rule (the `:human-approved@login` suffix on the stage reason).
	// ManualMerge is true when the stage reason is a lane rule the hive
	// never merges unattended (CI workflow / action edits, removed tests,
	// a human-merge path): at 090 the merge gate then reads "Awaiting
	// human merge" and names the approver, since the only step left is
	// the maintainer's merge click.
	HumanApprover string `json:"human_approver,omitempty"`
	ManualMerge   bool   `json:"manual_merge,omitempty"`
	// HumanMergeReason is, at 090, the compact code naming why a person
	// and not the hive merges this PR (prstage.HumanMergeReason*:
	// "workflow-files", "untrusted-author", "branch-protection", …;
	// "policy" when the lane reason has no more specific code), and
	// HumanMergeReasonText the one-sentence tooltip for it, so the gate
	// cell reads "Ready for human merge · workflow files" rather than
	// leaving the owner to ask why (hivecommons/hive#14658).
	HumanMergeReason     string `json:"human_merge_reason,omitempty"`
	HumanMergeReasonText string `json:"human_merge_reason_text,omitempty"`
	// UnauthorizedStep is set when the item wears hive:unauthorized: the
	// step the hive's GitHub App is not permitted to take, in words
	// ("approve workflow runs"), from the `unauthorized:<step>` stage
	// reason; the gate cell reads "Awaiting human: <step>". Unauthorized
	// is the pill's tooltip naming the permission and the human action
	// (hivecommons/hive#14589).
	UnauthorizedStep string `json:"unauthorized_step,omitempty"`
	Unauthorized     string `json:"unauthorized,omitempty"`
	// StaleLabels are control labels the row still wears that its stage
	// has outrun, so they are not read as modifiers: `needs-triage` on an
	// issue at 020 or later (015 is not a gate; the reconciler removes the
	// label on its next pass, hivecommons/hive#14586). Rendered as a muted
	// operator tooltip, never as the stage word.
	StaleLabels []string   `json:"stale_labels,omitempty"`
	NudgeCount  int        `json:"nudge_count,omitempty"`
	LastNudgeAt *time.Time `json:"last_nudge_at,omitempty"`
	AdoptedBy   int        `json:"adopted_by,omitempty"`
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
	NestedUnder int `json:"nested_under,omitempty"`
	// LinkedIssues lists, on a PR, the issues GitHub says it closes
	// (closingIssuesReferences, i.e. the PR body's Fixes/Closes/Resolves
	// keywords) in the same repo, so a PR whose issue is not listed —
	// already closed, or outside the snapshot — can still name it in the
	// issue slot of its row instead of standing there itself. Closing
	// links come first; after them the soft links the PR's own text
	// yields (`Refs #N`, `(#N, part 1)` titles, first-paragraph mentions:
	// github.PRReferencedIssues), each tagged with Via, so a PR that
	// works towards an issue joins its row instead of standing as an
	// orphan (hivecommons/hive#13544).
	LinkedIssues []StageTrackerLinkedIssue `json:"linked_issues,omitempty"`
	// ClosedIssue is set on a PR that stands as its own row because every
	// issue it closes is already closed: the row is anchored on the PR (its
	// number and title lead, its stage drives the strip, its timestamps
	// sort it) and the closed issue is a secondary `closes #N · closed`
	// chip. StateReason "not_planned" adds an "issue closed (not planned)"
	// chip so the reader can tell the PR may be moot (hivecommons/hive#14655).
	ClosedIssue *StageTrackerLinkedIssue `json:"closed_issue,omitempty"`
	History     []StageTrackerHop        `json:"history"`
}

// StageTrackerLinkedIssue is one issue a PR closes, as its row's issue
// slot needs it. State is "open" / "closed" when known, "" otherwise.
type StageTrackerLinkedIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
	// StateReason is GitHub's stateReason for a closed issue, lower-cased
	// ("completed", "not_planned", "duplicate"); "" when unknown.
	StateReason string `json:"state_reason,omitempty"`
	// Listed is true when the issue has its own row in this response; the
	// PR is then nested under it and never drawn as a top-level row.
	Listed bool `json:"listed,omitempty"`
	// Via is how the link was found: "" for GitHub's closing reference or
	// a closing keyword hive read itself, else one of github.PRLinkVia*
	// ("refs", "title", "mention"); the UI draws those as a soft `refs`
	// link rather than a closing one.
	Via string `json:"via,omitempty"`
}

// StageTrackerLinkedPR is one covering PR nested under an issue row.
type StageTrackerLinkedPR struct {
	BaseRef    string     `json:"base_ref,omitempty"`
	Number     int        `json:"number"`
	Title      string     `json:"title,omitempty"`
	URL        string     `json:"url,omitempty"`
	Author     string     `json:"author,omitempty"`
	AuthorKind string     `json:"author_kind,omitempty"`
	Stage      int        `json:"stage,omitempty"`
	StageSince *time.Time `json:"stage_since,omitempty"`
	Caption    string     `json:"caption,omitempty"`
	Modifiers  []string   `json:"modifiers,omitempty"`
	// SentinelAlert / SentinelReason mirror the nested PR's own row fields
	// so a sub-row under an issue names the alert too.
	SentinelAlert  bool   `json:"sentinel_alert,omitempty"`
	SentinelReason string `json:"sentinel_reason,omitempty"`
	// HumanApprover / ManualMerge / HumanMergeReason(Text) mirror the
	// nested PR's 090 fields so a sub-row's gate cell says why a person
	// merges it, as the top-level row would (hivecommons/hive#14658).
	HumanApprover        string `json:"human_approver,omitempty"`
	ManualMerge          bool   `json:"manual_merge,omitempty"`
	HumanMergeReason     string `json:"human_merge_reason,omitempty"`
	HumanMergeReasonText string `json:"human_merge_reason_text,omitempty"`
	Head                 string `json:"head,omitempty"`
}

// Stage numbers the tracker lays out by. Pinned to stagelabels.Table in
// stage_tracker_test.go.
const (
	trackerNumberReviewed      = 60
	trackerNumberAwaitingMerge = 80
	trackerNumberReviewPending = 50
	trackerNumberFixing        = 70
	trackerNumberNeedsAuthor   = 71
	trackerNumberReturned      = 72
	trackerNumberAdopted       = 74
	trackerNumberNeedsDecision = 85
	// trackerNumberIssueDecision is the issue parking stage (045): a main
	// issue-side stop, not a detour off the merge gate.
	trackerNumberIssueDecision = 45
	trackerNumberReadyForHuman = 90
	trackerNumberPreMergeSoak  = 95
	trackerNumberCoveredByPR   = 40
	trackerNumberClaimed       = 20
	trackerNumberPRInProgress  = 30
	trackerNumberActionable    = 10
	// trackerNumberTriageNeeded is the untriaged-actionable stage (015): it
	// lights the Actionable column with the word "Triage needed".
	trackerNumberTriageNeeded = 15
	// trackerNumberPossiblyExists is the prior-art park (017): the issue
	// lights the Actionable column with the word "Possibly exists" and
	// waits on its author (hivecommons/hive#13575).
	trackerNumberPossiblyExists = 17
	stageTrackerHistoryMaxItems = stagelabels.HistoryLimit
)

// Modifier names (also the CSS hooks in index.html).
const (
	modStalled       = "stalled"
	modRegressed     = "regressed"
	modHold          = "hold"
	modNeedsHuman    = "needs-human"
	modNeedsDecision = "needs-decision"
	modNeedsTriage   = "needs-triage"
	// modPossibleDupe: the issue carries the prior-art park label; the
	// chip reads "possible dupe".
	modPossibleDupe = "possible-dupe"
	modNeedsRebase  = "needs-rebase"
	modBlocked      = "blocked"
	modDraft        = "draft"
	// modUnauthorized: the item wears stagelabels.UnauthorizedLabel — the
	// hive's GitHub App lacks the permission for its next step and a
	// person must take it (hivecommons/hive#14589).
	modUnauthorized = "unauthorized"
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
	case stagelabels.TriageNeeded:
		return "Triage needed"
	case stagelabels.PossiblyExisting:
		return "Possibly exists"
	case stagelabels.Claimed:
		return "Claimed"
	case stagelabels.PRInProgress:
		return "PR in progress"
	case stagelabels.CoveredByPR:
		return "Covered by PR"
	case stagelabels.IssueNeedsDecision:
		return "Needs human decision"
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
	case stagelabels.PreMergeSoak:
		return "Soaking"
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

// stageTrackerSentinelMoreFormat renders how many findings follow the one
// named in SentinelReason.
const stageTrackerSentinelMoreFormat = " (+%d more)"

// stageTrackerSentinel reports whether the sentinel alert on repo#number
// at head still stands. The live labels decide: the sweep announces a
// blocking finding through the configured alert label, and a maintainer
// removing that label is the sweep's own clearance signal
// (sentinelClearedByMaintainer), so a webhook label patch flips the gate at
// once. The verified review evidence bundle for head supplies the findings
// count and the first finding's summary, and promotes the alert on its own
// only for a notice-only finding (a trusted author's PR: comment, never the
// label), since no label ever carried that one. A blocking finding whose
// label is gone is reported as cleared — findings and reason still fill
// the tooltip, alert does not. One file read per PR with a known head,
// ENOENT when the sentinel never wrote one; the tracker payload is memoised
// so this runs once per snapshot, not per request.
func (s *Server) stageTrackerSentinel(verifier *github.ReviewEvidenceVerifier, repo string, number int, head string, labels []string) (alert bool, findings int, reason string) {
	label := s.sentinelLabel()
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			alert = true
			break
		}
	}
	if head == "" || verifier == nil {
		return alert, 0, ""
	}
	b, err := github.LoadVerifiedReviewEvidence(github.ReviewEvidencePath("", repo, number, head), *verifier)
	if err != nil || b == nil || len(b.Sentinel) == 0 {
		return alert, 0, ""
	}
	findings = len(b.Sentinel)
	reason = strings.TrimSpace(b.Sentinel[0].Summary)
	if reason == "" {
		reason = strings.TrimSpace(b.Sentinel[0].Rule)
	}
	if findings > 1 {
		reason += fmt.Sprintf(stageTrackerSentinelMoreFormat, findings-1)
	}
	for _, f := range b.Sentinel {
		if f.Notice {
			alert = true
			break
		}
	}
	return alert, findings, reason
}
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
		case needsDecisionLabel, prNeedsDecisionLabel, "needs-decision":
			// The stage label is the park signal (hivecommons/hive#12683;
			// 045 on an issue, 085 on a PR); the retired spelling is read
			// until migrated.
			set[modNeedsDecision] = true
		case "needs-triage":
			// Still waiting on triage (reporter trust / maintainer look):
			// the Open row's action chip reads "triage needed".
			set[modNeedsTriage] = true
		case stagelabels.PossiblyExisting.Label():
			set[modPossibleDupe] = true
		case "needs-rebase":
			set[modNeedsRebase] = true
		case "blocked":
			set[modBlocked] = true
		case stagelabels.UnauthorizedLabel:
			set[modUnauthorized] = true
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
	for _, m := range []string{modStalled, modRegressed, modHold, modUnauthorized, modNeedsHuman, modNeedsDecision, modNeedsTriage, modPossibleDupe, modNeedsRebase, modBlocked, modDraft} {
		if set[m] {
			out = append(out, m)
		}
	}
	return out
}

// noVerdictClause is the bare no-verdict clause; stageTrackerCaption swaps
// in "awaiting review of head <sha7>" when the item's head is known.
const noVerdictClause = "no review verdict yet"

// shortHeadLen is the abbreviated SHA length the captions print.
const shortHeadLen = 7

func shortHead(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > shortHeadLen {
		return sha[:shortHeadLen]
	}
	return sha
}

// humanizeStageReason turns a lane reason token into a short clause with
// no leading capital and no trailing period, or "" when the token adds
// nothing to the stage description.
func humanizeStageReason(reason string) string {
	r := strings.TrimSpace(reason)
	if r == "" {
		return ""
	}
	if base, approver := stagelabels.HumanApprovedReason(r); approver != "" {
		clause := humanizeStageReason(base)
		if clause == "" {
			return "approved by @" + approver
		}
		return clause + " — approved by @" + approver
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
		return noVerdictClause
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
	if rest, ok := strings.CutPrefix(r, "base-branch-red:"); ok {
		return "waiting for the base branch's CI to recover (" + strings.ReplaceAll(rest, ",", ", ") + ") — not this PR's fault"
	}
	if rest, ok := strings.CutPrefix(r, stageengineprstage.StageReasonUnauthorizedPrefix); ok {
		return "the hive's GitHub App is not permitted to " + unauthorizedStepWords(rest) + "; a maintainer must"
	}
	if rest, ok := strings.CutPrefix(r, "fork-guard:"); ok {
		return "check " + strings.ReplaceAll(rest, ",", ", ") + " refuses fork PRs — a maintainer must push this branch to the repository"
	}
	if rest, ok := strings.CutPrefix(r, "shared-ci-incident:"); ok {
		return "deferred to shared-CI incident #" + rest + " — not this PR's fault"
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
		if left, ok := soakRemainingFromReason(r); ok {
			return "soaking, " + left.Round(time.Minute).String() + " left at the last sweep"
		}
		return "soaking"
	}
	if strings.HasPrefix(lower, "release-hold") {
		return "release hold"
	}
	if strings.HasPrefix(lower, "status-") || strings.HasPrefix(lower, "check-") {
		return "CI is red (" + r + ")"
	}
	switch lower {
	case stageengineprstage.ContributorLaneReasonWorkflowPath:
		return "workflow path"
	case stageengineprstage.ContributorLaneReasonTestsRemoved:
		return "tests removed"
	case stageengineprstage.AutoMergeReasonHumanMergePath:
		return "human-merge path"
	}
	if rest, ok := strings.CutPrefix(lower, "contributor-lane:"); ok {
		return strings.ReplaceAll(strings.ReplaceAll(rest, "-", " "), "_", " ")
	}
	return strings.NewReplacer("-", " ", "_", " ", ":", ": ").Replace(r)
}

// unauthorizedStepWords renders an `unauthorized:<step>` step as words.
func unauthorizedStepWords(step string) string {
	return strings.ReplaceAll(strings.TrimSpace(step), "-", " ")
}

// Tooltip for the unauthorized pill, per step; the generic form serves a
// label whose stage reason does not (or no longer) names the step.
const (
	stageTrackerUnauthorizedApproveRuns = "The hive's GitHub App lacks `actions: write`, so it cannot approve the workflow runs GitHub is holding for this head. A maintainer must click Approve and run on the PR's checks banner (or the Actions tab); see the hive:unauthorized comment for the run ids. Hive resumes, and clears the label, once the required checks report."
	stageTrackerUnauthorizedGeneric     = "The hive's GitHub App lacks a permission for the next step, so only a person can take it; the hive:unauthorized comment on the PR names the permission and the step. Hive resumes, and clears the label, once the step is done."
	stageTrackerUnauthorizedMergeWF     = "The hive's GitHub App lacks `workflows: write`, so it cannot merge this owner-approved PR that edits .github/workflows files. A maintainer must click Merge on GitHub (the approval already stands), or grant the App Workflows: Read and write so the hive can merge such PRs itself."
)

// stageTrackerUnauthorized derives the gate word and the tooltip for an
// item wearing hive:unauthorized from its stage reason.
func stageTrackerUnauthorized(reason string) (step, tip string) {
	base, _ := stagelabels.HumanApprovedReason(strings.TrimSpace(reason))
	rest, ok := strings.CutPrefix(base, stageengineprstage.StageReasonUnauthorizedPrefix)
	if !ok || strings.TrimSpace(rest) == "" {
		return "", stageTrackerUnauthorizedGeneric
	}
	step = unauthorizedStepWords(rest)
	if rest == stageengineprstage.UnauthorizedStepApproveWorkflowRuns {
		return step, stageTrackerUnauthorizedApproveRuns
	}
	if rest == stageengineprstage.UnauthorizedStepMergeWorkflowPath {
		return step, stageTrackerUnauthorizedMergeWF
	}
	return step, "The hive's GitHub App lacks the permission to " + step + ", so only a person can take that step; the hive:unauthorized comment on the PR names the permission and the action. Hive resumes, and clears the label, once the step is done."
}

// stageTrackerManualMergeReason reports whether a bare stage reason is a
// lane rule the hive never merges unattended, so a 090 under it is waiting
// on a maintainer's merge click rather than on /lgtm /approve.
func stageTrackerManualMergeReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case stageengineprstage.ContributorLaneReasonWorkflowPath, stageengineprstage.ContributorLaneReasonTestsRemoved, stageengineprstage.AutoMergeReasonHumanMergePath:
		return true
	}
	return false
}

// Gate-cell words and tooltips per prstage.HumanMergeReason* code: why a
// person, not the hive, merges a PR parked at 090 (hivecommons/hive#14658).
var stageTrackerHumanMergeReasonWords = map[string][2]string{
	stageengineprstage.HumanMergeReasonWorkflowFiles:     {"workflow files", "Changes .github/workflows/** (or action files) — the hive never merges workflow changes and its GitHub App lacks the workflows permission; click Merge on GitHub"},
	stageengineprstage.HumanMergeReasonTestsRemoved:      {"tests removed", "Deletes tests — the hive never merges a PR that removes tests unattended; a maintainer reviews the deletion and clicks Merge"},
	stageengineprstage.HumanMergeReasonManualMergePolicy: {"manual-merge policy", "Touches a path listed in auto_merge.human_merge_paths — the hive never merges it unattended; click Merge on GitHub"},
	stageengineprstage.HumanMergeReasonBranchProtection:  {"branch protection", "Branch protection or a ruleset requires an approving review the hive's GitHub App cannot give; approve on GitHub (or let the App bypass) and the hive merges, or click Merge"},
	stageengineprstage.HumanMergeReasonMergeMethod:       {"merge method", "The repository disallows the merge method this PR needs (a forward merge or merge-commit PR is never squashed); merge by hand with an allowed method or allow it in the repository settings"},
	stageengineprstage.HumanMergeReasonAutoMergeOff:      {"auto-merge off", "Auto-merge is switched off for this repository or lane, so every merge is a person's; click Merge on GitHub"},
	stageengineprstage.HumanMergeReasonUntrustedAuthor:   {"untrusted author", "No lane can vouch for the author (trust tier below require_tiers, no verified claim, or no trusted role), so the hive does not merge; a maintainer's /lgtm + /approve (or Merge) lands it"},
	stageengineprstage.HumanMergeReasonUnauthorized:      {"unauthorized", "The hive's GitHub App is not permitted to take the merge step (GitHub answered 403); a maintainer merges by hand — see the hive:unauthorized note on the PR"},
	stageengineprstage.HumanMergeReasonReviewerDeferred:  {"reviewer deferred", "The reviewer swarm returned requires_human on this head and a trusted merger approved it; the hive leaves the merge itself to a person"},
}

// stageTrackerHumanMergeReason derives, from a 090 stage reason, the
// compact code, the gate-cell word and the tooltip saying why a person
// must merge. An unrecognised lane reason falls back to the "policy" code
// with the raw reason in the tooltip; an empty reason yields "".
func stageTrackerHumanMergeReason(reason string) (code, word, tip string) {
	code = stageengineprstage.HumanMergeReason(reason)
	if code == "" {
		return "", "", ""
	}
	if w, ok := stageTrackerHumanMergeReasonWords[code]; ok {
		return code, w[0], w[1]
	}
	base, _ := stagelabels.HumanApprovedReason(reason)
	return stageengineprstage.HumanMergeReasonPolicy, "policy", "The merge lane will not merge this unattended (lane reason `" + strings.TrimSpace(base) + "`); a maintainer merges by hand"
}

// trackerSoakRemainingRE pulls the time left out of the lane's
// `contributor-lane:soaking(remaining=12m0s)` reason.
var trackerSoakRemainingRE = regexp.MustCompile(`remaining=([0-9hms.]+)`)

// soakRemainingFromReason parses the soak time left from a soaking reason.
func soakRemainingFromReason(reason string) (time.Duration, bool) {
	m := trackerSoakRemainingRE.FindStringSubmatch(reason)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(m[1])
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

// stageTrackerSoakRemaining is the soak time left now for a record at 095:
// the lane's recorded remaining, aged by the time since the sweep wrote it.
func stageTrackerSoakRemaining(rec stagelabels.Record, now time.Time) time.Duration {
	if rec.Stage != stagelabels.PreMergeSoak {
		return 0
	}
	left, ok := soakRemainingFromReason(rec.Reason)
	if !ok {
		return 0
	}
	if !rec.LastSeen.IsZero() {
		left -= now.Sub(rec.LastSeen)
	}
	if left < 0 {
		return 0
	}
	return left
}

// adoptedWorkCaption names what the hive is doing on a PR it adopted in
// place (stage 074) from the lane's reason token: the 074 label stays
// whether the head is conflicted, red or awaiting rework, so the caption is
// where "hive rebasing" vs "hive fixing CI" shows. Unknown reasons fall back
// to the bare ownership sentence.
func adoptedWorkCaption(reason string) string {
	const owned = "Hive adopted this PR"
	lower := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case lower == "":
		return owned
	case lower == "conflicting" || lower == "not-mergeable" || strings.Contains(lower, "rebase"):
		return owned + "; hive is rebasing it onto the base branch"
	case strings.HasPrefix(lower, "check-failing:") || strings.HasPrefix(lower, "required-check-failing:") ||
		strings.HasPrefix(lower, "optional-check-failing:") || lower == "ci-red" || lower == "check-runs":
		return owned + "; hive is fixing CI"
	case strings.HasPrefix(lower, "reviewer-verdict:") || strings.Contains(lower, "changes-requested") || strings.Contains(lower, "changes_requested"):
		return owned + "; hive is addressing the review findings"
	}
	return owned
}

func joinClause(head, clause string) string {
	if clause == "" {
		return head
	}
	return head + " — " + clause
}

// stageTrackerCaption builds the human sentence and the actor for an item.
func stageTrackerCaption(stage stagelabels.Stage, reason, author string, kind string, mirrored bool, rec stagelabels.Record, hasRec bool, linkedPR int, head string) (caption, actor string) {
	clause := humanizeStageReason(reason)
	if clause == noVerdictClause {
		if sha := shortHead(head); sha != "" {
			clause = "awaiting review of head " + sha
		}
	}
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
	case stagelabels.TriageNeeded:
		return joinClause("Actionable, but no maintainer has looked at it yet; waiting for triage", clause), "maintainer"
	case stagelabels.PossiblyExisting:
		return joinClause("Prior art found; the author must confirm the gap (`/hive decision proceed — <gap>`) or close it (`/hive decision exists`)", clause), strings.TrimPrefix(who, "@")
	case stagelabels.Claimed:
		return joinClause("Claimed; work is about to start", clause), "claimant"
	case stagelabels.PRInProgress:
		return joinClause("An agent has a PR under way; not yet opened", clause), "hive"
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
		c := adoptedWorkCaption(reason)
		if hasRec && rec.AdoptedBy > 0 {
			c += " (superseded by #" + strconv.Itoa(rec.AdoptedBy) + ")"
		}
		return joinClause(c, clause), "hive"
	case stagelabels.AwaitingMergeAdmission:
		return joinClause("Green and approved; waiting for the automerge lane to admit it", clause), "automerge lane"
	case stagelabels.NeedsDecision, stagelabels.IssueNeedsDecision:
		if step, _ := stageTrackerUnauthorized(reason); step != "" {
			// hivecommons/hive#14589: not a decision but a step only a
			// person may take; the gate cell says the same.
			return joinClause("Awaiting human: "+step, clause), "maintainer"
		}
		return joinClause("A human decision is owed", clause), "maintainer"
	case stagelabels.ReadyForHumanMerge:
		if base, approver := stagelabels.HumanApprovedReason(reason); stageTrackerManualMergeReason(base) {
			c := "Awaiting human merge (" + humanizeStageReason(base) + ")"
			if approver != "" {
				c += " — approved by @" + approver
			}
			return c + "; the hive never merges this itself: click Merge (or /lgtm + /approve for tide)", "maintainer"
		}
		return joinClause("Ready for a maintainer's /lgtm /approve; the hive cannot merge it itself", clause), "maintainer"
	case stagelabels.PreMergeSoak:
		return joinClause("Admitted by the automerge lane; the pre-merge soak window is running", clause), "automerge lane"
	case stagelabels.MergeQueue:
		return joinClause("In the merge queue; soak complete, waiting for tide or the merge window", clause), "tide"
	}
	return joinClause("No stage recorded yet", clause), ""
}

// stageTrackerRegression fills the item's regression narration from the
// record: the hop that opened the regression when History still has it,
// else the record's fall time and current stage.
func stageTrackerRegression(it *StageTrackerItem, rec stagelabels.Record) {
	if !rec.Regressed() {
		return
	}
	it.RegressedFrom = rec.RegressedFrom.Number()
	if hop, ok := rec.RegressionHop(); ok {
		it.RegressedTo = hop.Stage.Number()
		it.RegressedReason = hop.Reason
		at := hop.At
		it.RegressedAt = &at
		return
	}
	it.RegressedTo = rec.Stage.Number()
	it.RegressedReason = rec.Reason
	if !rec.RegressedAt.IsZero() {
		at := rec.RegressedAt
		it.RegressedAt = &at
	}
}

func stageTrackerHistory(rec stagelabels.Record) []StageTrackerHop {
	out := make([]StageTrackerHop, 0, len(rec.History))
	for _, h := range rec.History {
		out = append(out, StageTrackerHop{Number: h.Stage.Number(), Slug: h.Stage.Slug(), At: h.At, Reason: h.Reason, From: h.From.Number(), Regressed: h.Regressed})
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

// stageTrackerStaleDecisionRecord reports whether rec parks the item at
// 045/085 while its current labels carry no park signal at all — no stage
// label for the bay, no `needs-human`, no retired needs-decision spelling.
// The labels are the newer fact: the park lifted and the record is waiting
// on the engine's next reconcile.
func stageTrackerStaleDecisionRecord(rec stagelabels.Record, labels []string) bool {
	if rec.Stage != stagelabels.NeedsDecision && rec.Stage != stagelabels.IssueNeedsDecision {
		return false
	}
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case needsDecisionLabel, prNeedsDecisionLabel, "needs-decision", "needs-human", "needs-direction":
			return false
		}
	}
	return true
}

// buildStageTracker assembles the response from status and store at now.
func (s *Server) buildStageTracker(status *StatusPayload, store *stagelabels.Store, now time.Time) StageTrackerResponse {
	overrides := s.stageDwellOverrides()
	resp := StageTrackerResponse{GeneratedAt: now, Stages: stageTrackerStages(overrides), Items: []StageTrackerItem{}}
	if s.deps != nil && s.deps.StageEngineStatus != nil {
		st := s.deps.StageEngineStatus()
		resp.Engine = &st
	}
	resp.RecentlyMerged = s.stageTrackerRecentlyMerged(now, stageTrackerRepoQualifier(status))
	if status == nil {
		return resp
	}
	resp.HiveID = status.HiveID
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	resp.NeedsHumanLabels = needsHumanPresetLabels(cfg, status.HiveID)
	verifier, verifierErr := s.reviewEvidenceVerifier()
	var evidenceVerifier *github.ReviewEvidenceVerifier
	if verifierErr == nil {
		evidenceVerifier = &verifier
	}
	lookup := func(repo string, number int) (stagelabels.Record, bool) {
		if store == nil {
			return stagelabels.Record{}, false
		}
		return store.Get(stagelabels.Key(repo, number))
	}
	finish := func(it *StageTrackerItem, labels []string, stage stagelabels.Stage, rec stagelabels.Record, hasRec bool, draft bool) {
		// "Needs human decision" is read from the item's current labels, not
		// a store record that outlived them: once the park label is gone
		// (a `/hive decision` the engine has not re-derived yet) the row
		// must stop asking (hivecommons/docs#419).
		if hasRec && stageTrackerStaleDecisionRecord(rec, labels) {
			rec, hasRec = stagelabels.Record{}, false
		}
		// Likewise a 015 record on an issue whose labels already say
		// triage was accepted (the row menu's Accept triage): 010 until the
		// engine re-derives it.
		if hasRec && s.stageTrackerAcceptedTriageRecord(rec, labels) {
			rec, hasRec = stagelabels.Record{}, false
			if stage == stagelabels.None || stage == stagelabels.TriageNeeded {
				stage = stagelabels.Actionable
			}
		}
		if hasRec && rec.Stage != stagelabels.None {
			stage = rec.Stage
			t := rec.EnteredAt
			it.StageSince = &t
			it.Reason = rec.Reason
			stageTrackerRegression(it, rec)
			it.NudgeCount = rec.NudgeCount
			if !rec.LastNudgeAt.IsZero() {
				t2 := rec.LastNudgeAt
				it.LastNudgeAt = &t2
			}
			it.AdoptedBy = rec.AdoptedBy
			it.SoakRemainingSeconds = int64(stageTrackerSoakRemaining(rec, now) / time.Second)
		}
		it.Stage = stage.Number()
		it.StageSlug = stage.Slug()
		it.StageLabel = stage.Label()
		if m := trackerPRNumberRE.FindStringSubmatch(it.Reason); m != nil && it.Kind == "issue" {
			it.LinkedPR, _ = strconv.Atoi(m[1])
		}
		// An issue wearing a PR stage (050+) is riding its covering PR; its
		// own parking bay 045 is not a mirror.
		it.Mirrored = it.Kind == "issue" && stage != stagelabels.None && !stage.Applies(stagelabels.IssueItem)
		it.Modifiers = stageTrackerModifiers(labels, status.HiveID, rec, hasRec, overrides, now, draft)
		// 045/085 have their own chevron word ("Needs human decision"); the
		// chip only adds information when needs-decision modifies another
		// stage.
		if stage == stagelabels.NeedsDecision || stage == stagelabels.IssueNeedsDecision {
			it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsDecision })
		}
		// Likewise 015 says "Triage needed" itself, and a needs-triage left
		// on a claimed / covered issue is stale, not a signal: 015 is not a
		// gate, the stage word is the truth, and the reconciler removes the
		// label on its next pass (hivecommons/hive#14586). The chip stays
		// only before the claim (010 / 017 / 045).
		if stage == stagelabels.TriageNeeded || stage.SettlesTriage() {
			if stage != stagelabels.TriageNeeded && slices.Contains(it.Modifiers, modNeedsTriage) {
				it.StaleLabels = append(it.StaleLabels, modNeedsTriage)
			}
			it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsTriage })
		}
		it.Caption, it.Actor = stageTrackerCaption(stage, it.Reason, it.Author, it.Kind, it.Mirrored, rec, hasRec, it.LinkedPR, it.Head)
		base, approver := stagelabels.HumanApprovedReason(it.Reason)
		it.HumanApprover = approver
		it.ManualMerge = stageTrackerManualMergeReason(base)
		if stage == stagelabels.ReadyForHumanMerge {
			it.HumanMergeReason, _, it.HumanMergeReasonText = stageTrackerHumanMergeReason(it.Reason)
		}
		if slices.Contains(it.Modifiers, modUnauthorized) {
			it.UnauthorizedStep, it.Unauthorized = stageTrackerUnauthorized(it.Reason)
		}
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
	addIssue := func(repoName string, issue github.Issue, bucket, bucketReason string, progress *github.IssueChildren) {
		// A closed issue has no stage; a stage label it closed wearing is
		// the closed-issue sweep's to remove, not a row to render
		// (hivecommons/homebrew-hive#127).
		if strings.EqualFold(issue.State, "closed") {
			return
		}
		issue.Repo = nonEmpty(issue.Repo, repoName)
		rec, ok := lookup(repoName, issue.Number)
		it := StageTrackerItem{Repo: repoName, Number: issue.Number, Kind: "issue", Title: issue.Title, Author: issue.Author, Lane: laneIssue,
			Labels: append([]string(nil), issue.Labels...), CreatedAt: timePtr(issue.CreatedAt), UpdatedAt: timePtr(issue.UpdatedAt),
			Bucket: bucket, BucketReason: bucketReason,
			URL: overviewItemURL(issue.URL, issue.Repo, issue.Number, "issue", status.GitHubBaseURL)}
		it.Children = issue.Children
		if it.Children == nil {
			it.Children = progress
		}
		if it.Children == nil {
			it.Children = github.TrackerTaskProgress(issue.Body)
		}
		it.IsTracker = github.RendersAsTracker(issue.IsTracker, issue.Labels, it.Children)
		it.ClaimReference = issue.ClaimContext != nil && !issue.ClaimContext.MergedPR && issue.ClaimContext.Reference && issue.ClaimContext.Decision != github.ClaimDecisionReleasedRedStalePR
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
		// A held issue is "Open · on hold" whatever stage label or store
		// record the hold caught it with; the reconciler drops both on its
		// next cycle (hivecommons/hive#13012).
		if len(holdLabels(issue.Labels, status.HiveID)) > 0 {
			stage, rec, ok = stagelabels.None, stagelabels.Record{}, false
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
		if ok && rec.Superseded() {
			// hivecommons/hive#14471: the hive's adoption of this PR merged
			// and the original was told. Closed or not (the App may lack
			// permission on a fork PR), it is finished work, not a row.
			return
		}
		it := StageTrackerItem{Repo: repoName, Number: pr.Number, Kind: "pr", Title: pr.Title, Author: pr.Author, Head: pr.HeadSHA,
			Labels: append([]string(nil), pr.Labels...), CreatedAt: timePtr(pr.CreatedAt), UpdatedAt: timePtr(pr.UpdatedAt),
			Bucket: bucket, BucketReason: bucketReason,
			URL: overviewItemURL(pr.URL, pr.Repo, pr.Number, "pr", status.GitHubBaseURL)}
		it.BaseRef = pr.BaseRef
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
		it.SentinelAlert, it.SentinelFindings, it.SentinelReason = s.stageTrackerSentinel(evidenceVerifier, repoName, pr.Number, pr.HeadSHA, pr.Labels)
		it.LinkedIssues = stageTrackerPRLinkedIssues(pr, repoName, status.GitHubBaseURL)
		add(it)
	}
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		// The repo's canonical open-items set (open_items.go): the same
		// items, deduplicated the same way, that the Overview tile and its
		// sidebar pill count, so the tracker's unfiltered total is the
		// tile's issues + PRs by construction.
		for _, it := range repoOpenItems(repo) {
			switch {
			case it.Kind == "issue" && !it.Sidelined:
				addIssue(repoName, it.Issue, "", "", nil)
			case it.Kind == "pr" && !it.Sidelined:
				addPR(repoName, it.PR.pr, it.PR.verdict, "", "")
			case it.Kind == "issue":
				// The enumeration's sidelined issues: parked in a work-detail
				// bucket, the open items the first tracker never showed.
				w := it.WorkIssue
				addIssue(repoName, github.Issue{Repo: w.Repo, Number: w.Number, Title: w.Title, Author: w.Author, Labels: w.Labels, Assignees: w.Assignees,
					CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL, ClaimKind: w.ClaimKind, ClaimedBy: w.ClaimedBy, LinkedPRs: w.LinkedPRs, ClaimContext: w.ClaimContext, IsTracker: w.IsTracker, Children: w.Children}, w.Bucket, w.Reason, w.TrackerProgress)
			default:
				w := it.WorkPR
				addPR(repoName, github.PullRequest{Repo: w.Repo, Number: w.Number, Title: w.Title, Author: w.Author, Labels: w.Labels, Draft: w.Draft,
					AppAuthored: w.AppAuthored, HiveAttributed: w.HiveAttributed, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL, BaseRef: w.BaseRef, LinkedIssues: w.LinkedIssues, ReferencedIssues: w.ReferencedIssues}, nil, w.Bucket, w.Reason)
			}
		}
	}
	resp.OpenIssues, resp.OpenPRs = stageTrackerOpenCounts(resp.Items)
	stageTrackerResolveLinkedPRs(resp.Items)
	sort.SliceStable(resp.Items, stageTrackerLess(resp.Items))
	return resp
}

// Ordering tiers within a stage (stageTrackerAttentionTier; the dashboard's
// STAGE_TRACKER_TIER_* carry the same values).
const (
	stageTrackerTierAttention = 0
	stageTrackerTierNormal    = 1
	stageTrackerTierSunk      = 2
)

// stageTrackerAttentionStages are the stages that park an item on a person
// (dashboard STAGE_TRACKER_ATTENTION_STAGES): issue needs-decision, PR
// needs-author / returned-to-contributor, PR needs-decision and
// ready-for-human-merge.
var stageTrackerAttentionStages = []int{stagelabels.IssueNeedsDecision.Number(), stagelabels.NeedsAuthor.Number(), stagelabels.ReturnedToContributor.Number(), stagelabels.NeedsDecision.Number(), stagelabels.ReadyForHumanMerge.Number()}

// stageTrackerHumanMods are the modifiers that park an item on a person
// (dashboard STAGE_TRACKER_HUMAN_MODS).
var stageTrackerHumanMods = []string{modHold, modUnauthorized, modNeedsHuman, modNeedsDecision, modNeedsRebase, modBlocked, modStalled}

// stageTrackerSunkMods are the modifiers that sink a row to the bottom of
// its stage (dashboard STAGE_TRACKER_SUNK_MODS): a person parked it, or it
// is a draft nobody should act on yet.
var stageTrackerSunkMods = []string{modHold, modDraft}

// stageTrackerRegressionHandled mirrors the dashboard's
// stageTrackerRegressionHandled: a regressed item with no human-gate
// modifier on a hive-driven stage is the hive's to work through.
func stageTrackerRegressionHandled(it StageTrackerItem) bool {
	if !slices.Contains(it.Modifiers, modRegressed) {
		return false
	}
	if slices.ContainsFunc(it.Modifiers, func(m string) bool { return slices.Contains(stageTrackerHumanMods, m) }) {
		return false
	}
	return !slices.Contains(stageTrackerAttentionStages, it.Stage)
}

// stageTrackerNeedsAttention mirrors the dashboard's
// stageTrackerNeedsAttention: the item shows a pill or sits on a stage
// parked on a person. Only the shown Modifiers count, so a merge-gate pill
// held back until the gate never promotes a row.
func stageTrackerNeedsAttention(it StageTrackerItem) bool {
	if stageTrackerRegressionHandled(it) {
		return false
	}
	return len(it.Modifiers) > 0 || slices.Contains(stageTrackerAttentionStages, it.Stage)
}

// stageTrackerAttentionTier is the item's ordering tier within its stage
// (the dashboard's stageTrackerAttentionTier agrees): rows parked on a
// person first, then the rest, then rows nobody should act on — hold
// (incl. do-not-merge/*) and draft. The direction toggle never flips it.
func stageTrackerAttentionTier(it StageTrackerItem) int {
	if slices.ContainsFunc(it.Modifiers, func(m string) bool { return slices.Contains(stageTrackerSunkMods, m) }) {
		return stageTrackerTierSunk
	}
	if stageTrackerNeedsAttention(it) {
		return stageTrackerTierAttention
	}
	return stageTrackerTierNormal
}

// stageTrackerLess orders the payload: stage descending, then
// attention-first within the stage (stageTrackerAttentionTier), then the
// item that has waited longest, then repo and number — the dashboard's
// default view, so chat and hub consumers read the same order.
func stageTrackerLess(items []StageTrackerItem) func(i, j int) bool {
	return func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Stage != b.Stage {
			return a.Stage > b.Stage
		}
		if ta, tb := stageTrackerAttentionTier(a), stageTrackerAttentionTier(b); ta != tb {
			return ta < tb
		}
		// Within a tier the item that has waited longest comes first: it is
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
	}
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

// stageTrackerOpenCounts counts the tracker's items by kind.
func stageTrackerOpenCounts(items []StageTrackerItem) (issues, prs int) {
	for _, it := range items {
		if it.Kind == "pr" {
			prs++
		} else {
			issues++
		}
	}
	return issues, prs
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
// accounting (/api/gh-rate-limits). Serving the tracker is Charged 0; the
// only GitHub reads are the memoised issues GETs that title untitled links
// (stageTrackerEnrichSoftLinks).
const stageTrackerRESTCaller = "dashboard:stage-tracker"

// stageTrackerCache is the memoised payload: the encoded body, its ETag and
// when it was built.
type stageTrackerCache struct {
	body        []byte
	etag        string
	generatedAt time.Time
	// stale is set by invalidateStageTrackerCache: the body is still served
	// while the next request rebuilds, so an invalidation never blocks and a
	// reader never waits behind a build it did not start.
	stale bool
}

// stageTrackerBuild is the single in-flight /api/stage-tracker rebuild. done
// is closed when the cache has been updated (or the build failed).
type stageTrackerBuild struct {
	done chan struct{}
}

// stageTrackerSource values for the X-Hive-Stage-Tracker-Source header and
// the payload's source field.
const (
	stageTrackerSourceFreshName = "fresh"
	stageTrackerSourceCacheName = "cache"
	// stageTrackerSourceStaleName marks a body served from the cache while a
	// rebuild is in flight or after an invalidation: still the last complete
	// tracker, just older than the status it will be rebuilt from.
	stageTrackerSourceStaleName = "stale"
)

func (s *Server) stageTrackerCacheTTL() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerCacheTTL()
	}
	return (*config.Config)(nil).StageTrackerCacheTTL()
}

// stageTrackerPayload returns the cached body when it is younger than the
// TTL and not invalidated, else builds, encodes and caches a new one. fresh
// reports a rebuild by this call.
//
// The build runs OUTSIDE stageTrackerCacheMu and at most one runs at a time:
// the mutex only guards the cached fields, so invalidateStageTrackerCache
// (every webhook, every status publish) returns at once instead of queueing
// behind a build that walks the evidence tree on NFS. Requests that arrive
// while a build is in flight are served the previous body as "stale"; only a
// request with no body to fall back on waits, and only until the in-flight
// build finishes or its own context ends. Before this, the mutex was held
// across the build, so every webhook handler, the governor's publish and
// every tracker poll queued behind it in FIFO order — and because each
// publish and webhook re-invalidated the cache, the queue never drained
// (hivecommons/hive#14681: unbounded blocked handlers, connections and
// threads).
func (s *Server) stageTrackerPayload(status *StatusPayload, now time.Time) (body []byte, etag string, generatedAt time.Time, fresh bool) {
	var source string
	body, etag, generatedAt, source = s.stageTrackerPayloadSource(context.Background(), status, now)
	return body, etag, generatedAt, source == stageTrackerSourceFreshName
}

// stageTrackerPayloadSource is stageTrackerPayload reporting where the body
// came from (stageTrackerSource*Name; "" with a nil body when the build
// failed or ctx ended while waiting on an in-flight build).
func (s *Server) stageTrackerPayloadSource(ctx context.Context, status *StatusPayload, now time.Time) (body []byte, etag string, generatedAt time.Time, source string) {
	ttl := s.stageTrackerCacheTTL()
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerServed = true
	c := s.stageTrackerCache
	if len(c.body) > 0 && !c.stale && now.Sub(c.generatedAt) < ttl {
		s.stageTrackerCacheMu.Unlock()
		return c.body, c.etag, c.generatedAt, stageTrackerSourceCacheName
	}
	if inflight := s.stageTrackerBuild; inflight != nil {
		s.stageTrackerCacheMu.Unlock()
		if len(c.body) > 0 {
			return c.body, c.etag, c.generatedAt, stageTrackerSourceStaleName
		}
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return nil, "", now, ""
		}
		s.stageTrackerCacheMu.Lock()
		c = s.stageTrackerCache
		s.stageTrackerCacheMu.Unlock()
		if len(c.body) == 0 {
			return nil, "", now, ""
		}
		return c.body, c.etag, c.generatedAt, stageTrackerSourceCacheName
	}
	inflight := &stageTrackerBuild{done: make(chan struct{})}
	s.stageTrackerBuild = inflight
	gen := s.stageTrackerGen
	s.stageTrackerCacheMu.Unlock()

	built, ok := s.encodeStageTracker(status, now, ttl)

	s.stageTrackerCacheMu.Lock()
	if ok {
		// An invalidation that landed during the build means newer status
		// exists; keep serving this body but let the next request rebuild.
		built.stale = s.stageTrackerGen != gen
		s.stageTrackerCache = built
	}
	s.stageTrackerBuild = nil
	close(inflight.done)
	s.stageTrackerCacheMu.Unlock()
	if !ok {
		return nil, "", now, ""
	}
	return built.body, built.etag, built.generatedAt, stageTrackerSourceFreshName
}

// encodeStageTracker builds and encodes the tracker for status. It holds no
// server lock: this is the slow part (one evidence read per PR, on NFS in
// hosted hives).
func (s *Server) encodeStageTracker(status *StatusPayload, now time.Time, ttl time.Duration) (stageTrackerCache, bool) {
	build := s.buildStageTracker
	if s.stageTrackerBuildFn != nil {
		build = s.stageTrackerBuildFn
	}
	resp := build(status, s.stageStoreOrShared(), now)
	s.stageTrackerEnrichSoftLinks(resp.Items, stageTrackerRepoQualifier(status), now)
	resp.CacheTTLSeconds = int64(ttl / time.Second)
	// The encoded body carries source=cache: that is what every later hit
	// serves verbatim, and what the ETag validates. The building request
	// alone rewrites the marker to fresh on its way out (handleStageTracker).
	resp.Source = stageTrackerSourceCacheName
	// The ETag hashes the body with generated_at zeroed: the timestamp moves
	// on every rebuild, so including it would turn an unchanged tracker into
	// a 200 each time (the client revalidates with this ETag and gets a 304).
	var hashBuf bytes.Buffer
	hashResp := resp
	hashResp.GeneratedAt = time.Time{}
	if err := json.NewEncoder(&hashBuf).Encode(hashResp); err != nil {
		slogWarnOverviewEncode(err)
		return stageTrackerCache{}, false
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		slogWarnOverviewEncode(err)
		return stageTrackerCache{}, false
	}
	return stageTrackerCache{body: buf.Bytes(), etag: webstatic.ETagFor(hashBuf.Bytes()), generatedAt: now}, true
}

// invalidateStageTrackerCache marks the memoised payload stale.
// UpdateStatusIfFresh calls it on every status publish, and every webhook
// re-read that changed a row calls it too, so tracker freshness follows the
// eval cycle and the webhook stream rather than the cache TTL alone. It only
// flips a flag under the cache mutex — it never waits for a build in
// progress — so callers on the webhook and publish paths return at once. It
// reports whether the tracker has ever been served: only then is an SSE
// nudge worth sending, which also keeps the /api/events stream free of extra
// frames for clients that never asked for the tracker.
func (s *Server) invalidateStageTrackerCache() (served bool) {
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerCache.stale = true
	s.stageTrackerGen++
	served = s.stageTrackerServed
	s.stageTrackerCacheMu.Unlock()
	return served
}

// stageTrackerSSEFrame is the `stage-tracker` event /api/events sends after
// each status publish; the SPA refetches /api/stage-tracker on it instead of
// waiting for its poll. seq is the status sequence the tracker now reflects.
func stageTrackerSSEFrame(seq uint64) string {
	return "event: stage-tracker\ndata: {\"seq\":" + strconv.FormatUint(seq, 10) + "}\n\n"
}

var stageTrackerSourceCache = []byte(`"source":"cache"`)
var stageTrackerSourceFresh = []byte(`"source":"fresh"`)
var stageTrackerSourceStale = []byte(`"source":"stale"`)

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
	body, etag, generatedAt, source := s.stageTrackerPayloadSource(r.Context(), status, now)
	if body == nil {
		if r.Context().Err() != nil {
			jsonError(w, "stage tracker build still in progress", http.StatusServiceUnavailable)
			return
		}
		jsonError(w, "stage tracker unavailable", http.StatusInternalServerError)
		return
	}
	remaining := s.stageTrackerCacheTTL() - now.Sub(generatedAt)
	if remaining < 0 || source == stageTrackerSourceStaleName {
		remaining = 0
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age="+strconv.FormatInt(int64(remaining/time.Second), 10))
	w.Header().Set("X-Hive-Stage-Tracker-Source", source)
	w.Header().Set("X-Hive-Stage-Tracker-Generated-At", generatedAt.UTC().Format(time.RFC3339Nano))
	w.Header().Set("Last-Modified", generatedAt.UTC().Format(http.TimeFormat))
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		github.RecordLocalServe(stageTrackerRESTCaller, r.URL.Path, http.StatusNotModified)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	github.RecordLocalServe(stageTrackerRESTCaller, r.URL.Path, http.StatusOK)
	switch source {
	case stageTrackerSourceFreshName:
		body = bytes.Replace(body, stageTrackerSourceCache, stageTrackerSourceFresh, 1)
	case stageTrackerSourceStaleName:
		body = bytes.Replace(body, stageTrackerSourceCache, stageTrackerSourceStale, 1)
	}
	if _, err := w.Write(body); err != nil {
		slogWarnOverviewEncode(err)
	}
}

// stageTrackerPRLinkedIssues projects the issues pr links onto the tracker:
// GitHub's closing references first, then the references the PR's own text
// yields (github.PRReferencedIssues), in their priority order. Same-repo
// references only (a bare `#N`, or an explicit owner/repo#N naming the PR's
// own repo), never the PR's own number, each once at its strongest tier. A
// closing keyword hive read itself (PRLinkViaFixes) is a closing link here
// and carries no Via; the softer tiers keep theirs so the UI can mark them.
// Mention-tier entries are provisional: stageTrackerResolveLinkedPRs drops
// them unless the number is an issue listed in the response.
//
// repoName is the PR's repository as owner/name. pr.Repo may be the config's
// short name ("hive") while GitHub's closing references carry the full one
// ("hivecommons/hive"): the two name the same repository, so a titled
// closing link is kept — not dropped for the untitled text reference that
// rendered "(title unavailable)" — and the fallback URL is built from the
// full name, never from the short one.
func stageTrackerPRLinkedIssues(pr github.PullRequest, repoName, baseURL string) []StageTrackerLinkedIssue {
	var out []StageTrackerLinkedIssue
	seen := map[int]bool{}
	full := nonEmpty(strings.TrimSpace(repoName), pr.Repo)
	add := func(li github.PRLinkedIssue, via string) {
		if li.Number <= 0 || li.Number == pr.Number || seen[li.Number] {
			return
		}
		// Compare against the fully-qualified name when one is known so a
		// same-named repository under another owner is still foreign.
		if li.Repo != "" && !stageTrackerSameRepo(li.Repo, full) {
			return
		}
		seen[li.Number] = true
		out = append(out, StageTrackerLinkedIssue{
			Number: li.Number, Title: li.Title, State: strings.ToLower(strings.TrimSpace(li.State)),
			StateReason: strings.ToLower(strings.TrimSpace(li.StateReason)),
			URL:         overviewItemURL(li.URL, full, li.Number, "issue", baseURL), Via: via,
		})
	}
	for _, li := range pr.LinkedIssues {
		add(li, "")
	}
	for _, li := range pr.ReferencedIssues {
		via := li.Via
		if via == github.PRLinkViaFixes {
			via = ""
		}
		add(li, via)
	}
	return out
}

// stageTrackerSameRepo reports whether two repository names denote the
// same repository: equal ignoring case, or one is the bare name of the
// other's owner/name form.
func stageTrackerSameRepo(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	shortA, shortB := a, b
	if i := strings.LastIndex(a, "/"); i >= 0 {
		shortA = a[i+1:]
	}
	if i := strings.LastIndex(b, "/"); i >= 0 {
		shortB = b[i+1:]
	}
	// Both fully qualified but different owners: different repositories.
	if strings.Contains(a, "/") && strings.Contains(b, "/") {
		return false
	}
	return strings.EqualFold(shortA, shortB)
}

// stageTrackerLinkSoft reports whether a linked issue is a reference link
// rather than a closing one.
func stageTrackerLinkSoft(li StageTrackerLinkedIssue) bool {
	return li.Via != "" && li.Via != github.PRLinkViaFixes
}

// Bounds on the issues GETs stageTrackerEnrichSoftLinks makes. A lookup is
// memoised per Server for stageTrackerLinkTTL (a failed one for
// stageTrackerLinkRetry), because the tracker payload itself is rebuilt on
// every status publish. A rebuild makes at most stageTrackerLinkLookups GETs,
// all within stageTrackerLinkTimeout, as it holds stageTrackerCacheMu.
const (
	stageTrackerLinkTTL     = 15 * time.Minute
	stageTrackerLinkRetry   = 2 * time.Minute
	stageTrackerLinkLookups = 10
	stageTrackerLinkTimeout = 3 * time.Second
	stageTrackerLinkMemoMax = 1024
)

// stageTrackerLinkInfo is one memoised lookup: the issue's title and state,
// or pr when the number names a pull request; ok is false for a failed GET.
type stageTrackerLinkInfo struct {
	title, state string
	pr, ok       bool
	at           time.Time
}

// stageTrackerLinkMemo memoises link lookups keyed by owner/repo#number.
type stageTrackerLinkMemo struct {
	mu sync.Mutex
	m  map[string]stageTrackerLinkInfo
}

func (m *stageTrackerLinkMemo) get(key string, now time.Time) (stageTrackerLinkInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.m[key]
	if ok && now.Sub(in.at) < in.ttl() {
		return in, true
	}
	return stageTrackerLinkInfo{}, false
}

func (m *stageTrackerLinkMemo) put(key string, in stageTrackerLinkInfo, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]stageTrackerLinkInfo{}
	}
	if len(m.m) >= stageTrackerLinkMemoMax {
		for k, v := range m.m {
			if now.Sub(v.at) >= v.ttl() {
				delete(m.m, k)
			}
		}
	}
	in.at = now
	m.m[key] = in
}

func (in stageTrackerLinkInfo) ttl() time.Duration {
	if in.ok {
		return stageTrackerLinkTTL
	}
	return stageTrackerLinkRetry
}

// stageTrackerEnrichSoftLinks gives the links no listed issue or closing
// reference titled (soft refs / title / mention links, and closing keywords
// hive read from the PR text) a title and state, from one issues GET per
// distinct same-repo number, memoised across rebuilds (stageTrackerLinkMemo).
// A number that turns out to be a pull request, open or closed (`Supersedes
// #N`, an adoption's "close #N"), is dropped: it is not an issue the PR
// covers (hivecommons/hive#14667). A failed or skipped lookup leaves the link
// as it was.
func (s *Server) stageTrackerEnrichSoftLinks(items []StageTrackerItem, qualify func(string) string, now time.Time) {
	if s == nil || s.deps == nil || s.deps.GHClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(github.WithRESTCaller(context.Background(), stageTrackerRESTCaller), stageTrackerLinkTimeout)
	defer cancel()
	lookups := 0
	for i := range items {
		pr := &items[i]
		if pr.Kind != "pr" || len(pr.LinkedIssues) == 0 {
			continue
		}
		owner, name, ok := strings.Cut(qualify(pr.Repo), "/")
		if !ok {
			continue
		}
		kept := pr.LinkedIssues[:0]
		for _, li := range pr.LinkedIssues {
			if !li.Listed && li.Title == "" && li.State == "" {
				key := owner + "/" + name + "#" + strconv.Itoa(li.Number)
				in, hit := s.stageTrackerLinks.get(key, now)
				if !hit && lookups < stageTrackerLinkLookups && ctx.Err() == nil {
					lookups++
					title, state, isPR, err := s.deps.GHClient.IssueTitleState(ctx, owner, name, li.Number)
					in = stageTrackerLinkInfo{title: title, state: state, pr: isPR, ok: err == nil}
					// Running out of this rebuild's budget says nothing
					// about the issue: leave it for the next rebuild.
					if err == nil || ctx.Err() == nil {
						s.stageTrackerLinks.put(key, in, now)
					}
				}
				if in.pr {
					continue
				}
				if in.ok {
					li.Title, li.State = in.title, in.state
				}
			}
			kept = append(kept, li)
		}
		if len(kept) == 0 {
			kept = nil
		}
		pr.LinkedIssues = kept
	}
}

// stageTrackerPruneSoftLinks drops, from a PR's links, every soft link whose
// number is an open PR in the same repo (a squash title's `(#N)` or a
// `Refs #N` naming the PR this one follows) and every bare mention that is
// not an issue listed in the response — a keyword-less `#N` has no other
// way to prove it names an issue. A closed PR is not in the response;
// stageTrackerEnrichSoftLinks drops links to those.
func stageTrackerPruneSoftLinks(pr *StageTrackerItem, prs, issues map[string]*StageTrackerItem) {
	if len(pr.LinkedIssues) == 0 {
		return
	}
	kept := pr.LinkedIssues[:0]
	for _, li := range pr.LinkedIssues {
		if stageTrackerLinkSoft(li) {
			key := pr.Repo + "#" + strconv.Itoa(li.Number)
			if prs[key] != nil || (li.Via == github.PRLinkViaMention && issues[key] == nil) {
				continue
			}
		}
		kept = append(kept, li)
	}
	if len(kept) == 0 {
		kept = nil
	}
	pr.LinkedIssues = kept
}

// stageTrackerResolveLinkedPRs fills each issue's linked PR entries from the
// PR items in the same response (title, author, stage, since, modifiers),
// puts the PR the stage reason names first, drops duplicates and marks each
// such PR as nested under the lowest-numbered issue that lists it. The PR
// side is read too: a PR whose closing references name an open issue in
// the response is nested under that issue even when the issue's own claim
// evidence has not caught up with it, so the pair draws as one row.
func stageTrackerResolveLinkedPRs(items []StageTrackerItem) {
	prs := map[string]*StageTrackerItem{}
	issues := map[string]*StageTrackerItem{}
	for i := range items {
		key := items[i].Repo + "#" + strconv.Itoa(items[i].Number)
		switch items[i].Kind {
		case "pr":
			prs[key] = &items[i]
		case "issue":
			issues[key] = &items[i]
		}
	}
	for i := range items {
		pr := &items[i]
		if pr.Kind != "pr" {
			continue
		}
		stageTrackerPruneSoftLinks(pr, prs, issues)
		for j := range pr.LinkedIssues {
			li := &pr.LinkedIssues[j]
			issue := issues[pr.Repo+"#"+strconv.Itoa(li.Number)]
			if issue == nil {
				continue
			}
			li.Listed, li.State, li.Title = true, "open", issue.Title
			if li.URL == "" {
				li.URL = issue.URL
			}
			if issue.LinkedPR != pr.Number && !slices.ContainsFunc(issue.LinkedPRs, func(lp StageTrackerLinkedPR) bool { return lp.Number == pr.Number }) {
				issue.LinkedPRs = append(issue.LinkedPRs, StageTrackerLinkedPR{Number: pr.Number, URL: pr.URL})
			}
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
				lp.SentinelAlert, lp.SentinelReason, lp.Head = pr.SentinelAlert, pr.SentinelReason, pr.Head
				lp.HumanApprover, lp.ManualMerge = pr.HumanApprover, pr.ManualMerge
				lp.HumanMergeReason, lp.HumanMergeReasonText = pr.HumanMergeReason, pr.HumanMergeReasonText
				lp.BaseRef = pr.BaseRef
				if len(pr.Modifiers) > 0 {
					lp.Modifiers = append([]string(nil), pr.Modifiers...)
				}
				if lp.URL == "" {
					lp.URL = pr.URL
				}
				if pr.NestedUnder == 0 || it.Number < pr.NestedUnder {
					pr.NestedUnder = it.Number
				}
				// The PR's issue slot names the issue that nests it.
				if k := slices.IndexFunc(pr.LinkedIssues, func(li StageTrackerLinkedIssue) bool { return li.Number == it.Number }); k >= 0 {
					pr.LinkedIssues[k].Listed, pr.LinkedIssues[k].State, pr.LinkedIssues[k].Title = true, "open", it.Title
				} else {
					pr.LinkedIssues = append(pr.LinkedIssues, StageTrackerLinkedIssue{Number: it.Number, Title: it.Title, URL: it.URL, State: "open", Listed: true})
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
	for i := range items {
		if items[i].Kind == "pr" {
			items[i].ClosedIssue = stageTrackerClosedIssueAnchor(items[i])
		}
	}
}

// stageTrackerClosedIssueAnchor returns the closed issue a standalone PR
// row re-anchors around, or nil. A PR nested under a listed issue, a PR
// with an open closing link, and a PR whose links are all soft (`Refs #N`)
// or of unknown state keep the issue slot as is; only a PR whose every
// closing link names a closed issue is drawn as the row's own headline,
// with the first closed issue as its secondary chip
// (hivecommons/hive#14655). The PR's stage, modifiers and timestamps are
// untouched: the issue closing is not a regression of the PR.
func stageTrackerClosedIssueAnchor(pr StageTrackerItem) *StageTrackerLinkedIssue {
	if pr.Kind != "pr" || pr.NestedUnder != 0 {
		return nil
	}
	var closed *StageTrackerLinkedIssue
	for i := range pr.LinkedIssues {
		li := &pr.LinkedIssues[i]
		if li.Listed || stageTrackerLinkSoft(*li) {
			continue
		}
		switch li.State {
		case "closed":
			if closed == nil {
				closed = li
			}
		default:
			// An open (or unknown-state) closing link still anchors the row
			// on the issue.
			return nil
		}
	}
	if closed == nil {
		return nil
	}
	out := *closed
	return &out
}
