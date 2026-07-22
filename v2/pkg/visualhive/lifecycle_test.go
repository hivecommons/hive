package visualhive

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
)

func TestLifecyclePersistsAndDeduplicatesBundle(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-present-1", "present", "refs/heads/main", true)
	bundle := validateLocalBundle(t, manifestPath)
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}

	first, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 1 || first.BeadsCreated != 1 || first.OutboxCreated != 1 || first.Idempotent {
		t.Fatalf("unexpected first apply: %+v", first)
	}
	second, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || beadStore.Count() != 1 || len(lifecycle.PendingOutbox()) != 1 {
		t.Fatalf("retry was not idempotent: %+v beads=%d outbox=%d", second, beadStore.Count(), len(lifecycle.PendingOutbox()))
	}

	reloaded, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := bundle.Manifest.Observations[0].RepositoryFingerprint
	finding, ok := reloaded.Finding(fingerprint)
	if !ok || finding.Status != StatusDetected || finding.BeadID == "" {
		t.Fatalf("lifecycle state did not survive restart: %+v", finding)
	}
}

func TestAdvisoryLifecyclePersistsFindingAndBeadWithoutIssueOutbox(t *testing.T) {
	root := t.TempDir()
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-advisory-present", "present", "refs/heads/main", true))
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
	if err != nil {
		t.Fatal(err)
	}
	finding, exists := lifecycle.Finding(bundle.Manifest.Observations[0].RepositoryFingerprint)
	if result.Created != 1 || result.BeadsCreated != 1 || result.OutboxCreated != 0 || len(lifecycle.PendingOutbox()) != 0 || beadStore.Count() != 1 || !exists || finding.Status != StatusDetected {
		t.Fatalf("advisory evidence was not durable and publication-free: result=%+v finding=%+v beads=%d pending=%+v", result, finding, beadStore.Count(), lifecycle.PendingOutbox())
	}
	legacy := beadStore.FindByExternalRef(beadExternalRef(bundle.Manifest.Source.Repository, bundle.Manifest.Observations[0].RepositoryFingerprint))
	if legacy == nil || legacy.Status != beads.StatusOpen || legacy.Metadata["visual_hive_controller_owned"] != false {
		t.Fatalf("legacy ApplyBundle compatibility semantics changed: %+v", legacy)
	}
}

func TestAdvisoryLifecycleResolvesLocallyWithoutCloseOutbox(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-before-advisory", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	open := lifecycle.PendingOutbox()[0]
	if err := lifecycle.MarkIssueOpened(fingerprint, 77, "https://github.test/owner/repo/issues/77"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkOutboxAttempt(open.ID, nil); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-advisory-absent", "absent", "refs/heads/main", true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true})
	if err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if result.Resolved != 1 || result.OutboxCreated != 0 || len(lifecycle.PendingOutbox()) != 0 || finding.Status != StatusResolved || finding.IssueNumber != 77 {
		t.Fatalf("advisory absence did not resolve locally without GitHub publication: result=%+v finding=%+v pending=%+v", result, finding, lifecycle.PendingOutbox())
	}
}

func TestAdvisoryAuthorityCancelsPreexistingPublicationIntent(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-pending-before-advisory", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.PendingOutbox()) != 1 {
		t.Fatal("fixture did not create one pending publication")
	}
	cancelled, err := lifecycle.CancelPendingIssuePublication("automation authority is advisory")
	if err != nil || cancelled != 1 || len(lifecycle.PendingOutbox()) != 0 || beadStore.Count() != 1 {
		t.Fatalf("advisory downgrade did not durably consume publication intent: cancelled=%d err=%v pending=%+v beads=%d", cancelled, err, lifecycle.PendingOutbox(), beadStore.Count())
	}
	if finding, exists := lifecycle.Finding(bundle.Manifest.Observations[0].RepositoryFingerprint); !exists || finding.Status != StatusDetected {
		t.Fatalf("advisory cancellation mutated the finding: %+v", finding)
	}
}

func TestLifecycleRejectsReplayKeyWithDifferentDigest(t *testing.T) {
	root := t.TempDir()
	manifestPath := writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-replay", "present", "refs/heads/main", true)
	bundle := validateLocalBundle(t, manifestPath)
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}

	collision := *bundle
	collision.Manifest = bundle.Manifest
	collision.Manifest.OverallDigest = strings.Repeat("f", 64)
	if _, err := lifecycle.ApplyBundle(&collision, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err == nil || !strings.Contains(err.Error(), "different digest") {
		t.Fatalf("expected replay-key collision rejection, got %v", err)
	}
}

func TestLifecycleIssuePRMergeCloseAndRecurrence(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	presentPath := writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "refs/heads/main", true)
	present := validateLocalBundle(t, presentPath)
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	openEntry := lifecycle.PendingOutbox()[0]
	if openEntry.Action != OutboxOpenIssue {
		t.Fatalf("expected open issue outbox, got %s", openEntry.Action)
	}
	if err := lifecycle.MarkIssueOpened(fingerprint, 101, "https://github.com/owner/repo/issues/101"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkOutboxAttempt(openEntry.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-app-shell"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-app-shell"); err != nil {
		t.Fatal(err)
	}
	started, _ := lifecycle.Finding(fingerprint)
	if started.RepairAttempts != 1 {
		t.Fatalf("restart replay incremented repair attempts: %+v", started)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-sha", 202, "https://github.com/owner/repo/pull/202"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "wrong-sha", true); err == nil {
		t.Fatal("checks for a stale SHA must not authorize merge")
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-sha", true); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkMerged(fingerprint, present.Manifest.Source.CommitSHA); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "run-303", "https://github.com/owner/repo/actions/runs/303"); err != nil {
		t.Fatal(err)
	}

	absentPath := writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent", "absent", "refs/heads/main", true)
	absent := validateLocalBundle(t, absentPath)
	absent.Manifest.Source.WorkflowRunID = "run-303"
	resolved, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{
		TargetRef: "main", VerificationRunID: "run-303", VerificationCommitSHA: absent.Manifest.Source.CommitSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resolved != 1 {
		t.Fatalf("authoritative absence did not resolve finding: %+v", resolved)
	}
	pending := lifecycle.PendingOutbox()
	closeEntry := pending[len(pending)-1]
	if closeEntry.Action != OutboxCloseIssue || closeEntry.IssueNumber != 101 {
		t.Fatalf("expected close issue outbox, got %+v", closeEntry)
	}
	if err := lifecycle.MarkOutboxAttempt(closeEntry.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkIssueClosed(fingerprint, beadStore); err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusIssueClosed {
		t.Fatalf("finding was not closed: %+v", finding)
	}
	bead, err := beadStore.Get(finding.BeadID)
	if err != nil || bead.Status != beads.StatusClosed {
		t.Fatalf("bead was not closed: %+v err=%v", bead, err)
	}

	recurrencePath := writeLifecycleBundle(t, filepath.Join(root, "recurrence"), "bundle-recurrence", "present", "refs/heads/main", true)
	recurrence := validateLocalBundle(t, recurrencePath)
	reopened, err := lifecycle.ApplyBundle(recurrence, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Reopened != 1 || reopened.BeadsCreated != 0 {
		t.Fatalf("recurrence did not reopen the existing lifecycle: %+v", reopened)
	}
	finding, _ = lifecycle.Finding(fingerprint)
	bead, _ = beadStore.Get(finding.BeadID)
	if finding.Recurrences != 1 || bead.Status != beads.StatusOpen || finding.Branch != "" || finding.RepairCommitSHA != "" || finding.PRNumber != 0 || finding.MergeSHA != "" || finding.ValidationRunID != "" || finding.RepairAttempts != 0 || finding.HumanReviewRequired || finding.ManualReviewKind != "" {
		t.Fatalf("recurrence did not reopen existing issue/bead: finding=%+v bead=%+v", finding, bead)
	}
	reopenEntry := lifecycle.PendingOutbox()[0]
	if reopenEntry.Action != OutboxReopenIssue || reopenEntry.IssueNumber != 101 {
		t.Fatalf("expected reopen issue outbox, got %+v", reopenEntry)
	}
}

func TestRepairedFindingCannotResolveBeforeRecordedPostMergeVerification(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-premerge-present", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 101, "https://example.test/issues/101"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-a1"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-sha", 202, "https://example.test/pull/202"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-sha", true); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-premerge-absent", "absent", "refs/heads/main", true))
	absent.Manifest.Source.WorkflowRunID = "run-before-merge"
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{
		TargetRef: "main", VerificationRunID: "run-before-merge", VerificationCommitSHA: absent.Manifest.Source.CommitSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if result.Resolved != 0 || result.IgnoredAbsent != 1 || finding.Status != StatusReady {
		t.Fatalf("unreconciled repair was allowed to resolve: result=%+v finding=%+v", result, finding)
	}
	audit, err := os.ReadFile(filepath.Join(root, "lifecycle", "visual-hive-lifecycle-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"action":"resolve_finding"`) || !strings.Contains(string(audit), `"allowed":false`) || !strings.Contains(string(audit), "no recorded merge") {
		t.Fatalf("missing denied pre-merge resolution audit: %s", audit)
	}
}

func TestLifecyclePersistsManualBaselineReviewAcrossFreshObservations(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-review-one", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 48, "https://example.test/issues/48"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-review"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-head", 49, "https://example.test/pull/49"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkManualReviewRequired(fingerprint, "visual_baseline", "Review exact candidate digests in PR #50"); err != nil {
		t.Fatal(err)
	}
	fresh := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "fresh"), "bundle-review-two", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(fresh, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	held, _ := lifecycle.Finding(fingerprint)
	if !held.HumanReviewRequired || held.ManualReviewKind != "visual_baseline" || held.ObservationHumanReviewRequired {
		t.Fatalf("fresh producer evidence erased the manual hold: %+v", held)
	}
	if err := lifecycle.MarkPRHeadUpdated(fingerprint, "repair-head-with-baseline"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkManualReviewComplete(fingerprint, "visual_baseline"); err != nil {
		t.Fatal(err)
	}
	completed, _ := lifecycle.Finding(fingerprint)
	if completed.HumanReviewRequired || completed.ManualReviewKind != "" || completed.RepairCommitSHA != "repair-head-with-baseline" || completed.Status != StatusPROpen {
		t.Fatalf("manual review completion did not reset the exact-head gate: %+v", completed)
	}
}

func TestLifecycleCanHoldUnrepairableIssueForManualScopeReview(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-scope-review", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := bundle.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 101, "https://github.com/owner/repo/issues/101"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkManualReviewRequired(fingerprint, "repair_scope", "No safe repair evidence"); err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if !finding.HumanReviewRequired || finding.ManualReviewKind != "repair_scope" || finding.Status != StatusIssueOpen {
		t.Fatalf("issue was not held for scope review: %+v", finding)
	}
}

func TestLifecycleMigratesV1ManualReviewState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lifecycle")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"schema_version":"hive.visual-hive-lifecycle.v1","updated_at":"2026-07-10T00:00:00Z","findings":{"fp":{"repository":"owner/repo","fingerprint":"source","repository_fingerprint":"fp","status":"issue_open","issue_kind":"screenshot_diff","severity":"high","owning_agent_hint":"quality","title":"review","body":"review","labels":[],"affected_contracts":[],"validation_command":"vh","human_review_required":true,"first_seen_at":"2026-07-10T00:00:00Z","last_seen_at":"2026-07-10T00:00:00Z","last_bundle_id":"bundle","last_bundle_digest":"digest","repair_attempts":0,"recurrences":0}},"replay_keys":{},"outbox":[]}`
	statePath := filepath.Join(root, "visual-hive-lifecycle.json")
	if err := os.WriteFile(statePath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewLifecycleStore(root)
	if err != nil {
		t.Fatal(err)
	}
	finding, _ := store.Finding("fp")
	if !finding.ObservationHumanReviewRequired || !finding.HumanReviewRequired {
		t.Fatalf("legacy review authority was not migrated: %+v", finding)
	}
	data, err := os.ReadFile(statePath)
	if err != nil || !strings.Contains(string(data), LifecycleSchema) {
		t.Fatalf("migrated lifecycle schema was not persisted: %q err=%v", data, err)
	}
}

func TestPostMergeVerificationFailureReturnsFindingToRepair(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 101, "https://example.test/issues/101"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-a1"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-sha", 202, "https://example.test/pull/202"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-sha", true); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkMerged(fingerprint, "merge-sha"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "run-303", "https://example.test/runs/303"); err != nil {
		t.Fatal(err)
	}

	stillPresent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "still-present"), "bundle-still-present", "present", "refs/heads/main", true))
	stillPresent.Manifest.Source.WorkflowRunID = "run-303"
	stillPresent.Manifest.Source.CommitSHA = "merge-sha"
	if _, err := lifecycle.ApplyBundle(stillPresent, beadStore, ApplyLifecycleOptions{TargetRef: "main", VerificationRunID: "run-303"}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPostMergeFailed(fingerprint, "finding remained present"); err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusNeedsRevision || finding.MergeSHA != "merge-sha" || finding.LastCheckSummary != "finding remained present" {
		t.Fatalf("failed post-merge verification lost retry evidence: %+v", finding)
	}
}

func TestLifecycleDoesNotResolveFromWrongTargetRef(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent", "absent", "refs/heads/feature", true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 0 || result.IgnoredAbsent != 1 {
		t.Fatalf("wrong-ref absence was not ignored: %+v", result)
	}
}

