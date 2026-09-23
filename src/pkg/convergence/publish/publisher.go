package publish

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/forge"
)

// Typed publisher errors. Callers match with errors.Is; every refusal is also
// written to the audit sink so an operator can see why nothing was filed.
var (
	// ErrInvalidFinding: the finding lacks identity, receipt, or validation.
	ErrInvalidFinding = errors.New("finding is not publishable")
	// ErrLevelBelowFloor: the hive's ACMM level grants no issue-writing
	// authority (below config.PublicationMinACMMLevel).
	ErrLevelBelowFloor = errors.New("publication refused: ACMM level grants no issue-writing authority")
	// ErrNoPrivateChannel: a security-sensitive finding has nowhere private
	// to go, and a public issue is never an acceptable fallback.
	ErrNoPrivateChannel = errors.New("publication refused: security-sensitive finding has no private channel")
	// ErrNoIssueSeam: no forge issue seam is wired, so nothing can be filed.
	ErrNoIssueSeam = errors.New("publication refused: no issue seam is wired")
	// ErrPublicationUncertain: the effect ran but its result is unknown; the
	// journal holds it for reconciliation before any retry.
	ErrPublicationUncertain = errors.New("publication outcome uncertain, reconciliation required")
)

// Publication states recorded on the finding's publication bead.
const (
	// StatePublished: a public issue was filed (or replayed) for the finding.
	StatePublished = "published"
	// StateExisting: an open issue already carried the finding marker.
	StateExisting = "existing"
	// StatePrivate: the finding went to the private channel.
	StatePrivate = "private"
	// StateWithheld: policy withheld publication without error (disabled, or
	// a convergence mode that never writes).
	StateWithheld = "withheld"
	// StateRefused: publication was refused with a typed error.
	StateRefused = "refused"
	// StateNone: nothing was attempted.
	StateNone = "none"
)

// Withhold and refusal reasons.
const (
	ReasonDisabled         = "disabled"
	ReasonMode             = "mode"
	ReasonLevel            = "level"
	ReasonNoPrivateChannel = "no-private-channel"
	ReasonNoIssueSeam      = "no-issue-seam"
	ReasonInvalid          = "invalid"
	ReasonUncertain        = "uncertain"
)

// Audit actions the publisher records.
const (
	AuditFindingPublished = "finding_published"
	AuditFindingDisclosed = "finding_disclosed_privately"
	AuditFindingWithheld  = "finding_publication_withheld"
	AuditFindingRefused   = "finding_publication_refused"
)

// publicationGeneration is the desired generation every publication effect
// is journaled under. The logical ID must follow the finding identity alone
// so a campaign rerun replays the recorded issue; the campaign's own
// generation lives in the outcome ledger, not in the effect identity.
const publicationGeneration = 1

// publicationTransition names the hive transition in the journal entry.
const publicationTransition = "audit-publication"

// DefaultClaimTTL bounds a claim the publisher acquires for itself when the
// caller supplies no grant.
const DefaultClaimTTL = 10 * time.Minute

// claimSubjectSuffix distinguishes the publisher's own claim from the
// campaign's inspection claim on the same repo.
const claimSubjectSuffix = ":publication"

// Effect inputs.
const (
	inputCampaign  = "campaign"
	inputFinding   = "finding_hash"
	inputChannel   = "channel"
	channelPublic  = "public"
	channelPrivate = "private"
)

// Result provenance keys journaled for an applied publication.
const (
	resultIssueKey   = "issue="
	resultURLKey     = "url="
	resultPrivateKey = "private="
)

// PublicLabel is attached to every public issue the publisher files.
const PublicLabel = "audit-finding"

// Policy is the authorization snapshot a publication is judged against: the
// operator opt-in, the hive's ACMM level, and the resolved convergence mode.
type Policy struct {
	Enabled   bool
	ACMMLevel int
	Mode      string
}

// Grant is the mutation claim a publication runs under. The campaign passes
// its own claim so the publisher never competes with it for the repo's
// writer slot; a zero Grant makes the publisher acquire its own.
type Grant struct {
	ClaimKey string
	Epoch    uint64
	Holder   string
}

