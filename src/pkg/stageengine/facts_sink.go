package stageengine

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// FactSink is what out-of-cycle writers call instead of touching labels.
// Each call reconciles that one item immediately (fresh read, single
// writer), so a claim's stage lands as fast as it does today.
type FactSink interface {
	Claimed(ctx context.Context, ref Ref, c ClaimFact) error
	ClaimReleased(ctx context.Context, ref Ref, holder string) error
	InProgress(ctx context.Context, ref Ref, agent string, until time.Time) error
	CoveredBy(ctx context.Context, issue Ref, pr Ref) error
	// AddressedBy is CoveredBy for a PR that references the issue without
	// a closing keyword ("Refs #N"): still 040 — the hive is working it —
	// but the reason says "addresses part of", never "covers"
	// (ClaimFact.Partial; hivecommons/hive#15008).
	AddressedBy(ctx context.Context, issue Ref, pr Ref) error
	Uncovered(ctx context.Context, issue Ref, pr Ref) error
	Parked(ctx context.Context, ref Ref, label string) error // agent escalation (045/085/needs-human)
	Unparked(ctx context.Context, ref Ref, by string) error  // /hive approve|decision
	Closed(ctx context.Context, ref Ref) error               // the issue closed; its stage lifts
	// PRDecided is a trusted merger's `/hive decision` answered on a PR
	// parked at 085: the decision is recorded for Head, so the park lifts
	// now — in the same breath as the reply — instead of waiting for the
	// next sweep's lanes to notice (hivecommons/hive#14679). It returns
	// the transition the reconcile wrote, for the caller's audit line.
	PRDecided(ctx context.Context, ref Ref, d PRDecisionFact) (Transition, error)
}

// PRDecisionFact is what a `/hive decision` on a PR establishes for the
// stage: who decided, on which head, and whether the decision carries an
// instruction the author agent must act on (→ fixing) or is a bare approval
// (→ the merge path, decided by the lanes on their next pass).
type PRDecisionFact struct {
	Head        string
	By          string
	Instruction bool
	// SelfAuthored says the hive opened the PR (the self lane owns it and
	// fixing is 070); a PR it did not open is handed back to its author.
	SelfAuthored bool
}

// NeedsHumanLabel is the non-stage park label an agent may request; a
// Parked(...) carrying it sets ParkFacts.NeedsHuman.
const NeedsHumanLabel = "needs-human"

// NeedsHumanApplier is who put `needs-human` on an item, read from its label
// timeline: the last `labeled needs-human` actor classified against the
// hive's own accounts.
type NeedsHumanApplier int

const (
	// NeedsHumanApplierUnknown: no `labeled` event was found or the timeline
	// could not be read. An authorized ordinary un-park still lifts it;
	// only the reply's attribution is unknown.
	NeedsHumanApplierUnknown NeedsHumanApplier = iota
	// NeedsHumanApplierHive: one of the hive's accounts (an agent's `label
	// needs-human` + `/unclaim`, the relay, the task-list sweep).
	NeedsHumanApplierHive
	// NeedsHumanApplierPerson: anyone else.
	NeedsHumanApplierPerson
)

// DecisionLiftsNeedsHuman is the one rule for whether a maintainer's
// `/hive approve` / `/hive decision <text>` lifts `needs-human` along with
// the park it derives (stage/045-needs-decision). The label means "a person
// must answer"; the command IS a person answering, so it lifts whoever
// applied it:
//
//   - the hive asked (an agent parked the issue on a person): the
//     maintainer's decision is that answer — otherwise the next reconcile
//     re-derives 045 from the surviving source label and the un-park
//     silently reverts (hivecommons/hive#13075, seen on #12984);
//   - a person asked, or nobody can say who did: an explicit command from a
//     maintainer with write access is a stronger, later signal than a label
//     they (or anyone) added earlier — refusing it left the issue parked
//     and the tracker reading "Needs human decision" after the decision
//     was given (hivecommons/docs#419). A maintainer who wants agents to
//     stay off the issue after deciding says so with `hold`, which no
//     command ever touches.
//
// The applier still matters for the reply, which attributes the cleared
// label. The engine never writes `needs-human` itself; the command handler
// that owns source labels removes it and then submits the Unparked fact, so
// the derived stage follows from truth.
func DecisionLiftsNeedsHuman(applier NeedsHumanApplier) bool {
	_ = applier
	return true
}

