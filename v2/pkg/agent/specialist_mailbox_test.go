package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const testSpecialistDiff = "diff --git a/tests/widget_test.go b/tests/widget_test.go\n--- a/tests/widget_test.go\n+++ b/tests/widget_test.go\n@@ -1 +1 @@\n-old\n+new\n"

func TestSpecialistMailboxPrepareIsContentBoundAndIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "finding:visual:7", now.Add(2*time.Hour))

	first, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	second, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare(idempotent) error = %v", err)
	}
	if !equalJSON(first, second) {
		t.Fatalf("idempotent orders differ:\n%+v\n%+v", first, second)
	}
	if first.SchemaVersion != SpecialistWorkOrderSchema || first.ID != "swo-"+first.RequestSHA256 || !digestPattern.MatchString(first.RequestSHA256) {
		t.Fatalf("unexpected content identity: %+v", first)
	}
	if err := mailbox.ValidateRequestForOrder(first, request); err != nil {
		t.Fatalf("ValidateRequestForOrder(exact) error = %v", err)
	}
	if got := strings.Join(first.AllowedPaths, ","); got != "src/widget.ts,tests/widget_test.go" {
		t.Fatalf("canonical allowed paths = %q", got)
	}
	paths, err := mailbox.Paths(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateMode(t, paths.OrderDirectory, 0o700)
	assertPrivateMode(t, paths.Order, 0o600)

	otherMailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	other, err := otherMailbox.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID != first.ID || other.RequestSHA256 != first.RequestSHA256 {
		t.Fatalf("same request was not deterministic: %s != %s", first.ID, other.ID)
	}

	mutations := map[string]func(*SpecialistWorkOrderRequest){
		"repository":  func(value *SpecialistWorkOrderRequest) { value.Repository = "acme/other" },
		"fingerprint": func(value *SpecialistWorkOrderRequest) { value.RepositoryFingerprint = strings.Repeat("9", 64) },
		"recurrence":  func(value *SpecialistWorkOrderRequest) { value.RecurrenceKey = "finding:visual:8" },
		"attempt":     func(value *SpecialistWorkOrderRequest) { value.Attempt++ },
		"base commit": func(value *SpecialistWorkOrderRequest) { value.BaseSHA = strings.Repeat("d", 40) },
		"base tree":   func(value *SpecialistWorkOrderRequest) { value.BaseTreeSHA = strings.Repeat("e", 40) },
		"bundle":      func(value *SpecialistWorkOrderRequest) { value.Evidence.BundleSHA256 = strings.Repeat("8", 64) },
		"run":         func(value *SpecialistWorkOrderRequest) { value.Evidence.WorkflowRunID++ },
		"artifact":    func(value *SpecialistWorkOrderRequest) { value.Evidence.ArtifactID++ },
		"role":        func(value *SpecialistWorkOrderRequest) { value.Specialist = SpecialistScanner },
		"reason":      func(value *SpecialistWorkOrderRequest) { value.RouteReason = "general discovery" },
		"paths":       func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = append(value.AllowedPaths, "README.md") },
		"validation":  func(value *SpecialistWorkOrderRequest) { value.Validation = append(value.Validation, "go vet ./...") },
		"prompt": func(value *SpecialistWorkOrderRequest) {
			value.TaskPrompt += "\nExplain the evidence."
			value.TaskPromptSHA256 = sha256HexString(value.TaskPrompt)
		},
		"deadline": func(value *SpecialistWorkOrderRequest) { value.Deadline = value.Deadline.Add(time.Minute) },
	}
	for name, mutate := range mutations {
		t.Run("binds_"+name, func(t *testing.T) {
			changed := cloneSpecialistRequest(request)
			mutate(&changed)
			order, err := mailbox.Prepare(changed)
			if err != nil {
				t.Fatalf("Prepare(changed) error = %v", err)
			}
			if order.ID == first.ID || order.RequestSHA256 == first.RequestSHA256 {
				t.Fatalf("mutation %q did not change content identity", name)
			}
			if err := mailbox.ValidateRequestForOrder(first, changed); err == nil {
				t.Fatalf("ValidateRequestForOrder accepted %q drift", name)
			}
		})
	}
	now = request.Deadline.Add(time.Minute)
	if err := mailbox.ValidateRequestForOrder(first, request); err != nil {
		t.Fatalf("expired exact order could not be inspected safely: %v", err)
	}
}

func TestSpecialistMailboxReadsAndPreparesPersistedOrdinaryV1WithoutGovernedFields(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 20, 0, 0, time.UTC)
	source := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "failed-pr-revision:18", now.Add(2*time.Hour))
	request.RouteReason = "existing failed-PR revision route"
	order, err := source.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	sourcePaths, err := source.Paths(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(sourcePaths.Order)
	if err != nil {
		t.Fatal(err)
	}
	for _, governedField := range []string{
		"kind", "external_ref", "packet_sha256", "finding_sha256", "source_context_sha256", "source_context_binding_sha256", "authority_class",
		"policy_sha256", "knowledge_sha256", "tool_policy_sha256", "capability_sha256",
		"executor_profile", "executor_profile_sha256", "role_config_snapshot", "role_config_snapshot_sha256",
		"reproduction_mode", "reproduction", "affected_contracts", "knowledge_keywords",
	} {
		if strings.Contains(string(fixture), `"`+governedField+`"`) {
			t.Fatalf("ordinary persisted v1 unexpectedly contains governed field %q: %s", governedField, fixture)
		}
	}

	target := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	targetPaths, err := target.Paths(order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetPaths.OrderDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPaths.Order, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := target.LoadWorkOrder(order.ID)
	if err != nil {
		t.Fatalf("LoadWorkOrder(old v1 fixture) error = %v", err)
	}
	prepared, err := target.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare(old v1 ordinary request) error = %v", err)
	}
	if !equalJSON(loaded, order) || !equalJSON(prepared, order) {
		t.Fatalf("persisted ordinary v1 changed during compatibility round trip:\nloaded=%+v\nprepared=%+v\nwant=%+v", loaded, prepared, order)
	}
}

