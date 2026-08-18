package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/knowledge"
)

type testCanonicalVerifiedReceipt struct {
	evidence  agent.SpecialistEvidenceIdentity
	canonical json.RawMessage
	exportErr error
}

type testWorkerSourceContextComposer struct {
	result           WorkerSealedSourceContext
	err              error
	requests         []WorkerSourceContextRequest
	evidenceResult   WorkerEvidenceArtifactReadResult
	evidenceErr      error
	evidenceRequests []WorkerEvidenceArtifactRequest
}

var _ WorkerSourceContextComposer = (*testWorkerSourceContextComposer)(nil)
var _ WorkerEvidenceArtifactReader = (*testWorkerSourceContextComposer)(nil)

func (composer *testWorkerSourceContextComposer) ComposeGovernedSourceContext(request WorkerSourceContextRequest) (WorkerSealedSourceContext, error) {
	composer.requests = append(composer.requests, request)
	if composer.err != nil {
		return WorkerSealedSourceContext{}, composer.err
	}
	result := composer.result
	result.Files = append([]WorkerSourceBlobIdentity(nil), composer.result.Files...)
	return result, nil
}

func (composer *testWorkerSourceContextComposer) ReadGovernedEvidenceArtifacts(request WorkerEvidenceArtifactRequest) (WorkerEvidenceArtifactReadResult, error) {
	request.Artifacts = append([]EvidenceArtifact(nil), request.Artifacts...)
	composer.evidenceRequests = append(composer.evidenceRequests, request)
	if composer.evidenceErr != nil {
		return WorkerEvidenceArtifactReadResult{}, composer.evidenceErr
	}
	result := WorkerEvidenceArtifactReadResult{Artifacts: append([]WorkerEvidenceArtifactContent(nil), composer.evidenceResult.Artifacts...)}
	return result, nil
}

var _ CanonicalVerifiedReceipt = (*testCanonicalVerifiedReceipt)(nil)

func (receipt *testCanonicalVerifiedReceipt) EvidenceIdentity() agent.SpecialistEvidenceIdentity {
	return receipt.evidence
}

func (receipt *testCanonicalVerifiedReceipt) CanonicalReceiptJSON() (json.RawMessage, error) {
	if receipt.exportErr != nil {
		return nil, receipt.exportErr
	}
	return append(json.RawMessage(nil), receipt.canonical...), nil
}

// testCanonicalV3Receipt mirrors the current verified bundle-v3 receipt
// identity shape. Production composition consumes bytes exported by the
// intake-owned canonical type and does not define this shape.
type testCanonicalV3Receipt struct {
	RepositoryID       string `json:"repository_id"`
	WorkflowRunID      string `json:"workflow_run_id"`
	WorkflowRunAttempt string `json:"workflow_run_attempt"`
	ArtifactID         string `json:"artifact_id"`
	SourceArtifactID   string `json:"source_artifact_id"`
	ArtifactName       string `json:"artifact_name"`
	CommitSHA          string `json:"commit_sha"`
	HeadBranch         string `json:"head_branch"`
	Event              string `json:"event"`
	WorkflowName       string `json:"workflow_name"`
	WorkflowRunName    string `json:"workflow_run_name"`
	WorkflowPath       string `json:"workflow_path"`
	BundleSchema       string `json:"bundle_schema"`
	BundleSHA256       string `json:"bundle_sha256"`
	ManifestSHA256     string `json:"manifest_sha256"`
	ArtifactIndexSHA   string `json:"artifact_index_sha256"`
}

