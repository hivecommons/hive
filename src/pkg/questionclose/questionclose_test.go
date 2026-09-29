package questionclose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	testRepo   = "o/r"
	testIssue  = 42
	testAuthor = "asker"
	answerID   = int64(900)
	testWindow = 4 * time.Hour
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeTracker is an in-memory GitHub issue with call recording.
type fakeTracker struct {
	mu        sync.Mutex
	thread    github.IssueThread
	reactors  []string
	threadErr error
	reactErr  error
	closeErr  error
	labelErr  error
	held      []string

	threadReads int
	closes      []github.IssueCloseOptions
	addedLabels []string
}

func (f *fakeTracker) IssueThread(_ context.Context, repo string, number int) (github.IssueThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threadReads++
	if f.threadErr != nil {
		return github.IssueThread{}, f.threadErr
	}
	th := f.thread
	th.Labels = append([]string(nil), f.thread.Labels...)
	th.Comments = append([]github.ThreadComment(nil), f.thread.Comments...)
	return th, nil
}

func (f *fakeTracker) CommentReactors(_ context.Context, _ string, commentID int64, content string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reactErr != nil {
		return nil, f.reactErr
	}
	if commentID != answerID || content != "-1" {
		return nil, nil
	}
	return append([]string(nil), f.reactors...), nil
}

func (f *fakeTracker) CloseIssue(_ context.Context, _ string, _ int, opts github.IssueCloseOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closes = append(f.closes, opts)
	f.thread.State = "closed"
	return nil
}

func (f *fakeTracker) AddLabels(_ context.Context, _ string, _ int, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelErr != nil {
		return f.labelErr
	}
	f.addedLabels = append(f.addedLabels, labels...)
	f.thread.Labels = append(f.thread.Labels, labels...)
	return nil
}

func (f *fakeTracker) IsHeldLabels(labels []string) bool {
	for _, l := range labels {
		for _, h := range f.held {
			if strings.EqualFold(l, h) {
				return true
			}
		}
	}
	return github.HasHoldLabel(labels)
}

func (f *fakeTracker) addComment(id int64, author, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.thread.Comments = append(f.thread.Comments, github.ThreadComment{ID: id, Author: author, Body: body, CreatedAt: at})
}

// answeredThread is an open question whose last comment is the hive's answer.
func answeredThread() github.IssueThread {
	return github.IssueThread{
		State:  "open",
		Author: testAuthor,
		Labels: []string{"question"},
		Comments: []github.ThreadComment{
			{ID: 800, Author: testAuthor, Body: "more context", CreatedAt: t0.Add(-time.Hour)},
			{ID: answerID, Author: "hive-app[bot]", Body: "Use X.\n\n" + AnswerFooter(testWindow), CreatedAt: t0},
		},
	}
}

func enabledSettings() Settings {
	return Settings{Enabled: true, Window: testWindow, QuestionLabels: []string{"question"}, HumanLabel: "needs-human"}
}

type clock struct{ now time.Time }

