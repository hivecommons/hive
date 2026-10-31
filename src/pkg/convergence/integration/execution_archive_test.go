package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func executionSignedArchive(t *testing.T, proof ExecutionProof, key *rsa.PrivateKey) *ExecutionArchive {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"schema": 1, "protocol": "integration-producer-receipt-v1", "subject": proof.Subject,
		"runID": strconv.FormatInt(proof.RunID, 10), "runAttempt": strconv.Itoa(proof.Attempt),
		"outcomes": map[string]string{"ci": "success", "tests": "success", "security": "success"},
	})
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	keyHash, payloadHash := sha256.Sum256(spki), sha256.Sum256(payload)
	last := proof.Subject.Prefixes[len(proof.Subject.Prefixes)-1]
	archive := &ExecutionArchive{
		Role: "candidate", Issuer: executionArchiveIssuer, KeyID: "historical-key", SigningKeySPKI: spki,
		SigningKeySHA256: hex.EncodeToString(keyHash[:]), VerifiedAt: proof.ObservedAt,
		Payload: payload, ArtifactID: 1, WorkflowID: 2, SuiteID: 3, CheckIDs: []int64{4, 5}, FinalizerCheckID: 5,
		RepositoryID: 6, ActorID: 7, WorkflowSHA: last.Commit, WorkflowPath: ".github/workflows/integration-candidate.yml",
		Ref: "refs/tags/hive-integration/" + proof.Subject.ID, Event: "workflow_dispatch",
		JobStartedAt: proof.ObservedAt.Add(-time.Minute), JobCompletedAt: proof.ObservedAt,
	}
	claims := jwt.MapClaims{
		"iss": archive.Issuer, "aud": "hive-integration-producer-v1/" + hex.EncodeToString(payloadHash[:]),
		"repository": proof.Subject.Repo, "repository_id": "6", "actor_id": "7",
		"run_id": strconv.FormatInt(proof.RunID, 10), "run_attempt": strconv.Itoa(proof.Attempt), "check_run_id": "5",
		"workflow_ref": proof.Subject.Repo + "/" + archive.WorkflowPath + "@" + archive.Ref, "workflow_sha": archive.WorkflowSHA,
		"ref": archive.Ref, "event_name": archive.Event, "runner_environment": "github-hosted",
		"iat": proof.ObservedAt.Add(-30 * time.Second).Unix(), "nbf": proof.ObservedAt.Add(-30 * time.Second).Unix(),
		"exp": proof.ObservedAt.Add(30 * time.Minute).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = archive.KeyID
	archive.Token, err = token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestExecutionArchivedProducerHistoricalReplayAndIsolation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenExecution(executionFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	proof := executionProofFixture()
	proof.State, proof.JobCompleteness = Passed, "complete"
	proof.Archive = executionSignedArchive(t, proof, key)
	if err := validateExecutionArchive(proof); err != nil {
		t.Fatalf("valid historical archive: %v", err)
	}
	if err := store.AppendExecution("passed", proof.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Proof: &proof}); err != nil {
		t.Fatal(err)
	}
	replayed, err := OpenExecution(store.path)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := replayed.ExecutionProof(proof.Subject.ID, proof.RunID, proof.Attempt)
	if !ok || saved.State != Passed || saved.Archive == nil {
		t.Fatalf("lost archived proof: %+v", saved)
	}
	saved.Archive.SigningKeySPKI[0] ^= 1
	untouched, _ := replayed.ExecutionProof(proof.Subject.ID, proof.RunID, proof.Attempt)
	if err := validateExecutionArchive(untouched); err != nil {
		t.Fatalf("private archive alias escaped: %v", err)
	}
	public, err := json.Marshal(replayed.ExecutionSnapshot())
	if err != nil || strings.Contains(string(public), proof.Archive.Token) || strings.Contains(string(public), "signing_key_spki") {
		t.Fatal("private signature/key leaked in public ledger projection")
	}
	for _, test := range []struct {
		name   string
		change func(*ExecutionProof)
	}{
		{"role", func(p *ExecutionProof) { p.Archive.Role = "materializer" }},
		{"issuer", func(p *ExecutionProof) { p.Archive.Issuer = "https://invalid.example" }},
		{"key-id", func(p *ExecutionProof) { p.Archive.KeyID = "rotated" }},
		{"key-digest", func(p *ExecutionProof) { p.Archive.SigningKeySHA256 = strings.Repeat("0", 64) }},
		{"key-encoding", func(p *ExecutionProof) {
			p.Archive.SigningKeySPKI = []byte("invalid")
			sum := sha256.Sum256(p.Archive.SigningKeySPKI)
			p.Archive.SigningKeySHA256 = hex.EncodeToString(sum[:])
		}},
		{"payload", func(p *ExecutionProof) { p.Archive.Payload = append(p.Archive.Payload, ' ') }},
		{"signature", func(p *ExecutionProof) { p.Archive.Token = "invalid" }},
		{"run", func(p *ExecutionProof) { p.RunID++ }},
		{"attempt", func(p *ExecutionProof) { p.Attempt++ }},
		{"repo", func(p *ExecutionProof) { p.Subject.Repo = "other/repo" }},
		{"head", func(p *ExecutionProof) { p.Subject.Members[0].Head = strings.Repeat("3", 40) }},
		{"tree", func(p *ExecutionProof) { p.Subject.Prefixes[0].Tree = strings.Repeat("3", 40) }},
		{"commit", func(p *ExecutionProof) { p.Subject.Prefixes[0].Commit = strings.Repeat("3", 40) }},
		{"missing-finalizer", func(p *ExecutionProof) { p.Archive.FinalizerCheckID = 99 }},
		{"duplicate-check", func(p *ExecutionProof) { p.Archive.CheckIDs = []int64{5, 5} }},
		{"interval", func(p *ExecutionProof) {
			p.Archive.JobStartedAt = p.ObservedAt.Add(time.Hour)
			p.Archive.JobCompletedAt = p.Archive.JobStartedAt
		}},
		{"ref", func(p *ExecutionProof) { p.Archive.Ref = "refs/heads/v5" }},
		{"event", func(p *ExecutionProof) { p.Archive.Event = "push" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := cloneExecution(proof)
			test.change(&changed)
			if err := validateExecutionArchive(changed); err == nil {
				t.Fatal("accepted changed historical producer/subject binding")
			}
		})
	}
}
