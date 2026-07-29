package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	trustedWorkflowName    = "Hive Visual Hive Production"
	trustedWorkflowPath    = ".github/workflows/hive-visual-hive.yml"
	dispatchDeferralSchema = "hive.visual-dispatch-deferral.v1"
	dispatchDeferralReason = "one-PR acceptance loop selected another launchable finding; retry only from a later verified packet"
)

type visualSpecialistEvidenceSource interface {
	SpecialistEvidenceIdentity(visualhive.FindingLifecycle) (agent.SpecialistEvidenceIdentity, error)
	VerifiedEvidenceRoot() (string, error)
}

type RoleState interface {
	IsPaused(string) bool
}

type AuditEvent struct {
	EventID               string
	Stage                 string
	SourceExternalRef     string
	RepositoryFingerprint string
	Decision              *governor.WorkAdmissionDecision
	Detail                string
}

type AuditSink interface {
	RecordVisualWorkAudit(context.Context, AuditEvent) error
}

// RuntimeConfigLoader returns the authoritative installed Visual Hive
// contract together with the live normal Hive ACMM level. It is invoked while
// the controller lock is held immediately before intake and reservation.
type RuntimeConfigLoader func() (integrated.Config, int, error)

type Controller struct {
	mu              sync.Mutex
	governor        *governor.Governor
	lifecycle       *visualhive.LifecycleStore
	beadStores      map[string]*beads.Store
	roles           RoleState
	issueClient     visualhive.LifecycleIssueClient
	installed       integrated.Config
	normalACMM      int
	baseTree        func(context.Context, integrated.Config, string) (string, error)
	recordAdmission func(*beads.Store, string, string, governor.WorkAdmissionDecision) error
	markStage       func(*beads.Store, string, string, string) error
	persistDispatch func(*beads.Store, string, DispatchEnvelope) error
	loadPlan        func(hivegithub.VerifiedVisualHiveArtifact) (visualhive.VerifiedImportPlan, error)
	now             func() time.Time
	audit           AuditSink
	runtimeConfig   RuntimeConfigLoader
}

func (controller *Controller) SetAuditSink(sink AuditSink) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.audit = sink
}

func (controller *Controller) SetRuntimeConfigLoader(loader RuntimeConfigLoader) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.runtimeConfig = loader
}

// ReserveSpecialistWorkOrderIdentity lets the later normal scheduler adapter
// bind its deterministic existing swo-* request exactly once before launch.
// It does not dispatch or create another lifecycle record.
func (controller *Controller) ReserveSpecialistWorkOrderIdentity(sourceExternalRef, workOrderID, requestSHA256 string) (DispatchEnvelope, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := controller.refreshRuntimeConfigLocked(context.Background()); err != nil {
		return DispatchEnvelope{}, err
	}
	requestSHA256 = strings.ToLower(strings.TrimSpace(requestSHA256))
	workOrderID = strings.TrimSpace(workOrderID)
	if !validSHA256(requestSHA256) || workOrderID != "swo-"+requestSHA256 {
		return DispatchEnvelope{}, errors.New("deterministic swo-* ID and canonical specialist request digest are required")
	}
	var store *beads.Store
	var bead *beads.Bead
	for _, role := range sortedStoreRoles(controller.beadStores) {
		if candidate := controller.beadStores[role].FindByExternalRef(sourceExternalRef); candidate != nil {
			if bead != nil {
				return DispatchEnvelope{}, errors.New("source external ref exists in multiple role stores")
			}
			store, bead = controller.beadStores[role], candidate
		}
	}
	if bead == nil || visualBeadAdmissionState(bead) != "admitted_dispatch_pending" {
		return DispatchEnvelope{}, errors.New("durable dispatch-pending intent is unavailable")
	}
	envelope, ok := visualDispatchEnvelope(bead)
	if !ok {
		return DispatchEnvelope{}, errors.New("durable dispatch-pending intent is corrupt")
	}
	decision, recorded := visualBeadAdmissionDecision(bead)
	finding, exists := controller.lifecycle.Finding(envelope.Work.RepositoryFingerprint)
	if !recorded || !exists || validateVisualDispatchEnvelope(envelope, bead.ID, envelope.Work.Packet, envelope.Work, finding, envelope.Evidence, envelope.EvidenceRoot, decision) != nil {
		return DispatchEnvelope{}, errors.New("durable dispatch-pending intent failed immutable reconcile")
	}
	if err := controller.dispatchLaunchAllowed(envelope, finding); err != nil {
		holdStage := dispatchHoldStage(finding)
		if holdErr := controller.markStage(store, bead.ID, holdStage, err.Error()); holdErr != nil {
			return DispatchEnvelope{}, fmt.Errorf("durable dispatch-pending intent is not currently launchable: %v; persist hold: %w", err, holdErr)
		}
		_ = controller.recordAudit(context.Background(), AuditEvent{Stage: holdStage, SourceExternalRef: sourceExternalRef, RepositoryFingerprint: finding.RepositoryFingerprint, Decision: &decision, Detail: err.Error()})
		return DispatchEnvelope{}, fmt.Errorf("durable dispatch-pending intent is not currently launchable: %w", err)
	}
	if envelope.SpecialistWorkOrderID != "" || envelope.SpecialistRequestSHA256 != "" {
		if envelope.SpecialistWorkOrderID == workOrderID && envelope.SpecialistRequestSHA256 == strings.ToLower(requestSHA256) {
			return envelope, nil
		}
		return DispatchEnvelope{}, errors.New("specialist work-order identity was already reserved with different bytes")
	}
	envelope.SpecialistWorkOrderID = workOrderID
	envelope.SpecialistRequestSHA256 = requestSHA256
	if err := persistVisualDispatchEnvelope(store, bead.ID, envelope); err != nil {
		return DispatchEnvelope{}, err
	}
	persisted, ok := visualDispatchEnvelope(store.FindByExternalRef(sourceExternalRef))
	if !ok || !reflect.DeepEqual(persisted, envelope) {
		return DispatchEnvelope{}, errors.New("specialist work-order reservation was not durably persisted")
	}
	return persisted, nil
}

// RevalidateSpecialistBoundary rechecks the intake-owned immutable dispatch
// and all current Governor/role/budget/pause/installed policy before each new
// repair side effect. Once Worker has durably started the exact repair, its
// lifecycle status may advance; that advancement never weakens the current
// policy checks or permits a different recurrence/order.
func (controller *Controller) RevalidateSpecialistBoundary(sourceExternalRef, workOrderID, requestSHA256 string) (DispatchEnvelope, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := controller.refreshRuntimeConfigLocked(context.Background()); err != nil {
		return DispatchEnvelope{}, err
	}
	store, bead, envelope, decision, err := controller.findDispatchLocked(sourceExternalRef)
	if err != nil {
		return DispatchEnvelope{}, err
	}
	_ = store
	if workOrderID != "" || requestSHA256 != "" {
		requestSHA256 = strings.ToLower(strings.TrimSpace(requestSHA256))
		if workOrderID != envelope.SpecialistWorkOrderID || requestSHA256 != envelope.SpecialistRequestSHA256 || workOrderID != "swo-"+requestSHA256 {
			return DispatchEnvelope{}, errors.New("specialist boundary does not match the one reserved swo-* identity")
		}
	}
	current, exists := controller.lifecycle.Finding(envelope.Work.RepositoryFingerprint)
	if !exists || current.RepositoryFingerprint != envelope.Finding.RepositoryFingerprint || current.IssueNumber != envelope.Finding.IssueNumber || current.Recurrences != int(envelope.Recurrence) {
		return DispatchEnvelope{}, errors.New("specialist boundary no longer matches the lifecycle finding")
	}
	if err := validateVisualDispatchEnvelope(envelope, bead.ID, envelope.Work.Packet, envelope.Work, envelope.Finding, envelope.Evidence, envelope.EvidenceRoot, decision); err != nil {
		return DispatchEnvelope{}, errors.New("durable dispatch intent failed immutable boundary reconcile")
	}
	launchFinding := envelope.Finding
	launchFinding.HumanReviewRequired = current.HumanReviewRequired
	launchFinding.ObservationHumanReviewRequired = current.ObservationHumanReviewRequired
	launchFinding.ManualReviewKind = current.ManualReviewKind
	launchFinding.ManualReviewReason = current.ManualReviewReason
	if err := controller.dispatchLaunchAllowed(envelope, launchFinding); err != nil {
		return DispatchEnvelope{}, err
	}
	return envelope, nil
}

type dispatchDeferral struct {
	SchemaVersion             string `json:"schema_version"`
	SourceExternalRef         string `json:"source_external_ref"`
	SelectedSourceExternalRef string `json:"selected_source_external_ref"`
	WorkflowCorrelationSHA256 string `json:"workflow_correlation_sha256"`
	PacketDigest              string `json:"packet_digest"`
	Reason                    string `json:"reason"`
}

// DeferUnselectedSpecialistDispatches durably retains every launchable peer
// that the one-PR normal-service acceptance loop did not select. The existing
// role bead remains the sole work record; this method creates no queue, model
// invocation, lifecycle transition, or GitHub side effect. Replaying the exact
// selection is idempotent, while a later verified packet may re-admit the bead.
func (controller *Controller) DeferUnselectedSpecialistDispatches(
	_ context.Context,
	selectedSourceExternalRef string,
	deferredSourceExternalRefs []string,
	workflowCorrelationSHA256 string,
) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	selectedSourceExternalRef = strings.TrimSpace(selectedSourceExternalRef)
	workflowCorrelationSHA256 = strings.ToLower(strings.TrimSpace(workflowCorrelationSHA256))
	if selectedSourceExternalRef == "" || !validSHA256(workflowCorrelationSHA256) {
		return errors.New("dispatch deferral requires one selected source ref and exact workflow correlation")
	}
	_, selectedBead, err := controller.findVisualBeadLocked(selectedSourceExternalRef)
	if err != nil || visualBeadAdmissionState(selectedBead) != "admitted_dispatch_pending" {
		return errors.New("selected dispatch-pending intent is unavailable")
	}
	selectedEnvelope, ok := visualDispatchEnvelope(selectedBead)
	if !ok || selectedEnvelope.SourceExternalRef != selectedSourceExternalRef || !validSHA256(selectedEnvelope.Work.Packet.PacketDigest) || selectedEnvelope.SpecialistWorkOrderID != "" {
		return errors.New("selected dispatch-pending intent is corrupt")
	}
	if err := controller.validateDeferrableEnvelopeLocked(selectedBead, selectedEnvelope); err != nil {
		return fmt.Errorf("selected dispatch-pending intent failed immutable reconcile: %w", err)
	}

	refs := append([]string(nil), deferredSourceExternalRefs...)
	for index := range refs {
		refs[index] = strings.TrimSpace(refs[index])
	}
	sort.Strings(refs)
	type target struct {
		store   *beads.Store
		bead    *beads.Bead
		record  dispatchDeferral
		already bool
	}
	targets := make([]target, 0, len(refs))
	for index, ref := range refs {
		if ref == "" || ref == selectedSourceExternalRef || (index > 0 && ref == refs[index-1]) {
			return errors.New("deferred dispatch source refs must be unique and exclude the selected intent")
		}
		store, bead, findErr := controller.findVisualBeadLocked(ref)
		if findErr != nil {
			return findErr
		}
		state := visualBeadAdmissionState(bead)
		if state != "admitted_dispatch_pending" && state != "admitted_dispatch_deferred" {
			return fmt.Errorf("deferred dispatch %s is not a launchable or exactly deferred intent", ref)
		}
		envelope, envelopeOK := visualDispatchEnvelope(bead)
		if !envelopeOK || envelope.SourceExternalRef != ref || envelope.Work.Packet.PacketDigest != selectedEnvelope.Work.Packet.PacketDigest ||
			!strings.EqualFold(envelope.Work.Packet.Repository, selectedEnvelope.Work.Packet.Repository) || envelope.SpecialistWorkOrderID != "" {
			return fmt.Errorf("deferred dispatch %s does not match the selected packet or is already reserved", ref)
		}
		if err := controller.validateDeferrableEnvelopeLocked(bead, envelope); err != nil {
			return fmt.Errorf("deferred dispatch %s failed immutable reconcile: %w", ref, err)
		}
		record := dispatchDeferral{
			SchemaVersion: dispatchDeferralSchema, SourceExternalRef: ref, SelectedSourceExternalRef: selectedSourceExternalRef,
			WorkflowCorrelationSHA256: workflowCorrelationSHA256, PacketDigest: selectedEnvelope.Work.Packet.PacketDigest, Reason: dispatchDeferralReason,
		}
		already := false
		if state == "admitted_dispatch_deferred" {
			existing, exists := visualDispatchDeferral(bead)
			if !exists || existing != record {
				return fmt.Errorf("deferred dispatch %s is already bound to different selection bytes", ref)
			}
			already = true
		}
		targets = append(targets, target{store: store, bead: bead, record: record, already: already})
	}
	for _, target := range targets {
		if target.already {
			continue
		}
		if err := persistVisualDispatchDeferral(target.store, target.bead.ID, target.record); err != nil {
			return err
		}
		persisted := target.store.FindByExternalRef(target.record.SourceExternalRef)
		record, exists := visualDispatchDeferral(persisted)
		if visualBeadAdmissionState(persisted) != "admitted_dispatch_deferred" || !exists || record != target.record {
			return errors.New("unselected dispatch deferral was not durably persisted")
		}
	}
	return nil
}

func (controller *Controller) validateDeferrableEnvelopeLocked(bead *beads.Bead, envelope DispatchEnvelope) error {
	decision, recorded := visualBeadAdmissionDecision(bead)
	finding, exists := controller.lifecycle.Finding(envelope.Work.RepositoryFingerprint)
	if !recorded || !exists {
		return errors.New("durable Governor decision or lifecycle finding is unavailable")
	}
	return validateVisualDispatchEnvelope(
		envelope, bead.ID, envelope.Work.Packet, envelope.Work, finding,
		envelope.Evidence, envelope.EvidenceRoot, decision,
	)
}