func newManager(t *testing.T, path string, s Settings, tr *fakeTracker, c *clock) *Manager {
	t.Helper()
	m, err := New(path, s, func() Tracker { return tr }, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.now = func() time.Time { return c.now }
	return m
}

func questionIssue(updated time.Time) github.Issue {
	return github.Issue{Repo: testRepo, Number: testIssue, Labels: []string{"question"}, UpdatedAt: updated}
}

// scheduleOne offers the issue and ticks once, asserting a close was scheduled.
func scheduleOne(t *testing.T, m *Manager, c *clock) {
	t.Helper()
	m.Offer([]github.Issue{questionIssue(t0)})
	rep := m.Tick(context.Background())
	if rep.Scheduled != 1 {
		t.Fatalf("expected one schedule, got %+v", rep)
	}
	got := m.Scheduled()
	if len(got) != 1 || got[0].AnswerCommentID != answerID || !got[0].Deadline.Equal(t0.Add(testWindow)) || got[0].Author != testAuthor {
		t.Fatalf("scheduled = %+v", got)
	}
}

func TestNoReactionClosesAtDeadlineAsCompleted(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0.Add(time.Minute)}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)

	// Before the deadline nothing is closed.
	c.now = t0.Add(testWindow - time.Minute)
	if rep := m.Tick(context.Background()); rep.Closed != 0 || len(tr.closes) != 0 {
		t.Fatalf("closed before deadline: %+v", rep)
	}

	c.now = t0.Add(testWindow)
	rep := m.Tick(context.Background())
	if rep.Closed != 1 || len(tr.closes) != 1 {
		t.Fatalf("expected one close at the deadline, got %+v closes=%d", rep, len(tr.closes))
	}
	opts := tr.closes[0]
	if opts.StateReason != github.IssueStateReasonCompleted || !opts.SuppressOverrideComment {
		t.Fatalf("close options = %+v, want state_reason completed and no override comment", opts)
	}
	// The answer is still the last comment: the close added none.
	if last := tr.thread.Comments[len(tr.thread.Comments)-1]; last.ID != answerID {
		t.Fatalf("last comment = %d, want the answer %d", last.ID, answerID)
	}
	if len(m.Scheduled()) != 0 {
		t.Fatal("a closed issue must leave the schedule")
	}
	if s, ok := m.SettledFor(testRepo, testIssue); !ok || s.Outcome != OutcomeClosed {
		t.Fatalf("settled = %+v %v", s, ok)
	}
}

func TestAuthorThumbsDownKeepsOpenAndHandsToHuman(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread(), reactors: []string{"Asker"}}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)

	c.now = t0.Add(testWindow + time.Minute)
	rep := m.Tick(context.Background())
	if rep.KeptOpen != 1 || rep.Closed != 0 || len(tr.closes) != 0 {
		t.Fatalf("author 👎 must keep the issue open, got %+v closes=%d", rep, len(tr.closes))
	}
	if len(tr.addedLabels) != 1 || tr.addedLabels[0] != "needs-human" {
		t.Fatalf("added labels = %v, want [needs-human]", tr.addedLabels)
	}
	if s, _ := m.SettledFor(testRepo, testIssue); s.Outcome != OutcomeKeptOpen {
		t.Fatalf("outcome = %q", s.Outcome)
	}
	// Later passes see the human label and never reschedule it.
	m.Offer([]github.Issue{{Repo: testRepo, Number: testIssue, Labels: []string{"question", "needs-human"}, UpdatedAt: c.now}})
	if rep := m.Tick(context.Background()); rep.Scheduled != 0 {
		t.Fatalf("rescheduled a handed-off issue: %+v", rep)
	}
}

func TestThumbsDownFromSomeoneElseDoesNotKeepOpen(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread(), reactors: []string{"drive-by"}}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)
	c.now = t0.Add(testWindow)
	if rep := m.Tick(context.Background()); rep.Closed != 1 {
		t.Fatalf("only the author's 👎 keeps it open, got %+v", rep)
	}
}

func TestFollowUpCommentCancels(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)

	// The author replies an hour later; the next pass sees newer activity
	// and cancels right away, well before the deadline.
	reply := t0.Add(time.Hour)
	tr.addComment(901, testAuthor, "that did not work", reply)
	c.now = reply.Add(time.Minute)
	m.Offer([]github.Issue{questionIssue(reply)})
	rep := m.Tick(context.Background())
	if rep.Cancelled != 1 || len(m.Scheduled()) != 0 {
		t.Fatalf("follow-up comment must cancel, got %+v scheduled=%v", rep, m.Scheduled())
	}
	if s, _ := m.SettledFor(testRepo, testIssue); s.Outcome != OutcomeFollowUp {
		t.Fatalf("outcome = %q", s.Outcome)
	}
	c.now = t0.Add(2 * testWindow)
	m.Tick(context.Background())
	if len(tr.closes) != 0 {
		t.Fatal("a cancelled schedule must never close the issue")
	}
}

func TestFollowUpCommentCancelsAtDeadlineWithoutActivitySignal(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)
	tr.addComment(901, "someone", "me too", t0.Add(time.Hour))
	// No further Offer: the deadline read still sees the comment.
	c.now = t0.Add(testWindow)
	if rep := m.Tick(context.Background()); rep.Cancelled != 1 || len(tr.closes) != 0 {
		t.Fatalf("got %+v closes=%d", rep, len(tr.closes))
	}
}

func TestScheduleSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "question-autoclose.json")
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	first := newManager(t, path, enabledSettings(), tr, c)
	scheduleOne(t, first, c)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("schedule was not persisted: %v", err)
	}

	// A new process: fresh manager, no Offer, same file.
	c.now = t0.Add(testWindow + time.Minute)
	second := newManager(t, path, enabledSettings(), tr, c)
	got := second.Scheduled()
	if len(got) != 1 || !got[0].Deadline.Equal(t0.Add(testWindow)) {
		t.Fatalf("restarted schedule = %+v, want the original deadline", got)
	}
	if rep := second.Tick(context.Background()); rep.Closed != 1 {
		t.Fatalf("restarted manager did not close at the deadline: %+v", rep)
	}

	// The settled record survives a restart too, so the same answer is
	// never scheduled again.
	third := newManager(t, path, enabledSettings(), tr, c)
	if s, ok := third.SettledFor(testRepo, testIssue); !ok || s.AnswerCommentID != answerID {
		t.Fatalf("settled record lost across restart: %+v %v", s, ok)
	}
}

func TestToggleOffIsNoOp(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0.Add(2 * testWindow)}
	s := enabledSettings()
	s.Enabled = false
	m := newManager(t, "", s, tr, c)
	m.Offer([]github.Issue{questionIssue(t0)})
	if rep := m.Tick(context.Background()); rep != (Report{}) {
		t.Fatalf("disabled tick did work: %+v", rep)
	}
	if tr.threadReads != 0 || len(tr.closes) != 0 || len(tr.addedLabels) != 0 {
		t.Fatalf("disabled manager touched GitHub: reads=%d closes=%d labels=%v", tr.threadReads, len(tr.closes), tr.addedLabels)
	}
	if got := m.KickSection([]github.Issue{questionIssue(t0)}); got != "" {
		t.Fatalf("disabled kick section = %q", got)
	}
	var nilMgr *Manager
	nilMgr.Offer(nil)
	if nilMgr.Enabled() || nilMgr.Scheduled() != nil || nilMgr.KickSection(nil) != "" || nilMgr.Tick(context.Background()) != (Report{}) {
		t.Fatal("nil manager must be inert")
	}
	if _, ok := nilMgr.SettledFor(testRepo, testIssue); ok {
		t.Fatal("nil manager has no settled records")
	}
	nilMgr.Run(context.Background(), time.Millisecond)
}

func TestSettingsFromConfigDefaultsOff(t *testing.T) {
	t.Setenv(config.QuestionAutocloseEnvVar, "")
	t.Setenv(config.QuestionAutocloseHoursEnvVar, "")
	s := SettingsFromConfig(config.QuestionAutocloseConfig{})
	if s.Enabled || s.Window != 4*time.Hour || s.HumanLabel != "needs-human" || len(s.QuestionLabels) == 0 {
		t.Fatalf("settings = %+v", s)
	}
}

func TestExternalCloseAndRelabelCancel(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(tr *fakeTracker)
		offer   []string
		outcome string
		noRead  bool
	}{
		{name: "closed by someone else", mutate: func(tr *fakeTracker) { tr.thread.State = "closed" }, outcome: OutcomeClosedElsewhere},
		{name: "question label removed", mutate: func(tr *fakeTracker) { tr.thread.Labels = []string{"docs"} }, outcome: OutcomeRelabelled},
		{name: "hold added", mutate: func(tr *fakeTracker) { tr.thread.Labels = append(tr.thread.Labels, "hold") }, outcome: OutcomeRelabelled},
		{name: "operator hold label added", mutate: func(tr *fakeTracker) {
			tr.held = []string{"do-not-touch"}
			tr.thread.Labels = append(tr.thread.Labels, "do-not-touch")
		}, outcome: OutcomeRelabelled},
		{name: "relabelled bug in the pass list", offer: []string{"kind/bug"}, outcome: OutcomeRelabelled, noRead: true},
		{name: "answer deleted", mutate: func(tr *fakeTracker) { tr.thread.Comments = tr.thread.Comments[:1] }, outcome: OutcomeAnswerDeleted},
		{name: "became a human-filed bug", mutate: func(tr *fakeTracker) { tr.thread.BugFamily = true }, outcome: OutcomeOutOfScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTracker{thread: answeredThread()}
			c := &clock{now: t0}
			m := newManager(t, "", enabledSettings(), tr, c)
			scheduleOne(t, m, c)
			if tc.mutate != nil {
				tc.mutate(tr)
			}
			if tc.offer != nil {
				m.Offer([]github.Issue{{Repo: testRepo, Number: testIssue, Labels: tc.offer, UpdatedAt: t0}})
			}
			reads := tr.threadReads
			c.now = t0.Add(testWindow)
			rep := m.Tick(context.Background())
			if rep.Cancelled != 1 || len(tr.closes) != 0 {
				t.Fatalf("got %+v closes=%d", rep, len(tr.closes))
			}
			if s, _ := m.SettledFor(testRepo, testIssue); s.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", s.Outcome, tc.outcome)
			}
			if tc.noRead && tr.threadReads != reads {
				t.Fatal("a relabel visible in the pass list should cancel without an API read")
			}
		})
	}
}