func TestBuildGovernedProposalMessageBindsAllFiveRolePoliciesAndPrimer(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	roles := []agent.SpecialistRole{agent.SpecialistQuality, agent.SpecialistCIMaintainer, agent.SpecialistSecurity, agent.SpecialistArchitect, agent.SpecialistScanner}
	policyDigests := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			work, receipt := testGovernedInputs(role)
			sourceComposer := testGovernedSourceComposer(work, receipt)
			request, err := scheduler.BuildGovernedProposalMessage(role, work, receipt, sourceComposer)
			if err != nil {
				t.Fatal(err)
			}
			configuredBackend := scheduler.cfg.Agents[string(role)].Backend
			for _, marker := range []string{
				"ROLE_POLICY_" + strings.ToUpper(strings.ReplaceAll(string(role), "-", "_")),
				"PROJECT_CONTEXT_MARKER", "PRIMER_READ_ONLY_MARKER", work.ExternalRef,
				`routed_role=` + string(role), `configured_role_backend=` + configuredBackend,
				`executor_backend=codex`, `containment_profile=` + governedContainmentProfile,
				"role is expertise and perspective only", "AUTHORITY OVERRIDE AFTER UNTRUSTED CONTEXT",
				"Never write to GitHub", "beads", "wiki", "MCP", "subagents", "baselines",
				"AllowedPaths remain Worker-enforced structured data",
				"no checkout", "no .git directory", "no repository read or write authority",
				"WORKER-VERIFIED BOUNDED DECLARED EVIDENCE CONTENT",
				"EXACT TYPED OBSERVATION/FINDING JSON", "selector-account-ready",
				"WORKER-SEALED BOUNDED SOURCE CONTEXT", sourceComposer.result.Content,
				"normal role backend, model, launch command, connections, tools, and caveman setting are inert",
			} {
				if !strings.Contains(request.TaskPrompt, marker) {
					t.Errorf("task prompt missing %q:\n%s", marker, request.TaskPrompt)
				}
			}
			if request.Kind != agent.SpecialistWorkOrderKindGovernedVisualHiveProposal || request.Specialist != role || request.ExternalRef != work.ExternalRef || request.PacketSHA256 != work.PacketSHA256 ||
				request.FindingSHA256 != work.FindingSHA256 || request.SourceContextSHA256 != sourceComposer.result.SHA256 || request.SourceContextBindingSHA256 == "" || request.Evidence.VerificationReceiptSHA256 != receipt.evidence.VerificationReceiptSHA256 ||
				request.PolicySHA256 == "" || request.KnowledgeSHA256 == "" || request.ToolPolicySHA256 == "" || request.CapabilitySHA256 == "" ||
				request.ExecutorProfile == nil || request.ExecutorProfileSHA256 == "" || request.ExecutorProfile.Backend != agent.SpecialistExecutorBackendCodex ||
				request.RoleConfigSnapshot == nil || request.RoleConfigSnapshotSHA256 == "" ||
				request.ReproductionMode != agent.SpecialistReproductionModeNone || len(request.Reproduction) != 0 ||
				request.TaskPromptSHA256 != governedSHA256(request.TaskPrompt) {
				t.Fatalf("canonical work-order request lost bindings: %+v", request)
			}
			input := decodeGovernedPrompt(t, request.TaskPrompt)
			if input.ExternalRef != work.ExternalRef || input.AdmittedWorkJSON != string(work.Work) || input.AdmittedWorkSHA256 != work.WorkSHA256 ||
				input.PacketJSON != string(work.Packet) || input.FindingJSON != string(work.Finding) ||
				input.VerifiedReceiptJSON != string(receipt.canonical) || input.SourceContextSHA256 != sourceComposer.result.SHA256 || input.SourceContextBindingSHA256 != request.SourceContextBindingSHA256 || input.BaseSHA != work.BaseSHA || input.BaseTreeSHA != work.BaseTreeSHA ||
				input.RoutingReason != work.RoutingReason || input.RolePolicyExpertise == "" || input.ProjectContextSnapshot == "" ||
				input.KnowledgePrimerSnapshot == "" || input.PolicySHA256 != governedSHA256(input.RolePolicyExpertise) ||
				input.KnowledgeSHA256 != governedSHA256(input.KnowledgePrimerSnapshot) || input.Task != governedProposalTask ||
				input.ReproductionMode != agent.SpecialistReproductionModeNone {
				t.Fatalf("lossless governed prompt binding is invalid: %+v", input)
			}
			if strings.Contains(input.ProjectContextSnapshot, "AUTHORIZED REPOS") || strings.Contains(input.ProjectContextSnapshot, "you may") {
				t.Fatalf("project identity snapshot contains repository access instructions: %q", input.ProjectContextSnapshot)
			}
			sourceBindingSHA, err := governedJSONDigest(input.SourceContextBinding)
			if err != nil {
				t.Fatal(err)
			}
			if request.SourceContextBindingSHA256 != sourceBindingSHA || input.SourceContextBindingSHA256 != sourceBindingSHA ||
				input.SourceContextBinding.BaseSHA != work.BaseSHA || input.SourceContextBinding.BaseTreeSHA != work.BaseTreeSHA ||
				input.SourceContextBinding.ManifestSHA256 != receipt.evidence.ArtifactSHA256 || input.SourceContextBinding.VerificationReceiptSHA256 != receipt.evidence.VerificationReceiptSHA256 ||
				len(input.SourceContextBinding.Files) != 1 || input.SourceContextBinding.Files[0].BlobOID == "" {
				t.Fatalf("Worker source-context binding is incomplete: %+v", input.SourceContextBinding)
			}
			capabilitySHA, err := governedJSONDigest(input.Capability)
			if err != nil {
				t.Fatal(err)
			}
			if input.Capability.RoutedRole != role || input.Capability.ConfiguredRoleBackend != configuredBackend ||
				input.Capability.ConfiguredRoleModel != scheduler.cfg.Agents[string(role)].Model || input.Capability.ConfiguredRoleSnapshotClass != governedRoleSnapshotClass ||
				input.Capability.ExecutorBackend != agent.SpecialistExecutorBackendCodex || input.Capability.ExecutorModel != request.ExecutorProfile.Model ||
				input.Capability.ExecutorConfigSHA256 != request.ExecutorProfile.ConfigurationSHA256 || input.Capability.ExecutorProfileSHA256 != request.ExecutorProfileSHA256 ||
				input.Capability.ContainmentProfile != governedContainmentProfile ||
				input.Capability.BackendParityClaimed || input.Capability.SourceContextMode != "worker-sealed-bounded-regular-git-blobs" ||
				input.Capability.EvidenceContentMode != "worker-verified-declared-json-artifacts" ||
				input.Capability.ReproductionMode != agent.SpecialistReproductionModeNone || !input.Capability.Authority.SealedSourceContextRead ||
				input.Capability.Authority.RepositoryRead || input.Capability.Authority.DisposableWorktreeWrite ||
				input.Capability.Authority.RunReproduction || input.Capability.Authority.RunValidation ||
				input.CapabilitySHA256 != capabilitySHA || request.CapabilitySHA256 != capabilitySHA {
				t.Fatalf("truthful governed capability binding is invalid: %+v", input.Capability)
			}
			artifacts := make(map[string]string, len(input.EvidenceArtifacts))
			for _, artifact := range input.EvidenceArtifacts {
				artifacts[artifact.Reference] = artifact.SHA256
			}
			if artifacts["hive:verified-manifest"] != receipt.evidence.ArtifactSHA256 ||
				artifacts["hive:canonical-verification-receipt"] != receipt.evidence.VerificationReceiptSHA256 ||
				artifacts[".visual-hive/evidence/account.json"] != testGovernedSHA([]byte(testGovernedEvidenceContent)) {
				t.Fatalf("controller-owned verified evidence artifacts were not composed: %v", artifacts)
			}
			if len(input.EvidenceArtifactContents) != 1 || input.EvidenceArtifactContents[0].Content != testGovernedEvidenceContent || input.EvidenceArtifactContentSHA == "" || len(sourceComposer.evidenceRequests) != 1 {
				t.Fatalf("bounded declared evidence content was not composed exactly: input=%+v requests=%+v", input.EvidenceArtifactContents, sourceComposer.evidenceRequests)
			}
			roleConfig := scheduler.cfg.Agents[string(role)]
			expectedToolPolicy := governedToolPolicy{
				SnapshotClass: governedRoleSnapshotClass,
				Backend:       roleConfig.Backend,
				Model:         roleConfig.Model,
				Mode:          roleConfig.Mode,
				LaunchCmd:     roleConfig.LaunchCmd,
				CavemanMode:   roleConfig.CavemanMode,
				IncludeRepos:  roleConfig.ShouldIncludeRepos(),
				Connections:   append([]config.ConnectionConfig(nil), roleConfig.Connections...),
			}
			if roleConfig.Tools != nil {
				expectedToolPolicy.Preset = roleConfig.Tools.Preset
				expectedToolPolicy.Rules = append([]config.ToolRule(nil), roleConfig.Tools.EffectiveRules()...)
			}
			expectedToolSHA, err := governedJSONDigest(expectedToolPolicy)
			if err != nil {
				t.Fatal(err)
			}
			if request.ToolPolicySHA256 != expectedToolSHA || input.Capability.ToolPolicySHA256 != expectedToolSHA {
				t.Fatalf("configured role tool/connection profile was not bound: got %s want %s", request.ToolPolicySHA256, expectedToolSHA)
			}
			roleSnapshotSHA, err := governedJSONDigest(*request.RoleConfigSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			if request.RoleConfigSnapshot.FullConfigSHA256 != expectedToolSHA || request.RoleConfigSnapshotSHA256 != roleSnapshotSHA ||
				input.Capability.ConfiguredRoleSnapshotSHA256 != roleSnapshotSHA || !request.RoleConfigSnapshot.LaunchCmdPresent || request.RoleConfigSnapshot.LaunchCmdSHA256 == "" {
				t.Fatalf("canonical inert role snapshot is not independently comparable: request=%+v capability=%+v", request.RoleConfigSnapshot, input.Capability)
			}
			if len(sourceComposer.requests) != 1 || sourceComposer.requests[0].BaseSHA != work.BaseSHA || sourceComposer.requests[0].BaseTreeSHA != work.BaseTreeSHA ||
				sourceComposer.requests[0].ManifestSHA256 != receipt.evidence.ArtifactSHA256 || sourceComposer.requests[0].VerificationReceiptSHA256 != receipt.evidence.VerificationReceiptSHA256 {
				t.Fatalf("Worker source-context composer did not receive exact evidence/base bindings: %+v", sourceComposer.requests)
			}
			policyDigests[request.PolicySHA256] = struct{}{}
		})
	}
	if len(policyDigests) != len(roles) {
		t.Fatalf("all five specialists did not retain distinct role policies: %v", policyDigests)
	}
}