// BuildSchedulerAdmittedWork is the sole lossless projection from the native
// intake envelope into Scheduler composition. Callers must obtain the envelope
// from Import/RevalidateSpecialistBoundary and reserve the resulting swo-* via
// ReserveSpecialistWorkOrderIdentity before launch.
func BuildSchedulerAdmittedWork(envelope DispatchEnvelope) (scheduler.AdmittedWork, scheduler.CanonicalVerifiedReceipt, error) {
	workJSON, err := json.Marshal(envelope.Work)
	if err != nil {
		return scheduler.AdmittedWork{}, nil, err
	}
	packetJSON, err := json.Marshal(envelope.Work.Packet)
	if err != nil {
		return scheduler.AdmittedWork{}, nil, err
	}
	findingJSON, err := json.Marshal(envelope.Finding)
	if err != nil {
		return scheduler.AdmittedWork{}, nil, err
	}
	if !json.Valid([]byte(envelope.VerificationReceiptJSON)) {
		return scheduler.AdmittedWork{}, nil, errors.New("dispatch envelope has no canonical verification receipt")
	}
	expectedReceipt, err := evidenceReceiptJSON(envelope.Evidence)
	if err != nil || expectedReceipt != envelope.VerificationReceiptJSON {
		return scheduler.AdmittedWork{}, nil, errors.New("dispatch envelope verification receipt differs from its exact evidence identity")
	}
	artifacts := make([]scheduler.EvidenceArtifact, 0, len(envelope.Work.EvidenceArtifacts))
	for _, artifact := range envelope.Work.EvidenceArtifacts {
		artifacts = append(artifacts, scheduler.EvidenceArtifact{
			Reference: artifact.Path, SHA256: artifact.SHA256, Bytes: artifact.Bytes, Kind: artifact.Kind, ContentType: artifact.ContentType,
		})
	}
	digest := func(value []byte) string {
		hash := sha256.Sum256(value)
		return hex.EncodeToString(hash[:])
	}
	admitted := scheduler.AdmittedWork{
		ExternalRef: envelope.SourceExternalRef, Work: workJSON, WorkSHA256: digest(workJSON), Packet: packetJSON, PacketSHA256: digest(packetJSON),
		Finding: findingJSON, FindingSHA256: digest(findingJSON), Repository: envelope.Work.Packet.Repository,
		RepositoryFingerprint: envelope.Work.RepositoryFingerprint, RecurrenceKey: fmt.Sprintf("%s:r%d", envelope.Work.RepositoryFingerprint, envelope.Recurrence),
		Attempt: envelope.Attempt, BaseSHA: envelope.BaseSHA, BaseTreeSHA: envelope.BaseTreeSHA,
		RoutedRole: agent.SpecialistRole(envelope.Work.Role), RoutingReason: envelope.Work.RoutingReason, ObservationKind: envelope.Work.IssueKind,
		AllowedPaths: append([]string(nil), envelope.AllowedRepairPaths...), Validation: append([]string(nil), envelope.ValidationCommands...),
		ValidationMayReproduce: reflect.DeepEqual(envelope.Reproduction, envelope.ValidationCommands),
		AffectedContracts:      append([]string(nil), envelope.Work.AffectedContracts...), EvidenceArtifacts: artifacts, Deadline: envelope.CompositionDeadline.UTC(),
	}
	receipt := &dispatchCanonicalReceipt{evidence: envelope.Evidence, receipt: json.RawMessage(envelope.VerificationReceiptJSON)}
	return admitted, receipt, nil
}

type dispatchCanonicalReceipt struct {
	evidence agent.SpecialistEvidenceIdentity
	receipt  json.RawMessage
}

func (receipt *dispatchCanonicalReceipt) EvidenceIdentity() agent.SpecialistEvidenceIdentity {
	return receipt.evidence
}

func (receipt *dispatchCanonicalReceipt) CanonicalReceiptJSON() (json.RawMessage, error) {
	if receipt == nil || !json.Valid(receipt.receipt) {
		return nil, errors.New("canonical dispatch receipt is unavailable")
	}
	return append(json.RawMessage(nil), receipt.receipt...), nil
}

type SpecialistPullRequestCompletion struct {
	WorkOrderID          string `json:"work_order_id"`
	RequestSHA256        string `json:"request_sha256"`
	Branch               string `json:"branch"`
	CommitSHA            string `json:"commit_sha"`
	PullRequestNumber    int    `json:"pull_request_number"`
	PullRequestURL       string `json:"pull_request_url"`
	VerdictHeadSHA       string `json:"verdict_head_sha"`
	VerdictReceiptSHA256 string `json:"verdict_receipt_sha256"`
	VerdictStatus        string `json:"verdict_status"`
}

// SpecialistPullRequestRetirement is the exact durable identity of one red,
// unmerged Worker proposal that Hive retired after the default branch changed
// independently. It closes only the specialist work-order projection; it does
// not resolve the finding or close its issue.
type SpecialistPullRequestRetirement struct {
	WorkOrderID           string `json:"work_order_id"`
	RequestSHA256         string `json:"request_sha256"`
	Branch                string `json:"branch"`
	CommitSHA             string `json:"commit_sha"`
	BaseBranch            string `json:"base_branch"`
	BaseSHA               string `json:"base_sha"`
	CurrentDefaultHeadSHA string `json:"current_default_head_sha"`
	PullRequestNumber     int    `json:"pull_request_number"`
	PullRequestURL        string `json:"pull_request_url"`
	VerdictReceiptSHA256  string `json:"verdict_receipt_sha256"`
	RetirementReasonCode  string `json:"retirement_reason_code"`
}

// CompleteSpecialistPullRequest records only that the exact Worker-owned PR
// received an exact-head deterministic receipt. It grants no merge, baseline,
// resolution, or lifecycle-transition authority.
func (controller *Controller) CompleteSpecialistPullRequest(sourceExternalRef string, completion SpecialistPullRequestCompletion) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	completion = normalizeSpecialistPullRequestCompletion(completion)
	existingStore, existingBead, err := controller.findVisualBeadLocked(sourceExternalRef)
	if err != nil {
		return err
	}
	if visualBeadAdmissionState(existingBead) == "admitted_pr_verdict_recorded" {
		if existingBead.Status != beads.StatusClosed {
			return errors.New("durable specialist PR completion is not terminal")
		}
		encoded, _ := existingBead.Metadata["visual_hive_pr_completion_json"].(string)
		var existing SpecialistPullRequestCompletion
		if encoded == "" || json.Unmarshal([]byte(encoded), &existing) != nil {
			return errors.New("durable specialist PR completion is corrupt")
		}
		existing = normalizeSpecialistPullRequestCompletion(existing)
		canonical, marshalErr := json.Marshal(existing)
		if marshalErr != nil || encoded != string(canonical) {
			return errors.New("durable specialist PR completion is not canonical")
		}
		if existing != completion {
			return errors.New("specialist PR was already completed with different exact bytes")
		}
		return controller.recordSpecialistCompletionAudit(existingStore, existingBead, sourceExternalRef, existing)
	}
	if err := controller.refreshRuntimeConfigLocked(context.Background()); err != nil {
		return err
	}
	store, bead, envelope, decision, err := controller.findDispatchLocked(sourceExternalRef)
	if err != nil {
		return err
	}
	if err := validateVisualDispatchEnvelope(envelope, bead.ID, envelope.Work.Packet, envelope.Work, envelope.Finding, envelope.Evidence, envelope.EvidenceRoot, decision); err != nil {
		return errors.New("completed specialist dispatch failed immutable reconcile")
	}
	if completion.WorkOrderID != envelope.SpecialistWorkOrderID || completion.RequestSHA256 != envelope.SpecialistRequestSHA256 ||
		completion.WorkOrderID != "swo-"+completion.RequestSHA256 || completion.PullRequestNumber <= 0 || strings.TrimSpace(completion.Branch) == "" ||
		!validGitObject(completion.CommitSHA) || completion.VerdictHeadSHA != completion.CommitSHA || !validSHA256(completion.VerdictReceiptSHA256) || completion.VerdictStatus == "" {
		return errors.New("completed specialist PR identity or exact-head verdict receipt is invalid")
	}
	finding, exists := controller.lifecycle.Finding(envelope.Work.RepositoryFingerprint)
	lifecycleMatchesVerdict := exists && finding.Status == visualhive.StatusReady && readyFindingMatchesSpecialistCompletion(finding, envelope, completion)
	if !lifecycleMatchesVerdict || finding.PRNumber != completion.PullRequestNumber || finding.Branch != completion.Branch ||
		!strings.EqualFold(finding.RepairCommitSHA, completion.CommitSHA) || finding.PRURL != completion.PullRequestURL {
		return errors.New("completed specialist PR does not match the existing repair lifecycle")
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	auditEvent, err := specialistCompletionAuditEvent(bead, sourceExternalRef, completion, encoded)
	if err != nil {
		return err
	}
	if err := store.CloseWithUpdate(bead.ID, func(value *beads.Bead) {
		if value.Metadata == nil {
			value.Metadata = map[string]interface{}{}
		}
		value.Metadata["visual_hive_admission_state"] = "admitted_pr_verdict_recorded"
		value.Metadata["visual_hive_stage_detail"] = "exact-head PR verdict recorded; no merge or resolution authority granted"
		value.Metadata["visual_hive_pr_completion_json"] = string(encoded)
		value.Metadata["visual_hive_completion_audit_id"] = auditEvent.EventID
		value.Metadata["visual_hive_completion_audit_recorded"] = false
	}); err != nil {
		return err
	}
	return controller.recordSpecialistCompletionAudit(store, bead, sourceExternalRef, completion)
}

// RetireSpecialistPullRequest closes the exact specialist work-order bead
// after the lifecycle has durably returned the red proposal to its still-open
// issue. Replays are idempotent and re-attempt only an unacknowledged audit.
func (controller *Controller) RetireSpecialistPullRequest(sourceExternalRef string, retirement SpecialistPullRequestRetirement) error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	retirement = normalizeSpecialistPullRequestRetirement(retirement)
	store, bead, err := controller.findVisualBeadLocked(sourceExternalRef)
	if err != nil {
		return err
	}
	if visualBeadAdmissionState(bead) == "admitted_pr_retired_for_verification" {
		if bead.Status != beads.StatusClosed {
			return errors.New("durable specialist PR retirement is not terminal")
		}
		encoded, _ := bead.Metadata["visual_hive_pr_retirement_json"].(string)
		var existing SpecialistPullRequestRetirement
		if encoded == "" || json.Unmarshal([]byte(encoded), &existing) != nil {
			return errors.New("durable specialist PR retirement is corrupt")
		}
		existing = normalizeSpecialistPullRequestRetirement(existing)
		canonical, marshalErr := json.Marshal(existing)
		if marshalErr != nil || encoded != string(canonical) {
			return errors.New("durable specialist PR retirement is not canonical")
		}
		if existing != retirement {
			return errors.New("specialist PR was already retired with different exact bytes")
		}
		return controller.recordSpecialistRetirementAudit(store, bead, sourceExternalRef, existing)
	}
	if err := controller.refreshRuntimeConfigLocked(context.Background()); err != nil {
		return err
	}
	envelope, ok := visualDispatchEnvelope(bead)
	if !ok {
		return errors.New("specialist PR retirement has no durable dispatch intent")
	}
	if _, recorded := visualBeadAdmissionDecision(bead); !recorded {
		return errors.New("specialist PR retirement has no durable Governor decision")
	}
	finding, exists := controller.lifecycle.Finding(envelope.Work.RepositoryFingerprint)
	if !exists || finding.Status != visualhive.StatusIssueOpen || finding.LastRetiredRepair == nil {
		return errors.New("specialist PR retirement is not durably reflected in the lifecycle")
	}
	retired := finding.LastRetiredRepair
	if retirement.WorkOrderID != envelope.SpecialistWorkOrderID || retirement.RequestSHA256 != envelope.SpecialistRequestSHA256 ||
		retirement.WorkOrderID != "swo-"+retirement.RequestSHA256 || retirement.PullRequestNumber <= 0 ||
		strings.TrimSpace(retirement.Branch) == "" || !validGitObject(retirement.CommitSHA) ||
		strings.TrimSpace(retirement.BaseBranch) == "" || !validGitObject(retirement.BaseSHA) || !validGitObject(retirement.CurrentDefaultHeadSHA) ||
		retirement.BaseSHA == retirement.CurrentDefaultHeadSHA || !validSHA256(retirement.VerdictReceiptSHA256) ||
		retirement.RetirementReasonCode != "default_branch_changed_requires_fresh_authoritative_verification" ||
		retired.PullRequestNumber != retirement.PullRequestNumber || retired.PullRequestURL != retirement.PullRequestURL ||
		retired.Branch != retirement.Branch || !strings.EqualFold(retired.HeadSHA, retirement.CommitSHA) ||
		retired.BaseBranch != retirement.BaseBranch ||
		!strings.EqualFold(retired.BaseSHA, retirement.BaseSHA) ||
		!strings.EqualFold(retired.CurrentDefaultHeadSHA, retirement.CurrentDefaultHeadSHA) ||
		!strings.EqualFold(retired.VerdictReceiptSHA256, retirement.VerdictReceiptSHA256) {
		return errors.New("specialist PR retirement does not match the exact lifecycle, work order, proposal, and red receipt")
	}
	encoded, err := json.Marshal(retirement)
	if err != nil {
		return err
	}
	auditEvent, err := specialistRetirementAuditEvent(bead, sourceExternalRef, retirement, encoded)
	if err != nil {
		return err
	}
	if err := store.CloseWithUpdate(bead.ID, func(value *beads.Bead) {
		if value.Metadata == nil {
			value.Metadata = map[string]interface{}{}
		}
		value.Metadata["visual_hive_admission_state"] = "admitted_pr_retired_for_verification"
		value.Metadata["visual_hive_stage_detail"] = "exact red repair proposal retired; awaiting fresh authoritative verification"
		value.Metadata["visual_hive_pr_retirement_json"] = string(encoded)
		value.Metadata["visual_hive_retirement_audit_id"] = auditEvent.EventID
		value.Metadata["visual_hive_retirement_audit_recorded"] = false
	}); err != nil {
		return err
	}
	return controller.recordSpecialistRetirementAudit(store, bead, sourceExternalRef, retirement)
}

