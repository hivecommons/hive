package mutation

import (
	"errors"
	"strings"
	"testing"
)

func TestClaimKey_CanonicalForms(t *testing.T) {
	repo := RepoClaim("acme/widgets")
	if got := repo.Key(); got != "mutation:acme/widgets" {
		t.Fatalf("repo key = %q", got)
	}
	task := TaskClaim("acme/widgets", "acme/widgets#7")
	if got := task.Key(); got != "mutation:acme/widgets#7" {
		t.Fatalf("task key = %q", got)
	}
	external := TaskClaim("acme/widgets", "acme/widgets!ENG-42")
	if got := external.Key(); got != "mutation:acme/widgets!ENG-42" {
		t.Fatalf("external task key = %q", got)
	}
}

func TestClaimValidate_RefusesAliasesAndForgeries(t *testing.T) {
	cases := []Claim{
		{Type: ClaimTypeRepo, Repo: ""},
		{Type: ClaimTypeRepo, Repo: "no-slash"},
		{Type: ClaimTypeRepo, Repo: "a/b/c"},
		{Type: ClaimTypeRepo, Repo: "acme/widgets", Subject: "acme/widgets#7"},
		{Type: ClaimTypeTask, Repo: "acme/widgets"},
		{Type: ClaimTypeTask, Repo: "acme/widgets", Subject: "other/repo#7"},
		{Type: ClaimTypeTask, Repo: "acme/widgets", Subject: "acme/widgets"},
		{Type: ClaimTypeTask, Repo: "acme/widgets", Subject: "acme/widgets@outcome"},
		{Type: "path", Repo: "acme/widgets", Subject: "acme/widgets#7"},
	}
	for _, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("claim %+v must be invalid", c)
		}
		if key := c.Key(); key != "" {
			t.Errorf("invalid claim %+v must have no key, got %q", c, key)
		}
	}
}

// The complete accepted overlap algebra: equal keys conflict; the repo-top
// claim conflicts with every claim in that repo; everything else is disjoint.
func TestOverlaps_AcceptedAlgebra(t *testing.T) {
	repoA := RepoClaim("acme/widgets")
	taskA7 := TaskClaim("acme/widgets", "acme/widgets#7")
	taskA9 := TaskClaim("acme/widgets", "acme/widgets#9")
	repoB := RepoClaim("acme/gadgets")
	taskB7 := TaskClaim("acme/gadgets", "acme/gadgets#7")

	cases := []struct {
		a, b Claim
		want bool
	}{
		{taskA7, taskA7, true},   // equal keys
		{repoA, taskA7, true},    // top claim cannot be bypassed
		{taskA7, repoA, true},    // symmetric
		{repoA, repoA, true},     // equal repo claims
		{taskA7, taskA9, false},  // disjoint tasks, same repo
		{repoA, repoB, false},    // different repos
		{taskA7, taskB7, false},  // same number, different repos
		{repoA, taskB7, false},   // top claim scoped to its repo
		{Claim{}, taskA7, false}, // invalid overlaps nothing
	}
	for _, c := range cases {
		if got := c.a.Overlaps(c.b); got != c.want {
			t.Errorf("Overlaps(%s, %s) = %v, want %v", c.a.Key(), c.b.Key(), got, c.want)
		}
		if got := c.b.Overlaps(c.a); got != c.want {
			t.Errorf("overlap must be symmetric for (%s, %s)", c.a.Key(), c.b.Key())
		}
	}
}

func TestEffectLogicalID_ExcludesOwnerAndEpoch(t *testing.T) {
	e := testEffect()
	id := e.LogicalID()
	if id == "" || !strings.HasPrefix(id, "op:") {
		t.Fatalf("logical id = %q", id)
	}
	// Identical inputs → identical ID regardless of who retries.
	if e.LogicalID() != id {
		t.Fatal("logical id must be deterministic")
	}
	// Any load-bearing input change → a DIFFERENT logical operation.
	gen := e
	gen.DesiredGeneration = 2
	head := e
	head.Inputs = map[string]string{"repo": "acme/widgets", "head": "feature-2", "base": "main"}
	for _, other := range []Effect{gen, head} {
		if other.LogicalID() == id {
			t.Fatalf("changed inputs must produce a distinct logical id")
		}
	}
}

func TestEffectValidate_Refusals(t *testing.T) {
	bad := []Effect{
		{},
		func() Effect { e := testEffect(); e.DesiredGeneration = 0; return e }(),
		func() Effect { e := testEffect(); e.Inputs = nil; return e }(),
		func() Effect { e := testEffect(); e.ClaimKey = ""; return e }(),
	}
	for _, e := range bad {
		if err := e.Validate(); err == nil || !errors.Is(err, ErrInvalidEffect) {
			t.Errorf("effect %+v must be invalid", e)
		}
		if e.LogicalID() != "" {
			t.Errorf("invalid effect must have no id")
		}
	}
}

func testEffect() Effect {
	return Effect{
		OutcomeKey:        "default/acme/widgets@ship-green",
		DesiredGeneration: 1,
		Transition:        "contribute.open-pr",
		Subject:           "acme/widgets#7",
		ClaimKey:          TaskClaim("acme/widgets", "acme/widgets#7").Key(),
		Kind:              EffectCreatePR,
		Inputs:            map[string]string{"repo": "acme/widgets", "head": "feature-1", "base": "main"},
	}
}
