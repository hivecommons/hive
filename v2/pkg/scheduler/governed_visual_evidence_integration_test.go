package scheduler_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
)

type verticalCanonicalReceipt struct {
	identity agent.SpecialistEvidenceIdentity
	bytes    json.RawMessage
}

func (receipt *verticalCanonicalReceipt) EvidenceIdentity() agent.SpecialistEvidenceIdentity {
	return receipt.identity
}

func (receipt *verticalCanonicalReceipt) CanonicalReceiptJSON() (json.RawMessage, error) {
	return append(json.RawMessage(nil), receipt.bytes...), nil
}

type verticalWorkerComposition struct {
	evidence  *repair.GovernedEvidenceArtifactReader
	source    scheduler.WorkerSealedSourceContext
	readCalls int
}

func (composition *verticalWorkerComposition) ComposeGovernedSourceContext(scheduler.WorkerSourceContextRequest) (scheduler.WorkerSealedSourceContext, error) {
	result := composition.source
	result.Files = append([]scheduler.WorkerSourceBlobIdentity(nil), composition.source.Files...)
	return result, nil
}

func (composition *verticalWorkerComposition) ReadGovernedEvidenceArtifacts(request scheduler.WorkerEvidenceArtifactRequest) (scheduler.WorkerEvidenceArtifactReadResult, error) {
	composition.readCalls++
	return composition.evidence.ReadGovernedEvidenceArtifacts(request)
}