func normalizeSpecialistPullRequestRetirement(retirement SpecialistPullRequestRetirement) SpecialistPullRequestRetirement {
	retirement.WorkOrderID = strings.TrimSpace(retirement.WorkOrderID)
	retirement.RequestSHA256 = strings.ToLower(strings.TrimSpace(retirement.RequestSHA256))
	retirement.Branch = strings.TrimSpace(retirement.Branch)
	retirement.CommitSHA = strings.ToLower(strings.TrimSpace(retirement.CommitSHA))
	retirement.BaseBranch = strings.TrimSpace(retirement.BaseBranch)
	retirement.BaseSHA = strings.ToLower(strings.TrimSpace(retirement.BaseSHA))
	retirement.CurrentDefaultHeadSHA = strings.ToLower(strings.TrimSpace(retirement.CurrentDefaultHeadSHA))
	retirement.PullRequestURL = strings.TrimSpace(retirement.PullRequestURL)
	retirement.VerdictReceiptSHA256 = strings.ToLower(strings.TrimSpace(retirement.VerdictReceiptSHA256))
	retirement.RetirementReasonCode = strings.TrimSpace(retirement.RetirementReasonCode)
	return retirement
}

func (controller *Controller) recordSpecialistRetirementAudit(store *beads.Store, bead *beads.Bead, sourceExternalRef string, retirement SpecialistPullRequestRetirement) error {
	encoded, err := json.Marshal(retirement)
	if err != nil {
		return err
	}
	event, err := specialistRetirementAuditEvent(bead, sourceExternalRef, retirement, encoded)
	if err != nil {
		return err
	}
	storedID, _ := bead.Metadata["visual_hive_retirement_audit_id"].(string)
	if storedID != "" && storedID != event.EventID {
		return errors.New("durable specialist PR retirement audit identity is corrupt")
	}
	if value, exists := bead.Metadata["visual_hive_retirement_audit_recorded"]; exists {
		recorded, valid := value.(bool)
		if !valid {
			return errors.New("durable specialist PR retirement audit state is corrupt")
		}
		if recorded {
			if storedID != event.EventID {
				return errors.New("durable specialist PR retirement audit identity is unavailable")
			}
			return nil
		}
	}
	if err := controller.recordAudit(context.Background(), event); err != nil {
		return fmt.Errorf("record exact specialist PR retirement audit: %w", err)
	}
	if err := store.Update(bead.ID, func(value *beads.Bead) {
		if value.Metadata == nil {
			value.Metadata = map[string]interface{}{}
		}
		value.Metadata["visual_hive_retirement_audit_id"] = event.EventID
		value.Metadata["visual_hive_retirement_audit_recorded"] = true
	}); err != nil {
		return fmt.Errorf("persist exact specialist PR retirement audit acknowledgement: %w", err)
	}
	return nil
}

func specialistRetirementAuditEvent(bead *beads.Bead, sourceExternalRef string, retirement SpecialistPullRequestRetirement, encoded []byte) (AuditEvent, error) {
	envelope, ok := visualDispatchEnvelope(bead)
	if !ok || strings.TrimSpace(envelope.Work.RepositoryFingerprint) == "" {
		return AuditEvent{}, errors.New("durable specialist PR retirement audit binding is unavailable")
	}
	canonical := append([]byte(strings.TrimSpace(envelope.Work.RepositoryFingerprint)+"\n"+strings.TrimSpace(sourceExternalRef)+"\n"), encoded...)
	digest := sha256.Sum256(canonical)
	return AuditEvent{
		EventID:               "visual-work-retirement:" + hex.EncodeToString(digest[:]),
		Stage:                 "admitted_pr_retired_for_verification",
		SourceExternalRef:     strings.TrimSpace(sourceExternalRef),
		RepositoryFingerprint: envelope.Work.RepositoryFingerprint,
		Detail: fmt.Sprintf(
			"work_order=%s pr=%d branch=%s red_head=%s changed_default_head=%s receipt=%s reason=%s no_resolution_authority=true",
			retirement.WorkOrderID, retirement.PullRequestNumber, retirement.Branch, retirement.CommitSHA,
			retirement.CurrentDefaultHeadSHA, retirement.VerdictReceiptSHA256, retirement.RetirementReasonCode,
		),
	}, nil
}

func (controller *Controller) recordSpecialistCompletionAudit(store *beads.Store, bead *beads.Bead, sourceExternalRef string, completion SpecialistPullRequestCompletion) error {
	encoded, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	event, err := specialistCompletionAuditEvent(bead, sourceExternalRef, completion, encoded)
	if err != nil {
		return err
	}
	storedID, _ := bead.Metadata["visual_hive_completion_audit_id"].(string)
	if storedID != "" && storedID != event.EventID {
		return errors.New("durable specialist PR completion audit identity is corrupt")
	}
	if value, exists := bead.Metadata["visual_hive_completion_audit_recorded"]; exists {
		recorded, valid := value.(bool)
		if !valid {
			return errors.New("durable specialist PR completion audit state is corrupt")
		}
		if recorded {
			if storedID != event.EventID {
				return errors.New("durable specialist PR completion audit identity is unavailable")
			}
			return nil
		}
	}
	if err := controller.recordAudit(context.Background(), event); err != nil {
		return fmt.Errorf("record exact-head specialist PR completion audit: %w", err)
	}
	if err := store.Update(bead.ID, func(value *beads.Bead) {
		if value.Metadata == nil {
			value.Metadata = map[string]interface{}{}
		}
		value.Metadata["visual_hive_completion_audit_id"] = event.EventID
		value.Metadata["visual_hive_completion_audit_recorded"] = true
	}); err != nil {
		return fmt.Errorf("persist exact-head specialist PR completion audit acknowledgement: %w", err)
	}
	return nil
}

func specialistCompletionAuditEvent(bead *beads.Bead, sourceExternalRef string, completion SpecialistPullRequestCompletion, encoded []byte) (AuditEvent, error) {
	envelope, ok := visualDispatchEnvelope(bead)
	if !ok || strings.TrimSpace(envelope.Work.RepositoryFingerprint) == "" {
		return AuditEvent{}, errors.New("durable specialist PR completion audit binding is unavailable")
	}
	canonical := append([]byte(strings.TrimSpace(envelope.Work.RepositoryFingerprint)+"\n"+strings.TrimSpace(sourceExternalRef)+"\n"), encoded...)
	digest := sha256.Sum256(canonical)
	return AuditEvent{
		EventID:               "visual-work-completion:" + hex.EncodeToString(digest[:]),
		Stage:                 "admitted_pr_verdict_recorded",
		SourceExternalRef:     strings.TrimSpace(sourceExternalRef),
		RepositoryFingerprint: envelope.Work.RepositoryFingerprint,
		Detail: fmt.Sprintf(
			"work_order=%s pr=%d branch=%s exact_head=%s receipt=%s verdict=%s no_merge_authority=true",
			completion.WorkOrderID, completion.PullRequestNumber, completion.Branch, completion.CommitSHA, completion.VerdictReceiptSHA256, completion.VerdictStatus,
		),
	}, nil
}

func readyFindingMatchesSpecialistCompletion(finding visualhive.FindingLifecycle, envelope DispatchEnvelope, completion SpecialistPullRequestCompletion) bool {
	receipt := finding.LastPullRequestCheckReceipt
	if receipt == nil {
		return false
	}
	identity := receipt.Identity
	return receipt.ReceiptSHA256 == completion.VerdictReceiptSHA256 &&
		identity.Authority == (visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true}) &&
		strings.EqualFold(identity.Source.Repository, finding.Repository) && identity.Source.RepositoryID == finding.RepositoryID &&
		identity.Source.PullRequest == completion.PullRequestNumber && strings.EqualFold(identity.Source.Base.Repository, finding.Repository) &&
		identity.Source.Base.RepositoryID == finding.RepositoryID && identity.Source.Base.Ref == strings.TrimPrefix(envelope.Work.Packet.SourceRef, "refs/heads/") &&
		identity.Source.Base.SHA == envelope.BaseSHA && strings.EqualFold(identity.Source.Head.Repository, finding.Repository) && identity.Source.Head.RepositoryID == finding.RepositoryID &&
		identity.Source.Head.Ref == completion.Branch && identity.Source.Head.SHA == completion.CommitSHA &&
		identity.Workflow.Name == "Visual Hive PR" && identity.Workflow.Path == ".github/workflows/visual-hive-pr.yml" && identity.Workflow.Event == "pull_request" &&
		identity.Producer.GitCommit == hivegithub.VisualHivePullRequestProducerCommit && identity.Check.State == "success" &&
		identity.Check.Conclusion == completion.VerdictStatus
}

func normalizeSpecialistPullRequestCompletion(completion SpecialistPullRequestCompletion) SpecialistPullRequestCompletion {
	completion.WorkOrderID = strings.TrimSpace(completion.WorkOrderID)
	completion.RequestSHA256 = strings.ToLower(strings.TrimSpace(completion.RequestSHA256))
	completion.Branch = strings.TrimSpace(completion.Branch)
	completion.CommitSHA = strings.ToLower(strings.TrimSpace(completion.CommitSHA))
	completion.PullRequestURL = strings.TrimSpace(completion.PullRequestURL)
	completion.VerdictHeadSHA = strings.ToLower(strings.TrimSpace(completion.VerdictHeadSHA))
	completion.VerdictReceiptSHA256 = strings.ToLower(strings.TrimSpace(completion.VerdictReceiptSHA256))
	completion.VerdictStatus = strings.TrimSpace(completion.VerdictStatus)
	return completion
}

func (controller *Controller) findVisualBeadLocked(sourceExternalRef string) (*beads.Store, *beads.Bead, error) {
	var store *beads.Store
	var bead *beads.Bead
	for _, role := range sortedStoreRoles(controller.beadStores) {
		if candidate := controller.beadStores[role].FindByExternalRef(sourceExternalRef); candidate != nil {
			if bead != nil {
				return nil, nil, errors.New("source external ref exists in multiple role stores")
			}
			store, bead = controller.beadStores[role], candidate
		}
	}
	if bead == nil {
		return nil, nil, errors.New("durable Visual Hive intent is unavailable")
	}
	return store, bead, nil
}

func (controller *Controller) findDispatchLocked(sourceExternalRef string) (*beads.Store, *beads.Bead, DispatchEnvelope, governor.WorkAdmissionDecision, error) {
	store, bead, err := controller.findVisualBeadLocked(sourceExternalRef)
	if err != nil {
		return nil, nil, DispatchEnvelope{}, governor.WorkAdmissionDecision{}, err
	}
	if visualBeadAdmissionState(bead) != "admitted_dispatch_pending" {
		return nil, nil, DispatchEnvelope{}, governor.WorkAdmissionDecision{}, errors.New("durable dispatch-pending intent is unavailable")
	}
	envelope, ok := visualDispatchEnvelope(bead)
	if !ok {
		return nil, nil, DispatchEnvelope{}, governor.WorkAdmissionDecision{}, errors.New("durable dispatch-pending intent is corrupt")
	}
	decision, recorded := visualBeadAdmissionDecision(bead)
	if !recorded {
		return nil, nil, DispatchEnvelope{}, governor.WorkAdmissionDecision{}, errors.New("durable Governor decision is unavailable")
	}
	return store, bead, envelope, decision, nil
}

type Result struct {
	Lifecycle       visualhive.ApplyLifecycleResult
	Decisions       []governor.WorkAdmissionDecision
	DispatchPending []DispatchEnvelope
	Errors          []string
}

type DispatchEnvelope struct {
	SchemaVersion           string                           `json:"schema_version"`
	BeadID                  string                           `json:"bead_id"`
	SourceExternalRef       string                           `json:"source_external_ref"`
	Work                    visualhive.AdmittedVisualWork    `json:"work"`
	Finding                 visualhive.FindingLifecycle      `json:"finding"`
	Evidence                agent.SpecialistEvidenceIdentity `json:"evidence"`
	EvidenceRoot            string                           `json:"evidence_root"`
	VerificationReceiptJSON string                           `json:"verification_receipt_json"`
	GovernorRequestJSON     string                           `json:"governor_request_json"`
	GovernorRequestSHA256   string                           `json:"governor_request_sha256"`
	GovernorAllowed         bool                             `json:"governor_allowed"`
	GovernorDecisionCode    string                           `json:"governor_decision_code"`
	GovernorDecisionReason  string                           `json:"governor_decision_reason"`
	WorkIdentitySHA256      string                           `json:"work_identity_sha256"`
	ExecutionPolicySHA256   string                           `json:"execution_policy_sha256"`
	BaseSHA                 string                           `json:"base_sha"`
	BaseTreeSHA             string                           `json:"base_tree_sha"`
	AllowedRepairPaths      []string                         `json:"allowed_repair_paths"`
	ValidationCommands      []string                         `json:"validation_commands"`
	Reproduction            []string                         `json:"reproduction_commands"`
	Recurrence              uint64                           `json:"recurrence"`
	Attempt                 uint64                           `json:"attempt"`
	CompositionDeadline     time.Time                        `json:"composition_deadline"`
	SpecialistWorkOrderID   string                           `json:"specialist_work_order_id,omitempty"`
	SpecialistRequestSHA256 string                           `json:"specialist_request_sha256,omitempty"`
}

func New(
	gov *governor.Governor,
	lifecycle *visualhive.LifecycleStore,
	beadStores map[string]*beads.Store,
	roles RoleState,
	issueClient visualhive.LifecycleIssueClient,
	installed integrated.Config,
	normalACMM int,
) (*Controller, error) {
	if gov == nil || lifecycle == nil || len(beadStores) == 0 {
		return nil, errors.New("normal governor, Visual lifecycle, and role bead stores are required")
	}
	if err := validateRuntimeConfig(installed, normalACMM); err != nil {
		return nil, err
	}
	return &Controller{
		governor: gov, lifecycle: lifecycle, beadStores: beadStores, roles: roles, issueClient: issueClient,
		installed: cloneIntegratedConfig(installed), normalACMM: normalACMM, baseTree: resolveInstalledBaseTree,
		recordAdmission: recordVisualAdmissionDecision, markStage: markVisualBeadStage, persistDispatch: persistVisualDispatchEnvelope,
		loadPlan: func(source hivegithub.VerifiedVisualHiveArtifact) (visualhive.VerifiedImportPlan, error) {
			return source.ImportPlan()
		}, now: func() time.Time { return time.Now().UTC() },
	}, nil
}

