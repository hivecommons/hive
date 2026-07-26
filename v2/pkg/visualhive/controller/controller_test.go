package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/governor"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/internal/visualhivepr"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

func TestVisualWorkControllerAdmitsBeforeIssueAndLeavesSchedulerDispatchPending(t *testing.T) {
	quality, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := quality.Create("ordinary work", beads.TypeTask, beads.PriorityMedium, "quality", "normal://ordinary"); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repositoryFingerprint := strings.Repeat("a", 64)
	packetDigest := strings.Repeat("b", 64)
	bundle := &visualhive.ValidatedBundle{
		Validation: visualhive.Validation{Trusted: true, Status: "passed"},
		Manifest: visualhive.Manifest{
			SchemaVersion: "visual-hive.bundle.v2", BundleID: "controller-test", OverallDigest: packetDigest,
			GeneratedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2030, 7, 9, 12, 0, 0, 0, time.UTC),
			Producer:         visualhive.Producer{Name: "visual-hive", GitCommit: strings.Repeat("c", 40)},
			Source:           visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
			ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("e", 64)},
			Observations: []visualhive.Observation{{
				Fingerprint: "finding/source", RepositoryFingerprint: repositoryFingerprint, PublicationRole: "canonical", RootCauseKey: "finding/source",
				State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality",
				Title: "verified visual regression", Body: "evidence bound", Labels: []string{"visual-hive"},
				AffectedContracts: []string{"contract/app"}, ValidationCommand: "go test ./...", FirstSeenAt: "2026-07-09T12:00:00Z",
			}},
		},
	}
	apply, err := lifecycle.ApplyBundle(bundle, quality, visualhive.ApplyLifecycleOptions{MaxActiveIssues: 2, PreferRepairable: true, DisableIssuePublication: true})
	if err != nil || apply.Created != 1 || apply.OutboxCreated != 0 || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("verified apply = %+v, %v", apply, err)
	}
	visualRef := "visual-hive://owner/repo/" + repositoryFingerprint
	visualBead := quality.FindByExternalRef(visualRef)
	if visualBead != nil {
		_ = quality.Update(visualBead.ID, func(bead *beads.Bead) {
			bead.Status = beads.StatusBlocked
			bead.Metadata["visual_hive_controller_owned"] = true
			bead.Metadata["visual_hive_admission_state"] = "pending"
		})
		visualBead = quality.FindByExternalRef(visualRef)
	}
	if visualBead == nil || visualBead.Status != beads.StatusBlocked {
		t.Fatalf("controller bead = %+v", visualBead)
	}
	ready := quality.Ready("quality")
	if len(ready) != 1 || ready[0].ExternalRef != "normal://ordinary" {
		t.Fatalf("ordinary cadence saw controller work or lost normal work: %+v", ready)
	}

	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	evidence := completeControllerEvidence(packetDigest)
	issueClient := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": quality}, nil, issueClient, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 5,
		Automation: integrated.AutomationRepairPR, MaxActiveIssues: 2, AllowedRepairPaths: []string{"src/**"}, VisualHiveRef: strings.Repeat("c", 40), TestCommands: [][]string{{"go", "test", "./..."}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	audit := &controllerAuditSink{}
	controller.SetAuditSink(audit)
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) { return strings.Repeat("f", 40), nil }
	work := visualhive.AdmittedVisualWork{
		SourceExternalRef: visualRef, FindingFingerprint: "finding/source", RepositoryFingerprint: repositoryFingerprint,
		Role: "quality", RoutingReason: "verified deterministic route", RoutingAllowed: true,
		ValidationCommands: []string{"go test ./..."},
		Bead: beads.BatchInput{SourceID: repositoryFingerprint, ExternalRef: visualRef, DependsOn: []string{}, Metadata: map[string]interface{}{
			"visual_hive_evidence_bytes": int64(314), "visual_hive_empty_keywords": []string{},
		}},
	}
	packet := completeControllerPacket("controller-test", packetDigest)
	work.Packet = packet
	_ = quality.Update(visualBead.ID, func(bead *beads.Bead) {
		bead.Metadata["visual_hive_admission_state"] = "admitted_awaiting_issue"
		delete(bead.Metadata, "visual_hive_admission_decision_json")
	})
	corrupt := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(corrupt.Decisions) != 0 || len(gov.AdmissionHistory()) != 0 || len(lifecycle.PendingOutbox()) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "held_corrupt_admission_receipt" {
		t.Fatalf("corrupt admitted state was re-admitted: %+v", corrupt)
	}
	_ = quality.Update(visualBead.ID, func(bead *beads.Bead) {
		bead.Metadata["visual_hive_admission_state"] = "denied"
		bead.Metadata["visual_hive_admission_decision_json"] = `{"allowed":true,"code":"allowed","reason":"forged"}`
	})
	corrupt = resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(corrupt.Decisions) != 0 || len(gov.AdmissionHistory()) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "held_corrupt_admission_receipt" {
		t.Fatalf("internally corrupt denial was re-admitted: %+v", corrupt)
	}
	_ = quality.Update(visualBead.ID, func(bead *beads.Bead) {
		bead.Metadata["visual_hive_admission_state"] = "pending"
		delete(bead.Metadata, "visual_hive_admission_decision_json")
	})
	controller.recordAdmission = func(*beads.Store, string, string, governor.WorkAdmissionDecision) error {
		return errors.New("injected admission persistence failure")
	}
	failed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(failed.Errors) != 1 || len(lifecycle.PendingOutbox()) != 0 || issueClient.upserts != 0 {
		t.Fatalf("admission persistence failure leaked side effects: result=%+v outbox=%+v upserts=%d", failed, lifecycle.PendingOutbox(), issueClient.upserts)
	}
	controller.recordAdmission = recordVisualAdmissionDecision
	controller.persistDispatch = func(*beads.Store, string, DispatchEnvelope) error {
		return errors.New("injected dispatch stage persistence failure")
	}
	stageFailed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(stageFailed.Decisions) != 1 || !stageFailed.Decisions[0].Allowed || len(stageFailed.DispatchPending) != 0 || len(stageFailed.Errors) != 1 || issueClient.upserts != 1 {
		t.Fatalf("dispatch stage persistence failure leaked work: %+v", stageFailed)
	}
	controller.persistDispatch = persistVisualDispatchEnvelope
	result := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(result.Decisions) != 0 || len(result.DispatchPending) != 1 {
		t.Fatalf("controller result = %+v", result)
	}
	envelope := result.DispatchPending[0]
	if envelope.BeadID != visualBead.ID || envelope.SourceExternalRef != visualRef || envelope.GovernorRequestJSON == "" || envelope.GovernorRequestSHA256 == "" ||
		envelope.WorkIdentitySHA256 == "" || envelope.ExecutionPolicySHA256 == "" || envelope.BaseSHA != packet.BaseSHA || envelope.BaseTreeSHA == "" ||
		len(envelope.AllowedRepairPaths) != 1 || len(envelope.ValidationCommands) != 1 || envelope.Evidence.VerificationReceipt == nil || envelope.VerificationReceiptJSON == "" ||
		!envelope.GovernorAllowed || envelope.GovernorDecisionCode != "allowed" || envelope.Attempt != 1 || envelope.CompositionDeadline.IsZero() ||
		envelope.SpecialistWorkOrderID != "" || envelope.SpecialistRequestSHA256 != "" || !reflect.DeepEqual(envelope.Work.Bead, beads.BatchInput{}) {
		t.Fatalf("dispatch envelope is incomplete: %+v", envelope)
	}
	admitted, canonicalReceipt, err := BuildSchedulerAdmittedWork(envelope)
	if err != nil {
		t.Fatalf("lossless Scheduler projection: %v", err)
	}
	workJSON, _ := json.Marshal(envelope.Work)
	packetJSON, _ := json.Marshal(envelope.Work.Packet)
	findingJSON, _ := json.Marshal(envelope.Finding)
	receiptJSON, receiptErr := canonicalReceipt.CanonicalReceiptJSON()
	if receiptErr != nil || string(admitted.Work) != string(workJSON) || string(admitted.Packet) != string(packetJSON) || string(admitted.Finding) != string(findingJSON) ||
		admitted.ExternalRef != envelope.SourceExternalRef || admitted.RepositoryFingerprint != envelope.Work.RepositoryFingerprint ||
		admitted.BaseSHA != envelope.BaseSHA || admitted.BaseTreeSHA != envelope.BaseTreeSHA || admitted.RoutedRole != agent.SpecialistQuality ||
		string(receiptJSON) != envelope.VerificationReceiptJSON || !reflect.DeepEqual(canonicalReceipt.EvidenceIdentity(), envelope.Evidence) {
		t.Fatalf("Scheduler projection lost intake bytes or identity: admitted=%+v receipt=%s err=%v", admitted, receiptJSON, receiptErr)
	}
	tamperedReceipt := cloneDispatchEnvelope(t, envelope)
	tamperedReceipt.VerificationReceiptJSON = `{"repository_id":"different-but-valid-json"}`
	if _, _, err := BuildSchedulerAdmittedWork(tamperedReceipt); err == nil {
		t.Fatal("Scheduler projection accepted receipt bytes different from the evidence identity")
	}
	if !issueClient.admissionSeen || issueClient.upserts != 1 {
		t.Fatalf("ordering/admission issue=%+v", issueClient)
	}
	persistedDecision, _ := visualBeadAdmissionDecision(quality.FindByExternalRef(visualRef))
	persistedFinding, _ := lifecycle.Finding(repositoryFingerprint)
	controller.installed.TestCommands = [][]string{{"npm", "run", "test"}}
	if err := controller.dispatchLaunchAllowed(envelope, persistedFinding); err == nil || !strings.Contains(err.Error(), "current installed validation policy") {
		t.Fatalf("persisted dispatch ignored changed installed validation argv: %v", err)
	}
	controller.installed.TestCommands = [][]string{{"go", "test", "./..."}}
	for name, mutate := range map[string]func(*DispatchEnvelope){
		"bead ID":       func(value *DispatchEnvelope) { value.BeadID = "copied-from-another-bead" },
		"work":          func(value *DispatchEnvelope) { value.Work.Title = "tampered" },
		"finding":       func(value *DispatchEnvelope) { value.Finding.Title = "tampered" },
		"evidence root": func(value *DispatchEnvelope) { value.EvidenceRoot = "C:/tampered" },
		"base tree":     func(value *DispatchEnvelope) { value.BaseTreeSHA = strings.Repeat("0", 40) },
		"paths":         func(value *DispatchEnvelope) { value.AllowedRepairPaths[0] = "tampered/**" },
		"validation":    func(value *DispatchEnvelope) { value.ValidationCommands[0] = "false" },
		"reproduction":  func(value *DispatchEnvelope) { value.Reproduction = []string{"false"} },
		"recurrence":    func(value *DispatchEnvelope) { value.Recurrence++ },
		"attempt":       func(value *DispatchEnvelope) { value.Attempt++ },
		"deadline":      func(value *DispatchEnvelope) { value.CompositionDeadline = value.CompositionDeadline.Add(time.Second) },
		"policy digest": func(value *DispatchEnvelope) { value.ExecutionPolicySHA256 = strings.Repeat("0", 64) },
		"request bytes": func(value *DispatchEnvelope) { value.GovernorRequestJSON += " " },
	} {
		t.Run("reject dispatch tamper "+name, func(t *testing.T) {
			changed := cloneDispatchEnvelope(t, envelope)
			mutate(&changed)
			if err := validateVisualDispatchEnvelope(changed, visualBead.ID, packet, work, persistedFinding, evidence, "C:/verified/evidence", persistedDecision); err == nil {
				t.Fatal("tampered durable dispatch intent was accepted")
			}
		})
	}
	if visualBead = quality.FindByExternalRef(visualRef); visualBead.Status != beads.StatusBlocked || visualBead.Metadata["visual_hive_admission_state"] != "admitted_dispatch_pending" {
		t.Fatalf("dispatch-pending controller bead = %+v", visualBead)
	}
	copied := cloneDispatchEnvelope(t, envelope)
	copied.BeadID = "copied-from-another-bead"
	copiedJSON, _ := json.Marshal(copied)
	_ = quality.Update(visualBead.ID, func(bead *beads.Bead) {
		bead.Metadata["visual_hive_dispatch_envelope_json"] = string(copiedJSON)
	})
	requestDigest := strings.Repeat("6", 64)
	workOrderID := "swo-" + requestDigest
	if _, err := controller.ReserveSpecialistWorkOrderIdentity(visualRef, workOrderID, requestDigest); err == nil {
		t.Fatal("reservation accepted a valid envelope copied from another bead")
	}
	if err := persistVisualDispatchEnvelope(quality, visualBead.ID, envelope); err != nil {
		t.Fatal(err)
	}
	reserved, err := controller.ReserveSpecialistWorkOrderIdentity(visualRef, workOrderID, requestDigest)
	if err != nil || reserved.SpecialistWorkOrderID != workOrderID || reserved.SpecialistRequestSHA256 != requestDigest || reserved.SourceExternalRef != visualRef {
		t.Fatalf("specialist identity reservation = %+v err=%v", reserved, err)
	}
	differentDigest := strings.Repeat("5", 64)
	if _, err := controller.ReserveSpecialistWorkOrderIdentity(visualRef, "swo-"+differentDigest, differentDigest); err == nil {
		t.Fatal("reserved specialist identity was overwritten")
	}
	historyBeforeReplay := len(gov.AdmissionHistory())
	controller.installed.Paused = true
	replayed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(replayed.Decisions) != 0 || len(replayed.DispatchPending) != 0 || issueClient.upserts != 1 || len(gov.AdmissionHistory()) != historyBeforeReplay || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "admitted_dispatch_runtime_held" {
		t.Fatalf("paused restart exposed or recomposed dispatch: result=%+v upserts=%d history=%d", replayed, issueClient.upserts, len(gov.AdmissionHistory()))
	}
	controller.installed.Paused = false
	recovered := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(recovered.Decisions) != 0 || len(recovered.DispatchPending) != 1 || recovered.DispatchPending[0].SpecialistWorkOrderID != workOrderID || len(gov.AdmissionHistory()) != historyBeforeReplay {
		t.Fatalf("unpaused restart did not resume the exact durable dispatch: %+v", recovered)
	}
	controller.installed.Automation = integrated.AutomationIssues
	downgraded := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(downgraded.DispatchPending) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "admitted_dispatch_runtime_held" {
		t.Fatalf("authority downgrade exposed pending dispatch: %+v", downgraded)
	}
	if _, err := controller.ReserveSpecialistWorkOrderIdentity(visualRef, workOrderID, requestDigest); err == nil {
		t.Fatal("reservation ignored current automation authority downgrade")
	}
	controller.installed.Automation = integrated.AutomationRepairPR
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 || resumed.DispatchPending[0].SpecialistWorkOrderID != workOrderID {
		t.Fatalf("restored repair authority did not resume exact dispatch: %+v", resumed)
	}
	gov.UpdateAgents(map[string]config.AgentConfig{"quality": {Enabled: false, Role: "quality"}})
	disabled := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(disabled.DispatchPending) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "admitted_dispatch_runtime_held" {
		t.Fatalf("disabled role exposed pending dispatch: %+v", disabled)
	}
	gov.UpdateAgents(map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}})
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 || resumed.DispatchPending[0].SpecialistWorkOrderID != workOrderID {
		t.Fatalf("restored role configuration did not resume exact dispatch: %+v", resumed)
	}
	gov.UpdateAgents(map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality", Tools: &config.ToolsConfig{Rules: []config.ToolRule{{Pattern: "go test", Action: "allow"}}}}})
	if held := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(held.DispatchPending) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "admitted_dispatch_runtime_held" {
		t.Fatalf("role capability drift reused an earlier Governor allow: %+v", held)
	}
	exactAgent := map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}
	gov.UpdateAgents(exactAgent)
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 || resumed.DispatchPending[0].SpecialistWorkOrderID != workOrderID {
		t.Fatalf("exact role capability restore did not resume durable dispatch: %+v", resumed)
	}
	governorModes := config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"quality": "1m"}}, "busy": {Cadences: map[string]string{"quality": "1m"}},
	}}
	gov.UpdateConfigAndAgents(governorModes, exactAgent)
	gov.SetMode(governor.ModeBusy)
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 {
		t.Fatalf("same-policy Governor mode transition stranded durable dispatch: %+v", resumed)
	}
	gov.SetMode(governor.ModeIdle)
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 {
		t.Fatalf("exact Governor mode restore did not resume durable dispatch: %+v", resumed)
	}
	governorModes.Modes["idle"] = config.ModeConfig{Cadences: map[string]string{"quality": "2m"}}
	gov.UpdateConfigAndAgents(governorModes, exactAgent)
	if held := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(held.DispatchPending) != 0 {
		t.Fatalf("Governor cadence drift reused an earlier allow: %+v", held)
	}
	governorModes.Modes["idle"] = config.ModeConfig{Cadences: map[string]string{"quality": "1m"}}
	gov.UpdateConfigAndAgents(governorModes, exactAgent)
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 {
		t.Fatalf("exact Governor cadence restore did not resume durable dispatch: %+v", resumed)
	}
	controller.normalACMM = 4
	if held := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(held.DispatchPending) != 0 {
		t.Fatalf("live normal ACMM drift reused an earlier policy digest: %+v", held)
	}
	controller.normalACMM = 5
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 {
		t.Fatalf("exact normal ACMM restore did not resume durable dispatch: %+v", resumed)
	}
	controller.installed.AllowedRepairPaths = []string{"other/**"}
	if held := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(held.DispatchPending) != 0 {
		t.Fatalf("installed path-policy drift reused an earlier policy digest: %+v", held)
	}
	controller.installed.AllowedRepairPaths = []string{"src/**"}
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 {
		t.Fatalf("exact installed path-policy restore did not resume durable dispatch: %+v", resumed)
	}
	if err := lifecycle.MarkManualReviewRequired(repositoryFingerprint, "baseline_review", "human verification required"); err != nil {
		t.Fatal(err)
	}
	manualHeld := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(manualHeld.DispatchPending) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "admitted_manual_review_held" || !audit.saw("admitted_manual_review_held") {
		t.Fatalf("lifecycle human hold was overridden by prior Governor allow: %+v", manualHeld)
	}
	if err := lifecycle.MarkManualReviewComplete(repositoryFingerprint, "baseline_review"); err != nil {
		t.Fatal(err)
	}
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 || resumed.DispatchPending[0].SpecialistWorkOrderID != workOrderID {
		t.Fatalf("authorized manual-review completion did not resume exact dispatch: %+v", resumed)
	}
	controller.now = func() time.Time { return packet.ExpiresAt.Add(time.Second) }
	expired := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(expired.DispatchPending) != 0 || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "admitted_dispatch_runtime_held" {
		t.Fatalf("expired composition deadline exposed pending dispatch: %+v", expired)
	}
	controller.now = func() time.Time { return packet.ExpiresAt.Add(-time.Second) }
	if resumed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply}); len(resumed.DispatchPending) != 1 || resumed.DispatchPending[0].SpecialistWorkOrderID != workOrderID {
		t.Fatalf("valid deadline did not resume exact dispatch: %+v", resumed)
	}
	if !audit.saw("admitted_awaiting_issue") || !audit.saw("admitted_dispatch_pending") {
		t.Fatalf("normal audit sink missed Governor/stage events: %+v", audit.events)
	}
	for name, mutate := range map[string]func(*DispatchEnvelope){
		"operational path": func(value *DispatchEnvelope) { value.AllowedRepairPaths[0] = "tampered/**" },
		"specialist ID relation": func(value *DispatchEnvelope) {
			value.SpecialistWorkOrderID = "swo-" + strings.Repeat("7", 64)
		},
	} {
		t.Run("persisted tamper "+name, func(t *testing.T) {
			if err := persistVisualDispatchEnvelope(quality, visualBead.ID, reserved); err != nil {
				t.Fatal(err)
			}
			tamperedEnvelope := cloneDispatchEnvelope(t, reserved)
			mutate(&tamperedEnvelope)
			tamperedJSON, _ := json.Marshal(tamperedEnvelope)
			_ = quality.Update(visualBead.ID, func(bead *beads.Bead) {
				bead.Metadata["visual_hive_dispatch_envelope_json"] = string(tamperedJSON)
			})
			tampered := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{evidence}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
			if len(tampered.Decisions) != 0 || len(tampered.DispatchPending) != 0 || len(gov.AdmissionHistory()) != historyBeforeReplay || visualBeadAdmissionState(quality.FindByExternalRef(visualRef)) != "held_corrupt_dispatch_intent" {
				t.Fatalf("tampered persisted dispatch intent was returned or re-admitted: %+v", tampered)
			}
		})
	}
	if err := persistVisualDispatchEnvelope(quality, visualBead.ID, reserved); err != nil {
		t.Fatal(err)
	}
	branch := "hive/repair-controller-a1"
	commitSHA := strings.Repeat("9", 40)
	prURL := "https://example.invalid/pull/37"
	if err := lifecycle.MarkRepairStarted(repositoryFingerprint, branch); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.MarkPROpen(repositoryFingerprint, commitSHA, 37, prURL); err != nil {
		t.Fatal(err)
	}
	unsealedCompletion := SpecialistPullRequestCompletion{
		WorkOrderID: workOrderID, RequestSHA256: requestDigest, Branch: branch, CommitSHA: commitSHA,
		PullRequestNumber: 37, PullRequestURL: prURL, VerdictHeadSHA: commitSHA,
		VerdictReceiptSHA256: strings.Repeat("1", 64), VerdictStatus: "success",
	}
	if err := controller.CompleteSpecialistPullRequest(visualRef, unsealedCompletion); err == nil {
		t.Fatal("controller accepted caller-asserted verdict before opaque check evidence was applied")
	}
	checkReceipt, sealedReceipt := controllerPullRequestCheckReceipt(t, reserved, commitSHA, branch, 37)
	if _, err := lifecycle.ApplySealedPullRequestCheckReceipt(repositoryFingerprint, sealedReceipt); err != nil {
		t.Fatalf("apply exact check evidence before controller completion: %v", err)
	}
	completion := SpecialistPullRequestCompletion{
		WorkOrderID: workOrderID, RequestSHA256: requestDigest, Branch: branch, CommitSHA: commitSHA,
		PullRequestNumber: 37, PullRequestURL: prURL, VerdictHeadSHA: commitSHA,
		VerdictReceiptSHA256: checkReceipt.ReceiptSHA256, VerdictStatus: "success",
	}
	audit.failStage = "admitted_pr_verdict_recorded"
	if err := controller.CompleteSpecialistPullRequest(visualRef, completion); err == nil {
		t.Fatal("completion audit failure was not returned for retry")
	}
	completedBead := quality.FindByExternalRef(visualRef)
	if completedBead.Status != beads.StatusClosed || visualBeadAdmissionState(completedBead) != "admitted_pr_verdict_recorded" || completedBead.Meta("visual_hive_pr_completion_json") == "" ||
		completedBead.Meta("visual_hive_completion_audit_id") == "" || completedBead.Metadata["visual_hive_completion_audit_recorded"] != false {
		t.Fatalf("completed specialist bead = %+v", completedBead)
	}
	if err := controller.CompleteSpecialistPullRequest(visualRef, completion); err != nil {
		t.Fatalf("completion audit retry was not recovered: %v", err)
	}
	completedBead = quality.FindByExternalRef(visualRef)
	if completedBead.Metadata["visual_hive_completion_audit_recorded"] != true || audit.count("admitted_pr_verdict_recorded") != 1 {
		t.Fatalf("completion audit acknowledgement/events = metadata=%+v events=%+v", completedBead.Metadata, audit.events)
	}
	completionEventID := completedBead.Meta("visual_hive_completion_audit_id")
	if err := controller.CompleteSpecialistPullRequest(visualRef, completion); err != nil {
		t.Fatalf("ambiguous completion replay was not idempotent: %v", err)
	}
	if audit.count("admitted_pr_verdict_recorded") != 1 || completionEventID == "" || audit.events[len(audit.events)-1].EventID != completionEventID {
		t.Fatalf("completion replay duplicated or lost its stable audit identity: metadata=%+v events=%+v", completedBead.Metadata, audit.events)
	}
	completionJSON, _ := json.Marshal(completion)
	crossRepositoryEnvelope := cloneDispatchEnvelope(t, reserved)
	crossRepositoryEnvelope.Work.RepositoryFingerprint = strings.Repeat("4", 64)
	crossRepositoryJSON, _ := json.Marshal(crossRepositoryEnvelope)
	crossRepositoryBead := &beads.Bead{Metadata: map[string]interface{}{"visual_hive_dispatch_envelope_json": string(crossRepositoryJSON)}}
	crossRepositoryEvent, err := specialistCompletionAuditEvent(crossRepositoryBead, visualRef, completion, completionJSON)
	if err != nil || crossRepositoryEvent.EventID == completionEventID {
		t.Fatalf("completion audit identity was not repository-fingerprint-bound: event=%+v err=%v", crossRepositoryEvent, err)
	}
	different := completion
	different.VerdictStatus = "pass"
	if err := controller.CompleteSpecialistPullRequest(visualRef, different); err == nil {
		t.Fatal("completed specialist PR accepted a different replay receipt")
	}
}

