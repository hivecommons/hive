package github

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
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

// The hive's bot writes stage/045-needs-decision on any issue that carries a
// hard-suppress label, whoever applied it. A park that rests on a label a
// person applied is not the hive's: the note is asked for as on any other
// issue and, without the opt-in label, refused.

const parkSpecLabel = "needs-spec"

// parkLabelsJSON is the issue's label list as the fake serves it.
func parkLabelsJSON(names ...string) []any {
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"name": name})
	}
	return out
}

// parkReferring is a merged pull request in repo whose body refers to the
// parked issue, as the issue's timeline cross-references it.
func parkReferring(repo string, n int, after time.Duration) map[string]any {
	merged := parkT0.Add(after)
	return prCrossRef(repo, n, true, "closed", merged, merged, "Refs org/repo#"+strconv.Itoa(parkRelayIssue))
}

// parkUndecidedIssue is alice's issue #650 parked at parkT0 by one label
// event from the hive's bot; its park text names no other item.
func parkUndecidedIssue() *parkFake {
	f := prRelayParkedIssue("hive[bot]")
	f.comments[0]["body"] = "Maintainer decision needed: which of the two layouts to keep."
	f.timeline = f.timeline[:1]
	f.lookups = map[int]map[string]any{}
	return f
}

// wantNoParkWrite fails when the re-check wrote anything to the issue.
func wantNoParkWrite(t *testing.T, f *parkFake, res *ParkRecheckResult) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if res.Posted != 0 || res.Updated != 0 || len(f.posted)+len(f.edited) != 0 || f.labelOps != 0 {
		t.Fatalf("%+v posted=%q edited=%q labelOps=%d; want nothing written", res, f.posted, f.edited, f.labelOps)
	}
}

func TestParkRecheckParkRestingOnAPersonsLabelIsAskedAsBefore(t *testing.T) {
	// A maintainer adds needs-spec; the stage engine then writes 045 with
	// the hive's bot; a pull request that refers to the issue merges.
	needsSpec := parkUndecidedIssue()
	needsSpec.issue["labels"] = parkLabelsJSON(parkSpecLabel, "stage/045-needs-decision")
	needsSpec.timeline = []map[string]any{
		prLabelEventBy("labeled", parkSpecLabel, parkT0.Add(-time.Minute), "alice"),
		prLabelEventBy("labeled", "stage/045-needs-decision", parkT0, "hive[bot]"),
		parkReferring("org/repo", 702, 3*time.Hour),
	}

	// The hive parks the issue; five minutes later a person takes the label
	// off and puts it back. The park is still dated from the hive's event.
	readded := prRelayParkedIssue("hive[bot]")
	readded.timeline = []map[string]any{
		prLabelEventBy("labeled", "stage/045-needs-decision", parkT0, "hive[bot]"),
		prLabelEventBy("unlabeled", "stage/045-needs-decision", parkT0.Add(5*time.Minute), "alice"),
		prLabelEventBy("labeled", "stage/045-needs-decision", parkT0.Add(6*time.Minute), "alice"),
		readded.timeline[1],
	}

	// needs-human is on the issue and the timeline has no event for it.
	unexplained := prRelayParkedIssue("hive[bot]")
	unexplained.issue["labels"] = parkLabelsJSON("stage/045-needs-decision", "needs-human")

	// A person adds needs-human an hour after the hive's park.
	second := prRelayParkedIssue("hive[bot]")
	second.issue["labels"] = parkLabelsJSON("stage/045-needs-decision", "needs-human")
	second.timeline = append(second.timeline, prLabelEventBy("labeled", "needs-human", parkT0.Add(time.Hour), "alice"))

	for name, f := range map[string]*parkFake{
		"a person's needs-spec, the hive's 045":               needsSpec,
		"a person re-added the label inside the gap":          readded,
		"a hard-suppress label with no event in the timeline": unexplained,
		"a person's needs-human beside the hive's 045":        second,
	} {
		t.Run(name, func(t *testing.T) {
			gate := parkGate(&parkGateKnobs{level: 6}, writeback.Config{})
			c, logs := parkGatedClient(t, f, gate)
			wantNoParkWrite(t, f, parkPass(t, c))
			held := gate.Recent()
			if len(held) != 1 || held[0].Kind != writeback.KindNeedsDecisionComment || held[0].Reason != writeback.ReasonNeedsLabel {
				t.Fatalf("suppressed = %+v, want the one refusal for want of the opt-in label", held)
			}
			if parkHeldLines(logs, "INFO") != 1 || !strings.Contains(logs.String(), "hive_applied_park=false") {
				t.Fatalf("want one info line saying the park is not the hive's:\n%s", logs.String())
			}
		})
	}
}

