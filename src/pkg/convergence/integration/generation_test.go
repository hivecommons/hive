package integration

import (
	"strings"
	"testing"
	"time"
)

func TestLateOldWorkflowGenerationCannotReviveCurrentProof(t *testing.T) {
	for _, newerFirst := range []bool{false, true} {
		store := testStore(t)
		old := Evidence{Subject: testSubject(), State: Passed, ObservedAt: epoch.Add(time.Hour), Source: "bound-producer", RunID: 1, SuiteID: 2, CheckIDs: []int64{3}}
		newer := old
		newer.Subject.Generation++
		newer.Subject.TestedSHA = strings.Repeat("f", 40)
		newer.ObservedAt = epoch
		newer.RunID = 4
		events := []Event{{Version: 1, ID: "old", At: epoch.Add(time.Hour), Evidence: &old}, {Version: 1, ID: "new", At: epoch, Evidence: &newer}}
		if newerFirst {
			events[0], events[1] = events[1], events[0]
		}
		for _, event := range events {
			if err := store.Append(event); err != nil {
				t.Fatal(err)
			}
		}
		if store.evidence[old.Subject.ID()].State != Superseded || store.evidence[newer.Subject.ID()].State != Passed {
			t.Fatalf("generation ordering depended on delivery/update timestamps: %+v", store.evidence)
		}
		restarted, err := Open(store.path)
		if err != nil || restarted.evidence[old.Subject.ID()].State != Superseded {
			t.Fatalf("restart revived old generation: %v", err)
		}
	}
}

func TestOneBoundWorkflowAttemptHasOneImmutableTestedSubject(t *testing.T) {
	store := testStore(t)
	receipt := Evidence{Subject: testSubject(), State: Passed, ObservedAt: epoch, Source: "github-actions-bound-v1", RunID: 20, SuiteID: 2, CheckIDs: []int64{3}}
	if err := store.Append(Event{Version: 1, ID: "first", At: epoch, Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
	receipt.Subject.TestedTree = strings.Repeat("f", 40)
	if store.Append(Event{Version: 1, ID: "conflicting-subject", At: epoch, Evidence: &receipt}) == nil {
		t.Fatal("same physical workflow attempt changed tested result")
	}
}

func TestUnknownBoundObservationCanAcquireExactAttemptIdentity(t *testing.T) {
	store := testStore(t)
	receipt := Evidence{Subject: testSubject(), State: Unknown, ObservedAt: epoch, Source: "github-actions-bound-v1", RunID: 20, SuiteID: 2}
	if err := store.Append(Event{Version: 1, ID: "unknown", At: epoch, Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
	receipt.Subject.TestedTree = strings.Repeat("f", 40)
	receipt.State, receipt.CheckIDs = Passed, []int64{3}
	if err := store.Append(Event{Version: 1, ID: "bound", At: epoch, Evidence: &receipt}); err != nil {
		t.Fatalf("unknown placeholder prevented actual attempt proof: %v", err)
	}
}

func TestFailedAuthorHeadRequiresExactSuiteAndCheckForCandidateContext(t *testing.T) {
	store := testStore(t)
	receipt := Evidence{Subject: testSubject(), State: Passed, ObservedAt: epoch, Source: "github-actions-attested-v1", RunID: 20, SuiteID: 2, CheckIDs: []int64{3}}
	if err := store.Append(Event{Version: 1, ID: "candidate", At: epoch, Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
	head := receipt.Subject.Members[0].Head
	for _, tc := range []struct {
		head         string
		suite, check int64
		want         int
	}{
		{head, 2, 3, 1}, {head, 2, 0, 1}, {head, 0, 3, 0}, {head, 2, 99, 0}, {head, 99, 3, 0}, {strings.Repeat("f", 40), 2, 3, 0}, {receipt.Subject.TestedSHA, 0, 0, 1},
	} {
		if got := len(store.ForFailure("o/r", tc.head, tc.suite, tc.check)); got != tc.want {
			t.Fatalf("failure %+v got %d receipts", tc, got)
		}
	}
}