func TestBuildGovernedProposalMessageConsumesCanonicalV3ReceiptBytes(t *testing.T) {
	// This branch can characterize exact byte transport only. The intake-owned
	// visual-hive-v3 identity alias must supply and cross-check all outer fields
	// when branches are reconciled; this test is not a substitute for that gate.
	scheduler := testGovernedScheduler(t)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	request, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt))
	if err != nil {
		t.Fatal(err)
	}
	input := decodeGovernedPrompt(t, request.TaskPrompt)
	wantDigest := testGovernedSHA(receipt.canonical)
	if input.VerifiedReceiptJSON != string(receipt.canonical) || input.VerifiedReceiptSHA256 != wantDigest ||
		request.Evidence.VerificationReceiptSHA256 != wantDigest {
		t.Fatalf("canonical receipt bytes and shared evidence digest diverged: input=%+v evidence=%+v", input, request.Evidence)
	}

	tampered := *receipt
	tampered.canonical = append(append(json.RawMessage(nil), receipt.canonical...), ' ')
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, &tampered, testGovernedSourceComposer(work, &tampered)); err == nil {
		t.Fatal("canonical receipt bytes that do not match the shared evidence digest were accepted")
	}
	var nilReceipt *testCanonicalVerifiedReceipt
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, nilReceipt, testGovernedSourceComposer(work, receipt)); err == nil {
		t.Fatal("typed-nil canonical receipt adapter was accepted")
	}
}

