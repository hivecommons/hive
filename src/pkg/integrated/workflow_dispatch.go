package integrated

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	WorkflowDispatchSchema = "hive.workflow-dispatch.v1"
	workflowDispatchFile   = "workflow-dispatch.json"
	workflowDispatchInput  = "hive_dispatch_id"
	workflowDispatchPrefix = "Hive Visual Hive Production"
)

var workflowDispatchCorrelationPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// WorkflowDispatchIntent is persisted before the workflow_dispatch mutation.
// GitHub's current API returns a run ID, while older servers do not; the opaque
// correlation is the durable bridge across a crash before that response is
// persisted and the exact fallback for older servers. The run ID is persisted
// before Hive waits, allowing a restarted process to resume without redispatching.
type WorkflowDispatchIntent struct {
	SchemaVersion               string    `json:"schema_version"`
	Repository                  string    `json:"repository"`
	RepositoryID                string    `json:"repository_id"`
	WorkflowFile                string    `json:"workflow_file"`
	Ref                         string    `json:"ref"`
	Operation                   string    `json:"operation,omitempty"`
	CorrelationID               string    `json:"correlation_id"`
	ExpectedDisplayTitle        string    `json:"expected_display_title"`
	PreparedAt                  time.Time `json:"prepared_at"`
	DispatchAttemptedAt         time.Time `json:"dispatch_attempted_at,omitempty"`
	RequestDigest               string    `json:"request_digest,omitempty"`
	DispatchAcknowledgedAt      time.Time `json:"dispatch_acknowledged_at,omitempty"`
	RunID                       int64     `json:"run_id,omitempty"`
	RunURL                      string    `json:"run_url,omitempty"`
	MatchedAt                   time.Time `json:"matched_at,omitempty"`
	RecoveryAction              string    `json:"recovery_action,omitempty"`
	RecoveryRequestDigest       string    `json:"recovery_request_digest,omitempty"`
	RecoveryPlanDigest          string    `json:"recovery_plan_digest,omitempty"`
	RecoveryActor               string    `json:"recovery_actor,omitempty"`
	RecoveryReason              string    `json:"recovery_reason,omitempty"`
	RecoveryAuthorizedAt        time.Time `json:"recovery_authorized_at,omitempty"`
	RecoveryOriginalAttemptedAt time.Time `json:"recovery_original_attempted_at,omitempty"`
	RecoveryCount               int       `json:"recovery_count,omitempty"`
}

func newWorkflowDispatchIntent(config Config, workflowFile, ref string) (WorkflowDispatchIntent, error) {
	return newWorkflowDispatchIntentForOperation(config, workflowFile, ref, "production", "", time.Time{})
}

func newWorkflowDispatchIntentForOperation(config Config, workflowFile, ref, operation, correlation string, preparedAt time.Time) (WorkflowDispatchIntent, error) {
	buffer := make([]byte, 32)
	if strings.TrimSpace(correlation) == "" {
		if _, err := rand.Read(buffer); err != nil {
			return WorkflowDispatchIntent{}, fmt.Errorf("generate workflow dispatch correlation: %w", err)
		}
		correlation = hex.EncodeToString(buffer)
	}
	if preparedAt.IsZero() {
		preparedAt = time.Now().UTC()
	}
	correlation = strings.ToLower(strings.TrimSpace(correlation))
	intent := WorkflowDispatchIntent{
		SchemaVersion:        WorkflowDispatchSchema,
		Repository:           strings.TrimSpace(config.Repository),
		RepositoryID:         strings.TrimSpace(config.RepositoryID),
		WorkflowFile:         strings.TrimSpace(workflowFile),
		Ref:                  strings.TrimSpace(ref),
		Operation:            strings.TrimSpace(operation),
		CorrelationID:        correlation,
		ExpectedDisplayTitle: workflowDispatchDisplayTitle(correlation),
		PreparedAt:           preparedAt.UTC(),
	}
	if err := validateWorkflowDispatchIntent(intent); err != nil {
		return WorkflowDispatchIntent{}, err
	}
	return intent, nil
}

func workflowDispatchDisplayTitle(correlation string) string {
	return workflowDispatchPrefix + " [" + correlation + "]"
}

func (s *Store) SaveWorkflowDispatchIntent(intent WorkflowDispatchIntent) error {
	intent.SchemaVersion = WorkflowDispatchSchema
	if strings.TrimSpace(intent.Operation) == "" {
		intent.Operation = "production"
	}
	if !intent.DispatchAttemptedAt.IsZero() && intent.RequestDigest == "" {
		digest, err := workflowDispatchRequestDigest(intent)
		if err != nil {
			return err
		}
		intent.RequestDigest = digest
	}
	if err := validateWorkflowDispatchIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, workflowDispatchFile), append(data, '\n'))
}

func (s *Store) LoadWorkflowDispatchIntent() (WorkflowDispatchIntent, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, workflowDispatchFile))
	if os.IsNotExist(err) {
		return WorkflowDispatchIntent{}, false, nil
	}
	if err != nil {
		return WorkflowDispatchIntent{}, false, err
	}
	var intent WorkflowDispatchIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return WorkflowDispatchIntent{}, false, fmt.Errorf("decode workflow dispatch intent: %w", err)
	}
	if strings.TrimSpace(intent.Operation) == "" {
		intent.Operation = "production"
	}
	if err := validateWorkflowDispatchIntent(intent); err != nil {
		return WorkflowDispatchIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteWorkflowDispatchIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, workflowDispatchFile))
}

