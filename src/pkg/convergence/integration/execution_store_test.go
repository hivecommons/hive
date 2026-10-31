package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

func executionProofFixture() ExecutionProof {
	now := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	return ExecutionProof{
		Subject: forgestate.IntegrationCandidateSubject{
			Schema: 1, Protocol: "integration-prefix-v1", Engine: "hive", Repo: "o/r", Base: "v5",
			BaseSHA: strings.Repeat("a", 40), Policy: strings.Repeat("b", 64),
			WorkflowTree: strings.Repeat("c", 40), Generation: 1, ID: strings.Repeat("d", 64),
			Members:  []forgestate.IntegrationCandidateMember{{PR: 1, Head: strings.Repeat("e", 40)}},
			Prefixes: []forgestate.IntegrationCandidatePrefix{{Commit: strings.Repeat("f", 40), Tree: strings.Repeat("1", 40)}},
		},
		State: Running, ObservedAt: now, RunID: 10, Attempt: 1, Source: "github-actions-oidc-v1",
	}
}

func TestExecutionJournalOutOfOrderAttemptInvalidationAndReplay(t *testing.T) {
	path := executionFixturePath(t)
	store, err := OpenExecution(path)
	if err != nil {
		t.Fatal(err)
	}
	p := executionProofFixture()
	add := func(id string, p ExecutionProof) {
		t.Helper()
		if err := store.AppendExecution(id, p.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Proof: &p}); err != nil {
			t.Fatal(err)
		}
	}
	add("attempt1", p)
	add("attempt1", p)
	newer := cloneExecution(p)
	newer.Attempt, newer.ObservedAt = 2, p.ObservedAt.Add(time.Minute)
	add("attempt2", newer)
	p.ObservedAt = p.ObservedAt.Add(2 * time.Minute)
	add("late1", p)
	old, ok := store.ExecutionProof(p.Subject.ID, 10, 1)
	if !ok || old.State != Superseded {
		t.Fatalf("late stale attempt revived: %+v", old)
	}
	invalidation := ExecutionInvalidation{Repo: "o/r", Base: "v5", PR: 1, Revision: 1, Reason: "review withdrawn at unchanged head"}
	if err := store.AppendExecution("withdraw", newer.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Invalidate: &invalidation}); err != nil {
		t.Fatal(err)
	}
	current, ok := store.ExecutionProof(p.Subject.ID, 10, 2)
	if !ok || current.State != Superseded || current.ObservedState != Running {
		t.Fatalf("withdrawal lost technical/eligibility distinction: %+v", current)
	}
	newer.ObservedAt = newer.ObservedAt.Add(3 * time.Minute)
	add("stale-eligibility", newer)
	current, _ = store.ExecutionProof(p.Subject.ID, 10, 2)
	if current.State != Superseded {
		t.Fatal("old eligibility revision revived a held proof")
	}
	newer.EligibilityRevision = 1
	newer.ObservedAt = newer.ObservedAt.Add(time.Minute)
	add("fresh-eligibility", newer)
	replay, err := OpenExecution(path)
	if err != nil {
		t.Fatal(err)
	}
	current, _ = replay.ExecutionProof(p.Subject.ID, 10, 2)
	if current.State != Running || current.EligibilityRevision != 1 {
		t.Fatalf("fresh exact revision replay: %+v", current)
	}
	copyProof, _ := replay.ExecutionProof(p.Subject.ID, 10, 2)
	copyProof.Subject.Members[0].Head = "mutated by caller"
	current, _ = replay.ExecutionProof(p.Subject.ID, 10, 2)
	if current.Subject.Members[0].Head != p.Subject.Members[0].Head {
		t.Fatal("private store alias escaped")
	}
}