func TestSpecialistMailboxPrepareGovernedBindsAndRequiresSecurityContext(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testGovernedSpecialistRequest(now, "visual:packet-7/finding-3", now.Add(2*time.Hour))

	first, err := mailbox.PrepareGoverned(request)
	if err != nil {
		t.Fatalf("PrepareGoverned() error = %v", err)
	}
	if first.ExternalRef != request.ExternalRef || first.AuthorityClass != request.AuthorityClass || first.ID != "swo-"+first.RequestSHA256 {
		t.Fatalf("governed identity was not carried by the canonical work order: %+v", first)
	}

	mutations := map[string]func(*SpecialistWorkOrderRequest){
		"external ref":           func(value *SpecialistWorkOrderRequest) { value.ExternalRef += "/changed" },
		"packet":                 func(value *SpecialistWorkOrderRequest) { value.PacketSHA256 = strings.Repeat("7", 64) },
		"finding":                func(value *SpecialistWorkOrderRequest) { value.FindingSHA256 = strings.Repeat("8", 64) },
		"source context":         func(value *SpecialistWorkOrderRequest) { value.SourceContextSHA256 = strings.Repeat("d", 64) },
		"source context binding": func(value *SpecialistWorkOrderRequest) { value.SourceContextBindingSHA256 = strings.Repeat("e", 64) },
		"authority":              func(value *SpecialistWorkOrderRequest) { value.AuthorityClass = "other-proposal" },
		"policy":                 func(value *SpecialistWorkOrderRequest) { value.PolicySHA256 = strings.Repeat("9", 64) },
		"knowledge":              func(value *SpecialistWorkOrderRequest) { value.KnowledgeSHA256 = strings.Repeat("a", 64) },
		"tool policy": func(value *SpecialistWorkOrderRequest) {
			value.ToolPolicySHA256 = strings.Repeat("b", 64)
			value.RoleConfigSnapshot.FullConfigSHA256 = value.ToolPolicySHA256
			value.RoleConfigSnapshotSHA256 = specialistRoleConfigSnapshotDigest(*value.RoleConfigSnapshot)
		},
		"capability": func(value *SpecialistWorkOrderRequest) { value.CapabilitySHA256 = strings.Repeat("c", 64) },
		"executor profile": func(value *SpecialistWorkOrderRequest) {
			value.ExecutorProfile.Model = "proof-model-v2"
			value.ExecutorProfileSHA256 = specialistExecutorProfileDigest(*value.ExecutorProfile)
		},
		"role config snapshot": func(value *SpecialistWorkOrderRequest) {
			value.RoleConfigSnapshot.Model = "configured-role-model-v2"
			value.RoleConfigSnapshotSHA256 = specialistRoleConfigSnapshotDigest(*value.RoleConfigSnapshot)
		},
		"reproduction mode": func(value *SpecialistWorkOrderRequest) {
			value.ReproductionMode = SpecialistReproductionModeNone
			value.Reproduction = nil
		},
		"reproduction": func(value *SpecialistWorkOrderRequest) {
			value.Reproduction = append(value.Reproduction, "go test ./... -run Reproduce -count=1")
		},
		"affected contracts": func(value *SpecialistWorkOrderRequest) {
			value.AffectedContracts = append(value.AffectedContracts, "widget-contract-v2")
		},
		"knowledge keywords": func(value *SpecialistWorkOrderRequest) {
			value.KnowledgeKeywords = append(value.KnowledgeKeywords, "regression")
		},
	}
	for name, mutate := range mutations {
		t.Run("binds_"+name, func(t *testing.T) {
			changed := cloneSpecialistRequest(request)
			mutate(&changed)
			order, err := mailbox.PrepareGoverned(changed)
			if err != nil {
				t.Fatalf("PrepareGoverned(changed) error = %v", err)
			}
			if order.ID == first.ID {
				t.Fatalf("mutation %q did not change the canonical swo identity", name)
			}
		})
	}

	required := map[string]func(*SpecialistWorkOrderRequest){
		"kind":                   func(value *SpecialistWorkOrderRequest) { value.Kind = "" },
		"external ref":           func(value *SpecialistWorkOrderRequest) { value.ExternalRef = "" },
		"packet":                 func(value *SpecialistWorkOrderRequest) { value.PacketSHA256 = "" },
		"finding":                func(value *SpecialistWorkOrderRequest) { value.FindingSHA256 = "" },
		"source context":         func(value *SpecialistWorkOrderRequest) { value.SourceContextSHA256 = "" },
		"source context binding": func(value *SpecialistWorkOrderRequest) { value.SourceContextBindingSHA256 = "" },
		"authority":              func(value *SpecialistWorkOrderRequest) { value.AuthorityClass = "" },
		"policy":                 func(value *SpecialistWorkOrderRequest) { value.PolicySHA256 = "" },
		"knowledge":              func(value *SpecialistWorkOrderRequest) { value.KnowledgeSHA256 = "" },
		"tool policy":            func(value *SpecialistWorkOrderRequest) { value.ToolPolicySHA256 = "" },
		"capability":             func(value *SpecialistWorkOrderRequest) { value.CapabilitySHA256 = "" },
		"executor profile":       func(value *SpecialistWorkOrderRequest) { value.ExecutorProfile = nil },
		"executor profile digest": func(value *SpecialistWorkOrderRequest) {
			value.ExecutorProfileSHA256 = ""
		},
		"role config snapshot": func(value *SpecialistWorkOrderRequest) { value.RoleConfigSnapshot = nil },
		"role config snapshot digest": func(value *SpecialistWorkOrderRequest) {
			value.RoleConfigSnapshotSHA256 = ""
		},
		"reproduction mode":  func(value *SpecialistWorkOrderRequest) { value.ReproductionMode = "" },
		"reproduction":       func(value *SpecialistWorkOrderRequest) { value.Reproduction = nil },
		"affected contracts": func(value *SpecialistWorkOrderRequest) { value.AffectedContracts = nil },
		"knowledge keywords": func(value *SpecialistWorkOrderRequest) { value.KnowledgeKeywords = nil },
	}
	for name, mutate := range required {
		t.Run("requires_"+name, func(t *testing.T) {
			changed := cloneSpecialistRequest(request)
			mutate(&changed)
			if _, err := mailbox.PrepareGoverned(changed); err == nil {
				t.Fatalf("PrepareGoverned accepted a missing %s binding", name)
			}
		})
	}
	ordinary := testSpecialistRequest(now, "ordinary-cannot-claim-governed-kind", now.Add(time.Hour))
	ordinary.Kind = SpecialistWorkOrderKindGovernedVisualHiveProposal
	if _, err := mailbox.Prepare(ordinary); err == nil {
		t.Fatal("ordinary Prepare accepted a governed kind without its governed security bindings")
	}
	withoutReproduction := cloneSpecialistRequest(request)
	withoutReproduction.ReproductionMode = SpecialistReproductionModeNone
	withoutReproduction.Reproduction = nil
	if _, err := mailbox.PrepareGoverned(withoutReproduction); err != nil {
		t.Fatalf("PrepareGoverned rejected explicit no-reproduction mode: %v", err)
	}
	nonCodex := cloneSpecialistRequest(request)
	nonCodex.ExecutorProfile.Backend = "copilot"
	nonCodex.ExecutorProfileSHA256 = specialistExecutorProfileDigest(*nonCodex.ExecutorProfile)
	if _, err := mailbox.PrepareGoverned(nonCodex); err == nil || !strings.Contains(err.Error(), "must be codex") {
		t.Fatalf("PrepareGoverned did not fail closed on a non-Codex executor profile: %v", err)
	}
}

