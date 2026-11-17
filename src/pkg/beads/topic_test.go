package beads

import (
	"errors"
	"strings"
	"testing"
)

func newTopicStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetHiveID("hive-test")
	return s
}

func TestClaimTopicRefusesSecondOpenClaimAndFollowsRename(t *testing.T) {
	s := newTopicStore(t)
	got, err := s.ClaimTopic("deps", TopicClaim{Key: " Deps-Bump ", Repo: "acme/widget", PR: 12, Branch: "deps/a"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "deps-bump" || got.PR != 12 || got.Branch != "deps/a" || got.State != TopicStateOpen || got.Actor != "deps" || got.BeadID == "" {
		t.Fatalf("claim = %+v", got)
	}

	// Same PR again (a later kick, the branch renamed): idempotent, branch follows.
	again, err := s.ClaimTopic("deps", TopicClaim{Key: "deps-bump", Repo: "ACME/widget", PR: 12, Branch: "deps/b"})
	if err != nil || again.BeadID != got.BeadID || again.Branch != "deps/b" {
		t.Fatalf("re-claim = %+v, %v", again, err)
	}
	// Same PR with no branch keeps the recorded one.
	if keep, err := s.ClaimTopic("deps", TopicClaim{Key: "deps-bump", Repo: "acme/widget", PR: 12}); err != nil || keep.Branch != "deps/b" {
		t.Fatalf("re-claim without branch = %+v, %v", keep, err)
	}

	// A different PR for the open topic is refused and names the holder.
	held, err := s.ClaimTopic("deps", TopicClaim{Key: "deps-bump", Repo: "acme/widget", PR: 13})
	if !errors.Is(err, ErrTopicClaimed) || held.PR != 12 || !strings.Contains(err.Error(), "acme/widget#12") {
		t.Fatalf("second claim = %+v, %v", held, err)
	}
	if _, err := s.ClaimTopic("deps", TopicClaim{Key: "deps-bump", Repo: "other/widget", PR: 12}); !errors.Is(err, ErrTopicClaimed) {
		t.Fatalf("same number in another repo must be refused, got %v", err)
	}

	// Durable: a fresh store on the same dir sees the claim.
	reopened, err := NewStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := reopened.Topic("DEPS-BUMP")
	if !ok || c.PR != 12 || c.Branch != "deps/b" {
		t.Fatalf("reopened Topic = %+v, %v", c, ok)
	}
	if b, _ := reopened.Get(c.BeadID); b.Meta(hiveIDMetadataKey) != "hive-test" || b.Status != StatusInProgress || b.Type != TypeChore {
		t.Fatalf("bead = %+v", b)
	}
}

func TestClaimTopicValidation(t *testing.T) {
	s := newTopicStore(t)
	for name, c := range map[string]TopicClaim{
		"empty key": {Key: "  ", Repo: "a/b", PR: 1},
		"space key": {Key: "a b", Repo: "a/b", PR: 1},
		"long key":  {Key: strings.Repeat("k", maxTopicKeyLen+1), Repo: "a/b", PR: 1},
		"no repo":   {Key: "k", PR: 1},
		"no pr":     {Key: "k", Repo: "a/b"},
		"negative":  {Key: "k", Repo: "a/b", PR: -1},
	} {
		if _, err := s.ClaimTopic("x", c); err == nil {
			t.Errorf("%s: claim accepted", name)
		}
	}
	if _, ok := s.Topic("bad key"); ok {
		t.Error("Topic accepted an invalid key")
	}
	if _, err := s.ReleaseTopic("", TopicStateReleased); err == nil {
		t.Error("ReleaseTopic accepted an empty key")
	}
}

func TestReleaseTopicAndReclaim(t *testing.T) {
	s := newTopicStore(t)
	first, _ := s.ClaimTopic("deps", TopicClaim{Key: "k", Repo: "acme/widget", PR: 1})
	released, err := s.ReleaseTopic("k", "")
	if err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if _, ok := s.Topic("k"); ok {
		t.Fatal("released topic still open")
	}
	b, _ := s.Get(first.BeadID)
	if b.Status != StatusClosed || b.Meta(MetaTopicState) != TopicStateReleased || b.ClosedAt == nil {
		t.Fatalf("released bead = %+v", b)
	}
	if again, err := s.ReleaseTopic("k", TopicStateReleased); err != nil || again {
		t.Fatalf("second release = %v, %v", again, err)
	}
	// Released: a new PR may claim the topic.
	if c, err := s.ClaimTopic("deps", TopicClaim{Key: "k", Repo: "acme/widget", PR: 2}); err != nil || c.BeadID == first.BeadID {
		t.Fatalf("reclaim = %+v, %v", c, err)
	}
}

func TestReleaseTopicsForPRAndSetBranch(t *testing.T) {
	s := newTopicStore(t)
	_, _ = s.ClaimTopic("deps", TopicClaim{Key: "a", Repo: "acme/widget", PR: 5, Branch: "old"})
	_, _ = s.ClaimTopic("deps", TopicClaim{Key: "b", Repo: "acme/widget", PR: 6})

	if n, err := s.SetTopicBranch("widget", 5, "new"); err != nil || n != 1 {
		t.Fatalf("SetTopicBranch = %d, %v", n, err)
	}
	if n, _ := s.SetTopicBranch("widget", 5, "new"); n != 0 {
		t.Fatalf("unchanged branch counted %d", n)
	}
	if n, _ := s.SetTopicBranch("widget", 5, " "); n != 0 {
		t.Fatalf("empty branch counted %d", n)
	}
	if c, _ := s.Topic("a"); c.Branch != "new" {
		t.Fatalf("branch = %q", c.Branch)
	}

	if n, err := s.ReleaseTopicsForPR("acme/widget", 5, TopicStateMerged); err != nil || n != 1 {
		t.Fatalf("ReleaseTopicsForPR = %d, %v", n, err)
	}
	if n, _ := s.ReleaseTopicsForPR("acme/widget", 0, TopicStateMerged); n != 0 {
		t.Fatalf("pr 0 released %d", n)
	}
	open := s.OpenTopics()
	if len(open) != 1 || open[0].Key != "b" {
		t.Fatalf("open = %+v", open)
	}
	closed := 0
	for _, b := range s.List(ListFilter{}) {
		if b.Meta(MetaTopicState) == TopicStateMerged && b.Status == StatusClosed {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("merged beads = %d", closed)
	}
}

func TestSameTopicRepoAndProjection(t *testing.T) {
	if !SameTopicRepo("Acme/Widget", "acme/widget") || SameTopicRepo("a/widget", "b/widget") ||
		!SameTopicRepo("widget", "a/widget") || SameTopicRepo("", "widget") {
		t.Fatal("SameTopicRepo mismatch")
	}
	b := &Bead{ID: "x", ExternalRef: TopicRefPrefix + "k", Metadata: map[string]interface{}{MetaTopicPR: float64(9)}}
	if c := topicFromBead(b); c.PR != 9 || c.State != TopicStateOpen || c.Key != "k" {
		t.Fatalf("projection = %+v", c)
	}
	if isOpenTopicBead(&Bead{ExternalRef: "https://x"}) || isOpenTopicBead(nil) {
		t.Fatal("non-topic bead counted as a topic")
	}
}
