package main

// lifecyclewire.go wires the lifecycle-timeline producers that were missing
// since the panel shipped (#5656): pr_opened, merged and blocked. Enumerated
// and kicked are recorded by the governor eval loop (main.go:
// recordEnumeratedIssues / recordKick) and classified by the scheduler's
// classifier pass (pkg/scheduler.SetLifecycleRecorder). Everything here rides
// EXISTING paths — the attribution audit sink, the PR-opened hook and the
// escalation sweep — no new polling, no new goroutines.

import (
	"context"
	"strconv"
	"strings"

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
func recordLifecycleFromAudit(rec lifecycleRecorder, org, action, detail, agent string) {
	if rec == nil {
		return
	}
	var kind timeline.Kind
	switch action {
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
	kv := parseAuditDetail(detail)
	number, err := strconv.Atoi(kv["number"])
	if kv["repo"] == "" || err != nil || number <= 0 {
		return // not attributable to a work item — nothing to journey
	}
	ref := timelineRef(org, kv["repo"], number)
	attrs := map[string]string{"pr_ref": ref, "source": action}
	for _, key := range []string{"url", "author", "method", "sha", "reused"} {
		if v := kv[key]; v != "" {
			attrs[key] = v
		}
	}
	event := timeline.Event{IssueRef: ref, Kind: kind, Agent: agent, Attrs: attrs}
	_, span := tracing.StartTimelineSpan(context.Background(), event)
	store.Record(event)
	span.End()
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