func TestSpecialistMailboxReadsLegacyV1WithoutDigestMigration(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "specialist-mailbox")
	mailbox, err := NewSpecialistMailbox(root, SpecialistMailboxOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := testSpecialistRequest(now, "legacy-revision:7", now.Add(time.Hour))
	request.Evidence.ManifestSHA256 = ""
	request.Evidence.ArtifactIndexSHA256 = ""
	request.Evidence.RepositoryID = ""
	request.Evidence.WorkflowName, request.Evidence.WorkflowRunName, request.Evidence.WorkflowPath = "", "", ""
	request.Evidence.WorkflowEvent, request.Evidence.HeadBranch, request.Evidence.SourceArtifactID = "", "", 0
	order, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	encoded, err := os.ReadFile(paths.Order)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"\"variant\"", "\"manifest_sha256\"", "\"verification_receipt\""} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("legacy v1 bytes gained %s: %s", forbidden, encoded)
		}
	}
	reopened, err := NewSpecialistMailbox(root, SpecialistMailboxOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.ListPending()
	if err != nil || len(pending) != 1 || pending[0].ID != order.ID || pending[0].RequestSHA256 != order.RequestSHA256 {
		t.Fatalf("legacy v1 digest changed on read: pending=%+v err=%v", pending, err)
	}
}