func TestBuildGovernedProposalMessageComposesNoKnowledgeAndControllerReproduction(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	scheduler.SetPrimer(nil)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	request, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt))
	if err != nil {
		t.Fatal(err)
	}
	input := decodeGovernedPrompt(t, request.TaskPrompt)
	if input.KnowledgePrimerSnapshot != noApplicableKnowledgeSnapshot || request.KnowledgeSHA256 != governedSHA256(noApplicableKnowledgeSnapshot) ||
		strings.Join(input.KnowledgeKeywords, ",") != "visual_regression,quality,account-shell-visual" ||
		request.ReproductionMode != agent.SpecialistReproductionModeNone || len(request.Reproduction) != 0 {
		t.Fatalf("explicit no-knowledge/no-reproduction composition is invalid: input=%+v request=%+v", input, request)
	}

	work.ValidationMayReproduce = true
	withReproduction, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt))
	if err != nil {
		t.Fatal(err)
	}
	if withReproduction.ReproductionMode != agent.SpecialistReproductionModeVerifiedValidation ||
		strings.Join(withReproduction.Reproduction, "\n") != strings.Join(work.Validation, "\n") {
		t.Fatalf("verified validation was not deliberately composed as controller reproduction: %+v", withReproduction)
	}
}

func TestGovernedProposalUsesCanonicalMailboxIdentityWithoutConflatingExternalRef(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	request, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt))
	if err != nil {
		t.Fatal(err)
	}
	mailboxRoot := filepath.Join(t.TempDir(), "mailbox")
	mailbox, err := agent.NewSpecialistMailbox(mailboxRoot, agent.SpecialistMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	order, err := mailbox.PrepareGoverned(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(order.ID, "swo-") || order.ExternalRef != "visual-hive:packet-42/finding-7" || order.ExternalRef == order.ID {
		t.Fatalf("source and specialist identities were conflated: %+v", order)
	}
	// Replay must load the exact persisted request. Live role, primer, and tool
	// changes are deliberately irrelevant after PrepareGoverned returns the ID.
	scheduler.SetPrimer(nil)
	changedRole := scheduler.cfg.Agents[string(agent.SpecialistQuality)]
	changedRole.Model = "live-model-changed-after-prepare"
	scheduler.cfg.Agents[string(agent.SpecialistQuality)] = changedRole
	second, err := agent.NewSpecialistMailbox(mailboxRoot, agent.SpecialistMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := second.LoadWorkOrder(order.ID)
	if err != nil || loaded.ID != order.ID || loaded.RequestSHA256 != order.RequestSHA256 || loaded.TaskPrompt != order.TaskPrompt || !loaded.Deadline.Equal(work.Deadline.UTC()) {
		t.Fatalf("replay did not load the exact persisted composed request: loaded=%+v err=%v", loaded, err)
	}
	replayed, err := second.PrepareGoverned(request)
	if err != nil || replayed.ID != order.ID {
		t.Fatalf("canonical mapping was not deterministic: first=%s replay=%s err=%v", order.ID, replayed.ID, err)
	}
	request.ExternalRef += "/changed"
	changed, err := second.PrepareGoverned(request)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID == order.ID {
		t.Fatal("external source identity was not bound into the existing swo ID")
	}
}

func TestBuildGovernedProposalMessageFailsClosedOnMissingSecurityBindings(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	validWork, validReceipt := testGovernedInputs(agent.SpecialistQuality)
	tests := map[string]func(*AdmittedWork, *testCanonicalVerifiedReceipt){
		"verified route": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.RoutedRole = agent.SpecialistScanner },
		"external ref":   func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.ExternalRef = "" },
		"admitted work":  func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Work = nil },
		"admitted work digest": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.WorkSHA256 = strings.Repeat("0", 64)
		},
		"repository":    func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Repository = "acme/other" },
		"fingerprint":   func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.RepositoryFingerprint = "" },
		"packet digest": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.PacketSHA256 = strings.Repeat("0", 64) },
		"finding":       func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Finding = nil },
		"receipt digest": func(_ *AdmittedWork, receipt *testCanonicalVerifiedReceipt) {
			receipt.evidence.VerificationReceiptSHA256 = strings.Repeat("1", 64)
		},
		"receipt export": func(_ *AdmittedWork, receipt *testCanonicalVerifiedReceipt) {
			receipt.exportErr = errors.New("unavailable")
		},
		"base tree":      func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.BaseTreeSHA = "" },
		"routing reason": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.RoutingReason = "" },
		"observation kind": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.ObservationKind = "unsafe prose with spaces"
		},
		"deadline":       func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Deadline = time.Time{} },
		"paths":          func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.AllowedPaths = nil },
		"path traversal": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.AllowedPaths = []string{"../outside"} },
		"validation":     func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.Validation = nil },
		"contracts":      func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.AffectedContracts = nil },
		"base identity":  func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) { work.BaseSHA = strings.Repeat("0", 40) },
		"artifact collision": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.EvidenceArtifacts[0].Reference = "hive:verified-manifest"
		},
		"artifact digest": func(work *AdmittedWork, _ *testCanonicalVerifiedReceipt) {
			work.EvidenceArtifacts[0].SHA256 = ""
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			work := validWork
			receipt := *validReceipt
			work.AllowedPaths = append([]string(nil), validWork.AllowedPaths...)
			work.Validation = append([]string(nil), validWork.Validation...)
			work.AffectedContracts = append([]string(nil), validWork.AffectedContracts...)
			work.EvidenceArtifacts = append([]EvidenceArtifact(nil), validWork.EvidenceArtifacts...)
			receipt.canonical = append(json.RawMessage(nil), validReceipt.canonical...)
			mutate(&work, &receipt)
			if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, &receipt, testGovernedSourceComposer(work, &receipt)); err == nil {
				t.Fatal("invalid governed proposal input was accepted")
			}
		})
	}
}

