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
	"github.com/hivecommons/hive/pkg/forgestate"
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
	keyHash := sha256.Sum256(spki)
	last := proof.Subject.Prefixes[len(proof.Subject.Prefixes)-1]
	archive := &ExecutionArchive{
		Role: "candidate", Issuer: executionArchiveIssuer, KeyID: "historical-key", SigningKeySPKI: spki,
		SigningKeySHA256: hex.EncodeToString(keyHash[:]), VerifiedAt: proof.ObservedAt,
		Payload: payload, ArtifactID: 1, WorkflowID: 2, SuiteID: 3, CheckIDs: []int64{4, 5}, FinalizerCheckID: 5,
		RepositoryID: 6, ActorID: 7, WorkflowSHA: last.Commit, WorkflowPath: ".github/workflows/integration-candidate.yml",
		Ref: "refs/tags/hive-integration/" + proof.Subject.ID, Event: "workflow_dispatch",
		JobStartedAt: proof.ObservedAt.Add(-time.Minute), JobCompletedAt: proof.ObservedAt,
	}
	executionSignArchive(t, archive, key, proof.Subject.Repo, proof.RunID, proof.Attempt)
	return archive
}

func executionSignArchive(t *testing.T, archive *ExecutionArchive, key *rsa.PrivateKey, repo string, runID int64, attempt int) {
	t.Helper()
	payloadHash := sha256.Sum256(archive.Payload)
	audience := "hive-integration-producer-v1/"
	if archive.Role == "materializer" {
		audience = "hive-integration-materializer-v1/"
	}
	claims := jwt.MapClaims{
		"iss": archive.Issuer, "aud": audience + hex.EncodeToString(payloadHash[:]),
		"repository": repo, "repository_id": strconv.FormatInt(archive.RepositoryID, 10), "actor_id": strconv.FormatInt(archive.ActorID, 10),
		"run_id": strconv.FormatInt(runID, 10), "run_attempt": strconv.Itoa(attempt), "check_run_id": strconv.FormatInt(archive.FinalizerCheckID, 10),
		"workflow_ref": repo + "/" + archive.WorkflowPath + "@" + archive.Ref, "workflow_sha": archive.WorkflowSHA,
		"ref": archive.Ref, "event_name": archive.Event, "runner_environment": "github-hosted",
		"iat": archive.VerifiedAt.Add(-30 * time.Second).Unix(), "nbf": archive.VerifiedAt.Add(-30 * time.Second).Unix(),
		"exp": archive.VerifiedAt.Add(30 * time.Minute).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = archive.KeyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	archive.Token = signed
}

func TestExecutionV2GovernanceArchiveRejectsMissingFalseGreenOrForeignPhysicalChecks(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"passed", "missing", "foreign-check", "foreign-suite", "skipped", "metadata", "protocol", "superseded-false-green"} {
		t.Run(scenario, func(t *testing.T) {
			proof := executionProofFixture()
			proof.State, proof.ObservedState = Passed, Passed
			proof.Archive = executionSignedArchive(t, proof, key)
			contexts := map[string]forgestate.IntegrationContextReceipt{}
			for i, name := range forgestate.IntegrationRequiredContexts() {
				record := forgestate.IntegrationContextReceipt{JobID: int64(100 + i), CheckID: int64(200 + i),
					AppID: 15368, SuiteID: proof.Archive.SuiteID, Conclusion: "success"}
				contexts[name] = record
				proof.Archive.CheckIDs = append(proof.Archive.CheckIDs, record.CheckID)
			}
			metadata, protocol := strings.Repeat("a", 64), "integration-producer-receipt-v2"
			record := contexts["DCO"]
			switch scenario {
			case "missing":
				delete(contexts, "DCO")
			case "foreign-check":
				record.CheckID = 9999
				contexts["DCO"] = record
			case "foreign-suite":
				record.SuiteID++
				contexts["DCO"] = record
			case "skipped", "superseded-false-green":
				record.Conclusion = "skipped"
				contexts["DCO"] = record
				if scenario == "superseded-false-green" {
					proof.State = Superseded
				}
			case "metadata":
				metadata = ""
			case "protocol":
				protocol = "unbound"
			}
			proof.Archive.Payload, err = json.Marshal(map[string]any{
				"schema": 2, "protocol": protocol, "subject": proof.Subject,
				"runID": strconv.FormatInt(proof.RunID, 10), "runAttempt": strconv.Itoa(proof.Attempt),
				"contexts": contexts, "metadataDigest": metadata,
			})
			if err != nil {
				t.Fatal(err)
			}
			executionSignArchive(t, proof.Archive, key, proof.Subject.Repo, proof.RunID, proof.Attempt)
			err = validateExecutionArchive(proof)
			if (scenario == "passed") != (err == nil) {
				t.Fatalf("scenario %s: %v", scenario, err)
			}
		})
	}
}

