package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	stageengineprstage "github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The Overview card sorts every open issue and pull request into one band,
// and every band into one of six KPI tiles. It is display only: it reads
// what the hive already decided (the stored ownership decision, the row
// facts, the contributor queue's last verdicts and the stage records copied
// at publish) and decides nothing. No lane, gate or decoder reads a card_*
// field; ownership, the inbox and every existing export value are untouched.
//
// The card is computed on every read from the rows being served, the way
// chart_slice is, so /api/status, SSE, the export and MCP agree by
// construction. Nothing here does I/O or takes a lock beyond the two leaf
// caches overviewCardInputs reads (overview_card_stash.go).

// Band keys. The Your turn group's bands say what the person has to do;
// cardTurnBand reads that from the stored decision's verb and reason code,
// and each card-only your-turn rule names its band.
const (
	cardBandInspect         = "inspect"
	cardBandDecide          = "decide"
	cardBandApprove         = "approve"
	cardBandHandMerge       = "merge-by-hand"
	cardBandReview          = "review"
	cardBandTriage          = "triage"
	cardBandAdmit           = "admit"
	cardBandTurnOther       = "your-turn-other"
	cardBandToClose         = "to-close"
	cardBandUnexplained     = "unexplained"
	cardBandCoveredByPR     = "covered-by-pr"
	cardBandWaitingOnItem   = "waiting-on-item"
	cardBandHumanHold       = "human-hold"
	cardBandReporterConfirm = "reporter-confirm"
	cardBandAuthorUpdate    = "author-update"
	cardBandBotRebase       = "bot-rebase"
	cardBandQueued          = "queued"
	cardBandWorking         = "working"
	cardBandWillMerge       = "will-merge"
	cardBandNotWork         = "not-work"
	cardBandDraft           = "draft"
	cardBandHeld            = "held"
)

// Group keys. Every group is one KPI tile, and its key is the tile key.
const (
	cardGroupYourTurn   = "your-turn"
	cardGroupWaiting    = "waiting-elsewhere"
	cardGroupBlocked    = "blocked"
	cardGroupActionable = "actionable-now"
	cardGroupOutside    = "outside"
	cardGroupHeld       = "held"
)

// cardBandOrder lists every band key; cardIssueBandOrder and cardPRBandOrder
// are the two donuts in plot order.
var (
	cardBandOrder = []string{
		cardBandInspect, cardBandDecide, cardBandApprove, cardBandHandMerge, cardBandReview,
		cardBandTriage, cardBandAdmit, cardBandTurnOther, cardBandToClose, cardBandUnexplained,
		cardBandCoveredByPR, cardBandWaitingOnItem, cardBandHumanHold, cardBandReporterConfirm,
		cardBandAuthorUpdate, cardBandBotRebase,
		cardBandQueued, cardBandWorking, cardBandWillMerge,
		cardBandNotWork, cardBandDraft,
		cardBandHeld,
	}
	// Approve and Merge by hand are PR gates; an issue never lands there
	// (cardTurnBand). The Your turn bands come first, Unexplained last among
	// them, so the plot order is the legend's (the legend draws a headed
	// group's bands under its subtotal row in this order).
	cardIssueBandOrder = []string{
		cardBandInspect, cardBandDecide, cardBandReview, cardBandTriage, cardBandAdmit, cardBandTurnOther,
		cardBandToClose, cardBandUnexplained, cardBandCoveredByPR, cardBandWaitingOnItem, cardBandHumanHold,
		cardBandReporterConfirm, cardBandQueued, cardBandWorking, cardBandHeld, cardBandNotWork,
	}
	cardPRBandOrder = []string{
		cardBandInspect, cardBandDecide, cardBandApprove, cardBandHandMerge, cardBandReview,
		cardBandTriage, cardBandAdmit, cardBandTurnOther, cardBandToClose, cardBandUnexplained,
		cardBandWaitingOnItem, cardBandHumanHold, cardBandAuthorUpdate, cardBandBotRebase,
		cardBandWorking, cardBandWillMerge, cardBandHeld, cardBandDraft,
	}
	// cardGroupOrder is the tile order.
	cardGroupOrder = []string{cardGroupYourTurn, cardGroupWaiting, cardGroupHeld, cardGroupBlocked, cardGroupActionable, cardGroupOutside}
)

// cardBandDef is one band's spec. prRule is set only where the PR donut
// words the band differently.
type cardBandDef struct {
	label, group, rule, prRule string
	keep, hideZero             bool
}

var cardBandDefs = map[string]cardBandDef{
	cardBandInspect: {label: "Inspect a sentinel flag", group: cardGroupYourTurn, hideZero: true,
		rule:   "Sentinel found something risky. Look at it before agents work on it.",
		prRule: "Sentinel found something risky. Look before it merges."},
	cardBandDecide: {label: "Decide", group: cardGroupYourTurn,
		rule: "The hive stopped and is waiting for your call."},
	cardBandApprove: {label: "Approve", group: cardGroupYourTurn, hideZero: true,
		rule: "GitHub won't merge this without a person's approval."},
	cardBandHandMerge: {label: "Merge by hand", group: cardGroupYourTurn, hideZero: true,
		rule: "The hive doesn't merge this one, in this repo or for this author. A person merges it."},
	cardBandReview: {label: "Review", group: cardGroupYourTurn,
		rule: "The hive wants a person to look at it before it goes on."},
	cardBandTriage: {label: "Triage", group: cardGroupYourTurn, hideZero: true,
		rule: "Confirm the gap, or triage it."},
	cardBandAdmit: {label: "Admit", group: cardGroupYourTurn, hideZero: true,
		rule: "Add the admission label to let agents work on it, or close it."},
	cardBandTurnOther: {label: "Other", group: cardGroupYourTurn, hideZero: true,
		rule:   "Needs a person for a reason not listed above; the download shows each item's reason.",
		prRule: "Needs a person for a reason not listed above, such as a red base branch, nobody to fix it, a dependency-bot PR still red after the hand-off time, or a bot's red or conflicting PR labelled needs-human; the download shows each item's reason."},
	cardBandToClose: {label: "Yours to close", group: cardGroupYourTurn,
		rule: "The hive thinks it may be done or no longer needed but won't close it itself. Close it, or keep it open."},
	cardBandUnexplained: {label: "Unexplained", group: cardGroupYourTurn, keep: true,
		rule: "No rule explains what happens next. This is a hive bug; please report it."},
	cardBandCoveredByPR: {label: "Covered by an open PR", group: cardGroupWaiting,
		rule: "A PR is doing the work. Follow the PR."},
	cardBandWaitingOnItem: {label: "Waiting on another item", group: cardGroupWaiting,
		rule: "Another issue or PR must finish first, or someone who was asked must answer. Check that the blocker is still real and moving."},
	cardBandHumanHold: {label: "Human placed on hold", group: cardGroupWaiting,
		rule: "A person put a hold on it or paused it from the dashboard."},
	cardBandReporterConfirm: {label: "Reporter to confirm the fix", group: cardGroupWaiting,
		rule: "The fix landed; the person who filed it hasn't confirmed yet."},
	cardBandAuthorUpdate: {label: "Author to update the PR", group: cardGroupBlocked,
		rule: "Returned to its author for changes. It waits until the author pushes."},
	cardBandBotRebase: {label: "Dependency bot to rebase", group: cardGroupBlocked,
		rule: "The hive will ask the bot to rebase its red branch, and it is still within the hand-off time. Past that time it needs a person."},
	cardBandQueued: {label: "Queued for agents", group: cardGroupActionable, keep: true,
		rule: "In the contributor queue now. An agent will pick it up; nothing for you to do."},
	cardBandWorking: {label: "Agents are working it", group: cardGroupActionable,
		rule:   "An agent claimed it.",
		prRule: "The hive's review, fixer or adjudicator is actively on it."},
	cardBandWillMerge: {label: "Will merge on its own", group: cardGroupActionable,
		rule: "In the merge queue. It merges on its own."},
	cardBandNotWork: {label: "Not work", group: cardGroupOutside,
		rule: "Epics, trackers, dependency dashboards, advisory reports, and issues a skip label in this hive's settings keeps off the queue."},
	cardBandDraft: {label: "Draft", group: cardGroupOutside,
		rule: "A draft. Its author must mark it ready for review."},
	cardBandHeld: {label: "Held", group: cardGroupHeld, hideZero: true,
		rule: "Paused by a hold no earlier rule explains. It moves when the hold is released."},
}

// cardYourTurnRule is the your-turn tile's rule.
const cardYourTurnRule = "No agent will move these until a person acts. Each row says what to do next, for example decide, review or close. Unexplained is a hive bug a person should see."

// cardGroupDef is one group's spec; term is its Actionable-now equation term.
type cardGroupDef struct {
	label, term, rule string
	headed            bool
}

