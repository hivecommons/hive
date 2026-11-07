package main

import (
	"slices"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// adoptedFixCIRouteLedger remembers, per adopted PR, the red episode the
// fixer was last routed for (or the stage-085 park it was last held at), so
// writeMergeEligible announces each one once instead of on every eligibility
// pass. An unchanged head with the same failing checks is the same episode:
// on the projectbluefin spoke two adopted printer-app PRs whose FSDK builds
// were red on a fsdk-containers bug were "routed to fixer" 61 times each at
// one head. A new head, a different set of failing checks, or a park that is
// released starts a new episode and is announced again.
//
// It is in memory: after a restart each open episode is announced once more.
type adoptedFixCIRouteLedger struct {
	mu   sync.Mutex
	last map[string]string
}

// adoptedFixCIRoutes is the process-wide ledger; tests swap it.
var adoptedFixCIRoutes = &adoptedFixCIRouteLedger{}

// first records episode for repo#number and reports whether it differs from
// the one recorded before (true the first time a PR is seen).
func (l *adoptedFixCIRouteLedger) first(repo string, number int, episode string) bool {
	key := stagelabels.Key(repo, number)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]string{}
	}
	if l.last[key] == episode {
		return false
	}
	l.last[key] = episode
	return true
}

// retain forgets every PR whose key is not in open, so a closed or merged
// PR's episode does not linger.
func (l *adoptedFixCIRouteLedger) retain(open map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key := range l.last {
		if !open[key] {
			delete(l.last, key)
		}
	}
}

// fixCIEpisode names a red episode: the head plus its failing-check set,
// order-insensitive.
func fixCIEpisode(head string, failing []string) string {
	checks := make([]string, 0, len(failing))
	for _, c := range failing {
		if c = strings.TrimSpace(c); c != "" {
			checks = append(checks, c)
		}
	}
	slices.Sort(checks)
	checks = slices.Compact(checks)
	return "fix-ci\x00" + head + "\x00" + strings.Join(checks, "\x00")
}

// parkedEpisode names a stage-085 park of the adoption at head for reason.
func parkedEpisode(head, reason string) string {
	return "parked\x00" + head + "\x00" + reason
}

// adoptedFixCIParked reports the reason the hive parked an adopted PR's
// current head at stage 085 (stagelabels.Record.AdoptionEscalated): the
// 074 dwell ran out with nothing the hive could push, and a person was told
// so. Until the head moves or the escalation is released (the blocker
// cleared, a recovery attempt, an operator's intent) the PR is not fixer
// work, so it is not routed and not listed in ci-failing.json.
func adoptedFixCIParked(repo string, number int, head string) (string, bool) {
	rec, ok := stagelabels.Shared().Get(stagelabels.Key(repo, number))
	if !ok || !rec.AdoptionEscalated(head) {
		return "", false
	}
	return rec.AdoptEscalatedReason, true
}