// Publication is the recorded result of one publish call.
type Publication struct {
	State  string
	Reason string
	// Ref is the issue reference for published and existing states.
	Ref forge.IssueRef
	// Private is the disclosure reference for the private state.
	Private DisclosureRef
	// Replayed reports that the journal returned a recorded result instead
	// of running the effect again.
	Replayed bool
}

// Record renders the publication bead value: "published:42", "existing:42",
// "private:repo:acme/sec#7", "withheld:mode", "refused:level", "none".
func (p Publication) Record() string {
	switch p.State {
	case StatePublished, StateExisting:
		return p.State + ":" + strconv.Itoa(p.Ref.Number)
	case StatePrivate:
		return StatePrivate + ":" + p.Private.Ref
	case StateWithheld, StateRefused:
		return p.State + ":" + p.Reason
	}
	return StateNone
}

// Publisher is the one trusted publication effect.
type Publisher struct {
	// Executor journals every publication under the finding-derived logical
	// ID; its Mode decides whether anything is written at all.
	Executor mutation.Executor
	// Issues is the forge issue seam public findings file through.
	Issues forge.IssueSeam
	// Private is the channel security-sensitive findings go to; nil refuses.
	Private PrivateChannel
	// Classify decides sensitivity; nil means ConservativeClassifier.
	Classify Classifier
	// Proofs, when set, receives a hive.finding.published/v1 receipt.
	Proofs *proof.Store
	// PolicyFunc returns the live authorization snapshot per publication.
	PolicyFunc func() Policy
	// Actor is the campaign owner principal publications run under.
	Actor string
	// Audit receives one entry per publication, withhold, or refusal.
	Audit  agentaudit.AuditSink
	Logger *slog.Logger
	// Now supplies the clock; nil means time.Now.
	Now func() time.Time
	// ClaimTTL bounds a self-acquired claim; zero means DefaultClaimTTL.
	ClaimTTL time.Duration
	// AfterEffect runs after the forge effect and before its result is
	// acknowledged; an error leaves the operation Unknown. Test seam for the
	// crash window.
	AfterEffect func(Finding) error
}

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Publisher) policy() Policy {
	if p.PolicyFunc == nil {
		return Policy{}
	}
	return p.PolicyFunc()
}

func (p *Publisher) classify(f Finding) Sensitivity {
	if p.Classify == nil {
		return ConservativeClassifier(f)
	}
	return p.Classify(f)
}

func (p *Publisher) audit(action string, f Finding, fields map[string]any) {
	if p.Audit == nil {
		return
	}
	base := agentaudit.Fields("campaign", f.Campaign, "repo", f.Repo, "finding", f.ContentHash, "bead", f.BeadID)
	for k, v := range fields {
		base[k] = v
	}
	p.Audit.Record(p.Actor, action, proof.ProducerHivePublisher, base)
}

func (p *Publisher) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// Publish files one validated finding exactly once, or routes it privately,
// or withholds or refuses it according to policy. It never files a
// security-sensitive finding publicly, and it never performs a forge write
// when publication is disabled or the convergence mode is not enforce.
func (p *Publisher) Publish(ctx context.Context, f Finding, grant Grant) (Publication, error) {
	if err := f.Validate(); err != nil {
		p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonInvalid, "error", err.Error()))
		return Publication{State: StateRefused, Reason: ReasonInvalid}, err
	}
	pol := p.policy()
	if !pol.Enabled {
		p.audit(AuditFindingWithheld, f, agentaudit.Fields("outcome", ReasonDisabled))
		return Publication{State: StateWithheld, Reason: ReasonDisabled}, nil
	}
	if pol.ACMMLevel < config.PublicationMinACMMLevel {
		p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonLevel, "acmm_level", pol.ACMMLevel, "min_level", config.PublicationMinACMMLevel))
		return Publication{State: StateRefused, Reason: ReasonLevel},
			fmt.Errorf("%w: level %d is below L%d", ErrLevelBelowFloor, pol.ACMMLevel, config.PublicationMinACMMLevel)
	}
	if pol.Mode != config.ConvergenceModeEnforce {
		p.audit(AuditFindingWithheld, f, agentaudit.Fields("outcome", ReasonMode, "mode", pol.Mode))
		return Publication{State: StateWithheld, Reason: ReasonMode}, nil
	}
	if p.classify(f) == SensitivitySensitive {
		return p.disclose(ctx, f, grant, pol.Mode)
	}
	return p.file(ctx, f, grant, pol.Mode)
}

