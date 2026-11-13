package github

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/writeback"
)

// A person opened an issue. A contributor relay's agent commented that it
// was blocked on another issue and on a pull request awaiting approval, and
// the relay parked the issue with the hub-minted token, so the park label's
// actor is the hive's App bot. The pull request merged a few hours later.
// Every re-check pass after that logged "posted 0" and nothing else: the
// write-back gate refused the note because the issue carried no review
// opt-in label, and said so only at debug level. These tests run the
// re-check through a real gate.

const parkHeldLine = "park re-check note held by writeback gate"

// parkGateSeen is when the gate first saw org/repo: before the parked issue
// was created, so the backfill guard is not what decides these cases.
var parkGateSeen = parkT0.Add(-60 * 24 * time.Hour)

// parkGateKnobs are the operator settings a test moves between passes.
type parkGateKnobs struct {
	level  int
	paused bool
}

func parkGate(k *parkGateKnobs, cfg writeback.Config) *writeback.Gate {
	return writeback.New(cfg, writeback.Deps{
		LevelFor:  func(string) int { return k.level },
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) { return writeback.RepoPauseInfo{By: "owner"}, k.paused },
		Now:       func() time.Time { return parkGateSeen },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, "")
}

// prLabelEventBy is prLabelEvent with the account that applied the label;
// an empty login leaves the actor out, as GitHub does for a deleted account.
func prLabelEventBy(event, name string, at time.Time, login string) map[string]any {
	ev := map[string]any{"event": event, "label": map[string]any{"name": name}, "created_at": prTS(at)}
	if login != "" {
		ev["actor"] = map[string]any{"login": login}
	}
	return ev
}

const (
	parkRelayIssue = 650
	// parkRelayMerged is the note line for the pull request the park names,
	// merged three and a half hours after the park began.
	parkRelayMerged = "- #701 merged on 2026-10-04 (the park reason names it)."
	parkRelayClosed = "- #700 was closed on 2026-10-04 (the park reason names it)."
)

// prRelayParkedIssue is alice's issue #650, without the opt-in label, parked
// at parkT0 by one label event from parkedBy. The relay's comment names what
// it waits on: issue #700, still open, and pull request #701, since merged.
func prRelayParkedIssue(parkedBy string) *parkFake {
	f := prParkedIssue(parkRelayIssue, "Blocked: this waits on #700, which is still open, and on pull request #701, which awaits approval.")
	f.issue["user"] = map[string]any{"login": "alice"}
	merged := parkT0.Add(210 * time.Minute)
	f.timeline = []map[string]any{
		prLabelEventBy("labeled", "stage/045-needs-decision", parkT0, parkedBy),
		prCrossRef("org/repo", 701, true, "closed", merged, merged, ""),
	}
	f.lookups[700] = map[string]any{"number": 700, "state": "open"}
	return f
}

// closeParkBlocker closes issue #700, the other item the park names.
func closeParkBlocker(f *parkFake) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups[700] = map[string]any{"number": 700, "state": "closed", "closed_at": prTS(parkT0.Add(5 * time.Hour))}
}

// parkGatedClient is the re-check client for f behind gate, with its log
// captured at debug level.
func parkGatedClient(t *testing.T, f *parkFake, gate *writeback.Gate) (*Client, *bytes.Buffer) {
	t.Helper()
	c := parkClient(t, newParkFake(t, f))
	c.SetWritebackGate(gate)
	var logs bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return c, &logs
}

