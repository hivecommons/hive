package rfcnotify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeThreads is a scripted Discord: a message log plus failure switches.
type fakeThreads struct {
	posts       []fakePost
	postErrs    []error // consumed one per Post call; nil entries succeed
	nextMsg     int
	startErr    error
	started     map[string]string // message id -> thread id
	startNames  []string
	sends       []fakeSend
	sendErr     error
	echoes      []string
	recent      []Message
	recentErr   error
	recentCalls int
	recentLimit int
	urlErr      error
}

type fakePost struct{ content, nonce string }
type fakeSend struct{ thread, content string }

func (f *fakeThreads) Post(content, nonce string) (string, error) {
	f.posts = append(f.posts, fakePost{content, nonce})
	if len(f.postErrs) > 0 {
		err := f.postErrs[0]
		f.postErrs = f.postErrs[1:]
		if err != nil {
			return "", err
		}
	}
	f.nextMsg++
	return fmt.Sprintf("msg-%d", f.nextMsg), nil
}

func (f *fakeThreads) StartThread(messageID, name string) (string, error) {
	f.startNames = append(f.startNames, name)
	if f.startErr != nil {
		return "", f.startErr
	}
	if f.started == nil {
		f.started = map[string]string{}
	}
	id := "thr-" + messageID
	f.started[messageID] = id
	return id, nil
}

func (f *fakeThreads) Send(threadID, content string) error {
	f.sends = append(f.sends, fakeSend{threadID, content})
	return f.sendErr
}

func (f *fakeThreads) Echo(content string) error {
	f.echoes = append(f.echoes, content)
	return nil
}

func (f *fakeThreads) Recent(limit int) ([]Message, error) {
	f.recentCalls++
	f.recentLimit = limit
	return f.recent, f.recentErr
}

func (f *fakeThreads) ThreadURL(threadID string) (string, error) {
	if f.urlErr != nil {
		return "", f.urlErr
	}
	return "https://discord.com/channels/g/" + threadID, nil
}

func (f *fakeThreads) calls() int {
	return len(f.posts) + len(f.startNames) + len(f.sends) + len(f.echoes) + f.recentCalls
}

// commentSource is a fakeSource that can also comment on GitHub.
type commentSource struct {
	*fakeSource
	comments []string
	errs     []error
}

func (c *commentSource) Comment(_ context.Context, repo string, number int, body string) error {
	c.comments = append(c.comments, fmt.Sprintf("%s#%d %s", repo, number, body))
	if len(c.errs) > 0 {
		err := c.errs[0]
		c.errs = c.errs[1:]
		return err
	}
	return nil
}

// threadRig is a threads-mode announcer over a state file in a temp dir.
type threadRig struct {
	t    *testing.T
	path string
	now  time.Time
	src  *commentSource
	th   *fakeThreads
	sw   *Sweeper
	opts Options
}

func newThreadRig(t *testing.T, opts Options) *threadRig {
	t.Helper()
	r := &threadRig{t: t, path: filepath.Join(t.TempDir(), "rfc-notify.json"), now: t0, th: &fakeThreads{}}
	r.src = &commentSource{fakeSource: &fakeSource{open: []Issue{rfc(1, "open"), rfc(2, "open")}}}
	opts.StatePath, opts.Logger, opts.Threads = r.path, quiet, true
	opts.Now = func() time.Time { return r.now }
	r.opts = opts
	r.sw = r.newSweeper()
	r.run() // seed
	return r
}

func (r *threadRig) newSweeper() *Sweeper {
	sw := NewSweeper(r.opts)
	sw.SetThreads(r.th)
	return sw
}

// restart simulates a process restart over the same state file.
func (r *threadRig) restart() { r.sw = r.newSweeper() }

func (r *threadRig) run() Result {
	r.t.Helper()
	r.now = r.now.Add(DefaultInterval)
	res, err := r.sw.Run(context.Background(), r.src, Poster(func(string) error {
		r.t.Fatal("line poster used in threads mode")
		return nil
	}))
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return res
}

