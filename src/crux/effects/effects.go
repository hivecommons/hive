// Package effects declares the small external-mutation boundary seam shared by
// callers that must not import the convergence implementation.
package effects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	KindPullRequestCreate = "pull_request_create"
	KindIssueCreate       = "issue_create"
	KindIssueComment      = "issue_comment"
	KindPullRequestMerge  = "pull_request_merge"
	KindLabelMutation     = "label_mutation"
	KindReviewSubmit      = "review_submit"
	KindReviewRequest     = "review_request"
	KindBranchPush        = "branch_push"
	KindBranchUpdate      = "branch_update"
	KindSubIssueLink      = "sub_issue_link"
	KindDependencyLink    = "dependency_link"
	KindStatusCreate      = "status_create"
)

var ErrDenied = errors.New("mutation boundary denied")

// ErrNotApplied marks an effect error as a DEFINITE refusal: the external
// system answered and the effect certainly did not take place, so a
// journaling boundary may record it NotApplied (retry authorized) rather than
// Unknown (reconciliation required). Wrap with NotApplied.
var ErrNotApplied = errors.New("external effect definitely not applied")

// ErrNeedsReconciliation reports that the boundary refused to run an effect
// because an earlier attempt at the same logical operation is unresolved and
// must be reconciled (see Reconcile) before any retry.
var ErrNeedsReconciliation = errors.New("external effect needs reconciliation before retry")

// NotApplied wraps err as a definite refusal (ErrNotApplied) while keeping
// err's own message and chain, so callers still see the original error.
func NotApplied(err error) error {
	if err == nil {
		return nil
	}
	return notAppliedError{err: err}
}

type notAppliedError struct{ err error }

func (e notAppliedError) Error() string { return e.err.Error() }

func (e notAppliedError) Unwrap() []error { return []error{e.err, ErrNotApplied} }

// ExternalState is one authoritative observation of whether a claimed effect
// exists externally, used to resolve an operation left unresolved.
type ExternalState struct {
	Applied    bool
	Provenance string
}

// Reconciler is implemented by boundaries that journal effects and can
// resolve an unresolved operation from authoritative external state.
type Reconciler interface {
	Reconcile(ctx context.Context, claim Claim, state ExternalState) error
}

// Reconcile resolves claim's operation on boundary from state. Boundaries that
// do not journal (nil, NoopBoundary) have nothing to resolve.
func Reconcile(ctx context.Context, boundary Boundary, claim Claim, state ExternalState) error {
	r, ok := boundary.(Reconciler)
	if !ok {
		return nil
	}
	return r.Reconcile(ctx, claim, state)
}

// Expirer is implemented by boundaries that journal effects and can give up
// on an operation left unresolved (Planned/Unknown) for longer than a TTL
// when no authoritative observation is obtainable.
type Expirer interface {
	ExpireUnresolved(ctx context.Context, claim Claim, ttl time.Duration) (bool, error)
}

// ExpireUnresolved resolves claim's operation to not-applied when the
// boundary journals it as unresolved and its last transition is older than
// ttl, authorizing a retry. It reports whether anything was expired.
// Boundaries that do not journal (nil, NoopBoundary) have nothing to expire.
//
// This is the escape hatch for an operation nothing can reconcile — the
// external lookup itself keeps failing — so an unresolved entry can never
// fence every identical request forever. Callers run their own duplicate
// check (an open-issue scan, an open-PR-by-head lookup) before any retry, so
// expiry trades a bounded chance of one duplicate for a guaranteed end to a
// deadlock.
func ExpireUnresolved(ctx context.Context, boundary Boundary, claim Claim, ttl time.Duration) (bool, error) {
	e, ok := boundary.(Expirer)
	if !ok {
		return false, nil
	}
	return e.ExpireUnresolved(ctx, claim, ttl)
}

// DefaultUnresolvedTTL bounds how long an operation may stay unresolved when
// its external state cannot be observed before ExecuteReconciling expires it
// so a retry may run. It outlasts a single GitHub incident while keeping a
// stuck entry from fencing identical requests for days.
const DefaultUnresolvedTTL = time.Hour

// ExecuteReconciling runs effect through boundary like Execute, but does not
// leave an unresolved operation fencing retries forever. When the boundary
// refuses with ErrNeedsReconciliation (an earlier attempt at the SAME logical
// operation was journaled Unknown or never acknowledged), observe reads the
// authoritative external state, the operation is resolved from it and the
// same logical operation is replayed once: an applied observation replays as
// idempotent success without running effect, a not-applied one runs it again.
// When observe is nil or fails, the operation is expired to not-applied once
// its last transition is older than ttl (DefaultUnresolvedTTL when zero) and
// effect runs again; a younger entry keeps the refusal.
func ExecuteReconciling(ctx context.Context, boundary Boundary, claim Claim, observe func(context.Context) (ExternalState, error), ttl time.Duration, effect func(context.Context) (Result, error)) (Result, error) {
	out, err := Execute(ctx, boundary, claim, effect)
	if !errors.Is(err, ErrNeedsReconciliation) {
		return out, err
	}
	journalErr := err
	lookupErr := errors.New("no external observation for " + claim.Kind)
	if observe != nil {
		var state ExternalState
		if state, lookupErr = observe(ctx); lookupErr == nil {
			if recErr := Reconcile(ctx, boundary, claim, state); recErr != nil {
				return Result{}, fmt.Errorf("%w (reconciliation failed: %v)", journalErr, recErr)
			}
			return Execute(ctx, boundary, claim, effect)
		}
	}
	if ttl <= 0 {
		ttl = DefaultUnresolvedTTL
	}
	expired, expErr := ExpireUnresolved(ctx, boundary, claim, ttl)
	if expErr != nil {
		return Result{}, fmt.Errorf("%w (reconciliation lookup failed: %v; expiry failed: %v)", journalErr, lookupErr, expErr)
	}
	if !expired {
		return Result{}, fmt.Errorf("%w (reconciliation lookup failed: %v)", journalErr, lookupErr)
	}
	return Execute(ctx, boundary, claim, effect)
}

