package dashboard

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
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
	// (needsHumanPresetLabels) the Projects preset filters by. The
	// tracker's own "Needs human" chip no longer reads it: it counts
	// StageTrackerItem.NeedsHuman (hivecommons/hive#15008).
	NeedsHumanLabels []string `json:"needs_human_labels,omitempty"`
	// NeedsHuman counts Items whose NeedsHuman flag is set — the number the
	// SPA's "Needs human" chip shows; OnHold likewise for the "On hold"
	// chip. Both come from the same predicates the per-item flags do
	// (stageTrackerNeedsHuman / stageTrackerOnHold), so chip, count and
	// filter cannot disagree.
	NeedsHuman int `json:"needs_human"`
	OnHold     int `json:"on_hold"`
	// WaitingOnHuman lists the items parked on the operator / maintainers
	// (stage_tracker_waiting.go): the sidebar badge, topbar pill and
	// toast read it so a human wait is never silent.
	WaitingOnHuman StageTrackerWaitingOnHuman `json:"waiting_on_human"`
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
	// RecentlyClosed lists the pull requests that left Items closed but
	// not merged within stageTrackerMergedWindow — superseded, withdrawn,
	// closed by a decision — as the webhook / sweep / audit paths saw them
	// (stage_tracker_recently_closed.go). With RecentlyMerged it lets the
	// SPA tell a close from a merge when a row vanishes between two polls
	// and say so instead of staying silent (hivecommons/hive#15808).
	RecentlyClosed []StageTrackerClosed `json:"recently_closed,omitempty"`
	// MergedHoldMS is dashboard.stage_tracker.merged_hold_s in
	// milliseconds: how long the SPA may keep a vanished row at "Merging ·
	// merge landed" while it waits for the merged fact.
	MergedHoldMS int `json:"merged_hold_ms"`
	// Merged24h is the same list over the last 24 hours, newest first. It
	// seeded the SPA's former "Merged today" counter; the Merged chip now
	// reads Merged (the durable ledger) and this stays for older SPAs.
	// Best effort: the in-memory audit ring is the only source.
	Merged24h []StageTrackerMerged `json:"merged_24h,omitempty"`
	// Merged is the durable merge ledger's summary (merged_ledger.go):
	// counts for today (in the spoke's zone), 7 and 30 days, all time, and
	// the last few merges. Unlike RecentlyMerged it survives a roll, so it
	// is what the "Merged" chip shows.
	Merged StageTrackerMergedSummary `json:"merged"`
	// Settings carries the operator-tunable timings the tracker's settings
	// modal edits (hivecommons/hive#15813), so every viewer — not only the
	// owner who may read /api/config/dashboard — sees the live values.
	Settings StageTrackerSettings `json:"settings"`
}

// StageTrackerSettings is the live value of the timing knobs the Activity
// Monitor settings modal edits: dashboard.stage_tracker.toast_info_s,
// dashboard.stage_tracker.toast_error_s and
// auto_merge.contributor_lane.soak_minutes.
type StageTrackerSettings struct {
	ToastInfoS  int `json:"toast_info_s"`
	ToastErrorS int `json:"toast_error_s"`
	SoakMinutes int `json:"soak_minutes"`
}

// StageTrackerMerged is one recently merged pull request.
type StageTrackerMerged struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	MergedAt time.Time `json:"merged_at"`
}

// StageTrackerClosed is one recently closed, unmerged pull request.
type StageTrackerClosed struct {
	Repo     string    `json:"repo"`
	Number   int       `json:"number"`
	ClosedAt time.Time `json:"closed_at"`
}

// stageTrackerMergedWindow bounds RecentlyMerged (and RecentlyClosed). It is deliberately much
// longer than the cache TTL plus the SPA's 30 s poll: a tab that missed a
// few polls (throttled in the background) still sees the merge it was
// watching for, and the SPA dedupes per PR so a longer list never replays.
const stageTrackerMergedWindow = 30 * time.Minute

// stageTrackerMergedDayWindow bounds Merged24h.
const stageTrackerMergedDayWindow = 24 * time.Hour

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
	return s.stageTrackerMergedSince(now, stageTrackerMergedWindow, qualify)
}