func (controller *Controller) refreshRuntimeConfigLocked(ctx context.Context) error {
	if controller.runtimeConfig == nil {
		return nil
	}
	installed, normalACMM, err := controller.runtimeConfig()
	if err == nil {
		err = validateRuntimeConfig(installed, normalACMM)
	}
	if err == nil && !sameRuntimeIdentity(controller.installed, installed) {
		err = errors.New("authoritative Visual Hive repository or state identity drifted from the initialized controller")
	}
	if err != nil {
		err = fmt.Errorf("refresh authoritative Visual Hive runtime contract: %w", err)
		auditErr := controller.recordAudit(ctx, AuditEvent{Stage: "held_runtime_config", Detail: err.Error()})
		return errors.Join(err, auditErr)
	}
	controller.installed = cloneIntegratedConfig(installed)
	controller.normalACMM = normalACMM
	return nil
}

func validateRuntimeConfig(installed integrated.Config, normalACMM int) error {
	if strings.TrimSpace(installed.Repository) == "" || strings.TrimSpace(installed.RepositoryID) == "" || strings.TrimSpace(installed.DefaultBranch) == "" ||
		strings.TrimSpace(installed.StateDir) == "" || strings.TrimSpace(installed.VisualHiveRef) == "" ||
		installed.ACMMLevel < 1 || installed.ACMMLevel > 6 || normalACMM < 1 || normalACMM > 6 {
		return errors.New("installed Visual Hive repository contract and ACMM levels 1 through 6 are required")
	}
	return nil
}

func sameRuntimeIdentity(current, next integrated.Config) bool {
	return strings.EqualFold(strings.TrimSpace(current.Repository), strings.TrimSpace(next.Repository)) &&
		strings.TrimSpace(current.RepositoryID) == strings.TrimSpace(next.RepositoryID) &&
		filepath.Clean(strings.TrimSpace(current.StateDir)) == filepath.Clean(strings.TrimSpace(next.StateDir))
}

func cloneIntegratedConfig(source integrated.Config) integrated.Config {
	clone := source
	clone.ProviderArgs = append([]string(nil), source.ProviderArgs...)
	if source.PreviousHostedRelease != nil {
		identity := *source.PreviousHostedRelease
		clone.PreviousHostedRelease = &identity
	}
	clone.VisualHiveArgs = append([]string(nil), source.VisualHiveArgs...)
	if source.ManagedPathPreimages != nil {
		clone.ManagedPathPreimages = make(map[string]integrated.ManagedPathPreimage, len(source.ManagedPathPreimages))
		for path, preimage := range source.ManagedPathPreimages {
			preimage.Content = append([]byte(nil), preimage.Content...)
			clone.ManagedPathPreimages[path] = preimage
		}
	}
	clone.SetupBaselineInitialCandidates = append([]integrated.SetupBaselineCandidate(nil), source.SetupBaselineInitialCandidates...)
	clone.TestCommands = cloneStringMatrix(source.TestCommands)
	clone.AllowedRepairPaths = append([]string(nil), source.AllowedRepairPaths...)
	clone.AllowedAutoMergePaths = append([]string(nil), source.AllowedAutoMergePaths...)
	clone.AllowedAutoMergeRisk = append([]automation.RiskTier(nil), source.AllowedAutoMergeRisk...)
	return clone
}

func cloneStringMatrix(source [][]string) [][]string {
	if source == nil {
		return nil
	}
	clone := make([][]string, len(source))
	for index := range source {
		clone[index] = append([]string(nil), source[index]...)
	}
	return clone
}

func (controller *Controller) Import(ctx context.Context, source hivegithub.VerifiedVisualHiveArtifact) (Result, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := controller.refreshRuntimeConfigLocked(ctx); err != nil {
		return Result{}, err
	}

	if controller.loadPlan == nil {
		return Result{}, errors.New("sealed verified Visual Hive plan loader is unavailable")
	}
	plan, err := controller.loadPlan(source)
	if err != nil {
		return Result{}, err
	}
	packet := plan.Packet()
	if err := controller.validatePacket(packet); err != nil {
		return Result{}, err
	}
	apply := visualhive.ApplyLifecycleOptions{
		TargetRef: controller.installed.DefaultBranch, CurrentTargetCommitSHA: packet.BaseSHA,
		MaxActiveIssues:         controller.installed.MaxActiveIssues,
		PreferRepairable:        controller.installed.Automation == integrated.AutomationRepairPR || controller.installed.Automation == integrated.AutomationAutoMerge,
		DisableIssuePublication: true,
	}
	works, err := controller.lifecycle.ResolveImportPlanWorks(plan, apply)
	if err != nil {
		return Result{}, err
	}
	router := newVisualRoleBeadRouter(controller.beadStores, works)
	if router.err != nil {
		return Result{}, controller.recordRoutingHold(ctx, packet, router.err)
	}
	lifecycleResult, err := controller.lifecycle.ApplyImportPlan(plan, router, apply)
	result := Result{Lifecycle: lifecycleResult}
	if err != nil {
		return result, err
	}
	works = controller.resumeWorks(packet, works)
	router = newVisualRoleBeadRouter(controller.beadStores, works)
	if router.err != nil {
		return result, controller.recordRoutingHold(ctx, packet, router.err)
	}
	return controller.resumeAppliedWork(ctx, source, packet, works, router, result), nil
}

// Resume is the production crash-recovery seam. Callers must re-fetch and
// reverify the sealed source; packet and work fields are always re-derived by
// ImportPlan, and Import serializes recovery with intake and reservation.
func (controller *Controller) Resume(ctx context.Context, source hivegithub.VerifiedVisualHiveArtifact) (Result, error) {
	return controller.Import(ctx, source)
}

// resumeWorks adds only Hive-owned authoritative absence transitions inferred
// by the existing lifecycle. Their source identity and semantics come from the
// persisted finding and exact verified packet; no producer preview is used.
func (controller *Controller) resumeWorks(packet visualhive.VerifiedPacketIdentity, works []visualhive.AdmittedVisualWork) []visualhive.AdmittedVisualWork {
	result := append([]visualhive.AdmittedVisualWork(nil), works...)
	seen := make(map[string]bool, len(result))
	for _, work := range result {
		seen[work.RepositoryFingerprint] = true
	}
	snapshot := controller.lifecycle.Snapshot()
	keys := make([]string, 0, len(snapshot.Findings))
	for key := range snapshot.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := snapshot.Findings[key]
		if finding == nil || seen[key] || finding.LastBundleID != packet.PacketID || finding.LastBundleDigest != packet.PacketDigest ||
			(finding.Status != visualhive.StatusResolved && finding.Status != visualhive.StatusIssueClosed) {
			continue
		}
		validation := []string{}
		if command := strings.TrimSpace(finding.ValidationCommand); command != "" {
			validation = append(validation, command)
		}
		externalRef := visualSourceExternalRef(finding.Repository, finding.RepositoryFingerprint)
		role := controller.existingRoleForExternalRef(externalRef)
		result = append(result, visualhive.AdmittedVisualWork{
			SourceExternalRef: externalRef, Packet: packet,
			FindingFingerprint: finding.Fingerprint, RepositoryFingerprint: finding.RepositoryFingerprint,
			ObservationState: "absent", PublicationRole: finding.PublicationRole, RootCauseKey: finding.RootCauseKey,
			BlockedByRootKeys: append([]string(nil), finding.BlockedByRootKeys...), IssueKind: finding.IssueKind,
			Severity: finding.Severity, Title: finding.Title, Body: finding.Body, Labels: append([]string(nil), finding.Labels...), Role: role,
			RoutingReason: "persisted owning role for authoritative Hive lifecycle absence", RoutingAllowed: true,
			AffectedContracts: append([]string(nil), finding.AffectedContracts...), AffectedFiles: append([]string(nil), finding.AffectedFiles...), KnowledgeKeywordState: "unavailable_no_verified_facts",
			ValidationCommands: validation, ReproductionCommands: append([]string(nil), validation...),
			ReproductionSource: "persisted_verified_observation_validation_command", Authority: visualhive.VisualWorkAuthority{ProposalOnly: true},
		})
	}
	sortVisualAdmissionWorks(result)
	return result
}

// sortVisualAdmissionWorks keeps admission deterministic while ensuring a
// lower-severity advisory cannot consume a role's bounded WIP ahead of a
// verified higher-severity finding from the same packet. Fingerprints remain
// the stable tie-breaker, so identical evidence produces identical ordering.
func sortVisualAdmissionWorks(works []visualhive.AdmittedVisualWork) {
	sort.Slice(works, func(i, j int) bool {
		left, right := visualAdmissionSeverityRank(works[i].Severity), visualAdmissionSeverityRank(works[j].Severity)
		if left != right {
			return left < right
		}
		return works[i].RepositoryFingerprint < works[j].RepositoryFingerprint
	})
}

func visualAdmissionSeverityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	case "low":
		return 3
	default:
		return 4
	}
}

func (controller *Controller) existingRoleForExternalRef(externalRef string) string {
	role := ""
	for _, candidate := range sortedStoreRoles(controller.beadStores) {
		if controller.beadStores[candidate].FindByExternalRef(externalRef) == nil {
			continue
		}
		if role != "" {
			return ""
		}
		role = candidate
	}
	return role
}

func visualSourceExternalRef(repository, repositoryFingerprint string) string {
	return "visual-hive://" + strings.ToLower(strings.TrimSpace(repository)) + "/" + strings.ToLower(strings.TrimSpace(repositoryFingerprint))
}