// FactSinkOptions configures NewFactSink.
type FactSinkOptions struct {
	Now func() time.Time
	// Enrich, when set, tops the minimal Facts up before the reconcile —
	// typically a fresh read of the item's labels and state so the writer
	// plans against truth. It receives the Facts the sink built from the
	// submitted fact and returns what Reconcile should see.
	Enrich func(ctx context.Context, f Facts) (Facts, error)
}

type factSink struct {
	r    Reconciler
	opts FactSinkOptions
}

// NewFactSink returns the default FactSink: each submission builds the
// minimal Facts the fact implies (the item is open; the fact is its bucket,
// claim or park state), runs the optional Enrich hook and reconciles that
// one item now through r.
func NewFactSink(r Reconciler, opts FactSinkOptions) FactSink {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &factSink{r: r, opts: opts}
}

func (s *factSink) submit(ctx context.Context, f Facts) error {
	f.ObservedAt = s.opts.Now()
	f.Open = true
	if s.opts.Enrich != nil {
		var err error
		if f, err = s.opts.Enrich(ctx, f); err != nil {
			return err
		}
	}
	_, err := s.r.Reconcile(ctx, f)
	return err
}

func issueFacts(ref Ref, issue IssueFacts) Facts {
	return Facts{Ref: ref, Issue: &issue}
}

// Claimed implements FactSink: a live claim → 020, or 030 when it carries
// PR evidence (ClaimFact.PRStarted).
func (s *factSink) Claimed(ctx context.Context, ref Ref, c ClaimFact) error {
	return s.submit(ctx, issueFacts(ref, IssueFacts{Bucket: BucketClaimed, Claim: &c}))
}

// ClaimReleased implements FactSink: the claim lapsed with no PR → 010.
func (s *factSink) ClaimReleased(ctx context.Context, ref Ref, holder string) error {
	return s.submit(ctx, issueFacts(ref, IssueFacts{Bucket: BucketActionable}))
}

// InProgress implements FactSink: an agent kick is running on the issue
// with no PR evidence yet → 020 (030 needs a PR under way; see
// ClaimFact.PRStarted).
func (s *factSink) InProgress(ctx context.Context, ref Ref, agent string, until time.Time) error {
	return s.submit(ctx, issueFacts(ref, IssueFacts{
		Bucket: BucketInProgress,
		Claim:  &ClaimFact{Holder: agent, Kind: claims.KindAgent, Until: until},
	}))
}

// CoveredBy implements FactSink: an open PR covers the issue → 040.
func (s *factSink) CoveredBy(ctx context.Context, issue Ref, pr Ref) error {
	pr.Kind = stagelabels.PRItem
	return s.submit(ctx, issueFacts(issue, IssueFacts{
		Bucket:     BucketCoveredByPR,
		CoveringPR: &pr,
		Claim:      &ClaimFact{PRRepo: pr.FullRepo(), PRNumber: pr.Number},
	}))
}

// AddressedBy implements FactSink: an open PR references the issue
// without closing it → 040 with the partial reason.
func (s *factSink) AddressedBy(ctx context.Context, issue Ref, pr Ref) error {
	pr.Kind = stagelabels.PRItem
	return s.submit(ctx, issueFacts(issue, IssueFacts{
		Bucket:     BucketCoveredByPR,
		CoveringPR: &pr,
		Claim:      &ClaimFact{PRRepo: pr.FullRepo(), PRNumber: pr.Number, Partial: true},
	}))
}

// Uncovered implements FactSink: the covering PR closed unmerged → 010.
// The closed PR rides along so enrichment does not read the store's
// memory of that same PR as a cover that still stands.
func (s *factSink) Uncovered(ctx context.Context, issue Ref, pr Ref) error {
	pr.Kind = stagelabels.PRItem
	return s.submit(ctx, issueFacts(issue, IssueFacts{Bucket: BucketActionable, ClosedPR: &pr}))
}