// stageTrackerMergedSince is stageTrackerRecentlyMerged over any window.
func (s *Server) stageTrackerMergedSince(now time.Time, window time.Duration, qualify func(string) string) []StageTrackerMerged {
	if s == nil || s.audit == nil {
		return nil
	}
	if qualify == nil {
		qualify = func(repo string) string { return repo }
	}
	seen := map[string]bool{}
	var out []StageTrackerMerged
	for _, e := range s.audit.RecentWithPrefixSince(now.Add(-window), github.AuditActionPRMerged) {
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
	// FallsTo lists the lower stages (same item kind) a move from this
	// stage to is a genuine regression (stagelabels.IsRegression), so the
	// event line can say "fell back to" only for those. Lower stages not
	// listed are forward or neutral: 015 → 010 is triage accepted, 085 →
	// 080 a decision made, 072 → 070 the author resuming.
	FallsTo []int `json:"falls_to,omitempty"`
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
	Superseded *github.ReviewQueueSuperseded `json:"superseded,omitempty"`
	Repo       string                        `json:"repo"`
	Number     int                           `json:"number"`
	Kind       string                        `json:"kind"`
	Title      string                        `json:"title"`
	URL        string                        `json:"url,omitempty"`
	Author     string                        `json:"author,omitempty"`
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
	// NoteChangedAt is when the caption last changed meaning: the stage
	// entry, a new reason class, or a soak that ran out. A caption whose
	// text moves while this stands still (a soak countdown) is the same
	// note; the SPA refreshes the words in place and ages the note from
	// here, not from the sweep that re-read it (hivecommons/hive#15729).
	NoteChangedAt *time.Time `json:"note_changed_at,omitempty"`
	// Modifiers are the health and control markers in effect and drawn as
	// pills: stalled, regressed, hold, needs-human, needs-decision
	// (stage/045 on an issue or stage/085 on a PR carried as a park
	// signal), needs-rebase, blocked, draft, unauthorized. A merge-gate
	// marker on a PR that has not reached the gate band is not listed
	// here; it waits in DeferredModifiers.
	Modifiers []string `json:"modifiers"`
	// DeferredModifiers are the merge-gate markers held back while the PR
	// is still in motion before the gate (needs-human, needs-decision,
	// sentinel, a merge-time unauthorized step): nothing a person does now
	// moves the row, so the tracker draws no pill and "Needs attention"
	// does not count them until the PR reaches stage/080 or later. The
	// labels themselves are untouched; this is presentation only.
	DeferredModifiers []string `json:"deferred_modifiers,omitempty"`
	// Advisory is set on an issue that is a note for maintainers, not work
	// (triage/advisory label, dedupe checklist, "Decision required: no"):
	// read it, then close it (hivecommons/hive#15381). Its Chip is the
	// Open chevron's word; the row never counts as stalled or attention.
	Advisory *StageTrackerAdvisory `json:"advisory,omitempty"`
	// NeedsHuman is true when a person's decision or action is the ONLY
	// thing that advances the item (stageTrackerNeedsHuman): stages 015
	// triage-needed, 045/085 needs-decision, 071/072 needs-author /
	// returned-to-contributor with a human author, 090 ready-for-human-
	// merge, or a needs-human / needs-decision / unauthorized pill. A
	// hold is NOT this (OnHold): the owner chose to pause it. Neither are
	// stalled, a handled regression, blocked-on-pr, 040 covered or a PR
	// ready for auto-merge — the hive advances those (hivecommons/hive#15008).
	NeedsHuman bool `json:"needs_human,omitempty"`
	// OnHold is true when the item wears a hold pill: paused on purpose,
	// waiting on nobody.
	OnHold bool `json:"on_hold,omitempty"`
	// WaitingOnYou is true when the item is parked on the signed-in
	// operator / maintainers (stage_tracker_waiting.go); WaitKind names
	// why (needs-decision, ready-for-human-merge, approve-needed,
	// returned-to-contributor-offline, other-human-wait).
	WaitingOnYou bool   `json:"waiting_on_you,omitempty"`
	WaitKind     string `json:"wait_kind,omitempty"`
	// Hold says who set the `hold` label and why, when the stage-label
	// pass could read it (hivecommons/hive#15020): the pill then reads
	// "Held by hive · reporter trust (issue #N by @X, contributor)" or
	// "Held by @login" instead of a bare "hold".
	Hold *StageTrackerHold `json:"hold,omitempty"`
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
	// soak window is left now (the record's soak deadline; a record written
	// before the deadline existed falls back to its `soaking(remaining=…)`
	// reason aged by the time since it was recorded); 0 when unknown or
	// not soaking.
	SoakRemainingSeconds int64 `json:"soak_remaining_seconds,omitempty"`
	// MergeGate narrates a PR's position at the merge gate (060 / 080 /
	// 095 / 100): state, what holds it, whether hive's merge sweep is on
	// it now, the next pass and the soak countdown (hivecommons/hive#15073).
	MergeGate *StageTrackerMergeGate `json:"merge_gate,omitempty"`
	// Soak is the soak clock while the lane holds this head for the soak
	// window, at 060/080/095: when it started, when it ends, the window
	// and the head it was measured on. The SPA counts it down in place.
	Soak *StageTrackerSoak `json:"soak,omitempty"`
	// RebaseMerge is set while an operator's "Rebase, then merge as
	// maintainer" follow-through runs on the PR (or just settled): its
	// state and the row caption to show in place of the lane's.
	RebaseMerge *StageTrackerRebaseMerge `json:"rebase_merge,omitempty"`
	// Rebase is set on a conflicting PR the fixer is rebasing on the hive's
	// behalf: which attempt of the loop guard this is, or that the hive
	// ran out and a person must resolve it.
	Rebase *StageTrackerRebase `json:"rebase,omitempty"`
	// ciRed is the listing's CI verdict on the current head (failure or
	// named failing checks), unexported: the merge train view drops such
	// a row from its train (hivecommons/hive#15825) whatever its record
	// says — a header read "8 PRs" over seven seats because a red 090
	// row was counted.
	ciRed bool
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
	// AdmittedBy is the person's trust override standing on the current
	// head ("operator-approval:<login>", hivecommons/hive#15136): their
	// Approve admitted the head to the contributor lane, which merges it
	// as-is after checks and soak, and the adoption engine leaves the PR
	// alone while it stands. Empty when nobody admitted this head.
	AdmittedBy string `json:"admitted_by,omitempty"`
	// DCOUnsigned / DCOTotal are the stage record's commit check for the
	// current head (0/0 when not cached): how many of the PR's commits lack
	// a Signed-off-by. The 090 decision reads them to say, before anyone
	// presses Approve, that merging as-is would fail DCO and to offer
	// re-authoring as a separate choice (hivecommons/hive#15136).
	DCOUnsigned int `json:"dco_unsigned,omitempty"`
	DCOTotal    int `json:"dco_total,omitempty"`
	// UnauthorizedStep is set when the item wears hive:unauthorized: the
	// step the hive's GitHub App is not permitted to take, in words
	// ("approve workflow runs"), from the `unauthorized:<step>` stage
	// reason; the gate cell reads "Awaiting human: <step>". Unauthorized
	// is the pill's tooltip naming the permission and the human action
	// (hivecommons/hive#14589).
	UnauthorizedStep string `json:"unauthorized_step,omitempty"`
	Unauthorized     string `json:"unauthorized,omitempty"`
	// DecisionNeeded is set on a row parked on a person (085 / 045 / 015,
	// the needs-human or needs-decision pill): what the person must
	// decide, who asked, the plain-words blocker and the actions that
	// advance it, under the row context menu's action ids. See
	// stage_tracker_decision.go.
	DecisionNeeded *StageTrackerDecision `json:"decision_needed,omitempty"`
	// Blocker is the structured blocker recorded on a parked issue — what
	// the hive cannot do itself (a missing App permission, a CLA) — from
	// pkg/blocker's ledger. The pill reads "Blocked · <short>" instead of
	// "Needs human decision" and "Let hive decide" is disabled with the
	// reason (hivecommons/hive#15099).
	Blocker *StageTrackerBlocker `json:"blocker,omitempty"`
	// StaleLabels are control labels the row still wears that its stage
	// has outrun, so they are not read as modifiers: `needs-triage` on an
	// issue at 020 or later (015 is not a gate; the reconciler removes the
	// label on its next pass, hivecommons/hive#14586). Rendered as a muted
	// operator tooltip, never as the stage word.
	StaleLabels []string `json:"stale_labels,omitempty"`
	// MergeTrain is set on a PR riding a conflict-aware merge train
	// (hivecommons/hive#15117): its position, the front, the PRs ahead and
	// the overlapping files. See stage_tracker_merge_train.go.
	MergeTrain *StageTrackerMergeTrain `json:"merge_train,omitempty"`
	// LeftTrain is set on a PR the view took out of its merge train
	// because it is under repair — being fixed, waiting on its author,
	// parked on a decision, or red (stageTrackerTrainUnderRepair). The
	// row rides no train and wears no ordinal; the hover says so.
	LeftTrain *StageTrackerLeftTrain `json:"left_train,omitempty"`
	// Overlap is set on a PR short of the merge queue (below 080) whose
	// changed files overlap another open PR's: the planner has ordered
	// them, but the merge train is a merge-queue concept and this row is
	// not in the queue yet. No train, no ordinal — one hint naming the PR
	// it will queue behind (stageTrackerOverlapHint).
	Overlap *StageTrackerOverlap `json:"overlap,omitempty"`
	// BlockedOn lists the open GitHub "blocked by" dependencies ("repo#N")
	// of an issue the scheduler withholds from every kick until they close
	// (scheduler.partitionBlockedIssues). Without it a sequenced sub-issue
	// read as plain Actionable nobody was picking up (hivecommons/hive#14765);
	// the row wears the `blocked` modifier and the caption names the wait.
	BlockedOn []string `json:"blocked_on,omitempty"`
	// BlockedOnPR names the pull request a `blocked` issue waits on, from
	// the `<!-- hive:blocked-on pr=owner/repo#N -->` marker the review-
	// backlog filer and the blocked sweep write (or the enumeration's
	// "blocked on owner/repo#N" reason when only that is at hand). The pill
	// reads "blocked on #N" and links to it; the caption says what lifts
	// the block. State comes from the hive's caches only, never a fresh
	// GitHub call (hivecommons/hive#14952; #14863 wore a bare grey `blocked`).
	BlockedOnPR *StageTrackerBlockedOnPR `json:"blocked_on_pr,omitempty"`
	NudgeCount  int                      `json:"nudge_count,omitempty"`
	LastNudgeAt *time.Time               `json:"last_nudge_at,omitempty"`
	AdoptedBy   int                      `json:"adopted_by,omitempty"`
	// AdoptedByState qualifies AdoptedBy when the hive's copy is not simply
	// open (hivecommons/hive#15052): "closed" — the copy was closed without
	// merging and the hive is re-adopting; "closed-twice" — two copies were
	// closed within the hour, so the hive stopped and a person must decide;
	// "copy-failed" — a close-first copy failed and the original was
	// reopened for a person (hivecommons/hive#15135).
	AdoptedByState string `json:"adopted_by_state,omitempty"`
	// SupersededBy is, on an adopted original still open while the hive's
	// copy is live (AdoptedBy with no failed / closed state), that copy:
	// the SPA folds such a row into the copy's "adopted from" chip instead
	// of listing it — a superseded PR is closed and not listed, never a
	// second row for one logical PR (hivecommons/hive#15526 under #15511
	// read as two PRs). 0 when the copy is gone and the original is live
	// work again.
	SupersededBy int `json:"superseded_by,omitempty"`
	// Supersedes names the closed original this PR replaced (the hive's
	// close-first supersession, hivecommons/hive#15135). The original is
	// closed, unmerged, and never a tracker row; this is the only trace of
	// it the tracker draws — a small chip on the live PR.
	Supersedes *StageTrackerSupersedes `json:"supersedes,omitempty"`
	// Lineage is, on the hive's copy of an adopted PR, where it came from
	// (adopted from @x's #N, when, why) and, on a still-open original,
	// which copy superseded it (stage_tracker_lineage.go). Optional; a
	// row with no adoption history carries none.
	Lineage *StageTrackerLineage `json:"lineage,omitempty"`
	// Adoption is, on a nudged PR at stage/072, whether adoption is due,
	// what holds it, and the author's live presence, with the plain-words
	// line the row shows instead of a bare "Returned to contributor"
	// (hivecommons/hive#14524).
	Adoption *StageTrackerAdoption `json:"adoption,omitempty"`
	// Presence is, on a PR row waiting on a named person, whether that
	// person's clanker is online / offline / not registered here and the
	// hive's likely next move (wait or adopt, with the adopt-at time), so
	// the waiting-on pill can carry an indicator (hivecommons/hive#15051).
	Presence *StageTrackerPresence `json:"presence,omitempty"`
	// Rework is, on a PR row returned to its author (071 / 072), whether
	// the rework request has been picked up: requested, acknowledged,
	// in_progress or pushed, with the evidence (hivecommons/hive#15280,
	// stage_tracker_rework.go).
	Rework *StageTrackerRework `json:"rework,omitempty"`
	// Push is, on a PR row in the CI / Review column, the push the hive
	// waits on or the one that just landed, so the gray pill can say
	// "waiting on @zoe to push · 38m" or "@zoe pushed abc1234 · 2m"
	// (hivecommons/hive#15784, stage_tracker_push.go).
	Push *StageTrackerPush `json:"push,omitempty"`
	// Takeover is, on a 072 / 071 clanker PR, the operator's Adopt now /
	// Reroute offer and any pending reroute or forced adoption
	// (hivecommons/hive#15038, stage_tracker_takeover.go).
	Takeover *StageTrackerTakeover `json:"takeover,omitempty"`
	// ClaimantKind is the author kind of whoever holds the live claim on a
	// claimed issue (human / clanker / hive), from the worker-claim ledger.
	ClaimantKind string `json:"claimant_kind,omitempty"`
	// Claimant names who holds the row — the agent, clanker owner or person
	// — for the chevron suffix and the "Held by:" hover line
	// (stage_tracker_claimant.go).
	Claimant *StageTrackerClaimant `json:"claimant,omitempty"`
	// StageActor is who holds the next move on this row — hive, clanker,
	// human, a foreign bot, or nobody — with the plain-words why. The SPA
	// draws the stage pill's actor icon from StageActor.Kind only
	// (stage_tracker_actor.go).
	StageActor *StageTrackerActor `json:"stage_actor,omitempty"`
	LinkedPR   int                `json:"linked_pr,omitempty"`
	// LinkedPRs lists the open pull requests covering an issue (the one
	// named in the stage reason first) with everything the tracker needs
	// to draw each as a sub-row nested under the issue.
	LinkedPRs []StageTrackerLinkedPR `json:"linked_prs,omitempty"`
	// NestedUnder is, on a PR, the issue in the same repo whose row nests
	// it (0 when the PR has no listed issue and stands on its own).
	NestedUnder int `json:"nested_under,omitempty"`
	// ReviewNoteOf is, on a `blocked` review-backlog issue ("Review backlog
	// (<perspective>): …", with or without the `[reviewer]` prefix), the
	// open source PR in the same repo it waits on. The SPA folds such issues
	// under the PR's row as one muted "N review notes blocked on this PR"
	// line — or, when the PR is not a row, into the lowest-numbered note of
	// the group — instead of N first-class rows (hivecommons/hive: 16 of 62
	// open items were blocked review notes). 0 on every other item.
	ReviewNoteOf int `json:"review_note_of,omitempty"`
	// ReviewNotes lists, on a PR, the blocked review-backlog issues folded
	// under it (ReviewNoteOf == this PR), lowest number first.
	ReviewNotes []StageTrackerReviewNote `json:"review_notes,omitempty"`
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
	// issue it closes is already closed: the PR's stage drives the strip
	// and its timestamps sort it, and the row sinks to the bottom of its
	// column (hivecommons/hive#14655). The closed issue still leads the
	// row in the issue slot — struck through, with a `closed` chip, or
	// `closed by #N` when ClosedBy names the merged PR that closed it
	// (hivecommons/hive#15232) — exactly as a soft-linked closed issue
	// does; the PR never takes the issue slot itself. StateReason
	// "not_planned" adds an "issue closed (not planned)" chip so the reader
	// can tell the PR may be moot.
	ClosedIssue *StageTrackerLinkedIssue `json:"closed_issue,omitempty"`
	// LastOperatorAction is the newest owner row action recorded for the
	// item (approve, give-direction, hold, let-hive-decide, …) with what
	// became of it, and OperatorActionCount how many were recorded; the
	// row's ✓ badge and its hover read these (operator_actions.go).
	LastOperatorAction *StageTrackerOperatorAction `json:"last_operator_action,omitempty"`
	// Decided is the post-decision state machine (hivecommons/hive#15126):
	// set once the newest recorded operator action is a decision (let hive
	// decide with a verdict, approve, queue-merge, merge-now). While its
	// effect is pending the row is projected onto the decided stage.
	Decided *StageTrackerDecided `json:"decided,omitempty"`
	// AdoptedFrom is set on the hive's copy of an adopted PR: the original
	// and the decision that led to the adoption.
	AdoptedFrom         *StageTrackerAdoptedFrom `json:"adopted_from,omitempty"`
	OperatorActionCount int                      `json:"operator_action_count,omitempty"`
	// New is set on a 000-new item — open, unplaced, nothing holding it —
	// with who moves it next and the gates ahead; the Open column renders
	// it as "New · waiting for …" (hivecommons/hive#15094).
	New     *StageTrackerNew  `json:"new,omitempty"`
	History []StageTrackerHop `json:"history"`
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
	// ClosedBy is, on a closed issue, the number of the merged pull request
	// in the same repo that closed it when that PR is not the one carrying
	// this link (the merged ledger's record of what it was nested under);
	// 0 when unknown. The row's chip reads `closed by #N` so a PR still
	// open against an issue another PR already delivered reads as the
	// duplicate it is (hivecommons/hive#15232).
	ClosedBy int `json:"closed_by,omitempty"`
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
	// NoteChangedAt mirrors the PR row's: when the caption last changed
	// meaning.
	NoteChangedAt *time.Time `json:"note_changed_at,omitempty"`
	Modifiers     []string   `json:"modifiers,omitempty"`
	// DeferredModifiers mirrors the nested PR's own row field: the
	// merge-gate pills it is not yet drawing.
	DeferredModifiers []string `json:"deferred_modifiers,omitempty"`
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
	// AdmittedBy / DCOUnsigned / DCOTotal mirror the nested PR's trust
	// override and commit check (#15136) so the sub-row's confirmations
	// carry the same caveats.
	AdmittedBy  string `json:"admitted_by,omitempty"`
	DCOUnsigned int    `json:"dco_unsigned,omitempty"`
	DCOTotal    int    `json:"dco_total,omitempty"`
	Head        string `json:"head,omitempty"`
	// Claimant mirrors the nested PR's holder so the sub-row's chevron
	// names them too.
	Claimant *StageTrackerClaimant `json:"claimant,omitempty"`
	// DecisionNeeded mirrors the nested PR's own row field so the issue's
	// sub-row can open the same "Needs human" popover.
	DecisionNeeded *StageTrackerDecision `json:"decision_needed,omitempty"`
	// SoakRemainingSeconds / MergeGate mirror the PR item's (#15073): a
	// nested Soaking sub-row shows its time left too.
	SoakRemainingSeconds int64                   `json:"soak_remaining_seconds,omitempty"`
	MergeGate            *StageTrackerMergeGate  `json:"merge_gate,omitempty"`
	MergeTrain           *StageTrackerMergeTrain `json:"merge_train,omitempty"`
	// LeftTrain mirrors the nested PR's own row field so the sub-row's
	// hover can say it left its train while under repair.
	LeftTrain *StageTrackerLeftTrain `json:"left_train,omitempty"`
	Overlap   *StageTrackerOverlap   `json:"overlap,omitempty"`
	Soak      *StageTrackerSoak      `json:"soak,omitempty"`
	// Adoption mirrors the nested PR's own row field (hivecommons/hive#14524).
	Adoption *StageTrackerAdoption `json:"adoption,omitempty"`
	// LastOperatorAction / OperatorActionCount mirror the nested PR's own
	// row fields so its sub-row wears the ✓ badge too.
	LastOperatorAction  *StageTrackerOperatorAction `json:"last_operator_action,omitempty"`
	OperatorActionCount int                         `json:"operator_action_count,omitempty"`
	// Decided / AdoptedBy / AdoptedByState / AdoptedFrom mirror the nested
	// PR's own row fields (hivecommons/hive#15126): the sub-row reads the
	// decided state and its adoption pairing.
	Decided        *StageTrackerDecided     `json:"decided,omitempty"`
	AdoptedBy      int                      `json:"adopted_by,omitempty"`
	AdoptedByState string                   `json:"adopted_by_state,omitempty"`
	AdoptedFrom    *StageTrackerAdoptedFrom `json:"adopted_from,omitempty"`
	// Lineage mirrors the nested PR's own row field.
	Lineage *StageTrackerLineage `json:"lineage,omitempty"`
	// State is "closed" for an adopted original the hive (or a person)
	// closed once its copy existed (hivecommons/hive#15126): the sub-row
	// is one muted line "#N <title> superseded · closed · hive adopted →
	// #M" with no chevron track, listed only while the copy is listed open
	// (hivecommons/hive#15511). SupersededBy is the copy (set on an open
	// original too, once the copy exists); Title is cached from the copy
	// (same commits, same title), never fetched live.
	State        string `json:"state,omitempty"`
	SupersededBy int    `json:"superseded_by,omitempty"`
	// Hold mirrors the nested PR's own row field (hivecommons/hive#15020).
	Hold *StageTrackerHold `json:"hold,omitempty"`
	// StageActor mirrors the nested PR's own row field so the sub-row's
	// pill wears the same actor icon.
	StageActor *StageTrackerActor `json:"stage_actor,omitempty"`
	// DrawnUnder is the issue whose row draws this PR's full sub-row (the
	// PR's NestedUnder). A PR that closes several open issues (`Closes
	// #14910` and `Closes #14911` in one body) is listed by each of them,
	// but its live strip is drawn once, under this issue; every other
	// parent shows a compact "↳ #N · under #M" reference instead of a
	// second identical row (hivecommons/hive#14921 drew twice).
	DrawnUnder int `json:"drawn_under,omitempty"`
	// AlsoCloses is set only on the listing drawn here (DrawnUnder == the
	// parent issue): the other listed open issues this PR also closes
	// (number, title, url), lowest number first, so the PR's sub-row can
	// name them back — the reciprocal of the "↳ #N · under #M" chip
	// (hivecommons/hive#15806).
	AlsoCloses []StageTrackerLinkedIssue `json:"also_closes,omitempty"`
	// Presence mirrors the nested PR's own row field (hivecommons/hive#15051).
	Presence *StageTrackerPresence `json:"presence,omitempty"`
	// Rework mirrors the nested PR's own row field (hivecommons/hive#15280).
	Rework *StageTrackerRework `json:"rework,omitempty"`
	// Push mirrors the nested PR's own row field (hivecommons/hive#15784).
	Push *StageTrackerPush `json:"push,omitempty"`
	// Takeover mirrors the nested PR's own row field (hivecommons/hive#15038).
	Takeover *StageTrackerTakeover `json:"takeover,omitempty"`
	// Supersedes mirrors the nested PR's own row field.
	Supersedes *StageTrackerSupersedes `json:"supersedes,omitempty"`
}

// StageTrackerSupersedes is the closed original a live PR replaced
// (StageTrackerItem.Supersedes). ClosedAt is when the hive recorded the
// supersession; Open is true when the original could not be closed (a
// fork PR the App may not close) and was left open with the note.
type StageTrackerSupersedes struct {
	Number   int        `json:"number"`
	URL      string     `json:"url,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	Open     bool       `json:"open,omitempty"`
}

// StageTrackerHold is the row's hold provenance (stagelabels.HoldProvenance
// in payload shape). By is "hive" for the hive's own holds, else the login.
type StageTrackerHold struct {
	By          string     `json:"by"`
	Source      string     `json:"source,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	Issue       int        `json:"issue,omitempty"`
	Reporter    string     `json:"reporter,omitempty"`
	Association string     `json:"association,omitempty"`
	// Pill is the ready-made pill text ("Held by hive · reporter trust
	// (issue #15017 by @Karibhou, contributor)").
	Pill string `json:"pill"`
}

// stageTrackerHoldFrom maps the store's provenance to the payload; nil when
// the record has none or the row no longer wears `hold`.
func stageTrackerHoldFrom(rec stagelabels.Record, hasRec bool, labels []string) *StageTrackerHold {
	if !hasRec || rec.Hold == nil || strings.TrimSpace(rec.Hold.By) == "" || !slices.ContainsFunc(labels, func(l string) bool { return strings.EqualFold(strings.TrimSpace(l), modHold) }) {
		return nil
	}
	h := &StageTrackerHold{By: rec.Hold.By, Source: rec.Hold.Source, Reason: rec.Hold.Reason, Issue: rec.Hold.Issue, Reporter: rec.Hold.Reporter, Association: strings.ToLower(rec.Hold.Association), Pill: rec.Hold.Pill()}
	if !rec.Hold.Since.IsZero() {
		t := rec.Hold.Since
		h.Since = &t
	}
	return h
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
	// modSentinel names the sentinel chip in DeferredModifiers; the chip
	// itself is drawn from SentinelAlert, never from Modifiers.
	modSentinel = "sentinel"
)

// prowWIPLabel is prow's work-in-progress marker: it rides every draft PR
// (and a "WIP" title) and is dropped when the author marks the PR ready.
// On the tracker it is the draft state, not a maintainer hold, so a row
// wearing only it reads "draft" rather than "hold" (hivecommons/hive#14572).
const prowWIPLabel = "do-not-merge/work-in-progress"

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
// The PR strip collapses 050 and 060 into one "CI / Review" chevron (060 is
// pre-admission: approved, CI still pending or red), so review-side detours
// hang off 050; see stageTrackerColumn for the full stage → column table.
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
		return "Review passed"
	case stagelabels.Fixing:
		return "Fixing"
	case stagelabels.NeedsAuthor:
		return "Needs author"
	case stagelabels.ReturnedToContributor:
		return "Returned to contributor"
	case stagelabels.AdoptedByHive:
		return "Adopted by hive"
	case stagelabels.AwaitingMergeAdmission:
		return "Merge queue"
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
			FallsTo:      stageTrackerFallsTo(d),
		})
	}
	return out
}

