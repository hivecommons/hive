package automerge

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// humanClaimStub answers Engine.adoptionHumanClaim in place of the forge. It
// embeds the concrete client so every optional transport hook the engine type
// asserts for (the stage writer, the write-back gate) is still provided.
type humanClaimStub struct {
	*hgithub.Client
	mu     sync.Mutex
	reason string
	asked  []string
}

func (s *humanClaimStub) IssueHumanClaimReason(_ context.Context, _ string, _ int, holder string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, holder)
	return s.reason, nil
}

func (s *humanClaimStub) set(reason string) {
	s.mu.Lock()
	s.reason = reason
	s.mu.Unlock()
}

// An owner's claim on an issue the contributor PR closes keeps the hive from
// adopting it, however long the contributor has been offline
// (hivecommons/hive#18899); the block is on the record for the tracker row
// and lifts the moment the person releases the claim.
func TestAdoptionRefusedWhileAPersonHoldsAClaimOnTheIssue(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, adoptPolicy())
	defer done()
	stub := &humanClaimStub{Client: h.e.transport.(*hgithub.Client)}
	h.e.transport = stub
	start := h.clock.at
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.observe(adoptPR(7, "aaa"), "conflicting")
	if len(nudgeComments(h.api)) != 1 {
		t.Fatalf("no nudge: %v", h.api.comments)
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	key := stageKey("acme", "widget", 7)

	// Offline past the 15m wait, but the owner claimed the issue.
	stub.set(claims.HumanClaimReason("assigned", "andy"))
	h.api.reset()
	h.clock.at = start.Add(31 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "conflicting")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("adopted over a person's claim: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptionBlocked("aaa") != stagelabels.AdoptBlockedHumanClaim {
		t.Fatalf("block not recorded: %+v", rec)
	}
	if len(stub.asked) == 0 || stub.asked[0] != "mallory" {
		t.Fatalf("the claim check must name the PR author as the holder, asked %v", stub.asked)
	}

	// The person releases the claim: the next pass adopts.
	stub.set("")
	h.api.reset()
	h.clock.at = start.Add(46 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "conflicting")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("adoption did not resume after the claim lifted: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
}