// The your-turn group reads "Needs a person": the card is the same for every
// owner, so "your" would be wrong for most of what it counts. Only the words
// changed; its key (your-turn, the card_group value and the card_band alias)
// did not, and code and comments still call it the Your turn group.
var cardGroupDefs = map[string]cardGroupDef{
	cardGroupYourTurn:   {label: "Needs a person", term: overviewTermYourTurn, headed: true, rule: cardYourTurnRule},
	cardGroupWaiting:    {label: "Waiting on someone else", term: overviewTermWaiting, headed: true, rule: "Someone other than you has the next move. Nothing to do until they act."},
	cardGroupBlocked:    {label: "Blocked", term: overviewTermBlocked, headed: true, rule: "Someone else must push a fix: the author or a dependency bot."},
	cardGroupActionable: {label: "Hive has it", term: overviewTermActionable, headed: true, rule: "The hive moves these on its own. Nothing for you to do."},
	cardGroupOutside:    {label: "Outside", term: overviewTermOutside, rule: "Not work, drafts, and items this hive's settings leave alone."},
	cardGroupHeld:       {label: "Held", term: overviewTermHeld, rule: "Paused by a hold no earlier rule explains."},
}

// cardOutsideRemainderRule is added to the Outside tile's rule: the tile also
// counts what a source counted but the snapshot could not list.
const cardOutsideRemainderRule = " It also counts open items a source counted but the snapshot could not list, and sidelined PRs that are not drafts."

// OverviewCardBand is one row's place on the card.
type OverviewCardBand struct {
	Key        string `json:"card_band"`
	Group      string `json:"card_group"`
	Reason     string `json:"card_reason"`
	ReasonCode string `json:"card_reason_code"`
	// ChurnPRs are the pull requests behind an issue_churn verdict, merged
	// first, then closed unmerged (cardChurnPRs); nil on every other row.
	ChurnPRs []OverviewChurnPR `json:"card_churn_prs,omitempty"`
}

// OverviewChurnPR is one pull request behind an issue churn row. Repo,
// Number and State (github.PRStateMerged or github.PRStateClosed, closed
// meaning closed without merging) are the claim ledger's churn record.
// Title, URL and MergedAt are what the server's merged ledger lists for a
// merged one, copied at publish (cardStash.addChurnMerged); a merged PR the
// ledger does not list, and every closed one, has none of them.
type OverviewChurnPR struct {
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	State    string `json:"state"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
}

// OverviewCardBandSpec is one band of a donut, in plot order.
type OverviewCardBandSpec struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Rule     string `json:"rule"`
	Group    string `json:"group"`
	Count    int    `json:"count"`
	Keep     bool   `json:"keep"`
	HideZero bool   `json:"hide_zero"`
}

// OverviewCardGroupSpec is one group, in tile order. Issues and PRs count
// the rows on the donuts; the Outside tile adds the remainder.
type OverviewCardGroupSpec struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Rule   string `json:"rule"`
	Headed bool   `json:"headed"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
}

// OverviewCardTileRow is one band in a tile's breakdown.
type OverviewCardTileRow struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Rule   string `json:"rule"`
	Count  int    `json:"count"`
	Issues int    `json:"issues"`
	PRs    int    `json:"prs"`
	Keep   bool   `json:"keep"`
}

// OverviewCardTile is one of the six term tiles. Breakdown lists the tile's
// bands, zero rows included.
type OverviewCardTile struct {
	Key       string                `json:"key"`
	Term      string                `json:"term"`
	Label     string                `json:"label"`
	Rule      string                `json:"rule"`
	Count     int                   `json:"count"`
	Issues    int                   `json:"issues"`
	PRs       int                   `json:"prs"`
	Breakdown []OverviewCardTileRow `json:"breakdown"`
}

// OverviewCardKinds is a count per kind.
type OverviewCardKinds struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
}

// OverviewCardAdmission says when the contributor queue's verdicts the card
// read were taken. AdmissionSeq is approximate: the ladder reads the
// previous snapshot, and two publishers can race.
type OverviewCardAdmission struct {
	Present      bool   `json:"present"`
	At           string `json:"at"`
	AdmissionSeq int64  `json:"admission_seq"`
	Verdicts     int    `json:"verdicts"`
	Capped       bool   `json:"capped"`
}

// OverviewCard is the status block the card renders. The six tiles sum to
// the displayed totals: every row has one band, every band one group, and
// the remainder (displayed total minus card rows) counts in Outside.
// Unexplained also counts inside the Your turn tile.
type OverviewCard struct {
	Issues             []OverviewCardBandSpec  `json:"issues"`
	PRs                []OverviewCardBandSpec  `json:"prs"`
	Groups             []OverviewCardGroupSpec `json:"groups"`
	Tiles              []OverviewCardTile      `json:"tiles"`
	YourTurnNotInInbox OverviewCardKinds       `json:"your_turn_not_in_inbox"`
	Unexplained        OverviewCardKinds       `json:"unexplained"`
	Remainder          OverviewCardKinds       `json:"remainder"`
	Admission          OverviewCardAdmission   `json:"admission"`
}

// overviewCardRowFields are the card fields a status row carries. The
// reason sentence stays on export and MCP rows to keep SSE frames small.
// CardChurnPRs is set on issue churn rows only, and carries only each pull
// request's repo, number and state, so the repo card can link them without
// a fetch; the title, link and merge time stay on export and MCP rows.
type overviewCardRowFields struct {
	CardBand       string            `json:"card_band,omitempty"`
	CardGroup      string            `json:"card_group,omitempty"`
	CardReasonCode string            `json:"card_reason_code,omitempty"`
	CardChurnPRs   []OverviewChurnPR `json:"card_churn_prs,omitempty"`
}

func (b OverviewCardBand) rowFields() overviewCardRowFields {
	f := overviewCardRowFields{CardBand: b.Key, CardGroup: b.Group, CardReasonCode: b.ReasonCode}
	for _, pr := range b.ChurnPRs {
		f.CardChurnPRs = append(f.CardChurnPRs, OverviewChurnPR{Repo: pr.Repo, Number: pr.Number, State: pr.State})
	}
	return f
}

// cardInputs is everything the classifiers read besides the row: a value
// copy taken from the leaf caches, with no I/O.
type cardInputs struct {
	now    time.Time
	cfg    *config.Config
	hiveID string
	// autoMergeLabel is the hive's auto-merge label, read by the caller.
	autoMergeLabel string
	workers        map[string]stageTrackerWorker
	// stash is the publish-time copy (never mutated once stored); nil
	// before the first publish.
	stash *cardStash
	// dispatch is the cached fix dispatch state; dispatchLoaded is false
	// until the first lazy load.
	dispatch       review.DispatchState
	dispatchLoaded bool
	// laneCanOpenPR reports whether a routed lane may open PRs in a repo.
	laneCanOpenPR func(lane, repo string) bool
}

// overviewCardInputs copies the card's inputs from the leaf caches. Callers
// may hold statusMu: neither cache takes it, and nothing here reads a file.
func (s *Server) overviewCardInputs(now time.Time) cardInputs {
	in := cardInputs{now: now}
	if s == nil {
		return in
	}
	if s.deps != nil {
		in.cfg = s.deps.Config
	}
	in.stash = s.cardStashSnapshot()
	in.dispatch, in.dispatchLoaded = s.cardDispatchCached()
	return in
}

// buildCardStash starts this publish's stash: the rows were built at the
// snapshot's Timestamp (UpdateStatusIfFresh overwrites it only when it
// publishes), and the write-back gate's bot-rebase answers are taken now,
// outside statusMu. The caller adds the stages and the admission verdicts.
func (s *Server) buildCardStash(status *StatusPayload) *cardStash {
	now := time.Now().UTC()
	builtAt := now
	if status != nil {
		if t, err := time.Parse(time.RFC3339, status.Timestamp); err == nil {
			builtAt = t
		}
	}
	st := newCardStash(builtAt)
	st.addBotRebase(status, s.cardGatePolicyNow(now))
	return st
}

// OverviewCardSamples are the Overview card's six tile counts in a trend
// history sample. Pointers, so samples taken before the card omit them.
type OverviewCardSamples struct {
	OverviewCardYourTurn   *int `json:"overviewCardYourTurn,omitempty"`
	OverviewCardWaiting    *int `json:"overviewCardWaiting,omitempty"`
	OverviewCardHeld       *int `json:"overviewCardHeld,omitempty"`
	OverviewCardBlocked    *int `json:"overviewCardBlocked,omitempty"`
	OverviewCardActionable *int `json:"overviewCardActionable,omitempty"`
	OverviewCardOutside    *int `json:"overviewCardOutside,omitempty"`
}

// overviewCardSamples counts the card's tiles for a trend sample. The
// published status can be rewritten in place by a hold move, so the rows
// are read under statusMu (taken after trendHistoryMu; nothing takes them
// the other way round).
func (s *Server) overviewCardSamples(status *StatusPayload, now time.Time) OverviewCardSamples {
	in := s.overviewCardInputs(now)
	in.autoMergeLabel = s.autoMergeLabel()
	s.statusMu.RLock()
	card := overviewCardClassify(status, in).card
	s.statusMu.RUnlock()
	return OverviewCardSamples{
		OverviewCardYourTurn:   intPtr(card.tile(cardGroupYourTurn).Count),
		OverviewCardWaiting:    intPtr(card.tile(cardGroupWaiting).Count),
		OverviewCardHeld:       intPtr(card.tile(cardGroupHeld).Count),
		OverviewCardBlocked:    intPtr(card.tile(cardGroupBlocked).Count),
		OverviewCardActionable: intPtr(card.tile(cardGroupActionable).Count),
		OverviewCardOutside:    intPtr(card.tile(cardGroupOutside).Count),
	}
}