// stageTrackerFallsTo is the ordered list of lower same-kind stage numbers
// that are a regression from d (stagelabels.IsRegression).
func stageTrackerFallsTo(d stagelabels.Definition) []int {
	var out []int
	for _, to := range stagelabels.Table {
		if (d.Issue && to.Issue || d.PR && to.PR) && stagelabels.IsRegression(d.Stage, to.Stage) {
			out = append(out, to.Number)
		}
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
// so this runs once per snapshot, not per request. A person's sentinel
// acknowledgment on the bundle (evidence.ActionSentinelAcknowledged — the
// owner's Approve decision on a flagged PR) is the human review a
// notice-only alert asked for, so it clears the bundle-promoted alert (the
// label, while still on, still stands); the reason then names who
// acknowledged it.
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
	if by := b.SentinelAcknowledgedBy(); by != "" {
		// The label, while still on, is the sweep's live block; only the
		// bundle-promoted alert is answered by the acknowledgment.
		return alert, findings, reason + stageTrackerSentinelAckSuffix + by
	}
	for _, f := range b.Sentinel {
		if f.Notice {
			alert = true
			break
		}
	}
	return alert, findings, reason
}

// stageTrackerSentinelAckSuffix precedes the login of the person whose
// approval acknowledged the bundle's sentinel findings.
const stageTrackerSentinelAckSuffix = " — acknowledged by @"
