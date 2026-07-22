package visualhive

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/automation"
)

type LifecycleIssueClient interface {
	ResolveLifecycleIssueWriter(ctx context.Context, repository, marker string, issueNumber int) (login string, id int64, err error)
	UpsertLifecycleIssueOwned(ctx context.Context, repository, marker, writerLogin string, writerID int64, title, body string, labels []string) (number int, url string, created bool, err error)
	UpdateLifecycleIssueOwned(ctx context.Context, repository string, number int, marker, writerLogin string, writerID int64, title, body, state string, labels []string) (updatedNumber int, url string, err error)
	MigrateLifecycleIssueMarkerOwned(ctx context.Context, repository string, number int, previousMarker, marker, writerLogin string, writerID int64, title, body, state string, labels []string) (updatedNumber int, url string, err error)
}

type OutboxProcessorResult struct {
	Processed    int      `json:"processed"`
	Succeeded    int      `json:"succeeded"`
	Failed       int      `json:"failed"`
	Denied       int      `json:"denied"`
	StaleSkipped int      `json:"stale_skipped"`
	Errors       []string `json:"errors,omitempty"`
}

func ProcessOutbox(ctx context.Context, lifecycle *LifecycleStore, beadStore LifecycleBeadSink, policy automation.Policy, client LifecycleIssueClient) OutboxProcessorResult {
	return processOutbox(ctx, lifecycle, beadStore, policy, client, "")
}

func ProcessOutboxForFinding(ctx context.Context, lifecycle *LifecycleStore, beadStore LifecycleBeadSink, policy automation.Policy, client LifecycleIssueClient, repositoryFingerprint string) OutboxProcessorResult {
	return processOutbox(ctx, lifecycle, beadStore, policy, client, strings.TrimSpace(repositoryFingerprint))
}

func processOutbox(ctx context.Context, lifecycle *LifecycleStore, beadStore LifecycleBeadSink, policy automation.Policy, client LifecycleIssueClient, repositoryFingerprint string) OutboxProcessorResult {
	result := OutboxProcessorResult{}
	if lifecycle == nil || beadStore == nil || client == nil {
		result.Failed = 1
		result.Errors = []string{"lifecycle store, bead store, and GitHub client are required"}
		return result
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if repositoryFingerprint != "" && entry.RepositoryFingerprint != repositoryFingerprint {
			continue
		}
		if ctx.Err() != nil {
			result.Errors = append(result.Errors, ctx.Err().Error())
			break
		}
		result.Processed++
		finding, exists := lifecycle.Finding(entry.RepositoryFingerprint)
		if !exists {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("outbox %s references a missing finding", entry.ID))
			continue
		}
		if entry.BundleDigest != finding.LastBundleDigest {
			if err := lifecycle.RecordAuthorizationStrict(entry.RepositoryFingerprint, string(entry.Action), false, "stale outbox entry superseded by newer evidence"); err != nil {
				result.Failed++
				result.Errors = append(result.Errors, fmt.Sprintf("persist stale outbox authorization: %v", err))
				continue
			}
			if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
				result.Failed++
				result.Errors = append(result.Errors, fmt.Sprintf("persist stale outbox completion: %v", err))
				continue
			}
			result.StaleSkipped++
			continue
		}
		request := automation.ActionRequest{
			Action: automationAction(entry.Action), Agent: actorForHint(finding.OwningAgentHint),
			Repository: entry.Repository, RepairAttempts: finding.RepairAttempts,
		}
		decision := policy.Authorize(request)
		if err := lifecycle.RecordAuthorizationStrict(entry.RepositoryFingerprint, string(entry.Action), decision.Allowed, strings.Join(decision.Reasons, "; ")); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("persist %s authorization: %v", entry.Action, err))
			continue
		}
		if !decision.Allowed {
			result.Denied++
			continue
		}
		if err := processOutboxEntry(ctx, lifecycle, beadStore, client, finding, entry); err != nil {
			if persistErr := lifecycle.MarkOutboxAttempt(entry.ID, err); persistErr != nil {
				err = fmt.Errorf("%v; persist failed outbox attempt: %w", err, persistErr)
			}
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", entry.Action, err))
			continue
		}
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("persist outbox completion: %v", err))
			continue
		}
		result.Succeeded++
	}
	sort.Strings(result.Errors)
	return result
}

