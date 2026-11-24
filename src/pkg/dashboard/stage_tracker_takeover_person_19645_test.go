package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#19645: a person's PR waiting on its author to push
// showed no takeover actions at all. The operator can now Adopt now (an
// explicit, audited click; the engine still never adopts a person's PR on
// its own), Reroute stays off because there is no relay to reroute from.

func personWaitingItem() StageTrackerItem {
	return StageTrackerItem{Kind: "pr", Number: 19626, Author: "octo", AuthorKind: authorKindHuman, Stage: trackerNumberNeedsAuthor, Head: "abc123"}
}

func TestStageTrackerPersonTakeoverOffersAdoptNowNotReroute(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	clock := stageTrackerAdoptionClock{Enabled: true}
	candidatesAsked := false
	candidates := func(string) []RerouteCandidate {
		candidatesAsked = true
		return []RerouteCandidate{{Login: "bee2", Capable: true}}
	}
	presence := func(string) (automerge.ContributorPresence, bool) { return automerge.ContributorPresence{Online: true}, true }
	tk := stageTrackerTakeover(personWaitingItem(), stagelabels.Record{}, false, presence, candidates, clock, now)
	if tk == nil || !tk.Person || !tk.AdoptNow || tk.AdoptNowReason != "" || tk.Author != "octo" {
		t.Fatalf("takeover = %+v", tk)
	}
	if tk.Reroute || tk.RerouteReason != takeoverRerouteDisabledPerson || len(tk.Clankers) != 0 || candidatesAsked {
		t.Fatalf("reroute must be off on a person's PR: %+v (candidates asked: %v)", tk, candidatesAsked)
	}
	if tk.GraceElapsed || tk.AutoAdoptAt != nil {
		t.Fatalf("no auto-adoption clock on a person's PR: %+v", tk)
	}
	raw, _ := json.Marshal(tk)
	for _, want := range []string{`"person":true`, `"adopt_now":true`, `"reroute":false`, `"reroute_reason":"` + takeoverRerouteDisabledPerson + `"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("payload lacks %s: %s", want, raw)
		}
	}

	// The popover adds Adopt now only; no Reroute option.
	opts := takeoverDecisionOptions(tk, false)
	if len(opts) != 1 || opts[0].Action != decisionActionAdoptNow || !opts[0].Enabled {
		t.Fatalf("options = %+v", opts)
	}

	// Offering Adopt now does not hand the wait to the operator: the row
	// still waits on its author.
	it := personWaitingItem()
	it.Takeover = tk
	if stageTrackerContributorGraceElapsed(it) {
		t.Fatalf("a person's PR offering Adopt now still waits on its author")
	}
}

func TestStageTrackerPersonTakeoverDisabledReasons(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	on := stageTrackerAdoptionClock{Enabled: true}

	held := personWaitingItem()
	held.Modifiers = []string{modHold}
	if tk := stageTrackerTakeover(held, stagelabels.Record{}, false, nil, nil, on, now); tk == nil || tk.AdoptNow || tk.AdoptNowReason != takeoverAdoptNowDisabledHold {
		t.Fatalf("held: %+v", tk)
	}

	if tk := stageTrackerTakeover(personWaitingItem(), stagelabels.Record{}, false, nil, nil, stageTrackerAdoptionClock{}, now); tk == nil || tk.AdoptNow || tk.AdoptNowReason != takeoverAdoptNowDisabledOff {
		t.Fatalf("adoption off: %+v", tk)
	}

	forcedAt := now.Add(-time.Minute)
	rec := stagelabels.Record{AdoptForcedBy: "andan02", AdoptForcedAt: forcedAt}
	tk := stageTrackerTakeover(personWaitingItem(), rec, true, nil, nil, on, now)
	if tk == nil || tk.AdoptNow || tk.AdoptNowReason != takeoverAdoptNowQueued || tk.ForcedBy != "andan02" || tk.ForcedAt == nil || !tk.ForcedAt.Equal(forcedAt) {
		t.Fatalf("queued: %+v", tk)
	}
	if opts := takeoverDecisionOptions(tk, false); len(opts) != 0 {
		t.Fatalf("queued Adopt now is spent and there is no Reroute: %+v", opts)
	}

	// Other stages and already-adopted heads stay without a takeover.
	other := personWaitingItem()
	other.Stage = 50
	if tk := stageTrackerTakeover(other, stagelabels.Record{}, false, nil, nil, on, now); tk != nil {
		t.Fatalf("stage 50: %+v", tk)
	}
	adopted := stagelabels.Record{AdoptedHead: "abc123"}
	if tk := stageTrackerTakeover(personWaitingItem(), adopted, true, nil, nil, on, now); tk != nil {
		t.Fatalf("adopted head: %+v", tk)
	}
}

// The row's inline lead never becomes Adopt now on a person's PR; it is in
// the ⋯ menu only. A relay's PR still leads with it.
func TestStageTrackerFallbackLeadSkipsAdoptNowForPerson(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, jsFunc(t, html, "stageTrackerFallbackLead")+`
const assert = require('assert');
const adoptNow = { id: 'decide:adopt-now', enabled: true };
const actions = [{ id: 'hold', enabled: true }, adoptNow];
assert.strictEqual(stageTrackerFallbackLead({ kind: 'pr', author_kind: 'human' }, actions), null, 'no Adopt now lead on a person PR');
assert.strictEqual(stageTrackerFallbackLead({ kind: 'pr', takeover: { person: true } }, actions), null, 'takeover.person counts too (linked sub-rows)');
assert.strictEqual(stageTrackerFallbackLead({ kind: 'pr', author_kind: 'clanker' }, actions), adoptNow, 'a relay PR still leads with Adopt now');
assert.strictEqual(stageTrackerFallbackLead({ kind: 'pr', author_kind: 'clanker' }, [{ id: 'decide:adopt-now', enabled: false }]), null, 'a disabled Adopt now never leads');
const delegateAll = { id: 'decide:delegate-all', enabled: false };
assert.strictEqual(stageTrackerFallbackLead({ kind: 'pr', author_kind: 'human' }, [adoptNow, delegateAll]), delegateAll, 'the next fallback still leads');
const adopt = { id: 'decide:adopt', enabled: true };
assert.strictEqual(stageTrackerFallbackLead({ kind: 'pr', author_kind: 'human' }, [adoptNow, adopt]), adopt);
assert.strictEqual(stageTrackerFallbackLead(null, null), null);
`)
}