func TestLifecycleRejectsV3BeforeCompleteSourceVerification(t *testing.T) {
	manifestPath, _ := writeTestV3Bundle(t, nil)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 3,
		VerifiedProvenance: true, ExpectedProducerGitCommit: testV3ProducerCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err == nil || !strings.Contains(err.Error(), "trusted Visual Hive bundle") {
		t.Fatalf("lifecycle accepted a v3 bundle before source verification: %v", err)
	}
}

func TestLifecycleRequiresValidatedAuthorityForV3Absence(t *testing.T) {
	for _, test := range []struct {
		name        string
		omitFinding bool
		wantIgnored int
	}{
		{name: "explicit absent", wantIgnored: 1},
		{name: "omitted from exhaustive inventory", omitFinding: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
			lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
			if err != nil {
				t.Fatal(err)
			}
			present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-authority-present", "present", "refs/heads/main", true))
			if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
				t.Fatal(err)
			}
			fingerprint := present.Manifest.Observations[0].RepositoryFingerprint

			crafted := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "crafted"), "bundle-authority-crafted", "absent", "refs/heads/main", true))
			crafted.Manifest.SchemaVersion = ManifestSchemaV3
			crafted.Validation.Authoritative = false
			if test.omitFinding {
				crafted.Manifest.Observations = nil
			}
			result, err := lifecycle.ApplyBundle(crafted, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Resolved != 0 || result.IgnoredAbsent != test.wantIgnored {
				t.Fatalf("unverified v3 authority claim changed lifecycle state: %+v", result)
			}
			finding, ok := lifecycle.Finding(fingerprint)
			if !ok || finding.Status == StatusResolved || finding.Status == StatusIssueClosed {
				t.Fatalf("unverified v3 absence resolved finding: %+v", finding)
			}
		})
	}
}

func TestLifecycleInfersAbsenceOnlyForEvaluatedContracts(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present-inventory", "present", "refs/heads/main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 44, "https://github.com/owner/repo/issues/44"); err != nil {
		t.Fatal(err)
	}

	manifestPath := writeLifecycleBundle(t, filepath.Join(root, "complete"), "bundle-complete-inventory", "present", "refs/heads/main", true)
	manifest := readManifest(t, manifestPath)
	manifest.Observations = []Observation{}
	manifest.ReplayProtection.Nonce = manifest.BundleID
	manifest.ReplayProtection.Key = replayKey(manifest)
	file := manifest.Files[0]
	manifest.OverallDigest = digestBundleContent(manifest, []string{fmt.Sprintf("file\x00%s\x00%s\x00%d", file.Path, file.SHA256, file.Size)})
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	complete := validateLocalBundle(t, manifestPath)
	result, err := lifecycle.ApplyBundle(complete, beadStore, ApplyLifecycleOptions{TargetRef: "main", VerificationRunID: "501", VerificationURL: "https://github.com/owner/repo/actions/runs/501"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 1 {
		t.Fatalf("expected exhaustive inventory to resolve omitted finding: %+v", result)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.ValidationRunID != "501" || finding.ValidationRunURL == "" {
		t.Fatalf("verification evidence was not persisted: %+v", finding)
	}
}

func TestLifecycleRejectsPostMergeAbsenceAtWrongSHA(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, _ := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-present-sha", "present", "refs/heads/main", true))
	_, _ = lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	_ = lifecycle.MarkIssueOpened(fingerprint, 55, "https://github.com/owner/repo/issues/55")
	_ = lifecycle.MarkRepairStarted(fingerprint, "hive/repair")
	_ = lifecycle.MarkPROpen(fingerprint, "repair-sha", 56, "https://github.com/owner/repo/pull/56")
	_ = lifecycle.MarkChecks(fingerprint, "repair-sha", true)
	_ = lifecycle.MarkMerged(fingerprint, "different-merge-sha")
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-absent-sha", "absent", "refs/heads/main", true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 0 || result.IgnoredAbsent != 1 {
		t.Fatalf("wrong-SHA absence must not resolve: %+v", result)
	}
}

func TestResolutionAllowsLegacyTestAdequacyOnlyWhenUnitLayerWasEvaluated(t *testing.T) {
	finding := &FindingLifecycle{RepositoryFingerprint: "finding", IssueKind: "test_adequacy_gap"}
	manifest := Manifest{
		Source: Source{Ref: "refs/heads/main"},
		Scan:   Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"testing-layer:2"}},
	}
	if allowed, reason := resolutionAllowed(finding, manifest, true, "main", ApplyLifecycleOptions{}); !allowed {
		t.Fatalf("verified Unit layer should resolve a legacy test-adequacy finding: %s", reason)
	}
	manifest.Scan.EvaluatedContracts = []string{"testing-layer:3"}
	if allowed, _ := resolutionAllowed(finding, manifest, true, "main", ApplyLifecycleOptions{}); allowed {
		t.Fatal("a different testing layer must not resolve the Unit adequacy finding")
	}
	manifest.Scan.EvaluatedContracts = []string{"testing-layer:2"}
	manifest.Source.CommitSHA = "target-head"
	manifest.Source.WorkflowRunID = "run-verified"
	finding.Status = StatusPostMergeVerifying
	finding.MergeSHA = "repair-merge"
	finding.ValidationRunID = "run-verified"
	verified := ApplyLifecycleOptions{VerificationRunID: "run-verified", VerificationCommitSHA: "target-head", VerifiedMergeAncestorFingerprint: "finding", VerifiedMergeAncestorSHA: "repair-merge"}
	if allowed, reason := resolutionAllowed(finding, manifest, true, "main", verified); !allowed {
		t.Fatalf("verified non-conflicting descendant should be accepted: %s", reason)
	}
	verified.VerificationCommitSHA = "different-head"
	if allowed, _ := resolutionAllowed(finding, manifest, true, "main", verified); allowed {
		t.Fatal("descendant authorization bound to a different verification SHA must be rejected")
	}
}

func TestResolutionAllowsRepositoryAuditFindingsOnlyForExactEvaluatedScope(t *testing.T) {
	manifest := Manifest{
		Source: Source{Ref: "refs/heads/main"},
		Scan:   Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"workflow-safety"}},
	}
	finding := &FindingLifecycle{IssueKind: "workflow_safety"}
	if allowed, reason := resolutionAllowed(finding, manifest, true, "main", ApplyLifecycleOptions{}); !allowed {
		t.Fatalf("verified workflow audit should resolve a repository workflow finding: %s", reason)
	}
	manifest.Scan.EvaluatedContracts = []string{"provider-governance"}
	if allowed, _ := resolutionAllowed(finding, manifest, true, "main", ApplyLifecycleOptions{}); allowed {
		t.Fatal("provider evidence must not resolve a workflow finding")
	}
	finding.IssueKind = "provider_governance"
	if allowed, reason := resolutionAllowed(finding, manifest, true, "main", ApplyLifecycleOptions{}); !allowed {
		t.Fatalf("verified provider evidence should resolve a provider finding: %s", reason)
	}
}

func TestLifecycleAuditIsAppendOnlyJSONL(t *testing.T) {
	root := t.TempDir()
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-audit", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(root, "lifecycle", "visual-hive-lifecycle-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil || count == 0 {
		t.Fatalf("expected append-only audit entries, count=%d err=%v", count, err)
	}
}

func TestIssuePublicationSelectionEnforcesActiveWIPAndRanksDirectFailuresFirst(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "coverage", State: "present", Severity: "high", IssueKind: "missing_visual_coverage"},
		{RepositoryFingerprint: "regression", State: "present", Severity: "high", IssueKind: "visual_regression"},
		{RepositoryFingerprint: "medium", State: "present", Severity: "medium", IssueKind: "visual_regression"},
	}
	findings := map[string]*FindingLifecycle{
		"existing": {RepositoryFingerprint: "existing", IssueNumber: 17, Status: StatusIssueOpen},
	}

	selected := selectIssuePublications("", observations, findings, 0, 2, false)

	if len(selected) != 1 || !selected["regression"] {
		t.Fatalf("expected one remaining slot to select the direct high-severity failure, got %v", selected)
	}
}

func TestIssuePublicationSelectionPrefersRepairableConsoleFailure(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "baseline", State: "present", Severity: "high", IssueKind: "screenshot_diff", Title: "Review missing baseline for deploy-preview"},
		{RepositoryFingerprint: "regression", State: "present", Severity: "high", IssueKind: "screenshot_diff", Title: "deploy-preview failed deterministic validation"},
		{RepositoryFingerprint: "console", State: "present", Severity: "high", IssueKind: "external_repo_onboarding", Title: "Repair deploy-preview: console_error"},
	}
	findings := map[string]*FindingLifecycle{
		"existing": {RepositoryFingerprint: "existing", IssueNumber: 17, Status: StatusIssueOpen, HumanReviewRequired: true},
	}

	selected := selectIssuePublications("", observations, findings, 0, 2, true)

	if len(selected) != 1 || !selected["console"] {
		t.Fatalf("expected repair mode to select the actionable console failure, got %v", selected)
	}
	if !observationNeedsHumanReview(observations[0]) {
		t.Fatal("missing-baseline observations must require human review")
	}
}

