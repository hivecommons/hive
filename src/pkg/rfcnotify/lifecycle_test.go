package rfcnotify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rfcBody is an RFC body with a field table and a decisions list of open
// unchecked and done checked items.
func rfcBody(target, status string, open, done int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "| Field | Value |\n|---|---|\n| Target | %s |\n| Status | %s |\n\n## Summary\n\nA summary.\n\n## Decisions for maintainers\n\n", target, status)
	for i := 0; i < open; i++ {
		fmt.Fprintf(&b, "- [ ] open %d\n", i)
	}
	for i := 0; i < done; i++ {
		fmt.Fprintf(&b, "- [x] done %d\n", i)
	}
	return b.String()
}

func withBody(is Issue, body string) Issue { is.Body = body; return is }

// lifecycleDetector is a rich-mode detector seeded with issues at t0.
func lifecycleDetector(seed ...Issue) *Detector {
	d := NewDetector()
	d.lifecycle = true
	d.Seed(seed, t0)
	return d
}

func TestLifecycle_DecisionsDebouncedToOnePer30MinWithLatestCount(t *testing.T) {
	d := lifecycleDetector(withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0)))

	evs := d.Observe([]Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 1, 1))}, t0.Add(time.Minute))
	if len(evs) != 1 || evs[0].Kind != KindDecisions || evs[0].From != "2" || evs[0].To != "1" {
		t.Fatalf("first change = %+v", evs)
	}
	if got := Format(evs[0], false); !strings.HasPrefix(got, "RFC decisions: 2 → 1 open: #1 ") {
		t.Fatalf("line = %q", got)
	}
	if evs[0].Thread != "hivecommons/hive#1" {
		t.Fatalf("thread = %q", evs[0].Thread)
	}
	// A second change inside the window is held back.
	if evs := d.Observe([]Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 0, 2))}, t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("inside the window = %v", kinds(evs))
	}
	if evs := d.Observe(nil, t0.Add(20*time.Minute)); len(evs) != 0 {
		t.Fatalf("still inside the window = %v", kinds(evs))
	}
	// Once the window passes it goes out with the latest count, even though
	// the issue is not in the poll.
	evs = d.Observe(nil, t0.Add(31*time.Minute))
	if len(evs) != 1 || evs[0].Kind != KindDecisions || evs[0].From != "1" || evs[0].To != "0" {
		t.Fatalf("after the window = %+v", evs)
	}
	if evs[0].Issue.Number != 1 || evs[0].Issue.Title != rfc(1, "open").Title || evs[0].Issue.URL == "" || evs[0].Thread != "hivecommons/hive#1" {
		t.Fatalf("flushed issue = %+v", evs[0])
	}
	if evs := d.Observe(nil, t0.Add(2*time.Hour)); len(evs) != 0 {
		t.Fatalf("repeated = %v", kinds(evs))
	}
}

func TestLifecycle_DecisionsChangeAndRevertInsideWindowPostsNothing(t *testing.T) {
	d := lifecycleDetector(withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0)))
	d.Observe([]Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 1, 1))}, t0.Add(time.Minute))
	d.Observe([]Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0))}, t0.Add(2*time.Minute))
	d.Observe([]Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 1, 1))}, t0.Add(3*time.Minute))
	if evs := d.Observe(nil, t0.Add(time.Hour)); len(evs) != 0 {
		t.Fatalf("back at the announced count = %v", kinds(evs))
	}
}

func TestLifecycle_TargetAndStatusChangePostOnce(t *testing.T) {
	d := lifecycleDetector(withBody(rfc(1, "open"), rfcBody("v5", "Draft", 0, 0)))
	after := withBody(rfc(1, "open"), rfcBody("v6", "Accepted", 0, 0))
	evs := d.Observe([]Issue{after}, t0.Add(time.Minute))
	if len(evs) != 2 || evs[0].Kind != KindTargetChanged || evs[1].Kind != KindStatusChanged {
		t.Fatalf("events = %+v", evs)
	}
	if got := Format(evs[0], false); !strings.HasPrefix(got, "RFC target: v5 → v6: #1 ") {
		t.Fatalf("target line = %q", got)
	}
	if got := Format(evs[1], false); !strings.HasPrefix(got, "RFC status: Draft → Accepted: #1 ") {
		t.Fatalf("status line = %q", got)
	}
	if evs := d.Observe([]Issue{after}, t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("same snapshot again = %v", kinds(evs))
	}
}

