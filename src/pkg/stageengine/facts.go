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
)

// IssueFacts is the issue-side evidence.
type IssueFacts struct {
	Bucket         IssueBucket // Actionable | Claimed | InProgress | CoveredByPR | Filtered(reason)
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
}

// ClaimFact is the live claim on an issue.
type ClaimFact struct {
	Holder   string
	Kind     claims.Kind // KindAgent → 030, anything else → 020
	Until    time.Time
	PRRepo   string
	PRNumber int
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
	Lanes             []LaneFact // automerge lane opinions, precedence order (self > trusted > contributor)
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
	// AdoptionLive is prev.AdoptionLive(head): the hive owns the PR, so a
	// stage that yields to adoption reads 074. EscalationReason is set when
	// the stalled-adoption escape moved it to 085 instead.
	AdoptionLive     bool
	EscalationReason string
}

// PriorStage is the store record a pass started from.
type PriorStage struct {
	Stage        stagelabels.Stage
	Reason       string
	ActivityHead string
}

// ReviewFacts is THE "what counts as reviewed" fact (ADR-0025 Decision §6).
type ReviewFacts struct {
	State             stagelabels.VerdictState // folded for HeadSHA
	Verdict           string                   // raw swarm verdict, "" when none
	VerdictKnown      bool
	HumanApprover     string // trusted human whose APPROVED review matches HeadSHA
	HumanReviews      int    // review submissions by people on this head
	HumanReviewsKnown bool
	// DecisionRequested is set when the swarm's requires_human verdict was a
	// maintainer decision request (recorded reason
	// contributor-lane:decision-requested rather than
	// reviewer-verdict:requires_human). Additive to the ADR-0025 shape: the
	// legacy verdict carries it as a separate flag the deriver reads.
	DecisionRequested bool
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