func TestIssuePublicationSelectionPrioritizesBlockingRepositoryPlan(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "visual", State: "present", Severity: "critical", IssueKind: "visual_regression", Title: "Critical visual regression"},
		{RepositoryFingerprint: "repository-plan", State: "present", Severity: "high", IssueKind: RepositoryTestFailureKind, Title: "Repository test failed"},
	}
	selected := selectIssuePublications("", observations, nil, 0, 1, true)
	if len(selected) != 1 || !selected["repository-plan"] {
		t.Fatalf("blocking repository plan did not reserve the bounded repair slot: %v", selected)
	}
}

func TestIssuePublicationSelectionPrefersTestOnlyRepairOverUnrepairableBacklog(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "onboarding", State: "present", Severity: "critical", IssueKind: "external_repo_onboarding", Title: "Review readiness gate"},
		{RepositoryFingerprint: "tests", State: "present", Severity: "high", IssueKind: "test_adequacy_gap", Title: "Add repository unit test coverage"},
	}
	selected := selectIssuePublications("", observations, nil, 0, 1, true)
	if len(selected) != 1 || !selected["tests"] {
		t.Fatalf("repair mode should select the bounded test-only repair before advisory backlog: %v", selected)
	}
	selected = selectIssuePublications("", observations, nil, 0, 1, false)
	if len(selected) != 1 || !selected["onboarding"] {
		t.Fatalf("issues-only mode should preserve severity ordering: %v", selected)
	}
}

func TestIssuePublicationSelectionCountsHumanHeldIssueAgainstRepositoryWIP(t *testing.T) {
	const repository = "owner/repo"
	scopeRoot := "test-adequacy/repository/scope-review"
	observations := []Observation{
		{RepositoryFingerprint: "scope-review", State: "present", Severity: "high", IssueKind: "missing_visual_coverage", PublicationRole: "canonical", RootCauseKey: scopeRoot},
		{RepositoryFingerprint: "actionable", State: "present", Severity: "medium", IssueKind: "test_adequacy_gap", Title: "Add deterministic contract assertions", PublicationRole: "canonical", RootCauseKey: "test-adequacy/repository/actionable"},
	}
	findings := map[string]*FindingLifecycle{
		"scope-review": {
			Repository: repository, RepositoryFingerprint: "scope-review", PublicationRole: "canonical", RootCauseKey: scopeRoot,
			PublicationFingerprint: digest([]byte(repository + "\x00" + scopeRoot)), IssueNumber: 17, Status: StatusIssueOpen,
			HumanReviewRequired: true, ManualReviewKind: "repair_scope",
		},
	}

	selected := selectIssuePublications(repository, observations, findings, 0, 1, true)
	if !selected["scope-review"] || selected["actionable"] {
		t.Fatalf("repair mode exceeded repository WIP while preserving the human-held issue: %v", selected)
	}
	selected = selectIssuePublications(repository, observations, findings, 0, 1, false)
	if selected["actionable"] {
		t.Fatalf("issues-only mode unexpectedly excluded a human-held issue from the active issue limit: %v", selected)
	}
}

func TestIssuePublicationSelectionPrefersContractScopedCoverageRepair(t *testing.T) {
	observations := []Observation{
		{RepositoryFingerprint: "generic", State: "present", Severity: "high", IssueKind: "missing_visual_coverage", Title: "Add mutation evidence"},
		{RepositoryFingerprint: "contract", State: "present", Severity: "medium", IssueKind: "missing_visual_coverage", Title: "Add deterministic flow steps", AffectedContracts: []string{"production-ready-marker"}},
	}
	selected := selectIssuePublications("", observations, nil, 0, 1, true)
	if len(selected) != 1 || !selected["contract"] {
		t.Fatalf("repair mode did not select the contract-scoped contribution before a generic advisory gap: %v", selected)
	}
	selected = selectIssuePublications("", observations, nil, 0, 1, false)
	if len(selected) != 1 || !selected["generic"] {
		t.Fatalf("issues-only mode did not preserve severity ordering: %v", selected)
	}
}

func TestPendingOpenIssueReservationConsumesRepositoryWIP(t *testing.T) {
	repository := "owner/repo"
	findings := map[string]*FindingLifecycle{
		"reserved": {Repository: repository, RepositoryFingerprint: "reserved", PublicationFingerprint: "publication-reserved", Status: StatusDetected, LastBundleDigest: "bundle-digest"},
	}
	outbox := []*OutboxEntry{{Action: OutboxOpenIssue, Repository: repository, RepositoryFingerprint: "reserved", BundleDigest: "bundle-digest"}}
	reserved := pendingOpenIssueReservationCount(repository, findings, outbox)
	if reserved != 1 {
		t.Fatalf("pending open-issue reservations = %d, want 1", reserved)
	}
	selected := selectIssuePublications(repository, []Observation{{RepositoryFingerprint: "second", State: "present", Severity: "critical", IssueKind: "visual_regression"}}, findings, reserved, 1, true)
	if selected["second"] {
		t.Fatalf("pending outbox reservation did not consume the repository WIP slot: %v", selected)
	}
}

func TestApplyBundleSequentialEvidenceApplicationsShareOnePendingWIPSlot(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	options := ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1, PreferRepairable: true}

	first := publicationTestObservation("first", "canonical", "test-adequacy/repository/first", "test_adequacy_gap", "Add the first deterministic contract assertion")
	firstBundle := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "first"), "bundle-pending-wip-first", []Observation{first}, false))
	firstResult, err := lifecycle.ApplyBundle(firstBundle, beadStore, options)
	if err != nil {
		t.Fatal(err)
	}
	pendingAfterFirst := lifecycle.PendingOutbox()
	if firstResult.OutboxCreated != 1 || len(pendingAfterFirst) != 1 || pendingAfterFirst[0].Action != OutboxOpenIssue {
		t.Fatalf("first evidence application did not reserve exactly one issue slot: result=%+v pending=%+v", firstResult, pendingAfterFirst)
	}

	second := publicationTestObservation("second", "canonical", "test-adequacy/repository/second", "test_adequacy_gap", "Add the second deterministic contract assertion")
	secondBundle := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "second"), "bundle-pending-wip-second", []Observation{second}, false))
	secondResult, err := lifecycle.ApplyBundle(secondBundle, beadStore, options)
	if err != nil {
		t.Fatal(err)
	}
	pendingAfterSecond := lifecycle.PendingOutbox()
	if secondResult.OutboxCreated != 0 || secondResult.Deferred != 1 {
		t.Fatalf("second evidence application exceeded the pending repository WIP slot: result=%+v", secondResult)
	}
	if len(pendingAfterSecond) != 1 || pendingAfterSecond[0].ID != pendingAfterFirst[0].ID {
		t.Fatalf("second evidence application replaced or duplicated the pending issue reservation: first=%+v second=%+v", pendingAfterFirst, pendingAfterSecond)
	}
	secondFingerprint := secondBundle.Manifest.Observations[0].RepositoryFingerprint
	secondFinding, ok := lifecycle.Finding(secondFingerprint)
	if !ok || secondFinding.IssueNumber != 0 || secondFinding.Status != StatusDetected {
		t.Fatalf("deferred finding was not retained locally without an issue: found=%t finding=%+v", ok, secondFinding)
	}
}

func TestApplyBundleOpenIssueIdenticalRerunIsNoopAtWIPOne(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	options := ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1, PreferRepairable: true}
	observation := publicationTestObservation("replay", "canonical", "test-adequacy/repository/replay", "test_adequacy_gap", "Add one deterministic contract assertion")
	bundle := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-open-issue-identical-replay", []Observation{observation}, false))

	first, err := lifecycle.ApplyBundle(bundle, beadStore, options)
	if err != nil {
		t.Fatal(err)
	}
	pending := lifecycle.PendingOutbox()
	if first.Created != 1 || first.OutboxCreated != 1 || first.Idempotent || len(pending) != 1 || pending[0].Action != OutboxOpenIssue {
		t.Fatalf("first application did not create one exact issue reservation: result=%+v pending=%+v", first, pending)
	}
	fingerprint := bundle.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 17, "https://example.test/issues/17"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkOutboxAttempt(pending[0].ID, nil); err != nil {
		t.Fatal(err)
	}

	replay, err := lifecycle.ApplyBundle(bundle, beadStore, options)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := lifecycle.Snapshot()
	finding, ok := snapshot.Findings[fingerprint]
	if !replay.Idempotent || replay.Created != 0 || replay.Updated != 0 || replay.OutboxCreated != 0 || replay.Deferred != 0 {
		t.Fatalf("identical evidence replay was not a true lifecycle no-op: %+v", replay)
	}
	if !ok || len(snapshot.Findings) != 1 || finding.IssueNumber != 17 || finding.Status != StatusIssueOpen {
		t.Fatalf("identical evidence replay changed the one open finding: found=%t findings=%+v", ok, snapshot.Findings)
	}
	if len(snapshot.Outbox) != 1 || snapshot.Outbox[0].ID != pending[0].ID || snapshot.Outbox[0].Attempts != 1 || snapshot.Outbox[0].CompletedAt == nil || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("identical evidence replay duplicated or reopened issue publication: outbox=%+v pending=%+v", snapshot.Outbox, lifecycle.PendingOutbox())
	}
	if beadStore.Count() != 1 || pendingOpenIssueReservationCount(bundle.Manifest.Source.Repository, snapshot.Findings, snapshot.Outbox) != 0 {
		t.Fatalf("identical evidence replay changed durable cardinality: beads=%d reservations=%d", beadStore.Count(), pendingOpenIssueReservationCount(bundle.Manifest.Source.Repository, snapshot.Findings, snapshot.Outbox))
	}
}