func (controller *Controller) resumeAppliedWork(
	ctx context.Context,
	source visualSpecialistEvidenceSource,
	packet visualhive.VerifiedPacketIdentity,
	works []visualhive.AdmittedVisualWork,
	router *visualRoleBeadRouter,
	result Result,
) Result {
	if err := controller.validatePacket(packet); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	for _, work := range works {
		// Bead is the sealed lifecycle-import projection and is intentionally
		// excluded from AdmittedVisualWork JSON. Routing and lifecycle import
		// have consumed it before this boundary; retaining it would make the
		// in-memory work differ from its exact durable dispatch representation.
		work.Bead = beads.BatchInput{}
		storageRole := router.RoleForExternalRef(work.SourceExternalRef)
		if work.RoutingAllowed && storageRole != "" {
			role := storageRole
			work.Role = role
		}
		store := controller.beadStores[storageRole]
		bead := router.FindByExternalRef(work.SourceExternalRef)
		finding, exists := controller.lifecycle.Finding(work.RepositoryFingerprint)
		if !exists {
			continue
		}
		evidence, evidenceErr := source.SpecialistEvidenceIdentity(finding)
		evidenceRoot, rootErr := source.VerifiedEvidenceRoot()
		if evidenceErr == nil && rootErr != nil {
			evidenceErr = rootErr
		}
		if evidenceErr == nil {
			evidenceErr = validateVisualEvidenceForPacket(packet, evidence)
		}
		if bead == nil {
			continue
		}
		state := visualBeadAdmissionState(bead)
		decision, recorded := visualBeadAdmissionDecision(bead)
		controlledDecisionState := state == "denied" || strings.HasPrefix(state, "admitted_") || strings.HasPrefix(state, "held_manual_review")
		if controlledDecisionState && !recorded {
			controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_admission_receipt", "controlled state has no readable durable Governor decision", &result)
			continue
		}
		if controlledDecisionState {
			if err := validateStoredAdmissionDecision(decision); err != nil {
				controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_admission_receipt", err.Error(), &result)
				continue
			}
		}
		if state == "admitted_dispatch_deferred" {
			deferred, deferredOK := visualDispatchDeferral(bead)
			envelope, envelopeOK := visualDispatchEnvelope(bead)
			if !deferredOK || !envelopeOK || deferred.SourceExternalRef != work.SourceExternalRef || deferred.SelectedSourceExternalRef == work.SourceExternalRef ||
				deferred.PacketDigest != envelope.Work.Packet.PacketDigest {
				controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_dispatch_intent", "deferred dispatch has no exact selection, correlation, packet, or envelope binding", &result)
				continue
			}
			if deferred.PacketDigest == packet.PacketDigest {
				if validateVisualDispatchEnvelope(envelope, bead.ID, packet, work, finding, evidence, evidenceRoot, decision) != nil {
					controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_dispatch_intent", "deferred dispatch conflicts with its exact verified packet", &result)
					continue
				}
				// The one-PR acceptance loop explicitly made this exact packet
				// ineligible for launch. Current launch policy is relevant only
				// after a later verified packet reopens it; same-packet replay
				// must preserve the durable deferral without converting it into
				// a runtime hold.
				continue
			}
			if err := controller.markStage(store, bead.ID, "pending", "later verified packet reopened a previously deferred launchable finding"); err != nil {
				result.Errors = append(result.Errors, err.Error())
				continue
			}
			decision, recorded, state = governor.WorkAdmissionDecision{}, false, "pending"
		}
		if state == "admitted_dispatch_pending" || state == "admitted_dispatch_runtime_held" || state == "admitted_manual_review_held" {
			envelope, ok := visualDispatchEnvelope(bead)
			if !ok && state == "admitted_dispatch_pending" {
				controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_dispatch_intent", "dispatch-pending state has no readable durable dispatch intent", &result)
				continue
			}
			if ok && validateVisualDispatchEnvelope(envelope, bead.ID, packet, work, finding, evidence, evidenceRoot, decision) == nil {
				if launchErr := controller.dispatchLaunchAllowed(envelope, finding); launchErr != nil {
					controller.persistAndAuditStage(ctx, store, bead, work, decision, dispatchHoldStage(finding), launchErr.Error(), &result)
					continue
				}
				if state != "admitted_dispatch_pending" {
					if err := controller.persistDispatch(store, bead.ID, envelope); err != nil {
						result.Errors = append(result.Errors, err.Error())
						continue
					}
				}
				result.DispatchPending = append(result.DispatchPending, envelope)
				continue
			}
			if !ok {
				// A pause or authority hold can be recorded before composition;
				// resume through the normal admitted path when it clears.
			} else {
				controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_dispatch_intent", "durable dispatch intent conflicts with sealed work, lifecycle, evidence, or Governor policy", &result)
				continue
			}
		}
		allowedPaths := integrated.RepairPathsForFinding(controller.installed.AllowedRepairPaths, finding)
		policyDeniedFiles := repairPolicyDeniedFiles(work.AffectedFiles, allowedPaths)
		baseTreeSHA, treeErr := controller.baseTree(ctx, controller.installed, packet.BaseSHA)
		repairAuthorized := controller.installed.Automation == integrated.AutomationRepairPR || controller.installed.Automation == integrated.AutomationAutoMerge
		issueAuthorized := controller.installed.Automation == integrated.AutomationIssues || repairAuthorized
		manualLifecycleHold := finding.HumanReviewRequired || finding.ObservationHumanReviewRequired || strings.TrimSpace(finding.ManualReviewKind) != ""
		validationReady := true
		if repairAuthorized && work.ObservationState != "absent" && !manualLifecycleHold {
			_, validationErr := visualhive.ResolveValidationCommandArgv(work.ValidationCommands, controller.installed.TestCommands)
			validationReady = validationErr == nil
		}
		safeExecution := evidenceErr == nil && validationReady
		repairReady := safeExecution && !manualLifecycleHold && len(policyDeniedFiles) == 0 && treeErr == nil && baseTreeSHA != "" && len(allowedPaths) > 0 && len(work.ValidationCommands) > 0
		activeWIP := 0
		if store != nil {
			for _, candidate := range store.List(beads.ListFilter{}) {
				if candidate.ID == bead.ID {
					continue
				}
				candidateStage := visualBeadAdmissionState(candidate)
				if candidate.Status == beads.StatusOpen || candidate.Status == beads.StatusInProgress ||
					candidateStage == "admitted_awaiting_issue" || candidateStage == "admitted_dispatch_pending" || candidateStage == "admitted_repair_held" || candidateStage == "admitted_manual_review_held" {
					activeWIP++
				}
			}
		}
		runtimePaused := controller.roles != nil && controller.roles.IsPaused(work.Role)
		request := governor.WorkAdmissionRequest{
			SourceExternalRef: work.SourceExternalRef, PacketDigest: packet.PacketDigest,
			FindingFingerprint: work.FindingFingerprint, RepositoryFingerprint: work.RepositoryFingerprint,
			Role: work.Role, RoutingReason: work.RoutingReason, RoutingAllowed: work.RoutingAllowed,
			RuntimePaused: runtimePaused || controller.installed.Paused, AuthorityAllowsWork: issueAuthorized,
			SafeExecutionReady: safeExecution, ActiveWIP: activeWIP, WIPLimit: controller.installed.MaxActiveIssues,
			Evidence: evidence, WorkIdentitySHA256: visualWorkIdentitySHA256(work),
			ExecutionPolicySHA256: visualExecutionPolicySHA256(baseTreeSHA, allowedPaths, work.ValidationCommands, issueAuthorized, repairAuthorized),
			RuntimePolicySHA256:   visualRuntimePolicySHA256(controller.installed, controller.normalACMM),
		}
		boundRequest := controller.governor.BindAdmissionRequest(request)
		if recorded {
			matches, corrupt := visualAdmissionMatchesCurrentWork(decision, request)
			if corrupt {
				controller.holdCorruptStage(ctx, store, bead, work, "held_corrupt_admission_receipt", "durable Governor decision is not internally cross-bound", &result)
				continue
			}
			if !matches || visualTransientDenialNeedsReevaluation(decision, boundRequest) {
				detail := "new packet, evidence, work identity, or policy requires fresh Governor admission"
				if matches {
					detail = "transient Governor denial inputs changed and require deterministic re-evaluation"
				}
				if err := controller.markStage(store, bead.ID, "pending", detail); err != nil {
					result.Errors = append(result.Errors, err.Error())
					continue
				}
				decision, recorded, state = governor.WorkAdmissionDecision{}, false, "pending"
			}
		}
		if !recorded && !decision.Allowed {
			decision = controller.governor.AdmitWork(request)
			result.Decisions = append(result.Decisions, decision)
			if !decision.Allowed {
				if err := controller.recordAdmission(store, bead.ID, "denied", decision); err != nil {
					result.Errors = append(result.Errors, err.Error())
					continue
				}
				persistedBead := store.FindByExternalRef(work.SourceExternalRef)
				persisted, ok := visualBeadAdmissionDecision(persistedBead)
				if !ok || !reflect.DeepEqual(persisted, decision) || persisted.Allowed || visualBeadAdmissionState(persistedBead) != "denied" {
					result.Errors = append(result.Errors, "durable Governor denial receipt could not be revalidated")
					continue
				}
				if err := controller.recordAudit(ctx, AuditEvent{Stage: "denied", SourceExternalRef: work.SourceExternalRef, RepositoryFingerprint: work.RepositoryFingerprint, Decision: &persisted, Detail: persisted.Reason}); err != nil {
					result.Errors = append(result.Errors, err.Error())
				}
				decision, recorded, state = persisted, true, "denied"
			} else {
				if err := controller.recordAdmission(store, bead.ID, "admitted_awaiting_issue", decision); err != nil {
					result.Errors = append(result.Errors, err.Error())
					continue
				}
				persistedBead := store.FindByExternalRef(work.SourceExternalRef)
				persisted, ok := visualBeadAdmissionDecision(persistedBead)
				if !ok || !reflect.DeepEqual(persisted, decision) || !persisted.Allowed || persisted.RequestSHA256 == "" {
					result.Errors = append(result.Errors, "durable Governor allow receipt could not be revalidated")
					continue
				}
				decision, recorded, state = persisted, true, visualBeadAdmissionState(persistedBead)
				if err := controller.recordAudit(ctx, AuditEvent{Stage: state, SourceExternalRef: work.SourceExternalRef, RepositoryFingerprint: work.RepositoryFingerprint, Decision: &decision, Detail: decision.Reason}); err != nil {
					result.Errors = append(result.Errors, err.Error())
					continue
				}
			}
		}
		if !decision.Allowed {
			if decision.Code == "routing_held" {
				controller.maintainManualIssueLifecycle(ctx, router, store, bead, work, packet, decision, issueAuthorized, &result)
			}
			continue
		}
		if !issueAuthorized {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_dispatch_runtime_held", "current installed automation authority no longer permits issue-backed repair", &result)
			continue
		}
		if controller.issueClient == nil {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_awaiting_issue", "normal issue writer is unavailable", &result)
			continue
		}
		finding, exists = controller.lifecycle.Finding(work.RepositoryFingerprint)
		if !exists {
			result.Errors = append(result.Errors, "admitted lifecycle finding disappeared before issue synchronization")
			continue
		}
		action := finding.PendingIssueAction
		if action == "" && (work.ObservationState == "absent" || finding.Status == visualhive.StatusResolved || finding.Status == visualhive.StatusIssueClosed) {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_lifecycle_synced", "authoritative absence required no issue side effect", &result)
			continue
		}
		if action == "" && finding.IssueNumber <= 0 {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_lifecycle_held", "lifecycle did not authorize an issue transition", &result)
			continue
		}
		if _, queueErr := controller.lifecycle.QueueAdmittedIssueAction(work.RepositoryFingerprint, packet); queueErr != nil {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_awaiting_issue", queueErr.Error(), &result)
			continue
		}
		policy := integrated.PolicyForConfig(controller.installed)
		if controller.normalACMM < policy.ACMMLevel {
			policy.ACMMLevel = controller.normalACMM
		}
		outbox := visualhive.ProcessOutboxForFinding(ctx, controller.lifecycle, router, policy, controller.issueClient, work.RepositoryFingerprint)
		finding, exists = controller.lifecycle.Finding(work.RepositoryFingerprint)
		if outbox.Failed > 0 || outbox.Denied > 0 || !exists || finding.PendingIssueAction != "" {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_awaiting_issue", "issue synchronization incomplete", &result)
			continue
		}
		if action == visualhive.OutboxCloseIssue {
			if finding.Status != visualhive.StatusIssueClosed {
				result.Errors = append(result.Errors, "close action did not durably close lifecycle issue")
				continue
			}
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_lifecycle_synced", "verified absence closed the existing issue", &result)
			continue
		}
		if finding.IssueNumber <= 0 || (finding.Status != visualhive.StatusIssueOpen && finding.Status != visualhive.StatusFixQueued) {
			result.Errors = append(result.Errors, "issue action did not establish an open issue identity")
			continue
		}
		if !repairAuthorized {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_issue_only", "installed authority permits issue synchronization but not repair dispatch", &result)
			continue
		}
		if manualLifecycleHold {
			detail := "existing lifecycle human review hold blocks specialist repair dispatch"
			if finding.ManualReviewKind != "" {
				detail += ": " + finding.ManualReviewKind
			}
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_manual_review_held", detail, &result)
			continue
		}
		if len(policyDeniedFiles) > 0 {
			controller.persistAndAuditStage(
				ctx,
				store,
				bead,
				work,
				decision,
				"admitted_repair_policy_denied",
				"verified affected repository files are outside the installed repair allowlist: "+strings.Join(policyDeniedFiles, ", "),
				&result,
			)
			continue
		}
		if !repairReady {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_repair_held", "repair paths, base tree, or validation are incomplete", &result)
			continue
		}
		if runtimePaused {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_dispatch_runtime_held", "current normal role or installed Visual Hive service is paused", &result)
			continue
		}
		if !packet.ExpiresAt.After(controller.currentTime()) {
			controller.persistAndAuditStage(ctx, store, bead, work, decision, "admitted_dispatch_runtime_held", "verified packet composition deadline has expired", &result)
			continue
		}
		var admittedRequest governor.WorkAdmissionRequest
		if packet.ExpiresAt.IsZero() || json.Unmarshal([]byte(decision.RequestJSON), &admittedRequest) != nil {
			result.Errors = append(result.Errors, "dispatch intent requires the exact admitted request and one verified deadline")
			continue
		}
		receiptJSON, receiptErr := evidenceReceiptJSON(evidence)
		if receiptErr != nil {
			result.Errors = append(result.Errors, receiptErr.Error())
			continue
		}
		envelope := DispatchEnvelope{
			SchemaVersion: "hive.visual-dispatch-intent.v1", BeadID: bead.ID, SourceExternalRef: work.SourceExternalRef,
			Work: work, Finding: finding, Evidence: evidence, EvidenceRoot: evidenceRoot, VerificationReceiptJSON: receiptJSON,
			GovernorRequestJSON: decision.RequestJSON, GovernorRequestSHA256: decision.RequestSHA256,
			GovernorAllowed: decision.Allowed, GovernorDecisionCode: decision.Code, GovernorDecisionReason: decision.Reason,
			WorkIdentitySHA256: admittedRequest.WorkIdentitySHA256, ExecutionPolicySHA256: admittedRequest.ExecutionPolicySHA256,
			BaseSHA: packet.BaseSHA, BaseTreeSHA: baseTreeSHA, AllowedRepairPaths: append([]string(nil), allowedPaths...),
			ValidationCommands: append([]string(nil), work.ValidationCommands...), Reproduction: append([]string(nil), work.ReproductionCommands...),
			Recurrence: uint64(finding.Recurrences), Attempt: uint64(finding.RepairAttempts + 1), CompositionDeadline: packet.ExpiresAt.UTC(),
		}
		if err := controller.persistDispatch(store, bead.ID, envelope); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		persisted := store.FindByExternalRef(work.SourceExternalRef)
		persistedEnvelope, persistedOK := visualDispatchEnvelope(persisted)
		if visualBeadAdmissionState(persisted) != "admitted_dispatch_pending" || !persistedOK || !reflect.DeepEqual(persistedEnvelope, envelope) {
			result.Errors = append(result.Errors, "dispatch-pending stage was not durably persisted")
			continue
		}
		if err := controller.recordAudit(ctx, AuditEvent{Stage: "admitted_dispatch_pending", SourceExternalRef: work.SourceExternalRef, RepositoryFingerprint: work.RepositoryFingerprint, Decision: &decision, Detail: "awaiting production scheduler composition"}); err != nil {
			result.Errors = append(result.Errors, err.Error())
			continue
		}
		result.DispatchPending = append(result.DispatchPending, persistedEnvelope)
	}
	return result
}

func repairPolicyDeniedFiles(affectedFiles, allowedPatterns []string) []string {
	denied := make([]string, 0, len(affectedFiles))
	for _, candidate := range affectedFiles {
		allowed := false
		for _, pattern := range allowedPatterns {
			if repairPolicyPathAllowed(pattern, candidate) {
				allowed = true
				break
			}
		}
		if !allowed {
			denied = append(denied, candidate)
		}
	}
	sort.Strings(denied)
	return denied
}

func repairPolicyPathAllowed(pattern, candidate string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pattern)), "./")
	candidate = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(candidate)), "./")
	if pattern == "" || candidate == "" {
		return false
	}
	var expression strings.Builder
	expression.WriteString("^")
	for index := 0; index < len(pattern); {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index += 2
				if index < len(pattern) && pattern[index] == '/' {
					expression.WriteString("(?:.*/)?")
					index++
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
				index++
			}
		case '?':
			expression.WriteString("[^/]")
			index++
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
			index++
		}
	}
	expression.WriteString("$")
	matched, err := regexp.MatchString(expression.String(), candidate)
	return err == nil && matched
}

func (controller *Controller) validatePacket(packet visualhive.VerifiedPacketIdentity) error {
	if !strings.EqualFold(packet.Repository, controller.installed.Repository) || packet.RepositoryID != controller.installed.RepositoryID ||
		packet.ProducerName != "visual-hive" || packet.ProducerGitCommit != controller.installed.VisualHiveRef ||
		packet.WorkflowName != trustedWorkflowName || packet.WorkflowEvent != "workflow_dispatch" || !validSHA256(packet.PacketDigest) ||
		strings.TrimSpace(packet.SourceRef) != "refs/heads/"+strings.TrimSpace(controller.installed.DefaultBranch) ||
		(strings.TrimSpace(packet.BaseSHA) == "") || packet.ExpiresAt.IsZero() {
		return errors.New("verified packet does not match the installed repository, producer, workflow, and content identity")
	}
	return nil
}