func TestSpecialistMailboxRecoversPreUpgradeOrderWithoutRecomposition(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	current := testSpecialistRequest(now, "legacy-running:r2", now.Add(time.Hour))
	legacy := cloneSpecialistRequest(current)
	legacy.Evidence.Variant = ""
	legacy.Evidence.ManifestSHA256 = ""
	legacy.Evidence.ArtifactIndexSHA256 = ""
	legacy.Evidence.RepositoryID = ""
	legacy.Evidence.WorkflowName, legacy.Evidence.WorkflowRunName, legacy.Evidence.WorkflowPath = "", "", ""
	legacy.Evidence.WorkflowEvent, legacy.Evidence.HeadBranch, legacy.Evidence.SourceArtifactID = "", "", 0
	legacy.Evidence.VerificationReceipt = nil
	order, err := mailbox.Prepare(legacy)
	if err != nil {
		t.Fatal(err)
	}
	current.Evidence.Variant = visualhive.SpecialistEvidenceVariantVisualHiveV3
	receipt := visualhive.SpecialistEvidenceReceipt{
		RepositoryID: current.Evidence.RepositoryID, WorkflowRunID: "12345", WorkflowRunAttempt: "2",
		ArtifactID: "67890", SourceArtifactID: "67891", ArtifactName: current.Evidence.ArtifactName,
		CommitSHA: current.Evidence.WorkflowRunHeadSHA, HeadBranch: current.Evidence.HeadBranch, Event: current.Evidence.WorkflowEvent,
		WorkflowName: current.Evidence.WorkflowName, WorkflowRunName: current.Evidence.WorkflowRunName, WorkflowPath: current.Evidence.WorkflowPath,
		BundleSchema: current.Evidence.BundleSchemaVersion, BundleSHA256: current.Evidence.BundleSHA256,
		ManifestSHA256: current.Evidence.ManifestSHA256, ArtifactIndexSHA: current.Evidence.ArtifactIndexSHA256,
	}
	current.Evidence.VerificationReceipt = &receipt
	current.Evidence.VerificationReceiptSHA256, _ = receipt.SHA256()
	if err := mailbox.ValidateRequestForOrder(order, current); err == nil {
		t.Fatal("strict request validation unexpectedly recomposed the pre-upgrade order")
	}
	if err := mailbox.ValidateRecoveryRequestForOrder(order, current); err != nil {
		t.Fatalf("pre-upgrade running order was not accepted as recovery authority: %v", err)
	}
	authoritative := cloneSpecialistRequest(current)
	authoritative.RouteReason = "new controller wording that must not rewrite launched bytes"
	authoritative.Deadline = authoritative.Deadline.Add(time.Hour)
	if err := mailbox.ValidateRecoveryRequestForOrder(order, authoritative); err != nil {
		t.Fatalf("persisted route/deadline authority was recomposed: %v", err)
	}
	mutations := map[string]func(*SpecialistWorkOrderRequest){
		"repository":  func(value *SpecialistWorkOrderRequest) { value.Repository = "acme/other" },
		"fingerprint": func(value *SpecialistWorkOrderRequest) { value.RepositoryFingerprint = strings.Repeat("9", 64) },
		"recurrence":  func(value *SpecialistWorkOrderRequest) { value.RecurrenceKey = "other:r2" },
		"attempt":     func(value *SpecialistWorkOrderRequest) { value.Attempt++ },
		"base":        func(value *SpecialistWorkOrderRequest) { value.BaseSHA = strings.Repeat("8", 40) },
		"tree":        func(value *SpecialistWorkOrderRequest) { value.BaseTreeSHA = strings.Repeat("7", 40) },
		"bundle":      func(value *SpecialistWorkOrderRequest) { value.Evidence.BundleSHA256 = strings.Repeat("6", 64) },
		"run":         func(value *SpecialistWorkOrderRequest) { value.Evidence.WorkflowRunID++ },
		"head":        func(value *SpecialistWorkOrderRequest) { value.Evidence.WorkflowRunHeadSHA = strings.Repeat("5", 40) },
		"artifact":    func(value *SpecialistWorkOrderRequest) { value.Evidence.ArtifactID++ },
		"artifact name": func(value *SpecialistWorkOrderRequest) {
			value.Evidence.ArtifactName = "other-artifact"
		},
		"artifact digest": func(value *SpecialistWorkOrderRequest) { value.Evidence.ArtifactSHA256 = strings.Repeat("4", 64) },
		"role":            func(value *SpecialistWorkOrderRequest) { value.Specialist = SpecialistScanner },
		"paths":           func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = []string{"README.md"} },
		"validation":      func(value *SpecialistWorkOrderRequest) { value.Validation = []string{"false"} },
		"prompt": func(value *SpecialistWorkOrderRequest) {
			value.TaskPrompt = "different work"
			value.TaskPromptSHA256 = sha256HexString(value.TaskPrompt)
		},
	}
	for name, mutate := range mutations {
		t.Run("reject "+name, func(t *testing.T) {
			changed := cloneSpecialistRequest(current)
			mutate(&changed)
			if err := mailbox.ValidateRecoveryRequestForOrder(order, changed); err == nil {
				t.Fatal("recovery accepted safety-critical work drift")
			}
		})
	}
}

func TestSpecialistMailboxRecoversPreUpgradeRevisionOrderAfterVariantDiscriminant(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 45, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	legacy := testSpecialistRequest(now, "legacy-pr-revision:r3", now.Add(time.Hour))
	legacy.Evidence.BundleSchemaVersion = "visual-hive.pr-review.v1"
	legacy.Evidence.ManifestSHA256, legacy.Evidence.ArtifactIndexSHA256, legacy.Evidence.RepositoryID = "", "", ""
	legacy.Evidence.WorkflowName, legacy.Evidence.WorkflowRunName, legacy.Evidence.WorkflowPath = "", "", ""
	legacy.Evidence.WorkflowEvent, legacy.Evidence.HeadBranch, legacy.Evidence.SourceArtifactID = "", "", 0
	order, err := mailbox.Prepare(legacy)
	if err != nil {
		t.Fatal(err)
	}
	current := cloneSpecialistRequest(legacy)
	current.Evidence.Variant = visualhive.SpecialistEvidenceVariantRevision
	if err := mailbox.ValidateRequestForOrder(order, current); err == nil {
		t.Fatal("strict validation unexpectedly rewrote a pre-upgrade revision order")
	}
	if err := mailbox.ValidateRecoveryRequestForOrder(order, current); err != nil {
		t.Fatalf("revision variant addition interrupted the running persisted order: %v", err)
	}
}

func TestVisualHiveEvidenceCrossBindsCanonicalReceipt(t *testing.T) {
	evidence := testSpecialistRequest(time.Now(), "visual:1", time.Now().Add(time.Hour)).Evidence
	evidence.Variant = visualhive.SpecialistEvidenceVariantVisualHiveV3
	receipt := visualhive.SpecialistEvidenceReceipt{
		RepositoryID: evidence.RepositoryID, WorkflowRunID: "12345", WorkflowRunAttempt: "2", ArtifactID: "67890", SourceArtifactID: "67891",
		ArtifactName: evidence.ArtifactName, CommitSHA: evidence.WorkflowRunHeadSHA, HeadBranch: evidence.HeadBranch,
		Event: evidence.WorkflowEvent, WorkflowName: evidence.WorkflowName, WorkflowRunName: evidence.WorkflowRunName, WorkflowPath: evidence.WorkflowPath,
		BundleSchema: evidence.BundleSchemaVersion, BundleSHA256: evidence.BundleSHA256, ManifestSHA256: evidence.ManifestSHA256, ArtifactIndexSHA: evidence.ArtifactIndexSHA256,
	}
	evidence.VerificationReceipt = &receipt
	evidence.VerificationReceiptSHA256, _ = receipt.SHA256()
	if err := validateEvidence(evidence); err != nil {
		t.Fatalf("valid Visual Hive receipt rejected: %v", err)
	}
	mutations := map[string]func(*SpecialistEvidenceIdentity){
		"schema":     func(value *SpecialistEvidenceIdentity) { value.BundleSchemaVersion = "other" },
		"repository": func(value *SpecialistEvidenceIdentity) { value.RepositoryID = "999" },
		"run":        func(value *SpecialistEvidenceIdentity) { value.WorkflowRunAttempt++ },
		"artifact":   func(value *SpecialistEvidenceIdentity) { value.SourceArtifactID++ },
		"workflow":   func(value *SpecialistEvidenceIdentity) { value.WorkflowPath = ".github/workflows/other.yml" },
		"head":       func(value *SpecialistEvidenceIdentity) { value.HeadBranch = "other" },
		"content":    func(value *SpecialistEvidenceIdentity) { value.BundleSHA256 = strings.Repeat("9", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := evidence
			mutate(&changed)
			if err := validateEvidence(changed); err == nil {
				t.Fatal("outer identity drift was accepted")
			}
		})
	}
}