func parkPass(t *testing.T, c *Client) *ParkRecheckResult {
	t.Helper()
	res, err := c.RecheckParkedIssues(context.Background(), ParkRecheckConfig{Repos: []string{"org/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// parkHeldLines counts the held-note log lines at level ("INFO", "DEBUG").
func parkHeldLines(logs *bytes.Buffer, level string) int {
	n := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, parkHeldLine) && strings.Contains(line, "level="+level+" ") {
			n++
		}
	}
	return n
}

// wantParkLabelsUntouched fails when the re-check wrote a label or the
// issue's label set is not the single park label it started with.
func wantParkLabelsUntouched(t *testing.T, f *parkFake) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	labels, _ := f.issue["labels"].([]any)
	if f.labelOps != 0 || len(labels) != 1 {
		t.Fatalf("the re-check must not touch labels: %d label requests, labels %v", f.labelOps, labels)
	}
	if name, _ := labels[0].(map[string]any)["name"].(string); name != "stage/045-needs-decision" {
		t.Fatalf("park label changed to %q: the note never un-parks", name)
	}
}

// The note is posted on a park the hive applied, at L5 and at L6, without
// the opt-in label; an unchanged pass writes nothing, and a later change
// edits that one note in place.
func TestParkRecheckNotesHiveAppliedParkWithoutOptInLabel(t *testing.T) {
	for _, level := range []int{5, 6} {
		t.Run("L"+strconv.Itoa(level), func(t *testing.T) {
			f := prRelayParkedIssue("hive[bot]")
			gate := parkGate(&parkGateKnobs{level: level}, writeback.Config{})
			c, logs := parkGatedClient(t, f, gate)

			res := parkPass(t, c)
			if res.Posted != 1 || res.Updated != 0 || len(f.posted) != 1 || len(f.edited) != 0 {
				t.Fatalf("first pass: %+v posted=%q edited=%q; want exactly one note on a park the hive applied", res, f.posted, f.edited)
			}
			wantNote(t, f.posted[0], parkRelayMerged, "does not un-park")
			if strings.Contains(f.posted[0], "#700") {
				t.Fatalf("an item that is still open is listed:\n%s", f.posted[0])
			}

			if res := parkPass(t, c); res.Posted != 0 || res.Updated != 0 || len(f.posted) != 1 || len(f.edited) != 0 {
				t.Fatalf("unchanged pass wrote: %+v posted=%d edited=%d", res, len(f.posted), len(f.edited))
			}

			closeParkBlocker(f)
			if res := parkPass(t, c); res.Posted != 0 || res.Updated != 1 || len(f.posted) != 1 || len(f.edited) != 1 {
				t.Fatalf("after another blocker closed: %+v posted=%d edited=%d; want the one note edited in place", res, len(f.posted), len(f.edited))
			}
			wantNote(t, f.edited[0], parkRelayMerged, parkRelayClosed)

			if held := gate.Recent(); len(held) != 0 {
				t.Fatalf("the gate refused a write: %+v", held)
			}
			if strings.Contains(logs.String(), parkHeldLine) {
				t.Fatalf("a note that was posted is logged as held:\n%s", logs.String())
			}
			wantParkLabelsUntouched(t, f)
		})
	}
}

// A park a person applied, one whose label event names no actor, and one a
// person began and the hive only re-labelled are asked as before: without
// the opt-in label the note is refused, and now the log says so.
func TestParkRecheckPersonAppliedParkStillNeedsOptInLabel(t *testing.T) {
	relabelled := func() *parkFake {
		f := prRelayParkedIssue("alice")
		merged := parkT0.Add(210 * time.Minute)
		// alice parked it; a second later the hive swapped the label name.
		f.timeline = []map[string]any{
			prLabelEventBy("labeled", "needs-decision", parkT0, "alice"),
			prLabelEventBy("labeled", "stage/045-needs-decision", parkT0.Add(time.Second), "hive[bot]"),
			prLabelEventBy("unlabeled", "needs-decision", parkT0.Add(2*time.Second), "hive[bot]"),
			prCrossRef("org/repo", 701, true, "closed", merged, merged, ""),
		}
		return f
	}
	cases := map[string]*parkFake{
		"a person applied the park":                prRelayParkedIssue("alice"),
		"the label event names no actor":           prRelayParkedIssue(""),
		"another bot applied the park":             prRelayParkedIssue("other-app[bot]"),
		"a person parked, the hive re-labelled it": relabelled(),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			gate := parkGate(&parkGateKnobs{level: 6}, writeback.Config{})
			c, logs := parkGatedClient(t, f, gate)
			res := parkPass(t, c)
			if res.Posted != 0 || res.Updated != 0 || len(f.posted)+len(f.edited) != 0 {
				t.Fatalf("%+v posted=%q edited=%q; want no note without the opt-in label", res, f.posted, f.edited)
			}
			held := gate.Recent()
			if len(held) != 1 || held[0].Kind != writeback.KindNeedsDecisionComment || held[0].Number != parkRelayIssue || held[0].Reason != writeback.ReasonNeedsLabel {
				t.Fatalf("suppressed = %+v, want one needs_decision_comment refusal for want of the opt-in label", held)
			}
			if parkHeldLines(logs, "INFO") != 1 || !strings.Contains(logs.String(), "reason="+writeback.ReasonNeedsLabel) || !strings.Contains(logs.String(), "hive_applied_park=false") {
				t.Fatalf("want one info line naming the reason and that the park is not the hive's:\n%s", logs.String())
			}
			wantParkLabelsUntouched(t, f)
		})
	}
}