func TestVisualWorkControllerIssueOnlyPersistsStageWithoutDispatch(t *testing.T) {
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, digest := strings.Repeat("3", 64), strings.Repeat("8", 64)
	bundle := &visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true}, Manifest: visualhive.Manifest{
		BundleID: "issue-only", OverallDigest: digest, Source: visualhive.Source{Repository: "owner/repo", RepositoryID: "123", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
		ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("9", 64)},
		Observations:     []visualhive.Observation{{Fingerprint: "issue-only/source", RepositoryFingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: "issue-only/source", State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality", Title: "issue only", Body: "issue only", AffectedContracts: []string{"contract/app"}, ValidationCommand: "go test ./...", FirstSeenAt: "2026-07-09T12:00:00Z"}},
	}}
	apply, err := lifecycle.ApplyBundle(bundle, store, visualhive.ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 2})
	if err != nil {
		t.Fatal(err)
	}
	externalRef := "visual-hive://owner/repo/" + fingerprint
	bead := store.FindByExternalRef(externalRef)
	_ = store.Update(bead.ID, func(value *beads.Bead) {
		value.Status = beads.StatusBlocked
		value.Metadata["visual_hive_controller_owned"] = true
		value.Metadata["visual_hive_admission_state"] = "pending"
	})
	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]string{"quality": "1m"}}}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": store}, nil, issues, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 4, Automation: integrated.AutomationIssues,
		MaxActiveIssues: 2, AllowedRepairPaths: []string{"src/**"}, VisualHiveRef: strings.Repeat("c", 40),
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	audit := &controllerAuditSink{}
	controller.SetAuditSink(audit)
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) { return strings.Repeat("f", 40), nil }
	packet := completeControllerPacket("issue-only", digest)
	work := visualhive.AdmittedVisualWork{SourceExternalRef: externalRef, Packet: packet, FindingFingerprint: "issue-only/source", RepositoryFingerprint: fingerprint, ObservationState: "present", Role: "quality", RoutingAllowed: true, RoutingReason: "verified route", ValidationCommands: []string{"go test ./..."}}
	controller.markStage = func(*beads.Store, string, string, string) error {
		return errors.New("injected issue-only stage persistence failure")
	}
	failed := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(failed.DispatchPending) != 0 || len(failed.Errors) == 0 || issues.upserts != 1 || visualBeadAdmissionState(store.FindByExternalRef(externalRef)) != "admitted_awaiting_issue" {
		t.Fatalf("issue-only persistence failure was exposed as success: %+v bead=%+v", failed, store.FindByExternalRef(externalRef))
	}
	controller.markStage = markVisualBeadStage
	result := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(result.Decisions) != 0 || len(result.DispatchPending) != 0 || len(result.Errors) != 0 || issues.upserts != 1 || visualBeadAdmissionState(store.FindByExternalRef(externalRef)) != "admitted_issue_only" || !audit.saw("admitted_issue_only") {
		t.Fatalf("issue-only result = %+v bead=%+v audit=%+v", result, store.FindByExternalRef(externalRef), audit.events)
	}
}