func validateWorkflowDispatchIntent(intent WorkflowDispatchIntent) error {
	if intent.SchemaVersion != WorkflowDispatchSchema || strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" ||
		strings.TrimSpace(intent.WorkflowFile) == "" || strings.TrimSpace(intent.Ref) == "" || (intent.Operation != "production" && intent.Operation != setupBaselineWorkflowOperation) || !workflowDispatchCorrelationPattern.MatchString(intent.CorrelationID) ||
		intent.ExpectedDisplayTitle != workflowDispatchDisplayTitle(intent.CorrelationID) || intent.PreparedAt.IsZero() {
		return fmt.Errorf("workflow dispatch intent is incomplete or invalid")
	}
	if !intent.DispatchAcknowledgedAt.IsZero() && intent.DispatchAttemptedAt.IsZero() {
		return fmt.Errorf("workflow dispatch intent acknowledgement has no attempted mutation")
	}
	if intent.RunID < 0 || (intent.RunID == 0 && (!intent.MatchedAt.IsZero() || intent.RunURL != "")) ||
		(intent.RunID > 0 && (intent.DispatchAttemptedAt.IsZero() || intent.MatchedAt.IsZero())) {
		return fmt.Errorf("workflow dispatch intent has an invalid run binding")
	}
	if intent.DispatchAttemptedAt.IsZero() {
		if intent.RequestDigest != "" || !intent.DispatchAcknowledgedAt.IsZero() || intent.RunID != 0 {
			return fmt.Errorf("workflow dispatch intent has request state without an attempted mutation")
		}
	} else {
		digest, err := workflowDispatchRequestDigest(intent)
		if err != nil || intent.RequestDigest != digest {
			return fmt.Errorf("workflow dispatch intent request digest is missing or invalid")
		}
	}
	recoveryPresent := intent.RecoveryAction != "" || intent.RecoveryRequestDigest != "" || intent.RecoveryPlanDigest != "" || intent.RecoveryActor != "" || intent.RecoveryReason != "" || !intent.RecoveryAuthorizedAt.IsZero() || !intent.RecoveryOriginalAttemptedAt.IsZero() || intent.RecoveryCount != 0
	if recoveryPresent {
		if intent.RecoveryAction != string(WorkflowDispatchRecoveryRetry) || len(intent.RecoveryRequestDigest) != sha256.Size*2 || len(intent.RecoveryPlanDigest) != sha256.Size*2 ||
			strings.TrimSpace(intent.RecoveryActor) == "" || strings.TrimSpace(intent.RecoveryReason) == "" || intent.RecoveryAuthorizedAt.IsZero() || intent.RecoveryOriginalAttemptedAt.IsZero() || intent.RecoveryCount <= 0 {
			return fmt.Errorf("workflow dispatch recovery authorization is incomplete or invalid")
		}
		if _, err := hex.DecodeString(intent.RecoveryRequestDigest); err != nil {
			return fmt.Errorf("workflow dispatch recovery request digest is invalid")
		}
		if _, err := hex.DecodeString(intent.RecoveryPlanDigest); err != nil {
			return fmt.Errorf("workflow dispatch recovery plan digest is invalid")
		}
	}
	return nil
}

func workflowDispatchRequestDigest(intent WorkflowDispatchIntent) (string, error) {
	record := struct {
		SchemaVersion        string    `json:"schema_version"`
		Repository           string    `json:"repository"`
		RepositoryID         string    `json:"repository_id"`
		WorkflowFile         string    `json:"workflow_file"`
		Ref                  string    `json:"ref"`
		Operation            string    `json:"operation,omitempty"`
		CorrelationID        string    `json:"correlation_id"`
		ExpectedDisplayTitle string    `json:"expected_display_title"`
		PreparedAt           time.Time `json:"prepared_at"`
		AttemptedAt          time.Time `json:"attempted_at"`
	}{
		SchemaVersion: intent.SchemaVersion, Repository: intent.Repository, RepositoryID: intent.RepositoryID,
		WorkflowFile: intent.WorkflowFile, Ref: intent.Ref, Operation: dispatchDigestOperation(intent.Operation), CorrelationID: intent.CorrelationID,
		ExpectedDisplayTitle: intent.ExpectedDisplayTitle, PreparedAt: intent.PreparedAt, AttemptedAt: intent.DispatchAttemptedAt,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func WorkflowDispatchNeedsRecovery(intent WorkflowDispatchIntent) bool {
	return !intent.DispatchAttemptedAt.IsZero() && intent.DispatchAcknowledgedAt.IsZero() && intent.RunID == 0
}

func dispatchDigestOperation(operation string) string {
	if strings.TrimSpace(operation) == "" || operation == "production" {
		return ""
	}
	return strings.TrimSpace(operation)
}

func validateWorkflowDispatchBinding(intent WorkflowDispatchIntent, config Config, workflowFile, ref string) error {
	return validateWorkflowDispatchOperationBinding(intent, config, workflowFile, ref, "production")
}

func validateWorkflowDispatchOperationBinding(intent WorkflowDispatchIntent, config Config, workflowFile, ref, operation string) error {
	if intent.Repository != strings.TrimSpace(config.Repository) || intent.RepositoryID != strings.TrimSpace(config.RepositoryID) ||
		intent.WorkflowFile != strings.TrimSpace(workflowFile) || intent.Ref != strings.TrimSpace(ref) || intent.Operation != strings.TrimSpace(operation) {
		return fmt.Errorf("durable workflow dispatch intent is bound to a different repository, workflow, ref, or operation")
	}
	return nil
}

func consumeWorkflowDispatch(stateDir string, workflow WorkflowRunEvidence) error {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return err
	}
	if !exists || workflow.RunID <= 0 || workflow.CorrelationID == "" || intent.RunID != workflow.RunID || intent.CorrelationID != workflow.CorrelationID {
		return fmt.Errorf("cannot consume workflow evidence without its exact durable dispatch intent")
	}
	return store.DeleteWorkflowDispatchIntent()
}