func TestNeverSchedulesOutOfScopeIssues(t *testing.T) {
	cases := map[string]func(tr *fakeTracker) []string{
		"bug label":         func(tr *fakeTracker) []string { return []string{"question", "bug"} },
		"enhancement label": func(tr *fakeTracker) []string { return []string{"question", "enhancement"} },
		"hold label":        func(tr *fakeTracker) []string { return []string{"question", "hold"} },
		"no question label": func(tr *fakeTracker) []string { return []string{"docs"} },
		"answer by the author": func(tr *fakeTracker) []string {
			tr.thread.Comments[1].Author = testAuthor
			return []string{"question"}
		},
		"last comment is not an answer": func(tr *fakeTracker) []string {
			tr.thread.Comments[1].Body = "plain comment"
			return []string{"question"}
		},
		"issue already closed": func(tr *fakeTracker) []string {
			tr.thread.State = "closed"
			return []string{"question"}
		},
		"pull request": func(tr *fakeTracker) []string {
			tr.thread.IsPullRequest = true
			return []string{"question"}
		},
		"no comments": func(tr *fakeTracker) []string {
			tr.thread.Comments = nil
			return []string{"question"}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			tr := &fakeTracker{thread: answeredThread()}
			labels := setup(tr)
			c := &clock{now: t0}
			m := newManager(t, "", enabledSettings(), tr, c)
			m.Offer([]github.Issue{{Repo: testRepo, Number: testIssue, Labels: labels, UpdatedAt: t0}})
			if rep := m.Tick(context.Background()); rep.Scheduled != 0 || len(m.Scheduled()) != 0 {
				t.Fatalf("scheduled an out-of-scope issue: %+v", rep)
			}
		})
	}
}

func TestSettledAnswerIsNotRescheduledButANewAnswerIs(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)
	tr.addComment(901, testAuthor, "follow-up", t0.Add(time.Hour))
	c.now = t0.Add(testWindow)
	m.Tick(context.Background()) // cancels

	// Same answer, new activity: still not rescheduled.
	tr.mu.Lock()
	tr.thread.Comments = tr.thread.Comments[:2]
	tr.mu.Unlock()
	m.Offer([]github.Issue{questionIssue(c.now)})
	if rep := m.Tick(context.Background()); rep.Scheduled != 0 {
		t.Fatalf("settled answer rescheduled: %+v", rep)
	}

	// A fresh answer after the follow-up starts a new schedule.
	fresh := c.now.Add(time.Hour)
	tr.mu.Lock()
	tr.thread.Comments = append(tr.thread.Comments,
		github.ThreadComment{ID: 902, Author: testAuthor, Body: "still stuck", CreatedAt: fresh.Add(-time.Minute)},
		github.ThreadComment{ID: 903, Author: "hive-app[bot]", Body: "Try Y.\n" + AnswerFooter(testWindow), CreatedAt: fresh})
	tr.mu.Unlock()
	c.now = fresh.Add(time.Minute)
	m.Offer([]github.Issue{questionIssue(fresh)})
	if rep := m.Tick(context.Background()); rep.Scheduled != 1 {
		t.Fatalf("new answer not scheduled: %+v", rep)
	}
	if got := m.Scheduled(); got[0].AnswerCommentID != 903 {
		t.Fatalf("scheduled = %+v", got)
	}
}

