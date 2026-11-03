package automerge

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport/slogfeed"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#14466: the projectbluefin hive parked utah-packages#660,
// chairlift#638 and testsuite#1028 at 085 on hive-signoff-attribution after
// its own fix commit. The park is designed; two things around it were not.
// Its notes said the `Sign-off survives the squash` gate stays red, but that
// check is a workflow only some repositories carry and none of those three
// ran it. And the escalation was logged at WARN, so the slog feed counted
// three designed parks toward an unclassified_error_repeated self-report.

const escalationLogMsg = "stage labels: adopted PR sits with nothing the hive can push"

// squashUnsafeAdoption adopts PR 7 in place and gives head 021bb6f the shape
// of all three PRs: the contributor's own signed commit, then the hive's fix
// signed off as the App. The caller observes it once to trigger the stall.
// Since v5's signoff re-home (hivecommons/hive#13980) such a head is
// superseded at once unless something blocks it, so the observation carries
// a `hold` label: the stall note is then the one the hive still writes.
func squashUnsafeAdoption(t *testing.T) (*adoptHarness, func()) {
	t.Helper()
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	h.api.commitDetails = map[int][]stageCommitFixture{7: {
		{message: "fix(smoke): restore default app checks\n\nSigned-off-by: thatguywob <thatguywob@users.noreply.github.com>", authorLogin: "thatguywob", authorName: "thatguywob", authorEmail: "thatguywob@users.noreply.github.com"},
		{message: "fix(ci): preload apps for MIME checks\n\nSigned-off-by: hive[bot] <12345+hive[bot]@users.noreply.github.com>", authorLogin: testHiveAppBotLogin, authorName: "hive[bot]", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
	}}
	adoptInPlaceAt(h, "021bb6f", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("021bb6f", testHiveAppBotLogin)
	h.clock.advance(time.Minute)
	return h, done
}

func TestSignoffStallNoteNamesSquashGateOnlyWhereItRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate bool
	}{
		{"repository without dco-squash-attribution.yml", false},
		{"repository running it", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := squashUnsafeAdoption(t)
			defer done()
			if tc.gate {
				h.e.noteStageDCOGate("acme", "widget", 7, "021bb6f", map[string]bool{squashSignoffCheck: true}, nil)
			}
			h.observe(adoptPR(7, "021bb6f", append([]string{"hold"}, adoptedLabels...)...), contributorLaneReasonNoVerdict)
			notes := stalledComments(h.api)
			if len(notes) != 1 {
				t.Fatalf("stalled note: %v", h.api.comments)
			}
			for _, want := range []string{"so the hive will not squash-merge this head", "`gh pr merge 7 --merge`, not squash"} {
				if !strings.Contains(notes[0], want) {
					t.Fatalf("note missing %q:\n%s", want, notes[0])
				}
			}
			if named := strings.Contains(notes[0], squashSignoffCheck); named != tc.gate {
				t.Fatalf("note names the %q check = %v, want %v:\n%s", squashSignoffCheck, named, tc.gate, notes[0])
			}
			if strings.Contains(notes[0], "gate stays red") {
				t.Fatalf("note still uses the old wording:\n%s", notes[0])
			}
		})
	}
}

func TestSignoffAttributionDecisionNoteNamesSquashGateOnlyWhereItRuns(t *testing.T) {
	for _, gate := range []bool{false, true} {
		api, rec, _, _ := runSignoffCase(t, signoffCase{commits: testsuite991Commits, reason: contributorLaneReasonNoVerdict, squashGate: gate})
		if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
			t.Fatalf("gate=%v: record %+v, want 085 on hive-signoff-attribution", gate, rec)
		}
		notes := commentsWith(api, SignoffAttributionDecisionMarkerPrefix)
		if len(notes) != 1 {
			t.Fatalf("gate=%v: maintainer note: %q", gate, api.comments)
		}
		if !strings.Contains(notes[0], "the hive will not squash-merge this head") || strings.Contains(notes[0], "gate stays red") {
			t.Fatalf("gate=%v: note wording:\n%s", gate, notes[0])
		}
		if named := strings.Contains(notes[0], squashSignoffCheck); named != gate {
			t.Fatalf("note names the %q check = %v, want %v:\n%s", squashSignoffCheck, named, gate, notes[0])
		}
	}
}

// The check seen on an earlier head says nothing about this one.
func TestSquashSignoffGateNoteIsPerHead(t *testing.T) {
	obs := stageObservation{HeadSHA: "new"}
	obs.Detail.SquashSignoffGate, obs.Detail.DCOGateHead = true, "old"
	if got := squashSignoffGateNote(obs); got != "" {
		t.Fatalf("gate seen on another head: got %q, want none", got)
	}
	obs.Detail.DCOGateHead = "new"
	if got := squashSignoffGateNote(obs); !strings.Contains(got, squashSignoffCheck) {
		t.Fatalf("gate seen on this head: got %q", got)
	}
}

// The escalation is logged at INFO, so the slog feed (WARN and above) never
// counts it toward a self-report. The feed here is the real handler.
func TestAdoptionStallEscalationLogsAtInfo(t *testing.T) {
	h, done := squashUnsafeAdoption(t)
	defer done()
	var buf bytes.Buffer
	var mu sync.Mutex
	var fed []string
	h.e.logger = slog.New(slogfeed.NewWithObserver(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), func(_, _, message, _ string) {
		mu.Lock()
		defer mu.Unlock()
		fed = append(fed, message)
	}))

	h.observe(adoptPR(7, "021bb6f", append([]string{"hold"}, adoptedLabels...)...), contributorLaneReasonNoVerdict)
	if len(stalledComments(h.api)) != 1 {
		t.Fatalf("expected the escalation to happen: %v", h.api.comments)
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, escalationLogMsg) {
			line = l
			break
		}
	}
	if !strings.Contains(line, "level=INFO") {
		t.Fatalf("escalation log line %q, want level=INFO", line)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range fed {
		if strings.Contains(m, escalationLogMsg) {
			t.Fatalf("the slog feed counted the designed escalation toward a self-report: %q", m)
		}
	}
}