func TestVisualWorkControllerHeldRepairReevaluatesWithoutCountingItselfAsWIP(t *testing.T) {
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("6", 64)
	digest := strings.Repeat("7", 64)
	bundle := &visualhive.ValidatedBundle{
		Validation: visualhive.Validation{Trusted: true, Status: "failed"},
		Manifest: visualhive.Manifest{
			SchemaVersion: "visual-hive.bundle.v2", BundleID: "held-repair", OverallDigest: digest,
			GeneratedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2030, 7, 9, 12, 0, 0, 0, time.UTC),
			Producer:         visualhive.Producer{Name: "visual-hive", GitCommit: strings.Repeat("c", 40)},
			Source:           visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
			ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("e", 64)},
			Observations: []visualhive.Observation{{
				Fingerprint: "selector/source", RepositoryFingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: "selector/source",
				State: "present", IssueKind: "selector_contract_failure", Severity: "high", OwningAgentHint: "visual-hive/test-maintainer",
				Title: "selector contract failed", Body: "deterministic content mismatch", Labels: []string{"visual-hive"},
				AffectedContracts: []string{"app-shell-content-health"}, ValidationCommand: "visual-hive run --ci", FirstSeenAt: "2026-07-09T12:00:00Z",
			}},
		},
	}
	apply, err := lifecycle.ApplyBundle(bundle, store, visualhive.ApplyLifecycleOptions{MaxActiveIssues: 1, PreferRepairable: true, DisableIssuePublication: true})
	if err != nil || apply.Created != 1 {
		t.Fatalf("apply held-repair fixture = %+v, %v", apply, err)
	}
	externalRef := "visual-hive://owner/repo/" + fingerprint
	bead := store.FindByExternalRef(externalRef)
	if bead == nil {
		t.Fatal("held-repair fixture has no controller bead")
	}
	if err := store.Update(bead.ID, func(value *beads.Bead) {
		value.Status = beads.StatusBlocked
		value.Metadata["visual_hive_controller_owned"] = true
		value.Metadata["visual_hive_admission_state"] = "pending"
	}); err != nil {
		t.Fatal(err)
	}

	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": store}, nil, issues, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 5,
		Automation: integrated.AutomationRepairPR, MaxActiveIssues: 1, AllowedRepairPaths: []string{"scripts/testing/**"}, VisualHiveRef: strings.Repeat("c", 40),
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	treeAvailable := false
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) {
		if !treeAvailable {
			return "", errors.New("verified base is not yet fetched")
		}
		return strings.Repeat("f", 40), nil
	}
	packet := completeControllerPacket("held-repair", digest)
	work := visualhive.AdmittedVisualWork{
		SourceExternalRef: externalRef, FindingFingerprint: "selector/source", RepositoryFingerprint: fingerprint,
		Role: "quality", RoutingReason: "verified selector contract route", RoutingAllowed: true,
		ValidationCommands: []string{"visual-hive run --ci"}, Packet: packet,
	}
	evidence := controllerEvidenceSource{completeControllerEvidence(digest)}
	first := resumeAppliedForTest(controller, context.Background(), evidence, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(first.Decisions) != 1 || !first.Decisions[0].Allowed || first.Decisions[0].ActiveWIP != 0 || len(first.DispatchPending) != 0 ||
		issues.upserts != 1 || visualBeadAdmissionState(store.FindByExternalRef(externalRef)) != "admitted_repair_held" {
		t.Fatalf("missing base was not durably held after admission: result=%+v upserts=%d bead=%+v", first, issues.upserts, store.FindByExternalRef(externalRef))
	}

	treeAvailable = true
	second := resumeAppliedForTest(controller, context.Background(), evidence, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(second.Decisions) != 1 || !second.Decisions[0].Allowed || second.Decisions[0].ActiveWIP != 0 || len(second.DispatchPending) != 1 ||
		issues.upserts != 1 || visualBeadAdmissionState(store.FindByExternalRef(externalRef)) != "admitted_dispatch_pending" {
		t.Fatalf("held repair counted itself against WIP during recovery: result=%+v upserts=%d bead=%+v", second, issues.upserts, store.FindByExternalRef(externalRef))
	}
	if len(gov.AdmissionHistory()) != 2 || second.DispatchPending[0].BaseTreeSHA != strings.Repeat("f", 40) ||
		!reflect.DeepEqual(second.DispatchPending[0].AllowedRepairPaths, []string{"scripts/testing/**"}) ||
		!reflect.DeepEqual(second.DispatchPending[0].ValidationCommands, []string{"visual-hive run --ci"}) {
		t.Fatalf("recovered dispatch lost its exact policy: %+v history=%+v", second.DispatchPending, gov.AdmissionHistory())
	}
}

func TestVisualWorkControllerSynchronizesIssueAndDeniesVerifiedOutOfPolicyRepairFiles(t *testing.T) {
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("4", 64)
	digest := strings.Repeat("5", 64)
	bundle := &visualhive.ValidatedBundle{
		Validation: visualhive.Validation{Trusted: true, Status: "failed"},
		Manifest: visualhive.Manifest{
			SchemaVersion: "visual-hive.bundle.v2", BundleID: "policy-denied-repair", OverallDigest: digest,
			GeneratedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2030, 7, 9, 12, 0, 0, 0, time.UTC),
			Producer:         visualhive.Producer{Name: "visual-hive", GitCommit: strings.Repeat("c", 40)},
			Source:           visualhive.Source{Repository: "owner/repo", RepositoryID: "123", Ref: "refs/heads/main", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
			ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("e", 64)},
			Observations: []visualhive.Observation{{
				Fingerprint: "storybook/source", RepositoryFingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: "storybook/source",
				State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality",
				Title: "storybook accessibility failed", Body: "deterministic accessibility mismatch", Labels: []string{"visual-hive"},
				AffectedContracts: []string{"component-lab-storybook"}, AffectedFiles: []string{".storybook/preview.ts"},
				ValidationCommand: "visual-hive run --ci", FirstSeenAt: "2026-07-09T12:00:00Z",
			}},
		},
	}
	apply, err := lifecycle.ApplyBundle(bundle, store, visualhive.ApplyLifecycleOptions{MaxActiveIssues: 1, PreferRepairable: true, DisableIssuePublication: true})
	if err != nil || apply.Created != 1 {
		t.Fatalf("apply policy-denied fixture = %+v, %v", apply, err)
	}
	externalRef := "visual-hive://owner/repo/" + fingerprint
	bead := store.FindByExternalRef(externalRef)
	if bead == nil {
		t.Fatal("policy-denied fixture has no controller bead")
	}
	if err := store.Update(bead.ID, func(value *beads.Bead) {
		value.Status = beads.StatusBlocked
		value.Metadata["visual_hive_controller_owned"] = true
		value.Metadata["visual_hive_admission_state"] = "pending"
	}); err != nil {
		t.Fatal(err)
	}

	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": store}, nil, issues, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 5,
		Automation: integrated.AutomationRepairPR, MaxActiveIssues: 1, AllowedRepairPaths: []string{"src/**"},
		VisualHiveRef: strings.Repeat("c", 40), TestCommands: [][]string{{"go", "test", "./..."}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) { return strings.Repeat("f", 40), nil }
	audit := &controllerAuditSink{}
	controller.SetAuditSink(audit)
	packet := completeControllerPacket("policy-denied-repair", digest)
	work := visualhive.AdmittedVisualWork{
		SourceExternalRef: externalRef, FindingFingerprint: "storybook/source", RepositoryFingerprint: fingerprint,
		Role: "quality", RoutingReason: "verified Storybook contract route", RoutingAllowed: true,
		AffectedContracts: []string{"component-lab-storybook"}, AffectedFiles: []string{".storybook/preview.ts"},
		ValidationCommands: []string{"visual-hive run --ci"}, Packet: packet,
	}
	evidence := controllerEvidenceSource{completeControllerEvidence(digest)}
	first := resumeAppliedForTest(controller, context.Background(), evidence, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	persisted := store.FindByExternalRef(externalRef)
	if len(first.Decisions) != 1 || !first.Decisions[0].Allowed || len(first.DispatchPending) != 0 || len(first.Errors) != 0 ||
		issues.upserts != 1 || visualBeadAdmissionState(persisted) != "admitted_repair_policy_denied" || !audit.saw("admitted_repair_policy_denied") {
		t.Fatalf("out-of-policy repair was not issue-synchronized and denied: result=%+v bead=%+v audit=%+v", first, persisted, audit.events)
	}
	detail, _ := persisted.Metadata["visual_hive_stage_detail"].(string)
	if !strings.Contains(detail, ".storybook/preview.ts") {
		t.Fatalf("policy denial omitted the exact affected file: %q", detail)
	}

	replay := resumeAppliedForTest(controller, context.Background(), evidence, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(replay.Decisions) != 0 || len(replay.DispatchPending) != 0 || len(replay.Errors) != 0 || issues.upserts != 1 ||
		visualBeadAdmissionState(store.FindByExternalRef(externalRef)) != "admitted_repair_policy_denied" {
		t.Fatalf("policy-denied replay duplicated lifecycle work: result=%+v upserts=%d", replay, issues.upserts)
	}
}

func TestRepairPolicyDeniedFilesUsesWorkerDoubleStarSemantics(t *testing.T) {
	allowed := []string{"src/**", "**/src/**", "**/*.test.*", "**/*_test.go"}
	files := []string{
		"src/App.tsx",
		"packages/web/src/App.tsx",
		"packages/web/src/App.test.tsx",
		"packages/api/handler_test.go",
		".storybook/preview.ts",
	}
	if denied := repairPolicyDeniedFiles(files, allowed); !reflect.DeepEqual(denied, []string{".storybook/preview.ts"}) {
		t.Fatalf("controller repair policy diverged from Worker glob semantics: %v", denied)
	}
}

func TestVisualWorkControllerHoldsUninstalledProducerRecipeBeforeIssue(t *testing.T) {
	const recipe = "visual-hive improve-coverage && visual-hive issues --write"
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, digest := strings.Repeat("4", 64), strings.Repeat("8", 64)
	bundle := &visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true}, Manifest: visualhive.Manifest{
		BundleID: "unsupported-validation-recipe", OverallDigest: digest,
		Source:           visualhive.Source{Repository: "owner/repo", RepositoryID: "123", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
		ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("9", 64)},
		Observations: []visualhive.Observation{{
			Fingerprint: "coverage/source", RepositoryFingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: "coverage/source",
			State: "present", IssueKind: "missing_visual_coverage", Severity: "medium", OwningAgentHint: "visual-hive/test-creator",
			Title: "improve deterministic coverage", Body: "producer advisory", AffectedContracts: []string{"contract/app"},
			ValidationCommand: recipe, FirstSeenAt: "2026-07-09T12:00:00Z",
		}},
	}}
	apply, err := lifecycle.ApplyBundle(bundle, store, visualhive.ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 1, PreferRepairable: true})
	if err != nil || apply.Created != 1 {
		t.Fatalf("apply unsupported recipe = %+v, %v", apply, err)
	}
	externalRef := "visual-hive://owner/repo/" + fingerprint
	bead := store.FindByExternalRef(externalRef)
	if bead == nil {
		t.Fatal("unsupported recipe has no durable controller bead")
	}
	if err := store.Update(bead.ID, func(value *beads.Bead) {
		value.Status = beads.StatusBlocked
		value.Metadata["visual_hive_controller_owned"] = true
		value.Metadata["visual_hive_admission_state"] = "pending"
	}); err != nil {
		t.Fatal(err)
	}

	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": store}, nil, issues, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 5,
		Automation: integrated.AutomationRepairPR, MaxActiveIssues: 1, AllowedRepairPaths: []string{"src/**"},
		VisualHiveRef: strings.Repeat("c", 40), TestCommands: [][]string{{"npm", "run", "test"}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) { return strings.Repeat("f", 40), nil }
	packet := completeControllerPacket("unsupported-validation-recipe", digest)
	work := visualhive.AdmittedVisualWork{
		SourceExternalRef: externalRef, Packet: packet, FindingFingerprint: "coverage/source", RepositoryFingerprint: fingerprint,
		ObservationState: "present", Role: "quality", RoutingAllowed: true, RoutingReason: "verified route",
		ValidationCommands: []string{recipe}, ReproductionCommands: []string{recipe},
	}
	source := controllerEvidenceSource{completeControllerEvidence(digest)}
	result := resumeAppliedForTest(controller, context.Background(), source, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	persisted := store.FindByExternalRef(externalRef)
	finding, _ := lifecycle.Finding(fingerprint)
	if persisted == nil {
		t.Fatal("unsupported recipe lost its durable controller bead")
	}
	metadataValidationJSON, _ := json.Marshal(persisted.Metadata["visual_hive_validation_commands"])
	var metadataValidation []string
	if err := json.Unmarshal(metadataValidationJSON, &metadataValidation); err != nil {
		t.Fatalf("decode persisted validation evidence: %v", err)
	}
	if len(result.Decisions) != 1 || result.Decisions[0].Allowed || result.Decisions[0].Code != "execution_held" ||
		len(result.DispatchPending) != 0 || issues.upserts != 0 || persisted.Status != beads.StatusBlocked ||
		visualBeadAdmissionState(persisted) != "denied" || finding.ValidationCommand != recipe ||
		!reflect.DeepEqual(work.ReproductionCommands, []string{recipe}) || !reflect.DeepEqual(metadataValidation, []string{recipe}) {
		t.Fatalf("unsupported recipe crossed admission: result=%+v upserts=%d bead=%+v finding=%+v", result, issues.upserts, persisted, finding)
	}
	replay := resumeAppliedForTest(controller, context.Background(), source, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(replay.Decisions) != 0 || len(replay.DispatchPending) != 0 || issues.upserts != 0 || len(gov.AdmissionHistory()) != 1 || store.Count() != 1 {
		t.Fatalf("unsupported recipe replay duplicated work: result=%+v upserts=%d history=%+v beads=%d", replay, issues.upserts, gov.AdmissionHistory(), store.Count())
	}
}

func TestVisualWorkControllerDenialCreatesNoIssueIntent(t *testing.T) {
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("7", 64)
	digest := strings.Repeat("8", 64)
	bundle := &visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true}, Manifest: visualhive.Manifest{
		BundleID: "denied-packet", OverallDigest: digest, Source: visualhive.Source{Repository: "owner/repo", RepositoryID: "123", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
		ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("9", 64)},
		Observations:     []visualhive.Observation{{Fingerprint: "denied/source", RepositoryFingerprint: fingerprint, PublicationRole: "canonical", RootCauseKey: "denied/source", State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality", Title: "held", Body: "held", AffectedContracts: []string{"contract/app"}, ValidationCommand: "go test ./...", FirstSeenAt: "2026-07-09T12:00:00Z"}},
	}}
	apply, err := lifecycle.ApplyBundle(bundle, store, visualhive.ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 1})
	if err != nil || len(lifecycle.PendingOutbox()) != 0 {
		t.Fatalf("held apply = %+v err=%v outbox=%+v", apply, err, lifecycle.PendingOutbox())
	}
	heldRef := "visual-hive://owner/repo/" + fingerprint
	if held := store.FindByExternalRef(heldRef); held != nil {
		_ = store.Update(held.ID, func(bead *beads.Bead) {
			bead.Status = beads.StatusBlocked
			bead.Metadata["visual_hive_controller_owned"] = true
			bead.Metadata["visual_hive_admission_state"] = "pending"
		})
	}
	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]string{"quality": "1m"}}}}, map[string]config.AgentConfig{"quality": {Enabled: false, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": store}, nil, issues, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 5, Automation: integrated.AutomationRepairPR,
		MaxActiveIssues: 1, AllowedRepairPaths: []string{"src/**"}, VisualHiveRef: strings.Repeat("c", 40), TestCommands: [][]string{{"go", "test", "./..."}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) { return strings.Repeat("f", 40), nil }
	work := visualhive.AdmittedVisualWork{SourceExternalRef: heldRef, FindingFingerprint: "denied/source", RepositoryFingerprint: fingerprint, Role: "quality", RoutingAllowed: true, RoutingReason: "verified route", ValidationCommands: []string{"go test ./..."}}
	packet := completeControllerPacket("denied-packet", digest)
	work.Packet = packet
	result := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(result.Decisions) != 1 || result.Decisions[0].Allowed || len(lifecycle.PendingOutbox()) != 0 || issues.upserts != 0 {
		t.Fatalf("denial leaked issue intent: result=%+v outbox=%+v upserts=%d", result, lifecycle.PendingOutbox(), issues.upserts)
	}
	bead := store.FindByExternalRef(work.SourceExternalRef)
	decision, recorded := visualBeadAdmissionDecision(bead)
	if !recorded || decision.Code != result.Decisions[0].Code || visualBeadAdmissionState(bead) != "denied" {
		t.Fatalf("durable denial receipt = %+v recorded=%t bead=%+v", decision, recorded, bead)
	}
	replay := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	replayedDecision, replayedRecorded := visualBeadAdmissionDecision(store.FindByExternalRef(work.SourceExternalRef))
	if len(replay.Decisions) != 0 || len(gov.AdmissionHistory()) != 1 || !replayedRecorded || !reflect.DeepEqual(replayedDecision, decision) {
		t.Fatalf("exact denied replay changed the Governor receipt: result=%+v decision=%+v", replay, replayedDecision)
	}
	gov.UpdateAgents(map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}})
	recovered := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, []visualhive.AdmittedVisualWork{work}, Result{Lifecycle: apply})
	if len(recovered.Decisions) != 1 || !recovered.Decisions[0].Allowed || issues.upserts != 1 || len(recovered.DispatchPending) != 1 {
		t.Fatalf("corrected role config did not deterministically re-evaluate the transient denial: %+v upserts=%d", recovered, issues.upserts)
	}
}

