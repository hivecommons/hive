// Package effects is the Hive-side name for the external-mutation boundary
// seam. The seam itself lives in the Crux ring as pkg/crux/effects (stdlib
// only; the runtime's sandbox push path and the agent-report schema use it),
// and this package re-exports every symbol so governor-side callers keep
// their import path: Crux owns, Hive aliases (hivecommons/hive#13536).
package effects

import cruxeffects "github.com/hivecommons/hive/crux/effects"

const (
	KindPullRequestCreate = cruxeffects.KindPullRequestCreate
	KindIssueCreate       = cruxeffects.KindIssueCreate
	KindIssueComment      = cruxeffects.KindIssueComment
	KindPullRequestMerge  = cruxeffects.KindPullRequestMerge
	KindLabelMutation     = cruxeffects.KindLabelMutation
	KindReviewSubmit      = cruxeffects.KindReviewSubmit
	KindReviewRequest     = cruxeffects.KindReviewRequest
	KindBranchPush        = cruxeffects.KindBranchPush
	KindBranchUpdate      = cruxeffects.KindBranchUpdate
	KindSubIssueLink      = cruxeffects.KindSubIssueLink
	KindDependencyLink    = cruxeffects.KindDependencyLink
	KindStatusCreate      = cruxeffects.KindStatusCreate

	DefaultUnresolvedTTL = cruxeffects.DefaultUnresolvedTTL
)

var (
	ErrDenied              = cruxeffects.ErrDenied
	ErrNotApplied          = cruxeffects.ErrNotApplied
	ErrNeedsReconciliation = cruxeffects.ErrNeedsReconciliation
)

type (
	Boundary          = cruxeffects.Boundary
	JournaledBoundary = cruxeffects.JournaledBoundary
	Claim             = cruxeffects.Claim
	Expirer           = cruxeffects.Expirer
	ExternalState     = cruxeffects.ExternalState
	LoggingBoundary   = cruxeffects.LoggingBoundary
	Lookup            = cruxeffects.Lookup
	NoopBoundary      = cruxeffects.NoopBoundary
	Reconciler        = cruxeffects.Reconciler
	Recorder          = cruxeffects.Recorder
	Result            = cruxeffects.Result
	Stats             = cruxeffects.Stats
)

var (
	Execute            = cruxeffects.Execute
	ExecuteReconciling = cruxeffects.ExecuteReconciling
	ExpireUnresolved   = cruxeffects.ExpireUnresolved
	LookupApplied      = cruxeffects.LookupApplied
	NotApplied         = cruxeffects.NotApplied
	Reconcile          = cruxeffects.Reconcile
	StableDigest       = cruxeffects.StableDigest
	StableInputs       = cruxeffects.StableInputs
)
