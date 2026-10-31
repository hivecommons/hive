package stageengine

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Ref identifies one forge item. Key() equals stagelabels.Key(owner/repo, number)
// so Store records keep their identity.
type Ref struct {
	Owner, Repo string
	Number      int
	Kind        stagelabels.Item // stagelabels.IssueItem | stagelabels.PRItem
	// Source names the forge the item lives on. The zero value means
	// DefaultSource ("github"), so every Ref built today is a GitHub one.
	Source string
}

// DefaultSource is the forge a Ref with no Source belongs to.
const DefaultSource = "github"

// IsDefaultSource reports whether the Ref is on the default forge (empty or
// "github", compared case-insensitively).
func (r Ref) IsDefaultSource() bool {
	return r.Source == "" || strings.EqualFold(strings.TrimSpace(r.Source), DefaultSource)
}

// Key is the stagelabels.Store key for the item. GitHub items keep the
// existing "owner/repo#n" key unchanged; another forge prefixes its name
// ("source:owner/repo#n") so items on two forges never collide.
func (r Ref) Key() string {
	k := stagelabels.Key(r.Owner+"/"+r.Repo, r.Number)
	if r.IsDefaultSource() {
		return k
	}
	return strings.ToLower(strings.TrimSpace(r.Source)) + ":" + k
}

// FullRepo is "owner/repo".
func (r Ref) FullRepo() string { return r.Owner + "/" + r.Repo }

// String renders "owner/repo#number".
func (r Ref) String() string { return r.FullRepo() + "#" + strconv.Itoa(r.Number) }

// Facts is everything a Deriver may look at. Derivers do no forge I/O.
// Fields common to both kinds live here once; kind-specific evidence is the
// one non-nil pointer.
type Facts struct {
	Ref        Ref
	ObservedAt time.Time
	Labels     []string // list payload or fresh read; includes non-hive labels
	Open       bool
	Merged     bool
	Draft      bool
	Author     AuthorFacts
	Park       ParkFacts
	Adoption   AdoptionFacts
	Origin     string // the origin/<source> label, "" for a human filing
	Issue      *IssueFacts
	PR         *PRFacts
}

// AuthorFacts is who wrote the item.
type AuthorFacts struct {
	Login        string
	Kind         authorkind.Kind // Hive | Clanker | Human | Unknown
	SelfAuthored bool            // this App wrote it (selects 070 over 071/072)
	Role         stagelabels.Role
}

// ParkFacts: why the item is (or is not) waiting on a person.
type ParkFacts struct {
	Holds             []string // stagelabels.Holds(labels), lower-cased
	NeedsHuman        bool
	DecisionLabel     string // configured issue park label present, or 085 on a PR
	PossiblyExists    bool   // prior-art gate park: stage/017-possible-existing (#13575)
	Sentinel          bool
	HumanOnlyReason   string // lane rule unrelated to trust (workflow edits, tests removed, …)
	ApprovedDirection bool   // /hive approve|decision recorded; un-park keeps needs-human (#12715)
	DecisionQuestion  string
}

// AdoptionFacts is the stage-074 ownership state of a contributor PR.
type AdoptionFacts struct {
	Adopted bool
	Head    string // adopted head SHA; stale when PR.HeadSHA differs
	InPlace bool
	Live    bool // superseder still open / hive still pushing
	Yielded bool
	// SupersededBy is the superseding App PR of a supersede-mode adoption
	// (0 in place).
	SupersededBy int
}

// IssueBucket is the enumeration cycle's work bucket for an issue. The
// string values are the legacy work-detail bucket names so the adapter
// round-trips them losslessly; a bucket the engine does not know is carried
// as-is and derives to "outside the pipeline".
type IssueBucket string