func TestCandidateReadsAreThrottled(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	tr.thread.Comments[1].Body = "not an answer yet"
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	m.Offer([]github.Issue{questionIssue(t0)})
	m.Tick(context.Background())
	m.Tick(context.Background())
	if tr.threadReads != 1 {
		t.Fatalf("unchanged issue read %d times, want 1", tr.threadReads)
	}
	// No UpdatedAt: re-read only after the recheck interval.
	m.Offer([]github.Issue{questionIssue(time.Time{})})
	c.now = t0.Add(time.Minute)
	m.Tick(context.Background())
	if tr.threadReads != 1 {
		t.Fatalf("re-read inside the recheck interval: %d", tr.threadReads)
	}
	c.now = t0.Add(recheckInterval + time.Minute)
	m.Tick(context.Background())
	if tr.threadReads != 2 {
		t.Fatalf("not re-read after the recheck interval: %d", tr.threadReads)
	}
}

func TestTransientErrorsRetry(t *testing.T) {
	boom := errors.New("boom")
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)
	c.now = t0.Add(testWindow)

	for _, step := range []func(){
		func() { tr.threadErr = boom },
		func() { tr.threadErr = nil; tr.reactErr = boom },
		func() { tr.reactErr = nil; tr.closeErr = boom },
	} {
		step()
		if rep := m.Tick(context.Background()); rep != (Report{}) || len(m.Scheduled()) != 1 {
			t.Fatalf("a transient error must leave the schedule for retry: %+v", rep)
		}
	}
	tr.closeErr = nil
	if rep := m.Tick(context.Background()); rep.Closed != 1 {
		t.Fatalf("retry did not close: %+v", rep)
	}

	// Human-label failure on a 👎 retries too.
	tr2 := &fakeTracker{thread: answeredThread(), reactors: []string{testAuthor}, labelErr: boom}
	c2 := &clock{now: t0}
	m2 := newManager(t, "", enabledSettings(), tr2, c2)
	scheduleOne(t, m2, c2)
	c2.now = t0.Add(testWindow)
	if rep := m2.Tick(context.Background()); rep.KeptOpen != 0 || len(m2.Scheduled()) != 1 {
		t.Fatalf("label failure must retry: %+v", rep)
	}
	tr2.labelErr = nil
	if rep := m2.Tick(context.Background()); rep.KeptOpen != 1 {
		t.Fatalf("label retry: %+v", rep)
	}

	// Candidate read errors are logged and skipped.
	tr3 := &fakeTracker{threadErr: boom}
	m3 := newManager(t, "", enabledSettings(), tr3, &clock{now: t0})
	m3.Offer([]github.Issue{questionIssue(t0)})
	if rep := m3.Tick(context.Background()); rep != (Report{}) {
		t.Fatalf("got %+v", rep)
	}
}

func TestReporterGateRefusalEndsSchedule(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread(), closeErr: github.ErrReporterConfirmationRequired}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)
	c.now = t0.Add(testWindow)
	if rep := m.Tick(context.Background()); rep.Cancelled != 1 || len(m.Scheduled()) != 0 {
		t.Fatalf("got %+v", rep)
	}
}