func (controller *Controller) recordAudit(ctx context.Context, event AuditEvent) error {
	if controller.audit == nil {
		return nil
	}
	return controller.audit.RecordVisualWorkAudit(ctx, event)
}

func (controller *Controller) recordRoutingHold(ctx context.Context, packet visualhive.VerifiedPacketIdentity, routeErr error) error {
	if routeErr == nil {
		return nil
	}
	detail := routeErr.Error()
	lifecycleErr := controller.lifecycle.RecordControllerImportHold(packet, detail)
	auditErr := controller.recordAudit(ctx, AuditEvent{Stage: "held_missing_role_store", Detail: detail})
	if lifecycleErr != nil || auditErr != nil {
		return errors.Join(routeErr, lifecycleErr, auditErr)
	}
	return routeErr
}

func (controller *Controller) persistAndAuditStage(
	ctx context.Context,
	store *beads.Store,
	bead *beads.Bead,
	work visualhive.AdmittedVisualWork,
	decision governor.WorkAdmissionDecision,
	stage, detail string,
	result *Result,
) bool {
	if bead == nil || result == nil {
		return false
	}
	if err := controller.markStage(store, bead.ID, stage, detail); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return false
	}
	persisted := store.FindByExternalRef(work.SourceExternalRef)
	if persisted == nil || visualBeadAdmissionState(persisted) != stage {
		result.Errors = append(result.Errors, fmt.Sprintf("%s stage was not durably persisted", stage))
		return false
	}
	if stage == "admitted_lifecycle_synced" {
		if persisted.Status != beads.StatusClosed && persisted.Status != beads.StatusDone {
			result.Errors = append(result.Errors, "terminal lifecycle stage did not retire its normal bead")
			return false
		}
	} else if persisted.Status != beads.StatusBlocked {
		result.Errors = append(result.Errors, fmt.Sprintf("%s stage did not remain controller-held", stage))
		return false
	}
	if err := controller.recordAudit(ctx, AuditEvent{
		Stage: stage, SourceExternalRef: work.SourceExternalRef, RepositoryFingerprint: work.RepositoryFingerprint,
		Decision: &decision, Detail: detail,
	}); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return false
	}
	return true
}

func (controller *Controller) maintainManualIssueLifecycle(
	ctx context.Context,
	router *visualRoleBeadRouter,
	store *beads.Store,
	bead *beads.Bead,
	work visualhive.AdmittedVisualWork,
	packet visualhive.VerifiedPacketIdentity,
	decision governor.WorkAdmissionDecision,
	issueAuthorized bool,
	result *Result,
) {
	stage, detail := "held_manual_review", work.RoutingReason
	if !issueAuthorized {
		controller.persistAndAuditStage(ctx, store, bead, work, decision, stage, detail+": installed authority is advisory", result)
		return
	}
	if controller.issueClient == nil {
		controller.persistAndAuditStage(ctx, store, bead, work, decision, stage, detail+": normal issue writer is unavailable", result)
		return
	}
	finding, exists := controller.lifecycle.Finding(work.RepositoryFingerprint)
	if !exists {
		result.Errors = append(result.Errors, "manual-review lifecycle finding disappeared before issue synchronization")
		return
	}
	action := finding.PendingIssueAction
	if action == "" {
		if finding.Status == visualhive.StatusResolved || finding.Status == visualhive.StatusIssueClosed {
			stage = "held_manual_review_lifecycle_synced"
			detail += ": authoritative absence required no remaining issue side effect"
		}
		controller.persistAndAuditStage(ctx, store, bead, work, decision, stage, detail, result)
		return
	}
	if _, err := controller.lifecycle.QueueAdmittedIssueAction(work.RepositoryFingerprint, packet); err != nil {
		controller.persistAndAuditStage(ctx, store, bead, work, decision, stage, detail+": "+err.Error(), result)
		return
	}
	policy := integrated.PolicyForConfig(controller.installed)
	if controller.normalACMM < policy.ACMMLevel {
		policy.ACMMLevel = controller.normalACMM
	}
	outbox := visualhive.ProcessOutboxForFinding(ctx, controller.lifecycle, router, policy, controller.issueClient, work.RepositoryFingerprint)
	finding, exists = controller.lifecycle.Finding(work.RepositoryFingerprint)
	if outbox.Failed > 0 || outbox.Denied > 0 || !exists || finding.PendingIssueAction != "" {
		controller.persistAndAuditStage(ctx, store, bead, work, decision, stage, detail+": issue synchronization incomplete", result)
		return
	}
	if action == visualhive.OutboxCloseIssue {
		if finding.Status != visualhive.StatusIssueClosed {
			result.Errors = append(result.Errors, "manual-review close action did not durably close lifecycle issue")
			return
		}
		stage = "held_manual_review_lifecycle_synced"
		detail += ": verified absence closed the linked issue"
	} else {
		detail += ": linked issue lifecycle synchronized without specialist authority"
	}
	controller.persistAndAuditStage(ctx, store, bead, work, decision, stage, detail, result)
}

func (controller *Controller) holdCorruptStage(
	ctx context.Context,
	store *beads.Store,
	bead *beads.Bead,
	work visualhive.AdmittedVisualWork,
	stage, detail string,
	result *Result,
) {
	controller.persistAndAuditStage(ctx, store, bead, work, governor.WorkAdmissionDecision{}, stage, detail, result)
}

func evidenceReceiptJSON(evidence agent.SpecialistEvidenceIdentity) (string, error) {
	if evidence.VerificationReceipt == nil {
		return "", errors.New("canonical Visual Hive verification receipt is unavailable")
	}
	encoded, err := evidence.VerificationReceipt.CanonicalBytes()
	if err != nil {
		return "", fmt.Errorf("encode canonical Visual Hive verification receipt: %w", err)
	}
	if digest, err := evidence.VerificationReceipt.SHA256(); err != nil || digest != evidence.VerificationReceiptSHA256 {
		return "", errors.New("canonical Visual Hive verification receipt digest is inconsistent")
	}
	return string(encoded), nil
}

func (controller *Controller) currentTime() time.Time {
	if controller.now != nil {
		return controller.now().UTC()
	}
	return time.Now().UTC()
}

func dispatchHoldStage(finding visualhive.FindingLifecycle) string {
	if finding.HumanReviewRequired || finding.ObservationHumanReviewRequired || strings.TrimSpace(finding.ManualReviewKind) != "" {
		return "admitted_manual_review_held"
	}
	return "admitted_dispatch_runtime_held"
}

func (controller *Controller) dispatchLaunchAllowed(envelope DispatchEnvelope, finding visualhive.FindingLifecycle) error {
	if !envelope.CompositionDeadline.After(controller.currentTime()) {
		return errors.New("verified composition deadline has expired")
	}
	if err := controller.validatePacket(envelope.Work.Packet); err != nil {
		return fmt.Errorf("current installed packet policy no longer matches the dispatch intent: %w", err)
	}
	repairAuthorized := controller.installed.Automation == integrated.AutomationRepairPR || controller.installed.Automation == integrated.AutomationAutoMerge
	issueAuthorized := controller.installed.Automation == integrated.AutomationIssues || repairAuthorized
	if controller.installed.Automation != integrated.AutomationRepairPR && controller.installed.Automation != integrated.AutomationAutoMerge {
		return errors.New("installed automation authority no longer permits repair dispatch")
	}
	if _, err := visualhive.ResolveValidationCommandArgv(envelope.ValidationCommands, controller.installed.TestCommands); err != nil {
		return fmt.Errorf("current installed validation policy no longer permits repair dispatch: %w", err)
	}
	runtimePaused := controller.installed.Paused || (controller.roles != nil && controller.roles.IsPaused(envelope.Work.Role))
	if runtimePaused {
		return errors.New("normal role or installed Visual Hive service is paused")
	}
	var admitted governor.WorkAdmissionRequest
	if json.Unmarshal([]byte(envelope.GovernorRequestJSON), &admitted) != nil {
		return errors.New("durable Governor request is unavailable")
	}
	current := controller.governor.BindAdmissionRequest(admitted)
	if !validSHA256(admitted.RoleConfigSHA256) || !current.RoleConfigured || current.RoleConfigSHA256 != admitted.RoleConfigSHA256 {
		return errors.New("selected normal role is no longer configured and enabled")
	}
	// Opening the admitted issue can legitimately move the normal Governor
	// between modes before the specialist returns. The mode label itself is
	// not an execution grant: the selected role configuration and its cadence
	// are. Revalidate those exact values so a same-policy mode transition does
	// not strand an otherwise valid repair, while disabled, paused, missing,
	// or changed role policy still fails closed.
	if current.ConfiguredCadence != admitted.ConfiguredCadence || current.ConfiguredCadence == "" ||
		strings.EqualFold(current.ConfiguredCadence, "pause") || strings.EqualFold(current.ConfiguredCadence, "paused") {
		return errors.New("current Governor policy no longer enables the selected role")
	}
	if current.BudgetLimit > 0 && current.BudgetUsed >= current.BudgetLimit && !current.BudgetIgnoredForRole {
		return errors.New("current Governor budget no longer permits specialist launch")
	}
	if !validSHA256(admitted.RuntimePolicySHA256) || admitted.RuntimePolicySHA256 != visualRuntimePolicySHA256(controller.installed, controller.normalACMM) ||
		admitted.RuntimePaused != runtimePaused || admitted.AuthorityAllowsWork != issueAuthorized || admitted.WIPLimit != controller.installed.MaxActiveIssues {
		return errors.New("current installed runtime policy no longer matches the admitted policy")
	}
	policy := integrated.PolicyForConfig(controller.installed)
	if controller.normalACMM < policy.ACMMLevel {
		policy.ACMMLevel = controller.normalACMM
	}
	authority := policy.Authorize(automation.ActionRequest{
		Action: automation.ActionRepairModel, Agent: envelope.Work.Role, Repository: controller.installed.Repository,
		RepairAttempts: finding.RepairAttempts + 1,
	})
	if !authority.Allowed {
		return fmt.Errorf("current normal Hive authority denies specialist launch: %s", strings.Join(authority.Reasons, "; "))
	}
	if finding.HumanReviewRequired || finding.ObservationHumanReviewRequired || strings.TrimSpace(finding.ManualReviewKind) != "" {
		return errors.New("existing lifecycle human review hold blocks repair dispatch")
	}
	if finding.IssueNumber <= 0 || (finding.Status != visualhive.StatusIssueOpen && finding.Status != visualhive.StatusFixQueued) {
		return errors.New("existing Worker issue prerequisites are not satisfied")
	}
	paths := integrated.RepairPathsForFinding(controller.installed.AllowedRepairPaths, finding)
	if !reflect.DeepEqual(paths, envelope.AllowedRepairPaths) ||
		visualExecutionPolicySHA256(envelope.BaseTreeSHA, paths, envelope.ValidationCommands, issueAuthorized, repairAuthorized) != envelope.ExecutionPolicySHA256 {
		return errors.New("current repair policy no longer matches the durable dispatch intent")
	}
	return nil
}

func validateVisualEvidenceForPacket(packet visualhive.VerifiedPacketIdentity, evidence agent.SpecialistEvidenceIdentity) error {
	if evidence.Variant != visualhive.SpecialistEvidenceVariantVisualHiveV3 || evidence.BundleSchemaVersion != visualhive.ManifestSchemaV3 ||
		evidence.WorkflowName != trustedWorkflowName || strings.TrimSpace(evidence.WorkflowRunName) == "" || evidence.WorkflowPath != trustedWorkflowPath ||
		evidence.WorkflowEvent != "workflow_dispatch" || packet.WorkflowName != trustedWorkflowName || packet.WorkflowEvent != "workflow_dispatch" {
		return errors.New("specialist evidence does not match the installed production workflow identity")
	}
	runID, runErr := strconv.ParseUint(packet.WorkflowRunID, 10, 64)
	runAttempt, attemptErr := strconv.ParseUint(packet.WorkflowRunAttempt, 10, 64)
	artifactID, artifactErr := strconv.ParseUint(packet.WorkflowArtifactID, 10, 64)
	if runErr != nil || attemptErr != nil || artifactErr != nil || runID == 0 || runAttempt == 0 || artifactID == 0 {
		return errors.New("verified packet workflow identities are not canonical positive integers")
	}
	if evidence.BundleSHA256 != strings.ToLower(packet.PacketDigest) || evidence.RepositoryID != packet.RepositoryID ||
		evidence.WorkflowRunID != runID || evidence.WorkflowRunAttempt != runAttempt || evidence.SourceArtifactID != artifactID || evidence.ArtifactID == 0 ||
		evidence.WorkflowRunHeadSHA != strings.ToLower(packet.BaseSHA) || evidence.ArtifactIndexSHA256 != strings.ToLower(packet.ArtifactIndexSHA256) {
		return errors.New("specialist evidence does not cross-bind the verified packet identity")
	}
	receiptJSON, err := evidenceReceiptJSON(evidence)
	if err != nil || receiptJSON == "" {
		return err
	}
	receipt := evidence.VerificationReceipt
	if receipt.RepositoryID != packet.RepositoryID || receipt.WorkflowRunID != packet.WorkflowRunID || receipt.WorkflowRunAttempt != packet.WorkflowRunAttempt ||
		receipt.SourceArtifactID != packet.WorkflowArtifactID || receipt.ArtifactID != strconv.FormatUint(evidence.ArtifactID, 10) || receipt.CommitSHA != strings.ToLower(packet.BaseSHA) ||
		receipt.Event != packet.WorkflowEvent || receipt.WorkflowName != packet.WorkflowName || receipt.BundleSHA256 != strings.ToLower(packet.PacketDigest) ||
		receipt.ArtifactIndexSHA != strings.ToLower(packet.ArtifactIndexSHA256) {
		return errors.New("canonical verification receipt does not cross-bind the verified packet")
	}
	return nil
}