func (r *threadRig) record(key string) issueRecord {
	r.t.Helper()
	d, err := Load(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	rec := d.state.Issues[key]
	if rec == nil {
		r.t.Fatalf("no record for %s", key)
	}
	return *rec
}

// openRFC makes RFC n appear as newly opened on the next run.
func (r *threadRig) openRFC(n int) Issue {
	is := rfc(n, "open")
	is.CreatedAt = r.now.Add(time.Second)
	r.src.updated = []Issue{is}
	return is
}

const key3 = "hivecommons/hive#3"

func TestThreads_OpenedPostsOneCardAndStartsOneThread(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	is := r.openRFC(3)
	res := r.run()
	if res.Errors != 0 || len(r.th.posts) != 1 {
		t.Fatalf("res=%+v posts=%v", res, r.th.posts)
	}
	card := r.th.posts[0]
	if card.content != Format(Event{Kind: KindOpened, Issue: is}, false) {
		t.Fatalf("card = %q", card.content)
	}
	if card.nonce == "" || len(card.nonce) > 25 {
		t.Fatalf("nonce %q must be set and at most 25 characters", card.nonce)
	}
	if len(r.th.startNames) != 1 || !strings.HasPrefix(r.th.startNames[0], "#3 RFC: thing") {
		t.Fatalf("thread names = %v", r.th.startNames)
	}
	rec := r.record(key3)
	if rec.AnnounceState != StateThreaded || rec.MessageID != "msg-1" || rec.ThreadID != "thr-msg-1" || rec.Nonce != card.nonce {
		t.Fatalf("record = %+v", rec)
	}
	if res.Threads != 1 {
		t.Fatalf("Threads = %d, want 1", res.Threads)
	}
}

func TestThreads_NonceIsDeterministicPerRFC(t *testing.T) {
	if a, b := announceNonce(key3), announceNonce(key3); a != b || len(a) > 25 {
		t.Fatalf("nonce %q / %q", a, b)
	}
	if announceNonce(key3) == announceNonce("hivecommons/hive#4") {
		t.Fatal("different RFCs must not share a nonce")
	}
}

func TestThreads_ThreadNameTruncatedTo100(t *testing.T) {
	name := threadName(Issue{Number: 7, Title: strings.Repeat("é", 300)})
	if len([]rune(name)) != 100 || !strings.HasPrefix(name, "#7 ") {
		t.Fatalf("name = %q (%d runes)", name, len([]rune(name)))
	}
}

func TestThreads_ClosedPostsIntoThreadAndEchoesOnce(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.openRFC(3)
	r.run()
	closed := withReason(rfc(3, "closed"), "completed")
	r.src.updated = []Issue{closed}
	res := r.run()
	if len(r.th.posts) != 1 {
		t.Fatalf("a close must not post a second card: %v", r.th.posts)
	}
	want := Format(Event{Kind: KindClosed, Issue: closed}, false)
	if len(r.th.sends) != 1 || r.th.sends[0] != (fakeSend{"thr-msg-1", want}) {
		t.Fatalf("thread sends = %v", r.th.sends)
	}
	if len(r.th.echoes) != 1 || r.th.echoes[0] != want {
		t.Fatalf("channel echoes = %v", r.th.echoes)
	}
	if len(res.Posted) != 1 {
		t.Fatalf("Posted = %v", res.Posted)
	}
}

func TestThreads_ReadyPostsIntoThreadAndEchoes(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	held := rfc(3, "open", "hold")
	held.CreatedAt = r.now.Add(time.Second)
	r.src.updated = []Issue{held}
	r.run()
	lifted := rfc(3, "open")
	r.src.updated = []Issue{lifted}
	r.run()
	want := Format(Event{Kind: KindReady, Issue: lifted}, false)
	if len(r.th.sends) != 1 || r.th.sends[0] != (fakeSend{"thr-msg-1", want}) {
		t.Fatalf("sends = %v", r.th.sends)
	}
	if len(r.th.echoes) != 1 || r.th.echoes[0] != want {
		t.Fatalf("echoes = %v", r.th.echoes)
	}
}

func TestThreads_LegacyRFCWithoutThreadGetsAChannelLineNotABackfilledThread(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.src.updated = []Issue{withReason(rfc(1, "closed"), "completed")} // seeded before threads existed
	r.run()
	if len(r.th.startNames) != 0 || len(r.th.posts) != 0 {
		t.Fatalf("legacy RFC must not get a card or thread: posts=%v threads=%v", r.th.posts, r.th.startNames)
	}
	if len(r.th.echoes) != 1 || !strings.HasPrefix(r.th.echoes[0], "RFC closed") {
		t.Fatalf("echoes = %v", r.th.echoes)
	}
}

func TestThreads_WriteAheadIntentSurvivesAFailedPost(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.postErrs = []error{errors.New("discord down")}
	is := r.openRFC(3)
	res := r.run()
	if res.Errors != 1 || len(res.Posted) != 0 {
		t.Fatalf("res = %+v", res)
	}
	rec := r.record(key3)
	if rec.AnnounceState != StatePending || rec.Nonce != announceNonce(key3) || rec.MessageID != "" {
		t.Fatalf("intent not persisted: %+v", rec)
	}
	if rec.OpenText != Format(Event{Kind: KindOpened, Issue: is}, false) {
		t.Fatalf("intent lost the card text: %q", rec.OpenText)
	}
}

func TestThreads_PendingRetryRepostsWithSameNonceAndThreads(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.postErrs = []error{errors.New("discord down")}
	r.openRFC(3)
	r.run()
	r.src.updated = nil
	res := r.run() // retry: lookup finds nothing, re-post once
	if len(r.th.posts) != 2 || r.th.posts[0].nonce != r.th.posts[1].nonce || r.th.posts[0].content != r.th.posts[1].content {
		t.Fatalf("retry must re-post the same card and nonce: %v", r.th.posts)
	}
	if r.th.recentCalls != 1 || r.th.recentLimit != 50 {
		t.Fatalf("lookup calls=%d limit=%d, want one lookup of 50", r.th.recentCalls, r.th.recentLimit)
	}
	if rec := r.record(key3); rec.AnnounceState != StateThreaded {
		t.Fatalf("record = %+v", rec)
	}
	if res.Threads != 1 {
		t.Fatalf("Threads = %d", res.Threads)
	}
}

func TestThreads_RestartAfterPostBeforeThreadAttachesExistingMessage(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.startErr = errors.New("missing permissions")
	r.openRFC(3)
	res := r.run()
	if res.Errors != 1 || r.record(key3).AnnounceState != StatePosted {
		t.Fatalf("res=%+v rec=%+v", res, r.record(key3))
	}
	r.th.startErr = nil
	r.src.updated = nil
	r.restart()
	r.run()
	if len(r.th.posts) != 1 {
		t.Fatalf("restart posted a second card: %v", r.th.posts)
	}
	if rec := r.record(key3); rec.AnnounceState != StateThreaded || rec.ThreadID != "thr-msg-1" {
		t.Fatalf("record = %+v", rec)
	}
	if r.th.recentCalls != 0 {
		t.Fatalf("a known message needs no history lookup, got %d", r.th.recentCalls)
	}
}

func TestThreads_CrashBeforeMessageIDStoredAdoptsMessageFromHistory(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.postErrs = []error{errors.New("timeout after Discord accepted it")}
	is := r.openRFC(3)
	r.run()
	card := Format(Event{Kind: KindOpened, Issue: is}, false)
	r.th.recent = []Message{
		{ID: "other", Content: "unrelated", Self: true},
		{ID: "foreign", Content: card, Self: false}, // someone else's copy is not ours
		{ID: "m-adopted", Content: card, Self: true},
	}
	r.src.updated = nil
	r.restart()
	r.run()
	if len(r.th.posts) != 1 {
		t.Fatalf("an existing card must not be posted again: %v", r.th.posts)
	}
	if rec := r.record(key3); rec.MessageID != "m-adopted" || rec.ThreadID != "thr-m-adopted" || rec.AnnounceState != StateThreaded {
		t.Fatalf("record = %+v", rec)
	}
}

func TestThreads_HistoryMatchOnNonce(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.postErrs = []error{errors.New("timeout")}
	r.openRFC(3)
	r.run()
	r.th.recent = []Message{{ID: "m-n", Content: "edited text", Nonce: announceNonce(key3), Self: true}}
	r.src.updated = nil
	r.run()
	if len(r.th.posts) != 1 || r.record(key3).MessageID != "m-n" {
		t.Fatalf("posts=%v rec=%+v", r.th.posts, r.record(key3))
	}
}

func TestThreads_HistoryLookupFailureNeverReposts(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.postErrs = []error{errors.New("timeout")}
	r.openRFC(3)
	r.run()
	r.th.recentErr = errors.New("history unavailable")
	r.src.updated = nil
	res := r.run()
	if len(r.th.posts) != 1 || res.Errors != 1 {
		t.Fatalf("an unresolved lookup must not duplicate the card: posts=%v res=%+v", r.th.posts, res)
	}
}

func TestThreads_PendingRepostIsBounded(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.postErrs = []error{errors.New("e"), errors.New("e"), errors.New("e"), errors.New("e"), errors.New("e"), errors.New("e")}
	r.openRFC(3)
	for i := 0; i < 6; i++ {
		r.run()
		r.src.updated = nil
	}
	if len(r.th.posts) != maxPostTries {
		t.Fatalf("%d posts, want the retries bounded at %d", len(r.th.posts), maxPostTries)
	}
}

func TestThreads_ReconcileNeverScansWithoutAnIntent(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.run()
	r.run()
	if r.th.calls() != 0 {
		t.Fatalf("seeded records must never be reconciled or backfilled: %d Discord calls", r.th.calls())
	}
}

func TestThreads_ThreadStartRetryIsBounded(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.th.startErr = errors.New("no")
	r.openRFC(3)
	for i := 0; i < 6; i++ {
		r.run()
		r.src.updated = nil
	}
	if len(r.th.startNames) != maxThreadTries {
		t.Fatalf("%d thread attempts, want %d", len(r.th.startNames), maxThreadTries)
	}
}

func TestThreads_NonAnnouncerNeverTouchesDiscord(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: false})
	r.openRFC(3)
	res := r.run()
	r.src.updated = nil
	r.restart()
	r.run()
	if r.th.calls() != 0 || res.Withheld != 1 {
		t.Fatalf("calls=%d res=%+v", r.th.calls(), res)
	}
	if len(r.src.comments) != 0 {
		t.Fatalf("comments = %v", r.src.comments)
	}
}

