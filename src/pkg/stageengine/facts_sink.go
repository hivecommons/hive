package stageengine

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
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
	Uncovered(ctx context.Context, issue Ref, pr Ref) error
	Parked(ctx context.Context, ref Ref, label string) error // agent escalation (045/085/needs-human)
	Unparked(ctx context.Context, ref Ref, by string) error  // /hive approve|decision
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
	// could not be read. Not knowing is never permission to lift it.
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
// must answer"; who asked decides whose answer it is:
//
//   - the hive asked (an agent parked the issue on a person): the
//     maintainer's decision is that answer, so the label lifts and the
//     engine derives 010 — otherwise the next reconcile re-derives 045 from
//     the surviving source label and the un-park silently reverts
//     (hivecommons/hive#13075, seen on #12984);
//   - a person asked, or nobody can say who did: the label is a person's
//     hold and a decision never lifts it (hivecommons/hive#12680, #12681).
//
// The engine never writes `needs-human` itself; the command handler that
// owns source labels removes it when this returns true and then submits
// the Unparked fact, so the derived stage follows from truth.
func DecisionLiftsNeedsHuman(applier NeedsHumanApplier) bool {
	return applier == NeedsHumanApplierHive
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

// Claimed implements FactSink: a live claim → 020, or 030 for an agent.
func (s *factSink) Claimed(ctx context.Context, ref Ref, c ClaimFact) error {
	return s.submit(ctx, issueFacts(ref, IssueFacts{Bucket: BucketClaimed, Claim: &c}))
}

// ClaimReleased implements FactSink: the claim lapsed with no PR → 010.
func (s *factSink) ClaimReleased(ctx context.Context, ref Ref, holder string) error {
	return s.submit(ctx, issueFacts(ref, IssueFacts{Bucket: BucketActionable}))
}

// InProgress implements FactSink: an agent kick is running → 030.
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

// Uncovered implements FactSink: the covering PR closed unmerged → 010.
func (s *factSink) Uncovered(ctx context.Context, issue Ref, pr Ref) error {
	return s.submit(ctx, issueFacts(issue, IssueFacts{Bucket: BucketActionable}))
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