func TestTransientAdmissionDenialsReevaluateOnlyWhenTheirLiveInputsChange(t *testing.T) {
	newGovernor := func() *governor.Governor {
		return governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{"quality": "1m"}},
		}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	baseRequest := func() governor.WorkAdmissionRequest {
		return governor.WorkAdmissionRequest{
			SourceExternalRef: "visual-hive://owner/repo/finding", PacketDigest: strings.Repeat("1", 64),
			FindingFingerprint: "finding", RepositoryFingerprint: strings.Repeat("2", 64), Role: "quality",
			RoutingAllowed: true, RoutingReason: "verified route", AuthorityAllowsWork: true, SafeExecutionReady: true,
			ActiveWIP: 0, WIPLimit: 2, WorkIdentitySHA256: strings.Repeat("3", 64), ExecutionPolicySHA256: strings.Repeat("4", 64),
		}
	}
	assertReevaluatesToAllow := func(t *testing.T, gov *governor.Governor, denied governor.WorkAdmissionDecision, current governor.WorkAdmissionRequest) {
		t.Helper()
		bound := gov.BindAdmissionRequest(current)
		matches, corrupt := visualAdmissionMatchesCurrentWork(denied, current)
		if corrupt || !matches || !visualTransientDenialNeedsReevaluation(denied, bound) {
			t.Fatalf("transient denial was not eligible for re-evaluation: denied=%+v bound=%+v", denied, bound)
		}
		if allowed := gov.AdmitWork(current); !allowed.Allowed {
			t.Fatalf("cleared transient input remained denied: %+v", allowed)
		}
	}
	t.Run("pause", func(t *testing.T) {
		gov, request := newGovernor(), baseRequest()
		request.RuntimePaused = true
		denied := gov.AdmitWork(request)
		request.RuntimePaused = false
		assertReevaluatesToAllow(t, gov, denied, request)
	})
	t.Run("wip", func(t *testing.T) {
		gov, request := newGovernor(), baseRequest()
		request.ActiveWIP, request.WIPLimit = 1, 1
		denied := gov.AdmitWork(request)
		request.ActiveWIP = 0
		assertReevaluatesToAllow(t, gov, denied, request)
	})
	t.Run("budget", func(t *testing.T) {
		gov, request := newGovernor(), baseRequest()
		gov.SetBudgetLimit(1)
		gov.SeedBudget(1, nil, nil, time.Now().UTC())
		denied := gov.AdmitWork(request)
		gov.SeedBudget(0, nil, nil, time.Now().UTC())
		assertReevaluatesToAllow(t, gov, denied, request)
	})
	t.Run("execution ready", func(t *testing.T) {
		gov, request := newGovernor(), baseRequest()
		request.SafeExecutionReady = false
		denied := gov.AdmitWork(request)
		request.SafeExecutionReady = true
		assertReevaluatesToAllow(t, gov, denied, request)
	})
	t.Run("role configuration", func(t *testing.T) {
		gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle": {Cadences: map[string]string{"quality": "1m"}},
		}}, map[string]config.AgentConfig{"quality": {Enabled: false, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		request := baseRequest()
		denied := gov.AdmitWork(request)
		if denied.Code != "role_unconfigured" {
			t.Fatalf("disabled role decision = %+v", denied)
		}
		gov.UpdateAgents(map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}})
		assertReevaluatesToAllow(t, gov, denied, request)
	})
}