func TestThreads_FailedThreadPostRollsBackSoItRetries(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.openRFC(3)
	r.run()
	closed := withReason(rfc(3, "closed"), "completed")
	r.src.updated = []Issue{closed}
	r.th.sendErr = errors.New("rate limited")
	res := r.run()
	if res.Errors != 1 || len(res.Posted) != 0 {
		t.Fatalf("res = %+v", res)
	}
	r.th.sendErr = nil
	res = r.run()
	if len(res.Posted) != 1 || len(r.th.echoes) != 1 {
		t.Fatalf("the close must be retried exactly once: res=%+v echoes=%v", res, r.th.echoes)
	}
}

func TestThreads_DocAcceptedPostsIntoLinkedRFCThread(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.openRFC(3)
	r.run()
	pr := Issue{
		Repo: "hivecommons/hive", Number: 40, Title: "📝 proposal: thing", URL: "https://github.com/hivecommons/hive/pull/40",
		State: "closed", IsPR: true, Body: "Refs #3", CreatedAt: r.now.Add(-time.Hour), MergedAt: r.now.Add(time.Second),
	}
	r.src.updated = []Issue{pr}
	r.run()
	if len(r.th.sends) != 1 || r.th.sends[0].thread != "thr-msg-1" || !strings.HasPrefix(r.th.sends[0].content, "RFC doc accepted (RFC #3)") {
		t.Fatalf("sends = %v", r.th.sends)
	}
}