// cardIssueRow is one issue row of the snapshot with the facts the card
// reads: an actionable or held issue, or a non-actionable record.
type cardIssueRow struct {
	repoFull      string
	issue         github.Issue
	work          github.RepoWorkIssue
	held          bool
	nonActionable bool
	holdSources   []string
	ownership     *Ownership
	// hiveClosingPR and closingPR are the open PRs whose closing references
	// name a non-actionable issue (hiveClosingPRFor, openClosingPRFor).
	hiveClosingPR int
	closingPR     int
}

func (r cardIssueRow) number() int {
	if r.nonActionable {
		return r.work.Number
	}
	return r.issue.Number
}

func (r cardIssueRow) labels() []string {
	if r.nonActionable {
		return r.work.Labels
	}
	return r.issue.Labels
}

func (r cardIssueRow) bucket() string {
	if r.nonActionable {
		return r.work.Bucket
	}
	return ""
}

func (r cardIssueRow) bucketReason() string {
	if r.nonActionable {
		return r.work.Reason
	}
	return ""
}

func (r cardIssueRow) reporterGate() bool {
	if r.nonActionable {
		return r.work.ReporterConfirmationGate
	}
	return r.issue.ReporterConfirmationGate
}

// openBlockers are read for actionable and held rows only, as the your-turn
// decision reads them.
func (r cardIssueRow) openBlockers() []string {
	if r.nonActionable {
		return nil
	}
	return r.issue.OpenBlockers()
}

// cardPRRow is one PR row: open, held, or a sidelined draft.
type cardPRRow struct {
	repoFull string
	// pr is the PR as the hive tracks it (frontendPullRequest swaps a
	// suppressed stage in).
	pr              github.PullRequest
	verdict         *github.MergeVerdict
	held            bool
	sidelinedDraft  bool
	suppressedStage string
	ownership       *Ownership
}

// cardDecisionBucket is the decision park's canonical label.
const cardDecisionBucket = config.DefaultNeedsDecisionLabel

func cardBand(key, code, reason string) OverviewCardBand {
	return OverviewCardBand{Key: key, Group: cardBandDefs[key].group, ReasonCode: code, Reason: cardText(reason)}
}

func cardUnexplained(what, reason string) OverviewCardBand {
	return cardBand(cardBandUnexplained, "unexplained:"+what, reason)
}

// cardOwnershipBand is rules 1 and 2 of both tables: the stored decision
// says your turn. kind is ownershipKindIssue or ownershipKindPR.
func cardOwnershipBand(o *Ownership, kind string) (OverviewCardBand, bool) {
	if o == nil || !o.YourTurn {
		return OverviewCardBand{}, false
	}
	return cardBand(cardTurnBand(o.Verb, o.ReasonCode, kind), "ownership:"+o.ReasonCode, nonEmpty(o.Reason, "The hive holds it for a person.")), true
}

// cardTurnBand is the Your turn band of a stored decision: what the person
// has to do, read from its verb. Two PR review gates get their own band (a
// person's approval, a hand merge); on an issue they stay Review, so an
// issue never lands on a band the issue donut does not plot. A verb the card
// does not know is Other, never dropped: every your-turn decision is in the
// Your turn group.
func cardTurnBand(verb, reasonCode, kind string) string {
	switch verb {
	case OwnershipVerbInspect:
		return cardBandInspect
	case OwnershipVerbDecide, OwnershipVerbSteer, OwnershipVerbSpec:
		return cardBandDecide
	case OwnershipVerbReview:
		if kind == ownershipKindPR {
			switch reasonCode {
			case OwnershipReasonReviewDecision:
				return cardBandApprove
			case OwnershipReasonHandMerge:
				return cardBandHandMerge
			}
		}
		return cardBandReview
	case OwnershipVerbTriage:
		return cardBandTriage
	case OwnershipVerbAdmit:
		return cardBandAdmit
	case OwnershipVerbClose:
		return cardBandToClose
	}
	return cardBandTurnOther
}

// cardBandYourTurnAlias is the single Your turn band of before the split.
// A card_band filter still accepts it and reads it as cardTurnAliasRow: the
// set of rows that band held (it never held Yours to close or Unexplained,
// but it did hold the issue churn rows that are Yours to close now).
const cardBandYourTurnAlias = "your-turn"

// cardChurnReasonCode is an issue churn row's card reason code.
const cardChurnReasonCode = "verdict:" + contributorAdmissionReasonIssueChurn

// cardTurnAliasRow reports a row the card_band=your-turn alias returns: a
// row on a what-to-do band (cardTurnAction), or an issue churn row. The
// churn rows were in the single Your turn band and then in Triage; their
// action is closing the issue or adding the churn label, so they sit in
// Yours to close, and the alias keeps returning them.
func cardTurnAliasRow(b OverviewCardBand) bool {
	return cardTurnAction(b.Key) || (b.Key == cardBandToClose && b.ReasonCode == cardChurnReasonCode)
}

// cardTurnAction reports a Your turn band that says what to do: every Your
// turn band but Yours to close and Unexplained.
func cardTurnAction(key string) bool {
	return key != cardBandToClose && key != cardBandUnexplained && cardBandDefs[key].group == cardGroupYourTurn
}

// cardNotInInbox reports a band the not-in-inbox count reads: every Your
// turn band but Unexplained, which is counted apart.
func cardNotInInbox(key string) bool {
	return key != cardBandUnexplained && cardBandDefs[key].group == cardGroupYourTurn
}

func cardHoldSources(sources []string) string {
	unique := uniqueLowerLabels(sources)
	if len(unique) == 0 {
		return "label"
	}
	return strings.Join(unique, "+")
}

// cardParkingFor is ownershipEnv.parkingBucket over this hive's
// hard-suppress labels.
func cardParkingFor(cfg *config.Config) ownershipEnv {
	env := ownershipEnv{}
	if cfg != nil {
		env.parking = ownershipParkingFor(cfg.Project.IssueFilter.HardSuppressLabels)
	}
	return env
}

// cardIssueParking is the parking bucket the your-turn decision read for
// the row (decideIssueOwnership rule 5).
func cardIssueParking(row cardIssueRow, cfg *config.Config) string {
	env := cardParkingFor(cfg)
	if !row.nonActionable {
		return env.parkingBucket(row.labels())
	}
	parking := repoWorkIssueParkingBucket(row.work)
	if parking != "" {
		if ordered := env.parkingBucket(row.labels()); ordered != "" {
			parking = ordered
		}
	}
	return parking
}

// decideIssueCard places one issue row (overview card §3.1). The first
// matching rule wins.
func decideIssueCard(row cardIssueRow, in cardInputs) OverviewCardBand {
	o := row.ownership
	if band, ok := cardOwnershipBand(o, ownershipKindIssue); ok {
		return band
	}
	if o != nil && o.WaitingElsewhere {
		return cardIssueWaitingBand(row, o, in)
	}
	if row.held {
		return cardBand(cardBandHeld, "held:"+cardHoldSources(row.holdSources), "On hold; it moves when the hold is released.")
	}
	if row.nonActionable {
		return cardIssueBucketBand(row, in)
	}
	return cardActionableIssueBand(row, in)
}

// cardActionableIssueBand is rules 6, 7 and 9 for an issue on the agent
// queues: the stashed verdict, else a live claim, else its labels.
func cardActionableIssueBand(row cardIssueRow, in cardInputs) OverviewCardBand {
	if v, ok := in.verdictFor(row.repoFull, row.number()); ok {
		return cardIssueVerdictBand(row, v, in)
	}
	if _, live := row.issue.LiveClaim(in.now); live || issueClaimed(row.issue.Labels) {
		return cardBand(cardBandWorking, "flags:claimed", "An agent claimed it.")
	}
	const noVerdict = "No admission verdict cached yet; placed by its labels."
	if issueChartBand(row.issue) == "in-progress" {
		return cardBand(cardBandWorking, "flags:no_verdict", noVerdict)
	}
	return cardBand(cardBandQueued, "flags:no_verdict", noVerdict)
}

// cardNotARow is the band of a row the card does not count: it is left to
// the Outside remainder.
var cardNotARow = OverviewCardBand{}

func cardIssueWaitingBand(row cardIssueRow, o *Ownership, in cardInputs) OverviewCardBand {
	cause := cardIssueWaitingCause(row, cardIssueParking(row, in.cfg), in)
	code := "wait:" + cause
	switch cause {
	case cardWaitHold:
		return cardBand(cardBandHumanHold, code, "A person put a hold on it or paused it from the dashboard.")
	case cardWaitCoveredByPR, cardWaitHiveClosingPR, cardWaitClosingPR:
		return cardBand(cardBandCoveredByPR, code, nonEmpty(o.Reason, "An open PR covers it; follow the PR."))
	case cardWaitDecisionBlocker:
		return cardBand(cardBandWaitingOnItem, code, nonEmpty(o.Reason, "It waits on the decision on another issue."))
	case cardWaitBlockedBy:
		return cardBand(cardBandWaitingOnItem, code, "GitHub lists open blockers: "+strings.Join(row.openBlockers(), ", ")+".")
	case cardWaitReporter:
		return cardBand(cardBandReporterConfirm, code, "The fix landed; the person who filed it has not confirmed yet.")
	case cardWaitMaintainerReply:
		return cardBand(cardBandWaitingOnItem, code, nonEmpty(o.Reason, "A maintainer replied last; it waits on a reply."))
	}
	// The stored decision says someone else has the next move, but the row
	// changed since it was made (a webhook patch between publishes): display
	// lag, not a hive bug. TestCardIssueWaitingReturnsAreCounted catches a
	// new waiting return in the decision instead.
	return cardBand(cardBandWaitingOnItem, "wait:unmatched", nonEmpty(o.Reason, "The hive's last decision says someone else has the next move; the item changed since, and the next refresh places it again."))
}

