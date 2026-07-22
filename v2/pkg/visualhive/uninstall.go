package visualhive

import (
	"fmt"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
)

const StatusCancelled LifecycleStatus = "cancelled"

// CancelForUninstall records an explicit operator-requested lifecycle
// cancellation. It does not claim that a defect was fixed. Remote issue/PR
// cleanup must complete before the caller invokes this checkpoint.
func (s *LifecycleStore) CancelForUninstall(repositoryFingerprint string, issueClosed bool, beadStore *beads.Store) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	backup := cloneLifecycleState(s.state)
	finding := s.state.Findings[repositoryFingerprint]
	if finding == nil {
		return fmt.Errorf("finding %s not found", repositoryFingerprint)
	}
	if (finding.IssueNumber > 0) != issueClosed {
		return fmt.Errorf("uninstall cancellation issue-close proof does not match finding %s", repositoryFingerprint)
	}
	now := time.Now().UTC()
	for _, entry := range s.state.Outbox {
		if entry != nil && entry.RepositoryFingerprint == repositoryFingerprint && entry.CompletedAt == nil {
			completed := now
			entry.CompletedAt = &completed
			entry.LastError = "cancelled by explicit Hive uninstall"
		}
	}
	finding.Status = StatusCancelled
	finding.ResolvedAt = &now
	if issueClosed {
		finding.ClosedAt = &now
	}
	finding.HumanReviewRequired, finding.ManualReviewKind, finding.ManualReviewReason = false, "", ""
	if finding.BeadID != "" {
		if beadStore == nil {
			s.state = backup
			return fmt.Errorf("bead store is required to cancel finding %s", repositoryFingerprint)
		}
		if err := beadStore.Close(finding.BeadID); err != nil {
			s.state = backup
			return err
		}
	}
	s.auditLocked(LifecycleAuditEntry{Action: "uninstall_cancelled", Allowed: true, Repository: finding.Repository, RepositoryFingerprint: repositoryFingerprint, BundleID: finding.LastBundleID, Detail: "explicit uninstall closed exact remote resources and cancelled pending lifecycle work"})
	s.state.UpdatedAt = now
	if err := s.persistLocked(); err != nil {
		s.state = backup
		return err
	}
	return nil
}