func TestThreads_BacklinkPostedOnceAfterThreadAndNotAcrossRestarts(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true, Backlink: true})
	r.openRFC(3)
	r.run()
	want := "hivecommons/hive#3 Discussion thread: https://discord.com/channels/g/thr-msg-1"
	if len(r.src.comments) != 1 || r.src.comments[0] != want {
		t.Fatalf("comments = %v, want [%s]", r.src.comments, want)
	}
	if !r.record(key3).BacklinkPosted {
		t.Fatal("BacklinkPosted not persisted")
	}
	r.src.updated = nil
	r.run()
	r.restart()
	r.run()
	if len(r.src.comments) != 1 {
		t.Fatalf("backlink repeated: %v", r.src.comments)
	}
}

func TestThreads_BacklinkOffByDefault(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true})
	r.openRFC(3)
	r.run()
	if len(r.src.comments) != 0 || r.record(key3).BacklinkPosted {
		t.Fatalf("comments = %v", r.src.comments)
	}
}

func TestThreads_BacklinkWaitsForTheThread(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true, Backlink: true})
	r.th.startErr = errors.New("no thread")
	r.openRFC(3)
	r.run()
	if len(r.src.comments) != 0 {
		t.Fatalf("backlink posted before the thread existed: %v", r.src.comments)
	}
	r.th.startErr = nil
	r.src.updated = nil
	r.run()
	if len(r.src.comments) != 1 {
		t.Fatalf("comments = %v, want one once the thread exists", r.src.comments)
	}
}

