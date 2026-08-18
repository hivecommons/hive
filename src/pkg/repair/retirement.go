package repair

import (
	"fmt"
	"strings"
	"time"
)

// PullRequestRetirement binds the exact Worker checkpoint retired by the
// Visual Hive lifecycle. The PR identity remains in repair history after
// retirement, but it is no longer reported or resumed as an open PR.
type PullRequestRetirement struct {
	RepositoryFingerprint string
	PRNumber              int
	PRURL                 string
	Branch                string
	CommitSHA             string
}

// ValidatePullRequestRetirement fails closed unless retirement addresses the
// exact active PR checkpoint, or the same retirement was already persisted.
func (s *Store) ValidatePullRequestRetirement(retirement PullRequestRetirement) error {
	if s == nil {
		return fmt.Errorf("repair state store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return s.poisoned
	}
	_, err := s.validatePullRequestRetirementLocked(retirement)
	return err
}

// RetirePullRequestForVerification transitions the exact PR-open Worker
// checkpoint to a fresh-attempt boundary after the repository and lifecycle
// side effects have succeeded. Repeating the exact transition is a no-op.
func (s *Store) RetirePullRequestForVerification(retirement PullRequestRetirement) error {
	if s == nil {
		return fmt.Errorf("repair state store is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned != nil {
		return s.poisoned
	}
	attempt, err := s.validatePullRequestRetirementLocked(retirement)
	if err != nil {
		return err
	}
	if attempt.Stage == StageNoChange && !attempt.LifecyclePROpen {
		return nil
	}

	previous := cloneState(s.data)
	attempt.Stage = StageNoChange
	attempt.LifecyclePROpen = false
	attempt.LastFailureClass = ""
	attempt.LastFailureID = ""
	attempt.LastFailure = ""
	attempt.ResumeStage = ""
	clearRetryTransaction(attempt)
	clearSpecialistRevisionReservation(attempt)
	attempt.UpdatedAt = time.Now().UTC()
	desired := cloneState(s.data)
	if err := validatePersistedRepairState(desired); err != nil {
		s.data = previous
		return fmt.Errorf("validate retired repair worker state: %w", err)
	}
	if err := s.persistLocked(); err != nil {
		if reconcileErr := s.reconcilePersistFailureLocked(err, previous, desired); reconcileErr != nil {
			return reconcileErr
		}
	}
	return nil
}

func (s *Store) validatePullRequestRetirementLocked(retirement PullRequestRetirement) (*Attempt, error) {
	fingerprint := strings.TrimSpace(retirement.RepositoryFingerprint)
	branch := strings.TrimSpace(retirement.Branch)
	commitSHA := strings.ToLower(strings.TrimSpace(retirement.CommitSHA))
	prURL := strings.TrimSpace(retirement.PRURL)
	if fingerprint == "" || retirement.PRNumber <= 0 || prURL == "" ||
		!validHiveRepairBranch(branch) || !validGitCommitSHA(commitSHA) {
		return nil, fmt.Errorf("repair retirement requires an exact finding, PR, branch, URL, and commit")
	}
	attempt := s.data.Attempts[fingerprint]
	if attempt == nil {
		return nil, fmt.Errorf("repair retirement has no exact Worker checkpoint")
	}
	if attempt.RepositoryFingerprint != fingerprint || attempt.PRNumber != retirement.PRNumber ||
		strings.TrimSpace(attempt.PRURL) != prURL || strings.TrimSpace(attempt.Branch) != branch ||
		!strings.EqualFold(strings.TrimSpace(attempt.CommitSHA), commitSHA) {
		return nil, fmt.Errorf("repair retirement does not match the exact Worker PR checkpoint")
	}
	active := attempt.Stage == StagePROpen && attempt.LifecyclePROpen
	retired := attempt.Stage == StageNoChange && !attempt.LifecyclePROpen
	if !active && !retired {
		return nil, fmt.Errorf("repair retirement requires an active or already-retired Worker PR checkpoint")
	}
	return attempt, nil
}