// Work-detail buckets and filtered reasons the enumeration and its passes
// record on non-actionable issue rows (pkg/github: fetchIssues,
// classifyWebhookIssue, ContributorClaimGuard.FilterLiveIssueClaims,
// FilterInProgressIssues, prclaims; pkg/dashboard: seedOpenItemsFromForge,
// insertFrontendRepoItem) that pkg/github keeps unexported.
const (
	cardFilteredExempt         = "Exempt label"
	cardFilteredTracking       = "tracking" // gfi.SkipTracking, pinned by TestCardFilteredTrackingMatchesGFI
	cardBucketInProgress       = "in_progress"
	cardBucketClaimed          = "claimed"
	cardBucketActionable       = "actionable"
	cardBucketPendingRelease   = "pending_release_owner_step"
	cardBucketOther            = "other"
	cardFilteredBlockedNoFacts = "blocked"
)

// cardAgentProposalReason copies agentProposalFilterReason
// (pkg/github/issue_admitter.go).
var cardAgentProposalReason = "Agent proposal awaiting " + github.HumanAckLabel

// cardIssueBucketBand places a non-actionable row ownership left alone (or
// has not decided yet: a row a webhook added between publishes), by its
// enumeration bucket. Every bucket the hive records has an outcome; only an
// unknown one is Unexplained. Where the your-turn decision would claim the
// row, the card places it the way the decision would.
func cardIssueBucketBand(row cardIssueRow, in cardInputs) OverviewCardBand {
	w := row.work
	code := "bucket:" + w.Bucket
	switch w.Bucket {
	case repoWorkBucketHiveAdvisory:
		return cardBand(cardBandNotWork, code, "A hive advisory report: a standing meta issue, not work.")
	case repoWorkBucketDependencyDashboard:
		return cardBand(cardBandNotWork, code, "A dependency bot's dashboard issue: a control panel, not work.")
	case cardBucketInProgress:
		return cardBand(cardBandWorking, code, "An agent holds a live claim on it.")
	case cardBucketClaimed:
		return cardBand(cardBandWorking, code, "Someone holds a live claim on it.")
	case repoWorkBucketClaimedByPR:
		return cardBand(cardBandCoveredByPR, code, "An open PR covers it; follow the PR.")
	case repoWorkBucketReporterTriage:
		return cardBand(cardBandAdmit, code, "The reporter is not trusted; add an admission label to let agents work on it.")
	case cardBucketPendingRelease:
		return cardBand(cardBandDecide, code, "The hive parked it for the owner's release step.")
	case cardBucketActionable:
		// The claim guard lists an issue it keeps actionable here as well
		// as on the actionable list; the first copy wins, so this row is
		// placed only when the actionable copy is missing.
		issue := github.Issue{Repo: w.Repo, Number: w.Number, Labels: w.Labels, Assignees: w.Assignees, LinkedPRs: w.LinkedPRs, ClaimContext: w.ClaimContext, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, URL: w.URL}
		return cardActionableIssueBand(cardIssueRow{repoFull: row.repoFull, issue: issue}, in)
	case cardBucketOther:
		// Counted open (the forge cache, a webhook) but not yet placed by
		// the scanner: a paused repo, a skipped standing-meta issue,
		// enumeration lag. The hive has not classified it, so the card does
		// not either; it is in the Outside remainder.
		return cardNotARow
	case repoWorkBucketFiltered:
		return cardFilteredBand(row, in)
	}
	return cardUnexplained("bucket", "The scanner kept it off the queues for a reason the card does not know ("+nonEmpty(w.Bucket, "no bucket")+").")
}

// cardFilteredBand places a filtered row by the reason the enumeration
// recorded (pkg/github/client.go fetchIssues).
func cardFilteredBand(row cardIssueRow, in cardInputs) OverviewCardBand {
	w := row.work
	code := "bucket:filtered:"
	reason := strings.TrimSpace(w.Reason)
	switch {
	case reason == cardFilteredExempt:
		return cardBand(cardBandNotWork, code+"exempt", "An exempt label in this hive's settings keeps it off the queue.")
	case reason == cardFilteredTracking:
		return cardBand(cardBandNotWork, code+"tracking", "A tracking issue: it stays open on purpose and is not work.")
	case github.IsBlockedIssueReason(reason):
		return cardBand(cardBandWaitingOnItem, code+"blocked", cardBlockedReason(w))
	case reason == ownershipLabelReporterConfirmation || cardSHAHoldReason(reason, in.cfg):
		return cardBand(cardBandReporterConfirm, code+"reporter", "It waits on the person who filed it.")
	case reason == config.PossiblyExistsStageLabel:
		return cardBand(cardBandTriage, code+"possibly_exists", "It may already exist; confirm the gap or close it.")
	case repoWorkIssueParkingBucket(w) != "" || cardParkingFor(in.cfg).parkingBucket([]string{reason}) != "":
		// The row the stored decision will give it (parkedOwnership):
		// needs-human is Review, the decision, direction and spec parks
		// Decide. The park is the one the decision reads (cardIssueParking:
		// the recorded bucket, its labels in the decision's order), else the
		// one the recorded reason names.
		bucket := nonEmpty(cardIssueParking(row, in.cfg), cardParkingFor(in.cfg).parkingBucket([]string{reason}))
		park := parkedOwnership(bucket)
		return cardBand(cardTurnBand(park.Verb, park.ReasonCode, ownershipKindIssue), code+"parked", "Parked for a person ("+reason+").")
	case reason == github.IssueFilterReason:
		return cardIssueFilterBand(row, code+"issue_filter")
	case reason == cardAgentProposalReason:
		return cardBand(cardBandNotWork, code+"agent_proposal", "An agent's proposal that has not been acknowledged; the self-authorization hold is off here.")
	}
	return cardUnexplained("bucket", "The scanner filtered it for a reason the card does not know ("+nonEmpty(reason, "no reason")+").")
}

// cardIssueFilterBand places a row the issue filter refuses: an open PR that
// closes it covers it; with no stored decision yet, an issue a maintainer
// decided, an agent filed or a person filed waits on a maintainer admitting
// it (decideIssueOwnership rule 9b); otherwise it is not work.
func cardIssueFilterBand(row cardIssueRow, code string) OverviewCardBand {
	w := row.work
	if row.closingPR > 0 {
		return cardBand(cardBandCoveredByPR, code, fmt.Sprintf("PR #%d closes it.", row.closingPR))
	}
	if row.ownership == nil {
		labels := labelSet(w.Labels)
		if labels[ownershipLabelApprovedDirection] || issueAgentRole(w.Labels) != "" || w.AuthorIsHuman {
			return cardBand(cardBandAdmit, code, "The issue filter keeps it off the agent queue; add an admission label to admit it, or close it.")
		}
	}
	return cardBand(cardBandNotWork, code, "The issue filter keeps it off the agent queues, and it is not a person's to admit.")
}

// cardSHAHoldReason reports the SHA-hold label a filtered row's reason
// names: this hive's configured SHA-hold label (Issues.EffectiveSHAHoldTrustedLabel).
func cardSHAHoldReason(reason string, cfg *config.Config) bool {
	want := []string{config.DefaultSHAHoldTrustedLabel}
	if cfg != nil {
		want = append(want, cfg.Issues.EffectiveSHAHoldTrustedLabel())
	}
	return cardHasLabel(want, reason)
}

// cardBlockedReason says what a filtered blocked row waits on, from the
// facts the enumeration recorded on it (setBlockedRowFacts).
func cardBlockedReason(w github.RepoWorkIssue) string {
	switch {
	case len(w.BlockedOn) > 0:
		return "Blocked on " + strings.Join(w.BlockedOn, ", ") + "; check they are still open and moving."
	case !strings.EqualFold(strings.TrimSpace(w.Reason), cardFilteredBlockedNoFacts):
		return w.Reason
	case w.OpenBlockedBy > 0:
		return fmt.Sprintf("GitHub lists %d open blockers.", w.OpenBlockedBy)
	case !w.RecheckAfter.IsZero():
		return "The hive rechecks it after " + w.RecheckAfter.UTC().Format(cardClockFormat) + "."
	}
	return "Labelled blocked; nothing records what it waits on. Check the blocker is still real."
}

// cardClockFormat renders a card time in UTC.
const cardClockFormat = "2006-01-02 15:04Z"

// cardVerdict is the contributor queue's last answer for an issue.
type cardVerdict struct {
	admitted   bool
	queueHeld  bool
	heldReason string
	item       AdmissionWithheldItem
}