func TestLifecycle_UnparsableBodyKeepsTheKnownValues(t *testing.T) {
	d := lifecycleDetector(withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0)))
	if evs := d.Observe([]Issue{rfc(1, "open")}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("empty body = %v", kinds(evs))
	}
	rec := d.state.Issues["hivecommons/hive#1"]
	if rec.Target != "v5" || rec.Status != "Draft" || rec.OpenDecisions != 2 {
		t.Fatalf("record = %+v", rec)
	}
	// Removing a target is not announced either.
	if evs := d.Observe([]Issue{withBody(rfc(1, "open"), rfcBody("", "Draft", 2, 0))}, t0.Add(2*time.Minute)); len(evs) != 0 {
		t.Fatalf("target removed = %v", kinds(evs))
	}
}

func TestLifecycle_NeedsDecisionPostsOnceWhenAdded(t *testing.T) {
	for _, label := range []string{"stage/045-needs-decision", "needs-human"} {
		d := lifecycleDetector(rfc(1, "open"))
		labeled := rfc(1, "open", label)
		evs := d.Observe([]Issue{labeled}, t0.Add(time.Minute))
		if len(evs) != 1 || evs[0].Kind != KindNeedsDecision {
			t.Fatalf("%s: events = %v", label, kinds(evs))
		}
		if got := Format(evs[0], false); !strings.HasPrefix(got, "RFC needs a decision: #1 ") {
			t.Fatalf("line = %q", got)
		}
		if evs := d.Observe([]Issue{labeled}, t0.Add(2*time.Minute)); len(evs) != 0 {
			t.Fatalf("%s: still labeled = %v", label, kinds(evs))
		}
	}
	if NeedsDecision([]string{"needs-decision-later", "hold"}) {
		t.Fatal("unrelated labels must not count")
	}
}

func TestLifecycle_RetitleEvent(t *testing.T) {
	d := lifecycleDetector(rfc(1, "open"))
	renamed := rfc(1, "open")
	renamed.Title = "RFC: a better name"
	evs := d.Observe([]Issue{renamed}, t0.Add(time.Minute))
	if len(evs) != 1 || evs[0].Kind != KindRetitled || evs[0].From != rfc(1, "open").Title || evs[0].To != renamed.Title {
		t.Fatalf("events = %+v", evs)
	}
	if got := Format(evs[0], false); !strings.HasPrefix(got, "RFC retitled: #1 RFC: a better name") {
		t.Fatalf("line = %q", got)
	}
}

func TestLifecycle_LineModeObservesExactlyTheOldEvents(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0))}, t0)
	changed := withBody(rfc(1, "open", "needs-human"), rfcBody("v6", "Accepted", 0, 2))
	changed.Title = "RFC: renamed"
	if evs := d.Observe([]Issue{changed}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("line mode = %v", kinds(evs))
	}
	if evs := d.Observe(nil, t0.Add(time.Hour)); len(evs) != 0 {
		t.Fatalf("line mode flush = %v", kinds(evs))
	}
	// Two identical RFCs by one author are both announced in line mode.
	a, b := rfc(3, "open"), rfc(6, "open")
	a.CreatedAt, b.CreatedAt = t0.Add(time.Hour+time.Minute), t0.Add(time.Hour+2*time.Minute)
	if evs := d.Observe([]Issue{a, b}, t0.Add(time.Hour+3*time.Minute)); len(evs) != 2 || evs[0].Kind != KindOpened || evs[1].Kind != KindOpened {
		t.Fatalf("line mode duplicates = %v", kinds(evs))
	}
	// Switching to rich mode later does not announce what line mode saw.
	d.lifecycle = true
	if evs := d.Observe(nil, t0.Add(2*time.Hour)); len(evs) != 0 {
		t.Fatalf("after switching = %v", kinds(evs))
	}
}

