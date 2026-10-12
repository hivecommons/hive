package dashboard

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

const ownershipTransitionAction = "ownership_transition"

// ownershipIdentity is the part of a decision that identifies who owns an
// item: reason text and the offered action are deliberately excluded.
type ownershipIdentity struct {
	YourTurn   bool   `json:"your_turn,omitempty"`
	Verb       string `json:"verb,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
}

func ownershipIdentityOf(o Ownership) ownershipIdentity {
	return ownershipIdentity{YourTurn: o.YourTurn, Verb: o.Verb, ReasonCode: o.ReasonCode}
}

type ownershipAuditFunc func(at time.Time, repo string, number int, detail string)

var (
	ownershipAuditMu   sync.Mutex
	ownershipAuditSink ownershipAuditFunc
)

func setOwnershipAuditSink(fn ownershipAuditFunc) {
	ownershipAuditMu.Lock()
	ownershipAuditSink = fn
	ownershipAuditMu.Unlock()
}

func currentOwnershipAuditSink() ownershipAuditFunc {
	ownershipAuditMu.Lock()
	defer ownershipAuditMu.Unlock()
	return ownershipAuditSink
}

func transitionIdentityKey(repo, kind string, number int) string {
	return repo + "|" + kind + "#" + strconv.Itoa(number)
}

type ownershipTransition struct {
	repo   string
	kind   string
	number int
	url    string
	from   ownershipIdentity
	to     ownershipIdentity
	reason string
}

// recordOwnershipTransitions compares each item's ownership identity with the
// last one kept in the local record, writes one ownership_transition audit
// entry per change and stores the new identities. It runs only in the status
// refresh. The first refresh with no stored identities only stores them, and
// an item that is gone from a complete repository leaves with one entry.
func recordOwnershipTransitions(repos []FrontendRepo, now time.Time) {
	sink := currentOwnershipAuditSink()
	if sink == nil {
		return
	}
	ownershipStateMu.Lock()
	defer ownershipStateMu.Unlock()
	state := loadOwnershipState(ownershipStatePath)
	seeded := state.IdentitiesSeeded
	if state.Identities == nil {
		state.Identities = map[string]ownershipIdentity{}
	}

	present := map[string]bool{}
	complete := map[string]bool{}
	var transitions []ownershipTransition
	changed := !seeded
	for _, repo := range repos {
		repoName := overviewRepoName(repo)
		if repo.CountsIncomplete {
			// Rows may be missing: neither infer departures nor record
			// entries from a partial view.
			for key := range repo.Ownership {
				kind, num := splitOwnershipKey(key)
				present[transitionIdentityKey(repoName, kind, num)] = true
			}
			continue
		}
		complete[repoName] = true
		for key, decision := range repo.Ownership {
			kind, num := splitOwnershipKey(key)
			id := transitionIdentityKey(repoName, kind, num)
			present[id] = true
			cur := ownershipIdentityOf(decision)
			old, known := state.Identities[id]
			if known && old == cur {
				continue
			}
			state.Identities[id] = cur
			changed = true
			if !seeded || (!known && !cur.YourTurn) {
				continue
			}
			transitions = append(transitions, ownershipTransition{repo: repoName, kind: kind, number: num, url: repo.ownershipURLs[key], from: old, to: cur, reason: decision.Reason})
		}
	}
	for id, old := range state.Identities {
		if present[id] {
			continue
		}
		repoName, kind, num, ok := splitTransitionIdentityKey(id)
		if !ok || !complete[repoName] {
			continue
		}
		delete(state.Identities, id)
		changed = true
		if seeded && old.YourTurn {
			transitions = append(transitions, ownershipTransition{repo: repoName, kind: kind, number: num, from: old, reason: "closed or merged"})
		}
	}
	sort.Slice(transitions, func(a, b int) bool {
		x, y := transitions[a], transitions[b]
		if x.repo != y.repo {
			return x.repo < y.repo
		}
		if x.kind != y.kind {
			return x.kind < y.kind
		}
		return x.number < y.number
	})
	if !changed {
		return
	}
	state.IdentitiesSeeded = true
	saveOwnershipState(ownershipStatePath, state)
	for _, t := range transitions {
		sink(now, t.repo, t.number, auditDetail(
			"item", t.repo+"#"+strconv.Itoa(t.number),
			"kind", t.kind,
			"at", now.UTC().Format(time.RFC3339),
			"old_your_turn", strconv.FormatBool(t.from.YourTurn),
			"old_verb", t.from.Verb,
			"old_reason_code", t.from.ReasonCode,
			"new_your_turn", strconv.FormatBool(t.to.YourTurn),
			"new_verb", t.to.Verb,
			"new_reason_code", t.to.ReasonCode,
			"reason", t.reason,
			"url", t.url,
		))
	}
}

func splitOwnershipKey(key string) (string, int) {
	for i := 0; i < len(key); i++ {
		if key[i] == '#' {
			n, _ := strconv.Atoi(key[i+1:])
			return key[:i], n
		}
	}
	return key, 0
}

func splitTransitionIdentityKey(id string) (repo, kind string, number int, ok bool) {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == '|' {
			kind, number = splitOwnershipKey(id[i+1:])
			return id[:i], kind, number, true
		}
	}
	return "", "", 0, false
}
