package dashboard

import (
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageTrackerPersonPresence describes the named author a PR waits on. Unknown
// means no registered clanker, or an unavailable presence source; Kind preserves
// the author classification independently of relay registration.
type StageTrackerPersonPresence struct {
	Login    string     `json:"login"`
	State    string     `json:"state"`
	Kind     string     `json:"kind"`
	Since    *time.Time `json:"since,omitempty"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Next     string     `json:"next"`
	AdoptAt  *time.Time `json:"adopt_at,omitempty"`
	// Available distinguishes an unavailable hub from a known absent profile.
	Available bool `json:"available"`
}

func (s *Server) stageTrackerPersonPresence(it StageTrackerItem, rec stagelabels.Record, hasRec bool, lookup func(string) (automerge.ContributorPresence, bool)) *StageTrackerPersonPresence {
	if it.Kind != "pr" || it.Author == "" || (it.Stage != trackerNumberNeedsAuthor && it.Stage != trackerNumberReturned) {
		return nil
	}
	out := &StageTrackerPersonPresence{Login: it.Author, State: "unknown", Kind: "clanker", Next: "wait"}
	if it.AuthorKind == authorKindHuman {
		out.Kind = "human"
	}
	var p automerge.ContributorPresence
	if lookup != nil {
		p, out.Available = lookup(it.Author)
	}
	if out.Available && p.Registered {
		out.State = "offline"
		if p.Online {
			out.State = "online"
		}
	}
	if !p.LastSeen.IsZero() {
		t := p.LastSeen
		out.LastSeen = &t
	}
	// Offline age is counted from the later heartbeat / current-head nudge,
	// just like adoptionDue. Before the first nudge only presence is known.
	var start time.Time
	if hasRec && rec.NudgeHead == it.Head {
		start = rec.NudgeStart()
		if !start.IsZero() {
			out.Since = &start
		}
	}
	if p.LastSeen.After(start) {
		t := p.LastSeen
		out.Since = &t
	}
	if it.AuthorKind != authorKindClanker || it.Stage != trackerNumberReturned || !hasRec {
		return out
	}
	var cfg config.StageLabelsConfig
	if s != nil && s.deps != nil && s.deps.Config != nil {
		cfg = s.deps.Config.Governor.Labels.Stage
	}
	if cfg.Adoption.Enabled != nil && !*cfg.Adoption.Enabled {
		return out
	}
	if rec.AdoptionBlocked(it.Head) != "" || stageTrackerPresenceHeld(it.Labels) {
		return out
	}
	policy := automerge.StageLabelPolicy{
		GraceOnline: cfg.EffectiveContributorGraceOnline(), GraceOffline: cfg.EffectiveContributorGraceOffline(),
		AdoptAfterOffline: cfg.Adoption.EffectiveAfterOffline(), AdoptAfterUnregistered: cfg.Adoption.EffectiveAfterUnregistered(),
	}
	at, _ := automerge.AdoptionDeadline(rec, it.Head, policy, p, out.Available)
	if !at.IsZero() {
		out.Next, out.AdoptAt = "adopt", &at
	}
	return out
}

// Snapshot each login once per tracker response so adoption and the indicator
// cannot disagree when a relay connects during response construction.
func cachedStageTrackerPresence(lookup func(string) (automerge.ContributorPresence, bool)) func(string) (automerge.ContributorPresence, bool) {
	if lookup == nil {
		return nil
	}
	type result struct {
		p  automerge.ContributorPresence
		ok bool
	}
	cache := map[string]result{}
	return func(login string) (automerge.ContributorPresence, bool) {
		if r, ok := cache[login]; ok {
			return r.p, r.ok
		}
		p, ok := lookup(login)
		cache[login] = result{p, ok}
		return p, ok
	}
}

func stageTrackerPresenceHeld(labels []string) bool {
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "hold" || label == "do-not-merge" || label == "do-not-merge/hold" {
			return true
		}
	}
	return false
}
