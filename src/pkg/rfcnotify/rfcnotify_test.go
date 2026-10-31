package rfcnotify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func rfc(n int, state string, labels ...string) Issue {
	return Issue{
		Repo: "hivecommons/hive", Number: n, Title: "RFC: thing " + strings.Repeat("x", n%3),
		URL: "https://github.com/hivecommons/hive/issues/1", Author: "alice",
		State: state, Labels: labels, CreatedAt: t0.Add(-time.Hour),
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func kinds(evs []Event) []Kind {
	out := make([]Kind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind)
	}
	return out
}

func TestIsRFC(t *testing.T) {
	yes := []string{
		"RFC: Contribution UI",
		"[RFC] Harden the Hub's login with PKCE",
		"📝 RFC: [v6] Proxy egress allowlist",
		"✨ RFC: Repo-targeted campaigns",
		"[v6] RFC: something",
		"rfc - lowercase works",
	}
	for _, title := range yes {
		if !IsRFC(title, nil) {
			t.Errorf("IsRFC(%q) = false, want true", title)
		}
	}
	no := []string{
		"",
		"[scanner] [v6] Your-turn critical path (RFC #13617 P1)",
		"[v6] ✨ settings: expose config (RFC 11585 follow-up)",
		"RFCs are great",
		"RFC #123 follow-up",
		"Fix the rfc parser",
	}
	for _, title := range no {
		if IsRFC(title, nil) {
			t.Errorf("IsRFC(%q) = true, want false", title)
		}
	}
	if !IsRFC("plain title", []string{"enhancement", " RFC "}) {
		t.Error("rfc label should mark an RFC regardless of title")
	}
	if IsRFC("plain title", []string{"enhancement"}) {
		t.Error("unrelated label must not mark an RFC")
	}
}

func TestHeldAndActionable(t *testing.T) {
	if !Held([]string{"hold"}) || !Held([]string{"do-not-merge/hold"}) || Held([]string{"enhancement"}) {
		t.Error("Held mismatch")
	}
	if !Actionable([]string{"stage/010-actionable"}) || Actionable([]string{"stage/020-claimed"}) {
		t.Error("Actionable mismatch")
	}
}

func TestFormat(t *testing.T) {
	is := Issue{Repo: "hivecommons/hive", Number: 12062, Title: " RFC: Evidence pipeline ", URL: "https://github.com/hivecommons/hive/issues/12062", Author: "clubanderson"}
	cases := []struct {
		ev          Event
		includeRepo bool
		want        string
	}{
		{Event{Kind: KindOpened, Issue: is}, false, "RFC opened: #12062 RFC: Evidence pipeline - <https://github.com/hivecommons/hive/issues/12062> (by @clubanderson)"},
		{Event{Kind: KindOpened, Issue: Issue{Number: 1, Title: "RFC: x", URL: "u"}}, false, "RFC opened: #1 RFC: x - <u>"},
		{Event{Kind: KindClosed, Issue: withReason(is, "completed")}, false, "RFC closed (completed): #12062 RFC: Evidence pipeline - <https://github.com/hivecommons/hive/issues/12062>"},
		{Event{Kind: KindClosed, Issue: withReason(is, "not_planned")}, true, "RFC closed (not planned): hivecommons/hive#12062 RFC: Evidence pipeline - <https://github.com/hivecommons/hive/issues/12062>"},
		{Event{Kind: KindClosed, Issue: withReason(is, "")}, false, "RFC closed (completed): #12062 RFC: Evidence pipeline - <https://github.com/hivecommons/hive/issues/12062>"},
		{Event{Kind: KindReady, Issue: is}, true, "RFC ready to implement (hold removed): hivecommons/hive#12062 RFC: Evidence pipeline - <https://github.com/hivecommons/hive/issues/12062>"},
		{Event{Kind: Kind("odd"), Issue: is}, false, "RFC odd: #12062 RFC: Evidence pipeline - <https://github.com/hivecommons/hive/issues/12062>"},
	}
	for _, c := range cases {
		if got := Format(c.ev, c.includeRepo); got != c.want {
			t.Errorf("Format(%s)\n got %q\nwant %q", c.ev.Kind, got, c.want)
		}
	}
}

func withReason(is Issue, reason string) Issue {
	is.State = "closed"
	is.StateReason = reason
	return is
}

func TestDetectorSeedThenOpened(t *testing.T) {
	d := NewDetector()
	if d.Seeded() {
		t.Fatal("new detector must not be seeded")
	}
	// Observe before seeding takes a baseline and announces nothing.
	if evs := d.Observe([]Issue{rfc(1, "open"), rfc(2, "open", "hold")}, t0); len(evs) != 0 {
		t.Fatalf("unseeded observe announced %v", kinds(evs))
	}
	if !d.Seeded() || !d.Known("hivecommons/hive#1") || !d.Known("hivecommons/hive#2") {
		t.Fatal("seed did not record the baseline")
	}
	// A pre-existing issue not in the seed (closed at seed time, now
	// updated) is recorded silently.
	old := rfc(3, "open")
	old.CreatedAt = t0.Add(-2 * time.Hour)
	// A new RFC created after the watermark is announced once.
	fresh := rfc(4, "open")
	fresh.CreatedAt = t0.Add(time.Minute)
	// A non-RFC issue is ignored entirely.
	plain := Issue{Repo: "hivecommons/hive", Number: 5, Title: "Fix a bug", State: "open", CreatedAt: t0.Add(time.Minute)}
	evs := d.Observe([]Issue{old, fresh, plain}, t0.Add(2*time.Minute))
	if got := kinds(evs); len(got) != 1 || got[0] != KindOpened || evs[0].Issue.Number != 4 {
		t.Fatalf("events = %v (%+v)", got, evs)
	}
	if d.Known("hivecommons/hive#5") {
		t.Error("non-RFC must not be recorded")
	}
	// Re-observing the same snapshot posts nothing.
	if evs := d.Observe([]Issue{old, fresh, plain}, t0.Add(3*time.Minute)); len(evs) != 0 {
		t.Fatalf("double post: %v", kinds(evs))
	}
}

func TestDetectorClosedBothReasons(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open"), rfc(2, "open")}, t0)
	done := withReason(rfc(1, "open"), "completed")
	dropped := withReason(rfc(2, "open"), "not_planned")
	evs := d.Observe([]Issue{dropped, done}, t0.Add(time.Minute))
	if got := kinds(evs); len(got) != 2 || got[0] != KindClosed || got[1] != KindClosed {
		t.Fatalf("events = %v", got)
	}
	if evs[0].Issue.Number != 1 || evs[1].Issue.Number != 2 {
		t.Fatalf("events not ordered by number: %+v", evs)
	}
	if ClosedReason(evs[0].Issue.StateReason) != "completed" || ClosedReason(evs[1].Issue.StateReason) != "not planned" {
		t.Fatalf("reasons = %q %q", evs[0].Issue.StateReason, evs[1].Issue.StateReason)
	}
	if evs := d.Observe([]Issue{dropped, done}, t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("closed announced twice: %v", kinds(evs))
	}
	// Reopen then close again announces the second close.
	if evs := d.Observe([]Issue{rfc(1, "open")}, t0.Add(3*time.Minute)); len(evs) != 0 {
		t.Fatalf("reopen announced %v", kinds(evs))
	}
	if evs := d.Observe([]Issue{done}, t0.Add(4*time.Minute)); len(evs) != 1 || evs[0].Kind != KindClosed {
		t.Fatalf("second close = %v", kinds(evs))
	}
}

func TestDetectorHoldRemoved(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open", "hold", "proposal")}, t0)
	// Unrelated label churn while held: nothing.
	if evs := d.Observe([]Issue{rfc(1, "open", "hold", "proposal", "help wanted")}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("label churn announced %v", kinds(evs))
	}
	evs := d.Observe([]Issue{rfc(1, "open", "proposal", "help wanted")}, t0.Add(2*time.Minute))
	if got := kinds(evs); len(got) != 1 || got[0] != KindReady {
		t.Fatalf("events = %v", got)
	}
	// Same snapshot again, and further unrelated label changes: nothing.
	if evs := d.Observe([]Issue{rfc(1, "open", "proposal")}, t0.Add(3*time.Minute)); len(evs) != 0 {
		t.Fatalf("ready announced twice: %v", kinds(evs))
	}
	// Re-held and lifted again is a new episode.
	if evs := d.Observe([]Issue{rfc(1, "open", "hold")}, t0.Add(4*time.Minute)); len(evs) != 0 {
		t.Fatalf("re-hold announced %v", kinds(evs))
	}
	if evs := d.Observe([]Issue{rfc(1, "open")}, t0.Add(5*time.Minute)); len(evs) != 1 || evs[0].Kind != KindReady {
		t.Fatalf("second lift = %v", kinds(evs))
	}
	// Hold removed together with the close is a close, not a ready.
	d.Seed([]Issue{rfc(2, "open", "hold")}, t0)
	if evs := d.Observe([]Issue{withReason(rfc(2, "open"), "completed")}, t0.Add(time.Minute)); len(evs) != 1 || evs[0].Kind != KindClosed {
		t.Fatalf("close while held = %v", kinds(evs))
	}
	// Adding hold to an open RFC that was not held announces nothing.
	d.Seed([]Issue{rfc(3, "open")}, t0)
	if evs := d.Observe([]Issue{rfc(3, "open", "hold")}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("hold added announced %v", kinds(evs))
	}
}

func TestDetectorHeldToActionableDedupes(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open", "hold")}, t0)
	evs := d.Observe([]Issue{rfc(1, "open", "hold", "stage/010-actionable")}, t0.Add(time.Minute))
	if got := kinds(evs); len(got) != 1 || got[0] != KindReady {
		t.Fatalf("held to actionable = %v", got)
	}
	// Hold label removed afterwards: already announced for this episode.
	if evs := d.Observe([]Issue{rfc(1, "open", "stage/010-actionable")}, t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("second ready after stage transition: %v", kinds(evs))
	}
	// Actionable already present when held, then lifted: one ready.
	d.Seed([]Issue{rfc(2, "open", "hold", "stage/010-actionable")}, t0)
	if evs := d.Observe([]Issue{rfc(2, "open", "hold", "stage/010-actionable", "x")}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("still held announced %v", kinds(evs))
	}
	if evs := d.Observe([]Issue{rfc(2, "open", "stage/010-actionable")}, t0.Add(2*time.Minute)); len(evs) != 1 || evs[0].Kind != KindReady {
		t.Fatalf("lift = %v", kinds(evs))
	}
}

func TestDetectorForgetsIssueThatStopsBeingRFC(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open")}, t0)
	plain := rfc(1, "open")
	plain.Title = "not one any more"
	d.Observe([]Issue{plain}, t0.Add(time.Minute))
	if d.Known("hivecommons/hive#1") {
		t.Fatal("record should be dropped")
	}
}

func TestDetectorPrunesOldClosed(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open")}, t0)
	d.Observe([]Issue{withReason(rfc(1, "open"), "completed")}, t0.Add(time.Minute))
	if !d.Known("hivecommons/hive#1") {
		t.Fatal("closed record kept inside retention")
	}
	d.Observe(nil, t0.Add(closedRetention+2*time.Minute))
	if d.Known("hivecommons/hive#1") {
		t.Fatal("closed record should be pruned after retention")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "rfc-notify.json")
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open", "hold")}, t0)
	if err := d.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Seeded() || !loaded.Watermark().Equal(t0) || !loaded.Known("hivecommons/hive#1") {
		t.Fatalf("round trip lost state: %+v", loaded.state)
	}
	// Restart then lift the hold: announced exactly once, from the
	// persisted record.
	if evs := loaded.Observe([]Issue{rfc(1, "open")}, t0.Add(time.Minute)); len(evs) != 1 || evs[0].Kind != KindReady {
		t.Fatalf("after restart = %v", kinds(evs))
	}

	missing, err := Load(filepath.Join(dir, "none.json"))
	if err != nil || missing.Seeded() {
		t.Fatalf("missing file: %v %v", err, missing.Seeded())
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "bad.json")); err == nil {
		t.Fatal("corrupt state must error")
	}
	if err := os.WriteFile(filepath.Join(dir, "null.json"), []byte(`{"seeded":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if nul, err := Load(filepath.Join(dir, "null.json")); err != nil || nul.state.Issues == nil {
		t.Fatalf("nil issues map not repaired: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "unreadable.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "unreadable.json")); err == nil {
		t.Fatal("directory as state must error")
	}
	if err := d.Save(filepath.Join(dir, "bad.json", "y.json")); err == nil {
		t.Fatal("save under a regular file must error")
	}
}

// fakeSource scripts the two listings.
type fakeSource struct {
	open    []Issue
	updated []Issue
	since   []time.Time
	openErr error
	updErr  error
}

func (f *fakeSource) ListOpen(context.Context) ([]Issue, error) { return f.open, f.openErr }
func (f *fakeSource) ListUpdatedSince(_ context.Context, since time.Time) ([]Issue, error) {
	f.since = append(f.since, since)
	return f.updated, f.updErr
}

func TestSweeperSeedsThenPostsAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rfc-notify.json")
	now := t0
	clock := func() time.Time { return now }
	var posted []string
	post := func(s string) error { posted = append(posted, s); return nil }
	src := &fakeSource{open: []Issue{rfc(1, "open", "hold"), rfc(2, "open")}}
	sw := NewSweeper(Options{StatePath: path, Now: clock, IncludeRepo: true, Logger: quiet})

	res, err := sw.Run(context.Background(), src, Poster(post))
	if err != nil || !res.Seeded || len(posted) != 0 {
		t.Fatalf("seed run: %+v %v posted=%v", res, err, posted)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
	// Within the interval: skipped, no listing.
	now = now.Add(time.Minute)
	if res, _ := sw.Run(context.Background(), src, Poster(post)); !res.Skipped {
		t.Fatal("expected skip inside the interval")
	}
	// Next poll: hold lifted on #1, #2 closed, new #3 opened.
	now = now.Add(DefaultInterval)
	fresh := rfc(3, "open")
	fresh.CreatedAt = t0.Add(time.Minute)
	src.updated = []Issue{rfc(1, "open"), withReason(rfc(2, "open"), "not_planned"), fresh}
	res, err = sw.Run(context.Background(), src, Poster(post))
	if err != nil || res.Errors != 0 || len(res.Posted) != 3 {
		t.Fatalf("poll run: %+v %v", res, err)
	}
	if len(src.since) != 1 || !src.since[0].Equal(t0.Add(-DefaultSlack)) {
		t.Fatalf("since = %v, want watermark minus slack", src.since)
	}
	wantPrefixes := []string{"RFC ready to implement (hold removed): hivecommons/hive#1 ", "RFC closed (not planned): hivecommons/hive#2 ", "RFC opened: hivecommons/hive#3 "}
	for i, p := range wantPrefixes {
		if !strings.HasPrefix(posted[i], p) {
			t.Errorf("posted[%d] = %q, want prefix %q", i, posted[i], p)
		}
	}
	// A new sweeper on the same state file (restart) re-reads the same
	// snapshot and posts nothing.
	now = now.Add(DefaultInterval)
	posted = nil
	sw2 := NewSweeper(Options{StatePath: path, Now: clock, Logger: quiet})
	res, err = sw2.Run(context.Background(), src, Poster(post))
	if err != nil || len(posted) != 0 || res.Seeded {
		t.Fatalf("restart re-posted: %+v %v %v", res, err, posted)
	}
}

func TestSweeperPostFailureRetriesNextPoll(t *testing.T) {
	now := t0
	clock := func() time.Time { return now }
	src := &fakeSource{open: []Issue{rfc(1, "open"), rfc(2, "open")}}
	sw := NewSweeper(Options{Now: clock, Logger: quiet})
	if _, err := sw.Run(context.Background(), src, Poster(func(string) error { return nil })); err != nil {
		t.Fatal(err)
	}
	now = now.Add(DefaultInterval)
	src.updated = []Issue{withReason(rfc(1, "open"), "completed"), withReason(rfc(2, "open"), "completed")}
	calls := 0
	failSecond := func(string) error {
		calls++
		if calls == 2 {
			return errors.New("discord down")
		}
		return nil
	}
	res, err := sw.Run(context.Background(), src, Poster(failSecond))
	if err != nil || res.Errors != 1 || len(res.Posted) != 1 {
		t.Fatalf("first run: %+v %v", res, err)
	}
	// The watermark rolled back so the next listing covers the same window.
	if !sw.Detector().Watermark().Equal(t0) {
		t.Fatalf("watermark = %v, want %v", sw.Detector().Watermark(), t0)
	}
	now = now.Add(DefaultInterval)
	var posted []string
	res, err = sw.Run(context.Background(), src, Poster(func(s string) error { posted = append(posted, s); return nil }))
	if err != nil || len(posted) != 1 || !strings.Contains(posted[0], "#2 ") {
		t.Fatalf("retry run: %+v %v posted=%v", res, err, posted)
	}
	// Third poll: nothing left.
	now = now.Add(DefaultInterval)
	if res, _ := sw.Run(context.Background(), src, Poster(func(string) error { t.Fatal("unexpected post"); return nil })); len(res.Posted) != 0 {
		t.Fatal("re-posted")
	}
}

func TestSweeperRollbackForgetsUnpostedNewIssue(t *testing.T) {
	now := t0
	sw := NewSweeper(Options{Now: func() time.Time { return now }, Logger: quiet})
	src := &fakeSource{}
	if _, err := sw.Run(context.Background(), src, Poster(func(string) error { return nil })); err != nil {
		t.Fatal(err)
	}
	now = now.Add(DefaultInterval)
	fresh := rfc(9, "open")
	fresh.CreatedAt = t0.Add(time.Second)
	src.updated = []Issue{fresh}
	if res, _ := sw.Run(context.Background(), src, Poster(func(string) error { return errors.New("no") })); res.Errors != 1 {
		t.Fatalf("expected one error: %+v", res)
	}
	if sw.Detector().Known(fresh.Key()) {
		t.Fatal("unposted new issue must be forgotten so it is announced next poll")
	}
	now = now.Add(DefaultInterval)
	if res, _ := sw.Run(context.Background(), src, Poster(func(string) error { return nil })); len(res.Posted) != 1 {
		t.Fatalf("retry: %+v", res)
	}
}

func TestSweeperErrors(t *testing.T) {
	sw := NewSweeper(Options{})
	if _, err := sw.Run(context.Background(), nil, nil); err == nil {
		t.Fatal("nil source must error")
	}
	src := &fakeSource{openErr: errors.New("boom")}
	if _, err := sw.Run(context.Background(), src, Poster(func(string) error { return nil })); err == nil {
		t.Fatal("seed error must surface")
	}
	src.openErr = nil
	now := t0
	sw = NewSweeper(Options{Now: func() time.Time { return now }, Logger: quiet})
	if _, err := sw.Run(context.Background(), src, Poster(func(string) error { return nil })); err != nil {
		t.Fatal(err)
	}
	now = now.Add(DefaultInterval)
	src.updErr = errors.New("rate limited")
	if _, err := sw.Run(context.Background(), src, Poster(func(string) error { return nil })); err == nil {
		t.Fatal("list error must surface")
	}
}

func TestNewSweeperUnreadableStateStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	sw := NewSweeper(Options{StatePath: path, Logger: quiet})
	if sw.Detector().Seeded() {
		t.Fatal("fresh detector expected")
	}
	// Save failure (state path is a directory) surfaces from Run.
	sw = NewSweeper(Options{StatePath: dir, Logger: quiet})
	if _, err := sw.Run(context.Background(), &fakeSource{}, Poster(func(string) error { return nil })); err == nil {
		t.Fatal("save into a directory path must error")
	}
}

// gateSweeper seeds a sweeper on #1 and #2, then scripts the next poll to
// close both, returning a clock advancer for further polls.
func gateSweeper(t *testing.T, opts Options, src *fakeSource) (*Sweeper, func()) {
	t.Helper()
	now := t0
	opts.Now = func() time.Time { return now }
	if opts.Logger == nil {
		opts.Logger = quiet
	}
	sw := NewSweeper(opts)
	src.open = []Issue{rfc(1, "open"), rfc(2, "open")}
	if _, err := sw.Run(context.Background(), src, Poster(func(string) error { t.Fatal("seed posted"); return nil })); err != nil {
		t.Fatal(err)
	}
	return sw, func() { now = now.Add(DefaultInterval) }
}

func TestOptionsRich(t *testing.T) {
	if (Options{}).Rich() || (Options{Format: FormatLine, Announcer: true}).Rich() {
		t.Fatal("line mode must not be rich")
	}
	if !(Options{Format: FormatCard}).Rich() || !(Options{Threads: true}).Rich() {
		t.Fatal("card or threads must be rich")
	}
}

func TestSweeperRichModeWithoutAnnouncerPostsNothingOnce(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for _, opts := range []Options{{Format: FormatCard, Logger: logger}, {Threads: true, Logger: logger}} {
		logs.Reset()
		src := &fakeSource{}
		sw, tick := gateSweeper(t, opts, src)
		tick()
		src.updated = []Issue{withReason(rfc(1, "open"), "completed")}
		res, err := sw.Run(context.Background(), src, Poster(func(string) error { t.Fatal("non-announcer posted"); return nil }))
		if err != nil || len(res.Posted) != 0 || res.Withheld != 1 {
			t.Fatalf("first run: %+v %v", res, err)
		}
		tick()
		src.updated = []Issue{withReason(rfc(2, "open"), "completed")}
		if res, _ := sw.Run(context.Background(), src, Poster(func(string) error { t.Fatal("non-announcer posted"); return nil })); res.Withheld != 1 {
			t.Fatalf("second run: %+v", res)
		}
		if n := strings.Count(logs.String(), "rich mode requires rfc_announcer; not posting"); n != 1 {
			t.Fatalf("warning logged %d times, want 1:\n%s", n, logs.String())
		}
		// The state advanced: becoming the announcer later does not backfill.
		sw.opts.Announcer = true
		tick()
		src.updated = []Issue{withReason(rfc(1, "open"), "completed"), withReason(rfc(2, "open"), "completed")}
		if res, _ := sw.Run(context.Background(), src, Poster(func(string) error { t.Fatal("backfilled"); return nil })); len(res.Posted) != 0 {
			t.Fatalf("backfill: %+v", res)
		}
	}
}

func TestSweeperRichModeAnnouncerPosts(t *testing.T) {
	src := &fakeSource{}
	sw, tick := gateSweeper(t, Options{Format: FormatCard, Threads: true, Announcer: true}, src)
	tick()
	src.updated = []Issue{withReason(rfc(1, "open"), "completed")}
	var posted []string
	res, err := sw.Run(context.Background(), src, Poster(func(s string) error { posted = append(posted, s); return nil }))
	if err != nil || len(posted) != 1 || res.Withheld != 0 {
		t.Fatalf("announcer: %+v %v %v", res, err, posted)
	}
}

func TestSweeperLineModeIgnoresAnnouncer(t *testing.T) {
	for _, announcer := range []bool{false, true} {
		src := &fakeSource{}
		sw, tick := gateSweeper(t, Options{Announcer: announcer}, src)
		tick()
		src.updated = []Issue{withReason(rfc(1, "open"), "completed")}
		var posted []string
		if _, err := sw.Run(context.Background(), src, Poster(func(s string) error { posted = append(posted, s); return nil })); err != nil {
			t.Fatal(err)
		}
		want := "RFC closed (completed): #1 " + rfc(1, "open").Title + " - <https://github.com/hivecommons/hive/issues/1>"
		if len(posted) != 1 || posted[0] != want {
			t.Fatalf("announcer=%v posted %q, want %q", announcer, posted, want)
		}
	}
}

func TestSweeperPrivateGate(t *testing.T) {
	private := func(is Issue) Issue { is.Private = true; return is }
	src := &fakeSource{}
	sw, tick := gateSweeper(t, Options{}, src)
	tick()
	src.updated = []Issue{private(withReason(rfc(1, "open"), "completed")), withReason(rfc(2, "open"), "completed")}
	var posted []string
	post := func(s string) error { posted = append(posted, s); return nil }
	res, err := sw.Run(context.Background(), src, Poster(post))
	if err != nil || res.Withheld != 1 || len(posted) != 1 || !strings.Contains(posted[0], "#2 ") {
		t.Fatalf("private run: %+v %v %v", res, err, posted)
	}
	// The private record advanced, so authorizing later does not replay it.
	sw.opts.IncludePrivate = true
	tick()
	posted = nil
	if res, _ := sw.Run(context.Background(), src, Poster(post)); len(posted) != 0 || res.Withheld != 0 {
		t.Fatalf("replayed: %+v %v", res, posted)
	}

	src2 := &fakeSource{}
	sw2, tick2 := gateSweeper(t, Options{IncludePrivate: true}, src2)
	tick2()
	src2.updated = []Issue{private(withReason(rfc(1, "open"), "completed"))}
	posted = nil
	if res, _ := sw2.Run(context.Background(), src2, Poster(post)); len(posted) != 1 || res.Withheld != 0 {
		t.Fatalf("include_private: %+v %v", res, posted)
	}
}

func TestSweeperUnknownVisibilityRetriesNextPoll(t *testing.T) {
	unknown := func(is Issue) Issue { is.Private, is.VisibilityUnknown = true, true; return is }
	src := &fakeSource{}
	sw, tick := gateSweeper(t, Options{}, src)
	tick()
	src.updated = []Issue{unknown(withReason(rfc(1, "open"), "completed")), withReason(rfc(2, "open"), "completed")}
	var posted []string
	post := func(s string) error { posted = append(posted, s); return nil }
	res, err := sw.Run(context.Background(), src, Poster(post))
	if err != nil || res.Unresolved != 1 || res.Withheld != 0 || len(posted) != 1 || !strings.Contains(posted[0], "#2 ") {
		t.Fatalf("unknown run: %+v %v %v", res, err, posted)
	}
	// Visibility now resolves public: the dropped transition is announced.
	src.updated = []Issue{withReason(rfc(1, "open"), "completed")}
	tick()
	posted = nil
	if res, _ := sw.Run(context.Background(), src, Poster(post)); len(posted) != 1 || res.Unresolved != 0 {
		t.Fatalf("retry: %+v %v", res, posted)
	}
}
