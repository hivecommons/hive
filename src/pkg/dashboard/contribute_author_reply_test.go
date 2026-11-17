package dashboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The issue author's answer ends the no-PR cooldown the question started
// (contribute_author_reply.go). The shape is projectbluefin/
// gutenprint-printer-app#238: a contributor asked the issue's author for the
// exact wording, the task ended with no PR, the author answered a minute
// later, and the issue stayed withheld for the whole cooldown.

const (
	replyIssueAuthor = "maint"
	replyContributor = "contrib"
)

// liveQuestionBody is the question the contributor posted on #238, trimmed:
// the trailer sits inside a blockquote, so it is the identity footer that
// marks it.
const liveQuestionBody = "> **What I need from you:** Reply with the exact heading wording you would like instead.\n>\n" +
	"> — hive: backend=pi model=lemonade/some-model\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `8712a9f`"

// authorReplyFixture is one hub with a fake comments read, a queue snapshot
// and an issue it can book completions on.
type authorReplyFixture struct {
	t        *testing.T
	dir      string
	cfg      *config.Config
	hub      *ContributeWSHub
	srv      *Server
	number   int
	key      string
	mu       sync.Mutex
	comments []ghpkg.IssueCommentEntry
	reads    int
	readErr  error
}

func newAuthorReplyFixture(t *testing.T, dir string) *authorReplyFixture {
	t.Helper()
	f := &authorReplyFixture{t: t, dir: dir, cfg: &config.Config{}, number: 238, key: idleKey(238)}
	f.attach(ledgerHub(dir))
	return f
}

// attach wires a hub to the fixture's config, queue and fake read; a restart
// test attaches a second hub built over the same directory.
func (f *authorReplyFixture) attach(hub *ContributeWSHub) {
	f.srv = &Server{deps: &Dependencies{Config: f.cfg}}
	hub.server = f.srv
	hub.authorReplyComments = func(_ context.Context, repo string, number int, _ time.Time) ([]ghpkg.IssueCommentEntry, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if repo != idleRepo || number != f.number {
			f.t.Errorf("comments read for %s#%d, want %s#%d", repo, number, idleRepo, f.number)
		}
		f.reads++
		if f.readErr != nil {
			return nil, f.readErr
		}
		return append([]ghpkg.IssueCommentEntry(nil), f.comments...), nil
	}
	f.hub = hub
}

// complete books a no-PR idle completion like the task_complete handler (the
// completion, then the task's start), dated at so a test can lay out a
// timeline in the past. A zero start leaves the start unknown.
func (f *authorReplyFixture) complete(at, start time.Time) {
	f.t.Helper()
	f.hub.markTaskCompletedVerdictKeySignal(f.key, "", completionVerdictIdle, replyContributor,
		"agent reported complete without a pull request: asked the author for the wording", completionSignalVerdict)
	f.hub.completedMu.Lock()
	f.hub.completedTasks[f.key] = at
	rec, ok := f.hub.idleCompletions[f.key]
	rec.RecordedAt = at
	f.hub.idleCompletions[f.key] = rec
	f.hub.completedMu.Unlock()
	if !ok {
		f.t.Fatal("setup: a no-PR idle completion must book an idle record")
	}
	f.hub.saveCompletedTasks()
	f.hub.saveIdleCompletions()
	f.hub.stampIdleCompletionTaskStart(f.key, start)
	if !start.IsZero() {
		if rec, _ := f.hub.idleCompletionKey(f.key); !rec.TaskStartedAt.Equal(start) {
			f.t.Fatalf("setup: task start not stamped: %+v", rec)
		}
	}
}

// queue puts the issue in the queue snapshot with its author and updated_at.
func (f *authorReplyFixture) queue(updatedAt time.Time) {
	issue := idleIssue(f.number)
	issue["author"] = replyIssueAuthor
	issue["updated_at"] = updatedAt.UTC().Format(time.RFC3339Nano)
	idleStatus(f.srv, issue)
}

func (f *authorReplyFixture) add(cm ghpkg.IssueCommentEntry) {
	f.mu.Lock()
	f.comments = append(f.comments, cm)
	f.mu.Unlock()
}