func processOutboxEntry(ctx context.Context, lifecycle *LifecycleStore, beadStore LifecycleBeadSink, client LifecycleIssueClient, finding FindingLifecycle, entry OutboxEntry) error {
	markerFingerprint := finding.PublicationFingerprint
	if finding.RootCauseKey != "" {
		expected := publicationFingerprint(finding.Repository, finding.RootCauseKey, finding.RepositoryFingerprint)
		if markerFingerprint != expected {
			return fmt.Errorf("root publication fingerprint does not match repository and root cause key")
		}
		markerFingerprint = expected
	} else {
		markerFingerprint = finding.RepositoryFingerprint
	}
	marker := lifecycleMarker(markerFingerprint)
	writerLogin, writerID := strings.TrimSpace(finding.IssueWriterLogin), finding.IssueWriterID
	if writerID <= 0 || writerLogin == "" {
		resolvedLogin, resolvedID, err := client.ResolveLifecycleIssueWriter(ctx, entry.Repository, marker, finding.IssueNumber)
		if err != nil {
			return fmt.Errorf("resolve immutable lifecycle issue writer: %w", err)
		}
		if err := lifecycle.BindIssueWriter(entry.RepositoryFingerprint, resolvedID, resolvedLogin); err != nil {
			return fmt.Errorf("persist immutable lifecycle issue writer: %w", err)
		}
		writerLogin, writerID = resolvedLogin, resolvedID
	}
	body := lifecycleIssueBody(marker, entry, finding)
	if entry.PreviousMarker != "" {
		if finding.IssueNumber <= 0 || entry.IssueNumber != finding.IssueNumber || entry.PreviousMarker == marker {
			return fmt.Errorf("pending lifecycle marker migration does not match the persisted issue identity")
		}
		if finding.PendingMarkerMigrationFrom != "" && finding.PendingMarkerMigrationFrom != entry.PreviousMarker {
			return fmt.Errorf("outbox marker migration does not match the currently pending exact old marker")
		}
		state := "open"
		labels := activeLabels(entry.Labels)
		switch entry.Action {
		case OutboxUpdateIssue, OutboxReopenIssue:
		case OutboxCloseIssue:
			if finding.Status == StatusIssueClosed {
				if finding.PendingMarkerMigrationFrom != "" {
					return fmt.Errorf("closed finding still has a pending marker migration")
				}
				return nil
			}
			if finding.Status != StatusResolved {
				return fmt.Errorf("refusing to migrate and close issue while finding is %s", finding.Status)
			}
			state = "closed"
			labels = resolvedLabels(entry.Labels)
		default:
			return fmt.Errorf("unsupported lifecycle action %q for an exact marker migration", entry.Action)
		}
		number, url, err := client.MigrateLifecycleIssueMarkerOwned(ctx, entry.Repository, finding.IssueNumber, entry.PreviousMarker, marker, writerLogin, writerID, entry.Title, body, state, labels)
		if err != nil {
			return err
		}
		if err := lifecycle.MarkIssueMarkerMigrated(entry.RepositoryFingerprint, entry.PreviousMarker, marker, number, url); err != nil {
			return err
		}
		if entry.Action == OutboxCloseIssue {
			return lifecycle.MarkIssueClosed(entry.RepositoryFingerprint, beadStore)
		}
		return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
	}
	switch entry.Action {
	case OutboxOpenIssue:
		number, url, _, err := client.UpsertLifecycleIssueOwned(ctx, entry.Repository, marker, writerLogin, writerID, entry.Title, body, activeLabels(entry.Labels))
		if err != nil {
			return err
		}
		return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
	case OutboxUpdateIssue, OutboxReopenIssue:
		if finding.IssueNumber <= 0 {
			number, url, _, err := client.UpsertLifecycleIssueOwned(ctx, entry.Repository, marker, writerLogin, writerID, entry.Title, body, activeLabels(entry.Labels))
			if err != nil {
				return err
			}
			return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
		}
		number, url, err := client.UpdateLifecycleIssueOwned(ctx, entry.Repository, finding.IssueNumber, marker, writerLogin, writerID, entry.Title, body, "open", activeLabels(entry.Labels))
		if err != nil {
			return err
		}
		return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
	case OutboxCloseIssue:
		if finding.Status == StatusIssueClosed {
			// GitHub close and the lifecycle transition are durable before the
			// outbox completion bit. A crash in that narrow window must make the
			// pending close an idempotent success rather than a permanent wedge.
			if finding.IssueNumber <= 0 || entry.IssueNumber <= 0 {
				return fmt.Errorf("closed finding no longer matches pending close issue")
			}
			return nil
		}
		if finding.Status != StatusResolved {
			return fmt.Errorf("refusing to close issue while finding is %s", finding.Status)
		}
		if finding.IssueNumber <= 0 {
			return fmt.Errorf("cannot close finding without a persisted GitHub issue number")
		}
		number, url, err := client.UpdateLifecycleIssueOwned(ctx, entry.Repository, finding.IssueNumber, marker, writerLogin, writerID, entry.Title, body, "closed", resolvedLabels(entry.Labels))
		if err != nil {
			return err
		}
		if number != finding.IssueNumber || url != finding.IssueURL {
			if err := lifecycle.RebindResolvedIssue(entry.RepositoryFingerprint, number, url); err != nil {
				return err
			}
		}
		return lifecycle.MarkIssueClosed(entry.RepositoryFingerprint, beadStore)
	default:
		return fmt.Errorf("unsupported lifecycle outbox action %q", entry.Action)
	}
}

