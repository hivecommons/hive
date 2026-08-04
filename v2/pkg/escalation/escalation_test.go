package escalation

import (
	"path/filepath"
	"strings"
	"testing"
)

func obs(repo string, num int, sha string, red bool) Observation {
	return Observation{Repo: repo, Number: num, HeadSHA: sha, Red: red, Excerpt: "ReferenceError: seedMission is not defined"}
}

func TestSweep_EscalatesAtThresholdOfDistinctRedSHAs(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "streaks.json"))

	// Attempt 1: red.
	r := s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 1 || got.NewlyEscala {
		t.Fatalf("attempt 1: got %+v", got)
	}
	// Same SHA re-observed: NOT a new attempt.
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	if got := r[Key("org/repo", 7)]; got.Attempts != 1 {
		t.Fatalf("same sha must not increment: got %+v", got)
	}
	// Attempts 2 and 3: new SHAs, still red — threshold crossed on 3.
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha3", true)}, 3)
	got := r[Key("org/repo", 7)]
	if got.Attempts != 3 || !got.NewlyEscala {
		t.Fatalf("attempt 3 must escalate: got %+v", got)
	}

	// After MarkEscalated, further sweeps must not re-fire.
	s.MarkEscalated("org/repo", 7)
	r = s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	got = r[Key("org/repo", 7)]
	if got.NewlyEscala || !got.Escalated {
		t.Fatalf("escalation must fire once: got %+v", got)
	}
}

func TestSweep_GreenResetsAndAbsencePrunes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaks.json")
	s := Load(path)

	s.Sweep([]Observation{obs("org/repo", 7, "sha1", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 7, "sha2", true)}, 3)
	// Goes green: history forgotten.
	s.Sweep([]Observation{obs("org/repo", 7, "sha3", false)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("green must reset, got %d attempts", n)
	}
	// Red again, then vanishes from the open set (merged/closed): pruned.
	s.Sweep([]Observation{obs("org/repo", 7, "sha4", true)}, 3)
	s.Sweep([]Observation{obs("org/repo", 8, "shaX", true)}, 3)
	if n := s.Attempts("org/repo", 7); n != 0 {
		t.Fatalf("absent PR must be pruned, got %d attempts", n)
	}

	// Persistence across Load.
	s2 := Load(path)
	if n := s2.Attempts("org/repo", 8); n != 1 {
		t.Fatalf("ledger must persist, got %d attempts for #8", n)
	}
}

func TestCommentBody_LeadsWithEvidence(t *testing.T) {
	body := CommentBody(3, []string{"Coverage Suite", "build-gate"}, "ReferenceError: seedMission is not defined")
	for _, want := range []string{"3 distinct fix attempts", "Coverage Suite", "seedMission is not defined", NeedsHumanLabel} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body missing %q:\n%s", want, body)
		}
	}
}
