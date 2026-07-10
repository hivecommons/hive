package visualhive

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
)

type LifecycleIssueClient interface {
	UpsertLifecycleIssue(ctx context.Context, repository, marker, title, body string, labels []string) (number int, url string, created bool, err error)
	UpdateLifecycleIssue(ctx context.Context, repository string, number int, title, body, state string, labels []string) (updatedNumber int, url string, err error)
}

type OutboxProcessorResult struct {
	Processed    int      `json:"processed"`
	Succeeded    int      `json:"succeeded"`
	Failed       int      `json:"failed"`
	Denied       int      `json:"denied"`
	StaleSkipped int      `json:"stale_skipped"`
	Errors       []string `json:"errors,omitempty"`
}

func ProcessOutbox(ctx context.Context, lifecycle *LifecycleStore, beadStore *beads.Store, policy automation.Policy, client LifecycleIssueClient) OutboxProcessorResult {
	result := OutboxProcessorResult{}
	if lifecycle == nil || beadStore == nil || client == nil {
		result.Failed = 1
		result.Errors = []string{"lifecycle store, bead store, and GitHub client are required"}
		return result
	}
	for _, entry := range lifecycle.PendingOutbox() {
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
			_ = lifecycle.MarkOutboxAttempt(entry.ID, nil)
			lifecycle.RecordAuthorization(entry.RepositoryFingerprint, string(entry.Action), false, "stale outbox entry superseded by newer evidence")
			result.StaleSkipped++
			continue
		}
		request := automation.ActionRequest{
			Action: automationAction(entry.Action), Agent: actorForHint(finding.OwningAgentHint),
			Repository: entry.Repository, RepairAttempts: finding.RepairAttempts,
		}
		decision := policy.Authorize(request)
		lifecycle.RecordAuthorization(entry.RepositoryFingerprint, string(entry.Action), decision.Allowed, strings.Join(decision.Reasons, "; "))
		if !decision.Allowed {
			result.Denied++
			continue
		}
		if err := processOutboxEntry(ctx, lifecycle, beadStore, client, finding, entry); err != nil {
			_ = lifecycle.MarkOutboxAttempt(entry.ID, err)
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

func processOutboxEntry(ctx context.Context, lifecycle *LifecycleStore, beadStore *beads.Store, client LifecycleIssueClient, finding FindingLifecycle, entry OutboxEntry) error {
	marker := lifecycleMarker(finding.RepositoryFingerprint)
	body := lifecycleIssueBody(marker, entry, finding)
	switch entry.Action {
	case OutboxOpenIssue:
		number, url, _, err := client.UpsertLifecycleIssue(ctx, entry.Repository, marker, entry.Title, body, activeLabels(entry.Labels))
		if err != nil {
			return err
		}
		return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
	case OutboxUpdateIssue, OutboxReopenIssue:
		if finding.IssueNumber <= 0 {
			number, url, _, err := client.UpsertLifecycleIssue(ctx, entry.Repository, marker, entry.Title, body, activeLabels(entry.Labels))
			if err != nil {
				return err
			}
			return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
		}
		number, url, err := client.UpdateLifecycleIssue(ctx, entry.Repository, finding.IssueNumber, entry.Title, body, "open", activeLabels(entry.Labels))
		if err != nil {
			return err
		}
		return lifecycle.MarkIssueOpened(entry.RepositoryFingerprint, number, url)
	case OutboxCloseIssue:
		if finding.Status != StatusResolved {
			return fmt.Errorf("refusing to close issue while finding is %s", finding.Status)
		}
		if finding.IssueNumber <= 0 {
			return fmt.Errorf("cannot close finding without a persisted GitHub issue number")
		}
		if _, _, err := client.UpdateLifecycleIssue(ctx, entry.Repository, finding.IssueNumber, entry.Title, body, "closed", resolvedLabels(entry.Labels)); err != nil {
			return err
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
	lines := []string{
		marker,
		strings.TrimSpace(entry.Body),
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