func TestVisualRoleBeadRouterAtomicallyRoutesDependenciesAndExactReplay(t *testing.T) {
	quality, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := beads.NewStore(filepath.Join(t.TempDir(), "scanner"))
	if err != nil {
		t.Fatal(err)
	}
	qualityInput := testVisualBatchInput("quality-source", "visual-hive://owner/repo/quality", "quality", nil)
	scannerInput := testVisualBatchInput("scanner-source", "visual-hive://owner/repo/scanner", "scanner", []string{"quality-source"})
	router := newVisualRoleBeadRouter(map[string]*beads.Store{"quality": quality, "scanner": scanner}, []visualhive.AdmittedVisualWork{
		{SourceExternalRef: qualityInput.ExternalRef, Role: "quality", RoutingAllowed: true, Bead: qualityInput},
		{SourceExternalRef: scannerInput.ExternalRef, Role: "scanner", RoutingAllowed: true, Bead: scannerInput},
	})
	first, err := router.ImportBatch([]beads.BatchInput{scannerInput, qualityInput})
	if err != nil || first.Created != 2 || quality.Count() != 1 || scanner.Count() != 1 {
		t.Fatalf("multi-role import = %+v err=%v quality=%d scanner=%d", first, err, quality.Count(), scanner.Count())
	}
	qualityBead := quality.FindByExternalRef(qualityInput.ExternalRef)
	scannerBead := scanner.FindByExternalRef(scannerInput.ExternalRef)
	if scannerBead == nil || qualityBead == nil || len(scannerBead.DependsOn) != 1 || scannerBead.DependsOn[0] != qualityBead.ID {
		t.Fatalf("cross-role dependency was degraded: quality=%+v scanner=%+v", qualityBead, scannerBead)
	}
	beforeQuality, _ := json.Marshal(qualityBead)
	beforeScanner, _ := json.Marshal(scannerBead)
	replay, err := router.ImportBatch([]beads.BatchInput{qualityInput, scannerInput})
	if err != nil || replay.Created != 0 || replay.Skipped != 2 {
		t.Fatalf("multi-role replay = %+v err=%v", replay, err)
	}
	afterQuality, _ := json.Marshal(quality.FindByExternalRef(qualityInput.ExternalRef))
	afterScanner, _ := json.Marshal(scanner.FindByExternalRef(scannerInput.ExternalRef))
	if string(beforeQuality) != string(afterQuality) || string(beforeScanner) != string(afterScanner) {
		t.Fatalf("exact multi-role replay mutated normal role beads:\nquality before=%s after=%s\nscanner before=%s after=%s", beforeQuality, afterQuality, beforeScanner, afterScanner)
	}
}

func TestVisualRoleBeadRouterPreservesManualHoldInMixedPacket(t *testing.T) {
	quality, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := beads.NewStore(filepath.Join(t.TempDir(), "scanner"))
	if err != nil {
		t.Fatal(err)
	}
	architect, err := beads.NewStore(filepath.Join(t.TempDir(), "architect"))
	if err != nil {
		t.Fatal(err)
	}
	routedInput := testVisualBatchInput("routed-source", "visual-hive://owner/repo/routed", "scanner", nil)
	heldInput := testVisualBatchInput("held-source", "visual-hive://owner/repo/baseline", "", nil)
	heldInput.Metadata["visual_hive_route_allowed"] = false
	heldInput.Metadata["visual_hive_route_reason"] = "baseline approval remains human-held"
	router := newVisualRoleBeadRouter(map[string]*beads.Store{"architect": architect, "quality": quality, "scanner": scanner}, []visualhive.AdmittedVisualWork{
		{SourceExternalRef: routedInput.ExternalRef, Role: "scanner", RoutingAllowed: true, Bead: routedInput},
		{SourceExternalRef: heldInput.ExternalRef, RoutingAllowed: false, RoutingReason: "baseline approval remains human-held", Bead: heldInput},
	})
	if router.err != nil {
		t.Fatalf("intentional manual hold rejected the mixed packet: %v", router.err)
	}
	result, err := router.ImportBatch([]beads.BatchInput{heldInput, routedInput})
	if err != nil || result.Created != 2 {
		t.Fatalf("mixed routed/held import = %+v err=%v", result, err)
	}
	if routed := scanner.FindByExternalRef(routedInput.ExternalRef); routed == nil || routed.Status != beads.StatusBlocked {
		t.Fatalf("valid routed finding was erased by the manual hold: %+v", routed)
	}
	held := quality.FindByExternalRef(heldInput.ExternalRef)
	if held == nil || held.Status != beads.StatusBlocked || held.Actor != "quality" || held.Meta("visual_hive_manual_hold_store_role") != "quality" {
		t.Fatalf("manual finding was not projected as controller-held work: %+v", held)
	}
	if architect.FindByExternalRef(heldInput.ExternalRef) != nil {
		t.Fatal("manual hold was assigned to lexically first architect store")
	}
	if len(quality.Ready("quality")) != 0 || len(scanner.Ready("scanner")) != 0 {
		t.Fatal("mixed packet exposed controller-owned work to ordinary cadence")
	}
}

func TestRoleRouterFailsClosedOnVerifiedRouteChangeOrMissingDesiredStore(t *testing.T) {
	quality, _ := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	security, _ := beads.NewStore(filepath.Join(t.TempDir(), "security"))
	ref := "visual-hive://owner/repo/" + strings.Repeat("4", 64)
	if _, err := quality.Create("existing", beads.TypeBug, beads.PriorityHigh, "quality", ref); err != nil {
		t.Fatal(err)
	}
	work := visualhive.AdmittedVisualWork{SourceExternalRef: ref, Role: "security", RoutingAllowed: true, Bead: beads.BatchInput{ExternalRef: ref}}
	changed := newVisualRoleBeadRouter(map[string]*beads.Store{"quality": quality, "security": security}, []visualhive.AdmittedVisualWork{work})
	if changed.err == nil || !strings.Contains(changed.err.Error(), "without an authorized role migration") {
		t.Fatalf("stale quality bead silently overrode verified security route: %v", changed.err)
	}
	missing := newVisualRoleBeadRouter(map[string]*beads.Store{"quality": quality}, []visualhive.AdmittedVisualWork{work})
	if missing.err == nil || !strings.Contains(missing.err.Error(), "routed role \"security\"") {
		t.Fatalf("missing verified security store reused stale quality bead: %v", missing.err)
	}
}

func TestManualHoldStoreUsesScannerFallbackAndRejectsArbitraryRoles(t *testing.T) {
	ref := "visual-hive://owner/repo/" + strings.Repeat("5", 64)
	input := beads.BatchInput{SourceID: strings.Repeat("5", 64), Title: "manual Visual review", Type: beads.TypeAdvisory, Priority: beads.PriorityMedium, ExternalRef: ref, Status: beads.StatusBlocked, Metadata: map[string]interface{}{"visual_hive_controller_owned": true}}
	work := visualhive.AdmittedVisualWork{SourceExternalRef: ref, RoutingAllowed: false, RoutingReason: "manual", Bead: input}
	scanner, _ := beads.NewStore(filepath.Join(t.TempDir(), "scanner"))
	router := newVisualRoleBeadRouter(map[string]*beads.Store{"scanner": scanner}, []visualhive.AdmittedVisualWork{work})
	if router.err != nil || router.RoleForExternalRef(ref) != "scanner" {
		t.Fatalf("scanner fallback = role %q err=%v", router.RoleForExternalRef(ref), router.err)
	}
	if _, err := router.ImportBatch([]beads.BatchInput{input}); err != nil {
		t.Fatal(err)
	}
	if bead := scanner.FindByExternalRef(ref); bead == nil || bead.Actor != "scanner" || bead.Metadata["visual_hive_manual_hold_store_role"] != "scanner" {
		t.Fatalf("scanner manual hold = %+v", bead)
	}
	architect, _ := beads.NewStore(filepath.Join(t.TempDir(), "architect"))
	rejected := newVisualRoleBeadRouter(map[string]*beads.Store{"architect": architect}, []visualhive.AdmittedVisualWork{work})
	if rejected.err == nil || !strings.Contains(rejected.err.Error(), "no existing normal bead store") {
		t.Fatalf("manual hold used arbitrary architect store: %v", rejected.err)
	}
	existingRef := "visual-hive://owner/repo/" + strings.Repeat("6", 64)
	if _, err := architect.Create("previously routed work", beads.TypeAdvisory, beads.PriorityMedium, "architect", existingRef); err != nil {
		t.Fatal(err)
	}
	existingInput := input
	existingInput.SourceID = strings.Repeat("6", 64)
	existingInput.ExternalRef = existingRef
	existingWork := visualhive.AdmittedVisualWork{SourceExternalRef: existingRef, RoutingAllowed: false, RoutingReason: "new manual hold", Bead: existingInput}
	quality, _ := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	preserved := newVisualRoleBeadRouter(map[string]*beads.Store{"architect": architect, "quality": quality, "scanner": scanner}, []visualhive.AdmittedVisualWork{existingWork})
	if preserved.err != nil || preserved.RoleForExternalRef(existingRef) != "architect" {
		t.Fatalf("manual transition did not preserve the existing specialist role: role=%q err=%v", preserved.RoleForExternalRef(existingRef), preserved.err)
	}
	if _, err := preserved.ImportBatch([]beads.BatchInput{existingInput}); err != nil {
		t.Fatal(err)
	}
	if quality.FindByExternalRef(existingRef) != nil || scanner.FindByExternalRef(existingRef) != nil || architect.FindByExternalRef(existingRef) == nil {
		t.Fatalf("manual transition duplicated an existing role bead: architect=%+v quality=%+v scanner=%+v", architect.FindByExternalRef(existingRef), quality.FindByExternalRef(existingRef), scanner.FindByExternalRef(existingRef))
	}
}