func lifecycleMarker(repositoryFingerprint string) string {
	return fmt.Sprintf("<!-- hive-visual-fingerprint: %s -->", repositoryFingerprint)
}

func lifecycleIssueBody(marker string, entry OutboxEntry, finding FindingLifecycle) string {
	status := "active"
	if entry.Action == OutboxCloseIssue {
		status = "resolved after authoritative target-branch verification"
	}
	observationBody := strings.TrimSpace(entry.Body)
	if finding.RootCauseKey != "" {
		observationBody = stripLegacyVisualHiveDedupeMarkers(observationBody)
	}
	lines := []string{
		marker,
		observationBody,
		"",
		"## Hive lifecycle",
		"",
		fmt.Sprintf("- Status: **%s**", status),
		fmt.Sprintf("- Finding fingerprint: `%s`", finding.Fingerprint),
		fmt.Sprintf("- Bundle: `%s`", entry.BundleID),
		fmt.Sprintf("- Bundle digest: `%s`", entry.BundleDigest),
	}
	if value := entry.Evidence["commit_sha"]; value != "" {
		lines = append(lines, fmt.Sprintf("- Observed commit: `%s`", value))
	}
	if value := entry.Evidence["workflow_run_id"]; value != "" {
		lines = append(lines, fmt.Sprintf("- Workflow run ID: `%s`", value))
	}
	if finding.PRURL != "" {
		lines = append(lines, fmt.Sprintf("- Repair PR: %s", finding.PRURL))
	}
	if finding.ValidationRunURL != "" {
		lines = append(lines, fmt.Sprintf("- Target-branch verification: %s", finding.ValidationRunURL))
	}
	lines = append(lines, "", "Hive owns this issue lifecycle. Absence from partial, stale, failed, or non-target-branch evidence cannot close it.")
	return strings.Join(lines, "\n")
}

func stripLegacyVisualHiveDedupeMarkers(body string) string {
	const prefix = "<!-- visual-hive-issue dedupe:"
	for {
		start := strings.Index(body, prefix)
		if start < 0 {
			break
		}
		end := strings.Index(body[start:], "-->")
		if end < 0 {
			// A malformed marker cannot be used by the GitHub compatibility
			// lookup, so leave evidence text unchanged.
			break
		}
		body = body[:start] + body[start+end+3:]
	}
	return strings.TrimSpace(body)
}

func activeLabels(labels []string) []string {
	return lifecycleLabels(labels, []string{"hive/managed", "hive/active", "visual-hive/ready-for-hive"}, []string{"hive/resolved", "visual-hive/resolved-candidate"})
}

func resolvedLabels(labels []string) []string {
	return lifecycleLabels(labels, []string{"hive/managed", "hive/resolved"}, []string{"hive/active", "visual-hive/ready-for-hive", "visual-hive/still-active"})
}

func lifecycleLabels(base, add, remove []string) []string {
	removed := make(map[string]bool, len(remove))
	for _, label := range remove {
		removed[label] = true
	}
	values := make(map[string]bool)
	for _, label := range append(append([]string(nil), base...), add...) {
		label = strings.TrimSpace(label)
		if label != "" && !removed[label] {
			values[label] = true
		}
	}
	result := make([]string, 0, len(values))
	for label := range values {
		result = append(result, label)
	}
	sort.Strings(result)
	return result
}

func automationAction(action OutboxAction) automation.Action {
	switch action {
	case OutboxOpenIssue:
		return automation.ActionCreateIssue
	case OutboxUpdateIssue:
		return automation.ActionUpdateIssue
	case OutboxReopenIssue:
		return automation.ActionReopenIssue
	case OutboxCloseIssue:
		return automation.ActionCloseIssue
	default:
		return "unknown"
	}
}