// verdictFor reads the stashed verdict for repo#number.
func (in cardInputs) verdictFor(repoFull string, number int) (cardVerdict, bool) {
	if in.stash == nil || number <= 0 {
		return cardVerdict{}, false
	}
	key := cardStashKey(repoFull, number)
	if reason, ok := in.stash.queueHeld[key]; ok {
		return cardVerdict{queueHeld: true, heldReason: reason}, true
	}
	if in.stash.queued[key] {
		return cardVerdict{admitted: true}, true
	}
	if item, ok := in.stash.withheld[key]; ok {
		return cardVerdict{item: item}, true
	}
	return cardVerdict{}, false
}

// cardIssueVerdictBand places an actionable issue by the contributor
// queue's stashed verdict (§3.1.3). The verdict's Detail is not shown: the
// card writes its own text.
func cardIssueVerdictBand(row cardIssueRow, v cardVerdict, in cardInputs) OverviewCardBand {
	if v.queueHeld {
		reason := "A person put it on hold in the contributor queue."
		if v.heldReason != "" {
			reason = "A person put it on hold in the contributor queue: " + v.heldReason
		}
		return cardBand(cardBandHumanHold, "verdict:queue_hold", reason)
	}
	if v.admitted {
		return cardBand(cardBandQueued, "verdict:admitted", "In the contributor queue now.")
	}
	item := v.item
	r := item.Reason
	code := "verdict:" + r
	switch r {
	case withheldReasonCooldown, withheldReasonFailureCooldown, withheldReasonRepoUnmintable:
		what := "In a cooldown"
		if r == withheldReasonRepoUnmintable {
			what = "Its repository is excluded after a GitHub App token mint failure"
		}
		reason := what + "; it comes back to the queue on its own."
		if t, err := time.Parse(time.RFC3339, item.CooldownUntil); err == nil {
			reason = what + " until " + t.UTC().Format(cardClockFormat) + "; it comes back to the queue on its own."
		}
		return cardBand(cardBandQueued, code, reason)
	case contributorAdmissionReasonOwnerGrace:
		return cardBand(cardBandQueued, code, "An owner or maintainer filed it moments ago and gets the first claim.")
	case withheldReasonInFlight, contributorAdmissionReasonIssueClaim, withheldReasonAssignedToOther:
		return cardBand(cardBandWorking, code, "Someone is already on it.")
	case contributorAdmissionReasonOpenPRClaim, contributorAdmissionReasonOpenPRLinked:
		return cardBand(cardBandCoveredByPR, code, "An open PR"+cardClaimPRRef(item)+" already covers it; follow the PR.")
	case contributorAdmissionReasonCoveringPRStalled:
		return cardBand(cardBandCoveredByPR, code, "PR"+cardClaimPRRef(item)+" stalled; its own row asks you to close it or take it over.")
	case contributorAdmissionReasonMergedClaimStale:
		return cardBand(cardBandToClose, code, "A merged PR already fixed it and it is still open; close it or say what remains.")
	case withheldReasonNoWorkNeeded:
		return cardBand(cardBandToClose, code, "An agent found no work needed; close it or say what is left.")
	case completionVerdictNeedsDecision:
		return cardBand(cardBandDecide, code, "An agent needs a maintainer decision before it can go on.")
	case contributorAdmissionReasonIssueChurn:
		prs := cardChurnPRs(item, row.repoFull, in.stash)
		// A person ends the guard only by closing the issue or adding the
		// churn label (issueNeedsHumanTriage); nothing reads a comment, and
		// otherwise it lapses when the churn history ages out. So the row
		// is Yours to close, not Triage.
		band := cardBand(cardBandToClose, code, cardChurnReason(item, prs, nonEmpty(item.Repo, row.repoFull)))
		band.ChurnPRs = prs
		return band
	case contributorAdmissionReasonLabelSkipped:
		return cardSkippedLabelBand(item.SkippedLabel, code, in.cfg)
	case withheldReasonTracker:
		return cardBand(cardBandNotWork, code, "A tracker or umbrella issue; its child issues carry the work.")
	case contributorAdmissionReasonWorkflowBlocked, contributorAdmissionReasonDependencyBlocked, contributorAdmissionReasonDependencyUnknown,
		convergence.ReasonWaitingForDependency, convergence.ReasonDependencyUnknown:
		return cardBand(cardBandWaitingOnItem, code, "Another issue or PR must finish first.")
	case "":
		// The integration-orchestration gate's refusal carries no
		// convergence reason (withheldFromAdmissionDecision).
		return cardBand(cardBandWaitingOnItem, "verdict:integration_orchestration", "Integration orchestration holds it until another item finishes.")
	case withheldReasonDisabledRepo, withheldReasonContributorFilter:
		lane := row.issue.Lane
		if lane == "" {
			lane = classify.KeywordLane(row.issue)
		}
		if in.laneCanOpenPR != nil && in.laneCanOpenPR(lane, row.repoFull) {
			return cardBand(cardBandQueued, code, "The contributor queue leaves it out, but its routed lane can open a PR for it.")
		}
		if r == withheldReasonContributorFilter {
			return cardBand(cardBandNotWork, code, "A contributor filter in this hive's settings keeps it off the queue.")
		}
		return cardBand(cardBandNotWork, code, "This hive does not work this repository's issues.")
	}
	if strings.HasPrefix(r, "integration_orchestration:") {
		return cardBand(cardBandWaitingOnItem, "verdict:integration_orchestration", "Integration orchestration holds it until another item finishes.")
	}
	return cardUnexplained("verdict", "The contributor queue withholds it for a reason the card does not know ("+r+").")
}

// cardChurnInlineMax is how many pull request numbers an issue churn
// reason names per list before "and N more"; the row's ChurnPRs has them
// all.
const cardChurnInlineMax = 5

// cardChurnPRs reads an issue_churn verdict's pull requests. ChurnPRs is
// github.IssueChurn.PRNumbers: the merged ones first (ChurnMerged of them),
// then the closed-unmerged ones, each "#N" in the issue's repository or
// "owner/repo#N" in another. stash adds what the merged ledger lists for a
// merged one; nil adds nothing.
func cardChurnPRs(item AdmissionWithheldItem, repoFull string, stash *cardStash) []OverviewChurnPR {
	issueRepo := nonEmpty(item.Repo, repoFull)
	var out []OverviewChurnPR
	for i, ref := range item.ChurnPRs {
		repo, number, ok := cardChurnRef(ref, issueRepo)
		if !ok {
			continue
		}
		pr := OverviewChurnPR{Repo: repo, Number: number, State: github.PRStateClosed}
		if i < item.ChurnMerged {
			pr.State = github.PRStateMerged
			if stash != nil {
				if e, ok := stash.churnMerged[cardStashKey(repo, number)]; ok {
					pr.Title, pr.URL = cardText(e.Title), e.URL
					if !e.MergedAt.IsZero() {
						pr.MergedAt = e.MergedAt.UTC().Format(time.RFC3339)
					}
				}
			}
		}
		out = append(out, pr)
	}
	return out
}