func TestApplyBundleManualReviewRerunDoesNotAdvanceIssueBacklog(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	options := ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1, PreferRepairable: true}

	held := publicationTestObservation("held", "canonical", "test-adequacy/repository/held", "test_adequacy_gap", "Add a deterministic contract assertion")
	firstBundle := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "first"), "bundle-manual-review-first", []Observation{held}, false))
	firstResult, err := lifecycle.ApplyBundle(firstBundle, beadStore, options)
	if err != nil {
		t.Fatal(err)
	}
	heldFingerprint := firstBundle.Manifest.Observations[0].RepositoryFingerprint
	if firstResult.OutboxCreated != 1 {
		t.Fatalf("first evidence application did not reserve one issue slot: %+v", firstResult)
	}
	if err := lifecycle.MarkIssueOpened(heldFingerprint, 17, "https://example.test/issues/17"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkManualReviewRequired(heldFingerprint, "repair_scope", "review the exact bounded repair scope"); err != nil {
		t.Fatal(err)
	}

	actionable := publicationTestObservation("actionable", "canonical", "test-adequacy/repository/actionable", "test_adequacy_gap", "Add another deterministic contract assertion")
	secondBundle := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "second"), "bundle-manual-review-rerun", []Observation{held, actionable}, false))
	secondResult, err := lifecycle.ApplyBundle(secondBundle, beadStore, options)
	if err != nil {
		t.Fatal(err)
	}
	actionableFingerprint := secondBundle.Manifest.Observations[1].RepositoryFingerprint
	if secondResult.Deferred != 1 {
		t.Fatalf("manual-review rerun did not defer exactly the issue backlog behind WIP=1: %+v", secondResult)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if entry.RepositoryFingerprint == actionableFingerprint && entry.Action == OutboxOpenIssue {
			t.Fatalf("manual-review rerun advanced a second issue into the outbox: %+v", entry)
		}
	}
	heldFinding, heldOK := lifecycle.Finding(heldFingerprint)
	if !heldOK || heldFinding.IssueNumber != 17 || !heldFinding.HumanReviewRequired || heldFinding.ManualReviewKind != "repair_scope" {
		t.Fatalf("rerun did not preserve the exact human-held issue: found=%t finding=%+v", heldOK, heldFinding)
	}
	actionableFinding, actionableOK := lifecycle.Finding(actionableFingerprint)
	if !actionableOK || actionableFinding.IssueNumber != 0 || actionableFinding.Status != StatusDetected {
		t.Fatalf("deferred backlog was not retained locally without publication: found=%t finding=%+v", actionableOK, actionableFinding)
	}
}

func TestExplicitRootPublicationCollapsesNineObservationsToTwoIssues(t *testing.T) {
	mutationRoot := "mutation/api-500/localPreview/dashboard-shell"
	testRoot := "test-adequacy/repository/testing-layer:2"
	observations := []Observation{
		{RepositoryFingerprint: "mutation", State: "present", Severity: "high", IssueKind: "mutation_survivor", PublicationRole: "canonical", RootCauseKey: mutationRoot},
		{RepositoryFingerprint: "tests", State: "present", Severity: "high", IssueKind: "test_adequacy_gap", PublicationRole: "canonical", RootCauseKey: testRoot},
		{RepositoryFingerprint: "mutation-maintenance", State: "present", Severity: "high", IssueKind: "missing_visual_coverage", PublicationRole: "derivative", RootCauseKey: mutationRoot},
		{RepositoryFingerprint: "mutation-strengthen", State: "present", Severity: "high", IssueKind: "missing_visual_coverage", PublicationRole: "derivative", RootCauseKey: mutationRoot},
		{RepositoryFingerprint: "mutation-adequacy", State: "present", Severity: "high", IssueKind: "external_repo_onboarding", PublicationRole: "derivative", RootCauseKey: mutationRoot},
		{RepositoryFingerprint: "mutation-handoff", State: "present", Severity: "high", IssueKind: "external_repo_onboarding", PublicationRole: "derivative", RootCauseKey: mutationRoot},
		{RepositoryFingerprint: "weak", State: "present", Severity: "high", IssueKind: "weak_visual_test", PublicationRole: "derivative", RootCauseKey: mutationRoot},
		{RepositoryFingerprint: "unit-map", State: "present", Severity: "low", IssueKind: "missing_visual_coverage", PublicationRole: "derivative", RootCauseKey: testRoot},
		{RepositoryFingerprint: "readiness", State: "present", Severity: "high", IssueKind: "external_repo_onboarding", PublicationRole: "aggregate", RootCauseKey: "aggregate/readiness/readiness_gate", BlockedByRootKeys: []string{mutationRoot}},
	}

	for _, maxActive := range []int{0, 10} {
		selected := selectIssuePublications("", observations, nil, 0, maxActive, true)
		if len(selected) != 2 || !selected["mutation"] || !selected["tests"] {
			t.Fatalf("maxActive=%d publication set = %v, want exact two canonical roots", maxActive, selected)
		}
	}
}

func TestExplicitRootPublicationFailsOpenForUncoveredValidLinkage(t *testing.T) {
	root := "mutation/api-500/localPreview/dashboard-shell"
	observations := []Observation{
		{RepositoryFingerprint: "uncovered", State: "present", Severity: "high", IssueKind: "missing_visual_coverage", PublicationRole: "derivative", RootCauseKey: root},
		{RepositoryFingerprint: "aggregate", State: "present", Severity: "high", IssueKind: "external_repo_onboarding", PublicationRole: "aggregate", RootCauseKey: "aggregate/readiness", BlockedByRootKeys: []string{root, "workflow/unknown"}},
		{RepositoryFingerprint: "legacy-text", State: "present", Severity: "high", IssueKind: "missing_visual_coverage", Title: "Maintain visual test: mutation_survivor"},
	}
	selected := selectIssuePublications("", observations, nil, 0, 0, true)
	if len(selected) != len(observations) {
		t.Fatalf("uncovered or legacy valid metadata was suppressed: %v", selected)
	}

	active := map[string]*FindingLifecycle{
		"owner": {Repository: "owner/repo", RepositoryFingerprint: "owner", PublicationFingerprint: "corrupt", RootCauseKey: root, IssueNumber: 7, Status: StatusIssueOpen},
	}
	selected = selectIssuePublications("owner/repo", observations[:1], active, 0, 0, true)
	if len(selected) != 1 || !selected["uncovered"] {
		t.Fatalf("invalid root ownership suppressed an independently actionable derivative: %v", selected)
	}
	active["owner"].PublicationFingerprint = digest([]byte("owner/repo\x00" + root))
	selected = selectIssuePublications("owner/repo", observations[:1], active, 0, 0, true)
	if len(selected) != 0 {
		t.Fatalf("derivative was not deferred by exact active root owner: %v", selected)
	}
}

func TestExplicitRootLifecyclePreservesNineBeadsAndPublishesTwoIssues(t *testing.T) {
	root := t.TempDir()
	mutationRoot := "mutation/api-500/localPreview/dashboard-shell"
	testRoot := "test-adequacy/repository/testing-layer:2"
	observations := []Observation{
		publicationTestObservation("mutation", "canonical", mutationRoot, "mutation_survivor", "Mutation survived: api-500"),
		publicationTestObservation("tests", "canonical", testRoot, "test_adequacy_gap", "Add repository unit tests"),
		publicationTestObservation("mutation-maintenance", "derivative", mutationRoot, "missing_visual_coverage", "Maintain mutation test"),
		publicationTestObservation("mutation-strengthen", "derivative", mutationRoot, "missing_visual_coverage", "Strengthen mutation test"),
		publicationTestObservation("mutation-adequacy", "derivative", mutationRoot, "external_repo_onboarding", "Mutation adequacy handoff"),
		publicationTestObservation("mutation-handoff", "derivative", mutationRoot, "external_repo_onboarding", "Mutation survivor handoff"),
		publicationTestObservation("weak", "derivative", mutationRoot, "weak_visual_test", "Weak mutation test"),
		publicationTestObservation("unit-map", "derivative", testRoot, "missing_visual_coverage", "Repository map unit gap"),
		publicationTestObservation("readiness", "aggregate", "aggregate/readiness/readiness_gate", "external_repo_onboarding", "Readiness handoff"),
	}
	observations[1].AffectedContracts = []string{"testing-layer:2"}
	observations[8].BlockedByRootKeys = []string{mutationRoot}
	bundle := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-nine-to-two", observations, true))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	result, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 10, PreferRepairable: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 9 || result.BeadsCreated != 9 || result.OutboxCreated != 2 || result.Deferred != 7 || beadStore.Count() != 9 || len(lifecycle.PendingOutbox()) != 2 {
		t.Fatalf("explicit publication did not preserve 9 observations/beads and collapse only issues to 2: result=%+v beads=%d outbox=%+v", result, beadStore.Count(), lifecycle.PendingOutbox())
	}
}

func TestCanonicalObservationUpdatesUncoveredDerivativeRootOwnerWithoutDuplicate(t *testing.T) {
	root := t.TempDir()
	rootKey := "mutation/api-500/localPreview/dashboard-shell"
	derivative := publicationTestObservation("derivative-source", "derivative", rootKey, "missing_visual_coverage", "Uncovered mutation test gap")
	first := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "first"), "bundle-root-owner-first", []Observation{derivative}, false))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if result, err := lifecycle.ApplyBundle(first, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil || result.OutboxCreated != 1 {
		t.Fatalf("uncovered derivative did not fail open: result=%+v err=%v", result, err)
	}
	client := &fakeLifecycleIssueClient{}
	policy := automation.Policy{ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"}}
	if result := ProcessOutbox(context.Background(), lifecycle, beadStore, policy, client); result.Succeeded != 1 {
		t.Fatalf("root owner issue open failed: %+v", result)
	}
	expectedMarker := lifecycleMarker(digest([]byte("owner/repo\x00" + rootKey)))
	if client.marker != expectedMarker {
		t.Fatalf("root marker = %q, want %q", client.marker, expectedMarker)
	}
	ownerBeforePartial, _ := lifecycle.Finding(first.Manifest.Observations[0].RepositoryFingerprint)
	derivative.Title, derivative.Body = "Derivative-only partial wording churn", "Derivative-only partial wording churn"
	partial := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "partial"), "bundle-root-owner-partial", []Observation{derivative}, false))
	if result, err := lifecycle.ApplyBundle(partial, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil || result.OutboxCreated != 0 || result.Deferred != 1 {
		t.Fatalf("active derivative owner was churned by partial evidence: result=%+v err=%v", result, err)
	}
	ownerAfterPartial, _ := lifecycle.Finding(first.Manifest.Observations[0].RepositoryFingerprint)
	if ownerAfterPartial.Title != ownerBeforePartial.Title || ownerAfterPartial.LastBundleDigest != ownerBeforePartial.LastBundleDigest || ownerAfterPartial.Status != StatusIssueOpen {
		t.Fatalf("derivative-only partial evidence changed active owner: before=%+v after=%+v", ownerBeforePartial, ownerAfterPartial)
	}

	canonical := publicationTestObservation("canonical-source-wording-v2", "canonical", rootKey, "mutation_survivor", "Canonical mutation api-500")
	second := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "second"), "bundle-root-owner-second", []Observation{canonical}, true))
	result, err := lifecycle.ApplyBundle(second, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil || result.OutboxCreated != 1 || result.Created != 1 {
		t.Fatalf("canonical root update failed: result=%+v err=%v", result, err)
	}
	pending := lifecycle.PendingOutbox()
	if len(pending) != 1 || pending[0].Action != OutboxUpdateIssue || pending[0].RepositoryFingerprint != first.Manifest.Observations[0].RepositoryFingerprint || pending[0].IssueNumber != 17 {
		t.Fatalf("canonical observation did not target the existing derivative root owner: %+v", pending)
	}
	if processed := ProcessOutbox(context.Background(), lifecycle, beadStore, policy, client); processed.Succeeded != 1 || client.upserts != 1 || client.updates != 1 || client.marker != expectedMarker {
		t.Fatalf("canonical update created or rebound the root issue: processed=%+v client=%+v", processed, client)
	}
	owner, _ := lifecycle.Finding(first.Manifest.Observations[0].RepositoryFingerprint)
	if owner.IssueNumber != 17 || owner.Title != canonical.Title || owner.Fingerprint != canonical.Fingerprint || owner.PublicationFingerprint != digest([]byte("owner/repo\x00"+rootKey)) {
		t.Fatalf("stable root owner did not adopt canonical content: %+v", owner)
	}

	canonicalV3 := publicationTestObservation("canonical-source-wording-v3", "canonical", rootKey, "mutation_survivor", "Canonical mutation api-500 with clearer wording")
	third := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "third"), "bundle-root-owner-third", []Observation{canonicalV3}, true))
	result, err = lifecycle.ApplyBundle(third, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil || result.Created != 0 || result.OutboxCreated != 1 {
		t.Fatalf("canonical wording/fingerprint update changed root identity: result=%+v err=%v", result, err)
	}
	if processed := ProcessOutbox(context.Background(), lifecycle, beadStore, policy, client); processed.Succeeded != 1 || client.upserts != 1 || client.updates != 2 || client.marker != expectedMarker {
		t.Fatalf("canonical wording/fingerprint update duplicated the issue: processed=%+v client=%+v", processed, client)
	}
	owner, _ = lifecycle.Finding(first.Manifest.Observations[0].RepositoryFingerprint)
	if owner.Title != canonicalV3.Title || owner.Fingerprint != canonicalV3.Fingerprint || owner.IssueNumber != 17 {
		t.Fatalf("canonical wording/fingerprint update did not refresh exact root owner: %+v", owner)
	}
}