func TestMixedPacketAdmitsRoutedPeerAndNeverDispatchesManualHold(t *testing.T) {
	store, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := visualhive.NewLifecycleStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	routedFingerprint, heldFingerprint, peerFingerprint := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	digest := strings.Repeat("8", 64)
	bundle := &visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true}, Manifest: visualhive.Manifest{
		BundleID: "mixed-held-packet", OverallDigest: digest,
		Source:           visualhive.Source{Repository: "owner/repo", RepositoryID: "123", CommitSHA: strings.Repeat("d", 40), WorkflowRunID: "42"},
		ReplayProtection: visualhive.ReplayProtection{Key: strings.Repeat("9", 64)},
		Observations: []visualhive.Observation{
			{Fingerprint: "routed/source", RepositoryFingerprint: routedFingerprint, PublicationRole: "canonical", RootCauseKey: "routed/source", State: "present", IssueKind: "visual_regression", Severity: "high", OwningAgentHint: "hive/quality", Title: "routed finding", Body: "repairable", AffectedContracts: []string{"contract/app"}, ValidationCommand: "go test ./...", FirstSeenAt: "2026-07-09T12:00:00Z"},
			{Fingerprint: "baseline/source", RepositoryFingerprint: heldFingerprint, PublicationRole: "canonical", RootCauseKey: "baseline/source", State: "present", IssueKind: "missing_baseline", Severity: "medium", OwningAgentHint: "hive/quality", Title: "baseline review", Body: "human authority required", AffectedContracts: []string{"contract/baseline"}, ValidationCommand: "go test ./...", FirstSeenAt: "2026-07-09T12:00:00Z"},
			{Fingerprint: "peer/source", RepositoryFingerprint: peerFingerprint, PublicationRole: "canonical", RootCauseKey: "peer/source", State: "present", IssueKind: "test_adequacy", Severity: "medium", OwningAgentHint: "hive/quality", Title: "second routed finding", Body: "defer without loss", AffectedContracts: []string{"contract/peer"}, ValidationCommand: "go test ./...", FirstSeenAt: "2026-07-09T12:00:00Z"},
		},
	}}
	apply, err := lifecycle.ApplyBundle(bundle, store, visualhive.ApplyLifecycleOptions{DisableIssuePublication: true, MaxActiveIssues: 3})
	if err != nil || apply.BeadsCreated != 3 {
		t.Fatalf("mixed lifecycle apply = %+v err=%v", apply, err)
	}
	for _, fingerprint := range []string{routedFingerprint, heldFingerprint, peerFingerprint} {
		bead := store.FindByExternalRef("visual-hive://owner/repo/" + fingerprint)
		if bead == nil {
			t.Fatalf("mixed packet finding %s is not visible", fingerprint)
		}
		if err := store.Update(bead.ID, func(value *beads.Bead) {
			value.Status = beads.StatusBlocked
			value.Metadata["visual_hive_controller_owned"] = true
			value.Metadata["visual_hive_admission_state"] = "pending"
		}); err != nil {
			t.Fatal(err)
		}
	}
	gov := governor.New(config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Cadences: map[string]string{"quality": "1m"}}}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issues := &controllerIssueClient{governor: gov}
	controller, err := New(gov, lifecycle, map[string]*beads.Store{"quality": store}, nil, issues, integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), ACMMLevel: 5, Automation: integrated.AutomationRepairPR,
		MaxActiveIssues: 3, AllowedRepairPaths: []string{"src/**"}, VisualHiveRef: strings.Repeat("c", 40), TestCommands: [][]string{{"go", "test", "./..."}},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	controller.baseTree = func(context.Context, integrated.Config, string) (string, error) { return strings.Repeat("f", 40), nil }
	packet := completeControllerPacket("mixed-held-packet", digest)
	works := []visualhive.AdmittedVisualWork{
		{SourceExternalRef: "visual-hive://owner/repo/" + routedFingerprint, Packet: packet, FindingFingerprint: "routed/source", RepositoryFingerprint: routedFingerprint, ObservationState: "present", Role: "quality", RoutingAllowed: true, RoutingReason: "verified route", KnowledgeKeywordState: "unavailable_no_verified_facts", ValidationCommands: []string{"go test ./..."}, ReproductionCommands: []string{"go test ./..."}},
		{SourceExternalRef: "visual-hive://owner/repo/" + heldFingerprint, Packet: packet, FindingFingerprint: "baseline/source", RepositoryFingerprint: heldFingerprint, ObservationState: "present", RoutingAllowed: false, RoutingReason: "baseline approval remains human-held"},
		{SourceExternalRef: "visual-hive://owner/repo/" + peerFingerprint, Packet: packet, FindingFingerprint: "peer/source", RepositoryFingerprint: peerFingerprint, ObservationState: "present", Role: "quality", RoutingAllowed: true, RoutingReason: "verified route", KnowledgeKeywordState: "unavailable_no_verified_facts", ValidationCommands: []string{"go test ./..."}, ReproductionCommands: []string{"go test ./..."}},
	}
	result := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, works, Result{Lifecycle: apply})
	held := store.FindByExternalRef(works[1].SourceExternalRef)
	if len(result.Decisions) != 3 || !result.Decisions[0].Allowed || result.Decisions[1].Code != "routing_held" || !result.Decisions[2].Allowed || len(result.DispatchPending) != 2 || result.DispatchPending[0].SourceExternalRef != works[0].SourceExternalRef || result.DispatchPending[1].SourceExternalRef != works[2].SourceExternalRef ||
		issues.upserts != 3 || len(gov.AdmissionHistory()) != 3 || held == nil || held.Status != beads.StatusBlocked || visualBeadAdmissionState(held) != "held_manual_review" || len(store.Ready("quality")) != 0 {
		t.Fatalf("mixed controller result lost valid work or dispatched the hold: result=%+v held=%+v upserts=%d history=%+v", result, held, issues.upserts, gov.AdmissionHistory())
	}
	correlation := strings.Repeat("7", 64)
	if err := controller.DeferUnselectedSpecialistDispatches(context.Background(), works[0].SourceExternalRef, []string{works[2].SourceExternalRef}, correlation); err != nil {
		t.Fatalf("defer routed peer: %v", err)
	}
	peer := store.FindByExternalRef(works[2].SourceExternalRef)
	deferred, deferredOK := visualDispatchDeferral(peer)
	if peer == nil || visualBeadAdmissionState(peer) != "admitted_dispatch_deferred" || !deferredOK || deferred.SourceExternalRef != works[2].SourceExternalRef ||
		deferred.SelectedSourceExternalRef != works[0].SourceExternalRef || deferred.WorkflowCorrelationSHA256 != correlation || deferred.PacketDigest != digest {
		t.Fatalf("routed peer was not durably correlated as deferred: peer=%+v deferral=%+v ok=%t", peer, deferred, deferredOK)
	}
	if err := controller.DeferUnselectedSpecialistDispatches(context.Background(), works[0].SourceExternalRef, []string{works[2].SourceExternalRef}, correlation); err != nil {
		t.Fatalf("exact deferral replay: %v", err)
	}
	if err := controller.DeferUnselectedSpecialistDispatches(context.Background(), works[0].SourceExternalRef, []string{works[2].SourceExternalRef}, strings.Repeat("9", 64)); err == nil {
		t.Fatal("changed workflow correlation replay replaced an existing exact deferral")
	}
	history := len(gov.AdmissionHistory())
	replay := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, works, Result{Lifecycle: apply})
	if len(replay.Decisions) != 0 || len(replay.DispatchPending) != 1 || replay.DispatchPending[0].SourceExternalRef != works[0].SourceExternalRef || issues.upserts != 3 || len(gov.AdmissionHistory()) != history || visualBeadAdmissionState(store.FindByExternalRef(works[1].SourceExternalRef)) != "held_manual_review" || visualBeadAdmissionState(store.FindByExternalRef(works[2].SourceExternalRef)) != "admitted_dispatch_deferred" {
		t.Fatalf("mixed replay duplicated or dispatched manual-held work: %+v", replay)
	}
	gov.UpdateConfigAndAgents(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]string{"quality": "1m"}},
		"busy": {Cadences: map[string]string{"quality": "1m"}},
	}}, map[string]config.AgentConfig{"quality": {Enabled: true, Role: "quality"}})
	gov.SetMode(governor.ModeBusy)
	deferredReplay := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(digest)}, packet, []visualhive.AdmittedVisualWork{works[2]}, Result{Lifecycle: apply})
	if len(deferredReplay.Decisions) != 0 || len(deferredReplay.DispatchPending) != 0 || len(deferredReplay.Errors) != 0 ||
		visualBeadAdmissionState(store.FindByExternalRef(works[2].SourceExternalRef)) != "admitted_dispatch_deferred" {
		t.Fatalf("same-packet deferred replay was changed by unrelated launch-policy drift: %+v", deferredReplay)
	}
	gov.SetMode(governor.ModeIdle)
	peerDigest := strings.Repeat("2", 64)
	peerManifest := bundle.Manifest
	peerManifest.BundleID, peerManifest.OverallDigest = "peer-retry-packet", peerDigest
	peerManifest.ReplayProtection = visualhive.ReplayProtection{Key: strings.Repeat("1", 64)}
	peerManifest.Observations = []visualhive.Observation{bundle.Manifest.Observations[2]}
	peerApply, err := lifecycle.ApplyBundle(&visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true}, Manifest: peerManifest}, store, visualhive.ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true, MaxActiveIssues: 3, PreferRepairable: true})
	if err != nil {
		t.Fatal(err)
	}
	peerPacket := completeControllerPacket("peer-retry-packet", peerDigest)
	peerWork := works[2]
	peerWork.Packet = peerPacket
	peerRetry := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(peerDigest)}, peerPacket, []visualhive.AdmittedVisualWork{peerWork}, Result{Lifecycle: peerApply})
	peer = store.FindByExternalRef(peerWork.SourceExternalRef)
	_, staleDeferral := visualDispatchDeferral(peer)
	if len(peerRetry.Decisions) != 1 || !peerRetry.Decisions[0].Allowed || len(peerRetry.DispatchPending) != 1 || peerRetry.DispatchPending[0].SourceExternalRef != peerWork.SourceExternalRef ||
		peerRetry.DispatchPending[0].Work.KnowledgeKeywordState != "unavailable_no_verified_facts" || visualBeadAdmissionState(peer) != "admitted_dispatch_pending" || staleDeferral {
		t.Fatalf("later verified packet did not re-admit exactly the deferred peer: result=%+v peer=%+v", peerRetry, peer)
	}
	updateDigest := strings.Repeat("6", 64)
	updateManifest := bundle.Manifest
	updateManifest.BundleID, updateManifest.OverallDigest = "manual-update-packet", updateDigest
	updateManifest.ReplayProtection = visualhive.ReplayProtection{Key: strings.Repeat("5", 64)}
	updateManifest.Observations = []visualhive.Observation{bundle.Manifest.Observations[1]}
	updateManifest.Observations[0].Body = "updated human review evidence"
	updateBundle := &visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true}, Manifest: updateManifest}
	updateApply, err := lifecycle.ApplyBundle(updateBundle, store, visualhive.ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true, MaxActiveIssues: 3})
	if err != nil {
		t.Fatal(err)
	}
	updatePacket := completeControllerPacket("manual-update-packet", updateDigest)
	updateWork := works[1]
	updateWork.Packet, updateWork.Body = updatePacket, updateManifest.Observations[0].Body
	updatesBeforeManual := issues.updates
	updated := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(updateDigest)}, updatePacket, []visualhive.AdmittedVisualWork{updateWork}, Result{Lifecycle: updateApply})
	if len(updated.DispatchPending) != 0 || len(updated.Decisions) != 1 || updated.Decisions[0].Code != "routing_held" || issues.updates != updatesBeforeManual+1 || visualBeadAdmissionState(store.FindByExternalRef(updateWork.SourceExternalRef)) != "held_manual_review" {
		t.Fatalf("manual route stranded linked issue update or dispatched: result=%+v updates=%d", updated, issues.updates)
	}
	absentDigest := strings.Repeat("4", 64)
	absentManifest := updateManifest
	absentManifest.BundleID, absentManifest.OverallDigest = "manual-absence-packet", absentDigest
	absentManifest.ReplayProtection = visualhive.ReplayProtection{Key: strings.Repeat("3", 64)}
	absentManifest.Scan = visualhive.Scan{Scope: "full", AuthoritativeForResolution: true, EvaluatedContracts: []string{"contract/baseline"}}
	absentManifest.Source.Ref = "refs/heads/main"
	absentManifest.Observations[0].State = "absent"
	absentBundle := &visualhive.ValidatedBundle{Validation: visualhive.Validation{Trusted: true, Authoritative: true}, Manifest: absentManifest}
	absentApply, err := lifecycle.ApplyBundle(absentBundle, store, visualhive.ApplyLifecycleOptions{TargetRef: "main", DisableIssuePublication: true, MaxActiveIssues: 3})
	if err != nil {
		t.Fatal(err)
	}
	absentPacket := completeControllerPacket("manual-absence-packet", absentDigest)
	absentWork := updateWork
	absentWork.Packet, absentWork.ObservationState = absentPacket, "absent"
	absent := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(absentDigest)}, absentPacket, []visualhive.AdmittedVisualWork{absentWork}, Result{Lifecycle: absentApply})
	closed := store.FindByExternalRef(absentWork.SourceExternalRef)
	if len(absent.DispatchPending) != 0 || issues.closes != 1 || closed == nil || closed.Status != beads.StatusClosed || visualBeadAdmissionState(closed) != "held_manual_review_lifecycle_synced" {
		t.Fatalf("manual route stranded authoritative issue close: result=%+v closes=%d bead=%+v", absent, issues.closes, closed)
	}
	absentReplay := resumeAppliedForTest(controller, context.Background(), controllerEvidenceSource{completeControllerEvidence(absentDigest)}, absentPacket, []visualhive.AdmittedVisualWork{absentWork}, Result{Lifecycle: absentApply})
	if len(absentReplay.Decisions) != 0 || len(absentReplay.DispatchPending) != 0 || issues.closes != 1 {
		t.Fatalf("manual absence replay duplicated close or dispatched: %+v closes=%d", absentReplay, issues.closes)
	}
}