const (
	BucketActionable  IssueBucket = "actionable"
	BucketClaimed     IssueBucket = "claimed"
	BucketInProgress  IssueBucket = "in_progress"
	BucketCoveredByPR IssueBucket = "claimed_by_pr"
	BucketFiltered    IssueBucket = "filtered"
	// BucketReporterTriage is a human-filed issue the reporter-trust gate
	// (#9665) holds out until a maintainer adds the triage label; it
	// derives 015-triage-needed.
	BucketReporterTriage IssueBucket = "reporter_triage"
)

// IssueFacts is the issue-side evidence.
type IssueFacts struct {
	Bucket         IssueBucket // Actionable | Claimed | InProgress | CoveredByPR | Filtered(reason) | ReporterTriage
	FilteredReason string
	Claim          *ClaimFact
	LikelyDone     bool
	// Stalled is the dwell verdict: the sweep's stagelabels.Stalled over
	// the Store record, or, for an out-of-cycle fact, the `stage/stalled`
	// the issue already wears. The deriver keeps it only while the stage
	// stands still — a stage change restarts the clock.
	Stalled         bool
	CoveringPR      *Ref
	CoveringPRStage stagelabels.Stage // from the Store; None when unknown
	// ClosedPR is the pull request whose unmerged close an Uncovered fact
	// reports. The sink's enrichment never re-reads it as a still-open
	// cover (stagelabels.Record.CoveringPR) for the issue.
	ClosedPR *Ref
	// OpenChildren is the number of still-open sub-issues of a decomposed
	// parent (hivecommons/hive#12857). A parent waiting on children keeps
	// its stage with the wait named in the reason and is never stalled.
	OpenChildren int
	// The label-family verdicts below are resolved by the enumeration
	// (pkg/github reads the hive's configured hold, park, umbrella and
	// reporter-trust labels); the deriver only reads them, so it stays
	// free of configuration. Each is "" when the family is absent.
	//
	// HoldLabel is the hold-class label that pauses the issue (hold,
	// on-hold, do-not-merge/*, hive-pause/<id>, …; hivecommons/hive#13012).
	HoldLabel string
	// BlockedReason is set when the issue wears `blocked`: the
	// enumeration's reason ("blocked on owner/repo#N", "blocked until …")
	// or the bare label (hivecommons/hive#13093).
	BlockedReason string
	// NeedsHumanLabel names the needs-human label when Park.NeedsHuman is
	// set (the reason cites it).
	NeedsHumanLabel string
	// Blocker is the structured blocker kind recorded on the issue
	// (pkg/blocker: "capability:workflows-permission", …), "" when none.
	// A parked issue with a blocker stays 045 whatever direction a person
	// gave: no decision grants the App a permission
	// (hivecommons/hive#15099).
	Blocker string
	// BlockerShort is the blocker's pill text ("App lacks workflows
	// permission") for the reason.
	BlockerShort string
	// ApprovedDirectionLabel is the `approved-direction` label the issue
	// wears (a recorded /hive approve|decision), "" when absent. Next to
	// `needs-human` it is the stale one — the un-park that applied it
	// removed the park label, so a surviving `needs-human` came later —
	// and the writer clears it (hivecommons/hive#15099).
	ApprovedDirectionLabel string
	// UmbrellaLabel is the never-claim label the issue carries
	// (governor.claims.never_claim_labels, default `epic`).
	UmbrellaLabel string
	// TriageAwaiting is why an actionable issue is still untriaged (015):
	// the reporter-trust awaiting label it carries or the gate's
	// reporter_triage bucket.
	TriageAwaiting string
	// TriageAwaitingLabel is the reporter-trust awaiting label the issue
	// actually wears (`needs-triage` / `reporter_trust.awaiting_label`),
	// "" when it carries none. Separate from TriageAwaiting because a
	// claimed issue still wearing it derives 020, not 015, and the deriver
	// then asks the writer to clear the stale label in the same write
	// (hivecommons/hive#14586).
	TriageAwaitingLabel string
}

