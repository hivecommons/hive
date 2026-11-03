package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forgestate"
)

const ExecutionSchemaVersion = 2

func executionObject(value string) bool {
	return fullSHA(value) && value == strings.ToLower(value) && strings.Trim(value, "0") != ""
}

func executionHash(value string) bool {
	return len(value) == sha256.Size*2 && strings.Trim(value, "0123456789abcdef") == "" && strings.Trim(value, "0") != ""
}

// ExecutionRecipe is a planned ordered subject, not a proof or a lease. Its ID
// excludes the observer generation; changing any ordered head/base/policy does
// not borrow a previous candidate's evidence.
type ExecutionRecipe struct {
	Repo         string                                  `json:"repo"`
	Base         string                                  `json:"base"`
	BaseSHA      string                                  `json:"base_sha"`
	Policy       string                                  `json:"policy"`
	WorkflowTree string                                  `json:"workflow_tree"`
	Members      []forgestate.IntegrationCandidateMember `json:"members"`
}

func (r ExecutionRecipe) ID() string {
	data, _ := json.Marshal(r)
	sum := sha256.Sum256(data)
	return "integration-recipe-v2/" + hex.EncodeToString(sum[:])
}

func (r ExecutionRecipe) Validate() error {
	if r.Repo == "" || r.Base == "" || !executionObject(r.BaseSHA) || !executionObject(r.WorkflowTree) ||
		!executionHash(r.Policy) || len(r.Members) == 0 || len(r.Members) > config.MaxIntegrationExecutionCandidates {
		return fmt.Errorf("invalid exact execution recipe")
	}
	members, heads := map[int]bool{}, map[string]bool{}
	for _, member := range r.Members {
		if member.PR < 1 || !executionObject(member.Head) || members[member.PR] || heads[member.Head] {
			return fmt.Errorf("invalid ordered execution member")
		}
		members[member.PR], heads[member.Head] = true, true
	}
	return nil
}

type ExecutionPlan struct {
	Recipe               ExecutionRecipe                          `json:"recipe"`
	Generation           uint64                                   `json:"generation"`
	CreatedAt            time.Time                                `json:"created_at"`
	ObservedAt           time.Time                                `json:"observed_at"`
	State                State                                    `json:"state"`
	Reason               string                                   `json:"reason"`
	Priority             int                                      `json:"priority"`
	WorkKeys             []string                                 `json:"work_keys"`
	ExecutorID           string                                   `json:"executor_id"`
	AuthorityCommit      string                                   `json:"authority_commit"`
	RequestDigest        string                                   `json:"request_digest,omitempty"`
	MaterializerRunID    int64                                    `json:"materializer_run_id,omitempty"`
	MaterializerAttempt  int                                      `json:"materializer_attempt,omitempty"`
	JobCompleteness      string                                   `json:"job_completeness,omitempty"`
	Prepared             []forgestate.IntegrationCandidateSubject `json:"prepared"`
	MutationKey          string                                   `json:"mutation_key,omitempty"`
	Attempt              int                                      `json:"attempt"`
	AttemptAt            time.Time                                `json:"attempt_at"`
	DispatchOutcome      string                                   `json:"dispatch_outcome,omitempty"`
	WorkBindings         []ExecutionWorkBinding                   `json:"work_bindings"`
	ReservationsReleased bool                                     `json:"reservations_released"`
	Materialization      *ExecutionArchive                        `json:"materialization,omitempty"`
}

type ExecutionWorkBinding struct {
	Key              string `json:"key"`
	IntentGeneration uint64 `json:"intent_generation"`
	PlanRevision     int    `json:"plan_revision"`
}

// ExecutionArchive is private durable validation history. The caller must
// populate it only after authoritative cohort, signature and subject validation;
// a JWT or an artifact assertion alone cannot create a Passed observation.
type ExecutionArchive struct {
	Role             string    `json:"role"`
	Issuer           string    `json:"issuer"`
	KeyID            string    `json:"key_id"`
	SigningKeySPKI   []byte    `json:"signing_key_spki"`
	SigningKeySHA256 string    `json:"signing_key_sha256"`
	VerifiedAt       time.Time `json:"verified_at"`
	Payload          []byte    `json:"payload"`
	Token            string    `json:"token"`
	ArtifactID       int64     `json:"artifact_id"`
	ArtifactDigest   string    `json:"artifact_digest"`
	WorkflowID       int64     `json:"workflow_id"`
	SuiteID          int64     `json:"suite_id"`
	CheckIDs         []int64   `json:"check_ids"`
	FinalizerCheckID int64     `json:"finalizer_check_id"`
	RepositoryID     int64     `json:"repository_id"`
	ActorID          int64     `json:"actor_id"`
	WorkflowSHA      string    `json:"workflow_sha"`
	WorkflowPath     string    `json:"workflow_path"`
	Ref              string    `json:"ref"`
	Event            string    `json:"event"`
	JobStartedAt     time.Time `json:"job_started_at"`
	JobCompletedAt   time.Time `json:"job_completed_at"`
}

type ExecutionProof struct {
	Subject             forgestate.IntegrationCandidateSubject `json:"subject"`
	State               State                                  `json:"state"`
	ObservedState       State                                  `json:"observed_state"`
	ObservedAt          time.Time                              `json:"observed_at"`
	Source              string                                 `json:"source"`
	RunID               int64                                  `json:"run_id"`
	Attempt             int                                    `json:"attempt"`
	Archive             *ExecutionArchive                      `json:"archive,omitempty"`
	EligibilityRevision uint64                                 `json:"eligibility_revision"`
	InvalidatedBy       string                                 `json:"invalidated_by,omitempty"`
	Reason              string                                 `json:"reason"`
	JobCompleteness     string                                 `json:"job_completeness,omitempty"`
}