func TestMissingAllowedRoleStoreIsDurablyAuditedAndFailClosed(t *testing.T) {
	quality, err := beads.NewStore(filepath.Join(t.TempDir(), "quality"))
	if err != nil {
		t.Fatal(err)
	}
	input := testVisualBatchInput("scanner-source", "visual-hive://owner/repo/scanner", "scanner", nil)
	router := newVisualRoleBeadRouter(map[string]*beads.Store{"quality": quality}, []visualhive.AdmittedVisualWork{
		{SourceExternalRef: input.ExternalRef, Role: "scanner", RoutingAllowed: true, Bead: input},
	})
	if router.err == nil {
		t.Fatal("missing configured role store was silently skipped")
	}
	lifecycleDir := t.TempDir()
	lifecycle, err := visualhive.NewLifecycleStore(lifecycleDir)
	if err != nil {
		t.Fatal(err)
	}
	audit := &controllerAuditSink{}
	controller := &Controller{lifecycle: lifecycle, audit: audit}
	packet := completeControllerPacket("missing-role", strings.Repeat("8", 64))
	if err := controller.recordRoutingHold(context.Background(), packet, router.err); err == nil {
		t.Fatal("fail-closed routing error was not returned")
	}
	auditBytes, err := os.ReadFile(filepath.Join(lifecycleDir, "visual-hive-lifecycle-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !audit.saw("held_missing_role_store") || !strings.Contains(string(auditBytes), `"action":"controller_import_hold"`) || quality.Count() != 0 {
		t.Fatalf("missing role was not durably/auditably held: normal=%+v lifecycle=%s", audit.events, auditBytes)
	}
}

func TestVisualRoleBeadRouterRollsBackEarlierRoleOnLaterStoreFailure(t *testing.T) {
	root := t.TempDir()
	quality, err := beads.NewStore(filepath.Join(root, "quality"))
	if err != nil {
		t.Fatal(err)
	}
	scannerDir := filepath.Join(root, "scanner")
	scanner, err := beads.NewStore(scannerDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(scannerDir, "beads.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	qualityInput := testVisualBatchInput("quality-source", "visual-hive://owner/repo/quality", "quality", nil)
	scannerInput := testVisualBatchInput("scanner-source", "visual-hive://owner/repo/scanner", "scanner", nil)
	router := newVisualRoleBeadRouter(map[string]*beads.Store{"quality": quality, "scanner": scanner}, []visualhive.AdmittedVisualWork{
		{SourceExternalRef: qualityInput.ExternalRef, Role: "quality", RoutingAllowed: true, Bead: qualityInput},
		{SourceExternalRef: scannerInput.ExternalRef, Role: "scanner", RoutingAllowed: true, Bead: scannerInput},
	})
	if _, err := router.ImportBatch([]beads.BatchInput{qualityInput, scannerInput}); err == nil {
		t.Fatal("later role persistence failure was accepted")
	}
	if quality.FindByExternalRef(qualityInput.ExternalRef) != nil || scanner.FindByExternalRef(scannerInput.ExternalRef) != nil {
		t.Fatalf("multi-role failure left a partial import: quality=%+v scanner=%+v", quality.List(beads.ListFilter{}), scanner.List(beads.ListFilter{}))
	}
}

func TestResumeAndReserveSerializeOnControllerLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	runtime := integrated.Config{Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: t.TempDir(), VisualHiveRef: strings.Repeat("c", 40), ACMMLevel: 5}
	controller := &Controller{installed: cloneIntegratedConfig(runtime), normalACMM: 5, loadPlan: func(hivegithub.VerifiedVisualHiveArtifact) (visualhive.VerifiedImportPlan, error) {
		return visualhive.VerifiedImportPlan{}, errors.New("injected sealed-plan stop")
	}}
	refreshes := 0
	controller.SetRuntimeConfigLoader(func() (integrated.Config, int, error) {
		refreshes++
		if refreshes == 1 {
			close(entered)
			<-release
		}
		return runtime, 5, nil
	})
	resumeDone := make(chan error, 1)
	go func() {
		_, err := controller.Resume(context.Background(), hivegithub.VerifiedVisualHiveArtifact{})
		resumeDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("resume did not enter the authoritative runtime refresh boundary")
	}
	reserveDone := make(chan error, 1)
	go func() {
		digest := strings.Repeat("a", 64)
		_, err := controller.ReserveSpecialistWorkOrderIdentity("visual-hive://owner/repo/finding", "swo-"+digest, digest)
		reserveDone <- err
	}()
	select {
	case err := <-reserveDone:
		t.Fatalf("reservation bypassed in-flight resume lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-resumeDone; err == nil || !strings.Contains(err.Error(), "injected sealed-plan stop") {
		t.Fatalf("resume error = %v", err)
	}
	if err := <-reserveDone; err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("post-resume reservation error = %v", err)
	}
}

func TestRuntimeConfigRefreshIsAuthoritativeClonedAndIdentityBound(t *testing.T) {
	stateDir := t.TempDir()
	current := integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
		VisualHiveRef: strings.Repeat("c", 40), ACMMLevel: 5, Automation: integrated.AutomationRepairPR,
		ProviderArgs: []string{"--model", "one"}, VisualHiveArgs: []string{"run"}, TestCommands: [][]string{{"go", "test", "./..."}},
		AllowedRepairPaths: []string{"src/**"}, AllowedAutoMergePaths: []string{"src/**"}, AllowedAutoMergeRisk: []automation.RiskTier{automation.RiskLow},
		ManagedPathPreimages: map[string]integrated.ManagedPathPreimage{"hive.yaml": {Content: []byte("original")}},
	}
	live := cloneIntegratedConfig(current)
	live.Paused = true
	live.MaxActiveIssues = 7
	planCalls := 0
	controller := &Controller{installed: cloneIntegratedConfig(current), normalACMM: 5, loadPlan: func(hivegithub.VerifiedVisualHiveArtifact) (visualhive.VerifiedImportPlan, error) {
		planCalls++
		return visualhive.VerifiedImportPlan{}, errors.New("injected plan stop")
	}}
	audit := &controllerAuditSink{}
	controller.SetAuditSink(audit)
	controller.SetRuntimeConfigLoader(func() (integrated.Config, int, error) { return live, 4, nil })
	if _, err := controller.Resume(context.Background(), hivegithub.VerifiedVisualHiveArtifact{}); err == nil || !strings.Contains(err.Error(), "injected plan stop") {
		t.Fatalf("runtime refresh did not reach the sealed-plan boundary: %v", err)
	}
	if !controller.installed.Paused || controller.installed.MaxActiveIssues != 7 || controller.normalACMM != 4 || planCalls != 1 {
		t.Fatalf("authoritative runtime values were not refreshed: installed=%+v normal_acmm=%d plan_calls=%d", controller.installed, controller.normalACMM, planCalls)
	}
	live.ProviderArgs[0] = "tampered"
	live.VisualHiveArgs[0] = "tampered"
	live.TestCommands[0][0] = "tampered"
	live.AllowedRepairPaths[0] = "tampered/**"
	live.AllowedAutoMergePaths[0] = "tampered/**"
	live.AllowedAutoMergeRisk[0] = automation.RiskRestricted
	preimage := live.ManagedPathPreimages["hive.yaml"]
	preimage.Content[0] = 'X'
	if controller.installed.ProviderArgs[0] != "--model" || controller.installed.VisualHiveArgs[0] != "run" || controller.installed.TestCommands[0][0] != "go" ||
		controller.installed.AllowedRepairPaths[0] != "src/**" || controller.installed.AllowedAutoMergePaths[0] != "src/**" || controller.installed.AllowedAutoMergeRisk[0] != automation.RiskLow ||
		string(controller.installed.ManagedPathPreimages["hive.yaml"].Content) != "original" {
		t.Fatalf("loader-owned slices or bytes alias the controller snapshot: %+v", controller.installed)
	}
	drift := cloneIntegratedConfig(controller.installed)
	drift.RepositoryID = "999"
	controller.SetRuntimeConfigLoader(func() (integrated.Config, int, error) { return drift, 4, nil })
	if _, err := controller.Resume(context.Background(), hivegithub.VerifiedVisualHiveArtifact{}); err == nil || !strings.Contains(err.Error(), "identity drifted") {
		t.Fatalf("immutable installed identity drift was not held: %v", err)
	}
	if planCalls != 1 || controller.installed.RepositoryID != "123" || !audit.saw("held_runtime_config") {
		t.Fatalf("identity drift changed state or passed the plan boundary: installed=%+v calls=%d audit=%+v", controller.installed, planCalls, audit.events)
	}
}

func TestTerminalLifecycleStageRetiresWIPUntilRecurrence(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bead, err := store.Create("visual finding", beads.TypeBug, beads.PriorityHigh, "quality", "visual-hive://owner/repo/terminal")
	if err != nil {
		t.Fatal(err)
	}
	if err := markVisualBeadStage(store, bead.ID, "admitted_lifecycle_synced", "authoritative close synchronized"); err != nil {
		t.Fatal(err)
	}
	retired := store.FindByExternalRef(bead.ExternalRef)
	if retired.Status != beads.StatusClosed || retired.ClosedAt == nil || len(store.Ready("quality")) != 0 {
		t.Fatalf("terminal controller stage reopened WIP: %+v ready=%+v", retired, store.Ready("quality"))
	}
	if err := markVisualBeadStage(store, bead.ID, "pending", "new verified recurrence requires admission"); err != nil {
		t.Fatal(err)
	}
	recurrent := store.FindByExternalRef(bead.ExternalRef)
	if recurrent.Status != beads.StatusBlocked || recurrent.ClosedAt != nil || len(store.Ready("quality")) != 0 {
		t.Fatalf("recurrence was not reopened as controller-held work: %+v", recurrent)
	}
}

func testVisualBatchInput(sourceID, externalRef, role string, dependencies []string) beads.BatchInput {
	return beads.BatchInput{
		SourceID: sourceID, Title: sourceID, Type: beads.TypeBug, Status: beads.StatusBlocked, Priority: beads.PriorityHigh,
		Actor: role, ExternalRef: externalRef, DependsOn: dependencies,
		Metadata: map[string]interface{}{"visual_hive_controller_owned": true, "visual_hive_admission_state": "pending"},
	}
}

func controllerPullRequestCheckReceipt(t *testing.T, envelope DispatchEnvelope, headSHA, headBranch string, pullRequest int) (visualhive.PullRequestCheckReceiptSnapshot, visualhivepr.SealedReceipt) {
	t.Helper()
	digest := func(value string) string { return strings.Repeat(value, 64) }
	file := func(path, value string) visualhive.PullRequestFileIdentity {
		return visualhive.PullRequestFileIdentity{Path: path, SHA256: digest(value), Bytes: 100}
	}
	baseBranch := strings.TrimPrefix(envelope.Work.Packet.SourceRef, "refs/heads/")
	producerCommit := hivegithub.VisualHivePullRequestProducerCommit
	identity := visualhive.PullRequestCheckReceiptIdentity{
		SchemaVersion: visualhive.PullRequestCheckReceiptSchema,
		Source: visualhive.PullRequestSourceBinding{
			SchemaVersion: visualhive.PullRequestSourceBindingSchema,
			Repository:    "owner/repo", RepositoryID: "123", PullRequest: pullRequest,
			Base: visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: baseBranch, SHA: envelope.BaseSHA},
			Head: visualhive.PullRequestRevisionIdentity{Repository: "owner/repo", RepositoryID: "123", Ref: headBranch, SHA: headSHA},
			Workflow: visualhive.PullRequestWorkflowIdentity{
				Name: "Visual Hive PR", Path: ".github/workflows/visual-hive-pr.yml", Event: "pull_request", RunID: "77", RunAttempt: "2",
				Definition: file(visualhive.PullRequestWorkflowPath, "c"), ProducerCommit: producerCommit,
				SourceName: "visual-hive-pr-source-77-2", BundleName: "visual-hive-pr-bundle-77-2",
			},
			Plan: file(visualhive.PullRequestPlanPath, "1"), Report: file(visualhive.PullRequestReportPath, "2"), Config: file(visualhive.PullRequestConfigPath, "3"),
			ChangedFiles: file(visualhive.PullRequestChangedFilesPath, "4"), PlanContracts: file(visualhive.PullRequestPlanContractsPath, "b"),
			EvaluationScopes: file(visualhive.PullRequestEvaluationScopesPath, "c"), RunnerAttestation: file(visualhive.PullRequestRunnerAttestationPath, "d"),
			Runtime: visualhive.PullRequestRuntimeIdentity{
				Receipt: file(visualhive.PullRequestRuntimePath, "5"), GeneratedSpec: file(".visual-hive/generated/visual-hive.generated.spec.cjs", "0"),
				GeneratedConfig: file(".visual-hive/generated/visual-hive.generated.config.cjs", "e"), StableSHA256: digest("6"), ExecutionBindingSHA256: digest("7"),
			},
			Baselines: visualhive.PullRequestBaselineIdentity{SHA256: digest("8"), Files: []visualhive.PullRequestFileIdentity{
				file(".visual-hive/proof/pr/baselines/desktop.png", "9"),
				file(".visual-hive/proof/pr/baselines/mobile.png", "a"),
			}},
		},
		Workflow: visualhive.PullRequestCheckWorkflowIdentity{
			ID: "12", Name: "Visual Hive PR", Path: ".github/workflows/visual-hive-pr.yml", Event: "pull_request", RunID: "77", RunAttempt: "2",
			CheckSuiteID: "501", JobID: "701", CheckRunID: "801", AppID: "15368", Definition: file(visualhive.PullRequestWorkflowPath, "c"),
		},
		SourceArtifact: visualhive.PullRequestCheckArtifactIdentity{ID: "98", Name: "visual-hive-pr-source-77-2"},
		BundleArtifact: visualhive.PullRequestCheckArtifactIdentity{ID: "99", Name: "visual-hive-pr-bundle-77-2"},
		Producer:       visualhive.Producer{Name: "visual-hive", Version: "test", GitCommit: producerCommit},
		Bundle: visualhive.PullRequestCheckBundleIdentity{
			SchemaVersion: visualhive.ManifestSchemaV3, BundleID: "pr-bundle-77", OverallSHA256: digest("b"), ManifestSHA256: digest("c"),
			ArtifactIndex: visualhive.ArtifactIndexBinding{
				Path: "files/.visual-hive/artifacts-index.json", SourcePath: ".visual-hive/artifacts-index.json", SHA256: digest("d"),
				SchemaVersion: 1, ContentAddressed: true, Complete: true, ArtifactCount: 11, TotalBytes: 1100,
			},
		},
		Check: visualhive.PullRequestCheckResultIdentity{
			State: "success", Conclusion: "success", Summary: "Visual Hive exact PR rerun passed", RunURL: "https://github.test/owner/repo/actions/runs/77",
		},
		Authority: visualhive.PullRequestCheckAuthority{CheckEvidenceOnly: true},
	}
	identity.ReplayKey = visualhive.PullRequestCheckReplayKey(identity)
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	snapshot := visualhive.PullRequestCheckReceiptSnapshot{Identity: identity, ReceiptSHA256: hex.EncodeToString(sum[:])}
	sealed, err := visualhivepr.SealVerifiedReceipt(data, snapshot.ReceiptSHA256, identity.ReplayKey)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, sealed
}