// file publishes a public finding as an issue.
func (p *Publisher) file(ctx context.Context, f Finding, grant Grant, mode string) (Publication, error) {
	if p.Issues == nil {
		p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonNoIssueSeam))
		return Publication{State: StateRefused, Reason: ReasonNoIssueSeam}, ErrNoIssueSeam
	}
	marker := f.Marker()
	lookup := func(ctx context.Context) (string, bool, error) {
		ref, ok, err := p.Issues.FindIssueByMarker(ctx, f.Repo, marker)
		if err != nil || !ok {
			return "", false, err
		}
		return issueResult(ref), true, nil
	}
	effect := p.effect(f, grant, mutation.EffectCreateIssue, channelPublic)
	res, err := p.execute(ctx, f, grant, mode, effect, lookup, func() (string, error) {
		ref, err := p.Issues.CreateIssue(ctx, f.Repo, f.Title, f.Body(), append([]string{PublicLabel}, f.Labels...))
		if err != nil {
			return "", err
		}
		return issueResult(ref), nil
	})
	if err != nil {
		return Publication{State: StateRefused, Reason: ReasonUncertain}, err
	}
	ref := parseIssueResult(res.result)
	if res.existing {
		// An open issue already carries the marker: the repository outcome is
		// satisfied and nothing is filed.
		p.audit(AuditFindingWithheld, f, agentaudit.Fields("outcome", StateExisting, "issue", ref.Number))
		return Publication{State: StateExisting, Ref: ref}, nil
	}
	p.recordProof(f, ref.Number)
	p.audit(AuditFindingPublished, f, agentaudit.Fields("outcome", StatePublished, "issue", ref.Number, "replayed", res.replayed))
	return Publication{State: StatePublished, Ref: ref, Replayed: res.replayed}, nil
}

// disclose routes a security-sensitive finding to the private channel.
func (p *Publisher) disclose(ctx context.Context, f Finding, grant Grant, mode string) (Publication, error) {
	if p.Private == nil {
		p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonNoPrivateChannel))
		return Publication{State: StateRefused, Reason: ReasonNoPrivateChannel}, ErrNoPrivateChannel
	}
	// Pre-flight: an unroutable channel is a refusal decided here, before any
	// journal entry exists, never an uncertain effect awaiting reconciliation.
	if checker, ok := p.Private.(ReadyChecker); ok {
		if err := checker.Ready(); err != nil {
			p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonNoPrivateChannel, "error", err.Error()))
			return Publication{State: StateRefused, Reason: ReasonNoPrivateChannel}, err
		}
	}
	marker := f.Marker()
	var lookup func(context.Context) (string, bool, error)
	if finder, ok := p.Private.(DisclosureFinder); ok {
		lookup = func(ctx context.Context) (string, bool, error) {
			ref, ok, err := finder.FindDisclosure(ctx, marker)
			if err != nil || !ok {
				return "", false, err
			}
			return privateResult(ref), true, nil
		}
	}
	effect := p.effect(f, grant, mutation.EffectPrivateDisclosure, channelPrivate)
	res, err := p.execute(ctx, f, grant, mode, effect, lookup, func() (string, error) {
		ref, err := p.Private.Disclose(ctx, Disclosure{Finding: f, Body: f.Body(), Marker: marker})
		if err != nil {
			return "", err
		}
		return privateResult(ref), nil
	})
	if err != nil {
		return Publication{State: StateRefused, Reason: ReasonUncertain}, err
	}
	ref := parsePrivateResult(res.result)
	replayed := res.replayed || res.existing
	if !res.existing {
		p.recordProof(f, ref.Number)
	}
	p.audit(AuditFindingDisclosed, f, agentaudit.Fields("outcome", StatePrivate, "ref", ref.Ref, "replayed", replayed))
	return Publication{State: StatePrivate, Private: ref, Replayed: replayed}, nil
}