func TestSpecialistMailboxRejectsExistingIDWithAlteredBody(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "finding:1", now.Add(time.Hour))
	order, err := mailbox.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	encoded, err := os.ReadFile(paths.Order)
	if err != nil {
		t.Fatal(err)
	}
	var altered map[string]any
	if err := json.Unmarshal(encoded, &altered); err != nil {
		t.Fatal(err)
	}
	altered["route_reason"] = "tampered route"
	encoded, _ = json.Marshal(altered)
	if err := os.WriteFile(paths.Order, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.Prepare(request); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Prepare(tampered ID) error = %v", err)
	}
}

func TestSpecialistMailboxValidatesRequestsAndPaths(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	base := testSpecialistRequest(now, "finding:2", now.Add(time.Hour))
	tests := map[string]func(*SpecialistWorkOrderRequest){
		"unknown role":      func(value *SpecialistWorkOrderRequest) { value.Specialist = "generic" },
		"traversal":         func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = []string{"../secret"} },
		"absolute path":     func(value *SpecialistWorkOrderRequest) { value.AllowedPaths = []string{"C:/secret"} },
		"prompt mismatch":   func(value *SpecialistWorkOrderRequest) { value.TaskPromptSHA256 = strings.Repeat("0", 64) },
		"unverified bundle": func(value *SpecialistWorkOrderRequest) { value.Evidence.VerificationReceiptSHA256 = "" },
		"past deadline":     func(value *SpecialistWorkOrderRequest) { value.Deadline = now.Add(-time.Second) },
		"missing command":   func(value *SpecialistWorkOrderRequest) { value.Validation = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := cloneSpecialistRequest(base)
			mutate(&request)
			if _, err := mailbox.Prepare(request); err == nil {
				t.Fatal("Prepare() accepted invalid request")
			}
		})
	}
	if _, err := mailbox.Paths("../../escape"); err == nil {
		t.Fatal("Paths() accepted traversal")
	}
}

func TestSpecialistMailboxSupportsRepairWorkerContextEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 45, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	request := testSpecialistRequest(now, "finding:large-context", now.Add(time.Hour))
	request.TaskPrompt = strings.Repeat("x", 600<<10)
	request.TaskPromptSHA256 = sha256HexString(request.TaskPrompt)
	if _, err := mailbox.Prepare(request); err != nil {
		t.Fatalf("Prepare(600 KiB repair context) error = %v", err)
	}
	request.RecurrenceKey = "finding:oversize-context"
	request.TaskPrompt = strings.Repeat("x", maxSpecialistTaskPromptBytes+1)
	request.TaskPromptSHA256 = sha256HexString(request.TaskPrompt)
	if _, err := mailbox.Prepare(request); err == nil || !strings.Contains(err.Error(), "task prompt") {
		t.Fatalf("Prepare(oversize context) error = %v", err)
	}
}

func TestSpecialistMailboxLeaseIsExclusiveIdempotentAndRecoverable(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:lease", now.Add(4*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	first, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireLease() error = %v", err)
	}
	again, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(30*time.Minute))
	if err != nil || !equalJSON(first, again) {
		t.Fatalf("AcquireLease(idempotent) = %+v, %v", again, err)
	}
	if _, err := mailbox.AcquireLease(order, "other", "quality-2", now.Add(2*time.Hour)); !errors.Is(err, ErrSpecialistLeaseHeld) {
		t.Fatalf("AcquireLease(contender) error = %v", err)
	}
	paths, _ := mailbox.Paths(order.ID)
	assertPrivateMode(t, paths.Lease, 0o600)

	now = first.Deadline.Add(time.Second)
	recovered, err := mailbox.AcquireLease(order, "hive-controller", "quality-2", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("AcquireLease(recovery) error = %v", err)
	}
	if recovered.SessionID != "quality-2" || recovered.LeaseSHA256 == first.LeaseSHA256 {
		t.Fatalf("expired lease was not safely replaced: %+v", recovered)
	}
	matches, err := filepath.Glob(paths.Lease + ".expired-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("expired lease quarantine was not cleaned: %v, %v", matches, err)
	}
}

func TestSpecialistMailboxAbandonsOnlyExactUnlaunchedLease(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:unlaunched", now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tampered := lease
	tampered.SessionID = "quality-other"
	if err := mailbox.ReleaseUndeliveredLease(order, tampered); err == nil || !strings.Contains(err.Error(), "mismatched") {
		t.Fatalf("mismatched lease abandonment = %v", err)
	}
	if err := mailbox.ReleaseUndeliveredLease(order, lease); err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	if _, err := os.Lstat(paths.Lease); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned lease remains: %v", err)
	}
	replacement, err := mailbox.AcquireLease(order, "hive-controller", "quality-2", now.Add(time.Hour))
	if err != nil || replacement.SessionID != "quality-2" {
		t.Fatalf("immediate retry did not acquire a new lease: %+v, %v", replacement, err)
	}
	matches, err := filepath.Glob(paths.Lease + ".abandoned-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("abandoned lease quarantine remains: %v, %v", matches, err)
	}
}