func TestBuildGovernedProposalMessageRequiresWorkerOwnedCrossBoundSourceContext(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, nil); err == nil || !strings.Contains(err.Error(), "Worker source-context composer") {
		t.Fatalf("controller-prebuilt path did not fail closed without Worker composer: %v", err)
	}
	var typedNil *testWorkerSourceContextComposer
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, typedNil); err == nil {
		t.Fatal("typed-nil Worker source-context composer was accepted")
	}
	failing := testGovernedSourceComposer(work, receipt)
	failing.err = errors.New("sealed tree unavailable")
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, failing); err == nil || !strings.Contains(err.Error(), "sealed tree unavailable") {
		t.Fatalf("Worker source-context failure was not preserved: %v", err)
	}

	mutations := map[string]func(*WorkerSealedSourceContext){
		"content digest": func(value *WorkerSealedSourceContext) { value.SHA256 = strings.Repeat("0", 64) },
		"base":           func(value *WorkerSealedSourceContext) { value.BaseSHA = strings.Repeat("0", 40) },
		"tree":           func(value *WorkerSealedSourceContext) { value.BaseTreeSHA = strings.Repeat("0", 40) },
		"manifest":       func(value *WorkerSealedSourceContext) { value.ManifestSHA256 = strings.Repeat("0", 64) },
		"receipt":        func(value *WorkerSealedSourceContext) { value.VerificationReceiptSHA256 = strings.Repeat("0", 64) },
		"inventory":      func(value *WorkerSealedSourceContext) { value.Files = nil },
		"blob":           func(value *WorkerSealedSourceContext) { value.Files[0].BlobOID = "not-a-blob" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			composer := testGovernedSourceComposer(work, receipt)
			mutate(&composer.result)
			if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, composer); err == nil {
				t.Fatal("unbound Worker source context was accepted")
			}
		})
	}
}

func TestGovernedMutationAndTestAdequacyRequireStructuredEvidenceContent(t *testing.T) {
	for _, issueKind := range []string{"mutation_survivor", "test_adequacy_gap"} {
		t.Run(issueKind, func(t *testing.T) {
			composer := &testWorkerSourceContextComposer{evidenceResult: WorkerEvidenceArtifactReadResult{Artifacts: []WorkerEvidenceArtifactContent{}}}
			work := AdmittedWork{
				ObservationKind: issueKind,
				EvidenceArtifacts: []EvidenceArtifact{{
					Reference: ".visual-hive/screenshots/failure.png", SHA256: strings.Repeat("a", 64),
					Bytes: 128, Kind: "image", ContentType: "image/png",
				}},
			}
			if _, _, err := composeWorkerEvidenceArtifacts(work, composer); err == nil || !strings.Contains(err.Error(), "none was safely embeddable") {
				t.Fatalf("%s accepted an empty structured evidence subset: %v", issueKind, err)
			}
		})
	}
}