func TestNilTrackerIsSkipped(t *testing.T) {
	m, err := New("", enabledSettings(), func() Tracker { return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.Offer([]github.Issue{questionIssue(t0), {Repo: "", Number: 1}, {Repo: testRepo, Number: 0}})
	if rep := m.Tick(context.Background()); rep != (Report{}) {
		t.Fatalf("got %+v", rep)
	}
	m2, _ := New("", enabledSettings(), nil, nil)
	if rep := m2.Tick(context.Background()); rep != (Report{}) {
		t.Fatalf("got %+v", rep)
	}
}

func TestSettledRecordsArePruned(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	m := newManager(t, "", enabledSettings(), tr, c)
	scheduleOne(t, m, c)
	c.now = t0.Add(testWindow)
	m.Tick(context.Background())
	c.now = t0.Add(testWindow + settledRetention + time.Hour)
	m.Tick(context.Background())
	if _, ok := m.SettledFor(testRepo, testIssue); ok {
		t.Fatal("settled record outlived its retention")
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := New(bad, enabledSettings(), nil, nil)
	if err == nil || m == nil || len(m.Scheduled()) != 0 {
		t.Fatalf("corrupt file: m=%v err=%v", m, err)
	}
	// A directory where the file should be cannot be read.
	if _, err := New(dir, enabledSettings(), nil, nil); err == nil {
		t.Fatal("expected a read error for a directory path")
	}
	// Invalid rows are dropped on load.
	rows := filepath.Join(dir, "rows.json")
	body := `{"version":1,"scheduled":[{"repo":"","issue":1,"answer_comment_id":1},{"repo":"o/r","issue":2,"answer_comment_id":5}],"settled":[{"repo":"","issue":3},{"repo":"o/r","issue":4,"outcome":"closed"}]}`
	if err := os.WriteFile(rows, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err = New(rows, enabledSettings(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Scheduled(); len(got) != 1 || got[0].Issue != 2 {
		t.Fatalf("scheduled = %+v", got)
	}
	if _, ok := m.SettledFor("o/r", 4); !ok {
		t.Fatal("valid settled row dropped")
	}
}

func TestPersistFailureKeepsMemoryState(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file, so every save fails.
	tr := &fakeTracker{thread: answeredThread()}
	c := &clock{now: t0}
	// Loading through a regular file also fails; the manager still starts.
	m, err := New(filepath.Join(blocker, "q.json"), enabledSettings(), func() Tracker { return tr }, nil)
	if err == nil || m == nil {
		t.Fatalf("expected a load error and a usable manager, got m=%v err=%v", m, err)
	}
	m.now = func() time.Time { return c.now }
	scheduleOne(t, m, c)
}

func TestAnswerFooterAndKickSection(t *testing.T) {
	footer := AnswerFooter(testWindow)
	if !strings.HasPrefix(footer, AnswerMarker+"\n") || !strings.Contains(footer, "react 👎 to this comment and the issue will stay open") ||
		!strings.Contains(footer, "4 hours") {
		t.Fatalf("footer = %q", footer)
	}
	if strings.Contains(footer, "@") {
		t.Fatalf("footer carries a raw mention: %q", footer)
	}
	for d, want := range map[time.Duration]string{time.Hour: "1 hour", 90 * time.Minute: "1h30m0s", 30 * time.Minute: "30m0s", 6 * time.Hour: "6 hours"} {
		if got := formatWindow(d); got != want {
			t.Fatalf("formatWindow(%v) = %q, want %q", d, got, want)
		}
	}

	m := newManager(t, "", enabledSettings(), &fakeTracker{}, &clock{now: t0})
	sec := m.KickSection([]github.Issue{
		questionIssue(t0),
		{Repo: testRepo, Number: 7, Labels: []string{"question", "bug"}},
		{Repo: testRepo, Number: 8, Labels: []string{"docs"}},
	})
	if !strings.Contains(sec, footer) || !strings.Contains(sec, "o/r#42") || strings.Contains(sec, "o/r#7") || strings.Contains(sec, "o/r#8") {
		t.Fatalf("kick section = %q", sec)
	}
	if !strings.Contains(sec, "label it `question`") {
		t.Fatalf("kick section does not name the question label: %q", sec)
	}
	noLabels := enabledSettings()
	noLabels.QuestionLabels = []string{" "}
	m2 := newManager(t, "", noLabels, &fakeTracker{}, &clock{now: t0})
	if sec := m2.KickSection(nil); !strings.Contains(sec, "label it `question`") || strings.Contains(sec, "Question issues in your list") {
		t.Fatalf("kick section = %q", sec)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	tr := &fakeTracker{thread: answeredThread()}
	m := newManager(t, "", enabledSettings(), tr, &clock{now: t0})
	m.Offer([]github.Issue{questionIssue(t0)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx, time.Millisecond)
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for len(m.Scheduled()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never ticked")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done

	// Zero interval falls back to the default and still stops on cancel.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	m.Run(ctx2, 0)
}