func TestLifecycle_UntrackedRecordIsFilledSilently(t *testing.T) {
	d := lifecycleDetector(rfc(1, "open"))
	d.state.Issues["hivecommons/hive#1"] = &issueRecord{Open: true, LastSeen: t0}
	changed := withBody(rfc(1, "open", "stage/045-needs-decision"), rfcBody("v6", "Draft", 3, 0))
	if evs := d.Observe([]Issue{changed}, t0.Add(time.Minute)); len(evs) != 0 {
		t.Fatalf("untracked record = %v", kinds(evs))
	}
	rec := d.state.Issues["hivecommons/hive#1"]
	if !rec.Tracked || rec.Target != "v6" || rec.OpenDecisions != 3 || rec.DecisionsAnnounced != 3 || !rec.NeedsDecision {
		t.Fatalf("record = %+v", rec)
	}
}

func TestLifecycle_ClosedRFCHasNoLifecycleEvents(t *testing.T) {
	d := lifecycleDetector(withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0)))
	closed := withBody(withReason(rfc(1, "open", "needs-human"), "completed"), rfcBody("v6", "Draft", 0, 2))
	evs := d.Observe([]Issue{closed}, t0.Add(time.Minute))
	if len(evs) != 1 || evs[0].Kind != KindClosed {
		t.Fatalf("events = %v", kinds(evs))
	}
	if evs := d.Observe(nil, t0.Add(time.Hour)); len(evs) != 0 {
		t.Fatalf("flush after close = %v", kinds(evs))
	}
}

func sameRFC(n int, title, author string, created time.Time) Issue {
	is := rfc(n, "open")
	is.Title, is.Author, is.CreatedAt = title, author, created
	is.URL = fmt.Sprintf("https://github.com/hivecommons/hive/issues/%d", n)
	return is
}

func TestLifecycle_SameAuthorDuplicateIsNotedOnceAndItsCloseIsSilent(t *testing.T) {
	d := lifecycleDetector()
	first := sameRFC(10, "📝 RFC: Same Thing", "alice", t0.Add(time.Minute))
	if evs := d.Observe([]Issue{first}, t0.Add(2*time.Minute)); len(evs) != 1 || evs[0].Kind != KindOpened {
		t.Fatalf("first = %v", kinds(evs))
	}
	dup := sameRFC(11, "[RFC]  same   THING 🚀", "Alice", t0.Add(30*time.Minute))
	evs := d.Observe([]Issue{dup}, t0.Add(31*time.Minute))
	if len(evs) != 1 || evs[0].Kind != KindDuplicate || evs[0].Thread != "hivecommons/hive#10" {
		t.Fatalf("duplicate = %+v", evs)
	}
	if got := Format(evs[0], false); got != "RFC duplicate filed as #11 - <https://github.com/hivecommons/hive/issues/11>" {
		t.Fatalf("line = %q", got)
	}
	if rec := d.state.Issues["hivecommons/hive#11"]; rec.DuplicateOf != "hivecommons/hive#10" {
		t.Fatalf("record = %+v", rec)
	}
	if evs := d.Observe([]Issue{dup}, t0.Add(32*time.Minute)); len(evs) != 0 {
		t.Fatalf("noted twice = %v", kinds(evs))
	}
	// A duplicate's own changes and close post nothing.
	changed := withBody(dup, rfcBody("v6", "Draft", 1, 0))
	changed.Labels = []string{"needs-human"}
	if evs := d.Observe([]Issue{changed}, t0.Add(33*time.Minute)); len(evs) != 0 {
		t.Fatalf("duplicate changed = %v", kinds(evs))
	}
	if evs := d.Observe([]Issue{withReason(dup, "not_planned")}, t0.Add(34*time.Minute)); len(evs) != 0 {
		t.Fatalf("duplicate closed = %v", kinds(evs))
	}
	if evs := d.Observe([]Issue{withReason(first, "completed")}, t0.Add(35*time.Minute)); len(evs) != 1 || evs[0].Kind != KindClosed {
		t.Fatalf("original closed = %v", kinds(evs))
	}
}

