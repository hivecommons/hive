package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The trigger matrix behind automatic adoption (hivecommons/hive#15565,
// #15566, #15609, #15312: PRs by an offline clanker sat at 072 —
// conflicting, changes requested, an optional check red, a fork hive
// cannot update — until the operator clicked Adopt now). Every 072 reason
// the nudge fires for × the author's presence × whether a new head landed,
// asserting adopt vs wait through the real sweep (ApplyStageLabels), with
// the default 15m offline wait.
func TestAutoAdoptionTriggerMatrix(t *testing.T) {
	// optionalRed is the lane snapshot for an optional check red on the
	// head: the engine derives optional-check-failing:<name> from the check
	// digest, not from the recorded lane reason.
	const optionalRed = "optional-check-failing:overlayfs-exec-guard"
	reasons := []string{
		"conflicting",
		contributorLaneReasonChangesRequested,
		optionalRed,
		"required-check-failing:changelog-fragment-guard",
		autoMergeReasonForkNotEditable,
	}
	type presenceCase struct {
		name string
		// presence is the author's live presence read after the nudge;
		// lastSeenAfterNudge is the heartbeat offset (0 = none).
		online             bool
		lastSeenAfterNudge time.Duration
		// adoptAt16m / adoptAt31m: adopted when the sweep runs 16m / 31m
		// after the nudge with the same head.
		adoptAt16m, adoptAt31m bool
	}
	presences := []presenceCase{
		// Online clanker: the 2h online grace holds (it may be mid-fix).
		{name: "online", online: true},
		// Offline since the nudge: adopted once the 15m wait is out.
		{name: "offline", adoptAt16m: true, adoptAt31m: true},
		// Acknowledged — a heartbeat (reconnect, queued task completing) 5m
		// after the nudge — buys one window, not a sliding one: not yet at
		// 16m, adopted by 31m.
		{name: "acknowledged", lastSeenAfterNudge: 5 * time.Minute, adoptAt31m: true},
	}
	for _, reason := range reasons {
		for _, pc := range presences {
			for _, headMoved := range []bool{false, true} {
				name := reason + "/" + pc.name
				if headMoved {
					name += "/head-moved"
				}
				t.Run(name, func(t *testing.T) {
					policy := adoptPolicy()
					h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, policy)
					defer done()
					start := h.clock.at
					h.sink.presence = &ContributorPresence{Registered: true, Online: pc.online}
					laneReason := reason
					if reason == optionalRed {
						laneReason = "queued"
						h.e.noteStageRedChecks("acme", "widget", 7, "aaa", []string{"overlayfs-exec-guard"})
					}
					h.observe(adoptPR(7, "aaa"), laneReason)
					if len(nudgeComments(h.api)) != 1 {
						t.Fatalf("no nudge for %q: %v", reason, h.api.comments)
					}
					labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
					p := ContributorPresence{Registered: true, Online: pc.online}
					if pc.lastSeenAfterNudge > 0 {
						p.LastSeen = start.Add(pc.lastSeenAfterNudge)
					}
					h.sink.presence = &p
					head := "aaa"
					if headMoved {
						// The clanker pushed: the hive never adopts a PR
						// whose author is pushing; the clock restarts.
						head = "bbb"
					}
					for _, step := range []struct {
						at    time.Duration
						adopt bool
					}{{16 * time.Minute, pc.adoptAt16m}, {31 * time.Minute, pc.adoptAt31m}} {
						h.api.reset()
						h.clock.at = start.Add(step.at)
						if reason == optionalRed {
							// The lane re-reports the red optional check on
							// every sweep; the note is consumed by each pass.
							h.e.noteStageRedChecks("acme", "widget", 7, head, []string{"overlayfs-exec-guard"})
						}
						h.observe(adoptPR(7, head, labels...), laneReason)
						want := step.adopt
						if headMoved {
							// The new head is nudged afresh on the 16m sweep;
							// 15m later it is adopted unless the clanker is
							// online.
							want = step.at > 16*time.Minute && !pc.online
						}
						got := len(h.adopt.pulls) > 0 || len(adoptionComments(h.api)) > 0
						if got != want {
							t.Fatalf("at +%v adopted=%v want %v (pulls=%d comments=%v)", step.at, got, want, len(h.adopt.pulls), h.api.comments)
						}
						if got {
							if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "reason="+AdoptReasonOffline) {
								t.Fatalf("adoption note: %v", c)
							}
							break
						}
					}
				})
			}
		}
	}
}