func TestPublicationOwnerReopenPreservesLegacyAndControllerBeadStates(t *testing.T) {
	for _, test := range []struct {
		name       string
		controller bool
		wantStatus beads.Status
	}{
		{name: "ordinary ApplyBundle", wantStatus: beads.StatusOpen},
		{name: "sealed ApplyImportPlan", controller: true, wantStatus: beads.StatusBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootKey := "mutation/api-500/localPreview/dashboard-shell"
			firstBundle, firstPlan := verifiedPublicationOwnerPlan(t, "publication-owner-first-"+strings.ReplaceAll(test.name, " ", "-"), "derivative/source", "derivative", rootKey)
			secondBundle, secondPlan := verifiedPublicationOwnerPlan(t, "publication-owner-second-"+strings.ReplaceAll(test.name, " ", "-"), "canonical/source", "canonical", rootKey)
			lifecycle, err := NewLifecycleStore(filepath.Join(t.TempDir(), "lifecycle"))
			if err != nil {
				t.Fatal(err)
			}
			store := newTestBeadStore(t, filepath.Join(t.TempDir(), "beads"))
			if test.controller {
				_, err = lifecycle.ApplyImportPlan(firstPlan, store, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4})
			} else {
				_, err = lifecycle.ApplyBundle(firstBundle, store, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4})
			}
			if err != nil {
				t.Fatal(err)
			}
			ownerID := firstPlan.Works()[0].RepositoryFingerprint
			if err := lifecycle.MarkIssueOpened(ownerID, 17, "https://example.invalid/issues/17"); err != nil {
				t.Fatal(err)
			}
			owner, exists := lifecycle.Finding(ownerID)
			if !exists || owner.BeadID == "" {
				t.Fatalf("publication owner was not established: %+v", owner)
			}
			markFindingResolvedForReopen(t, lifecycle, ownerID)
			if err := store.Close(owner.BeadID); err != nil {
				t.Fatal(err)
			}
			var result ApplyLifecycleResult
			if test.controller {
				result, err = lifecycle.ApplyImportPlan(secondPlan, store, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4})
			} else {
				result, err = lifecycle.ApplyBundle(secondBundle, store, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4})
			}
			if err != nil || result.Reopened != 1 {
				t.Fatalf("publication owner reopen = %+v err=%v", result, err)
			}
			reopened, err := store.Get(owner.BeadID)
			if err != nil || reopened.Status != test.wantStatus || reopened.ClosedAt != nil {
				t.Fatalf("reopened publication owner bead = %+v err=%v, want status %s", reopened, err, test.wantStatus)
			}
		})
	}

	t.Run("owner update failure rolls lifecycle back", func(t *testing.T) {
		rootKey := "mutation/api-500/localPreview/dashboard-shell"
		firstBundle, firstPlan := verifiedPublicationOwnerPlan(t, "publication-owner-rollback-first", "derivative/rollback", "derivative", rootKey)
		secondBundle, secondPlan := verifiedPublicationOwnerPlan(t, "publication-owner-rollback-second", "canonical/rollback", "canonical", rootKey)
		lifecycle, err := NewLifecycleStore(filepath.Join(t.TempDir(), "lifecycle"))
		if err != nil {
			t.Fatal(err)
		}
		store := newTestBeadStore(t, filepath.Join(t.TempDir(), "beads"))
		if _, err := lifecycle.ApplyBundle(firstBundle, store, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4}); err != nil {
			t.Fatal(err)
		}
		ownerID := firstPlan.Works()[0].RepositoryFingerprint
		if err := lifecycle.MarkIssueOpened(ownerID, 17, "https://example.invalid/issues/17"); err != nil {
			t.Fatal(err)
		}
		owner, _ := lifecycle.Finding(ownerID)
		markFindingResolvedForReopen(t, lifecycle, ownerID)
		if err := store.Close(owner.BeadID); err != nil {
			t.Fatal(err)
		}
		canonicalWork := secondPlan.Works()[0]
		if _, err := store.Create(canonicalWork.Title, canonicalWork.Bead.Type, canonicalWork.Bead.Priority, "quality", canonicalWork.SourceExternalRef); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(lifecycle.Snapshot())
		failing := &failingLifecycleBeadUpdate{Store: store, failID: owner.BeadID}
		if _, err := lifecycle.ApplyBundle(secondBundle, failing, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4}); err == nil || !strings.Contains(err.Error(), "publication owner") {
			t.Fatalf("publication owner update failure = %v", err)
		}
		after, _ := json.Marshal(lifecycle.Snapshot())
		ownerBead, _ := store.Get(owner.BeadID)
		if string(after) != string(before) || ownerBead.Status != beads.StatusClosed || ownerBead.ClosedAt == nil {
			t.Fatalf("failed owner reopen changed lifecycle or bead: before=%s after=%s bead=%+v", before, after, ownerBead)
		}
	})
}

func verifiedPublicationOwnerPlan(t *testing.T, packetID, fingerprint, publicationRole, rootKey string) (*ValidatedBundle, VerifiedImportPlan) {
	t.Helper()
	manifestPath, sourceRoot := writeTestV3ObservationBundle(t)
	manifest := readManifest(t, manifestPath)
	observation := manifest.Observations[0]
	observation.Fingerprint = fingerprint
	observation.PublicationRole = publicationRole
	observation.RootCauseKey = rootKey
	fingerprintSource := fingerprint
	if publicationRole == "canonical" {
		fingerprintSource = rootKey
	}
	observation.RepositoryFingerprint = digest([]byte("owner/repo\x00" + fingerprintSource))
	observation.Title = publicationRole + " exact-root publication"
	if publicationRole == "derivative" {
		observation.IssueKind = "missing_visual_coverage"
		observation.OwningAgentHint = "visual-hive/test-creator"
	} else {
		observation.IssueKind = "mutation_survivor"
		observation.OwningAgentHint = "visual-hive/mutation"
	}
	manifest.Observations = []Observation{observation}
	manifest.BundleID = packetID
	manifest.ReplayProtection.Nonce = packetID
	manifest.ReplayProtection.Key = replayKeyForManifest(manifest)
	manifest.OverallDigest = digestV3BundleContent(manifest)
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{
		Now: time.Date(2026, 7, 9, 12, 30, 0, 0, time.UTC), MaxACMM: 6, VerifiedProvenance: true,
		ExpectedProducerGitCommit: testV3ProducerCommit, ExpectedRepository: "owner/repo", ExpectedRepositoryID: "123",
		ExpectedWorkflowRunID: "42", ExpectedWorkflowRunAttempt: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.VerifySourceArtifact(sourceRoot); err != nil {
		t.Fatal(err)
	}
	plan, err := bundle.BuildImportPlan()
	if err != nil {
		t.Fatal(err)
	}
	return bundle, plan
}

func TestResolveImportPlanWorksRemapsCanonicalObservationToDurableLegacyOwner(t *testing.T) {
	canonicalBundle, canonicalPlan := verifiedPublicationOwnerPlan(t, "canonical-owner-new", "shared/source", "canonical", "shared/root")
	legacyManifest, err := cloneCanonicalManifest(canonicalBundle.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	legacyManifest.BundleID = "canonical-owner-legacy"
	legacyManifest.Observations[0].PublicationRole = ""
	legacyManifest.Observations[0].RootCauseKey = ""
	legacyManifest.Observations[0].RepositoryFingerprint = digest([]byte("owner/repo\x00" + legacyManifest.Observations[0].Fingerprint))
	legacyManifest.ReplayProtection.Nonce = legacyManifest.BundleID
	legacyManifest.ReplayProtection.Key = replayKeyForManifest(legacyManifest)
	legacyManifest.OverallDigest = digestV3BundleContent(legacyManifest)
	legacyManifest.Provenance.SubjectDigest = legacyManifest.OverallDigest
	legacyBundle := *canonicalBundle
	legacyBundle.Manifest = legacyManifest
	legacyBundle.validatedManifest = &legacyManifest
	lifecycle, _ := NewLifecycleStore(filepath.Join(t.TempDir(), "lifecycle"))
	legacyStore := newTestBeadStore(t, filepath.Join(t.TempDir(), "legacy"))
	normalStore := newTestBeadStore(t, filepath.Join(t.TempDir(), "normal"))
	if _, err := lifecycle.ApplyBundle(&legacyBundle, legacyStore, ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 4}); err != nil {
		t.Fatal(err)
	}
	legacyID := legacyManifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(legacyID, 27, "https://example.invalid/issues/27"); err != nil {
		t.Fatal(err)
	}
	resolved, err := lifecycle.ResolveImportPlanWorks(canonicalPlan, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true, MaxActiveIssues: 4})
	if err != nil || len(resolved) != 1 {
		t.Fatalf("resolved canonical works = %+v err=%v", resolved, err)
	}
	sealed := canonicalPlan.Works()[0]
	if resolved[0].RepositoryFingerprint != legacyID || resolved[0].ObservationFingerprint != sealed.RepositoryFingerprint || resolved[0].FindingFingerprint != sealed.FindingFingerprint || resolved[0].Title != sealed.Title || resolved[0].SourceExternalRef != beadExternalRef("owner/repo", legacyID) {
		t.Fatalf("canonical observation was not safely mapped to durable owner: sealed=%+v resolved=%+v", sealed, resolved[0])
	}
	if _, err := lifecycle.ApplyImportPlan(canonicalPlan, normalStore, ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true, MaxActiveIssues: 4}); err != nil {
		t.Fatal(err)
	}
	if normalStore.Count() != 1 || normalStore.FindByExternalRef(resolved[0].SourceExternalRef) == nil || normalStore.FindByExternalRef(sealed.SourceExternalRef) != nil {
		t.Fatalf("canonical owner projection duplicated identity: normal=%+v", normalStore.List(beads.ListFilter{}))
	}
}