func (f *authorReplyFixture) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *authorReplyFixture) sweep() int {
	return f.hub.sweepAuthorReplyReleases(context.Background(), time.Now())
}

func (f *authorReplyFixture) inCooldown() bool {
	f.hub.completedMu.Lock()
	defer f.hub.completedMu.Unlock()
	_, ok := f.hub.completedTasks[f.key]
	return ok
}

func replyQuestion(id int64, at time.Time) ghpkg.IssueCommentEntry {
	return ghpkg.IssueCommentEntry{ID: id, Author: replyContributor, Body: liveQuestionBody, CreatedAt: at}
}

func replyComment(id int64, author string, at time.Time, body string) ghpkg.IssueCommentEntry {
	return ghpkg.IssueCommentEntry{ID: id, Author: author, Body: body, CreatedAt: at}
}

// replyBase is the start of every test's timeline: an hour ago, so the whole
// story lies in the past and the four-hour cooldown is still running.
func replyBase() time.Time { return time.Now().Add(-time.Hour).Truncate(time.Second) }

// The #238 story: the author's triage comment comes before the task, the
// question during it, the answer a few seconds after the task ended, and the
// answer quotes the question. The cooldown ends, the issue is offerable, the
// question is booked, and an unchanged queue snapshot costs no second read.
func TestAuthorReplyEndsNoPRCooldown(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyComment(100, replyIssueAuthor, base.Add(-time.Minute), "/triage accepted"))
	f.add(replyQuestion(101, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)
	reply := replyComment(102, replyIssueAuthor, base.Add(11*time.Minute+8*time.Second),
		"> 🐝 **Hive Agent**: `contributor` | **SHA:** `8712a9f`\n\nPlease use: `### Web interface and access control`")
	f.add(reply)
	f.queue(reply.CreatedAt)

	if !f.hub.isTaskInCooldownKey(f.key) {
		t.Fatal("setup: the no-PR completion must start a cooldown")
	}
	if got := f.sweep(); got != 1 {
		t.Fatalf("sweep released %d cooldowns, want 1", got)
	}
	if f.hub.isTaskInCooldownKey(f.key) {
		t.Fatal("the issue's author answered the question; the issue must be offerable again")
	}
	f.hub.completedMu.Lock()
	rel, ok := f.hub.authorReplyReleases[f.key]
	f.hub.completedMu.Unlock()
	if !ok || rel.QuestionID != 101 || rel.ReplyAuthor != replyIssueAuthor || !rel.ReplyAt.Equal(reply.CreatedAt.UTC()) {
		t.Fatalf("release record = %+v (found %v), want question 101 answered by %s", rel, ok, replyIssueAuthor)
	}
	if got := f.sweep(); got != 0 || f.readCount() != 1 {
		t.Fatalf("a second sweep released %d and made %d reads in all, want 0 and 1", got, f.readCount())
	}
}

// Only the issue's author answers: a reply by anyone else, by the
// contributor, by a bot, or a hive-marked comment under the author's login
// leaves the cooldown running.
func TestAuthorReplyFromSomeoneElseKeepsCooldown(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyQuestion(201, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)
	f.add(replyComment(202, "bystander", base.Add(12*time.Minute), "Use `### Web access`"))
	f.add(replyComment(203, replyContributor, base.Add(13*time.Minute), "any news?"))
	f.add(ghpkg.IssueCommentEntry{ID: 204, Author: replyIssueAuthor + "[bot]", AuthorIsBot: true,
		CreatedAt: base.Add(14 * time.Minute), Body: "automated"})
	f.add(replyComment(205, replyIssueAuthor, base.Add(20*time.Minute), liveQuestionBody))
	f.queue(base.Add(20 * time.Minute))

	if got := f.sweep(); got != 0 || !f.inCooldown() {
		t.Fatalf("sweep released %d (cooldown still on: %v); only the issue's author's own reply may end it", got, f.inCooldown())
	}
	if f.readCount() != 1 {
		t.Fatalf("reads = %d, want 1", f.readCount())
	}
}