func TestExecutionRejectsMalformedObservationAndPartitionMixing(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ExecutionProof)
	}{
		{"unsigned-pass", func(p *ExecutionProof) { p.State = Passed }},
		{"source", func(p *ExecutionProof) { p.Source = "artifact-assertion" }},
		{"id", func(p *ExecutionProof) { p.Subject.ID = strings.Repeat("0", 64) }},
		{"zero-head", func(p *ExecutionProof) { p.Subject.Members[0].Head = strings.Repeat("0", 40) }},
		{"object", func(p *ExecutionProof) { p.Subject.Prefixes[0].Commit = "short" }},
		{"state", func(p *ExecutionProof) { p.State = "green-ish" }},
		{"run", func(p *ExecutionProof) { p.RunID = 0 }},
		{"attempt", func(p *ExecutionProof) { p.Attempt = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenExecution(executionFixturePath(t))
			if err != nil {
				t.Fatal(err)
			}
			proof := executionProofFixture()
			test.change(&proof)
			if err := store.AppendExecution("bad", proof.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Proof: &proof}); err == nil {
				t.Fatal("accepted invalid execution proof")
			}
		})
	}
	store, err := OpenExecution(executionFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	proof := executionProofFixture()
	if err := store.AppendExecution("wrong-version", proof.ObservedAt, ExecutionEvent{Version: 999, Proof: &proof}); err == nil {
		t.Fatal("accepted future schema")
	}
	if err := store.AppendExecution("no-observation", proof.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion}); err == nil {
		t.Fatal("accepted empty event")
	}
	if err := store.Append(Event{Version: SchemaVersion, ID: "v1-release", At: proof.ObservedAt, Release: "*"}); err == nil {
		t.Fatal("execution journal accepted legacy admission mutation")
	}
	if err := store.AppendExecution("core", proof.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Proof: &proof}); err != nil {
		t.Fatal(err)
	}
	proof.Subject.BaseSHA = strings.Repeat("2", 40)
	if err := store.AppendExecution("same-attempt-other-core", proof.ObservedAt, ExecutionEvent{Version: ExecutionSchemaVersion, Proof: &proof}); err == nil {
		t.Fatal("one workflow attempt changed its immutable subject")
	}
}

func TestExecutionEligibilityAgingAndPublicPrivateProjection(t *testing.T) {
	store, _, in := executionAdmissionFixture(t)
	eligible := ExecutionEligibility{Repo: "o/r", Base: "v5", PR: 1, Head: in.Members[0].Head,
		WorkKey: "o/r#10", ReadyAt: in.Now, ObservedAt: in.Now, Source: "normal-automerge-lane-v1"}
	for i := range 2 {
		eligible.ObservedAt = in.Now.Add(time.Duration(i) * time.Minute)
		eligible.ReadyAt = eligible.ObservedAt
		if err := store.AppendExecution("eligible-"+eligible.ObservedAt.String(), eligible.ObservedAt,
			ExecutionEvent{Version: ExecutionSchemaVersion, Eligibility: &eligible}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.ExecutionSnapshot()
	if len(snapshot.Eligibility) != 1 || snapshot.Eligibility[0].ReadyAt != in.Now {
		t.Fatalf("waiting age reset: %+v", snapshot.Eligibility)
	}
	store.mu.Lock()
	store.executionData().proofs["private"] = ExecutionProof{Archive: &ExecutionArchive{Token: "private-token", Payload: []byte("private-payload")}}
	store.executionData().plans["private"] = ExecutionPlan{Materialization: &ExecutionArchive{Token: "private-materializer"}}
	store.mu.Unlock()
	raw, err := json.Marshal(store.ExecutionSnapshot())
	if err != nil || strings.Contains(string(raw), "private-token") || strings.Contains(string(raw), "private-materializer") {
		t.Fatalf("private receipt escaped projection: %s %v", raw, err)
	}
}

func TestExecutionIdentityDurablePrivateAndNoAutomaticRepair(t *testing.T) {
	path := executionFixturePath(t) + ".identity"
	first, err := OpenExecutionIdentity(path)
	if err != nil {
		t.Fatal(err)
	}

	second, err := OpenExecutionIdentity(path)
	if err != nil || first.ID != second.ID || len(first.ID) != 64 {
		t.Fatalf("identity changed: %+v %+v %v", first, second, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExecutionIdentity(path); err == nil {
		t.Fatal("public identity private state accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExecutionIdentity(path); err == nil {
		t.Fatal("corrupt identity was regenerated/taken over")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "{" {
		t.Fatal("corrupt identity was overwritten")
	}
}

func TestExecutionIdentityRejectsInvalidParentAndFuturePrivateState(t *testing.T) {
	path := executionFixturePath(t) + ".identity"
	if err := os.WriteFile(path, []byte("existing regular file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExecutionIdentity(filepath.Join(path, "child")); err == nil {
		t.Fatal("identity accepted a file as parent")
	}
	if _, err := OpenExecutionIdentity(""); err == nil {
		t.Fatal("identity accepted empty path")
	}
	if err := os.WriteFile(path, []byte(`{"version":999,"key":"AA=="}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenExecutionIdentity(path); err == nil {
		t.Fatal("future/corrupt private identity was accepted or automatically repaired")
	}
}