// Parked implements FactSink: an agent (or a person) parked the item on a
// human with label → 045 on an issue, 085 on a PR.
func (s *factSink) Parked(ctx context.Context, ref Ref, label string) error {
	label = strings.TrimSpace(label)
	f := Facts{Ref: ref, Labels: []string{label}}
	f.Park = ParkFacts{
		Holds:         stagelabels.Holds(f.Labels),
		NeedsHuman:    strings.EqualFold(label, NeedsHumanLabel),
		DecisionLabel: label,
	}
	if ref.Kind == stagelabels.PRItem {
		f.PR = &PRFacts{}
	} else {
		f.Issue = &IssueFacts{Bucket: BucketFiltered, FilteredReason: label}
	}
	return s.submit(ctx, f)
}

// Unparked implements FactSink: /hive approve or /hive decision recorded by
// by; the park lifts. Whether `needs-human` survives is the caller's call
// under DecisionLiftsNeedsHuman — the deriver decides from the labels Enrich
// reads, so a kept `needs-human` re-derives 045 (#12715) and a lifted one
// lets the item reach 010 (#13075).
func (s *factSink) Unparked(ctx context.Context, ref Ref, by string) error {
	f := Facts{Ref: ref, Park: ParkFacts{ApprovedDirection: true}}
	if ref.Kind == stagelabels.PRItem {
		f.PR = &PRFacts{}
	} else {
		f.Issue = &IssueFacts{Bucket: BucketActionable}
	}
	return s.submit(ctx, f)
}

// PRDecided implements FactSink. A decision with an instruction is the
// self lane's human-changes-requested opinion on Head with the decider as
// the standing change requester, so ClassifyStage derives fixing over the
// reviewer's decision-requested verdict on the same head. A bare approval
// only lifts the park direction: the lanes release requires_human from the
// recorded approval on their next pass, and the deriver abstains here.
func (s *factSink) PRDecided(ctx context.Context, ref Ref, d PRDecisionFact) (Transition, error) {
	ref.Kind = stagelabels.PRItem
	f := Facts{Ref: ref, ObservedAt: s.opts.Now(), Open: true, Park: ParkFacts{ApprovedDirection: true}}
	head := strings.TrimSpace(d.Head)
	pr := &PRFacts{HeadSHA: head}
	if d.Instruction && head != "" && strings.TrimSpace(d.By) != "" {
		lane := stageLaneSelf
		if !d.SelfAuthored {
			lane = stageLaneTrusted
		}
		pr.Review.HumanChangeRequester = strings.TrimSpace(d.By)
		pr.Review.DecisionAnswered, pr.Review.DecisionDirective = true, prstage.DecisionDirectiveChanges
		pr.Lanes = []LaneFact{{Lane: lane, Reason: autoMergeReasonHumanChangesRequested}}
	} else if head != "" && strings.TrimSpace(d.By) != "" {
		pr.Review.HumanApprover = strings.TrimSpace(d.By)
		pr.Review.DecisionAnswered, pr.Review.DecisionDirective = true, prstage.DecisionDirectiveApprove
	}
	f.PR = pr
	if s.opts.Enrich != nil {
		var err error
		if f, err = s.opts.Enrich(ctx, f); err != nil {
			return Transition{Ref: ref}, err
		}
	}
	return s.r.Reconcile(ctx, f)
}

// Closed implements FactSink: the issue closed (for any reason — a merged
// fix, a maintainer's `/fixed`, a manual close) and is out of the pipeline;
// the deriver answers a closed item with Terminal/None, so the writer takes
// every stage/* label off and the Store forgets it. Open is pinned false
// after Enrich: the close is the fact being reported, and a fresh read
// that still says open (eventual consistency) must not re-stage the item.
func (s *factSink) Closed(ctx context.Context, ref Ref) error {
	f := Facts{Ref: ref, ObservedAt: s.opts.Now(), Open: false}
	if ref.Kind == stagelabels.PRItem {
		f.PR = &PRFacts{}
	} else {
		f.Issue = &IssueFacts{Bucket: BucketActionable}
	}
	if s.opts.Enrich != nil {
		var err error
		if f, err = s.opts.Enrich(ctx, f); err != nil {
			return err
		}
	}
	f.Open = false
	_, err := s.r.Reconcile(ctx, f)
	return err
}
