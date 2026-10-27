package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
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
	SentinelAlert    bool       `json:"sentinel_alert,omitempty"`
	SentinelFindings int        `json:"sentinel_findings,omitempty"`
	SentinelReason   string     `json:"sentinel_reason,omitempty"`
	NudgeCount       int        `json:"nudge_count,omitempty"`
	LastNudgeAt      *time.Time `json:"last_nudge_at,omitempty"`
	AdoptedBy        int        `json:"adopted_by,omitempty"`
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
	History      []StageTrackerHop         `json:"history"`
}

// StageTrackerLinkedIssue is one issue a PR closes, as its row's issue
// slot needs it. State is "open" / "closed" when known, "" otherwise.
type StageTrackerLinkedIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	State  string `json:"state,omitempty"`
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
	Head           string `json:"head,omitempty"`
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

// stageTrackerSentinel reports whether the sentinel flagged repo#number at
// head: the PR wears the configured alert label (the sweep's blocking
// signal, read from the live labels so a webhook label patch flips it at
// once) or the verified review evidence bundle for head records sentinel
// findings (how a trusted author's notice-only finding is seen, since those
// carry a comment rather than the label). The findings count and the first
// finding's summary come from the bundle; both are empty when only the
// label says so. One file read per PR with a known head, ENOENT when the
// sentinel never wrote one; the tracker payload is memoised so this runs
// once per snapshot, not per request.
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
	return true, findings, reason
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
	for _, m := range []string{modStalled, modRegressed, modHold, modNeedsHuman, modNeedsDecision, modNeedsTriage, modPossibleDupe, modNeedsRebase, modBlocked, modDraft} {
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
	if rest, ok := strings.CutPrefix(lower, "contributor-lane:"); ok {
		return strings.ReplaceAll(strings.ReplaceAll(rest, "-", " "), "_", " ")
	}
	return strings.NewReplacer("-", " ", "_", " ", ":", ": ").Replace(r)
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
		return joinClause("A human decision is owed", clause), "maintainer"
	case stagelabels.ReadyForHumanMerge:
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
		// Likewise 015 says "Triage needed" itself; the needs-triage chip
		// only adds information on another stage (a claimed or covered
		// issue the gate is still holding).
		if stage == stagelabels.TriageNeeded {
			it.Modifiers = slices.DeleteFunc(it.Modifiers, func(m string) bool { return m == modNeedsTriage })
		}
		it.Caption, it.Actor = stageTrackerCaption(stage, it.Reason, it.Author, it.Kind, it.Mirrored, rec, hasRec, it.LinkedPR, it.Head)
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
		it.LinkedIssues = stageTrackerPRLinkedIssues(pr, status.GitHubBaseURL)
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
	s.stageTrackerServed = true
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
	// The ETag hashes the body with generated_at zeroed: the timestamp moves
	// on every rebuild, so including it would turn an unchanged tracker into
	// a 200 each time (the client revalidates with this ETag and gets a 304).
	var hashBuf bytes.Buffer
	hashResp := resp
	hashResp.GeneratedAt = time.Time{}
	if err := json.NewEncoder(&hashBuf).Encode(hashResp); err != nil {
		slogWarnOverviewEncode(err)
		return nil, "", now, true
	}
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(resp); err != nil {
		slogWarnOverviewEncode(err)
		return nil, "", now, true
	}
	c.body, c.etag, c.generatedAt = buf.Bytes(), webstatic.ETagFor(hashBuf.Bytes()), now
	return c.body, c.etag, c.generatedAt, true
}

// invalidateStageTrackerCache forgets the memoised payload. UpdateStatusIfFresh
// calls it on every status publish so tracker freshness follows the eval
// cycle rather than the cache TTL alone. It reports whether the tracker has
// ever been served: only then is an SSE nudge worth sending, which also keeps
// the /api/events stream free of extra frames for clients that never asked
// for the tracker.
func (s *Server) invalidateStageTrackerCache() (served bool) {
	s.stageTrackerCacheMu.Lock()
	s.stageTrackerCache = stageTrackerCache{}
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
	w.Header().Set("X-Hive-Stage-Tracker-Generated-At", generatedAt.UTC().Format(time.RFC3339Nano))
	w.Header().Set("Last-Modified", generatedAt.UTC().Format(http.TimeFormat))
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
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

// stageTrackerPRLinkedIssues projects the issues pr links onto the tracker:
// GitHub's closing references first, then the references the PR's own text
// yields (github.PRReferencedIssues), in their priority order. Same-repo
// references only (a bare `#N`, or an explicit owner/repo#N naming the PR's
// own repo), never the PR's own number, each once at its strongest tier. A
// closing keyword hive read itself (PRLinkViaFixes) is a closing link here
// and carries no Via; the softer tiers keep theirs so the UI can mark them.
// Mention-tier entries are provisional: stageTrackerResolveLinkedPRs drops
// them unless the number is an issue listed in the response.
func stageTrackerPRLinkedIssues(pr github.PullRequest, baseURL string) []StageTrackerLinkedIssue {
	var out []StageTrackerLinkedIssue
	seen := map[int]bool{}
	add := func(li github.PRLinkedIssue, via string) {
		if li.Number <= 0 || li.Number == pr.Number || seen[li.Number] {
			return
		}
		if li.Repo != "" && !strings.EqualFold(li.Repo, pr.Repo) {
			return
		}
		seen[li.Number] = true
		out = append(out, StageTrackerLinkedIssue{
			Number: li.Number, Title: li.Title, State: strings.ToLower(strings.TrimSpace(li.State)),
			URL: overviewItemURL(li.URL, pr.Repo, li.Number, "issue", baseURL), Via: via,
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

// stageTrackerLinkSoft reports whether a linked issue is a reference link
// rather than a closing one.
func stageTrackerLinkSoft(li StageTrackerLinkedIssue) bool {
	return li.Via != "" && li.Via != github.PRLinkViaFixes
}

// stageTrackerPruneSoftLinks drops, from a PR's links, every soft link whose
// number is an open PR in the same repo (a squash title's `(#N)` or a
// `Refs #N` naming the PR this one follows) and every bare mention that is
// not an issue listed in the response — a keyword-less `#N` has no other
// way to prove it names an issue.
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
}