func TestThreads_BacklinkFailureIsBoundedAndLeavesFlagUnset(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true, Backlink: true})
	r.src.errs = []error{errors.New("403"), errors.New("403"), errors.New("403"), errors.New("403"), errors.New("403")}
	r.openRFC(3)
	for i := 0; i < 5; i++ {
		r.run()
		r.src.updated = nil
	}
	if len(r.src.comments) != maxBacklinkTries {
		t.Fatalf("%d attempts, want %d", len(r.src.comments), maxBacklinkTries)
	}
	if r.record(key3).BacklinkPosted {
		t.Fatal("flag must stay unset on failure")
	}
}

func TestThreads_BacklinkNeedsACommenterSource(t *testing.T) {
	r := newThreadRig(t, Options{Announcer: true, Backlink: true})
	r.openRFC(3)
	r.now = r.now.Add(DefaultInterval)
	if _, err := r.sw.Run(context.Background(), r.src.fakeSource, Poster(noopPost)); err != nil {
		t.Fatal(err)
	}
	if len(r.src.comments) != 0 {
		t.Fatalf("comments = %v", r.src.comments)
	}
}

func noopPost(string) error { return nil }

func TestThreads_V1StateLoadsUnchangedAndSavesAsV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rfc-notify.json")
	v1 := `{"version":1,"seeded":true,"watermark":"2026-10-27T12:00:00Z","issues":{"hivecommons/hive#1":{"open":true,"held":true,"actionable":false,"ready_announced":true,"last_seen":"2026-10-27T12:00:00Z"}}}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Load(path)
	if err != nil {
		t.Fatalf("Load v1: %v", err)
	}
	rec := d.state.Issues["hivecommons/hive#1"]
	if rec == nil || !rec.Open || !rec.Held || !rec.ReadyAnnounced || rec.AnnounceState != "" || rec.MessageID != "" || rec.ThreadID != "" {
		t.Fatalf("v1 record = %+v", rec)
	}
	if !d.Seeded() || !d.Watermark().Equal(t0) {
		t.Fatal("v1 seed/watermark lost")
	}
	if err := d.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"version": 2`) {
		t.Fatalf("saved state is not v2:\n%s", data)
	}
}

func TestThreads_NewerStateVersionIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rfc-notify.json")
	if err := os.WriteFile(path, []byte(`{"version":3,"seeded":true,"issues":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a state written by a newer hive must not be silently downgraded")
	}
}

func TestThreads_ObserveKeepsTheMappingAcrossPolls(t *testing.T) {
	d := NewDetector()
	d.Seed([]Issue{rfc(1, "open")}, t0)
	d.state.Issues["hivecommons/hive#1"].MessageID = "m"
	d.state.Issues["hivecommons/hive#1"].ThreadID = "t"
	d.state.Issues["hivecommons/hive#1"].AnnounceState = StateThreaded
	d.Observe([]Issue{rfc(1, "open", "hold")}, t0.Add(time.Minute))
	rec := d.state.Issues["hivecommons/hive#1"]
	if rec.MessageID != "m" || rec.ThreadID != "t" || rec.AnnounceState != StateThreaded {
		t.Fatalf("mapping dropped by Observe: %+v", rec)
	}
}