func TestBuildGovernedProposalMessageTreatsNormalRoleExecutionConfigAsInert(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	for _, role := range []agent.SpecialistRole{agent.SpecialistQuality, agent.SpecialistCIMaintainer} {
		work, receipt := testGovernedInputs(role)
		request, err := scheduler.BuildGovernedProposalMessage(role, work, receipt, testGovernedSourceComposer(work, receipt))
		if err != nil {
			t.Fatalf("normal %s role config prevented governed composition: %v", role, err)
		}
		configured := scheduler.cfg.Agents[string(role)]
		input := decodeGovernedPrompt(t, request.TaskPrompt)
		if input.Capability.ConfiguredRoleBackend != configured.Backend || !input.Capability.ConfiguredRoleLaunchCmdPresent ||
			input.Capability.ConfiguredRoleConnectionCount != len(configured.Connections) || input.Capability.ConfiguredRoleCavemanMode != configured.CavemanMode ||
			input.Capability.ExecutorBackend != agent.SpecialistExecutorBackendCodex || input.Capability.BackendParityClaimed ||
			request.ExecutorProfile == nil || request.ExecutorProfile.Backend != agent.SpecialistExecutorBackendCodex {
			t.Fatalf("configured role metadata selected or granted executor capability: %+v", input.Capability)
		}
		if strings.Contains(request.TaskPrompt, configured.LaunchCmd) || strings.Contains(request.TaskPrompt, configured.Connections[0].URI) {
			t.Fatal("raw configured role launch/connection data leaked into the proposal prompt")
		}
	}
}

func TestGovernedProposalExecutorProfileFailsClosedUnlessExplicitCodex(t *testing.T) {
	missing := testGovernedScheduler(t)
	missing.governedExecutor = nil
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	if _, err := missing.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt)); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("missing proposal executor profile was accepted: %v", err)
	}
	for _, backend := range []string{"copilot", "claude", "inference"} {
		t.Run(backend, func(t *testing.T) {
			scheduler := testGovernedScheduler(t)
			profile := testGovernedExecutorProfile()
			profile.Backend = backend
			if err := scheduler.SetGovernedProposalExecutorProfile(profile); err == nil || !strings.Contains(err.Error(), "must be codex") {
				t.Fatalf("non-Codex proposal executor was accepted: %v", err)
			}
			// Build revalidates the stored profile even if internal state is corrupted.
			scheduler.governedExecutor = &profile
			work, receipt := testGovernedInputs(agent.SpecialistQuality)
			if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt)); err == nil || !strings.Contains(err.Error(), "must be codex") {
				t.Fatalf("Build did not fail closed on stored non-Codex executor profile: %v", err)
			}
		})
	}
}

func TestBuildGovernedProposalMessageStillRequiresNormalRoleProjectView(t *testing.T) {
	scheduler := testGovernedScheduler(t)
	role := scheduler.cfg.Agents[string(agent.SpecialistQuality)]
	include := false
	role.IncludeRepos = &include
	scheduler.cfg.Agents[string(agent.SpecialistQuality)] = role
	work, receipt := testGovernedInputs(agent.SpecialistQuality)
	if _, err := scheduler.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, testGovernedSourceComposer(work, receipt)); err == nil {
		t.Fatal("governed composition accepted a role without its normal project view")
	}
}