func TestLifecycle_DuplicateNeedsSameAuthorTitleRepoAndWindow(t *testing.T) {
	first := sameRFC(10, "RFC: Same thing", "alice", t0.Add(time.Minute))
	for _, tc := range []struct {
		name string
		is   Issue
	}{
		{"other author", sameRFC(11, "RFC: Same thing", "bob", t0.Add(3*time.Minute))},
		{"other title", sameRFC(11, "RFC: Other thing", "alice", t0.Add(3*time.Minute))},
		{"after the window", sameRFC(11, "RFC: Same thing", "alice", t0.Add(time.Minute+duplicateWindow+time.Second))},
		{"other repo", func() Issue {
			is := sameRFC(11, "RFC: Same thing", "alice", t0.Add(3*time.Minute))
			is.Repo = "hivecommons/other"
			return is
		}()},
		{"no author", sameRFC(11, "RFC: Same thing", "", t0.Add(3*time.Minute))},
	} {
		d := lifecycleDetector()
		d.Observe([]Issue{first}, t0.Add(2*time.Minute))
		if evs := d.Observe([]Issue{tc.is}, tc.is.CreatedAt.Add(time.Second)); len(evs) != 1 || evs[0].Kind != KindOpened {
			t.Fatalf("%s: events = %v", tc.name, kinds(evs))
		}
	}
}

func TestLifecycle_DuplicatesInOnePollKeepTheOlderOne(t *testing.T) {
	d := lifecycleDetector()
	older := sameRFC(12, "RFC: Same thing", "alice", t0.Add(time.Minute))
	newer := sameRFC(11, "RFC: same thing", "alice", t0.Add(2*time.Minute))
	evs := d.Observe([]Issue{newer, older}, t0.Add(3*time.Minute))
	if len(evs) != 2 || evs[0].Kind != KindDuplicate || evs[0].Issue.Number != 11 || evs[0].Thread != "hivecommons/hive#12" || evs[1].Kind != KindOpened {
		t.Fatalf("events = %+v", evs)
	}
}

