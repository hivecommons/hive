package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"strings"
	"testing"
)

func TestExecutionQualificationRequiresActualPromotionAndSignedProofLineage(t *testing.T) {
	store, cfg, in := executionAdmissionFixture(t)
	proof := executionProofFixture()
	proof.Subject.Base = "hive-integration-qualification/fixture"
	proof.State = Passed
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	proof.Archive = executionSignedArchive(t, proof, key)
	last := proof.Subject.Prefixes[len(proof.Subject.Prefixes)-1]
	qualification := ExecutionQualification{ID: "actual-probe", Repo: "o/r", Base: "v5", ProbeBase: proof.Subject.Base,
		ExecutorID: cfg.Scopes[0].ExecutorID, AuthorityCommit: cfg.Scopes[0].AuthorityCommit, RequestActorID: 2,
		Policy: proof.Subject.Policy, WorkflowTree: proof.Subject.WorkflowTree, ProtectionDigest: strings.Repeat("a", 64),
		ProfileDigest: strings.Repeat("b", 64), SubjectID: proof.Subject.ID, TestedSHA: last.Commit, MutationKey: "fenced-promotion", ObservedAt: in.Now}
	event := ExecutionEvent{Version: ExecutionSchemaVersion, Qualification: &qualification}
	if err := store.AppendExecution("without-promotion", in.Now, event); err == nil {
		t.Fatal("qualified a configured boolean")
	}
	promotion := ExecutionPromotion{Repo: "o/r", Base: proof.Subject.Base, SubjectID: proof.Subject.ID, ProviderAcknowledged: true,
		ExpectedSHA: proof.Subject.BaseSHA, TestedSHA: last.Commit, AuthorityCommit: qualification.AuthorityCommit,
		MutationKey: qualification.MutationKey, State: "applied", PR: 1, Head: proof.Subject.Members[0].Head, Attempt: 1, ObservedAt: in.Now}
	unacknowledged, err := OpenExecution(executionFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	reconciled := promotion
	reconciled.ProviderAcknowledged = false
	for _, observed := range []ExecutionEvent{
		{Version: ExecutionSchemaVersion, Proof: &proof},
		{Version: ExecutionSchemaVersion, Promotion: &reconciled},
	} {
		if err := unacknowledged.AppendExecution(fmt.Sprintf("unacknowledged-%t", observed.Proof != nil), in.Now, observed); err != nil {
			t.Fatal(err)
		}
	}
	if err := unacknowledged.AppendExecution("reconciliation-is-not-qualification", in.Now, event); err == nil {
		t.Fatal("an observed destination without successful provider RPC qualified an unproven actor")
	}
	if err := store.AppendExecution("actual-promotion", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Promotion: &promotion}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExecution("without-proof", in.Now, event); err == nil {
		t.Fatal("qualified without trusted technical lineage")
	}
	if err := store.AppendExecution("actual-proof", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Proof: &proof}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendExecution("actual-qualification", in.Now, event); err != nil {
		t.Fatal(err)
	}
	promotion.State = "unknown"
	if err := store.AppendExecution("late-ambiguous-promotion", in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Promotion: &promotion}); err != nil {
		t.Fatal(err)
	}
	replayed, err := OpenExecution(store.path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := replayed.ExecutionSnapshot()
	if len(snapshot.Qualifications) != 1 || len(snapshot.Promotions) != 1 || snapshot.Promotions[0].State != "applied" {
		t.Fatal("late ambiguity erased applied durable qualification lineage")
	}
	for _, test := range []struct {
		name   string
		change func(*ExecutionQualification)
	}{
		{"same-base", func(q *ExecutionQualification) { q.ProbeBase = q.Base }},
		{"not-isolated", func(q *ExecutionQualification) { q.ProbeBase = "topic" }},
		{"wrong-promotion", func(q *ExecutionQualification) { q.MutationKey = "other" }},
		{"wrong-subject", func(q *ExecutionQualification) { q.SubjectID = strings.Repeat("e", 64) }},
		{"wrong-policy", func(q *ExecutionQualification) { q.Policy = strings.Repeat("e", 64) }},
		{"wrong-tested", func(q *ExecutionQualification) { q.TestedSHA = strings.Repeat("e", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := qualification
			test.change(&q)
			if err := replayed.AppendExecution("invalid-"+test.name, in.Now, ExecutionEvent{Version: ExecutionSchemaVersion, Qualification: &q}); err == nil {
				t.Fatal("accepted unbound qualification")
			}
		})
	}
}
