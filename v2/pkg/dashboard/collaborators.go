package dashboard

// Contributor collaborators — the people you have worked alongside.
//
// The dossier's Collaborators zone used to be a hardcoded empty state with no
// data source behind it; it could never fill. This file gives it a real one.
//
// Design rules (they mirror the rest of the dossier):
//   - APPEND-ONLY. A collaboration, once recorded, is never removed and never
//     decays. Stepping away costs you nothing, exactly like the Golden Path.
//   - SYMMETRIC. Meeting is mutual: every record is written to BOTH profiles, so
//     neither party's dossier disagrees with the other's.
//   - HONEST. A record is only ever written from an event that actually
//     happened: an invite that was redeemed, an issue two people both worked, or
//     a session where both were genuinely on operation. Nothing is inferred from
//     mere co-membership of the hive.
//   - BOUNDED. The list is capped so a busy hive cannot grow a profile file
//     without limit; the earliest-met are kept, because the first people you
//     worked with are the ones worth remembering.

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxCollaborators bounds the stored list per profile.
	maxCollaborators = 64

	// How a collaboration was FIRST established. The origin story is never
	// overwritten by a later, more ordinary meeting.
	collabHowInvite   = "invite"
	collabHowIssue    = "issue"
	collabHowPresence = "presence"
)

// CollaboratorRecord is one person you have worked alongside, and how you met.
type CollaboratorRecord struct {
	Username string `json:"username"`
	// FirstMetAt is when the first qualifying event between you occurred.
	FirstMetAt string `json:"first_met_at"`
	// Occasions counts qualifying events, so a long-running working
	// relationship reads differently from a single crossing of paths.
	Occasions int `json:"occasions"`
	// How records the FIRST way you met: invite | issue | presence.
	How string `json:"how"`
}

// collabMu serialises the read-modify-write of the two profiles a collaboration
// touches. The profile store is plain files with no locking of its own, so two
// concurrent completions on the same issue could otherwise lose a record.
var collabMu sync.Mutex

// collabHowRank orders the ways of meeting by how much they mean. When a pair
// meets again by a *stronger* route than the one on record, the stored How is
// upgraded — being invited by someone and later working an issue with them is
// better described as having worked together.
func collabHowRank(how string) int {
	switch how {
	case collabHowIssue:
		return 3
	case collabHowInvite:
		return 2
	case collabHowPresence:
		return 1
	}
	return 0
}

// recordCollaboration records that a and b worked together, writing the pair
// symmetrically into both profiles. It is the single mutation point for the
// collaborator graph.
//
// It is idempotent per event only in the sense that it always counts an
// occasion; callers that may fire repeatedly for the SAME underlying event
// (backfill, replayed presence) must guard with collaborationExists.
func recordCollaboration(a, b, how string, when time.Time) {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" || strings.EqualFold(a, b) {
		return // nobody collaborates with themselves
	}
	collabMu.Lock()
	defer collabMu.Unlock()
	addCollaboratorLocked(a, b, how, when)
	addCollaboratorLocked(b, a, how, when)
}

// addCollaboratorLocked writes one direction of the pair. Caller holds collabMu.
//
// The owner is resolved through findContributor, not loadContributorProfile: the
// latter is an exact-case file read, and a GitHub login's case is NOT stable
// across the surfaces that feed us (InvitedBy comes from the OAuth session via
// resolveViewerUsername, while the profile filename carries whatever case first
// registered). An exact-case lookup would silently drop one half of the pair and
// break the SYMMETRIC invariant this file exists to hold.
func addCollaboratorLocked(owner, other, how string, when time.Time) {
	p := findContributor(owner)
	if p == nil || p.GitHubUsername == "" {
		return // no profile on this hive — nothing to record against
	}
	// Record the canonical stored spelling, never the caller's casing.
	if canon := findContributor(other); canon != nil && canon.GitHubUsername != "" {
		other = canon.GitHubUsername
	}
	ts := when.UTC().Format(time.RFC3339)
	for i := range p.Collaborators {
		if strings.EqualFold(p.Collaborators[i].Username, other) {
			p.Collaborators[i].Occasions++
			if collabHowRank(how) > collabHowRank(p.Collaborators[i].How) {
				p.Collaborators[i].How = how
			}
			_ = saveContributorProfile(p)
			return
		}
	}
	if len(p.Collaborators) >= maxCollaborators {
		return // bounded: keep the earliest-met
	}
	p.Collaborators = append(p.Collaborators, CollaboratorRecord{
		Username:   other,
		FirstMetAt: ts,
		Occasions:  1,
		How:        how,
	})
	_ = saveContributorProfile(p)
}

// collaborationExists reports whether owner already has other on record. Callers
// that may re-fire for the same underlying event use this to stay idempotent.
// Resolves through findContributor for the same case-stability reason as the
// writer — a case-sensitive miss here would re-record the pair on every boot.
func collaborationExists(owner, other string) bool {
	p := findContributor(owner)
	if p == nil {
		return false
	}
	for i := range p.Collaborators {
		if strings.EqualFold(p.Collaborators[i].Username, other) {
			return true
		}
	}
	return false
}

// sortedCollaborators returns the record ordered for display: most occasions
// first, then earliest-met, then name — deterministic in every case.
func sortedCollaborators(p *ContributorProfile) []CollaboratorRecord {
	if p == nil || len(p.Collaborators) == 0 {
		return []CollaboratorRecord{}
	}
	out := make([]CollaboratorRecord, len(p.Collaborators))
	copy(out, p.Collaborators)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Occasions != out[j].Occasions {
			return out[i].Occasions > out[j].Occasions
		}
		if out[i].FirstMetAt != out[j].FirstMetAt {
			return out[i].FirstMetAt < out[j].FirstMetAt
		}
		return out[i].Username < out[j].Username
	})
	return out
}

// backfillInviteCollaborations seeds the graph from invite attribution that was
// recorded before collaborators existed, so the zone is not empty on the day it
// ships for hives that have been running invites for months.
//
// It is idempotent: a pair already on record is skipped, so repeated boots never
// inflate Occasions. Runs once at startup and is cheap — the profile store is a
// flat directory that is already listed for the leaderboard.
func backfillInviteCollaborations() int {
	profiles := listContributorProfiles()
	n := 0
	for i := range profiles {
		inviter := strings.TrimSpace(profiles[i].InvitedBy)
		invitee := profiles[i].GitHubUsername
		if inviter == "" || invitee == "" || strings.EqualFold(inviter, invitee) {
			continue
		}
		// Both directions: a half-written pair (from an earlier case-sensitive
		// miss) must still be repaired, not skipped forever.
		if collaborationExists(invitee, inviter) && collaborationExists(inviter, invitee) {
			continue
		}
		when := time.Now()
		if t, err := time.Parse(time.RFC3339, profiles[i].RegisteredAt); err == nil {
			when = t // met when they joined, not when the backfill ran
		}
		recordCollaboration(invitee, inviter, collabHowInvite, when)
		n++
	}
	return n
}