func TestNormalizeTitle(t *testing.T) {
	for in, want := range map[string]string{
		"📝 RFC: Richer   RFC publishing ✨": "richer rfc publishing",
		"[RFC] Richer RFC publishing":      "richer rfc publishing",
		"Richer RFC publishing!":           "richer rfc publishing",
		"🚀🚀":                               "",
	} {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// editThreads is a fakeThreads that can also edit cards and rename threads.
type editThreads struct {
	*fakeThreads
	edits     []fakeSend // message id, content
	renames   []fakeSend // thread id, name
	editErr   error
	renameErr error
}

func (e *editThreads) EditMessage(messageID, content string) error {
	e.edits = append(e.edits, fakeSend{messageID, content})
	return e.editErr
}

func (e *editThreads) RenameThread(threadID, name string) error {
	e.renames = append(e.renames, fakeSend{threadID, name})
	return e.renameErr
}

// threadedRFC3 opens RFC 3 with body in a threads rig and returns it.
func threadedRFC3(t *testing.T, r *threadRig, body string) Issue {
	t.Helper()
	is := withBody(r.openRFC(3), body)
	r.src.updated = []Issue{is}
	if res := r.run(); res.Threads != 1 {
		t.Fatalf("open: %+v", res)
	}
	return is
}

func TestThreads_RetitleEditsTheCardAndRenamesTheThreadWithoutPosting(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	ed := &editThreads{fakeThreads: r.th}
	r.sw.SetThreads(ed)
	is := threadedRFC3(t, r, "")
	is.Title = "RFC: a better name"
	r.src.updated = []Issue{is}
	res := r.run()
	wantText := Format(Event{Kind: KindOpened, Issue: is}, false)
	if len(ed.edits) != 1 || ed.edits[0] != (fakeSend{"msg-1", wantText}) {
		t.Fatalf("edits = %v", ed.edits)
	}
	if len(ed.renames) != 1 || ed.renames[0] != (fakeSend{"thr-msg-1", "#3 RFC: a better name"}) {
		t.Fatalf("renames = %v", ed.renames)
	}
	if len(r.th.posts) != 1 || len(r.th.sends) != 0 || len(r.th.echoes) != 0 {
		t.Fatalf("a retitle must post nothing: posts=%v sends=%v echoes=%v", r.th.posts, r.th.sends, r.th.echoes)
	}
	if res.Edited != 1 || res.Errors != 0 || len(res.Posted) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if rec := r.record(key3); rec.OpenText != wantText || rec.ThreadName != "#3 RFC: a better name" || rec.Title != "RFC: a better name" {
		t.Fatalf("record = %+v", rec)
	}
	if res := r.run(); res.Edited != 0 || len(ed.edits) != 1 {
		t.Fatalf("edited twice: %+v", res)
	}
}

func TestThreads_RetitleFailureIsCountedAndNotRetried(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	ed := &editThreads{fakeThreads: r.th, editErr: errors.New("discord API 404"), renameErr: errors.New("discord API 429")}
	r.sw.SetThreads(ed)
	is := threadedRFC3(t, r, "")
	is.Title = "RFC: a better name"
	r.src.updated = []Issue{is}
	if res := r.run(); res.Errors != 2 || res.Edited != 0 {
		t.Fatalf("res = %+v", res)
	}
	if res := r.run(); res.Errors != 0 || len(ed.edits) != 1 || len(ed.renames) != 1 {
		t.Fatalf("retried: %+v edits=%v", res, ed.edits)
	}
	if rec := r.record(key3); rec.Title != "RFC: a better name" || rec.OpenText == Format(Event{Kind: KindOpened, Issue: is}, false) {
		t.Fatalf("record = %+v", rec)
	}
}

func TestThreads_RetitleWithoutAnEditorChangesNothing(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	is := threadedRFC3(t, r, "")
	is.Title = "RFC: a better name"
	r.src.updated = []Issue{is}
	calls := r.th.calls()
	if res := r.run(); res.Errors != 0 || res.Edited != 0 || r.th.calls() != calls {
		t.Fatalf("res = %+v calls %d -> %d", res, calls, r.th.calls())
	}
}

func TestThreads_LifecycleNoticesGoIntoTheThreadOnly(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	is := threadedRFC3(t, r, rfcBody("v5", "Draft", 2, 0))
	changed := withBody(is, rfcBody("v6", "Draft", 1, 1))
	changed.Labels = []string{"stage/045-needs-decision"}
	r.src.updated = []Issue{changed}
	res := r.run()
	want := []string{
		Format(Event{Kind: KindTargetChanged, Issue: changed, From: "v5", To: "v6"}, false),
		Format(Event{Kind: KindNeedsDecision, Issue: changed}, false),
	}
	if res.Errors != 0 || len(r.th.sends) != 3 {
		t.Fatalf("res=%+v sends=%v", res, r.th.sends)
	}
	for i, w := range want {
		if r.th.sends[i] != (fakeSend{"thr-msg-1", w}) {
			t.Fatalf("send %d = %v, want %q", i, r.th.sends[i], w)
		}
	}
	if s := r.th.sends[2]; s.thread != "thr-msg-1" || !strings.HasPrefix(s.content, "RFC decisions: 2 → 1 open: #3 ") {
		t.Fatalf("decisions send = %v", s)
	}
	if len(r.th.echoes) != 0 || len(r.th.posts) != 1 {
		t.Fatalf("echoes=%v posts=%v", r.th.echoes, r.th.posts)
	}
	if res := r.run(); len(res.Posted) != 0 || len(r.th.sends) != 3 {
		t.Fatalf("repeated: %+v", res)
	}
}

func TestThreads_DuplicateGetsNoCardAndOneNoteInTheFirstThread(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	first := threadedRFC3(t, r, "")
	dup := rfc(4, "open")
	dup.Title, dup.CreatedAt = first.Title, r.now.Add(time.Second)
	r.src.updated = []Issue{dup}
	res := r.run()
	if len(r.th.posts) != 1 || len(r.th.startNames) != 1 {
		t.Fatalf("a duplicate must get no card or thread: posts=%v threads=%v", r.th.posts, r.th.startNames)
	}
	want := Format(Event{Kind: KindDuplicate, Issue: dup}, false)
	if len(r.th.sends) != 1 || r.th.sends[0] != (fakeSend{"thr-msg-1", want}) || len(r.th.echoes) != 0 {
		t.Fatalf("sends=%v echoes=%v", r.th.sends, r.th.echoes)
	}
	if res.Errors != 0 || len(res.Posted) != 1 {
		t.Fatalf("res = %+v", res)
	}
	r.src.updated = []Issue{withReason(dup, "not_planned")}
	if res := r.run(); len(res.Posted) != 0 || len(r.th.sends) != 1 || len(r.th.echoes) != 0 {
		t.Fatalf("closing the duplicate posted: %+v sends=%v", res, r.th.sends)
	}
}

func TestSweeperCardModeLifecycleNoticesArePlainLines(t *testing.T) {
	src := &fakeSource{}
	sw, tick := gateSweeper(t, Options{Format: FormatCard, Announcer: true}, src)
	tick()
	src.updated = []Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 1, 0))}
	pub := &recordPublisher{}
	if _, err := sw.Run(context.Background(), src, pub); err != nil || len(pub.posted) != 0 || len(pub.cards) != 0 {
		t.Fatalf("a first parsable body is the baseline, not a change: err=%v posted=%v", err, pub.posted)
	}
	tick()
	retitled := withBody(rfc(1, "open"), rfcBody("v6", "Draft", 1, 0))
	retitled.Title = "RFC: renamed"
	src.updated = []Issue{retitled}
	res, err := sw.Run(context.Background(), src, pub)
	if err != nil || len(pub.cards) != 0 || len(pub.posted) != 1 || !strings.HasPrefix(pub.posted[0], "RFC target: v5 → v6: #1 RFC: renamed") {
		t.Fatalf("res=%+v err=%v posted=%v cards=%d", res, err, pub.posted, len(pub.cards))
	}
	if len(res.Posted) != 1 || res.Edited != 0 {
		t.Fatalf("a retitle without threads must post nothing: %+v", res)
	}
}