func TestSpecialistMailboxRefusesLeaseAbandonmentAfterAnyOutput(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 45, 0, 0, time.UTC)
	for _, output := range []string{"summary.txt", "completion.status", "proposal.diff", ".tmp-partial"} {
		t.Run(output, func(t *testing.T) {
			mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
			request := testSpecialistRequest(now, "finding:output:"+strings.ReplaceAll(output, ".", "-"), now.Add(2*time.Hour))
			order, err := mailbox.Prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			paths, _ := mailbox.Paths(order.ID)
			if err := os.WriteFile(filepath.Join(paths.OrderDirectory, output), []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := mailbox.ReleaseUndeliveredLease(order, lease); err == nil || !strings.Contains(err.Error(), "task output") {
				t.Fatalf("lease with %s output was abandoned: %v", output, err)
			}
			loaded, err := mailbox.LoadLease(order)
			if err != nil || !equalJSON(loaded, lease) {
				t.Fatalf("refused abandonment changed the lease: %+v, %v", loaded, err)
			}
		})
	}
}

func TestSpecialistMailboxReceiptRoundTripAndPendingSort(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	late, err := mailbox.Prepare(testSpecialistRequest(now, "finding:late", now.Add(3*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	early, err := mailbox.Prepare(testSpecialistRequest(now, "finding:early", now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := mailbox.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != early.ID || pending[1].ID != late.ID {
		t.Fatalf("ListPending() was not deadline sorted: %+v", pending)
	}
	lease, err := mailbox.AcquireLease(early, "hive-controller", "quality-session", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff)
	receipt, err := NewSpecialistReceipt(early, lease, SpecialistCompletionProposed, diff, "Adds the missing deterministic test.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(early, lease, receipt, diff); err != nil {
		t.Fatalf("SubmitReceipt() error = %v", err)
	}
	if err := mailbox.SubmitReceipt(early, lease, receipt, diff); err != nil {
		t.Fatalf("SubmitReceipt(idempotent) error = %v", err)
	}
	loaded, loadedDiff, err := mailbox.LoadReceipt(early, lease)
	if err != nil {
		t.Fatalf("LoadReceipt() error = %v", err)
	}
	if !equalJSON(loaded, receipt) || string(loadedDiff) != string(diff) {
		t.Fatalf("loaded receipt differs: %+v", loaded)
	}
	loadedOrder, err := mailbox.LoadWorkOrder(early.ID)
	if err != nil || !equalJSON(loadedOrder, early) {
		t.Fatalf("LoadWorkOrder() = %+v, %v", loadedOrder, err)
	}
	loadedLease, loadedCompletion, replayDiff, err := mailbox.LoadCompletion(loadedOrder)
	if err != nil || !equalJSON(loadedLease, lease) || !equalJSON(loadedCompletion, receipt) || string(replayDiff) != string(diff) {
		t.Fatalf("LoadCompletion() = %+v, %+v, %q, %v", loadedLease, loadedCompletion, replayDiff, err)
	}
	if _, err := mailbox.AcquireLease(early, "hive-controller", "quality-rekick", now.Add(90*time.Minute)); !errors.Is(err, ErrSpecialistOrderComplete) {
		t.Fatalf("AcquireLease(completed) error = %v", err)
	}
	paths, _ := mailbox.Paths(early.ID)
	assertPrivateMode(t, paths.UnifiedDiff, 0o600)
	assertPrivateMode(t, paths.Receipt, 0o600)
	pending, err = mailbox.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != late.ID {
		t.Fatalf("completed order remains pending: %+v", pending)
	}
}

func TestSpecialistMailboxPersistsLateObservedInLeaseCompletion(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:late-observation", now.Add(10*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "controller", "session-late", now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, []byte(testSpecialistDiff), "Completed before the controller restarted.", now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	now = lease.Deadline.Add(time.Minute)
	if err := mailbox.SubmitReceipt(order, lease, receipt, []byte(testSpecialistDiff)); err != nil {
		t.Fatalf("late observation rejected an in-lease completion: %v", err)
	}
	if _, _, err := mailbox.LoadReceipt(order, lease); err != nil {
		t.Fatalf("late observed receipt did not round trip: %v", err)
	}
	if _, err := NewSpecialistReceipt(order, lease, SpecialistCompletionBlocked, nil, "Too late.", lease.Deadline.Add(time.Nanosecond)); err == nil {
		t.Fatal("out-of-lease completion time was accepted")
	}
}

func TestSpecialistMailboxRejectsMismatchedStaleSymlinkAndOversizeReceipts(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:receipt", now.Add(3*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-1", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff)
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, diff, "A bounded patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*SpecialistReceipt){
		"request": func(value *SpecialistReceipt) { value.RequestSHA256 = strings.Repeat("1", 64) },
		"role":    func(value *SpecialistReceipt) { value.Specialist = SpecialistScanner },
		"session": func(value *SpecialistReceipt) { value.SessionID = "quality-2" },
		"base":    func(value *SpecialistReceipt) { value.BaseSHA = strings.Repeat("f", 40) },
		"tree":    func(value *SpecialistReceipt) { value.BaseTreeSHA = strings.Repeat("e", 40) },
		"task":    func(value *SpecialistReceipt) { value.TaskPromptSHA256 = strings.Repeat("2", 64) },
		"diff":    func(value *SpecialistReceipt) { value.UnifiedDiffSHA256 = strings.Repeat("3", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed)
			changed.ReceiptSHA256, _ = digestReceipt(changed)
			if err := mailbox.SubmitReceipt(order, lease, changed, diff); err == nil {
				t.Fatal("SubmitReceipt() accepted mismatched receipt")
			}
		})
	}

	now = lease.Deadline.Add(time.Second)
	replacement, err := mailbox.AcquireLease(order, "hive-controller", "quality-2", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, diff); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("SubmitReceipt(stale) error = %v", err)
	}
	newReceipt, err := NewSpecialistReceipt(order, replacement, SpecialistCompletionProposed, diff, "Replacement patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(order, replacement, newReceipt, diff); err != nil {
		t.Fatal(err)
	}
	paths, _ := mailbox.Paths(order.ID)
	if err := os.Remove(paths.UnifiedDiff); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.diff")
	if err := os.WriteFile(external, diff, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, paths.UnifiedDiff); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, _, err := mailbox.LoadReceipt(order, replacement); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("LoadReceipt(symlink) error = %v", err)
	}
}