// ClaimFact is the live claim on an issue.
type ClaimFact struct {
	Holder string
	Kind   claims.Kind
	Until  time.Time
	// PRStarted is PR evidence behind the claim — the holder's PR request
	// named the issue, or its start marker named a branch / PR. Only a
	// claim with it derives 030-pr-in-progress; any other live claim,
	// an agent's included, derives 020-claimed (hivecommons/hive#13069).
	PRStarted bool
	PRRepo    string
	PRNumber  int
	// Partial says the covering PR references the issue without a closing
	// keyword ("Refs #N", "Part of #N") — it works the issue but does not
	// claim to finish it (hivecommons/hive#15008). The deriver still stages
	// 040 (the hive is on it; dispatch is withheld, #13747) but the reason
	// and the activity monitor say "addresses part of", never "covers".
	Partial bool
}

// PRFacts is the pull-request-side evidence.
type PRFacts struct {
	HeadSHA           string
	HeadFresh         bool // nothing has reported on the head yet
	BaseRef           string
	Conflicting       bool
	ChangesRequested  bool
	Checks            stagelabels.CheckSummary
	DCO               stagelabels.DCOState
	DCOGate           bool // repo enforces DCO on this head
	Review            ReviewFacts
	Soak, SoakElapsed time.Duration
	SharedCIIncident  int
	// BaseRedChecks are the head's failing checks that are also red on the
	// base branch tip; non-empty only when every failing check is (the PR
	// is then kept out of the repair stages with reason base-branch-red).
	BaseRedChecks []string
	// ForkGuardChecks are the head's failing checks that refuse the PR only
	// because it is from a fork; non-empty only when every failing check
	// does (the PR is then parked for a maintainer with reason fork-guard).
	ForkGuardChecks []string
	Lanes           []LaneFact // automerge lane opinions, precedence order (self > trusted > contributor)
	// Pass is what the sweep's own pass learned after the lanes spoke and
	// before the stage was written: the inputs of ApplyStageLabels' pure
	// post-steps (author-kind split, normalized-fixing guard, human route,
	// DCO override, adoption yield). Additive to the ADR-0025 shape; nil
	// when the sweep ran none of them. DerivePR reproduces the written
	// stage only from Facts, so the shadow comparison is written vs derived.
	Pass *PRPassFacts
}