func TestSweeperLineModeIgnoresLifecycleChanges(t *testing.T) {
	src := &fakeSource{}
	sw, tick := gateSweeper(t, Options{}, src)
	tick()
	changed := withBody(rfc(1, "open", "needs-human"), rfcBody("v6", "Draft", 1, 0))
	changed.Title = "RFC: renamed"
	src.updated = []Issue{changed}
	res, err := sw.Run(context.Background(), src, Poster(func(s string) error { t.Fatalf("line mode posted %q", s); return nil }))
	if err != nil || len(res.Posted) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSweeperFailureDoesNotRepeatAnAnnouncedEventOfTheSameRFC(t *testing.T) {
	src := &fakeSource{}
	sw, tick := gateSweeper(t, Options{Format: FormatCard, Announcer: true}, src)
	tick()
	src.updated = []Issue{withBody(rfc(1, "open"), rfcBody("v5", "Draft", 0, 0))}
	var posted []string
	post := Poster(func(s string) error { posted = append(posted, s); return nil })
	if _, err := sw.Run(context.Background(), src, post); err != nil {
		t.Fatal(err)
	}
	tick()
	src.updated = []Issue{withBody(rfc(1, "open"), rfcBody("v6", "Accepted", 0, 0))}
	calls := 0
	res, err := sw.Run(context.Background(), src, Poster(func(s string) error {
		calls++
		if calls == 2 {
			return errors.New("discord down")
		}
		posted = append(posted, s)
		return nil
	}))
	if err != nil || res.Errors != 1 || len(posted) != 1 || !strings.HasPrefix(posted[0], "RFC target: ") {
		t.Fatalf("res=%+v err=%v posted=%v", res, err, posted)
	}
	tick()
	if res, _ := sw.Run(context.Background(), src, post); len(res.Posted) != 0 || len(posted) != 1 {
		t.Fatalf("repeated the announced event: %+v posted=%v", res, posted)
	}
}

func TestLifecycle_DecisionsKeepVisibilityUnknown(t *testing.T) {
	is := withBody(rfc(1, "open"), rfcBody("v5", "Draft", 2, 0))
	is.VisibilityUnknown = true
	d := lifecycleDetector(is)
	rec := d.state.Issues["hivecommons/hive#1"]
	if !rec.VisibilityUnknown {
		t.Fatalf("record lost the unknown visibility: %+v", rec)
	}
	rec.DecisionsAnnounced, rec.DecisionsChangedAt = 0, time.Time{}
	evs := d.decisions(t0.Add(time.Hour))
	if len(evs) != 1 || !evs[0].Issue.VisibilityUnknown {
		t.Fatalf("events = %+v", evs)
	}
}