func visualBeadAdmissionState(bead *beads.Bead) string {
	if bead == nil || bead.Metadata == nil {
		return ""
	}
	state, _ := bead.Metadata["visual_hive_admission_state"].(string)
	return strings.TrimSpace(state)
}

func visualBeadAdmissionDecision(bead *beads.Bead) (governor.WorkAdmissionDecision, bool) {
	if bead == nil || bead.Metadata == nil {
		return governor.WorkAdmissionDecision{}, false
	}
	encoded, _ := bead.Metadata["visual_hive_admission_decision_json"].(string)
	if encoded == "" {
		return governor.WorkAdmissionDecision{}, false
	}
	var decision governor.WorkAdmissionDecision
	if json.Unmarshal([]byte(encoded), &decision) != nil {
		return governor.WorkAdmissionDecision{}, false
	}
	return decision, true
}

func visualDispatchEnvelope(bead *beads.Bead) (DispatchEnvelope, bool) {
	if bead == nil || bead.Metadata == nil {
		return DispatchEnvelope{}, false
	}
	encoded, _ := bead.Metadata["visual_hive_dispatch_envelope_json"].(string)
	var envelope DispatchEnvelope
	if encoded == "" || json.Unmarshal([]byte(encoded), &envelope) != nil || envelope.SchemaVersion != "hive.visual-dispatch-intent.v1" {
		return DispatchEnvelope{}, false
	}
	if (envelope.SpecialistWorkOrderID == "") != (envelope.SpecialistRequestSHA256 == "") ||
		(envelope.SpecialistWorkOrderID != "" && (!validSHA256(envelope.SpecialistRequestSHA256) || envelope.SpecialistRequestSHA256 != strings.ToLower(envelope.SpecialistRequestSHA256) || envelope.SpecialistWorkOrderID != "swo-"+envelope.SpecialistRequestSHA256)) {
		return DispatchEnvelope{}, false
	}
	return envelope, true
}

func visualDispatchDeferral(bead *beads.Bead) (dispatchDeferral, bool) {
	if bead == nil || bead.Metadata == nil {
		return dispatchDeferral{}, false
	}
	encoded, _ := bead.Metadata["visual_hive_dispatch_deferral_json"].(string)
	var value dispatchDeferral
	if encoded == "" || json.Unmarshal([]byte(encoded), &value) != nil || value.SchemaVersion != dispatchDeferralSchema ||
		value.SourceExternalRef == "" || value.SourceExternalRef != strings.TrimSpace(value.SourceExternalRef) ||
		value.SelectedSourceExternalRef == "" || value.SelectedSourceExternalRef != strings.TrimSpace(value.SelectedSourceExternalRef) || value.SourceExternalRef == value.SelectedSourceExternalRef ||
		value.WorkflowCorrelationSHA256 != strings.ToLower(strings.TrimSpace(value.WorkflowCorrelationSHA256)) || value.PacketDigest != strings.ToLower(strings.TrimSpace(value.PacketDigest)) ||
		!validSHA256(value.WorkflowCorrelationSHA256) || !validSHA256(value.PacketDigest) || value.Reason != dispatchDeferralReason {
		return dispatchDeferral{}, false
	}
	canonical, err := json.Marshal(value)
	if err != nil || encoded != string(canonical) {
		return dispatchDeferral{}, false
	}
	return value, true
}

func validateVisualDispatchEnvelope(
	envelope DispatchEnvelope,
	expectedBeadID string,
	packet visualhive.VerifiedPacketIdentity,
	work visualhive.AdmittedVisualWork,
	finding visualhive.FindingLifecycle,
	evidence agent.SpecialistEvidenceIdentity,
	evidenceRoot string,
	decision governor.WorkAdmissionDecision,
) error {
	if err := validateStoredAdmissionDecision(decision); err != nil || !decision.Allowed || decision.Code != "allowed" {
		return errors.New("dispatch intent has no valid durable Governor allow")
	}
	receiptJSON, err := evidenceReceiptJSON(evidence)
	if err != nil {
		return err
	}
	var admitted governor.WorkAdmissionRequest
	if json.Unmarshal([]byte(decision.RequestJSON), &admitted) != nil {
		return errors.New("dispatch intent Governor request is unreadable")
	}
	workDigest := visualWorkIdentitySHA256(work)
	policyDigest := visualExecutionPolicySHA256(envelope.BaseTreeSHA, envelope.AllowedRepairPaths, envelope.ValidationCommands, true, true)
	if envelope.SchemaVersion != "hive.visual-dispatch-intent.v1" || envelope.BeadID == "" || envelope.BeadID != expectedBeadID || envelope.SourceExternalRef != work.SourceExternalRef ||
		(envelope.SpecialistWorkOrderID == "") != (envelope.SpecialistRequestSHA256 == "") ||
		(envelope.SpecialistWorkOrderID != "" && (!validSHA256(envelope.SpecialistRequestSHA256) || envelope.SpecialistRequestSHA256 != strings.ToLower(envelope.SpecialistRequestSHA256) || envelope.SpecialistWorkOrderID != "swo-"+envelope.SpecialistRequestSHA256)) ||
		!reflect.DeepEqual(envelope.Work, work) || !visualDispatchFindingMatches(envelope.Finding, finding) ||
		envelope.WorkIdentitySHA256 == "" || envelope.WorkIdentitySHA256 != workDigest || envelope.WorkIdentitySHA256 != visualWorkIdentitySHA256(envelope.Work) ||
		envelope.Work.Packet.PacketDigest != packet.PacketDigest || envelope.Work.Packet.PacketID != packet.PacketID ||
		envelope.BaseSHA != packet.BaseSHA || !validGitObject(envelope.BaseTreeSHA) || envelope.CompositionDeadline.IsZero() || !envelope.CompositionDeadline.Equal(packet.ExpiresAt.UTC()) ||
		envelope.EvidenceRoot != evidenceRoot || !reflect.DeepEqual(envelope.Evidence, evidence) || envelope.VerificationReceiptJSON != receiptJSON ||
		envelope.GovernorRequestSHA256 != decision.RequestSHA256 || envelope.GovernorRequestJSON != decision.RequestJSON ||
		envelope.GovernorAllowed != decision.Allowed || envelope.GovernorDecisionCode != decision.Code || envelope.GovernorDecisionReason != decision.Reason ||
		envelope.ExecutionPolicySHA256 == "" || envelope.ExecutionPolicySHA256 != admitted.ExecutionPolicySHA256 || envelope.ExecutionPolicySHA256 != policyDigest ||
		!validSHA256(admitted.RoleConfigSHA256) || !validSHA256(admitted.RuntimePolicySHA256) ||
		envelope.WorkIdentitySHA256 != admitted.WorkIdentitySHA256 || !reflect.DeepEqual(envelope.ValidationCommands, work.ValidationCommands) ||
		!reflect.DeepEqual(envelope.Reproduction, work.ReproductionCommands) || len(envelope.AllowedRepairPaths) == 0 || len(envelope.ValidationCommands) == 0 ||
		envelope.Recurrence != uint64(finding.Recurrences) || envelope.Attempt != uint64(finding.RepairAttempts+1) {
		return errors.New("dispatch intent content does not match its immutable source and policy bindings")
	}
	return nil
}

func visualDispatchFindingMatches(sealed, current visualhive.FindingLifecycle) bool {
	sealed.HumanReviewRequired = current.HumanReviewRequired
	sealed.ObservationHumanReviewRequired = current.ObservationHumanReviewRequired
	sealed.ManualReviewKind = current.ManualReviewKind
	sealed.ManualReviewReason = current.ManualReviewReason
	return reflect.DeepEqual(sealed, current)
}