func markFindingResolvedForReopen(t *testing.T, lifecycle *LifecycleStore, repositoryFingerprint string) {
	t.Helper()
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	finding := lifecycle.state.Findings[repositoryFingerprint]
	if finding == nil {
		t.Fatalf("finding %s is missing", repositoryFingerprint)
	}
	now := time.Now().UTC()
	finding.Status = StatusResolved
	finding.ResolvedAt = &now
	if err := lifecycle.persistLocked(); err != nil {
		t.Fatal(err)
	}
}

type failingLifecycleBeadUpdate struct {
	*beads.Store
	failID string
}

func (sink *failingLifecycleBeadUpdate) Update(id string, update func(*beads.Bead)) error {
	if id == sink.failID {
		return fmt.Errorf("injected publication owner update failure")
	}
	return sink.Store.Update(id, update)
}

func TestExplicitMetadataNeverSilentlyRebindsOrClosesLegacyDerivativeIssue(t *testing.T) {
	root := t.TempDir()
	legacy := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "legacy"), "bundle-legacy-derivative", "present", "main", true))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(legacy, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	legacyID := legacy.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(legacyID, 16, "https://github.test/owner/repo/issues/16"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}

	}
	rootKey := "mutation/api-500/localPreview/dashboard-shell"
	canonical := publicationTestObservation("canonical-new", "canonical", rootKey, "mutation_survivor", "Canonical mutation")
	derivative := publicationTestObservation(legacy.Manifest.Observations[0].Fingerprint, "derivative", rootKey, "missing_visual_coverage", "Legacy derivative now has explicit metadata")
	current := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "current"), "bundle-legacy-derivative-current", []Observation{canonical, derivative}, true))
	result, err := lifecycle.ApplyBundle(current, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	legacyFinding, _ := lifecycle.Finding(legacyID)
	if legacyFinding.IssueNumber != 16 || legacyFinding.Status != StatusIssueOpen || legacyFinding.RootCauseKey != "" || legacyFinding.PublicationRole != "" || legacyFinding.PendingMarkerMigrationFrom != "" {
		t.Fatalf("legacy derivative issue was silently rebound or closed: %+v", legacyFinding)
	}
	pending := lifecycle.PendingOutbox()
	if result.OutboxCreated != 1 || len(pending) != 1 || pending[0].Action != OutboxOpenIssue || pending[0].RepositoryFingerprint == legacyID {
		t.Fatalf("explicit canonical publication did not remain separate from legacy identity: result=%+v pending=%+v", result, pending)
	}
}

func TestExactLegacyCanonicalMigrationPreservesIssueBeadAndRepairIdentity(t *testing.T) {
	root := t.TempDir()
	const sourceFingerprint = "visual-hive:mutation:api-500"
	const rootKey = "mutation/api-500/localPreview/dashboard-shell"
	legacy := validateLocalBundle(t, writeLegacyLifecycleObservationBundle(t, filepath.Join(root, "legacy"), "bundle-legacy-canonical", sourceFingerprint, "mutation_survivor"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if result, err := lifecycle.ApplyBundle(legacy, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1}); err != nil || result.BeadsCreated != 1 {
		t.Fatalf("legacy canonical setup failed: result=%+v err=%v", result, err)
	}
	legacyID := legacy.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(legacyID, 15, "https://github.test/owner/repo/issues/15"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := lifecycle.MarkRepairStarted(legacyID, "hive/repair-legacy-canonical"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(legacyID, strings.Repeat("a", 40), 19, "https://github.test/owner/repo/pull/19"); err != nil {
		t.Fatal(err)
	}
	before, _ := lifecycle.Finding(legacyID)

	canonical := publicationTestObservation(sourceFingerprint, "canonical", rootKey, "mutation_survivor", "Mutation survived: api-500")
	current := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "current"), "bundle-explicit-canonical", []Observation{canonical}, false))
	result, err := lifecycle.ApplyBundle(current, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1})
	if err != nil {
		t.Fatal(err)
	}
	newCanonicalID := current.Manifest.Observations[0].RepositoryFingerprint
	if result.Created != 0 || result.BeadsCreated != 0 || result.OutboxCreated != 1 || beadStore.Count() != 1 {
		t.Fatalf("exact legacy migration duplicated lifecycle state: result=%+v beads=%d", result, beadStore.Count())
	}
	if _, exists := lifecycle.Finding(newCanonicalID); exists {
		t.Fatalf("exact legacy migration created a second root finding %s", newCanonicalID)
	}
	pending := lifecycle.PendingOutbox()
	expectedPreviousMarker := lifecycleMarker(legacyID)
	if len(pending) != 1 || pending[0].Action != OutboxUpdateIssue || pending[0].RepositoryFingerprint != legacyID || pending[0].IssueNumber != 15 || pending[0].PreviousMarker != expectedPreviousMarker {
		t.Fatalf("migration did not directly update the exact existing issue: %+v", pending)
	}
	after, _ := lifecycle.Finding(legacyID)
	if after.IssueNumber != before.IssueNumber || after.BeadID != before.BeadID || after.Branch != before.Branch || after.PRNumber != before.PRNumber || after.PRURL != before.PRURL || after.RepairCommitSHA != before.RepairCommitSHA ||
		after.RootCauseKey != rootKey || after.PublicationRole != "canonical" || after.PublicationFingerprint != digest([]byte("owner/repo\x00"+rootKey)) || after.RepositoryFingerprint != legacyID || after.PendingMarkerMigrationFrom != expectedPreviousMarker {
		t.Fatalf("migration did not preserve legacy issue/bead/repair identity: before=%+v after=%+v", before, after)
	}
	// New evidence may supersede this outbox record before GitHub is reached.
	// The exact old marker must remain durable on the replacement record.
	canonical.Title, canonical.Body = "Mutation survived: api-500 (new evidence)", "Mutation survived: api-500 (new evidence)"
	newerPending := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "newer-pending"), "bundle-explicit-canonical-newer-pending", []Observation{canonical}, false))
	if result, err = lifecycle.ApplyBundle(newerPending, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1}); err != nil || result.OutboxCreated != 1 {
		t.Fatalf("newer evidence did not preserve the pending marker migration: result=%+v err=%v", result, err)
	}
	pending = lifecycle.PendingOutbox()
	if len(pending) != 2 || pending[1].PreviousMarker != expectedPreviousMarker {
		t.Fatalf("replacement outbox lost the durable old marker: %+v", pending)
	}
	client := &fakeLifecycleIssueClient{migrationErr: fmt.Errorf("authenticated old-marker verification failed")}
	policy := automation.Policy{ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"}}
	if denied := ProcessOutbox(context.Background(), lifecycle, beadStore, policy, client); denied.StaleSkipped != 1 || denied.Failed != 1 || denied.Succeeded != 0 {
		t.Fatalf("failed remote ownership verification was not retained for retry: result=%+v client=%+v", denied, client)
	}
	if retained, _ := lifecycle.Finding(legacyID); retained.PendingMarkerMigrationFrom != expectedPreviousMarker || len(lifecycle.PendingOutbox()) != 1 {
		t.Fatalf("failed marker migration was not durable: finding=%+v pending=%+v", retained, lifecycle.PendingOutbox())
	}
	client.migrationErr = nil
	if processed := ProcessOutbox(context.Background(), lifecycle, beadStore, policy, client); processed.Succeeded != 1 || client.upserts != 0 || client.updates != 0 || client.migrations != 2 || client.previousMarker != expectedPreviousMarker || client.marker != lifecycleMarker(digest([]byte("owner/repo\x00"+rootKey))) {
		t.Fatalf("migration did not use a direct issue update with the stable root marker: result=%+v client=%+v", processed, client)
	}
	if migrated, _ := lifecycle.Finding(legacyID); migrated.PendingMarkerMigrationFrom != "" {
		t.Fatalf("completed marker migration remained pending: %+v", migrated)
	}

	// A later canonical scan must continue using the migrated identity without
	// creating the new root-key bead or finding on a second run.
	canonical.Title, canonical.Body = "Mutation survived: api-500 (confirmed)", "Mutation survived: api-500 (confirmed)"
	later := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "later"), "bundle-explicit-canonical-later", []Observation{canonical}, false))
	if result, err = lifecycle.ApplyBundle(later, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1}); err != nil || result.Created != 0 || result.BeadsCreated != 0 || result.OutboxCreated != 1 || beadStore.Count() != 1 {
		t.Fatalf("migrated owner was not stable on a later canonical scan: result=%+v err=%v beads=%d", result, err, beadStore.Count())
	}
}

func TestLegacyDerivativeAndAggregateKindsAreNeverMigrationCandidates(t *testing.T) {
	for _, issueKind := range []string{"missing_visual_coverage", "weak_visual_test", "external_repo_onboarding"} {
		t.Run(issueKind, func(t *testing.T) {
			const fingerprint = "visual-hive:legacy-publication"
			repositoryFingerprint := digest([]byte("owner/repo\x00" + fingerprint))
			findings := map[string]*FindingLifecycle{
				repositoryFingerprint: {
					Repository: "owner/repo", Fingerprint: fingerprint, RepositoryFingerprint: repositoryFingerprint,
					IssueKind: issueKind, IssueNumber: 17, IssueURL: "https://github.test/owner/repo/issues/17", Status: StatusIssueOpen,
				},
			}
			observation := Observation{Fingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: "root/future", State: "present", IssueKind: issueKind}
			if matches := exactLegacyCanonicalMigrationMatches("owner/repo", findings, observation); len(matches) != 0 {
				t.Fatalf("legacy derivative/aggregate-compatible kind was eligible for migration: %+v", matches)
			}
		})
	}
}