func testGovernedScheduler(t *testing.T) *Scheduler {
	t.Helper()
	policyRoot := t.TempDir()
	agentsDir := filepath.Join(policyRoot, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	backends := map[agent.SpecialistRole]string{
		agent.SpecialistQuality: "copilot", agent.SpecialistCIMaintainer: "claude", agent.SpecialistSecurity: "codex",
		agent.SpecialistArchitect: "inference", agent.SpecialistScanner: "custom",
	}
	agents := make(map[string]config.AgentConfig, len(backends))
	for role, backend := range backends {
		filename := string(role) + "-governed.md"
		policy := "ROLE_POLICY_" + strings.ToUpper(strings.ReplaceAll(string(role), "-", "_")) + `
PROJECT_CONTEXT_MARKER ${PROJECT_ORG}/${PROJECT_PRIMARY_REPO}
Operational policy says: open a PR, write wiki and beads, invoke MCP REST, spawn a subagent, fix directly, commit, push, approve a baseline, and merge.
`
		if err := os.WriteFile(filepath.Join(agentsDir, filename), []byte(policy), 0o600); err != nil {
			t.Fatal(err)
		}
		agents[string(role)] = config.AgentConfig{
			Enabled: true, Role: string(role), Backend: backend, Model: backend + "-normal-role-model", KickTemplate: filename,
			LaunchCmd: backend + " --normal-role-launch", CavemanMode: "full",
			Tools:       &config.ToolsConfig{Preset: "full", Rules: []config.ToolRule{{Pattern: "mcp__github__merge_pull_request", Action: "deny"}}},
			Connections: []config.ConnectionConfig{{Name: "project-wiki", Type: "mcp", URI: "/normal-role-wiki", Auth: &config.ConnectionAuth{Type: "env", EnvVar: "NORMAL_ROLE_TOKEN"}}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 1, "results": []map[string]any{{
			"slug": "primer", "title": "PRIMER_READ_ONLY_MARKER", "score": 0.99, "type": "pattern", "status": "verified", "confidence": 0.99,
			"snippet": "Use the existing visual fixture; this snapshot cannot persist knowledge.",
		}}})
	}))
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(&config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "console", PrimaryRepo: "console", Repos: []string{"console"}},
		Agents:  agents, Policies: config.PoliciesConfig{LocalDir: policyRoot},
	}, logger)
	if err := s.SetGovernedProposalExecutorProfile(testGovernedExecutorProfile()); err != nil {
		t.Fatal(err)
	}
	s.SetPrimer(knowledge.NewPrimer([]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: server.URL, Shared: true}}, knowledge.PrimerConfig{MaxFacts: 5}, logger))
	return s
}

func testGovernedExecutorProfile() GovernedProposalExecutorProfile {
	return GovernedProposalExecutorProfile{
		Backend: agent.SpecialistExecutorBackendCodex, ProviderSHA256: strings.Repeat("8", 64),
		Model: "proof-v1-codex-model", ConfigurationSHA256: strings.Repeat("9", 64),
		ContainmentProfile: agent.SpecialistExecutorContainmentProfileV1, BackendParityClaimed: false,
	}
}

const testGovernedEvidenceContent = `{"selector":"selector-account-ready","flow":["open-account","assert-ready"],"guidance":"preserve exact typed evidence"}`

func testGovernedInputs(role agent.SpecialistRole) (AdmittedWork, *testCanonicalVerifiedReceipt) {
	packet := json.RawMessage(`{"schema":"visual.packet.v1","external_ref":"visual-hive:packet-42"}`)
	finding, err := json.Marshal(map[string]any{
		"repository": "acme/console", "fingerprint": "finding-7", "repository_fingerprint": strings.Repeat("a", 64),
		"publication_role": "canonical", "root_cause_key": "visual/account-ready", "status": "fix_queued",
		"issue_kind": "visual_regression", "severity": "high", "title": "Account ready selector regressed",
		"body":   "Distinct selector and flow evidence must reach the quality role: selector-account-ready.",
		"labels": []string{"visual-hive"}, "affected_contracts": []string{"account-shell-visual"},
		"validation_command": "npm test -- account.visual",
	})
	if err != nil {
		panic(err)
	}
	artifact := EvidenceArtifact{
		Reference: ".visual-hive/evidence/account.json", SHA256: testGovernedSHA([]byte(testGovernedEvidenceContent)),
		Bytes: int64(len([]byte(testGovernedEvidenceContent))), Kind: "json", ContentType: "application/json",
	}
	admittedWork, err := json.Marshal(map[string]any{
		"source_external_ref": "visual-hive:packet-42/finding-7", "packet": packet,
		"finding_fingerprint": "finding-7", "repository_fingerprint": strings.Repeat("a", 64),
		"observation_state": "present", "publication_role": "canonical", "root_cause_key": "visual/account-ready",
		"blocked_by_root_keys": []string{}, "issue_kind": "visual_regression", "severity": "high",
		"title": "Account ready selector regressed", "body": "Distinct selector and flow evidence must reach the quality role: selector-account-ready.",
		"labels": []string{"visual-hive"}, "role": string(role), "routing_reason": "verified route owner", "routing_allowed": true,
		"affected_contracts": []string{"account-shell-visual"}, "knowledge_keywords": []string{"selector-account-ready"},
		"evidence_artifacts":    []map[string]any{{"path": artifact.Reference, "sha256": artifact.SHA256, "bytes": artifact.Bytes, "kind": artifact.Kind, "content_type": artifact.ContentType}},
		"reproduction_commands": []string{"npm test -- account.visual"}, "reproduction_source": "verified_observation_validation_command",
		"validation_commands": []string{"npm test -- account.visual"},
		"authority":           map[string]any{"proposal_only": true, "github_write_allowed": false, "merge_allowed": false, "baseline_changes_allowed": false, "baseline_approval_allowed": false},
	})
	if err != nil {
		panic(err)
	}
	receiptBytes, err := json.Marshal(testCanonicalV3Receipt{
		RepositoryID: "R_acme_console", WorkflowRunID: "88", WorkflowRunAttempt: "2",
		ArtifactID: "77", SourceArtifactID: "78", ArtifactName: "visual-hive-evidence",
		CommitSHA: strings.Repeat("b", 40), HeadBranch: "main", Event: "workflow_dispatch",
		WorkflowName: "Visual Hive", WorkflowRunName: "visual-hive-88",
		WorkflowPath: ".github/workflows/visual-hive.yml", BundleSchema: "visual-hive.hive-bundle.v3",
		BundleSHA256: strings.Repeat("d", 64), ManifestSHA256: strings.Repeat("e", 64), ArtifactIndexSHA: strings.Repeat("f", 64),
	})
	if err != nil {
		panic(err)
	}
	receiptSHA := testGovernedSHA(receiptBytes)
	return AdmittedWork{
			ExternalRef: "visual-hive:packet-42/finding-7", Work: admittedWork, WorkSHA256: testGovernedSHA(admittedWork),
			Packet: packet, PacketSHA256: testGovernedSHA(packet), Finding: finding, FindingSHA256: testGovernedSHA(finding),
			Repository: "acme/console", RepositoryFingerprint: strings.Repeat("a", 64),
			RecurrenceKey: "visual-hive:packet-42/finding-7:recurrence-2", Attempt: 2,
			BaseSHA: strings.Repeat("b", 40), BaseTreeSHA: strings.Repeat("c", 40), RoutedRole: role, RoutingReason: "verified route owner",
			ObservationKind: "visual_regression",
			AllowedPaths:    []string{"web/account.tsx", "web/account.visual.test.ts"}, Validation: []string{"npm test -- account.visual"},
			AffectedContracts: []string{"account-shell-visual"},
			EvidenceArtifacts: []EvidenceArtifact{artifact},
			Deadline:          time.Now().UTC().Add(time.Hour),
		}, &testCanonicalVerifiedReceipt{
			canonical: receiptBytes,
			evidence: agent.SpecialistEvidenceIdentity{
				BundleSchemaVersion: "visual-hive.hive-bundle.v3", BundleSHA256: strings.Repeat("d", 64), VerificationReceiptSHA256: receiptSHA,
				WorkflowRunID: 88, WorkflowRunAttempt: 2, WorkflowRunHeadSHA: strings.Repeat("b", 40),
				ArtifactID: 77, ArtifactName: "visual-hive-evidence", ArtifactSHA256: strings.Repeat("e", 64),
			},
		}
}