func TestGovernedQualityPromptPreservesMutationEvidenceAcrossMailboxReplay(t *testing.T) {
	evidenceRoot := filepath.Join(t.TempDir(), ".visual-hive")
	if err := os.MkdirAll(filepath.Join(evidenceRoot, "evidence"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(evidenceRoot, "screenshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	mutationBytes := []byte(`{"issueKind":"mutation_survivor","mutationOperator":"replace-200-with-500","survivorId":"checkout-ready-survived","operatorGuidance":"preserve the 500 error path","missingTestRecommendation":"add a quality assertion for retry banner","selector":"checkout-retry-banner","flow":["submit","receive-500","retry"]}`)
	mutationPath := filepath.Join(evidenceRoot, "evidence", "mutation.json")
	if err := os.WriteFile(mutationPath, mutationBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	pngBytes := []byte("PNG_BYTES_MUST_NOT_REACH_THE_CHILD")
	if err := os.WriteFile(filepath.Join(evidenceRoot, "screenshots", "checkout.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	evidenceReader, err := repair.NewGovernedEvidenceArtifactReader(evidenceRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = evidenceReader.Close() })

	policyRoot := t.TempDir()
	agentsDir := filepath.Join(policyRoot, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "quality-vh.md"), []byte("NORMAL_QUALITY_ROLE_POLICY ${PROJECT_ORG}/${PROJECT_PRIMARY_REPO}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := scheduler.New(&config.Config{
		Project: config.ProjectConfig{Org: "acme", Name: "console", PrimaryRepo: "console", Repos: []string{"console"}},
		Agents: map[string]config.AgentConfig{
			string(agent.SpecialistQuality): {Enabled: true, Role: string(agent.SpecialistQuality), Backend: "claude", Model: "normal-quality-model", KickTemplate: "quality-vh.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: policyRoot},
	}, logger)
	if err := s.SetGovernedProposalExecutorProfile(scheduler.GovernedProposalExecutorProfile{
		Backend: agent.SpecialistExecutorBackendCodex, ProviderSHA256: strings.Repeat("8", 64),
		Model: "proof-v1-codex", ConfigurationSHA256: strings.Repeat("9", 64), ContainmentProfile: agent.SpecialistExecutorContainmentProfileV1,
	}); err != nil {
		t.Fatal(err)
	}

	packet := json.RawMessage(`{"packet_id":"vh-packet-7","repository":"acme/console","repository_id":"R_console","producer":"visual-hive","producer_commit":"7777777777777777777777777777777777777777","workflow_event":"workflow_dispatch"}`)
	jsonReceipt := scheduler.EvidenceArtifact{Reference: ".visual-hive/evidence/mutation.json", SHA256: verticalDigest(mutationBytes), Bytes: int64(len(mutationBytes)), Kind: "json", ContentType: "application/json"}
	pngReceipt := scheduler.EvidenceArtifact{Reference: ".visual-hive/screenshots/checkout.png", SHA256: verticalDigest(pngBytes), Bytes: int64(len(pngBytes)), Kind: "image", ContentType: "image/png"}
	finding := verticalJSON(t, map[string]any{
		"repository": "acme/console", "fingerprint": "mutation/checkout-ready", "repository_fingerprint": strings.Repeat("a", 64),
		"publication_role": "canonical", "root_cause_key": "mutation/checkout-ready", "status": "fix_queued",
		"issue_kind": "mutation_survivor", "severity": "high", "title": "Checkout mutation survived",
		"body": "The replace-200-with-500 operator survived; add the missing retry-banner test.", "labels": []string{"mutation", "visual-hive"},
		"affected_contracts": []string{"checkout-recovery"}, "validation_command": "npm test -- checkout-recovery",
	})
	workBytes := verticalJSON(t, map[string]any{
		"source_external_ref": "visual-hive://acme/console/mutation-checkout-ready", "packet": packet,
		"finding_fingerprint": "mutation/checkout-ready", "repository_fingerprint": strings.Repeat("a", 64), "observation_state": "present",
		"publication_role": "canonical", "root_cause_key": "mutation/checkout-ready", "blocked_by_root_keys": []string{},
		"issue_kind": "mutation_survivor", "severity": "high", "title": "Checkout mutation survived",
		"body": "The replace-200-with-500 operator survived; add the missing retry-banner test.", "labels": []string{"mutation", "visual-hive"},
		"role": "quality", "routing_reason": "verified mutation evidence routes to normal quality", "routing_allowed": true,
		"affected_contracts": []string{"checkout-recovery"}, "knowledge_keywords": []string{"mutation_survivor", "checkout-recovery"},
		"evidence_artifacts":    []map[string]any{verticalArtifactJSON(jsonReceipt), verticalArtifactJSON(pngReceipt)},
		"reproduction_commands": []string{"npm test -- checkout-recovery"}, "reproduction_source": "verified_observation_validation_command",
		"validation_commands": []string{"npm test -- checkout-recovery"},
		"authority":           map[string]any{"proposal_only": true, "github_write_allowed": false, "merge_allowed": false, "baseline_changes_allowed": false, "baseline_approval_allowed": false},
	})
	receiptBytes := verticalJSON(t, map[string]any{
		"repository_id": "R_console", "workflow_run_id": "700", "workflow_run_attempt": "2", "artifact_id": "701", "source_artifact_id": "702",
		"artifact_name": "visual-hive-evidence", "commit_sha": strings.Repeat("b", 40), "head_branch": "main", "event": "workflow_dispatch",
		"workflow_name": "Visual Hive", "workflow_run_name": "visual-hive-700", "workflow_path": ".github/workflows/visual-hive.yml",
		"bundle_schema": "visual-hive.hive-bundle.v3", "bundle_sha256": strings.Repeat("d", 64), "manifest_sha256": strings.Repeat("e", 64), "artifact_index_sha256": strings.Repeat("f", 64),
	})
	receipt := &verticalCanonicalReceipt{bytes: receiptBytes, identity: agent.SpecialistEvidenceIdentity{
		BundleSchemaVersion: "visual-hive.hive-bundle.v3", BundleSHA256: strings.Repeat("d", 64), VerificationReceiptSHA256: verticalDigest(receiptBytes),
		WorkflowRunID: 700, WorkflowRunAttempt: 2, WorkflowRunHeadSHA: strings.Repeat("b", 40), ArtifactID: 701,
		ArtifactName: "visual-hive-evidence", ArtifactSHA256: strings.Repeat("e", 64),
	}}
	work := scheduler.AdmittedWork{
		ExternalRef: "visual-hive://acme/console/mutation-checkout-ready", Work: workBytes, WorkSHA256: verticalDigest(workBytes),
		Packet: packet, PacketSHA256: verticalDigest(packet), Finding: finding, FindingSHA256: verticalDigest(finding),
		Repository: "acme/console", RepositoryFingerprint: strings.Repeat("a", 64), RecurrenceKey: "mutation-checkout-ready:r1", Attempt: 1,
		BaseSHA: strings.Repeat("b", 40), BaseTreeSHA: strings.Repeat("c", 40), RoutedRole: agent.SpecialistQuality,
		RoutingReason: "verified mutation evidence routes to normal quality", ObservationKind: "mutation_survivor",
		AllowedPaths: []string{"web/checkout.tsx", "web/checkout.test.ts"}, Validation: []string{"npm test -- checkout-recovery"}, ValidationMayReproduce: true,
		AffectedContracts: []string{"checkout-recovery"}, EvidenceArtifacts: []scheduler.EvidenceArtifact{jsonReceipt, pngReceipt}, Deadline: time.Now().UTC().Add(time.Hour),
	}
	sourceText := "HIVE_UNTRUSTED_SOURCE_CONTEXT_BEGIN\n{\"path\":\"web/checkout.tsx\",\"content\":\"export const checkout = true;\"}\nHIVE_UNTRUSTED_SOURCE_CONTEXT_END"
	composition := &verticalWorkerComposition{evidence: evidenceReader, source: scheduler.WorkerSealedSourceContext{
		Content: sourceText, SHA256: verticalDigest([]byte(sourceText)), BaseSHA: work.BaseSHA, BaseTreeSHA: work.BaseTreeSHA,
		ManifestSHA256: receipt.identity.ArtifactSHA256, VerificationReceiptSHA256: receipt.identity.VerificationReceiptSHA256,
		Files: []scheduler.WorkerSourceBlobIdentity{{Path: "web/checkout.tsx", BlobOID: strings.Repeat("1", 40), ContentSHA256: verticalDigest([]byte("export const checkout = true;")), Bytes: 29}},
	}}
	request, err := s.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, composition)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{string(mutationBytes), "replace-200-with-500", "checkout-ready-survived", "add a quality assertion for retry banner", "NORMAL_QUALITY_ROLE_POLICY", "checkout-recovery", "npm test -- checkout-recovery"} {
		if !strings.Contains(request.TaskPrompt, exact) {
			t.Fatalf("normal quality prompt lost exact typed mutation evidence %q", exact)
		}
	}
	if strings.Contains(request.TaskPrompt, string(pngBytes)) {
		t.Fatal("binary screenshot bytes were embedded in the proposal prompt")
	}

	mailboxRoot := filepath.Join(t.TempDir(), "mailbox")
	mailbox, err := agent.NewSpecialistMailbox(mailboxRoot, agent.SpecialistMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := mailbox.PrepareGoverned(request)
	if err != nil {
		t.Fatal(err)
	}
	originalPrompt := prepared.TaskPrompt
	if composition.readCalls != 1 {
		t.Fatalf("initial composition read evidence %d times", composition.readCalls)
	}
	if err := os.WriteFile(mutationPath, []byte(`{"changed_after_crash":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := agent.NewSpecialistMailbox(mailboxRoot, agent.SpecialistMailboxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.LoadWorkOrder(prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != prepared.ID || loaded.RequestSHA256 != prepared.RequestSHA256 || loaded.TaskPrompt != originalPrompt || composition.readCalls != 1 {
		t.Fatalf("crash replay recomposed or reread changed evidence: loaded=%s/%s reads=%d", loaded.ID, loaded.RequestSHA256, composition.readCalls)
	}
	pending, err := restarted.ListPending()
	if err != nil || len(pending) != 1 || pending[0].ID != prepared.ID {
		t.Fatalf("crash replay changed mailbox cardinality: pending=%+v err=%v", pending, err)
	}
	if _, err := s.BuildGovernedProposalMessage(agent.SpecialistQuality, work, receipt, composition); err == nil {
		t.Fatal("changed evidence bytes were accepted under the old receipt during an explicit fresh composition")
	}
}

func verticalArtifactJSON(artifact scheduler.EvidenceArtifact) map[string]any {
	return map[string]any{"path": artifact.Reference, "sha256": artifact.SHA256, "bytes": artifact.Bytes, "kind": artifact.Kind, "content_type": artifact.ContentType}
}

func verticalJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func verticalDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