func visualWorkIdentitySHA256(work visualhive.AdmittedVisualWork) string {
	encoded, err := json.Marshal(work)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func visualExecutionPolicySHA256(baseTree string, allowedPaths, validation []string, issueAuthorized, repairAuthorized bool) string {
	encoded, err := json.Marshal(struct {
		BaseTree         string   `json:"base_tree"`
		AllowedPaths     []string `json:"allowed_paths"`
		Validation       []string `json:"validation"`
		IssueAuthorized  bool     `json:"issue_authorized"`
		RepairAuthorized bool     `json:"repair_authorized"`
	}{baseTree, allowedPaths, validation, issueAuthorized, repairAuthorized})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func visualRuntimePolicySHA256(installed integrated.Config, normalACMM int) string {
	encoded, err := json.Marshal(struct {
		Repository             string                   `json:"repository"`
		RepositoryID           string                   `json:"repository_id"`
		DefaultBranch          string                   `json:"default_branch"`
		Automation             integrated.Automation    `json:"automation"`
		Provider               string                   `json:"provider"`
		ProviderCommand        string                   `json:"provider_command"`
		ProviderArgs           []string                 `json:"provider_args"`
		ExecutionMode          integrated.ExecutionMode `json:"execution_mode"`
		InstalledACMM          int                      `json:"installed_acmm"`
		NormalACMM             int                      `json:"normal_acmm"`
		MaxActiveIssues        int                      `json:"max_active_issues"`
		MaxRepairAttempts      int                      `json:"max_repair_attempts"`
		Paused                 bool                     `json:"paused"`
		VisualHiveRef          string                   `json:"visual_hive_ref"`
		VisualHiveConfigDigest string                   `json:"visual_hive_config_digest"`
		AllowedRepairPaths     []string                 `json:"allowed_repair_paths"`
		AllowedAutoMergePaths  []string                 `json:"allowed_auto_merge_paths"`
		AllowedAutoMergeRisk   []automation.RiskTier    `json:"allowed_auto_merge_risk"`
	}{
		Repository: installed.Repository, RepositoryID: installed.RepositoryID, DefaultBranch: installed.DefaultBranch,
		Automation: installed.Automation, Provider: installed.Provider, ProviderCommand: installed.ProviderCommand,
		ProviderArgs: installed.ProviderArgs, ExecutionMode: installed.ExecutionMode, InstalledACMM: installed.ACMMLevel,
		NormalACMM: normalACMM, MaxActiveIssues: installed.MaxActiveIssues, MaxRepairAttempts: installed.MaxRepairAttempts,
		Paused: installed.Paused, VisualHiveRef: installed.VisualHiveRef, VisualHiveConfigDigest: installed.VisualHiveConfigDigest,
		AllowedRepairPaths: installed.AllowedRepairPaths, AllowedAutoMergePaths: installed.AllowedAutoMergePaths,
		AllowedAutoMergeRisk: installed.AllowedAutoMergeRisk,
	})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validGitObject(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sortedStoreRoles(stores map[string]*beads.Store) []string {
	roles := make([]string, 0, len(stores))
	for role := range stores {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

func cloneStringInterfaceMap(source map[string]interface{}) map[string]interface{} {
	clone := make(map[string]interface{}, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func manualHoldStoreRole(stores map[string]*beads.Store) string {
	for _, role := range []string{"quality", "scanner"} {
		if stores[role] != nil {
			return role
		}
	}
	return ""
}

func validateStoredAdmissionDecision(decision governor.WorkAdmissionDecision) error {
	if decision.Timestamp.IsZero() || decision.Timestamp.Location() != time.UTC || decision.RequestJSON == "" || decision.RequestSHA256 == "" || decision.Code == "" || decision.Reason == "" {
		return errors.New("durable Governor decision is incomplete")
	}
	var admitted governor.WorkAdmissionRequest
	if json.Unmarshal([]byte(decision.RequestJSON), &admitted) != nil || governor.AdmissionRequestSHA256(admitted) != decision.RequestSHA256 {
		return errors.New("durable Governor request bytes do not match their digest")
	}
	if admitted.SchemaVersion != "hive.visual-work-admission-request.v1" {
		return errors.New("durable Governor request schema is unsupported")
	}
	if decision.SourceExternalRef != admitted.SourceExternalRef || decision.PacketDigest != admitted.PacketDigest ||
		decision.FindingFingerprint != admitted.FindingFingerprint || decision.RepositoryFingerprint != admitted.RepositoryFingerprint ||
		decision.Role != admitted.Role || decision.ActiveWIP != admitted.ActiveWIP || decision.WIPLimit != admitted.WIPLimit ||
		!reflect.DeepEqual(decision.Evidence, admitted.Evidence) || decision.Mode != admitted.GovernorMode {
		return errors.New("durable Governor decision does not cross-bind its canonical request")
	}
	if decision.Allowed != (decision.Code == "allowed") {
		return errors.New("durable Governor allow bit and decision code disagree")
	}
	return nil
}

func visualAdmissionMatchesCurrentWork(decision governor.WorkAdmissionDecision, current governor.WorkAdmissionRequest) (matches, corrupt bool) {
	if err := validateStoredAdmissionDecision(decision); err != nil {
		return false, true
	}
	var admitted governor.WorkAdmissionRequest
	_ = json.Unmarshal([]byte(decision.RequestJSON), &admitted)
	return admitted.SourceExternalRef == current.SourceExternalRef && admitted.PacketDigest == current.PacketDigest &&
		admitted.FindingFingerprint == current.FindingFingerprint && admitted.RepositoryFingerprint == current.RepositoryFingerprint &&
		admitted.Role == current.Role && admitted.RoutingReason == current.RoutingReason && admitted.RoutingAllowed == current.RoutingAllowed &&
		admitted.WorkIdentitySHA256 == current.WorkIdentitySHA256 && admitted.ExecutionPolicySHA256 == current.ExecutionPolicySHA256 &&
		admitted.RuntimePolicySHA256 == current.RuntimePolicySHA256 &&
		reflect.DeepEqual(admitted.Evidence, current.Evidence), false
}

func visualTransientDenialNeedsReevaluation(decision governor.WorkAdmissionDecision, current governor.WorkAdmissionRequest) bool {
	if decision.Allowed {
		return false
	}
	var admitted governor.WorkAdmissionRequest
	if json.Unmarshal([]byte(decision.RequestJSON), &admitted) != nil {
		return false
	}
	switch decision.Code {
	case "role_unconfigured":
		return admitted.RoleConfigured != current.RoleConfigured || admitted.RoleConfigSHA256 != current.RoleConfigSHA256
	case "role_paused":
		return admitted.RuntimePaused != current.RuntimePaused || admitted.GovernorMode != current.GovernorMode || admitted.ConfiguredCadence != current.ConfiguredCadence
	case "wip_limit":
		return admitted.ActiveWIP != current.ActiveWIP || admitted.WIPLimit != current.WIPLimit
	case "budget_exhausted":
		return admitted.BudgetUsed != current.BudgetUsed || admitted.BudgetLimit != current.BudgetLimit || admitted.BudgetIgnoredForRole != current.BudgetIgnoredForRole
	case "execution_held":
		return admitted.SafeExecutionReady != current.SafeExecutionReady
	case "mode_unconfigured", "mode_role_unconfigured":
		return admitted.GovernorMode != current.GovernorMode || admitted.ConfiguredCadence != current.ConfiguredCadence
	default:
		return false
	}
}

func resolveInstalledBaseTree(ctx context.Context, installed integrated.Config, baseSHA string) (string, error) {
	if strings.TrimSpace(installed.CheckoutDir) == "" || strings.TrimSpace(baseSHA) == "" {
		return "", errors.New("installed checkout and base SHA are required")
	}
	output, err := exec.CommandContext(ctx, "git", "-C", installed.CheckoutDir, "rev-parse", strings.ToLower(strings.TrimSpace(baseSHA))+"^{tree}").Output()
	if err != nil {
		return "", fmt.Errorf("resolve installed base tree: %w", err)
	}
	value := strings.ToLower(strings.TrimSpace(string(output)))
	if len(value) != 40 && len(value) != 64 {
		return "", errors.New("installed base tree identity is invalid")
	}
	return value, nil
}

func recordVisualAdmissionDecision(store *beads.Store, id, state string, decision governor.WorkAdmissionDecision) error {
	if store == nil || id == "" {
		return errors.New("selected role bead is unavailable")
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	return store.Update(id, func(bead *beads.Bead) {
		bead.Status = beads.StatusBlocked
		bead.ClosedAt = nil
		if bead.Metadata == nil {
			bead.Metadata = map[string]interface{}{}
		}
		bead.Metadata["visual_hive_admission_state"] = state
		bead.Metadata["visual_hive_admission_schema"] = "hive.visual-work-admission.v1"
		bead.Metadata["visual_hive_admission_decision_json"] = string(encoded)
	})
}

func markVisualBeadStage(store *beads.Store, id, state, detail string) error {
	if store == nil || id == "" {
		return errors.New("selected role bead is unavailable")
	}
	update := func(bead *beads.Bead) {
		if state != "admitted_lifecycle_synced" && state != "held_manual_review_lifecycle_synced" {
			bead.Status = beads.StatusBlocked
			bead.ClosedAt = nil
		}
		if bead.Metadata == nil {
			bead.Metadata = map[string]interface{}{}
		}
		bead.Metadata["visual_hive_admission_state"] = state
		bead.Metadata["visual_hive_stage_detail"] = detail
		if state == "pending" {
			delete(bead.Metadata, "visual_hive_dispatch_deferral_json")
		}
	}
	if state == "admitted_lifecycle_synced" || state == "held_manual_review_lifecycle_synced" {
		return store.CloseWithUpdate(id, update)
	}
	return store.Update(id, update)
}

func persistVisualDispatchEnvelope(store *beads.Store, id string, envelope DispatchEnvelope) error {
	if store == nil || id == "" {
		return errors.New("selected role bead is unavailable")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return store.Update(id, func(bead *beads.Bead) {
		bead.Status = beads.StatusBlocked
		bead.ClosedAt = nil
		if bead.Metadata == nil {
			bead.Metadata = map[string]interface{}{}
		}
		bead.Metadata["visual_hive_admission_state"] = "admitted_dispatch_pending"
		bead.Metadata["visual_hive_stage_detail"] = "awaiting production scheduler composition"
		bead.Metadata["visual_hive_dispatch_envelope_json"] = string(encoded)
		delete(bead.Metadata, "visual_hive_dispatch_deferral_json")
	})
}

func persistVisualDispatchDeferral(store *beads.Store, id string, value dispatchDeferral) error {
	if store == nil || id == "" {
		return errors.New("selected role bead is unavailable")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return store.Update(id, func(bead *beads.Bead) {
		bead.Status = beads.StatusBlocked
		bead.ClosedAt = nil
		if bead.Metadata == nil {
			bead.Metadata = map[string]interface{}{}
		}
		bead.Metadata["visual_hive_admission_state"] = "admitted_dispatch_deferred"
		bead.Metadata["visual_hive_stage_detail"] = fmt.Sprintf("%s; selected=%s correlation=%s", value.Reason, value.SelectedSourceExternalRef, value.WorkflowCorrelationSHA256)
		bead.Metadata["visual_hive_dispatch_deferral_json"] = string(encoded)
	})
}

type visualRoleBeadRouter struct {
	stores      map[string]*beads.Store
	routes      map[string]string
	inputs      map[string]beads.BatchInput
	manualHolds map[string]bool
	err         error
}

func newVisualRoleBeadRouter(stores map[string]*beads.Store, works []visualhive.AdmittedVisualWork) *visualRoleBeadRouter {
	router := &visualRoleBeadRouter{stores: stores, routes: map[string]string{}, inputs: map[string]beads.BatchInput{}, manualHolds: map[string]bool{}}
	for _, work := range works {
		existingRole := ""
		for _, role := range router.sortedRoles() {
			if stores[role].FindByExternalRef(work.SourceExternalRef) != nil {
				if existingRole != "" {
					router.err = fmt.Errorf("Visual Hive external ref %s exists in multiple normal role stores", work.SourceExternalRef)
					return router
				}
				existingRole = role
			}
		}
		role := ""
		if work.RoutingAllowed {
			if strings.TrimSpace(work.Role) == "" || stores[work.Role] == nil {
				router.err = fmt.Errorf("Visual Hive work %s has no existing normal bead store for routed role %q", work.SourceExternalRef, work.Role)
				return router
			}
			if existingRole != "" && existingRole != work.Role {
				router.err = fmt.Errorf("Visual Hive work %s changed verified route from existing role %q to %q without an authorized role migration", work.SourceExternalRef, existingRole, work.Role)
				return router
			}
			role = work.Role
		} else {
			preferred := manualHoldStoreRole(stores)
			if existingRole != "" {
				role = existingRole
			} else {
				role = preferred
			}
			router.manualHolds[work.SourceExternalRef] = true
		}
		if role == "" {
			router.err = fmt.Errorf("Visual Hive work %s has no existing normal bead store for routed role %q", work.SourceExternalRef, work.Role)
			return router
		}
		router.routes[work.SourceExternalRef] = role
		if work.Bead.ExternalRef != "" {
			router.inputs[work.SourceExternalRef] = work.Bead
		}
	}
	return router
}

func (router *visualRoleBeadRouter) ImportBatch(inputs []beads.BatchInput) (beads.BatchResult, error) {
	groups := map[string][]beads.BatchInput{}
	result := beads.BatchResult{}
	for _, input := range inputs {
		role := router.routes[input.ExternalRef]
		expected, exists := router.inputs[input.ExternalRef]
		if role == "" || !exists {
			return beads.BatchResult{}, fmt.Errorf("Visual Hive bead %s has no sealed normal role-store route", input.ExternalRef)
		}
		manualHold := router.manualHolds[input.ExternalRef]
		if !reflect.DeepEqual(input, expected) || (!manualHold && input.Actor != role) || (manualHold && strings.TrimSpace(input.Actor) != "") || input.Status != beads.StatusBlocked {
			return beads.BatchResult{}, fmt.Errorf("Visual Hive bead %s does not match the sealed role import plan", input.ExternalRef)
		}
		routedInput := input
		if router.manualHolds[input.ExternalRef] {
			routedInput.Actor = role
			routedInput.Metadata = cloneStringInterfaceMap(input.Metadata)
			routedInput.Metadata["visual_hive_manual_hold_store_role"] = role
		}
		groups[role] = append(groups[role], routedInput)
	}
	roles := make([]string, 0, len(groups))
	for role := range groups {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	committedCreated := map[string][]string{}
	for _, role := range roles {
		createdRefs := []string{}
		for _, input := range groups[role] {
			if router.stores[role].FindByExternalRef(input.ExternalRef) == nil {
				createdRefs = append(createdRefs, input.ExternalRef)
			}
		}
		imported, err := router.stores[role].ImportBatch(groups[role])
		if err != nil {
			rollbackErrors := []string{}
			for priorIndex := len(roles) - 1; priorIndex >= 0; priorIndex-- {
				priorRole := roles[priorIndex]
				if refs := committedCreated[priorRole]; len(refs) > 0 {
					if rollbackErr := router.stores[priorRole].RollbackImportedExternalRefs(refs); rollbackErr != nil {
						rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: %v", priorRole, rollbackErr))
					}
				}
			}
			if len(rollbackErrors) > 0 {
				return beads.BatchResult{}, fmt.Errorf("%w; multi-role rollback failed: %s", err, strings.Join(rollbackErrors, "; "))
			}
			return beads.BatchResult{}, err
		}
		committedCreated[role] = createdRefs
		result.Created += imported.Created
		result.Skipped += imported.Skipped
	}
	sourceToID := map[string]string{}
	for externalRef, expected := range router.inputs {
		role := router.routes[externalRef]
		if role == "" || router.stores[role] == nil {
			continue
		}
		if bead := router.stores[role].FindByExternalRef(externalRef); bead != nil {
			sourceToID[expected.SourceID] = bead.ID
		}
	}
	type dependencySnapshot struct {
		role string
		id   string
		deps []string
	}
	updatedDependencies := []dependencySnapshot{}
	rollback := func(cause error) (beads.BatchResult, error) {
		rollbackErrors := []string{}
		for index := len(updatedDependencies) - 1; index >= 0; index-- {
			snapshot := updatedDependencies[index]
			if err := router.stores[snapshot.role].Update(snapshot.id, func(bead *beads.Bead) {
				bead.DependsOn = append([]string(nil), snapshot.deps...)
			}); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("restore %s/%s dependencies: %v", snapshot.role, snapshot.id, err))
			}
		}
		for index := len(roles) - 1; index >= 0; index-- {
			role := roles[index]
			if refs := committedCreated[role]; len(refs) > 0 {
				if err := router.stores[role].RollbackImportedExternalRefs(refs); err != nil {
					rollbackErrors = append(rollbackErrors, fmt.Sprintf("remove %s imports: %v", role, err))
				}
			}
		}
		if len(rollbackErrors) > 0 {
			return beads.BatchResult{}, fmt.Errorf("%w; multi-role rollback failed: %s", cause, strings.Join(rollbackErrors, "; "))
		}
		return beads.BatchResult{}, cause
	}
	for _, role := range roles {
		for _, input := range groups[role] {
			bead := router.stores[role].FindByExternalRef(input.ExternalRef)
			if bead == nil {
				return rollback(fmt.Errorf("imported Visual Hive bead %s disappeared", input.ExternalRef))
			}
			desired := make([]string, 0, len(input.DependsOn))
			for _, sourceID := range input.DependsOn {
				id := sourceToID[sourceID]
				if id == "" {
					return rollback(fmt.Errorf("Visual Hive dependency %s for %s was not imported", sourceID, input.ExternalRef))
				}
				desired = append(desired, id)
			}
			sort.Strings(desired)
			current := append([]string(nil), bead.DependsOn...)
			sort.Strings(current)
			if len(current) == len(desired) && (len(current) == 0 || reflect.DeepEqual(current, desired)) {
				continue
			}
			updatedDependencies = append(updatedDependencies, dependencySnapshot{role: role, id: bead.ID, deps: append([]string(nil), bead.DependsOn...)})
			if err := router.stores[role].Update(bead.ID, func(value *beads.Bead) {
				value.DependsOn = append([]string(nil), desired...)
			}); err != nil {
				return rollback(fmt.Errorf("persist Visual Hive dependencies for %s: %w", input.ExternalRef, err))
			}
		}
	}
	return result, nil
}

func (router *visualRoleBeadRouter) RoleForExternalRef(ref string) string { return router.routes[ref] }

func (router *visualRoleBeadRouter) FindByExternalRef(ref string) *beads.Bead {
	role := router.routes[ref]
	if role == "" || router.stores[role] == nil {
		return nil
	}
	return router.stores[role].FindByExternalRef(ref)
}

func (router *visualRoleBeadRouter) Update(id string, update func(*beads.Bead)) error {
	for _, role := range router.sortedRoles() {
		store := router.stores[role]
		if _, err := store.Get(id); err == nil {
			return store.Update(id, update)
		}
	}
	return fmt.Errorf("bead %s not found in routed role stores", id)
}

func (router *visualRoleBeadRouter) Close(id string) error {
	for _, role := range router.sortedRoles() {
		store := router.stores[role]
		if _, err := store.Get(id); err == nil {
			return store.Close(id)
		}
	}
	return fmt.Errorf("bead %s not found in routed role stores", id)
}

func (router *visualRoleBeadRouter) RollbackImportedExternalRefs(refs []string) error {
	grouped := map[string][]string{}
	for _, ref := range refs {
		role := router.routes[ref]
		if role == "" || router.stores[role] == nil {
			return fmt.Errorf("Visual Hive bead %s has no routed store for rollback", ref)
		}
		grouped[role] = append(grouped[role], ref)
	}
	errorsByRole := []string{}
	roles := make([]string, 0, len(grouped))
	for role := range grouped {
		roles = append(roles, role)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(roles)))
	for _, role := range roles {
		if err := router.stores[role].RollbackImportedExternalRefs(grouped[role]); err != nil {
			errorsByRole = append(errorsByRole, fmt.Sprintf("%s: %v", role, err))
		}
	}
	if len(errorsByRole) > 0 {
		return errors.New(strings.Join(errorsByRole, "; "))
	}
	return nil
}

func (router *visualRoleBeadRouter) sortedRoles() []string {
	roles := make([]string, 0, len(router.stores))
	for role := range router.stores {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

var _ visualhive.LifecycleBeadSink = (*visualRoleBeadRouter)(nil)