// effect is the journaled description of one publication. Its logical ID
// derives from the campaign and finding hash (plus the channel), never from
// the holder, epoch, or attempt, so a retry adopts the same entry.
func (p *Publisher) effect(f Finding, grant Grant, kind, channel string) mutation.Effect {
	return mutation.Effect{
		OutcomeKey:        f.Repo + "@" + f.Campaign,
		DesiredGeneration: publicationGeneration,
		Transition:        publicationTransition,
		Subject:           f.Repo + "!" + f.Campaign + ":" + f.ContentHash,
		ClaimKey:          grant.ClaimKey,
		Kind:              kind,
		Inputs:            map[string]string{inputCampaign: f.Campaign, inputFinding: f.ContentHash, inputChannel: channel},
	}
}

// execResult is what execute hands back: the journaled result provenance,
// whether it came from a journal replay, and whether the forge already held
// the effect (found by marker) so nothing was journaled or run.
type execResult struct {
	result   string
	replayed bool
	existing bool
}

// execute runs the effect through the mutation executor under the grant. In
// order: reconcile an earlier unresolved attempt for the same logical
// operation from authoritative forge state; dedupe against an effect the
// forge already holds; then run the effect, returning the recorded result on
// a journal replay.
func (p *Publisher) execute(ctx context.Context, f Finding, grant Grant, mode string, effect mutation.Effect,
	lookup func(context.Context) (string, bool, error), run func() (string, error)) (execResult, error) {
	// The executor judges under the SAME resolved mode the policy check used,
	// so a live mode flip between boot and this publication cannot leave the
	// journal and the policy disagreeing about whether anything was written.
	executor := p.Executor
	executor.Mode = mode
	if grant.ClaimKey == "" {
		acquired, release, err := p.acquire(f)
		if err != nil {
			p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonUncertain, "error", err.Error()))
			return execResult{}, err
		}
		defer release()
		grant = acquired
		effect.ClaimKey = grant.ClaimKey
	}
	if err := p.reconcile(ctx, effect, lookup); err != nil {
		p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonUncertain, "error", err.Error()))
		return execResult{}, err
	}
	if lookup != nil {
		// A lookup failure is not authoritative: fall through to the journal,
		// which still refuses a second effect for an Applied operation.
		if found, ok, err := lookup(ctx); err == nil && ok {
			return execResult{result: found, existing: true}, nil
		}
	}
	op, err := executor.Execute(effect, grant.Epoch, grant.Holder, func() (string, error) {
		result, err := run()
		if err != nil {
			return "", err
		}
		if p.AfterEffect != nil {
			if err := p.AfterEffect(f); err != nil {
				return "", err
			}
		}
		return result, nil
	})
	if errors.Is(err, mutation.ErrAlreadyApplied) {
		return execResult{result: op.Result, replayed: true}, nil
	}
	if err != nil {
		p.audit(AuditFindingRefused, f, agentaudit.Fields("outcome", ReasonUncertain, "error", err.Error()))
		return execResult{}, fmt.Errorf("%w: %v", ErrPublicationUncertain, err)
	}
	return execResult{result: op.Result}, nil
}

// reconcile resolves an unresolved earlier attempt for the same logical
// operation from authoritative forge state before any retry: found by marker
// means Applied (replayed, never repeated); an authoritative miss authorizes
// one retry; no lookup leaves it Unknown, which Begin then refuses.
func (p *Publisher) reconcile(ctx context.Context, effect mutation.Effect, lookup func(context.Context) (string, bool, error)) error {
	if p.Executor.Journal == nil {
		return nil
	}
	prior, ok := p.Executor.Journal.Get(effect.LogicalID())
	if !ok || (prior.Status != mutation.StatusPlanned && prior.Status != mutation.StatusUnknown) {
		return nil
	}
	state := mutation.ExternalState{}
	if lookup != nil {
		result, found, err := lookup(ctx)
		if err == nil {
			state = mutation.ExternalState{Known: true, Applied: found, Result: result}
		}
	}
	if _, err := p.Executor.Journal.Reconcile(prior.LogicalID, state, p.now()); err != nil {
		return fmt.Errorf("%w: %v", ErrPublicationUncertain, err)
	}
	if !state.Known {
		return fmt.Errorf("%w: earlier attempt for %s could not be reconciled", ErrPublicationUncertain, effect.Subject)
	}
	return nil
}