// Regression for the three example rows (hive's spoke, 2026-10-29): each
// nudged once at its head, author registered and offline 13m, with a
// heartbeat (acknowledgement) just after the nudge. Under the old clock
// the heartbeat restarted the 15m wait and the operator had to click; the
// one predicate says adoption is due 15m after the nudge-capped heartbeat.
func TestAutoAdoptionExampleRows(t *testing.T) {
	now := time.Date(2026, 10, 29, 22, 48, 0, 0, time.UTC)
	nudged := now.Add(-13 * time.Minute)
	clock := StageLabelPolicy{}.AdoptionClock()
	for _, tc := range []struct {
		pr   int
		head string
	}{{15565, "1f5a879f"}, {15566, "7336cd99"}, {15609, "4cd8b23c"}} {
		rec := stagelabels.Record{NudgeHead: tc.head, NudgeCount: 1, FirstNudgeAt: nudged, LastNudgeAt: nudged}
		// The clanker pinged once right after the nudge, then went quiet.
		p := ContributorPresence{Registered: true, LastSeen: nudged.Add(time.Minute)}
		v := AutoAdoption(rec, tc.head, clock, now, p, true)
		if !v.Running || v.Due || v.Reason != AdoptReasonOffline || !v.At.Equal(nudged.Add(16*time.Minute)) {
			t.Fatalf("#%d at 13m: %+v", tc.pr, v)
		}
		if v = AutoAdoption(rec, tc.head, clock, now.Add(3*time.Minute), p, true); !v.Due {
			t.Fatalf("#%d not due at 16m: %+v", tc.pr, v)
		}
		// A heartbeat long after the nudge cannot push the wait past one
		// window: still due at nudge+30m.
		late := ContributorPresence{Registered: true, LastSeen: nudged.Add(29 * time.Minute)}
		if v = AutoAdoption(rec, tc.head, clock, nudged.Add(30*time.Minute), late, true); !v.Due {
			t.Fatalf("#%d: late heartbeat extended the wait: %+v", tc.pr, v)
		}
		// Head moved: nothing until the new head is nudged.
		if v = AutoAdoption(rec, "newhead", clock, now.Add(time.Hour), p, true); v.Running || v.Due {
			t.Fatalf("#%d: moved head judged: %+v", tc.pr, v)
		}
	}
	// "queued" is a registered relay's queue, not its presence: a nudge
	// answered with mode queued by an offline relay records NudgeOnline
	// false, so without a presence source the offline grace applies.
	store := stagelabels.NewStore("", func() time.Time { return now })
	store.Observe("hivecommons/hive#15566", stagelabels.ReturnedToContributor, "conflicting")
	store.MarkNudged("hivecommons/hive#15566", "7336cd99", 1, false, nudged)
	rec, _ := store.Get("hivecommons/hive#15566")
	if v := AutoAdoption(rec, "7336cd99", clock, nudged.Add(15*time.Minute), ContributorPresence{}, false); !v.Due || v.Reason != AdoptReasonGrace {
		t.Fatalf("no-presence offline grace: %+v", v)
	}
	// Trigger names on the adoption log line.
	if got := adoptionTrigger(AdoptReasonOffline, "conflicting"); got != "automatic:conflicting" {
		t.Fatalf("trigger %q", got)
	}
	if got := adoptionTrigger(AdoptReasonOperatorForced, "conflicting"); got != AdoptTriggerForced {
		t.Fatalf("forced trigger %q", got)
	}
}