// parkTimelineEvents decodes timeline events the way the client reads them.
func parkTimelineEvents(t *testing.T, events ...map[string]any) []*gh.Timeline {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	var out []*gh.Timeline
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestParkHiveApplied(t *testing.T) {
	const park = "stage/045-needs-decision"
	hive045 := prLabelEventBy("labeled", park, parkT0, "hive[bot]")
	plain := parkClient(t, newParkFake(t, &parkFake{}))
	renamed := parkClient(t, newParkFake(t, &parkFake{}))
	renamed.SetIssueFilter(config.IssueFilterConfig{HardSuppressLabels: config.HardSuppressLabelsConfig{NeedsSpec: []string{"awaiting-design"}}})
	for _, tc := range []struct {
		name     string
		client   *Client
		parkedBy string
		labels   []string
		events   []map[string]any
		want     bool
	}{
		{"the hive's 045 and nothing else", plain, "hive[bot]", []string{park}, []map[string]any{hive045}, true},
		{"labels that park nothing are not looked at", plain, "hive[bot]", []string{"bug", "help wanted", park}, []map[string]any{hive045}, true},
		{"label names compare without case", plain, "hive[bot]", []string{"Stage/045-Needs-Decision"}, []map[string]any{hive045}, true},
		{"the park is dated from a person's event", plain, "alice", []string{park}, []map[string]any{hive045}, false},
		{"a person's needs-spec beside the hive's 045", plain, "hive[bot]", []string{parkSpecLabel, park},
			[]map[string]any{prLabelEventBy("labeled", parkSpecLabel, parkT0.Add(-time.Minute), "alice"), hive045}, false},
		{"the hive's own needs-spec beside its 045", plain, "hive[bot]", []string{parkSpecLabel, park},
			[]map[string]any{prLabelEventBy("labeled", parkSpecLabel, parkT0.Add(-time.Minute), "hive[bot]"), hive045}, true},
		{"a person re-added the label", plain, "hive[bot]", []string{park},
			[]map[string]any{hive045, prLabelEventBy("unlabeled", park, parkT0.Add(5*time.Minute), "alice"), prLabelEventBy("labeled", park, parkT0.Add(6*time.Minute), "alice")}, false},
		{"the hive re-added a label a person once applied", plain, "hive[bot]", []string{park},
			[]map[string]any{prLabelEventBy("labeled", park, parkT0.Add(-48*time.Hour), "alice"), prLabelEventBy("unlabeled", park, parkT0.Add(-47*time.Hour), "alice"), hive045}, true},
		{"two events in the same second: the later one counts", plain, "hive[bot]", []string{park},
			[]map[string]any{hive045, prLabelEventBy("labeled", park, parkT0, "alice")}, false},
		{"a hard-suppress label with no labeled event", plain, "hive[bot]", []string{park, "needs-human"}, []map[string]any{hive045}, false},
		{"a labeled event that names no actor", plain, "hive[bot]", []string{park, "needs-human"},
			[]map[string]any{hive045, prLabelEventBy("labeled", "needs-human", parkT0.Add(time.Hour), "")}, false},
		{"no hard-suppress label on the issue", plain, "hive[bot]", []string{"bug"}, []map[string]any{hive045}, false},
		{"blocked does not start a decision park and is not looked at", plain, "hive[bot]", []string{"blocked", park},
			[]map[string]any{prLabelEventBy("labeled", "blocked", parkT0.Add(-time.Hour), "alice"), hive045}, true},
		{"an operator-configured hard-suppress label a person applied", renamed, "hive[bot]", []string{"awaiting-design", park},
			[]map[string]any{prLabelEventBy("labeled", "awaiting-design", parkT0.Add(-time.Minute), "alice"), hive045}, false},
		{"the same label without the operator's configuration", plain, "hive[bot]", []string{"awaiting-design", park},
			[]map[string]any{prLabelEventBy("labeled", "awaiting-design", parkT0.Add(-time.Minute), "alice"), hive045}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.client.parkHiveApplied(tc.parkedBy, tc.labels, parkTimelineEvents(t, tc.events...)); got != tc.want {
				t.Fatalf("parkHiveApplied = %v, want %v", got, tc.want)
			}
		})
	}
}