// The author spoke during the task before the question was asked, and not
// since.
func TestAuthorCommentBeforeQuestionKeepsCooldown(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyComment(301, replyIssueAuthor, base.Add(time.Minute), "please make it shorter"))
	f.add(replyQuestion(302, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)
	// Something else bumped the issue after the completion (a label).
	f.queue(base.Add(15 * time.Minute))

	if got := f.sweep(); got != 0 || !f.inCooldown() {
		t.Fatalf("sweep released %d; an author comment before the question answers nothing", got)
	}
}

// A plain no-PR completion: no hive-marked comment during the task, so an
// author comment is just a comment and the cooldown runs as today.
func TestAuthorReplyWithoutHiveQuestionKeepsCooldown(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyComment(401, replyContributor, base.Add(5*time.Minute), "looking at this"))
	f.complete(base.Add(11*time.Minute), base)
	f.add(replyComment(402, replyIssueAuthor, base.Add(12*time.Minute), "thanks"))
	f.queue(base.Add(12 * time.Minute))

	if got := f.sweep(); got != 0 || !f.inCooldown() {
		t.Fatalf("sweep released %d; with no hive question the cooldown must run its course", got)
	}
}

// One question earns one release. After it, the next task ends with no PR
// again without asking anything new, and the author writes again: whether the
// hub knows the task's start (the question is outside the task) or not (the
// question is the one already released), nothing is released.
func TestAuthorReplySameQuestionReleasesOnce(t *testing.T) {
	for _, knownStart := range []bool{true, false} {
		name := "task start unknown"
		if knownStart {
			name = "task start known"
		}
		t.Run(name, func(t *testing.T) {
			f := newAuthorReplyFixture(t, t.TempDir())
			base := replyBase()
			f.add(replyQuestion(501, base.Add(10*time.Minute)))
			f.complete(base.Add(11*time.Minute), base)
			f.add(replyComment(502, replyIssueAuthor, base.Add(12*time.Minute), "Use `### Web access`"))
			f.queue(base.Add(12 * time.Minute))
			if got := f.sweep(); got != 1 {
				t.Fatalf("setup: the first answer must release, released %d", got)
			}

			start := time.Time{}
			if knownStart {
				start = base.Add(20 * time.Minute)
			}
			f.complete(base.Add(25*time.Minute), start)
			f.add(replyComment(503, replyIssueAuthor, base.Add(26*time.Minute), "did that work?"))
			f.queue(base.Add(26 * time.Minute))
			if got := f.sweep(); got != 0 || !f.inCooldown() {
				t.Fatalf("sweep released %d; a question already released must not release a later cooldown", got)
			}
			if f.readCount() != 2 {
				t.Fatalf("reads = %d, want 2 (one per completion with new activity)", f.readCount())
			}
		})
	}
}

// A later task that asks a NEW question and ends with no PR again may earn one
// more release.
func TestAuthorReplyNewQuestionReleasesAgain(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyQuestion(601, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)
	f.add(replyComment(602, replyIssueAuthor, base.Add(12*time.Minute), "Use `### Web access`"))
	f.queue(base.Add(12 * time.Minute))
	if got := f.sweep(); got != 1 {
		t.Fatalf("setup: the first answer must release, released %d", got)
	}

	f.add(replyQuestion(603, base.Add(24*time.Minute)))
	f.complete(base.Add(25*time.Minute), base.Add(20*time.Minute))
	f.add(replyComment(604, replyIssueAuthor, base.Add(26*time.Minute), "the second one, please"))
	f.queue(base.Add(26 * time.Minute))
	if got := f.sweep(); got != 1 || f.inCooldown() {
		t.Fatalf("sweep released %d; a new question answered by the author earns one more release", got)
	}
}