func TestAmbiguousLegacyCanonicalMigrationAdoptsNeitherAndDoesNotStarveRoot(t *testing.T) {
	root := t.TempDir()
	const sourceFingerprint = "visual-hive:mutation:api-500"
	legacy := validateLocalBundle(t, writeLegacyLifecycleObservationBundle(t, filepath.Join(root, "legacy"), "bundle-legacy-ambiguous", sourceFingerprint, "mutation_survivor"))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(legacy, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1}); err != nil {
		t.Fatal(err)
	}
	legacyID := legacy.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(legacyID, 15, "https://github.test/owner/repo/issues/15"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	duplicate := *lifecycle.state.Findings[legacyID]
	duplicate.IssueNumber = 16
	duplicate.IssueURL = "https://github.test/owner/repo/issues/16"
	lifecycle.state.Findings["ambiguous-state-key"] = &duplicate

	const rootKey = "mutation/api-500/localPreview/dashboard-shell"
	canonical := publicationTestObservation(sourceFingerprint, "canonical", rootKey, "mutation_survivor", "Mutation survived: api-500")
	current := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "current"), "bundle-explicit-ambiguous", []Observation{canonical}, true))
	result, err := lifecycle.ApplyBundle(current, beadStore, ApplyLifecycleOptions{TargetRef: "main", MaxActiveIssues: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{legacyID, "ambiguous-state-key"} {
		finding := lifecycle.state.Findings[key]
		if finding.RootCauseKey != "" || finding.PublicationRole != "" || finding.Status == StatusResolved || finding.Status == StatusIssueClosed {
			t.Fatalf("ambiguous legacy issue %s was adopted or closed: %+v", key, finding)
		}
	}
	canonicalID := current.Manifest.Observations[0].RepositoryFingerprint
	canonicalFinding, exists := lifecycle.Finding(canonicalID)
	if !exists || canonicalFinding.RootCauseKey != rootKey || result.Created != 1 || result.OutboxCreated != 1 {
		t.Fatalf("legacy WIP starved the independently owned canonical root: result=%+v finding=%+v", result, canonicalFinding)
	}
	pending := lifecycle.PendingOutbox()
	if len(pending) != 1 || pending[0].Action != OutboxOpenIssue || pending[0].RepositoryFingerprint != canonicalID {
		t.Fatalf("ambiguous migration did not fail closed to a new exact-marker issue: %+v", pending)
	}
}

func TestAuthoritativelyAbsentRootUnblocksAggregateInSameCycle(t *testing.T) {
	root := t.TempDir()
	const rootKey = "mutation/api-500/localPreview/dashboard-shell"
	canonical := publicationTestObservation("canonical", "canonical", rootKey, "mutation_survivor", "Mutation survived: api-500")
	present := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "present"), "bundle-root-before-absence", []Observation{canonical}, true))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	ownerID := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(ownerID, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}

	absentCanonical := canonical
	absentCanonical.State = "absent"
	aggregate := publicationTestObservation("readiness", "aggregate", "aggregate/readiness", "external_repo_onboarding", "Repository readiness handoff")
	aggregate.BlockedByRootKeys = []string{rootKey}
	current := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "current"), "bundle-root-absent-aggregate-present", []Observation{absentCanonical, aggregate}, true))
	result, err := lifecycle.ApplyBundle(current, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 1 || result.Created != 1 || result.OutboxCreated != 2 || result.Deferred != 0 {
		t.Fatalf("authoritatively absent root deferred the now-actionable aggregate for another cycle: %+v", result)
	}
	pending := lifecycle.PendingOutbox()
	if len(pending) != 2 || pending[0].Action != OutboxCloseIssue || pending[1].Action != OutboxOpenIssue {
		t.Fatalf("same-cycle root close and aggregate open were not both scheduled: %+v", pending)
	}
}

func TestRootOwnerClosesOnlyAfterRootWideAuthoritativeAbsence(t *testing.T) {
	root := t.TempDir()
	rootKey := "mutation/api-500/localPreview/dashboard-shell"
	canonical := publicationTestObservation("canonical", "canonical", rootKey, "mutation_survivor", "Canonical mutation")
	derivative := publicationTestObservation("derivative", "derivative", rootKey, "missing_visual_coverage", "Derivative mutation guidance")
	present := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "present"), "bundle-root-present", []Observation{canonical, derivative}, true))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	ownerID := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(ownerID, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := lifecycle.Finding(ownerID)

	partial := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "partial"), "bundle-root-partial", []Observation{derivative}, false))
	if result, err := lifecycle.ApplyBundle(partial, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil || result.Resolved != 0 || result.OutboxCreated != 0 {
		t.Fatalf("derivative-only partial evidence churned the owner: result=%+v err=%v", result, err)
	}
	afterPartial, _ := lifecycle.Finding(ownerID)
	if afterPartial.Status != StatusIssueOpen || afterPartial.LastBundleDigest != before.LastBundleDigest || afterPartial.Title != before.Title {
		t.Fatalf("derivative-only partial evidence changed the root owner: before=%+v after=%+v", before, afterPartial)
	}

	fullDerivative := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "full-derivative"), "bundle-root-full-derivative", []Observation{derivative}, true))
	if result, err := lifecycle.ApplyBundle(fullDerivative, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil || result.Resolved != 0 || result.OutboxCreated != 0 {
		t.Fatalf("present derivative allowed root owner closure: result=%+v err=%v", result, err)
	}
	if owner, _ := lifecycle.Finding(ownerID); owner.Status != StatusIssueOpen {
		t.Fatalf("root owner closed while exact-root evidence remained: %+v", owner)
	}

	absent := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-root-absent", nil, true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil || result.Resolved != 2 || result.OutboxCreated != 1 {
		t.Fatalf("root-wide authoritative absence did not resolve root evidence: result=%+v err=%v", result, err)
	}
	owner, _ := lifecycle.Finding(ownerID)
	pending := lifecycle.PendingOutbox()
	if owner.Status != StatusResolved || len(pending) != 1 || pending[0].Action != OutboxCloseIssue || pending[0].RepositoryFingerprint != ownerID {
		t.Fatalf("root owner close was not exact and singular: owner=%+v pending=%+v", owner, pending)
	}
}

func TestCanonicalRootClosureResolvesContractlessDeferredDerivative(t *testing.T) {
	root := t.TempDir()
	rootKey := "test-adequacy/repository/testing-layer:2"
	canonical := publicationTestObservation("canonical", "canonical", rootKey, "test_adequacy_gap", "Add unit coverage")
	canonical.AffectedContracts = []string{"testing-layer:2"}
	derivative := publicationTestObservation("derivative", "derivative", rootKey, "missing_visual_coverage", "Repository map unit gap")
	derivative.AffectedContracts = nil
	present := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "present"), "bundle-contractless-derivative-present", []Observation{canonical, derivative}, true))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	ownerID := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(ownerID, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}

	absent := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-contractless-derivative-absent", nil, true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 2 || result.OutboxCreated != 1 {
		t.Fatalf("canonical proof did not resolve its contractless deferred derivative and owner: %+v", result)
	}
	owner, _ := lifecycle.Finding(ownerID)
	pending := lifecycle.PendingOutbox()
	if owner.Status != StatusResolved || len(pending) != 1 || pending[0].Action != OutboxCloseIssue || pending[0].RepositoryFingerprint != ownerID {
		t.Fatalf("canonical issue closure was not exact and singular: owner=%+v pending=%+v", owner, pending)
	}
}

func TestCanonicalRootClosureCannotSubstituteForDerivativeDistinctContract(t *testing.T) {
	root := t.TempDir()
	rootKey := "test-adequacy/repository/testing-layer:2"
	canonical := publicationTestObservation("canonical", "canonical", rootKey, "test_adequacy_gap", "Add unit coverage")
	canonical.AffectedContracts = []string{"testing-layer:2"}
	derivative := publicationTestObservation("derivative", "derivative", rootKey, "missing_visual_coverage", "Distinct integration gap")
	derivative.AffectedContracts = []string{"integration-contract-not-evaluated"}
	present := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "present"), "bundle-distinct-derivative-present", []Observation{canonical, derivative}, true))
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	ownerID := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(ownerID, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}

	absent := validateLocalBundle(t, writePublicationLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-distinct-derivative-absent", nil, true))
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 0 || result.OutboxCreated != 0 {
		t.Fatalf("canonical contract incorrectly substituted for a derivative's distinct unevaluated contract: %+v", result)
	}
	owner, _ := lifecycle.Finding(ownerID)
	if owner.Status != StatusIssueOpen {
		t.Fatalf("canonical issue closed despite an unresolved distinct-contract facet: %+v", owner)
	}
}

func TestLifecycleRefusesResolutionFromStaleDefaultHead(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-stale-present", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-stale-absent", "absent", "main", true))
	if absent.Manifest.Source.CommitSHA == "new-default-head" {
		t.Fatal("test fixture unexpectedly uses the adversarial live head")
	}
	result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: "new-default-head"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Resolved != 0 || result.IgnoredAbsent != 1 {
		t.Fatalf("stale authoritative absence was accepted: %+v", result)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusIssueOpen {
		t.Fatalf("stale default-head evidence changed lifecycle status: %+v", finding)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if entry.Action == OutboxCloseIssue {
			t.Fatalf("stale default-head evidence enqueued issue closure: %+v", entry)
		}
	}
}