// PRPassFacts carries the per-PR evidence the automerge stage pass gathers
// after resolveStage (forge reads, store state and live policy the lanes
// never see). Each field is one post-step's input, recorded as the pass
// read it; the deriver applies the same pure steps in the same order.
type PRPassFacts struct {
	// AuthorKind is stageAuthorKind's detection. The lanes record no kind,
	// so resolveStage classifies with it unknown and the pass splits the
	// 07x outcome by it afterwards; Facts.Author.Kind stays the lanes' view.
	AuthorKind authorkind.Kind
	// Prior is the store record before this pass (the normalized-fixing
	// guard reads Stage, Reason and ActivityHead).
	Prior *PriorStage
	// Route is the human-route evidence (stagelabels.RouteHuman's input)
	// when the resolved opinion was a hand-off (085/090); nil otherwise.
	Route *stagelabels.Evidence
	// DCO is the pass's own commit check for the head and whether the repo
	// enforces it (the 07x override); SquashUnsafeHive counts hive commits a
	// squash merge would misattribute; AuthorIsApp says the author is this
	// App (the attribution override applies only when it is not).
	DCO              stagelabels.DCOState
	DCOKnown         bool
	DCORequired      bool
	SquashUnsafeHive int
	AuthorIsApp      bool
	// MergeCommit is the pass's merge-method verdict for the head:
	// MergeCommitWanted when it elected a merge commit (mixed authorship,
	// each commit self-signed, role/trusted, repo not squash-only), in
	// which case the attribution override does not apply and the writer
	// puts `merge-method/merge` on; NotWanted takes it off; Unknown (commit
	// check not read) leaves it as found.
	MergeCommit stagelabels.MergeCommitState
	// AdoptionLive is prev.AdoptionLive(head): the hive owns the PR, so a
	// stage that yields to adoption reads 074. EscalationReason is set when
	// the stalled-adoption escape moved it to 085 instead.
	AdoptionLive     bool
	EscalationReason string
	// AdoptionRefusedReason is set when the record holds a terminal
	// refusal of a delegated supersede at this head
	// (stagelabels.AdoptBlockedSupersedeConflict): the PR keeps its lane's
	// fixing stage but the reason names the failed replay, not the
	// generic conflict, so the owner sees why the copy never opened.
	AdoptionRefusedReason string
	// RebaseFailedTwice is set on the hive's own adoption copy when it
	// still conflicts after the fixer's two rebase-resolve attempts: the
	// PR reads 085 with stagelabels.LaneReasonRebaseFailedTwice so a
	// person decides (hivecommons/hive#15307).
	RebaseFailedTwice bool
	// FixerExhausted is the attempt count when the review fix dispatcher
	// refuses another fix for this hive-authored PR because the
	// re-engagement cap is spent: the PR reads 085 with
	// stagelabels.FixerExhaustedReason(n) so a person decides instead of
	// the row claiming "Fixing" with nobody on it (hivecommons/hive#15744).
	FixerExhausted int
	// FixerExcluded is true when no fix job is active and the dispatcher
	// bars one for any reason (exhausted, no enabled fixer, …): a stale
	// answered decision on the same head must not pull the PR back from
	// 085 to 070 and strip stage/stalled (hivecommons/hive#15744).
	FixerExcluded bool
	// Stalled is the pass's dwell verdict (stagelabels.StalledWith over the
	// store record after this pass's Observe); KindLabels are the
	// configured author/<kind> names. Both are outputs of the pass the
	// writer needs on the Desired, so the Decision carries them through;
	// before they were recorded here an enforce-mode pass never wrote
	// stage/stalled on a PR and ignored custom author-kind names.
	Stalled    bool
	KindLabels authorkind.Names
}

// PriorStage is the store record a pass started from.
type PriorStage struct {
	Stage        stagelabels.Stage
	Reason       string
	ActivityHead string
}

// ReviewFacts is THE "what counts as reviewed" fact (ADR-0025 Decision §6).
type ReviewFacts struct {
	State         stagelabels.VerdictState // folded for HeadSHA
	Verdict       string                   // raw swarm verdict, "" when none
	VerdictKnown  bool
	HumanApprover string // trusted human whose APPROVED review matches HeadSHA
	// HumanChangeRequester is the trusted human whose change request
	// (review or `/hive decision <text>`) stands against HeadSHA; it
	// outranks a requires_human verdict on the same head (#14679).
	HumanChangeRequester string
	HumanReviews         int // review submissions by people on this head
	HumanReviewsKnown    bool
	// DecisionRequested is set when the swarm's requires_human verdict was a
	// maintainer decision request (recorded reason
	// contributor-lane:decision-requested rather than
	// reviewer-verdict:requires_human). Additive to the ADR-0025 shape: the
	// legacy verdict carries it as a separate flag the deriver reads.
	DecisionRequested bool
	// DecisionAnswered is set when a trusted human answered the verdict on
	// HeadSHA (`/hive decision` or a change-request review); DecisionDirective
	// is approve or changes_requested. The answer is what keeps a
	// decision-requested head out of 085 on every later derivation
	// (hivecommons/hive#14679).
	DecisionAnswered  bool
	DecisionDirective string
}

// LaneFact is one automerge lane's view of the PR this cycle. It is the
// typed form of today's stageObservation + stageDetail; the automerge adapter
// fills it, the PR deriver reads it.
type LaneFact struct {
	Lane          string // "self-authored" | "trusted-author" | <contributor tier>
	Reason        string // lane skip/admission reason (today's reason constants)
	Err           bool
	Merged        bool
	Role          stagelabels.Role
	RedChecks     []string // completed-red non-required checks on RedChecksHead
	RedChecksHead string
}