// The release is persisted: a hub restarted over the same ledgers does not
// release the same question again.
func TestAuthorReplyReleaseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	f := newAuthorReplyFixture(t, dir)
	base := replyBase()
	f.add(replyQuestion(701, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)
	f.add(replyComment(702, replyIssueAuthor, base.Add(12*time.Minute), "Use `### Web access`"))
	f.queue(base.Add(12 * time.Minute))
	if got := f.sweep(); got != 1 {
		t.Fatalf("setup: the first answer must release, released %d", got)
	}
	// The next task ends with no PR and no new question; its start is unknown,
	// so only the persisted release can tell the question was answered.
	f.complete(base.Add(25*time.Minute), time.Time{})
	f.add(replyComment(703, replyIssueAuthor, base.Add(26*time.Minute), "anything else?"))

	if _, err := os.Stat(filepath.Join(dir, authorReplyReleasesFileName)); err != nil {
		t.Fatalf("the release ledger must be written beside the no-PR streaks: %v", err)
	}
	hub2 := ledgerHub(dir)
	hub2.completedTasks = map[string]time.Time{}
	hub2.loadCompletedTasks()
	hub2.loadIdleCompletions()
	hub2.loadAuthorReplyReleases()
	f.attach(hub2)
	f.queue(base.Add(26 * time.Minute))
	if !f.hub.isTaskInCooldownKey(f.key) {
		t.Fatal("setup: the reloaded hub must carry the second completion's cooldown")
	}
	if got := f.sweep(); got != 0 || !f.inCooldown() {
		t.Fatalf("after a restart the sweep released %d; the question was already released", got)
	}
}

// With the completion cooldown switched off there is nothing to end, and the
// sweep reads nothing.
func TestAuthorReplyCooldownDisabledChangesNothing(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyQuestion(801, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)
	f.add(replyComment(802, replyIssueAuthor, base.Add(12*time.Minute), "Use `### Web access`"))
	f.queue(base.Add(12 * time.Minute))
	off := false
	f.cfg.Hub.ContributeCooldownEnabled = &off

	if got := f.sweep(); got != 0 || f.readCount() != 0 {
		t.Fatalf("with the cooldown off the sweep released %d and read %d times, want 0 and 0", got, f.readCount())
	}
	if !f.inCooldown() {
		t.Fatal("with the cooldown off the completion record must be left as it was")
	}
}

// A completion that shipped a PR books the with-PR cooldown; an answer to a
// question never ends it, and the sweep does not even read.
func TestAuthorReplyNeverEndsWithPRCooldown(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	f.hub.markTaskCompletedVerdictKeySignal(f.key, "https://github.com/"+idleRepo+"/pull/9",
		completionVerdictIdle, replyContributor, "", completionSignalVerdict)
	f.hub.completedMu.Lock()
	completedAt := f.hub.completedTasks[f.key]
	f.hub.completedMu.Unlock()
	f.add(replyQuestion(901, completedAt.Add(-30*time.Second)))
	f.add(replyComment(902, replyIssueAuthor, completedAt.Add(5*time.Second), "Use `### Web access`"))
	f.queue(completedAt.Add(5 * time.Second))

	if got := f.sweep(); got != 0 || f.readCount() != 0 || !f.hub.isTaskInCooldownKey(f.key) {
		t.Fatalf("with-PR cooldown: released %d, reads %d, cooldown on %v; want 0, 0, true",
			got, f.readCount(), f.hub.isTaskInCooldownKey(f.key))
	}
}

// The queue decides whether a read is spent: no activity since the
// completion, or activity the hub cannot see, reads nothing; a failed read is
// retried later rather than every pass.
func TestAuthorReplyReadsOnlyOnNewActivity(t *testing.T) {
	f := newAuthorReplyFixture(t, t.TempDir())
	base := replyBase()
	f.add(replyQuestion(1001, base.Add(10*time.Minute)))
	f.complete(base.Add(11*time.Minute), base)

	f.queue(base.Add(10 * time.Minute))
	if f.sweep(); f.readCount() != 0 {
		t.Fatalf("issue quiet since the completion: %d reads, want 0", f.readCount())
	}
	issue := idleIssue(f.number)
	issue["author"] = replyIssueAuthor
	idleStatus(f.srv, issue)
	if f.sweep(); f.readCount() != 0 {
		t.Fatalf("updated_at unknown: %d reads, want 0", f.readCount())
	}

	f.readErr = errors.New("boom")
	f.queue(base.Add(12 * time.Minute))
	f.sweep()
	f.sweep()
	if f.readCount() != 1 || !f.inCooldown() {
		t.Fatalf("a failed read: %d reads over two passes, want 1 (retry waits)", f.readCount())
	}
}
