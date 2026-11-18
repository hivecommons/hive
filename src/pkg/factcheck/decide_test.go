package factcheck

import (
	"strings"
	"testing"
	"time"
)

var (
	t0       = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	headOne  = "1111111111111111111111111111111111111111"
	headTwo  = "2222222222222222222222222222222222222222"
	basePol  = Policy{MaxRounds: 3, ReadyPolicy: ReadyHuman, RevisionWait: 6 * time.Hour}
	hivePol  = Policy{MaxRounds: 3, ReadyPolicy: ReadyHive, RevisionWait: 6 * time.Hour}
	openDraf = PR{Repo: "o/r", Number: 7, Open: true, Draft: true, HeadSHA: headOne}
)

func mk(round int, v Verdict, head string) Marker {
	return Marker{Round: round, Verdict: v, Head: head}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name      string
		in        Input
		action    Action
		round     int
		reasonHas string
	}{
		{
			name:   "fresh PR gets round 1",
			in:     Input{Policy: basePol, PR: openDraf, Now: t0},
			action: ActionReview, round: 1,
		},
		{
			name:   "closed PR is left alone",
			in:     Input{Policy: basePol, PR: PR{Open: false, HeadSHA: headOne}, Now: t0},
			action: ActionNone,
		},
		{
			name:   "a person reviewing stops the loop before any round",
			in:     Input{Policy: basePol, PR: openDraf, HumanTookOver: true, Now: t0},
			action: ActionNone, reasonHas: "person",
		},
		{
			name:   "a person reviewing stops a pending revision too",
			in:     Input{Policy: basePol, PR: openDraf, Markers: []Marker{mk(1, VerdictFindings, headOne)}, HumanTookOver: true, Now: t0},
			action: ActionNone,
		},
		{
			name:   "hand-off already posted: nothing more, ever",
			in:     Input{Policy: basePol, PR: openDraf, NeedsHumanPosted: true, Now: t0},
			action: ActionNone,
		},
		{
			name:   "hand-off remembered in state",
			in:     Input{Policy: basePol, PR: openDraf, State: State{NeedsHumanNoted: true}, Now: t0},
			action: ActionNone,
		},
		{
			name: "review in flight is not re-kicked",
			in: Input{Policy: basePol, PR: openDraf, Now: t0.Add(10 * time.Minute),
				State: State{ReviewKicks: 1, LastReviewKick: t0, LastReviewHead: headOne}},
			action: ActionNone, reasonHas: "in progress",
		},
		{
			name: "silent reviewer: the round is spent and the next one starts",
			in: Input{Policy: basePol, PR: openDraf, Now: t0.Add(ReviewWait + time.Minute),
				State: State{ReviewKicks: 1, LastReviewKick: t0, LastReviewHead: headOne}},
			action: ActionReview, round: 2,
		},
		{
			name: "silent reviewer cannot extend the loop past the limit",
			in: Input{Policy: basePol, PR: openDraf, Now: t0.Add(ReviewWait + time.Minute),
				State: State{ReviewKicks: 3, LastReviewKick: t0, LastReviewHead: headOne}},
			action: ActionNeedsHuman, reasonHas: "round limit",
		},
		{
			name:   "pass on current head, human decides readiness",
			in:     Input{Policy: basePol, PR: openDraf, Markers: []Marker{mk(1, VerdictPass, headOne)}, Now: t0},
			action: ActionNone, reasonHas: "passed",
		},
		{
			name:   "pass on current head, hive policy marks the draft ready",
			in:     Input{Policy: hivePol, PR: openDraf, Markers: []Marker{mk(1, VerdictPass, headOne)}, Now: t0},
			action: ActionMarkReady, round: 1,
		},
		{
			name:   "pass accepts an abbreviated head in the marker",
			in:     Input{Policy: hivePol, PR: openDraf, Markers: []Marker{mk(1, VerdictPass, headOne[:9])}, Now: t0},
			action: ActionMarkReady, round: 1,
		},
		{
			name: "pass but already marked ready: no repeat",
			in: Input{Policy: hivePol, PR: openDraf, Markers: []Marker{mk(1, VerdictPass, headOne)},
				State: State{ReadyMarked: true}, Now: t0},
			action: ActionNone,
		},
		{
			name:   "pass on a PR that is not a draft: nothing to mark",
			in:     Input{Policy: hivePol, PR: PR{Open: true, Draft: false, HeadSHA: headOne}, Markers: []Marker{mk(1, VerdictPass, headOne)}, Now: t0},
			action: ActionNone,
		},
		{
			name: "pass on an older head does not unlock ready; the new head is reviewed",
			in: Input{Policy: hivePol, PR: PR{Open: true, Draft: true, HeadSHA: headTwo}, Markers: []Marker{mk(1, VerdictPass, headOne)},
				State: State{ReviewKicks: 1}, Now: t0},
			action: ActionReview, round: 2,
		},
		{
			name:   "findings on current head go to the author once",
			in:     Input{Policy: basePol, PR: openDraf, Markers: []Marker{mk(1, VerdictFindings, headOne)}, Now: t0},
			action: ActionRevise, round: 1,
		},
		{
			name: "findings already sent, author has time",
			in: Input{Policy: basePol, PR: openDraf, Markers: []Marker{mk(1, VerdictFindings, headOne)},
				State: State{RevisionHead: headOne, RevisionAt: t0}, Now: t0.Add(time.Hour)},
			action: ActionNone, reasonHas: "waiting",
		},
		{
			name: "findings sent, author never pushed: a person",
			in: Input{Policy: basePol, PR: openDraf, Markers: []Marker{mk(1, VerdictFindings, headOne)},
				State: State{RevisionHead: headOne, RevisionAt: t0}, Now: t0.Add(7 * time.Hour)},
			action: ActionNeedsHuman, reasonHas: "did not push",
		},
		{
			name: "author pushed a revision: the new head gets the next round",
			in: Input{Policy: basePol, PR: PR{Open: true, Draft: true, HeadSHA: headTwo},
				Markers: []Marker{mk(1, VerdictFindings, headOne)},
				State:   State{ReviewKicks: 1, RevisionHead: headOne, RevisionAt: t0, LastReviewHead: headOne, LastReviewKick: t0}, Now: t0.Add(time.Hour)},
			action: ActionReview, round: 2,
		},
		{
			name: "round limit: findings on the last round, author revised, no round left",
			in: Input{Policy: basePol, PR: PR{Open: true, Draft: true, HeadSHA: headTwo},
				Markers: []Marker{mk(3, VerdictFindings, headOne)},
				State:   State{ReviewKicks: 3, RevisionHead: headOne, RevisionAt: t0}, Now: t0.Add(time.Hour)},
			action: ActionNeedsHuman, reasonHas: "round limit",
		},
		{
			name: "a reviewer that writes a huge round number only ends the loop sooner",
			in: Input{Policy: basePol, PR: PR{Open: true, Draft: true, HeadSHA: headTwo},
				Markers: []Marker{mk(99, VerdictFindings, headOne)}, Now: t0},
			action: ActionNeedsHuman, reasonHas: "round limit",
		},
		{
			name:   "inconclusive goes to a person, not into another round",
			in:     Input{Policy: basePol, PR: openDraf, Markers: []Marker{mk(1, VerdictInconclusive, headOne)}, Now: t0},
			action: ActionNeedsHuman, reasonHas: "could not verify",
		},
		{
			name: "latest round wins among markers",
			in: Input{Policy: hivePol, PR: PR{Open: true, Draft: true, HeadSHA: headTwo},
				Markers: []Marker{mk(1, VerdictFindings, headOne), mk(2, VerdictPass, headTwo)}, Now: t0},
			action: ActionMarkReady, round: 2,
		},
		{
			name:   "policy without limits still bounds the loop at one round",
			in:     Input{Policy: Policy{}, PR: openDraf, State: State{ReviewKicks: 1, LastReviewHead: headTwo}, Now: t0},
			action: ActionNeedsHuman,
		},
		{
			name: "policy without revision wait uses the default",
			in: Input{Policy: Policy{MaxRounds: 3}, PR: openDraf, Markers: []Marker{mk(1, VerdictFindings, headOne)},
				State: State{RevisionHead: headOne, RevisionAt: t0}, Now: t0.Add(DefaultRevisionWait + time.Minute)},
			action: ActionNeedsHuman,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.in)
			if got.Action != tt.action {
				t.Fatalf("action = %s (%s), want %s", got.Action, got.Reason, tt.action)
			}
			if tt.round != 0 && got.Round != tt.round {
				t.Fatalf("round = %d, want %d", got.Round, tt.round)
			}
			if tt.reasonHas != "" && !strings.Contains(got.Reason, tt.reasonHas) {
				t.Fatalf("reason %q should mention %q", got.Reason, tt.reasonHas)
			}
			if got.Reason == "" {
				t.Fatal("every decision carries a reason")
			}
		})
	}
}