// The fact is no way round the operator's settings: below L5, on a paused
// repo and with the repo's write-back off, the note on a hive-applied park
// is refused, whether it would be created or edited.
func TestParkRecheckHiveAppliedParkStillHeldByLevelPauseAndRepoMode(t *testing.T) {
	off := writeback.Config{PerRepo: map[string]writeback.RepoOverride{"org/repo": {Mode: writeback.RepoModeOff}}}
	cases := []struct {
		name   string
		knobs  parkGateKnobs
		cfg    writeback.Config
		reason string
	}{
		{"below L5", parkGateKnobs{level: 4}, writeback.Config{}, writeback.ReasonBelowLevel},
		{"level unset", parkGateKnobs{level: 0}, writeback.Config{}, writeback.ReasonLevelUnknown},
		{"paused repo", parkGateKnobs{level: 6, paused: true}, writeback.Config{}, writeback.ReasonRepoPaused},
		{"repo mode off", parkGateKnobs{level: 6}, off, writeback.ReasonRepoOff},
	}
	for _, tc := range cases {
		for _, existing := range []bool{false, true} {
			t.Run(tc.name+"/existing note "+strconv.FormatBool(existing), func(t *testing.T) {
				f := prRelayParkedIssue("hive[bot]")
				if existing {
					f.comments = append(f.comments, map[string]any{"id": int64(99), "body": parkRecheckMarker + "\nstale", "user": map[string]any{"login": "hive[bot]"}})
				}
				knobs := tc.knobs
				gate := parkGate(&knobs, tc.cfg)
				c, logs := parkGatedClient(t, f, gate)
				res := parkPass(t, c)
				if res.Posted != 0 || res.Updated != 0 || len(f.posted)+len(f.edited) != 0 {
					t.Fatalf("%+v posted=%q edited=%q; want the note refused", res, f.posted, f.edited)
				}
				held := gate.Recent()
				if len(held) != 1 || held[0].Reason != tc.reason {
					t.Fatalf("suppressed = %+v, want one refusal for %s", held, tc.reason)
				}
				if parkHeldLines(logs, "INFO") != 1 || !strings.Contains(logs.String(), "reason="+tc.reason) || !strings.Contains(logs.String(), "hive_applied_park=true") {
					t.Fatalf("want one info line naming %s:\n%s", tc.reason, logs.String())
				}
				wantParkLabelsUntouched(t, f)
			})
		}
	}
}

// A refusal is logged at info once per issue and reason, at debug on the
// passes that repeat it, at info again when the reason changes, and is
// forgotten once the note is posted.
func TestParkRecheckHeldNoteLoggedOncePerReason(t *testing.T) {
	f := prRelayParkedIssue("hive[bot]")
	knobs := &parkGateKnobs{level: 4}
	gate := parkGate(knobs, writeback.Config{})
	c, logs := parkGatedClient(t, f, gate)

	parkPass(t, c)
	parkPass(t, c)
	if info, debug := parkHeldLines(logs, "INFO"), parkHeldLines(logs, "DEBUG"); info != 1 || debug != 1 {
		t.Fatalf("two passes held for one reason: %d info and %d debug lines, want 1 and 1:\n%s", info, debug, logs.String())
	}
	if !strings.Contains(logs.String(), "issue="+strconv.Itoa(parkRelayIssue)) || !strings.Contains(logs.String(), "reason="+writeback.ReasonBelowLevel) || !strings.Contains(logs.String(), "detail=") {
		t.Fatalf("the line must name the issue, the reason and the detail:\n%s", logs.String())
	}

	knobs.level, knobs.paused = 6, true
	parkPass(t, c)
	parkPass(t, c)
	if info, debug := parkHeldLines(logs, "INFO"), parkHeldLines(logs, "DEBUG"); info != 2 || debug != 2 {
		t.Fatalf("after the reason changed: %d info and %d debug lines, want 2 and 2:\n%s", info, debug, logs.String())
	}
	if !strings.Contains(logs.String(), "reason="+writeback.ReasonRepoPaused) {
		t.Fatalf("the new reason is not logged:\n%s", logs.String())
	}

	knobs.paused = false
	if res := parkPass(t, c); res.Posted != 1 || len(f.posted) != 1 {
		t.Fatalf("once the hold lifts the note is posted: %+v posted=%d", res, len(f.posted))
	}
	if info := parkHeldLines(logs, "INFO"); info != 2 {
		t.Fatalf("a posted note logged another hold (%d info lines):\n%s", info, logs.String())
	}
	if !gate.NoteHeld("org/repo", parkRelayIssue, writeback.KindNeedsDecisionComment, writeback.ReasonRepoPaused) {
		t.Fatal("the hold must be forgotten once the note is posted, so a later one is logged again")
	}
	wantParkLabelsUntouched(t, f)
}