func TestAuthoritativeRescanRefreshesPendingInferredClose(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-rescan-present", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 18, "https://github.test/owner/repo/issues/18"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}

	oldHead := strings.Repeat("a", 40)
	oldAbsent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "old-absent"), "bundle-rescan-old", "absent", "main", true))
	oldAbsent.Manifest.Source.CommitSHA = oldHead
	oldAbsent.Manifest.Observations = nil
	if result, err := lifecycle.ApplyBundle(oldAbsent, beadStore, ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: oldHead}); err != nil || result.Resolved != 1 {
		t.Fatalf("first inferred absence was not applied: result=%+v err=%v", result, err)
	}
	oldClose := lifecycle.PendingOutbox()[0]

	newHead := strings.Repeat("b", 40)
	newAbsent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "new-absent"), "bundle-rescan-new", "absent", "main", true))
	newAbsent.Manifest.Source.CommitSHA = newHead
	newAbsent.Manifest.Observations = nil
	if result, err := lifecycle.ApplyBundle(newAbsent, beadStore, ApplyLifecycleOptions{TargetRef: "main", CurrentTargetCommitSHA: newHead}); err != nil || result.Resolved != 1 || result.OutboxCreated != 1 {
		t.Fatalf("current inferred absence did not refresh the pending close: result=%+v err=%v", result, err)
	}
	pending := lifecycle.PendingOutbox()
	if len(pending) != 2 || pending[0].ID != oldClose.ID || pending[1].BundleDigest != newAbsent.Manifest.OverallDigest {
		t.Fatalf("rescan did not retain stale close for deterministic supersession and enqueue current close: %+v", pending)
	}
	client := &fakeLifecycleIssueClient{}
	policy := automation.Policy{ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"}}
	processed := ProcessOutbox(context.Background(), lifecycle, beadStore, policy, client)
	if processed.StaleSkipped != 1 || processed.Succeeded != 1 || processed.Failed != 0 || client.updates != 1 {
		t.Fatalf("only the current close should reach GitHub: result=%+v updates=%d", processed, client.updates)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusIssueClosed || finding.LastBundleDigest != newAbsent.Manifest.OverallDigest {
		t.Fatalf("finding was not closed by current-head evidence: %+v", finding)
	}
}

func TestResetStalePostMergeVerificationRestoresMergeAndDiscardsClose(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	present := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "present"), "bundle-reset-present", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(present, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := present.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.MarkIssueOpened(fingerprint, 19, "https://github.test/owner/repo/issues/19"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range lifecycle.PendingOutbox() {
		if err := lifecycle.MarkOutboxAttempt(entry.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := lifecycle.MarkRepairStarted(fingerprint, "hive/repair-reset"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(fingerprint, "repair-reset-head", 29, "https://github.test/owner/repo/pull/29"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkChecks(fingerprint, "repair-reset-head", true); err != nil {
		t.Fatal(err)
	}
	staleHead := strings.Repeat("c", 40)
	if err := lifecycle.MarkMerged(fingerprint, staleHead); err != nil {
		t.Fatal(err)
	}
	const staleRun = "700"
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, staleRun, "https://github.test/owner/repo/actions/runs/700"); err != nil {
		t.Fatal(err)
	}
	absent := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "absent"), "bundle-reset-absent", "absent", "main", true))
	absent.Manifest.Source.CommitSHA = staleHead
	absent.Manifest.Source.WorkflowRunID = staleRun
	if result, err := lifecycle.ApplyBundle(absent, beadStore, ApplyLifecycleOptions{
		TargetRef: "main", CurrentTargetCommitSHA: staleHead, VerificationRunID: staleRun, VerificationCommitSHA: staleHead,
	}); err != nil || result.Resolved != 1 {
		t.Fatalf("stale post-merge absence was not durably applied: result=%+v err=%v", result, err)
	}
	if err := lifecycle.ResetStalePostMergeVerification(fingerprint, "wrong-run", staleHead); err == nil {
		t.Fatal("mismatched run reset was accepted")
	}
	if err := lifecycle.ResetStalePostMergeVerification(fingerprint, staleRun, staleHead); err != nil {
		t.Fatal(err)
	}
	finding, _ := lifecycle.Finding(fingerprint)
	if finding.Status != StatusMerged || finding.ValidationRunID != "" || finding.ResolvedAt != nil || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("stale post-merge state was not restored atomically: finding=%+v pending=%+v", finding, lifecycle.PendingOutbox())
	}
	if err := lifecycle.MarkPostMergeVerifying(fingerprint, "701", "https://github.test/owner/repo/actions/runs/701"); err != nil {
		t.Fatalf("fresh verification could not restart: %v", err)
	}
	if err := lifecycle.ResetStalePostMergeVerification(fingerprint, "701", strings.Repeat("d", 40)); err != nil {
		t.Fatalf("pre-apply stale verification could not reset: %v", err)
	}
	if finding, _ = lifecycle.Finding(fingerprint); finding.Status != StatusMerged {
		t.Fatalf("pre-apply reset did not restore merged state: %+v", finding)
	}
}

func writeLifecycleBundle(t *testing.T, root, bundleID, state, ref string, authoritative bool) string {
	t.Helper()
	manifestPath := writeTestBundle(t, root, false)
	manifest := readManifest(t, manifestPath)
	manifest.BundleID = bundleID
	manifest.Source.Ref = ref
	manifest.Scan.AuthoritativeForResolution = authoritative
	if authoritative {
		manifest.Scan.Scope = "full"
	} else {
		manifest.Scan.Scope = "partial"
	}
	manifest.Observations[0].State = state
	manifest.ReplayProtection.Nonce = bundleID
	manifest.ReplayProtection.Key = replayKey(manifest)
	file := manifest.Files[0]
	manifest.OverallDigest = digestBundleContent(manifest, []string{fmt.Sprintf("file\x00%s\x00%s\x00%d", file.Path, file.SHA256, file.Size)})
	manifest.Provenance.SubjectDigest = manifest.OverallDigest
	writeManifest(t, manifestPath, manifest)
	return manifestPath
}

func publicationTestObservation(fingerprint, role, rootCauseKey, issueKind, title string) Observation {
	return Observation{
		Fingerprint: fingerprint, State: "present", PublicationRole: role, RootCauseKey: rootCauseKey,
		IssueKind: issueKind, Severity: "high", Title: title, Body: title,
		AffectedContracts: []string{"app-shell"}, BlockedByRootKeys: []string{},
	}
}

func writeLegacyLifecycleObservationBundle(t *testing.T, root, bundleID, fingerprint, issueKind string) string {
	t.Helper()
	manifestPath := writeTestBundle(t, root, false)
	manifest := readManifest(t, manifestPath)
	manifest.BundleID = bundleID
	manifest.Source.Ref = "refs/heads/main"
	manifest.Scan.Scope = "full"
	manifest.Scan.AuthoritativeForResolution = true
	manifest.Observations[0].Fingerprint = fingerprint
	manifest.Observations[0].RepositoryFingerprint = digest([]byte("owner/repo\x00" + fingerprint))
	manifest.Observations[0].IssueKind = issueKind
	manifest.Observations[0].PublicationRole = ""
	manifest.Observations[0].RootCauseKey = ""
	manifest.Observations[0].BlockedByRootKeys = nil
	manifest.ReplayProtection.Nonce = bundleID
	manifest.ReplayProtection.Key = replayKey(manifest)
	sealTestManifest(t, manifestPath, &manifest)
	return manifestPath
}

func writePublicationLifecycleBundle(t *testing.T, root, bundleID string, observations []Observation, authoritative bool) string {
	t.Helper()
	manifestPath := writeTestBundle(t, root, false)
	manifest := readManifest(t, manifestPath)
	template := manifest.Observations[0]
	manifest.BundleID = bundleID
	manifest.DigestAlgorithm = PublicationDigestAlgorithm
	manifest.Source.Ref = "refs/heads/main"
	manifest.Scan.AuthoritativeForResolution = authoritative
	manifest.Scan.Scope = "partial"
	if authoritative {
		manifest.Scan.Scope = "full"
	}
	manifest.Scan.EvaluatedContracts = []string{"app-shell", "testing-layer:2"}
	manifest.Observations = make([]Observation, 0, len(observations))
	for _, input := range observations {
		observation := template
		observation.Fingerprint = input.Fingerprint
		observation.PublicationRole = input.PublicationRole
		observation.RootCauseKey = input.RootCauseKey
		observation.BlockedByRootKeys = nil
		if input.BlockedByRootKeys != nil {
			observation.BlockedByRootKeys = append([]string{}, input.BlockedByRootKeys...)
		}
		observation.State = input.State
		observation.IssueKind = input.IssueKind
		observation.Severity = input.Severity
		observation.Title = input.Title
		observation.Body = input.Body
		observation.AffectedContracts = append([]string(nil), input.AffectedContracts...)
		fingerprintSource := observation.Fingerprint
		if observation.PublicationRole == "canonical" {
			fingerprintSource = observation.RootCauseKey
		}
		observation.RepositoryFingerprint = digest([]byte("owner/repo\x00" + fingerprintSource))
		manifest.Observations = append(manifest.Observations, observation)
	}
	manifest.ReplayProtection.Nonce = bundleID
	manifest.ReplayProtection.Key = replayKey(manifest)
	sealTestManifest(t, manifestPath, &manifest)
	return manifestPath
}

func validateLocalBundle(t *testing.T, manifestPath string) *ValidatedBundle {
	t.Helper()
	manifest := readManifest(t, manifestPath)
	legacyAuthorityFixture := manifest.SchemaVersion == ManifestSchema && manifest.Scan.AuthoritativeForResolution
	for _, observation := range manifest.Observations {
		legacyAuthorityFixture = legacyAuthorityFixture || (manifest.SchemaVersion == ManifestSchema && observation.State == "absent")
	}
	if legacyAuthorityFixture {
		// These fixtures unit-test lifecycle state transitions that predate the
		// v3 ingestion contract. Construct the already-validated test input
		// directly so they do not imply that production v2 ingestion can confer
		// authority; bundle_test.go covers that fail-closed boundary.
		return &ValidatedBundle{Manifest: manifest, Validation: Validation{SchemaVersion: "test.lifecycle-input.v1", Status: "passed", Trusted: true, Authoritative: manifest.Scan.AuthoritativeForResolution}}
	}
	bundle, err := ValidateBundle(manifestPath, ValidationOptions{Now: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), MaxACMM: 6, AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func newTestBeadStore(t *testing.T, dir string) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestLifecycleAuditFailureBlocksRemoteTransitionAndRetryRecovers(t *testing.T) {
	root := t.TempDir()
	lifecycle, err := NewLifecycleStore(filepath.Join(root, "lifecycle"))
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-audit-recovery", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := bundle.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.BindIssueWriter(fingerprint, 42, "hive-writer"); err != nil {
		t.Fatal(err)
	}
	entry := lifecycle.PendingOutbox()[0]
	finding, _ := lifecycle.Finding(fingerprint)
	auditBackup := lifecycle.auditPath + ".backup"
	if err := os.Rename(lifecycle.auditPath, auditBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lifecycle.auditPath, 0o700); err != nil {
		t.Fatal(err)
	}
	client := &fakeLifecycleIssueClient{}
	if err := processOutboxEntry(context.Background(), lifecycle, beadStore, client, finding, entry); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("audit write failure did not block lifecycle publication state: %v", err)
	}
	blocked, _ := lifecycle.Finding(fingerprint)
	if client.upserts != 1 || blocked.IssueNumber != 0 || blocked.Status != StatusDetected || len(lifecycle.PendingOutbox()) != 1 {
		t.Fatalf("remote-success/audit-failure window was not recoverable: finding=%+v pending=%d client=%+v", blocked, len(lifecycle.PendingOutbox()), client)
	}
	if err := os.Remove(lifecycle.auditPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(auditBackup, lifecycle.auditPath); err != nil {
		t.Fatal(err)
	}
	result := ProcessOutbox(context.Background(), lifecycle, beadStore, automation.Policy{ACMMLevel: 4, Mode: automation.ModeIssues, AllowedRepositories: []string{"owner/repo"}}, client)
	recovered, _ := lifecycle.Finding(fingerprint)
	if result.Succeeded != 1 || result.Failed != 0 || client.upserts != 2 || recovered.Status != StatusIssueOpen || recovered.IssueNumber != 17 || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("idempotent outbox retry did not recover audit transition: result=%+v finding=%+v client=%+v", result, recovered, client)
	}
}

func TestLifecycleAuditReceiptRecoversAuditBeforeStateCrashWindow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "lifecycle")
	lifecycle, err := NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	beadStore := newTestBeadStore(t, filepath.Join(root, "beads"))
	bundle := validateLocalBundle(t, writeLifecycleBundle(t, filepath.Join(root, "bundle"), "bundle-audit-receipt", "present", "main", true))
	if _, err := lifecycle.ApplyBundle(bundle, beadStore, ApplyLifecycleOptions{TargetRef: "main"}); err != nil {
		t.Fatal(err)
	}
	fingerprint := bundle.Manifest.Observations[0].RepositoryFingerprint
	if err := lifecycle.BindIssueWriter(fingerprint, 42, "hive-writer"); err != nil {
		t.Fatal(err)
	}
	stateBackup := lifecycle.statePath + ".backup"
	if err := os.Rename(lifecycle.statePath, stateBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lifecycle.statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkIssueOpened(fingerprint, 17, "https://github.test/owner/repo/issues/17"); err == nil {
		t.Fatal("state publication failure was not returned after durable audit receipt")
	}
	rolledBack, _ := lifecycle.Finding(fingerprint)
	if rolledBack.IssueNumber != 0 || rolledBack.Status != StatusDetected {
		t.Fatalf("failed state publication was not rolled back: %+v", rolledBack)
	}
	if err := os.Remove(lifecycle.statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stateBackup, lifecycle.statePath); err != nil {
		t.Fatal(err)
	}
	recoveredStore, err := NewLifecycleStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoveredStore.MarkIssueOpened(fingerprint, 17, "https://github.test/owner/repo/issues/17"); err != nil {
		t.Fatalf("receipt-bound state retry failed: %v", err)
	}
	if count := countLifecycleAuditAction(t, recoveredStore.auditPath, "issue_opened"); count != 1 {
		t.Fatalf("audit-before-state retry appended %d issue-opened receipts; want exactly one", count)
	}
}

func countLifecycleAuditAction(t *testing.T, path, action string) int {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry LifecycleAuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Action == action {
			count++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return count
}