func (p ExecutionProof) Key() string {
	return fmt.Sprintf("%s/%d/%d/%s", strings.ToLower(p.Subject.Repo), p.RunID, p.Attempt, p.Subject.ID)
}

type ExecutionInvalidation struct {
	Repo     string `json:"repo"`
	Base     string `json:"base"`
	PR       int    `json:"pr,omitempty"`
	Head     string `json:"head,omitempty"`
	Policy   string `json:"policy,omitempty"`
	Revision uint64 `json:"revision"`
	Reason   string `json:"reason"`
}

type ExecutionPromotion struct {
	ProviderAcknowledged bool      `json:"provider_acknowledged"`
	Repo                 string    `json:"repo"`
	Base                 string    `json:"base"`
	SubjectID            string    `json:"subject_id"`
	ExpectedSHA          string    `json:"expected_sha"`
	TestedSHA            string    `json:"tested_sha"`
	AuthorityCommit      string    `json:"authority_commit"`
	MutationKey          string    `json:"mutation_key"`
	State                string    `json:"state"`
	Reason               string    `json:"reason"`
	PR                   int       `json:"pr"`
	Head                 string    `json:"head"`
	Attempt              int       `json:"attempt"`
	ObservedAt           time.Time `json:"observed_at"`
}

type ExecutionStatus struct {
	Repo            string    `json:"repo"`
	Base            string    `json:"base"`
	ExecutorID      string    `json:"executor_id"`
	DesiredMode     string    `json:"desired_mode"`
	EffectiveWriter string    `json:"effective_writer"`
	Reason          string    `json:"reason"`
	SourceVersion   string    `json:"source_version"`
	RuntimeVersion  string    `json:"runtime_version"`
	ObservedAt      time.Time `json:"observed_at"`
}

// ExecutionEligibility records author/lane admission separately from combined
// CI provenance. It expires as scheduling input and never authorizes promotion.
type ExecutionEligibility struct {
	Repo                  string    `json:"repo"`
	Base                  string    `json:"base"`
	PR                    int       `json:"pr"`
	Head                  string    `json:"head"`
	WorkKey               string    `json:"work_key,omitempty"`
	IntentGeneration      uint64    `json:"intent_generation"`
	PlanRevision          int       `json:"plan_revision"`
	ScopeKnown            bool      `json:"scope_known"`
	DependenciesKnown     bool      `json:"dependencies_known"`
	DependenciesSatisfied bool      `json:"dependencies_satisfied"`
	ReadyAt               time.Time `json:"ready_at"`
	ObservedAt            time.Time `json:"observed_at"`
	Source                string    `json:"source"`
}

type ExecutionQualification struct {
	ID               string    `json:"id"`
	Repo             string    `json:"repo"`
	Base             string    `json:"base"`
	ProbeBase        string    `json:"probe_base"`
	ExecutorID       string    `json:"executor_id"`
	AuthorityCommit  string    `json:"authority_commit"`
	RequestActorID   int64     `json:"request_actor_id"`
	Policy           string    `json:"policy"`
	WorkflowTree     string    `json:"workflow_tree"`
	ProtectionDigest string    `json:"protection_digest"`
	ProfileDigest    string    `json:"profile_digest"`
	SubjectID        string    `json:"subject_id"`
	TestedSHA        string    `json:"tested_sha"`
	MutationKey      string    `json:"mutation_key"`
	ObservedAt       time.Time `json:"observed_at"`
}

func (e ExecutionEligibility) Key() string {
	return fmt.Sprintf("%s/%d", scopeKey(e.Repo, e.Base), e.PR)
}

// ExecutionEvent uses a separately opened execution-v2 journal. It never writes
// these subjects into convergence's single-PR BaseSHA proof or the v1 partition.
type ExecutionEvent struct {
	Version       int                     `json:"version"`
	Plan          *ExecutionPlan          `json:"plan,omitempty"`
	Proof         *ExecutionProof         `json:"proof,omitempty"`
	Invalidate    *ExecutionInvalidation  `json:"invalidate,omitempty"`
	Promotion     *ExecutionPromotion     `json:"promotion,omitempty"`
	Status        *ExecutionStatus        `json:"status,omitempty"`
	Eligibility   *ExecutionEligibility   `json:"eligibility,omitempty"`
	Qualification *ExecutionQualification `json:"qualification,omitempty"`
}

type ExecutionSnapshot struct {
	Version        int                      `json:"version"`
	Plans          []ExecutionPlan          `json:"plans"`
	Proofs         []ExecutionProof         `json:"proofs"`
	Invalidations  []ExecutionInvalidation  `json:"invalidations"`
	Promotions     []ExecutionPromotion     `json:"promotions"`
	Statuses       []ExecutionStatus        `json:"statuses"`
	Eligibility    []ExecutionEligibility   `json:"eligibility"`
	Qualifications []ExecutionQualification `json:"qualifications"`
}

type executionState struct {
	plans          map[string]ExecutionPlan
	proofs         map[string]ExecutionProof
	invalidations  map[string]ExecutionInvalidation
	promotions     map[string]ExecutionPromotion
	statuses       map[string]ExecutionStatus
	eligibility    map[string]ExecutionEligibility
	qualifications map[string]ExecutionQualification
}