func TestSpecialistMailboxRejectsOversizeFilesAndMalformedJSON(t *testing.T) {
	now := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{MaxReceiptBytes: 128, MaxUnifiedDiffBytes: 96})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:bounds", now.Add(2*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff + strings.Repeat("+padding\n", 20))
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, diff, "Bounded patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, diff); err == nil || !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("SubmitReceipt(oversize diff) error = %v", err)
	}

	paths, _ := mailbox.Paths(order.ID)
	if err := os.WriteFile(paths.Receipt, []byte(`{"schema_version":"bad"} trailing`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mailbox.LoadReceipt(order, lease); err == nil {
		t.Fatal("LoadReceipt() accepted malformed JSON")
	}
	if err := os.WriteFile(paths.Receipt, []byte(strings.Repeat("x", 129)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mailbox.LoadReceipt(order, lease); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("LoadReceipt(oversize JSON) error = %v", err)
	}
}

func TestSpecialistMailboxBlockedReceiptIsDiffFreeAndIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 16, 6, 30, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:blocked", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality-blocked", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, nil, "No patch.", now.Add(time.Minute)); err == nil {
		t.Fatal("NewSpecialistReceipt(proposed without diff) succeeded")
	}
	if _, err := NewSpecialistReceipt(order, lease, SpecialistCompletionBlocked, []byte(testSpecialistDiff), "Provider could not safely edit the target.", now.Add(time.Minute)); err == nil {
		t.Fatal("NewSpecialistReceipt(blocked with diff) succeeded")
	}
	if _, err := NewSpecialistReceipt(order, lease, "unknown", nil, "Unknown status.", now.Add(time.Minute)); err == nil {
		t.Fatal("NewSpecialistReceipt(unknown status) succeeded")
	}
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionBlocked, nil, "Provider could not identify a safe repository-local change.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	tampered.UnifiedDiffSHA256 = strings.Repeat("a", 64)
	tampered.ReceiptSHA256, _ = digestReceipt(tampered)
	if err := mailbox.SubmitReceipt(order, lease, tampered, nil); err == nil || !strings.Contains(err.Error(), "must not include") {
		t.Fatalf("SubmitReceipt(blocked with digest) error = %v", err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, nil); err != nil {
		t.Fatalf("SubmitReceipt(blocked) error = %v", err)
	}
	if err := mailbox.SubmitReceipt(order, lease, receipt, nil); err != nil {
		t.Fatalf("SubmitReceipt(blocked idempotent) error = %v", err)
	}
	loadedLease, loaded, diff, err := mailbox.LoadCompletion(order)
	if err != nil || !equalJSON(loadedLease, lease) || !equalJSON(loaded, receipt) || len(diff) != 0 {
		t.Fatalf("LoadCompletion(blocked) = %+v, %+v, %q, %v", loadedLease, loaded, diff, err)
	}
	paths, _ := mailbox.Paths(order.ID)
	if _, err := os.Lstat(paths.UnifiedDiff); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked receipt wrote a diff file: %v", err)
	}
	if _, err := mailbox.AcquireLease(order, "hive-controller", "quality-rekick", now.Add(45*time.Minute)); !errors.Is(err, ErrSpecialistOrderComplete) {
		t.Fatalf("AcquireLease(completed blocked order) error = %v", err)
	}
}