func TestExecutionMaterializerPrivateReplayAndPartialPrefix(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	admitted, err := store.PlanExecution(cfg, in)
	if err != nil || admitted.Plan == nil {
		t.Fatalf("plan: %+v %v", admitted, err)
	}
	plan := *admitted.Plan
	request := map[string]any{"schema": 1, "protocol": "integration-prefix-v1", "engine": "hive",
		"repo": plan.Recipe.Repo, "base": plan.Recipe.Base, "baseSHA": plan.Recipe.BaseSHA, "policy": plan.Recipe.Policy,
		"generation": int64(plan.Attempt), "members": plan.Recipe.Members, "createdAt": plan.AttemptAt.UTC().Format(time.RFC3339Nano)}
	_, plan.RequestDigest, err = forgestate.CanonicalIntegrationPreparation(request)
	if err != nil {
		t.Fatal(err)
	}
	subject := executionProofFixture().Subject
	subject.Repo, subject.Base, subject.BaseSHA, subject.Policy, subject.WorkflowTree = plan.Recipe.Repo, plan.Recipe.Base, plan.Recipe.BaseSHA, plan.Recipe.Policy, plan.Recipe.WorkflowTree
	subject.Members = plan.Recipe.Members[:1]
	subject.PreparedBy = forgestate.IntegrationPreparationSource{SourceSHA: cfg.Scopes[0].WorkflowTree, RequestDigest: plan.RequestDigest, GitVersion: "git fixture"}
	plan.Prepared = []forgestate.IntegrationCandidateSubject{subject}
	receipt := map[string]any{"schema": 1, "protocol": "integration-materializer-receipt-v1", "request": request, "preparedBy": subject.PreparedBy,
		"prepared": []any{map[string]any{"subject": subject, "publication": "accepted", "dispatch": "accepted"}}}
	payload, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	proof := executionProofFixture()
	archive := executionSignedArchive(t, proof, key)
	archive.Role, archive.Payload, archive.WorkflowSHA, archive.WorkflowPath, archive.Ref, archive.Event =
		"materializer", payload, subject.PreparedBy.SourceSHA, ".github/workflows/integration-candidate-prepare.yml", "refs/heads/v5", "repository_dispatch"
	plan.MaterializerRunID, plan.MaterializerAttempt, plan.Materialization = 20, 1, archive
	executionSignArchive(t, archive, key, plan.Recipe.Repo, 20, 1)
	plan.Generation++
	if err := store.AppendExecution("partial-materialization", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Plan: &plan}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExecution(store.path)
	if err != nil {
		t.Fatal(err)
	}
	saved, ok := reopened.ExecutionPlan(plan.Recipe.ID())
	if !ok || saved.Materialization == nil || len(saved.Prepared) != 1 || len(saved.Recipe.Members) != 2 {
		t.Fatal("lost partial prefix/private request archive")
	}
	for _, test := range []struct {
		name   string
		change func(*ExecutionPlan)
	}{
		{"request", func(p *ExecutionPlan) { p.RequestDigest = strings.Repeat("f", 64) }},
		{"generation", func(p *ExecutionPlan) { p.Attempt++ }},
		{"scope", func(p *ExecutionPlan) { p.Recipe.Base = "other" }},
		{"head", func(p *ExecutionPlan) { p.Recipe.Members[0].Head = strings.Repeat("f", 40) }},
		{"prefix", func(p *ExecutionPlan) { p.Prepared[0].Prefixes[0].Tree = strings.Repeat("f", 40) }},
		{"publication", func(p *ExecutionPlan) { p.Prepared = nil }},
		{"payload", func(p *ExecutionPlan) { p.Materialization.Payload = []byte("{") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := cloneExecution(saved)
			test.change(&changed)
			if err := validateMaterializationArchive(changed); err == nil {
				t.Fatal("accepted altered materializer request/prefix history")
			}
		})
	}
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