const (
	parkOutsideLine = "outsider/fork#9"
	parkLeftOutLine = "park re-check left out changes from outside the project"
)

// A note the gate allows only because the hive applied the park lists what
// the park text names and what the project's own repositories merged. When
// nothing of that kind moved it writes nothing, pass after pass, and asks
// the gate nothing.
func TestParkRecheckFactOnlyNoteListsOnlyProjectChanges(t *testing.T) {
	t.Run("only a pull request outside the project refers to the issue", func(t *testing.T) {
		f := parkUndecidedIssue()
		f.timeline = append(f.timeline, parkReferring("outsider/fork", 9, 3*time.Hour))
		gate := parkGate(&parkGateKnobs{level: 6}, writeback.Config{})
		c, logs := parkGatedClient(t, f, gate)
		wantNoParkWrite(t, f, parkPass(t, c))
		wantNoParkWrite(t, f, parkPass(t, c))
		if held := gate.Recent(); len(held) != 0 {
			t.Fatalf("nothing was to be written, yet the gate recorded a refusal: %+v", held)
		}
		if strings.Contains(logs.String(), parkHeldLine) {
			t.Fatalf("a pass with nothing to write logged a held note:\n%s", logs.String())
		}
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(strings.ToLower(line), "park re-check") && !strings.Contains(line, "level=DEBUG ") {
				t.Fatalf("a pass with nothing to write logged the re-check above debug: %s", line)
			}
		}
		if strings.Contains(logs.String(), "PARK-RECHECK") {
			t.Fatalf("a pass with nothing to write logged a posted note:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), parkLeftOutLine) {
			t.Fatalf("the debug log must say a change was left out:\n%s", logs.String())
		}
		wantParkLabelsUntouched(t, f)
	})

	t.Run("the park text names an item outside the project", func(t *testing.T) {
		f := parkUndecidedIssue()
		f.comments[0]["body"] = "Blocked: this waits on upstream/lib#12."
		merged := parkT0.Add(3 * time.Hour)
		f.timeline = append(f.timeline,
			prCrossRef("upstream/lib", 12, true, "closed", merged, merged, ""),
			parkReferring("outsider/fork", 9, 4*time.Hour))
		gate := parkGate(&parkGateKnobs{level: 6}, writeback.Config{})
		c, _ := parkGatedClient(t, f, gate)
		if res := parkPass(t, c); res.Posted != 1 || len(f.posted) != 1 {
			t.Fatalf("%+v posted=%q; want one note for the item the park names", res, f.posted)
		}
		wantNote(t, f.posted[0], "- upstream/lib#12 merged on 2026-10-04 (the park reason names it).")
		if strings.Contains(f.posted[0], parkOutsideLine) {
			t.Fatalf("the note lists a pull request from outside the project:\n%s", f.posted[0])
		}
		wantParkLabelsUntouched(t, f)
	})

	t.Run("pull requests in the issue's repository and in another project repository", func(t *testing.T) {
		f := parkUndecidedIssue()
		f.timeline = append(f.timeline,
			parkReferring("org/repo", 702, 3*time.Hour),
			parkReferring("Org/Tools", 5, 4*time.Hour),
			parkReferring("outsider/fork", 9, 5*time.Hour))
		gate := parkGate(&parkGateKnobs{level: 6}, writeback.Config{})
		c, _ := parkGatedClient(t, f, gate)
		c.SetRepos([]string{"repo", "tools"})
		if res := parkPass(t, c); res.Posted != 1 || len(f.posted) != 1 {
			t.Fatalf("%+v posted=%q; want one note", res, f.posted)
		}
		wantNote(t, f.posted[0],
			"- #702 merged on 2026-10-04 (it refers to this issue).",
			"- Org/Tools#5 merged on 2026-10-04 (it refers to this issue).")
		if strings.Contains(f.posted[0], parkOutsideLine) {
			t.Fatalf("the note lists a pull request from outside the project:\n%s", f.posted[0])
		}
		wantParkLabelsUntouched(t, f)
	})
}

