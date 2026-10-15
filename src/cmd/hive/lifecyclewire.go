package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/tracing"
)

// timelineRef renders the canonical short "repo#number" journey key the
// enumeration/kick producers already use (actionable items carry org-less
// repo names). Sources that report a full "org/repo" (the attribution audit,
// the escalation sweep) are normalized here so all stages of one item land on
// ONE journey instead of splitting across "repo#N" and "org/repo#N".
func timelineRef(org, repo string, number int) string {
	repo = strings.TrimSpace(repo)
	if org != "" {
		repo = strings.TrimPrefix(repo, org+"/")
	}
	return issueRef(repo, number)
}

// parseAuditDetail parses the attribution audit's "k=v, k=v" detail string
// (github.InvocationMeta.AuditDetail) back into pairs. Defensive: segments
// without a "=" are skipped rather than failing the whole parse.
func parseAuditDetail(detail string) map[string]string {
	kv := map[string]string{}
	for _, part := range strings.Split(detail, ", ") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			continue
		}
		kv[k] = v
	}
	return kv
}

// recordLifecycleFromAudit bridges the attribution audit stream into the
// lifecycle timeline. Every mediated PR creation (agent_pr_created — the
// pr_request watcher) and every pr_merged trail entry (MergePR: the dashboard
// queue and merge-watcher merges humans drive through the hive, plus BOTH
// automerge sweep paths) already flows through this single sink, so one wire
// covers them all — including future emitters. The store dedupes by
// (ref, kind), so overlap with recordPROpened is a refresh, not a dupe.
//
// The work item comes from the typed Repo/Target audit fields (#9587); the
// legacy repo=/number= detail pairs are the fallback for a record that
// carries no typed target. Evidence attrs (url, sha, ...) still come from
// detail, which is the only place they are written.
func recordLifecycleFromAudit(rec lifecycleRecorder, org string, audit github.AuditRecord) {
	if rec == nil {
		return
	}
	var kind timeline.Kind
	switch audit.Action {
	case github.AuditActionAgentPRCreated:
		kind = timeline.KindPROpened
	case github.AuditActionPRMerged:
		kind = timeline.KindMerged
	default:
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	kv := parseAuditDetail(audit.Detail)
	repo, number := audit.Repo, audit.Target
	if repo == "" || number <= 0 {
		repo = kv["repo"]
		number, _ = strconv.Atoi(kv["number"]) // 0 on a malformed pair
	}
	if repo == "" || number <= 0 {
		return // not attributable to a work item — nothing to journey
	}
	ref := timelineRef(org, repo, number)
	attrs := map[string]string{"pr_ref": ref, "source": audit.Action}
	for _, key := range []string{"url", "author", "method", "sha", "reused"} {
		if v := kv[key]; v != "" {
			attrs[key] = v
		}
	}
	event := timeline.Event{IssueRef: ref, Kind: kind, Agent: audit.Agent, Attrs: attrs}
	_, span := tracing.StartTimelineSpan(context.Background(), event)
	store.Record(event)
	span.End()
	if kind == timeline.KindMerged {
		recordMergedOnLinkedIssues(store, ref, audit.Agent, 0, attrs)
	}
}

// recordMergedOnLinkedIssues folds a PR's merge onto the ISSUE journeys whose
// pr_opened stage names that PR (recordPROpenedForIssues), closing their
// pr→merge and open→merge SLA spans. The PR's own journey already has the
// stage; linked issues get a copy with the same instant. No I/O.
func recordMergedOnLinkedIssues(store *timeline.Store, prRef, agent string, at int64, attrs map[string]string) {
	if store == nil {
		return
	}
	for _, issue := range store.LinkedIssueRefs(prRef) {
		copyAttrs := make(map[string]string, len(attrs))
		for k, v := range attrs {
			copyAttrs[k] = v
		}
		store.Record(timeline.Event{IssueRef: issue, Kind: timeline.KindMerged, Agent: agent, At: at, Attrs: copyAttrs})
	}
}

// recordObservedMerge records a merge the PR-claim ledger OBSERVED (a human
// merged outside the hive, or the governor's merge arrived via the sync) at
// the instant it was seen, on the PR's journey and the issues it fixes. The
// audit bridge covers hive-driven merges; this covers the rest, so pr→merge
// has a sample whoever pressed the button. Dedupe makes overlap a refresh.
func recordObservedMerge(rec lifecycleRecorder, org, repo string, number int, observedAt time.Time, actor, attribution string) {
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	ref := timelineRef(org, repo, number)
	if ref == "" {
		return
	}
	attrs := map[string]string{"pr_ref": ref, "source": "observed", "actor": actor, "path": attribution}
	event := timeline.Event{IssueRef: ref, Kind: timeline.KindMerged, Attrs: attrs}
	if !observedAt.IsZero() {
		event.At = observedAt.UnixMilli()
	}
	store.Record(event)
	recordMergedOnLinkedIssues(store, ref, "", event.At, attrs)
}

// recordPROpenedForIssues records pr_opened on each ISSUE journey a new PR
// names (Fixes/Closes), with pr_ref pointing at the PR's own "repo#N" so the
// SLA derivation can tell an issue's journey from the PR's and later merges
// can be folded back (LinkedIssueRefs). Guarded and nil-safe; no I/O.
func recordPROpenedForIssues(rec lifecycleRecorder, org, agent, repo string, number int, url string, issues []int) {
	if rec == nil || len(issues) == 0 {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	prRef := timelineRef(org, repo, number)
	if prRef == "" {
		return
	}
	for _, n := range issues {
		if n <= 0 || n == number {
			continue
		}
		store.Record(timeline.Event{
			IssueRef: timelineRef(org, repo, n),
			Kind:     timeline.KindPROpened,
			Agent:    agent,
			Attrs:    map[string]string{timeline.AttrPRRef: prRef, "url": url},
		})
	}
}

// recordPROpened records a pr_opened stage from the typed PR-opened hook (the
// pr_request watcher fires it on the exact path that opened the PR). Guarded
// and nil-safe like the sibling recorders; no I/O.
func recordPROpened(rec lifecycleRecorder, org, agent, repo string, number int, url string) {
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	ref := timelineRef(org, repo, number)
	if ref == "" {
		return
	}
	event := timeline.Event{
		IssueRef: ref,
		Kind:     timeline.KindPROpened,
		Agent:    agent,
		Attrs:    map[string]string{"pr_ref": ref, "url": url},
	}
	_, span := tracing.StartTimelineSpan(context.Background(), event)
	store.Record(event)
	span.End()
}

// recordStarted records a started stage from the agent start-signal hook
// (#10527): the agent's own `hive-open-issue claim`, comment, label or
// `hive-open-pr --issues` request on an issue. The store keeps FirstAt on
// re-recording, so a later signal on the same issue refreshes the stage
// without moving the start instant.
func recordStarted(rec lifecycleRecorder, org, agent, repo string, number int, signal string) {
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	ref := timelineRef(org, repo, number)
	if ref == "" {
		return
	}
	// A number whose own pr_opened names itself is a PR: a comment or label on
	// it is not the start of work, and would double-count the PR journey.
	if j, ok := store.Journey(ref); ok {
		if st := j.Stages[timeline.KindPROpened]; st != nil && st.Attrs[timeline.AttrPRRef] == ref {
			return
		}
	}
	event := timeline.Event{
		IssueRef: ref,
		Kind:     timeline.KindStarted,
		Agent:    agent,
		Attrs:    map[string]string{"signal": signal},
	}
	_, span := tracing.StartTimelineSpan(context.Background(), event)
	store.Record(event)
	span.End()
}

// recordBlocked records a blocked stage when the escalation machinery hands a
// PR to a human (needs-human), carrying the evidence the operator needs to
// read the row. Called from runEscalationSweep on the newly-escalated path
// only — the store's dedupe makes re-escalation after a restart a refresh.
func recordBlocked(ctx context.Context, rec lifecycleRecorder, org, repo string, number, attempts int, failingChecks []string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if rec == nil {
		return
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return
	}
	ref := timelineRef(org, repo, number)
	if ref == "" {
		return
	}
	attrs := map[string]string{
		"reason":       "fix-loop-escalated",
		"fix_attempts": strconv.Itoa(attempts),
	}
	if len(failingChecks) > 0 {
		attrs["failing_checks"] = strings.Join(failingChecks, ",")
	}
	event := timeline.Event{IssueRef: ref, Kind: timeline.KindBlocked, Attrs: attrs}
	_, span := tracing.StartTimelineSpan(ctx, event)
	store.Record(event)
	span.End()
}