// acquire takes the publisher's own task claim when the caller passed none.
func (p *Publisher) acquire(f Finding) (Grant, func(), error) {
	if p.Executor.Ledger == nil {
		return Grant{}, nil, fmt.Errorf("%w: publisher has no mutation ledger to claim through", ErrPublicationUncertain)
	}
	ttl := p.ClaimTTL
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	holder := p.Actor
	if holder == "" {
		holder = proof.ProducerHivePublisher
	}
	claim := mutation.TaskClaim(f.Repo, f.Repo+"!"+f.Campaign+claimSubjectSuffix)
	entry, err := p.Executor.Ledger.Acquire(claim, holder, ttl, p.now())
	if err != nil {
		return Grant{}, nil, fmt.Errorf("%w: %v", ErrPublicationUncertain, err)
	}
	release := func() {
		if _, err := p.Executor.Ledger.Release(claim.Key(), entry.Epoch, p.now()); err != nil {
			p.logger().Warn("publication claim release failed", "claim", claim.Key(), "error", err)
		}
	}
	return Grant{ClaimKey: claim.Key(), Epoch: entry.Epoch, Holder: holder}, release, nil
}

// recordProof writes the hive.finding.published/v1 receipt binding the issue
// number and finding hash. A disclosure without a number (notify) leaves no
// receipt: there is no bounded evidence to bind.
func (p *Publisher) recordProof(f Finding, number int) {
	if p.Proofs == nil || number < 1 {
		return
	}
	rec := proof.Record{
		Fingerprint: proof.Fingerprint{
			OutcomeKey:        f.Repo + "@" + f.Campaign,
			PredicateID:       proof.PredicateFindingPublished,
			DesiredGeneration: publicationGeneration,
			Producer:          proof.ProducerHivePublisher,
			IssueNumber:       number,
			FindingHash:       f.ContentHash,
		},
		Result:     proof.ResultSuccess,
		Provenance: proof.Provenance{Query: "publish@" + f.ContentHash},
		ObservedAt: p.now(),
	}
	if _, err := p.Proofs.Put(rec); err != nil {
		p.logger().Warn("publication proof not recorded", "finding", f.ContentHash, "error", err)
	}
}

func issueResult(ref forge.IssueRef) string {
	return resultIssueKey + strconv.Itoa(ref.Number) + " " + resultURLKey + ref.URL
}

func parseIssueResult(result string) forge.IssueRef {
	var ref forge.IssueRef
	for _, field := range strings.Fields(result) {
		switch {
		case strings.HasPrefix(field, resultIssueKey):
			ref.Number, _ = strconv.Atoi(strings.TrimPrefix(field, resultIssueKey))
		case strings.HasPrefix(field, resultURLKey):
			ref.URL = strings.TrimPrefix(field, resultURLKey)
		}
	}
	return ref
}

func privateResult(ref DisclosureRef) string {
	return resultPrivateKey + ref.Ref + " " + resultIssueKey + strconv.Itoa(ref.Number)
}

func parsePrivateResult(result string) DisclosureRef {
	var ref DisclosureRef
	for _, field := range strings.Fields(result) {
		switch {
		case strings.HasPrefix(field, resultPrivateKey):
			ref.Ref = strings.TrimPrefix(field, resultPrivateKey)
		case strings.HasPrefix(field, resultIssueKey):
			ref.Number, _ = strconv.Atoi(strings.TrimPrefix(field, resultIssueKey))
		}
	}
	return ref
}