// The loop must end for every behaviour of a reviewer that never produces a
// passing marker: simulate the sweep against a worst case and count kicks.
func TestDecideLoopAlwaysTerminates(t *testing.T) {
	pol := Policy{MaxRounds: 3, ReadyPolicy: ReadyHive, RevisionWait: time.Hour}
	for _, behaviour := range []string{"silent", "always-findings", "always-inconclusive"} {
		t.Run(behaviour, func(t *testing.T) {
			now := t0
			head := headOne
			st := State{}
			var markers []Marker
			reviews, revisions, ended := 0, 0, false
			for i := 0; i < 200 && !ended; i++ {
				now = now.Add(2 * time.Hour)
				d := Decide(Input{Policy: pol, PR: PR{Open: true, Draft: true, HeadSHA: head}, Markers: markers, State: st, Now: now})
				switch d.Action {
				case ActionReview:
					reviews++
					st.ReviewKicks = d.Round
					st.LastReviewKick, st.LastReviewHead = now, head
					switch behaviour {
					case "always-findings":
						markers = append(markers, mk(d.Round, VerdictFindings, head))
					case "always-inconclusive":
						markers = append(markers, mk(d.Round, VerdictInconclusive, head))
					}
				case ActionRevise:
					revisions++
					st.RevisionHead, st.RevisionAt = head, now
					head = strings.Repeat(string(rune('a'+revisions)), 40) // the author pushes
				case ActionNeedsHuman:
					ended = true
				case ActionMarkReady:
					t.Fatal("a pass was never produced")
				}
			}
			if !ended {
				t.Fatal("the loop did not end")
			}
			if reviews > pol.MaxRounds {
				t.Fatalf("%d reviewer kicks, limit %d", reviews, pol.MaxRounds)
			}
		})
	}
}

func TestHumanTookOver(t *testing.T) {
	tests := []struct {
		name string
		ps   []Participant
		want bool
	}{
		{"nobody", nil, false},
		{"only bots", []Participant{{Login: "dependabot", Human: false, Substantive: true}}, false},
		{"a person's review", []Participant{{Login: "maintainer", Human: true, Substantive: true}}, true},
		{"a person's bare comment is not a review", []Participant{{Login: "maintainer", Human: true, Substantive: false}}, false},
		{"anonymous person ignored", []Participant{{Login: " ", Human: true, Substantive: true}}, false},
		{"bot then person", []Participant{{Login: "bot", Human: false, Substantive: true}, {Login: "p", Human: true, Substantive: true}}, true},
	}
	for _, tt := range tests {
		if got := HumanTookOver(tt.ps); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}
