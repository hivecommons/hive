package dashboard

import (
	"testing"
)

func TestCollabHowRank(t *testing.T) {
	tests := []struct {
		how  string
		want int
	}{
		{collabHowIssue, 3},
		{collabHowInvite, 2},
		{collabHowPresence, 1},
		{"", 0},
		{"unknown", 0},
	}
	for _, tc := range tests {
		if got := collabHowRank(tc.how); got != tc.want {
			t.Errorf("collabHowRank(%q) = %d, want %d", tc.how, got, tc.want)
		}
	}
}

func TestCollabHowRank_Ordering(t *testing.T) {
	// issue > invite > presence
	if collabHowRank(collabHowIssue) <= collabHowRank(collabHowInvite) {
		t.Error("issue should rank higher than invite")
	}
	if collabHowRank(collabHowInvite) <= collabHowRank(collabHowPresence) {
		t.Error("invite should rank higher than presence")
	}
	if collabHowRank(collabHowPresence) <= collabHowRank("") {
		t.Error("presence should rank higher than unknown")
	}
}

func TestSortedCollaborators_NilProfile(t *testing.T) {
	got := sortedCollaborators(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("nil profile: expected empty slice, got %v", got)
	}
}

func TestSortedCollaborators_EmptyList(t *testing.T) {
	p := &ContributorProfile{Collaborators: []CollaboratorRecord{}}
	got := sortedCollaborators(p)
	if len(got) != 0 {
		t.Errorf("empty list: expected empty, got %d items", len(got))
	}
}

func TestSortedCollaborators_OrderByOccasions(t *testing.T) {
	p := &ContributorProfile{
		Collaborators: []CollaboratorRecord{
			{Username: "alice", Occasions: 2, FirstMetAt: "2026-01-01T00:00:00Z"},
			{Username: "bob", Occasions: 5, FirstMetAt: "2026-01-02T00:00:00Z"},
			{Username: "carol", Occasions: 1, FirstMetAt: "2026-01-03T00:00:00Z"},
		},
	}
	got := sortedCollaborators(p)
	if got[0].Username != "bob" || got[1].Username != "alice" || got[2].Username != "carol" {
		t.Errorf("expected bob > alice > carol by occasions, got %s > %s > %s",
			got[0].Username, got[1].Username, got[2].Username)
	}
}

func TestSortedCollaborators_TiebreakByFirstMet(t *testing.T) {
	p := &ContributorProfile{
		Collaborators: []CollaboratorRecord{
			{Username: "bob", Occasions: 3, FirstMetAt: "2026-02-01T00:00:00Z"},
			{Username: "alice", Occasions: 3, FirstMetAt: "2026-01-01T00:00:00Z"},
		},
	}
	got := sortedCollaborators(p)
	// Same occasions → earliest-met first
	if got[0].Username != "alice" {
		t.Errorf("same occasions: expected alice (earlier met) first, got %s", got[0].Username)
	}
}

func TestSortedCollaborators_TiebreakByName(t *testing.T) {
	p := &ContributorProfile{
		Collaborators: []CollaboratorRecord{
			{Username: "zara", Occasions: 1, FirstMetAt: "2026-01-01T00:00:00Z"},
			{Username: "alice", Occasions: 1, FirstMetAt: "2026-01-01T00:00:00Z"},
		},
	}
	got := sortedCollaborators(p)
	// Same occasions, same time → alphabetical
	if got[0].Username != "alice" {
		t.Errorf("same occasions+time: expected alice first, got %s", got[0].Username)
	}
}

func TestSortedCollaborators_DoesNotMutateOriginal(t *testing.T) {
	p := &ContributorProfile{
		Collaborators: []CollaboratorRecord{
			{Username: "bob", Occasions: 1},
			{Username: "alice", Occasions: 5},
		},
	}
	_ = sortedCollaborators(p)
	// Original order preserved
	if p.Collaborators[0].Username != "bob" {
		t.Error("sortedCollaborators mutated the original slice")
	}
}