// Where the note was allowed before the fact existed, it lists what it
// always listed: an issue with the opt-in label, whoever parked it, and a
// client with no gate installed.
func TestParkRecheckNoteOnOptedInIssueListsEveryChange(t *testing.T) {
	for _, parkedBy := range []string{"hive[bot]", "alice"} {
		t.Run("opt-in label, parked by "+parkedBy, func(t *testing.T) {
			f := parkUndecidedIssue()
			f.issue["labels"] = parkLabelsJSON("stage/045-needs-decision", "hive-review")
			f.timeline = []map[string]any{
				prLabelEventBy("labeled", "stage/045-needs-decision", parkT0, parkedBy),
				parkReferring("outsider/fork", 9, 3*time.Hour),
			}
			gate := parkGate(&parkGateKnobs{level: 6}, writeback.Config{})
			c, _ := parkGatedClient(t, f, gate)
			if res := parkPass(t, c); res.Posted != 1 || len(f.posted) != 1 || f.labelOps != 0 {
				t.Fatalf("%+v posted=%q; want the note posted as before", res, f.posted)
			}
			wantNote(t, f.posted[0], "- "+parkOutsideLine+" merged on 2026-10-04 (it refers to this issue).")
		})
	}
	t.Run("no gate installed", func(t *testing.T) {
		f := parkUndecidedIssue()
		f.timeline = append(f.timeline, parkReferring("outsider/fork", 9, 3*time.Hour))
		c := parkClient(t, newParkFake(t, f))
		if res := parkPass(t, c); res.Posted != 1 || len(f.posted) != 1 {
			t.Fatalf("%+v posted=%q; want the note posted as before", res, f.posted)
		}
		wantNote(t, f.posted[0], "- "+parkOutsideLine+" merged on 2026-10-04 (it refers to this issue).")
	})
}

func TestParkProjectChanges(t *testing.T) {
	c := parkClient(t, newParkFake(t, &parkFake{}))
	c.SetRepos([]string{"repo", "elsewhere/tools"})
	for repo, want := range map[string]bool{
		"org/repo":         true,
		"ORG/Repo":         true,
		"elsewhere/tools":  true,
		"org/tools":        false, // the bare name "repo" takes the org, "elsewhere/tools" keeps its owner
		"outsider/fork":    false,
		"outsider/repo":    false,
		"":                 false,
		"org/repo-archive": false,
	} {
		if got := c.parkProjectRepo(repo, "org/issues"); got != want {
			t.Errorf("parkProjectRepo(%q) = %v, want %v", repo, got, want)
		}
	}
	if !c.parkProjectRepo("Org/Issues", "org/issues") {
		t.Error("the issue's own repository is always the project's")
	}

	changes := []parkChange{
		{ref: "#701", repo: "org/repo", named: true},
		{ref: "upstream/lib#12", repo: "upstream/lib", named: true},
		{ref: "#702", repo: "org/repo"},
		{ref: "outsider/fork#9", repo: "outsider/fork"},
		// The hive's copy of a pull request: kept when the park names the
		// original or the copy is in a project repository.
		{ref: "upstream/lib#30", repo: "upstream/lib", copyOf: "upstream/lib#12", named: true},
		{ref: "#873", repo: "org/repo", copyOf: "#722"},
		{ref: "outsider/fork#11", repo: "outsider/fork", copyOf: "outsider/fork#9"},
		{ref: "#703"},
	}
	var got []string
	for _, ch := range c.parkProjectChanges(changes, "org/repo", parkRelayIssue) {
		got = append(got, ch.ref)
	}
	if want := "#701,upstream/lib#12,#702,upstream/lib#30,#873"; strings.Join(got, ",") != want {
		t.Fatalf("kept %v, want %s", got, want)
	}
}