// cardChurnRef parses one PRNumbers entry; "#N" is in issueRepo.
func cardChurnRef(ref, issueRepo string) (string, int, bool) {
	ref = strings.TrimSpace(ref)
	i := strings.LastIndexByte(ref, '#')
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(ref[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	repo := strings.TrimSpace(ref[:i])
	if repo == "" {
		repo = issueRepo
	}
	return repo, n, repo != ""
}

// cardChurnReason is an issue churn row's reason: the merged and the
// closed-unmerged counts, each with its pull requests' numbers inline, at
// most cardChurnInlineMax of them, "#N" in the issue's repository and
// "owner/repo#N" in another.
func cardChurnReason(item AdmissionWithheldItem, prs []OverviewChurnPR, issueRepo string) string {
	const tail = " and it is still open; close it if it is done, or add " + churnTriagedMarker + " to give it back to agents."
	var merged, closed []string
	for _, pr := range prs {
		ref := "#" + strconv.Itoa(pr.Number)
		if !strings.EqualFold(pr.Repo, issueRepo) {
			ref = pr.Repo + ref
		}
		if pr.State == github.PRStateMerged {
			merged = append(merged, ref)
		} else {
			closed = append(closed, ref)
		}
	}
	list := func(refs []string) string {
		if len(refs) == 0 {
			return ""
		}
		if len(refs) <= cardChurnInlineMax {
			return " (" + strings.Join(refs, ", ") + ")"
		}
		return " (" + strings.Join(refs[:cardChurnInlineMax], ", ") + " and " + strconv.Itoa(len(refs)-cardChurnInlineMax) + " more)"
	}
	prsWord := func(n int) string {
		if n == 1 {
			return "1 PR"
		}
		return strconv.Itoa(n) + " PRs"
	}
	nMerged, nClosed := item.ChurnMerged, item.ChurnClosed
	switch {
	case nMerged > 0 && nClosed > 0:
		return prsWord(nMerged) + " merged" + list(merged) + ", " + strconv.Itoa(nClosed) + " closed unmerged" + list(closed) + tail
	case nClosed > 0:
		return prsWord(nClosed) + " closed unmerged" + list(closed) + tail
	default:
		return prsWord(nMerged) + " merged" + list(merged) + tail
	}
}

// cardClaimPRRef names the covering PR from the claim URL, " #N", or "".
func cardClaimPRRef(item AdmissionWithheldItem) string {
	url := strings.TrimRight(item.ClaimURL, "/")
	i := strings.LastIndexByte(url, '/')
	if i < 0 {
		return ""
	}
	if n, err := strconv.Atoi(url[i+1:]); err == nil && n > 0 {
		return " #" + strconv.Itoa(n)
	}
	return ""
}

// cardSkippedLabelBand places a label_skipped verdict by the label that
// matched the contribute skip set.
func cardSkippedLabelBand(label, code string, cfg *config.Config) OverviewCardBand {
	l := strings.ToLower(strings.TrimSpace(label))
	awaiting := config.ReporterTrustConfig{}.EffectiveAwaitingLabel()
	decision := append([]string{config.DefaultNeedsDecisionLabel, config.LegacyNeedsDecisionLabel, config.PRNeedsDecisionStageLabel}, config.HardSuppressLabelsConfig{}.EffectiveNeedsDecision()...)
	if cfg != nil {
		awaiting = cfg.Project.IssueFilter.ReporterTrust.EffectiveAwaitingLabel()
		decision = append(decision, cfg.Project.IssueFilter.HardSuppressLabels.EffectiveNeedsDecision()...)
	}
	switch {
	case strings.EqualFold(l, config.DefaultReporterTrustAwaitingLabel) || (awaiting != "" && strings.EqualFold(l, awaiting)):
		return cardBand(cardBandTriage, code, label+" keeps agents off it; triage it.")
	case l == ownershipLabelAlreadyDone:
		return cardBand(cardBandToClose, code, "Marked already done; close it or keep it.")
	case cardHasLabel(decision, l):
		return cardBand(cardBandDecide, code, "It waits on a maintainer's decision ("+label+").")
	}
	return cardBand(cardBandNotWork, code, "The skip label "+nonEmpty(label, "(unnamed)")+" keeps it off the queue; skip labels are this hive's settings.")
}

// Author kinds the PR rules tell apart.
const (
	cardAuthorHive    = "hive"
	cardAuthorPerson  = "person"
	cardAuthorClanker = "clanker"
	cardAuthorBot     = "bot"
)

// cardPRAuthorKind classifies a PR's author: the hive's own (an agent role,
// App-authored or author/hive), a contributor's agent, a non-hive bot, or a
// person.
func cardPRAuthorKind(pr github.PullRequest) string {
	if prAgentRole(pr) != "" {
		return cardAuthorHive
	}
	kind, _ := stageTrackerAuthorKind(pr.Labels, pr.Author, pr.AppAuthored, pr.HiveAttributed)
	switch kind {
	case authorKindHive:
		return cardAuthorHive
	case authorKindClanker:
		return cardAuthorClanker
	case authorKindHuman:
		return cardAuthorPerson
	}
	if stageTrackerBotAuthor(pr.Author, kind) {
		return cardAuthorBot
	}
	return cardAuthorPerson
}

// cardBotNeedsHumanReason is the card reason of a bot's red or conflicting
// PR that the stored decision holds for needs-human.
const cardBotNeedsHumanReason = "A bot's PR labelled needs-human with red CI or a conflict; there is nothing to review yet. Ask the bot to rebase it (tick its rebase box), close it, or fix it by hand."

// cardBotNeedsHumanBand is rule 1 for a non-hive bot's PR the stored
// decision holds for needs-human (person_pr_needs_human): with red CI or a
// conflict, the person's job is to close it, fix it or ask the bot, not to
// review it, so it is Other. The card does not claim the hive will or won't
// ask the bot to rebase: the stale-red pass still asks once after its own
// red-CI hand-off. A green one stays on Review.
// Either way it stays in the Your turn group with its ownership reason
// code, so the tile and the inbox agree.
func cardBotNeedsHumanBand(row cardPRRow) (OverviewCardBand, bool) {
	o := row.ownership
	if o == nil || !o.YourTurn || o.ReasonCode != OwnershipReasonPersonPRNeedsHuman || cardPRAuthorKind(row.pr) != cardAuthorBot {
		return OverviewCardBand{}, false
	}
	if !cardDependencyBotRedCI(row.pr) && !cardConflictMergeState(row.pr) {
		return OverviewCardBand{}, false
	}
	return cardBand(cardBandTurnOther, "ownership:"+o.ReasonCode, cardBotNeedsHumanReason), true
}

// decidePRCard places one PR row (overview card §3.2). The first matching
// rule wins.
func decidePRCard(row cardPRRow, in cardInputs) OverviewCardBand {
	pr := row.pr
	o := row.ownership
	if band, ok := cardBotNeedsHumanBand(row); ok {
		return band
	}
	if band, ok := cardOwnershipBand(o, ownershipKindPR); ok {
		return band
	}
	if pr.Draft || row.sidelinedDraft {
		return cardBand(cardBandDraft, "flags:draft", "A draft; its author must mark it ready for review.")
	}
	if o != nil && o.WaitingElsewhere {
		return cardPRWaitingBand(row, o)
	}
	if row.held || len(holdLabels(pr.Labels, in.hiveID)) > 0 {
		sources := pr.HoldSources
		return cardBand(cardBandHeld, "held:"+cardHoldSources(sources), "On hold; it moves when the hold is released.")
	}
	facts, hasFacts := in.stageFacts(row.repoFull, pr.Number)
	stage := cardPRStageNumber(pr.Labels)
	fromRecord := false
	if stage == 0 && hasFacts && facts.Stage.Applies(stagelabels.PRItem) {
		stage, fromRecord = facts.Stage.Number(), true
	}
	if v := row.verdict; v != nil && v.State == github.MergeVerdictEligible && !v.HandMerge {
		return cardBand(cardBandWillMerge, "verdict:eligible", "The merge sweep's verdict is eligible; it merges on its own.")
	}
	switch stage {
	case trackerNumberAwaitingMerge, trackerNumberPreMergeSoak, stagelabels.MergeQueueNumber:
		return cardBand(cardBandWillMerge, cardStageCode(stage, "merge"), "In the merge lane; it merges on its own.")
	}
	if stage == 0 {
		return cardPRNoStageBand(row, in)
	}
	band := cardPRStageBand(row, stage, facts, hasFacts, in)
	if band.Key == cardBandUnexplained && fromRecord && in.stash != nil && !in.stash.builtAt.IsZero() && facts.EnteredAt.After(in.stash.builtAt) {
		// The stage came from a record that moved after the rows were
		// built: the rows are older than the stage, so the row is placed as
		// if it had no stage.
		return cardPRNoStageBand(row, in)
	}
	if cardHasLabel(pr.Labels, stagelabels.StalledLabel) {
		band.Reason += " Stalled."
	}
	return band
}

// cardPRStageNumber is the first stage/NNN label's number, or 0.
func cardPRStageNumber(labels []string) int {
	for _, l := range labels {
		if n := stageNumber(l); n > 0 {
			return n
		}
	}
	return 0
}

func cardStageCode(stage int, what string) string {
	return fmt.Sprintf("stage:%03d:%s", stage, what)
}

func cardPRWaitingBand(row cardPRRow, o *Ownership) OverviewCardBand {
	switch cardPRWaitingCause(row.pr, row.held) {
	case cardWaitHold:
		return cardBand(cardBandHumanHold, "wait:hold", "A person put a hold on it or paused it from the dashboard.")
	case cardWaitAdjudicator:
		return cardBand(cardBandWorking, "wait:adjudicator", nonEmpty(o.Reason, "Waiting for the adjudicator."))
	}
	if reviewDeciderWaiting(o) {
		// The hive's review asked a named person; the reason says who.
		return cardBand(cardBandWaitingOnItem, "wait:review_decider", o.Reason)
	}
	kind := cardPRAuthorKind(row.pr)
	code := "wait:answered:" + kind
	switch kind {
	case cardAuthorHive:
		return cardBand(cardBandWorking, code, "A person answered the hive's review; the hive's fixer carries it out.")
	case cardAuthorBot:
		return cardBand(cardBandTurnOther, code, "A person answered the review; the hive does not fix bot PRs.")
	}
	return cardBand(cardBandAuthorUpdate, code, nonEmpty(o.Reason, "A person answered the review; the author must push the change."))
}

// stageFacts reads the stage record copied at publish for repo#number.
func (in cardInputs) stageFacts(repoFull string, number int) (cardStageFacts, bool) {
	if in.stash == nil || number <= 0 {
		return cardStageFacts{}, false
	}
	f, ok := in.stash.stages[cardStashKey(repoFull, number)]
	return f, ok
}

// cardBaseRed reports a stage reason that blames the base branch or a
// shared CI incident (pkg/stageengine/prstage/reasons.go).
func cardBaseRed(reason string) bool {
	return strings.HasPrefix(reason, stageengineprstage.StageReasonBaseBranchRed) || strings.HasPrefix(reason, stageengineprstage.StageReasonSharedCIIncident)
}

// cardPRStageBand places a PR by its stage (§3.2.1).
func cardPRStageBand(row cardPRRow, stage int, facts cardStageFacts, hasFacts bool, in cardInputs) OverviewCardBand {
	pr := row.pr
	kind := cardPRAuthorKind(pr)
	switch stage {
	case trackerNumberReviewPending, trackerNumberReviewed:
		if hasFacts && cardBaseRed(facts.Reason) {
			return cardBand(cardBandTurnOther, cardStageCode(stage, "base_red"), "Red because main is red; fix main or follow its fix.")
		}
		return cardBand(cardBandWorking, cardStageCode(stage, "review"), "The hive's review is on it.")
	case trackerNumberFixing, trackerNumberNeedsAuthor:
		if kind == cardAuthorBot {
			// The hand-off bound runs from the stage's entry; with no record
			// of it, from the PR's last update.
			since := pr.UpdatedAt
			if hasFacts && !facts.EnteredAt.IsZero() {
				since = facts.EnteredAt
			}
			return cardBotPRBand(row, since, cardStageCode(stage, "bot"), in)
		}
		if stage == trackerNumberNeedsAuthor {
			return cardAuthorStageBand(row, stage, kind, facts, hasFacts, in)
		}
		if kind != cardAuthorHive {
			return cardUnexplained("stage_070_author", "At 070 fixing, but the hive fixes only its own PRs.")
		}
		if !in.dispatchLoaded {
			return cardBand(cardBandWorking, cardStageCode(stage, "dispatch_unread"), "The hive's fixer is on it (fix dispatch state not read yet).")
		}
		if why, excluded := cardFixerExcluded(in.dispatch, in.workers, row.repoFull, pr); excluded {
			return cardBand(cardBandTurnOther, cardStageCode(stage, "fixer_excluded"), "Nobody is fixing it: "+why+".")
		}
		return cardBand(cardBandWorking, cardStageCode(stage, "fixing"), "The hive's fixer is on it.")
	case trackerNumberReturned:
		return cardAuthorStageBand(row, stage, kind, facts, hasFacts, in)
	case trackerNumberAdopted:
		return cardBand(cardBandWorking, cardStageCode(stage, "adopted"), "The hive adopted it and is finishing it.")
	case trackerNumberNeedsDecision, trackerNumberReadyForHuman:
		reason := fmt.Sprintf("At stage %03d, but the hive did not put it in your turn.", stage)
		if row.suppressedStage != "" {
			reason += " The hive holds it at " + row.suppressedStage + "; the write-back gate kept the label off GitHub."
		}
		return cardUnexplained(fmt.Sprintf("stage_%03d_not_your_turn", stage), reason)
	}
	return cardUnexplained("stage_unknown", fmt.Sprintf("At stage %03d, which the card does not know for a PR.", stage))
}

// cardAuthorStageBand is 071/072: the author must push, unless this
// repository's person takeover is on and the hive posted its notice.
func cardAuthorStageBand(row cardPRRow, stage int, kind string, facts cardStageFacts, hasFacts bool, in cardInputs) OverviewCardBand {
	if in.cfg != nil && hasFacts && !facts.PersonNoticeAt.IsZero() && in.cfg.PersonTakeoverAllowed(row.repoFull, kind == cardAuthorPerson) {
		return cardBand(cardBandWorking, cardStageCode(stage, "takeover"), "The hive posted its takeover notice and takes the PR over.")
	}
	reason := "Returned to its author; it waits until the author pushes."
	if hasFacts && facts.AdoptionBlocked != "" {
		reason = "Returned to its author; the hive's adoption is held (" + facts.AdoptionBlocked + ")."
	}
	return cardBand(cardBandAuthorUpdate, cardStageCode(stage, "author"), reason)
}

// cardBotPRBand places a non-hive bot's red or conflicting PR: the
// dependency bot is asked to rebase while the hand-off time runs, when the
// hive would ask it; otherwise it is a person's.
func cardBotPRBand(row cardPRRow, since time.Time, code string, in cardInputs) OverviewCardBand {
	pr := row.pr
	red, conflict := cardDependencyBotRedCI(pr), cardConflictMergeState(pr)
	if cardBotRebaseCandidate(pr) && (red || conflict) {
		allowed, known := cardBotRebaseAllowed(in, row.repoFull, pr)
		if !known {
			return cardBand(cardBandTurnOther, code+"_gate_unknown", "Not yet checked against the write-back policy; the next refresh says whether the hive asks the bot to rebase.")
		}
		if !allowed {
			return cardBand(cardBandTurnOther, code+"_no_rebase", "The hive will not ask the bot to rebase; close it, rebase it by hand, or ask the bot.")
		}
		grace := cardBotConflictGrace
		if red {
			grace = cardBotRedCIGrace
		}
		if since.IsZero() {
			return cardBand(cardBandTurnOther, code+"_unknown", "The hand-off time is unknown; check whether the bot is still updating it.")
		}
		deadline := since.Add(grace).UTC()
		if in.now.Before(deadline) {
			return cardBand(cardBandBotRebase, code+"_rebase", "Inside the hand-off time; a person gets it at "+deadline.Format(cardClockFormat)+".")
		}
		return cardBand(cardBandTurnOther, code+"_overdue", "Hand-off overdue since "+deadline.Format(cardClockFormat)+".")
	}
	return cardBand(cardBandTurnOther, code+"_no_rebase", "The hive will not ask the bot to rebase; close it, rebase it by hand, or ask the bot.")
}

// cardPRNoStageBand places a PR with no stage at all (engine off, shadow
// mode, the deriver abstained, a new PR) by its merge state (§3.2.2). It is
// never Unexplained.
func cardPRNoStageBand(row cardPRRow, in cardInputs) OverviewCardBand {
	pr := row.pr
	eligible, blocked, _ := prBandFlags(pr, row.verdict, labelSet(pr.Labels), in.autoMergeLabel, prGitHubReview(pr))
	switch {
	case eligible:
		return cardBand(cardBandWillMerge, "flags:no_stage:eligible", "Merge-eligible; it merges on its own.")
	case blocked:
		switch kind := cardPRAuthorKind(pr); {
		case pr.SharedCIIncident > 0:
			return cardBand(cardBandTurnOther, "flags:no_stage:shared_ci", "Red because of a shared CI incident; fix it or follow its fix.")
		case kind == cardAuthorHive:
			return cardBand(cardBandWorking, "flags:no_stage:hive_blocked", "The hive's own PR; its fixer takes it.")
		case kind == cardAuthorBot:
			return cardBotPRBand(row, pr.UpdatedAt, "flags:no_stage:bot", in)
		default:
			return cardBand(cardBandAuthorUpdate, "flags:no_stage:author", "Blocked; it waits until its author pushes.")
		}
	}
	var overrides map[string]time.Duration
	if in.cfg != nil {
		overrides = in.cfg.Governor.Labels.Stage.DwellOverrides()
	}
	if !pr.CreatedAt.IsZero() && in.now.Sub(pr.CreatedAt) < stagelabels.Dwell(stagelabels.ReviewPending, overrides) {
		return cardBand(cardBandWorking, "flags:no_stage:new", "New; the hive has not staged it yet.")
	}
	return cardBand(cardBandWorking, "flags:no_stage:open", "Open with no stage; the hive moves it on its own.")
}

// cardRowKey keys a row's band in overviewCardResult.
func cardRowKey(kind, repoFull string, number int) string {
	return kind + "|" + strings.ToLower(repoFull) + "#" + strconv.Itoa(number)
}

// cardRepoFull is the owner/repo a row belongs to: its own Repo when that
// is qualified, else the repository card's name (Full first).
func cardRepoFull(rowRepo, repoName string) string {
	if strings.Contains(rowRepo, "/") {
		return rowRepo
	}
	return repoName
}

// overviewCardResult is one classification of a snapshot: each row's band
// and the card block counted from them.
type overviewCardResult struct {
	rows map[string]OverviewCardBand
	card *OverviewCard
	// issueRows and prRows count the rows classified, per kind; repoRows
	// per repository card (lower-case overviewRepoName).
	issueRows, prRows int
	repoRows          map[string]OverviewCardKinds
}

// band returns the classified band of a row.
func (r overviewCardResult) band(kind, repoFull string, number int) (OverviewCardBand, bool) {
	b, ok := r.rows[cardRowKey(kind, repoFull, number)]
	return b, ok
}

// cardTally counts classified rows per kind and band.
type cardTally struct {
	issues, prs             map[string]int
	notInInbox, unexplained OverviewCardKinds
	issueRows, prRows       int
}

func newCardTally() *cardTally {
	return &cardTally{issues: map[string]int{}, prs: map[string]int{}}
}

func (t *cardTally) add(kind string, band OverviewCardBand, o *Ownership) {
	inbox := o != nil && o.YourTurn
	notInInbox := cardNotInInbox(band.Key) && !inbox
	if kind == ownershipKindPR {
		t.prs[band.Key]++
		t.prRows++
		if notInInbox {
			t.notInInbox.PRs++
		}
		if band.Key == cardBandUnexplained {
			t.unexplained.PRs++
		}
		return
	}
	t.issues[band.Key]++
	t.issueRows++
	if notInInbox {
		t.notInInbox.Issues++
	}
	if band.Key == cardBandUnexplained {
		t.unexplained.Issues++
	}
}

// overviewCardClassify classifies every row the card counts: actionable,
// held and non-actionable issues; open and held PRs and the sidelined
// drafts. It is the one walk /api/status, the export and the history read.
func overviewCardClassify(status *StatusPayload, in cardInputs) overviewCardResult {
	res := overviewCardResult{rows: map[string]OverviewCardBand{}, repoRows: map[string]OverviewCardKinds{}}
	tally := newCardTally()
	if status == nil {
		res.card = buildOverviewCard(tally, FrontendOverviewTotals{}, in.stash)
		return res
	}
	in.hiveID = status.HiveID
	if in.workers == nil {
		in.workers = stageTrackerWorkersFromStatus(status)
	}
	if in.laneCanOpenPR == nil {
		cfg := in.cfg
		in.laneCanOpenPR = cardLaneCanOpenPR(status.Agents, func(repo string) int {
			if cfg == nil {
				return 1
			}
			if level := cfg.EffectiveACMMLevelForRepo(repo); level > 0 {
				return level
			}
			return detectACMMLevel(cfg)
		})
	}
	for _, repo := range status.Repos {
		repoName := overviewRepoName(repo)
		repoKey := strings.ToLower(repoName)
		// An issues-feed echo of a PR is not an issue: the canonical
		// open-items set (repoOpenItems) skips it, and so does the card, so
		// its rows stay within the displayed totals.
		prNumbers := map[int]bool{}
		for _, held := range []bool{false, true} {
			for _, entry := range frontendRepoPRs(repo, held) {
				prNumbers[entry.pr.Number] = true
			}
		}
		for _, raw := range repo.NonActionablePrs {
			if w, ok := repoWorkPR(raw); ok {
				prNumbers[w.Number] = true
			}
		}
		prEcho := func(number int, url string) bool {
			return prNumbers[number] || strings.Contains(url, "/pull/")
		}
		// A row listed twice (the claim guard's work-detail copy of an
		// actionable issue, an App stale draft on both PR lists) counts
		// once: the first copy wins, in repoOpenItems' order.
		seen := map[string]bool{}
		first := func(kind string, number int) bool {
			key := kind + "#" + strconv.Itoa(number)
			if seen[key] {
				return false
			}
			seen[key] = true
			return true
		}
		record := func(kind, repoFull string, number int, band OverviewCardBand, o *Ownership) {
			if band.Key == "" {
				return
			}
			res.rows[cardRowKey(kind, repoFull, number)] = band
			tally.add(kind, band, o)
			counts := res.repoRows[repoKey]
			if kind == ownershipKindPR {
				counts.PRs++
			} else {
				counts.Issues++
			}
			res.repoRows[repoKey] = counts
		}
		for _, held := range []bool{false, true} {
			source := repo.ActionableIssues
			if held {
				source = repo.HeldIssues
			}
			for _, raw := range source {
				issue, ok := frontendIssue(raw)
				if !ok || prEcho(issue.Number, issue.URL) || !first(ownershipKindIssue, issue.Number) {
					continue
				}
				row := cardIssueRow{repoFull: cardRepoFull(issue.Repo, repoName), issue: issue, held: held,
					ownership: ownershipFor(repo.Ownership, ownershipKindIssue, issue.Number)}
				switch v := raw.(type) {
				case github.HoldItem:
					row.holdSources = v.HoldSources
				case *github.HoldItem:
					row.holdSources = v.HoldSources
				}
				record(ownershipKindIssue, row.repoFull, issue.Number, decideIssueCard(row, in), row.ownership)
			}
		}
		for _, raw := range repo.NonActionableIssues {
			item, ok := repoWorkIssueRow(raw)
			if !ok || prEcho(item.Number, item.URL) || !first(ownershipKindIssue, item.Number) {
				continue
			}
			row := cardIssueRow{repoFull: cardRepoFull(item.Repo, repoName), work: item, nonActionable: true,
				ownership: ownershipFor(repo.Ownership, ownershipKindIssue, item.Number)}
			row.hiveClosingPR = hiveClosingPRFor(repo, repoName, item.Number)
			if item.Bucket == repoWorkBucketFiltered && item.Reason == github.IssueFilterReason {
				row.closingPR = openClosingPRFor(repo, repoName, item.Number)
			}
			record(ownershipKindIssue, row.repoFull, item.Number, decideIssueCard(row, in), row.ownership)
		}
		for _, held := range []bool{false, true} {
			for _, entry := range frontendRepoPRs(repo, held) {
				if !first(ownershipKindPR, entry.pr.Number) {
					continue
				}
				row := cardPRRow{repoFull: cardRepoFull(entry.pr.Repo, repoName), pr: entry.pr, verdict: entry.verdict, held: held,
					suppressedStage: entry.wire.SuppressedStage, ownership: ownershipFor(repo.Ownership, ownershipKindPR, entry.pr.Number)}
				record(ownershipKindPR, row.repoFull, entry.pr.Number, decidePRCard(row, in), row.ownership)
			}
		}
		for _, entry := range frontendRepoDraftPRs(repo) {
			if !first(ownershipKindPR, entry.pr.Number) {
				continue
			}
			row := cardPRRow{repoFull: cardRepoFull(entry.pr.Repo, repoName), pr: entry.pr, sidelinedDraft: true,
				ownership: ownershipFor(repo.Ownership, ownershipKindPR, entry.pr.Number)}
			record(ownershipKindPR, row.repoFull, entry.pr.Number, decidePRCard(row, in), row.ownership)
		}
	}
	res.issueRows, res.prRows = tally.issueRows, tally.prRows
	res.card = buildOverviewCard(tally, overviewTotals(status), in.stash)
	return res
}

// cardBandRule is a band's rule for one kind, or both kinds' words when
// they differ ("" kind).
func cardBandRule(key, kind string) string {
	def := cardBandDefs[key]
	switch {
	case def.prRule == "":
		return def.rule
	case kind == overviewKindIssues:
		return def.rule
	case kind == overviewKindPRs:
		return def.prRule
	}
	return "Issues: " + def.rule + " PRs: " + def.prRule
}

func cardBandSpecs(order []string, kind string, counts map[string]int) []OverviewCardBandSpec {
	out := make([]OverviewCardBandSpec, 0, len(order))
	for _, key := range order {
		def := cardBandDefs[key]
		out = append(out, OverviewCardBandSpec{Key: key, Label: def.label, Rule: cardBandRule(key, kind), Group: def.group, Count: counts[key], Keep: def.keep, HideZero: def.hideZero})
	}
	return out
}

// cardGroupRule is a group's rule; the Outside tile adds the remainder.
func cardGroupRule(group string, tile bool) string {
	rule := cardGroupDefs[group].rule
	if tile && group == cardGroupOutside {
		rule += cardOutsideRemainderRule
	}
	return rule
}

// buildOverviewCard counts the tally into the status block. The remainder
// is the displayed total (totals.<kind>.Forge, what the Total tiles print)
// minus the card rows, never below 0; it counts only in Outside.
func buildOverviewCard(t *cardTally, totals FrontendOverviewTotals, stash *cardStash) *OverviewCard {
	card := &OverviewCard{
		Issues:             cardBandSpecs(cardIssueBandOrder, overviewKindIssues, t.issues),
		PRs:                cardBandSpecs(cardPRBandOrder, overviewKindPRs, t.prs),
		YourTurnNotInInbox: t.notInInbox,
		Unexplained:        t.unexplained,
		Remainder:          OverviewCardKinds{Issues: max(0, totals.Issues.Forge-t.issueRows), PRs: max(0, totals.PRs.Forge-t.prRows)},
		Admission:          cardAdmissionOf(stash),
	}
	for _, group := range cardGroupOrder {
		def := cardGroupDefs[group]
		spec := OverviewCardGroupSpec{Key: group, Label: def.label, Rule: cardGroupRule(group, false), Headed: def.headed}
		tile := OverviewCardTile{Key: group, Term: def.term, Label: def.label, Rule: cardGroupRule(group, true), Breakdown: []OverviewCardTileRow{}}
		for _, key := range cardBandOrder {
			band := cardBandDefs[key]
			if band.group != group {
				continue
			}
			issues, prs := t.issues[key], t.prs[key]
			spec.Issues += issues
			spec.PRs += prs
			tile.Breakdown = append(tile.Breakdown, OverviewCardTileRow{Key: key, Label: band.label, Rule: cardBandRule(key, ""), Count: issues + prs, Issues: issues, PRs: prs, Keep: band.keep})
		}
		tile.Issues, tile.PRs = spec.Issues, spec.PRs
		if group == cardGroupOutside {
			tile.Issues += card.Remainder.Issues
			tile.PRs += card.Remainder.PRs
		}
		tile.Count = tile.Issues + tile.PRs
		card.Groups = append(card.Groups, spec)
		card.Tiles = append(card.Tiles, tile)
	}
	return card
}

func cardAdmissionOf(stash *cardStash) OverviewCardAdmission {
	if stash == nil || stash.at.IsZero() {
		return OverviewCardAdmission{}
	}
	return OverviewCardAdmission{
		Present:      true,
		At:           stash.at.UTC().Format(time.RFC3339),
		AdmissionSeq: stash.admissionSeq,
		Verdicts:     len(stash.queued) + len(stash.queueHeld) + len(stash.withheld),
		Capped:       stash.capped,
	}
}

// tile returns the card's tile for key, or a zero tile.
func (c *OverviewCard) tile(key string) OverviewCardTile {
	if c == nil {
		return OverviewCardTile{}
	}
	for _, t := range c.Tiles {
		if t.Key == key {
			return t
		}
	}
	return OverviewCardTile{}
}