func cloneDispatchEnvelope(t *testing.T, envelope DispatchEnvelope) DispatchEnvelope {
	t.Helper()
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var clone DispatchEnvelope
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

type controllerEvidenceSource struct {
	evidence agent.SpecialistEvidenceIdentity
}

func resumeAppliedForTest(
	controller *Controller,
	ctx context.Context,
	source visualSpecialistEvidenceSource,
	packet visualhive.VerifiedPacketIdentity,
	works []visualhive.AdmittedVisualWork,
	result Result,
) Result {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	works = controller.resumeWorks(packet, works)
	router := newVisualRoleBeadRouter(controller.beadStores, works)
	if router.err != nil {
		result.Errors = append(result.Errors, router.err.Error())
		return result
	}
	return controller.resumeAppliedWork(ctx, source, packet, works, router, result)
}

type controllerAuditSink struct {
	events    []AuditEvent
	failStage string
}

func (sink *controllerAuditSink) RecordVisualWorkAudit(_ context.Context, event AuditEvent) error {
	if sink.failStage == event.Stage {
		sink.failStage = ""
		return errors.New("injected audit failure")
	}
	sink.events = append(sink.events, event)
	return nil
}

func (sink *controllerAuditSink) saw(stage string) bool {
	for _, event := range sink.events {
		if event.Stage == stage {
			return true
		}
	}
	return false
}

func (sink *controllerAuditSink) count(stage string) int {
	count := 0
	for _, event := range sink.events {
		if event.Stage == stage {
			count++
		}
	}
	return count
}

func (source controllerEvidenceSource) SpecialistEvidenceIdentity(visualhive.FindingLifecycle) (agent.SpecialistEvidenceIdentity, error) {
	return source.evidence, nil
}

func (controllerEvidenceSource) VerifiedEvidenceRoot() (string, error) {
	return "C:/verified/evidence", nil
}

type controllerIssueClient struct {
	governor      *governor.Governor
	admissionSeen bool
	upserts       int
	updates       int
	closes        int
}

func (client *controllerIssueClient) ResolveLifecycleIssueWriter(context.Context, string, string, int) (string, int64, error) {
	history := client.governor.AdmissionHistory()
	client.admissionSeen = len(history) > 0 && history[len(history)-1].Allowed
	return "hive-bot", 7, nil
}
func (client *controllerIssueClient) UpsertLifecycleIssueOwned(context.Context, string, string, string, int64, string, string, []string) (int, string, bool, error) {
	client.upserts++
	return 101, "https://example.invalid/issues/101", true, nil
}
func (client *controllerIssueClient) UpdateLifecycleIssueOwned(_ context.Context, _ string, number int, _ string, _ string, _ int64, _ string, _ string, state string, _ []string) (int, string, error) {
	if state == "closed" {
		client.closes++
	} else {
		client.updates++
	}
	return number, "https://example.invalid/issues/101", nil
}
func (*controllerIssueClient) MigrateLifecycleIssueMarkerOwned(context.Context, string, int, string, string, string, int64, string, string, string, []string) (int, string, error) {
	return 0, "", errors.New("unexpected marker migration")
}

func completeControllerEvidence(bundleDigest string) agent.SpecialistEvidenceIdentity {
	receipt := visualhive.SpecialistEvidenceReceipt{
		RepositoryID: "123", WorkflowRunID: "42", WorkflowRunAttempt: "2", ArtifactID: "99", SourceArtifactID: "100",
		ArtifactName: "visual-hive-bundle", CommitSHA: strings.Repeat("d", 40), HeadBranch: "main", Event: "workflow_dispatch",
		WorkflowName: trustedWorkflowName, WorkflowRunName: trustedWorkflowName, WorkflowPath: trustedWorkflowPath,
		BundleSchema: visualhive.ManifestSchemaV3, BundleSHA256: bundleDigest, ManifestSHA256: strings.Repeat("1", 64), ArtifactIndexSHA: strings.Repeat("2", 64),
	}
	receiptDigest, _ := receipt.SHA256()
	return agent.SpecialistEvidenceIdentity{
		Variant:             visualhive.SpecialistEvidenceVariantVisualHiveV3,
		BundleSchemaVersion: visualhive.ManifestSchemaV3, BundleSHA256: bundleDigest, ManifestSHA256: strings.Repeat("1", 64),
		ArtifactIndexSHA256: strings.Repeat("2", 64), VerificationReceiptSHA256: receiptDigest, RepositoryID: "123",
		WorkflowRunID: 42, WorkflowRunAttempt: 2, WorkflowRunHeadSHA: strings.Repeat("d", 40), WorkflowName: trustedWorkflowName,
		WorkflowRunName: trustedWorkflowName, WorkflowPath: trustedWorkflowPath, WorkflowEvent: "workflow_dispatch",
		HeadBranch: "main", ArtifactID: 99, SourceArtifactID: 100, ArtifactName: "visual-hive-bundle", ArtifactSHA256: strings.Repeat("1", 64), VerificationReceipt: &receipt,
	}
}

func completeControllerPacket(packetID, bundleDigest string) visualhive.VerifiedPacketIdentity {
	return visualhive.VerifiedPacketIdentity{
		PacketID: packetID, PacketDigest: bundleDigest, ReplayKey: strings.Repeat("e", 64), Repository: "owner/repo", RepositoryID: "123",
		SourceRef: "refs/heads/main", BaseSHA: strings.Repeat("d", 40), WorkflowName: trustedWorkflowName, WorkflowEvent: "workflow_dispatch",
		WorkflowRunID: "42", WorkflowRunAttempt: "2", WorkflowArtifactID: "100", ProducerName: "visual-hive", ProducerVersion: "test",
		ProducerGitCommit: strings.Repeat("c", 40), ArtifactIndexSHA256: strings.Repeat("2", 64),
		GeneratedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2030, 7, 9, 12, 0, 0, 0, time.UTC), Verdict: "fail",
	}
}

func TestVisualEvidenceBindsManifestSourceArtifactSeparatelyFromBundleArtifact(t *testing.T) {
	digest := strings.Repeat("8", 64)
	packet := completeControllerPacket("distinct-artifacts", digest)
	evidence := completeControllerEvidence(digest)
	if evidence.ArtifactID == evidence.SourceArtifactID || packet.WorkflowArtifactID != "100" || evidence.ArtifactID != 99 || evidence.SourceArtifactID != 100 {
		t.Fatalf("fixture does not model distinct bundle/source artifacts: packet=%+v evidence=%+v", packet, evidence)
	}
	if err := validateVisualEvidenceForPacket(packet, evidence); err != nil {
		t.Fatalf("exact distinct artifact provenance was rejected: %v", err)
	}
	bundleAsSource := packet
	bundleAsSource.WorkflowArtifactID = "99"
	if err := validateVisualEvidenceForPacket(bundleAsSource, evidence); err == nil {
		t.Fatal("manifest source artifact identity was incorrectly matched to bundle artifact ID")
	}
	swapped := completeControllerEvidence(digest)
	swapped.ArtifactID, swapped.SourceArtifactID = swapped.SourceArtifactID, swapped.ArtifactID
	swapped.VerificationReceipt.ArtifactID, swapped.VerificationReceipt.SourceArtifactID = "100", "99"
	swapped.VerificationReceiptSHA256, _ = swapped.VerificationReceipt.SHA256()
	if err := validateVisualEvidenceForPacket(packet, swapped); err == nil {
		t.Fatal("swapped bundle/source artifact identities were accepted")
	}
}