func TestSpecialistMailboxWaitReceiptHonorsPollingAndContext(t *testing.T) {
	now := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC)
	mailbox := newTestSpecialistMailbox(t, &now, SpecialistMailboxOptions{})
	order, err := mailbox.Prepare(testSpecialistRequest(now, "finding:wait", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := mailbox.AcquireLease(order, "hive-controller", "quality", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	diff := []byte(testSpecialistDiff)
	receipt, err := NewSpecialistReceipt(order, lease, SpecialistCompletionProposed, diff, "Asynchronous patch.", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- mailbox.SubmitReceipt(order, lease, receipt, diff)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	loaded, _, err := mailbox.WaitReceipt(ctx, order, lease, 5*time.Millisecond)
	if err != nil || loaded.ReceiptSHA256 != receipt.ReceiptSHA256 {
		t.Fatalf("WaitReceipt() = %+v, %v", loaded, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	second, err := mailbox.Prepare(testSpecialistRequest(now, "finding:cancel", now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := mailbox.AcquireLease(second, "hive-controller", "quality-2", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, _, err := mailbox.WaitReceipt(canceled, second, secondLease, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitReceipt(canceled) error = %v", err)
	}
	if _, _, err := mailbox.WaitReceipt(context.Background(), second, secondLease, 0); err == nil {
		t.Fatal("WaitReceipt() accepted zero polling interval")
	}
}

func newTestSpecialistMailbox(t *testing.T, now *time.Time, options SpecialistMailboxOptions) *SpecialistMailbox {
	t.Helper()
	options.Now = func() time.Time { return *now }
	mailbox, err := NewSpecialistMailbox(filepath.Join(t.TempDir(), "specialist-mailbox"), options)
	if err != nil {
		t.Fatalf("NewSpecialistMailbox() error = %v", err)
	}
	return mailbox
}

func testSpecialistRequest(now time.Time, recurrence string, deadline time.Time) SpecialistWorkOrderRequest {
	prompt := "Use verified Visual Hive evidence to add the smallest deterministic repository test. Do not write to GitHub."
	return SpecialistWorkOrderRequest{
		Repository:            "Acme/Widget",
		RepositoryFingerprint: strings.Repeat("a", 64),
		RecurrenceKey:         recurrence,
		Attempt:               1,
		BaseSHA:               strings.Repeat("b", 40),
		BaseTreeSHA:           strings.Repeat("c", 40),
		Evidence: SpecialistEvidenceIdentity{
			BundleSchemaVersion:       "visual-hive.hive-bundle.v3",
			BundleSHA256:              strings.Repeat("d", 64),
			ManifestSHA256:            strings.Repeat("f", 64),
			ArtifactIndexSHA256:       strings.Repeat("1", 64),
			VerificationReceiptSHA256: strings.Repeat("e", 64),
			RepositoryID:              "123",
			WorkflowRunID:             12345,
			WorkflowRunAttempt:        2,
			WorkflowRunHeadSHA:        strings.Repeat("b", 40),
			WorkflowName:              "Hive Visual Hive Production",
			WorkflowRunName:           "Hive Visual Hive Production [test]",
			WorkflowPath:              ".github/workflows/hive-visual-hive.yml",
			WorkflowEvent:             "workflow_dispatch",
			HeadBranch:                "main",
			ArtifactID:                67890,
			SourceArtifactID:          67891,
			ArtifactName:              "visual-hive-evidence",
			ArtifactSHA256:            strings.Repeat("f", 64),
		},
		Specialist:       SpecialistQuality,
		RouteReason:      "verified test adequacy gap routes to quality",
		AllowedPaths:     []string{"tests/widget_test.go", `src\widget.ts`},
		Validation:       []string{"go test ./... -count=1"},
		TaskPrompt:       prompt,
		TaskPromptSHA256: sha256HexString(prompt),
		Deadline:         deadline,
	}
}

func testGovernedSpecialistRequest(now time.Time, externalRef string, deadline time.Time) SpecialistWorkOrderRequest {
	request := testSpecialistRequest(now, externalRef+":recurrence", deadline)
	request.Kind = SpecialistWorkOrderKindGovernedVisualHiveProposal
	request.ExternalRef = externalRef
	request.PacketSHA256 = strings.Repeat("1", 64)
	request.FindingSHA256 = strings.Repeat("2", 64)
	request.SourceContextSHA256 = strings.Repeat("7", 64)
	request.SourceContextBindingSHA256 = strings.Repeat("0", 64)
	request.AuthorityClass = "sealed-source-context-proposal-v1"
	request.PolicySHA256 = strings.Repeat("3", 64)
	request.KnowledgeSHA256 = strings.Repeat("4", 64)
	request.ToolPolicySHA256 = strings.Repeat("5", 64)
	request.CapabilitySHA256 = strings.Repeat("6", 64)
	request.ExecutorProfile = &SpecialistExecutorProfile{
		Backend: SpecialistExecutorBackendCodex, ProviderSHA256: strings.Repeat("7", 64),
		Model: "proof-model-v1", ConfigurationSHA256: strings.Repeat("8", 64),
		ContainmentProfile: SpecialistExecutorContainmentProfileV1, BackendParityClaimed: false,
	}
	request.ExecutorProfileSHA256 = specialistExecutorProfileDigest(*request.ExecutorProfile)
	request.RoleConfigSnapshot = &SpecialistRoleConfigSnapshot{
		SchemaVersion: SpecialistRoleConfigSnapshotSchema, Backend: "copilot", Model: "normal-role-model", Mode: "ADVISORY",
		LaunchCmdPresent: true, LaunchCmdSHA256: strings.Repeat("9", 64), IncludeRepositories: true,
		ToolRulesSHA256: strings.Repeat("a", 64), ConnectionsSHA256: strings.Repeat("b", 64), ConnectionCount: 1,
		FullConfigSHA256: request.ToolPolicySHA256,
	}
	request.RoleConfigSnapshotSHA256 = specialistRoleConfigSnapshotDigest(*request.RoleConfigSnapshot)
	request.ReproductionMode = SpecialistReproductionModeVerifiedValidation
	request.Reproduction = []string{"go test ./... -run Reproduce"}
	request.AffectedContracts = []string{"widget-contract-v1"}
	request.KnowledgeKeywords = []string{"widget", "visual"}
	return request
}

func cloneSpecialistRequest(value SpecialistWorkOrderRequest) SpecialistWorkOrderRequest {
	copy := value
	copy.AllowedPaths = append([]string(nil), value.AllowedPaths...)
	copy.Reproduction = append([]string(nil), value.Reproduction...)
	copy.Validation = append([]string(nil), value.Validation...)
	copy.AffectedContracts = append([]string(nil), value.AffectedContracts...)
	copy.KnowledgeKeywords = append([]string(nil), value.KnowledgeKeywords...)
	if value.ExecutorProfile != nil {
		profile := *value.ExecutorProfile
		copy.ExecutorProfile = &profile
	}
	if value.RoleConfigSnapshot != nil {
		snapshot := *value.RoleConfigSnapshot
		copy.RoleConfigSnapshot = &snapshot
	}
	return copy
}

func specialistExecutorProfileDigest(profile SpecialistExecutorProfile) string {
	digest, err := digestJSON(profile)
	if err != nil {
		panic(err)
	}
	return digest
}

func specialistRoleConfigSnapshotDigest(snapshot SpecialistRoleConfigSnapshot) string {
	digest, err := digestJSON(snapshot)
	if err != nil {
		panic(err)
	}
	return digest
}

func sha256HexString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func assertPrivateMode(t *testing.T, target string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != want {
		t.Fatalf("mode(%s) = %o, want %o", target, info.Mode().Perm(), want)
	}
}
