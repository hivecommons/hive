package main

import (
	"context"
	"log/slog"
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
	recordPROpenedForIssuesAt(rec, org, agent, repo, number, url, issues, 0)
}

// recordPROpenedForIssuesAt is recordPROpenedForIssues at an explicit instant
// (Unix ms; <= 0 means now), so a backfill can stamp the PR's real created_at.
func recordPROpenedForIssuesAt(rec lifecycleRecorder, org, agent, repo string, number int, url string, issues []int, at int64) {
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
			At:       at,
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

// backfillMergedPROnIssues folds one merged PR onto the issue journeys it
// closes, stamped with the PR's real created_at (pr_opened) and merged_at
// (merged) rather than now (hivecommons/hive#12007). Issue-side stages only
// began recording with #11880, so without this every issue started before
// the upgrade stays "stale in-progress" although its fix already merged.
//
// Only issues the store already journeys are touched (a backfill must not
// evict live journeys for issues the spoke never worked), and an issue whose
// journey already has merged is skipped, so re-running on every merge sweep
// and every restart is a no-op once applied. An existing pr_opened (which may
// name another PR) is kept. Returns how many issue journeys it updated.
func backfillMergedPROnIssues(rec lifecycleRecorder, org string, obs github.PRTerminalObservation) int {
	if rec == nil || obs.State != github.PRStateMerged || obs.ObservedAt.IsZero() || len(obs.Issues) == 0 {
		return 0
	}
	store := rec.LifecycleTimeline()
	if store == nil {
		return 0
	}
	prRef := timelineRef(org, obs.Repo, obs.Number)
	if prRef == "" {
		return 0
	}
	var needOpened []int
	var needMerged []string
	for _, n := range obs.Issues {
		if n <= 0 || n == obs.Number {
			continue
		}
		ref := timelineRef(org, obs.Repo, n)
		j, ok := store.Journey(ref)
		if !ok || j.Stages[timeline.KindMerged] != nil {
			continue
		}
		if j.Stages[timeline.KindPROpened] == nil {
			needOpened = append(needOpened, n)
		}
		needMerged = append(needMerged, ref)
	}
	if len(needMerged) == 0 {
		return 0
	}
	// Without the PR's created_at there is no truthful pr_opened instant;
	// merged alone still ends the issue's in-progress state.
	if !obs.CreatedAt.IsZero() {
		recordPROpenedForIssuesAt(rec, org, "", obs.Repo, obs.Number, obs.URL, needOpened, obs.CreatedAt.UnixMilli())
	}
	mergedAt := obs.ObservedAt.UnixMilli()
	for _, ref := range needMerged {
		attrs := map[string]string{timeline.AttrPRRef: prRef, "source": "backfill"}
		if obs.URL != "" {
			attrs["url"] = obs.URL
		}
		if obs.Actor != "" {
			attrs["actor"] = obs.Actor
		}
		store.Record(timeline.Event{IssueRef: ref, Kind: timeline.KindMerged, At: mergedAt, Attrs: attrs})
	}
	return len(needMerged)
}

// mergedPRLister is the narrow GitHub read the startup backfill needs.
type mergedPRLister interface {
	MergedPRsSince(ctx context.Context, since time.Time) ([]github.PRTerminalObservation, error)
}

// backfillRecentMergedPRs runs backfillMergedPROnIssues over every PR merged
// within github.MergedPRBackfillWindow of now: one bounded closed-PR listing
// per repo. Run once at startup; the merge sweep keeps it current afterwards
// through the PR-terminal hook. A partial listing failure is logged and the
// repos that did list are still applied.
func backfillRecentMergedPRs(ctx context.Context, lister mergedPRLister, rec lifecycleRecorder, org string, now time.Time, logger *slog.Logger) int {
	if lister == nil || rec == nil {
		return 0
	}
	prs, err := lister.MergedPRsSince(ctx, now.Add(-github.MergedPRBackfillWindow))
	if err != nil && logger != nil {
		logger.Warn("lifecycle merged-PR backfill: listing incomplete", "error", err)
	}
	updated := 0
	for _, obs := range prs {
		updated += backfillMergedPROnIssues(rec, org, obs)
	}
	if logger != nil {
		logger.Info("lifecycle merged-PR backfill done", "merged_prs", len(prs), "issues_updated", updated)
	}
	return updated
}
