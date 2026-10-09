package github

// Audit actions for the hive's own GitHub writes (#9587 phase 2).
//
// These writes have no agent request behind them: the hive performs them on
// its own schedule with the App token, so they are not subject to the lane
// allowlist. Phase 1 listed them in docs/github-write-surface.md as
// unaudited. Each is now recorded through recordWriteAudit, like the relay
// writes, with typed repo/target fields and redacted detail, under the
// governor system agent. An entry is written only when the write landed: a
// skipped, unchanged or failed write produced nothing on GitHub.
const (
	// AuditActionSignedCommitReauthored is recorded when the signed-commit
	// reconciler re-authors a hive PR's unsigned tail as one GitHub-signed
	// commit and moves the branch to it (pr_signed_reconcile.go).
	AuditActionSignedCommitReauthored = "signed_commit_reauthored"
	// AuditActionSignedCommitSkipNoted is recorded when the reconciler posts
	// its one-per-PR "cannot sign this branch" comment.
	AuditActionSignedCommitSkipNoted = "signed_commit_skip_noted"
	// AuditActionReviewBacklogIssueFiled is recorded for each backlog issue
	// filed (or reused and linked) from an out-of-scope review finding
	// (review_backlog.go). Target is the backlog issue; pr= names the PR.
	AuditActionReviewBacklogIssueFiled = "review_backlog_issue_filed"
	// AuditActionReviewBacklogSummaryPosted is recorded when the one summary
	// comment listing the filed backlog issues lands on the reviewed PR.
	AuditActionReviewBacklogSummaryPosted = "review_backlog_summary_posted"
	// AuditActionReviewBacklogBatchRouted is recorded once per backlog
	// filing pass that filed or updated at least one item, in any
	// destination (GitHub issue, Projects, Linear, Jira). Target is the PR.
	AuditActionReviewBacklogBatchRouted = "review_backlog_batch_routed"
	// AuditActionHiveLabelApplied is recorded when the hive applies an
	// existing repo label to a PR: the human-decision label and the review
	// priority labels (human_decision_label.go).
	AuditActionHiveLabelApplied = "hive_label_applied"
	// AuditActionRecommendationsPosted is recorded when the recommendations
	// issue is opened or its body rewritten (recommendations.go). An
	// unchanged body is not rewritten and not audited.
	AuditActionRecommendationsPosted = "recommendations_posted"
	// AuditActionFleetReportPosted is recorded when a fleet report issue is
	// opened, or an existing one gets a new comment (fleet_report.go).
	AuditActionFleetReportPosted = "fleet_report_posted"
	// AuditActionFleetReportRecovered is recorded when a recovery comment
	// lands on a fleet report issue (and the issue is closed, when the hive
	// opened it).
	AuditActionFleetReportRecovered = "fleet_report_recovered"
	// AuditActionHoldMigrationLabelAdded is recorded for each item the
	// one-time hold-label migration puts under the new hold label
	// (hive_hold_migration.go).
	AuditActionHoldMigrationLabelAdded = "hold_migration_label_added"
	// AuditActionReporterTrustWaitNoticed is recorded when the core poller
	// posts the one-shot reporter-trust wait explanation comment.
	AuditActionReporterTrustWaitNoticed = "reporter_trust_wait_noticed"
	// AuditActionReporterTrustWaitCleared is recorded when the core poller
	// removes a needs-triage label that its wait marker proves Hive added.
	AuditActionReporterTrustWaitCleared = "reporter_trust_wait_cleared"
)

// Outcome values for the internal-writer audit entries' "outcome" pair.
const (
	auditOutcomeCreated   = "created"
	auditOutcomeUpdated   = "updated"
	auditOutcomeCommented = "commented"
)

// hiveWriteMeta is the invocation metadata for a hive-internal write: the
// governor system agent, the same name the auto-merge and advisory writes use.
func hiveWriteMeta() InvocationMeta {
	return InvocationMeta{Agent: AttributionAgentGovernor}
}

// InternalWriteAuditActions returns every hive-internal writer's audit action,
// in a stable order. The docs inventory is checked against it.
func InternalWriteAuditActions() []string {
	return []string{
		AuditActionSignedCommitReauthored,
		AuditActionSignedCommitSkipNoted,
		AuditActionReviewBacklogIssueFiled,
		AuditActionReviewBacklogSummaryPosted,
		AuditActionReviewBacklogBatchRouted,
		AuditActionHiveLabelApplied,
		AuditActionRecommendationsPosted,
		AuditActionFleetReportPosted,
		AuditActionFleetReportRecovered,
		AuditActionHoldMigrationLabelAdded,
		AuditActionReporterTrustWaitNoticed,
		AuditActionReporterTrustWaitCleared,
	}
}