// The remembered holds follow the parked issues: an issue whose park label
// came off, and one the listing no longer returns, are forgotten; a repo
// that was not read this pass keeps its holds.
func TestParkRecheckHeldMemoryFollowsParkedIssues(t *testing.T) {
	const kind = writeback.KindNeedsDecisionComment
	f := prRelayParkedIssue("hive[bot]")
	gate := parkGate(&parkGateKnobs{level: 4}, writeback.Config{})
	c, _ := parkGatedClient(t, f, gate)

	parkPass(t, c)
	if gate.NoteHeld("org/repo", parkRelayIssue, kind, writeback.ReasonBelowLevel) {
		t.Fatal("the refused pass must have remembered the hold")
	}

	// The timeline now shows the park label removed: nothing is parked.
	f.mu.Lock()
	f.timeline = append(f.timeline, prLabelEventBy("unlabeled", "stage/045-needs-decision", parkT0.Add(6*time.Hour), "alice"))
	f.mu.Unlock()
	parkPass(t, c)
	if !gate.NoteHeld("org/repo", parkRelayIssue, kind, writeback.ReasonBelowLevel) {
		t.Fatal("an issue that left the park must be forgotten")
	}

	// #650 is remembered again by the NoteHeld above; #651 and other/repo#7
	// stand for issues that were held and then closed or never read.
	gate.NoteHeld("org/repo", 651, kind, writeback.ReasonBelowLevel)
	gate.NoteHeld("other/repo", 7, kind, writeback.ReasonBelowLevel)
	c.retainParkRecheckHeld(map[string]bool{"org/repo": true}, map[string]bool{parkRecheckHeldKey("org", "repo", parkRelayIssue): true})
	if gate.NoteHeld("org/repo", parkRelayIssue, kind, writeback.ReasonBelowLevel) {
		t.Fatal("an issue that is still parked must keep its hold")
	}
	if !gate.NoteHeld("org/repo", 651, kind, writeback.ReasonBelowLevel) {
		t.Fatal("an issue the listing no longer returns must be forgotten")
	}
	if gate.NoteHeld("other/repo", 7, kind, writeback.ReasonBelowLevel) {
		t.Fatal("a repo that was not read this pass must keep its holds")
	}
}

func TestParkAppliedByHive(t *testing.T) {
	c := parkClient(t, newParkFake(t, &parkFake{issue: map[string]any{"number": 1}}))
	c.SetAdditionalBotLogins([]string{"second-hive[bot]"})
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-author"})
	for login, want := range map[string]bool{
		"hive[bot]":        true,
		"HIVE[bot]":        true,
		"hive":             true, // the App bot without the REST suffix
		"second-hive[bot]": true,
		"hive-author":      true,
		"alice":            false,
		"other-app[bot]":   false,
		"":                 false,
	} {
		if got := c.parkAppliedByHive(login); got != want {
			t.Errorf("parkAppliedByHive(%q) = %v, want %v", login, got, want)
		}
	}
	// With no identity configured nothing is the hive's.
	bare := newTestClient(t, newParkFake(t, &parkFake{issue: map[string]any{"number": 1}}), "org", []string{"repo"})
	if bare.parkAppliedByHive("hive[bot]") {
		t.Fatal("a client with no hive identity must not attribute a park to the hive")
	}
}