func testGovernedSourceComposer(work AdmittedWork, receipt *testCanonicalVerifiedReceipt) *testWorkerSourceContextComposer {
	content := "HIVE_UNTRUSTED_SOURCE_CONTEXT_BEGIN\n" + `{"format":"hive-repair-source-context-v2","source_tree_oid":"` + work.BaseTreeSHA + `","files":[{"path":"web/account.tsx","blob_oid":"` + strings.Repeat("1", 40) + `","content":"export const account = true;\n"}]}` + "\nHIVE_UNTRUSTED_SOURCE_CONTEXT_END"
	return &testWorkerSourceContextComposer{result: WorkerSealedSourceContext{
		Content: content, SHA256: testGovernedSHA([]byte(content)), BaseSHA: work.BaseSHA, BaseTreeSHA: work.BaseTreeSHA,
		ManifestSHA256: receipt.evidence.ArtifactSHA256, VerificationReceiptSHA256: receipt.evidence.VerificationReceiptSHA256,
		Files: []WorkerSourceBlobIdentity{{Path: "web/account.tsx", BlobOID: strings.Repeat("1", 40), ContentSHA256: testGovernedSHA([]byte("export const account = true;\n")), Bytes: 29}},
	}, evidenceResult: WorkerEvidenceArtifactReadResult{Artifacts: []WorkerEvidenceArtifactContent{{
		Reference: work.EvidenceArtifacts[0].Reference, SHA256: work.EvidenceArtifacts[0].SHA256, Bytes: work.EvidenceArtifacts[0].Bytes,
		Kind: work.EvidenceArtifacts[0].Kind, ContentType: work.EvidenceArtifacts[0].ContentType, Content: testGovernedEvidenceContent,
	}}}}
}

func testGovernedSHA(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func decodeGovernedPrompt(t *testing.T, prompt string) governedPromptInput {
	t.Helper()
	_, encoded, ok := strings.Cut(prompt, "CANONICAL LOSSLESS INPUT JSON\n")
	if !ok {
		t.Fatal("governed prompt has no canonical input marker")
	}
	encoded, _, ok = strings.Cut(encoded, "\n\nEXACT TYPED OBSERVATION/FINDING JSON")
	if !ok {
		t.Fatal("governed prompt has no sealed source-context marker")
	}
	var input governedPromptInput
	if err := json.Unmarshal([]byte(encoded), &input); err != nil {
		t.Fatalf("decode canonical governed prompt input: %v", err)
	}
	return input
}