// Lookup is implemented by boundaries that journal effects and can report,
// without running anything, whether claim's logical operation is already
// applied. Callers use it to recognise an equivalent operation under a
// neighbouring key (e.g. the previous idempotency bucket) that Execute's
// exact-ID replay cannot see.
type Lookup interface {
	LookupApplied(ctx context.Context, claim Claim) (Result, bool)
}

// LookupApplied reports whether boundary has claim's operation journaled as
// applied, with its recorded provenance. Boundaries that do not journal (nil,
// NoopBoundary) never report an applied operation.
func LookupApplied(ctx context.Context, boundary Boundary, claim Claim) (Result, bool) {
	l, ok := boundary.(Lookup)
	if !ok {
		return Result{}, false
	}
	return l.LookupApplied(ctx, claim)
}

type Claim struct {
	Repo   string
	Kind   string
	Target string
	Actor  string
	Inputs map[string]string
}

type Result struct {
	Provenance string
}

type Boundary interface {
	Execute(ctx context.Context, claim Claim, effect func(context.Context) (Result, error)) (Result, error)
}

// JournaledBoundary refuses to invoke effect without an active operation
// journal. Implementations freeze their effective mode for the whole operation;
// ordinary Boundary may deliberately pass effects through when journaling is off.
type JournaledBoundary interface {
	ExecuteJournaled(ctx context.Context, claim Claim, effect func(context.Context) (Result, error)) (Result, error)
}

type NoopBoundary struct{}

func (NoopBoundary) Execute(ctx context.Context, _ Claim, effect func(context.Context) (Result, error)) (Result, error) {
	return effect(ctx)
}

func Execute(ctx context.Context, boundary Boundary, claim Claim, effect func(context.Context) (Result, error)) (Result, error) {
	if boundary == nil {
		boundary = NoopBoundary{}
	}
	return boundary.Execute(ctx, claim, effect)
}

func StableDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func StableInputs(inputs map[string]string) string {
	if len(inputs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(inputs[k])
		b.WriteByte('\n')
	}
	return b.String()
}

type Stats struct {
	Mode      string `json:"mode"`
	Journaled int64  `json:"journaled"`
	Fenced    int64  `json:"fenced"`
	Denied    int64  `json:"denied"`
}

type Recorder struct {
	mu    sync.Mutex
	stats Stats
}

func (r *Recorder) SetMode(mode string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Mode = mode
}

func (r *Recorder) IncJournaled() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.Journaled++
	r.mu.Unlock()
}

func (r *Recorder) IncFenced() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.Fenced++
	r.mu.Unlock()
}

func (r *Recorder) IncDenied() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.Denied++
	r.mu.Unlock()
}

func (r *Recorder) Snapshot() Stats {
	if r == nil {
		return Stats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

type LoggingBoundary struct {
	Next     Boundary
	Logger   *slog.Logger
	Recorder *Recorder
}

func (b LoggingBoundary) Execute(ctx context.Context, claim Claim, effect func(context.Context) (Result, error)) (Result, error) {
	res, err := Execute(ctx, b.Next, claim, effect)
	return b.record(claim, res, err)
}

func (b LoggingBoundary) ExecuteJournaled(ctx context.Context, claim Claim, effect func(context.Context) (Result, error)) (Result, error) {
	journaled, ok := b.Next.(JournaledBoundary)
	if !ok {
		return Result{}, fmt.Errorf("%w: mutation boundary cannot require journaling", ErrDenied)
	}
	res, err := journaled.ExecuteJournaled(ctx, claim, effect)
	return b.record(claim, res, err)
}

func (b LoggingBoundary) record(claim Claim, res Result, err error) (Result, error) {
	if err != nil && errors.Is(err, ErrDenied) && b.Logger != nil {
		b.Logger.Warn("mutation boundary denied external effect",
			"repo", claim.Repo, "kind", claim.Kind, "target", claim.Target, "error", err)
	}
	if err == nil && b.Recorder != nil {
		b.Recorder.IncJournaled()
	}
	return res, err
}

func (b LoggingBoundary) Reconcile(ctx context.Context, claim Claim, state ExternalState) error {
	return Reconcile(ctx, b.Next, claim, state)
}

func (b LoggingBoundary) ExpireUnresolved(ctx context.Context, claim Claim, ttl time.Duration) (bool, error) {
	return ExpireUnresolved(ctx, b.Next, claim, ttl)
}

func (c Claim) Validate() error {
	if strings.TrimSpace(c.Repo) == "" || !strings.Contains(c.Repo, "/") {
		return fmt.Errorf("repo must be owner/name")
	}
	if strings.TrimSpace(c.Kind) == "" {
		return fmt.Errorf("kind is required")
	}
	if strings.TrimSpace(c.Target) == "" {
		return fmt.Errorf("target is required")
	}
	return nil
}
