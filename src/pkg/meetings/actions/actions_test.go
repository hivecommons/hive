package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

type recorder struct {
	mu    sync.Mutex
	got   []Draft
	res   Result
	err   error
	block chan struct{}
}

func (r *recorder) Execute(ctx context.Context, d Draft) (Result, error) {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Result{}, r.err
	}
	r.got = append(r.got, d)
	return r.res, nil
}

func (r *recorder) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

type announcer struct {
	mu    sync.Mutex
	texts []string
	where string
	err   error
}

func (a *announcer) Announce(_ context.Context, id, text string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return "", a.err
	}
	a.texts = append(a.texts, id+": "+text)
	return a.where, nil
}

type fixture struct {
	svc   *Service
	exec  map[Type]*recorder
	ann   *announcer
	st    Settings
	mu    sync.Mutex
	now   time.Time
	ids   int
	opts  Options
	clock func() time.Time
}

func defaultSettings() Settings {
	return Settings{
		Enabled: true, Types: []Type{TypeIssue, TypeRFC, TypeSpek, TypeKnowledge},
		DefaultRepo: "hivecommons/hive", Repos: []string{"hivecommons/hive", "hivecommons/spektacular"},
		AutoApprove: map[Type]bool{},
	}
}

func newFixture(t *testing.T, dir string) *fixture {
	t.Helper()
	f := &fixture{exec: map[Type]*recorder{}, ann: &announcer{where: "chat"}, st: defaultSettings(),
		now: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	execs := map[Type]Executor{}
	for _, ty := range []Type{TypeIssue, TypeRFC, TypeSpek, TypeKnowledge} {
		r := &recorder{res: Result{URL: "https://github.com/hivecommons/hive/issues/7"}}
		f.exec[ty], execs[ty] = r, r
	}
	f.clock = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
	f.opts = Options{
		Dir: dir, Executors: execs, Announce: f.ann, Now: f.clock,
		Settings: func() Settings { f.mu.Lock(); defer f.mu.Unlock(); return f.st },
		NewID:    func() string { f.mu.Lock(); defer f.mu.Unlock(); f.ids++; return fmt.Sprintf("act_%d", f.ids) },
	}
	svc, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	t.Cleanup(svc.Wait)
	return f
}

func (f *fixture) set(fn func(*Settings)) { f.mu.Lock(); fn(&f.st); f.mu.Unlock() }
func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func draft(ty Type) Draft {
	return Draft{MeetingID: "mtg-1", Type: ty, Title: "Lobby timeout", Body: "It gives up too soon.", Excerpt: "Ana: it gives up", RequestedBy: "Ana"}
}

func TestSubmitWaitsForApproval(t *testing.T) {
	f := newFixture(t, t.TempDir())
	for _, ty := range []Type{TypeIssue, TypeRFC, TypeSpek, TypeKnowledge} {
		d, err := f.svc.Submit(draft(ty))
		if err != nil || d.Status != StatusPending || d.AutoApproved || d.ID == "" {
			t.Fatalf("%s: %+v %v", ty, d, err)
		}
	}
	for ty, r := range f.exec {
		if r.n() != 0 {
			t.Fatalf("%s created without approval", ty)
		}
	}
	if n := len(f.svc.List()); n != 4 {
		t.Fatalf("list %d", n)
	}
}

func TestSubmitValidation(t *testing.T) {
	f := newFixture(t, "")
	if _, err := f.svc.Submit(Draft{Type: TypeIssue, Title: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no meeting = %v", err)
	}
	if _, err := f.svc.Submit(Draft{MeetingID: "m", Type: TypeIssue, Title: "  "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no title = %v", err)
	}
	f.set(func(s *Settings) { s.Types = []Type{TypeIssue} })
	if _, err := f.svc.Submit(draft(TypeRFC)); !errors.Is(err, ErrDisabled) {
		t.Fatalf("type off = %v", err)
	}
	f.set(func(s *Settings) { s.Enabled = false })
	if _, err := f.svc.Submit(draft(TypeIssue)); !errors.Is(err, ErrDisabled) {
		t.Fatalf("off = %v", err)
	}
	if _, err := f.svc.Approve(context.Background(), "x", "o"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("approve while off = %v", err)
	}
}

func TestSubmitRepoAndCleaning(t *testing.T) {
	f := newFixture(t, "")
	d := draft(TypeIssue)
	d.Repo = "evil/repo"
	d.Title = "Ping   @maintainer\nnow " + strings.Repeat("x", MaxTitleRunes)
	d.Body = "cc @octocat, mail me at a@b.co"
	got, err := f.svc.Submit(d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "hivecommons/hive" {
		t.Fatalf("unmonitored repo kept: %q", got.Repo)
	}
	if strings.Contains(got.Title, "@") || len([]rune(got.Title)) != MaxTitleRunes || strings.Contains(got.Title, "\n") {
		t.Fatalf("title %q", got.Title)
	}
	if got.Body != "cc octocat, mail me at a@b.co" {
		t.Fatalf("body %q", got.Body)
	}
	d.Repo = "HiveCommons/Spektacular"
	if got, _ := f.svc.Submit(d); got.Repo != "hivecommons/spektacular" {
		t.Fatalf("monitored repo %q", got.Repo)
	}
	k := draft(TypeKnowledge)
	k.Repo = "hivecommons/hive"
	if got, _ := f.svc.Submit(k); got.Repo != "" {
		t.Fatalf("knowledge repo %q", got.Repo)
	}
	// Without a default the first monitored repo is used; with neither, none.
	s := defaultSettings()
	s.DefaultRepo = ""
	if r := s.ResolveRepo(TypeIssue, ""); r != "hivecommons/hive" {
		t.Fatal(r)
	}
	s.Repos = nil
	if r := s.ResolveRepo(TypeIssue, "x/y"); r != "" {
		t.Fatal(r)
	}
}

func TestApproveCreatesAndAnnounces(t *testing.T) {
	f := newFixture(t, t.TempDir())
	d, _ := f.svc.Submit(draft(TypeIssue))
	got, err := f.svc.Approve(context.Background(), d.ID, "andy")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.ResultURL == "" || got.DecidedBy != "andy" || got.DecidedAt == nil || got.Announced != "chat" {
		t.Fatalf("approved %+v", got)
	}
	if f.exec[TypeIssue].n() != 1 {
		t.Fatal("executor not called once")
	}
	if len(f.ann.texts) != 1 || !strings.Contains(f.ann.texts[0], "mtg-1: Approved: an issue \"Lobby timeout\" is open: https://") {
		t.Fatalf("announced %v", f.ann.texts)
	}
	if _, err := f.svc.Approve(context.Background(), d.ID, "andy"); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-approve = %v", err)
	}
	if _, err := f.svc.Reject(d.ID, "andy"); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject done = %v", err)
	}
	if _, err := f.svc.Approve(context.Background(), "nope", "andy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
}

func TestApproveFailureCanRetry(t *testing.T) {
	f := newFixture(t, "")
	f.exec[TypeRFC].err = errors.New("GitHub said no")
	d, _ := f.svc.Submit(draft(TypeRFC))
	got, err := f.svc.Approve(context.Background(), d.ID, "andy")
	if err != nil || got.Status != StatusFailed || got.Error != "GitHub said no" || len(f.ann.texts) != 0 {
		t.Fatalf("failed %+v %v %v", got, err, f.ann.texts)
	}
	f.exec[TypeRFC].err = nil
	if got, _ := f.svc.Approve(context.Background(), d.ID, "andy"); got.Status != StatusDone || got.Error != "" {
		t.Fatalf("retry %+v", got)
	}
}

func TestApproveWithoutExecutorOrRepo(t *testing.T) {
	f := newFixture(t, "")
	delete(f.svc.opts.Executors, TypeSpek)
	d, _ := f.svc.Submit(draft(TypeSpek))
	if got, _ := f.svc.Approve(context.Background(), d.ID, "o"); got.Status != StatusFailed || !strings.Contains(got.Error, "cannot be created") {
		t.Fatalf("no executor %+v", got)
	}
	f.set(func(s *Settings) { s.DefaultRepo, s.Repos = "", nil })
	d, _ = f.svc.Submit(draft(TypeIssue))
	if _, err := f.svc.Approve(context.Background(), d.ID, "o"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no repo = %v", err)
	}
}

func TestAnnounceFailureKeepsResult(t *testing.T) {
	f := newFixture(t, "")
	f.ann.err = errors.New("chat down")
	d, _ := f.svc.Submit(draft(TypeIssue))
	if got, _ := f.svc.Approve(context.Background(), d.ID, "o"); got.Status != StatusDone || got.Announced != "" {
		t.Fatalf("announce failure %+v", got)
	}
	f.svc.SetAnnouncer(nil)
	d, _ = f.svc.Submit(draft(TypeIssue))
	if got, _ := f.svc.Approve(context.Background(), d.ID, "o"); got.Status != StatusDone || got.Announced != "" {
		t.Fatalf("no announcer %+v", got)
	}
}

func TestRejectAndEdit(t *testing.T) {
	f := newFixture(t, "")
	d, _ := f.svc.Submit(draft(TypeIssue))
	title, body, repo, ty := "Lobby timeout is 10 minutes", "Raise it to 20.", "hivecommons/spektacular", "rfc"
	got, err := f.svc.Edit(d.ID, Edit{Title: &title, Body: &body, Repo: &repo, Type: &ty}, "bo")
	if err != nil || got.Title != title || got.Body != body || got.Repo != repo || got.Type != TypeRFC || got.EditedBy != "bo" {
		t.Fatalf("edit %+v %v", got, err)
	}
	bad := "evil/repo"
	if _, err := f.svc.Edit(d.ID, Edit{Repo: &bad}, "bo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad repo = %v", err)
	}
	unknown := "pr"
	if _, err := f.svc.Edit(d.ID, Edit{Type: &unknown}, "bo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad type = %v", err)
	}
	empty := " "
	if _, err := f.svc.Edit(d.ID, Edit{Title: &empty}, "bo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title = %v", err)
	}
	k := "knowledge"
	if got, _ := f.svc.Edit(d.ID, Edit{Type: &k}, "bo"); got.Repo != "" {
		t.Fatalf("knowledge keeps no repo: %+v", got)
	}
	issue := "issue"
	if got, _ := f.svc.Edit(d.ID, Edit{Type: &issue}, "bo"); got.Repo != "hivecommons/hive" {
		t.Fatalf("issue gets the default repo back: %+v", got)
	}
	f.set(func(s *Settings) { s.Types = []Type{TypeIssue} })
	if _, err := f.svc.Edit(d.ID, Edit{Type: &k}, "bo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("type off = %v", err)
	}
	if _, err := f.svc.Edit("nope", Edit{}, "bo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	got, err = f.svc.Reject(d.ID, "andy")
	if err != nil || got.Status != StatusRejected || got.DecidedBy != "andy" {
		t.Fatalf("reject %+v %v", got, err)
	}
	if _, err := f.svc.Edit(d.ID, Edit{Title: &title}, "bo"); !errors.Is(err, ErrConflict) {
		t.Fatalf("edit rejected = %v", err)
	}
	if _, err := f.svc.Approve(context.Background(), d.ID, "andy"); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve rejected = %v", err)
	}
	if _, err := f.svc.Reject("nope", "andy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reject unknown = %v", err)
	}
	for _, r := range f.exec {
		if r.n() != 0 {
			t.Fatal("reject or edit created something")
		}
	}
	if g, err := f.svc.Get(d.ID); err != nil || g.Status != StatusRejected {
		t.Fatalf("get %+v %v", g, err)
	}
	if _, err := f.svc.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAutoApproveOnlyKnowledge(t *testing.T) {
	f := newFixture(t, "")
	f.set(func(s *Settings) {
		s.AutoApprove = map[Type]bool{TypeIssue: true, TypeRFC: true, TypeSpek: true, TypeKnowledge: true}
	})
	for _, ty := range []Type{TypeIssue, TypeRFC, TypeSpek} {
		if d, _ := f.svc.Submit(draft(ty)); d.Status != StatusPending || d.AutoApproved {
			t.Fatalf("%s auto-approved: %+v", ty, d)
		}
	}
	d, err := f.svc.Submit(draft(TypeKnowledge))
	if err != nil || !d.AutoApproved || d.Status != StatusApproving || d.DecidedBy != AutoApprover {
		t.Fatalf("knowledge %+v %v", d, err)
	}
	f.svc.Wait()
	got, _ := f.svc.Get(d.ID)
	if got.Status != StatusDone || f.exec[TypeKnowledge].n() != 1 {
		t.Fatalf("auto note %+v", got)
	}
	for _, ty := range []Type{TypeIssue, TypeRFC, TypeSpek} {
		if f.exec[ty].n() != 0 {
			t.Fatalf("%s created without approval", ty)
		}
	}
}

func TestPersistenceAndRestart(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, dir)
	pending, _ := f.svc.Submit(draft(TypeIssue))
	block := make(chan struct{})
	f.exec[TypeRFC].block = block
	rfc, _ := f.svc.Submit(draft(TypeRFC))
	done := make(chan struct{})
	go func() { _, _ = f.svc.Approve(context.Background(), rfc.ID, "o"); close(done) }()
	// Wait until the approval is recorded as in flight, then "restart".
	testutil.Eventually(t, 5*time.Second, func() bool {
		g, _ := f.svc.Get(rfc.ID)
		return g.Status == StatusApproving
	}, "approval never started")
	if fi, err := os.Stat(filepath.Join(dir, FileName)); err != nil || fi.Mode().Perm() != fileMode {
		t.Fatalf("file %v %v", fi, err)
	}
	again, err := New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := again.Get(pending.ID); g.Status != StatusPending {
		t.Fatalf("pending after restart %+v", g)
	}
	if g, _ := again.Get(rfc.ID); g.Status != StatusFailed || g.Error != interruptedError {
		t.Fatalf("interrupted after restart %+v", g)
	}
	close(block)
	<-done
	// A damaged file is an error, never replaced.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f.opts); err == nil {
		t.Fatal("a damaged queue must not open")
	}
}

func TestPruneDecided(t *testing.T) {
	f := newFixture(t, t.TempDir())
	old, _ := f.svc.Submit(draft(TypeIssue))
	if _, err := f.svc.Reject(old.ID, "o"); err != nil {
		t.Fatal(err)
	}
	f.advance(decidedRetention + time.Hour)
	waiting, _ := f.svc.Submit(draft(TypeIssue))
	if _, err := f.svc.Get(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("an old decided draft must be pruned")
	}
	if _, err := f.svc.Get(waiting.ID); err != nil {
		t.Fatal("a pending draft is never pruned")
	}
	for i := 0; i < keepDecided+2; i++ {
		f.advance(time.Second)
		d, _ := f.svc.Submit(draft(TypeIssue))
		_, _ = f.svc.Reject(d.ID, "o")
	}
	n := 0
	for _, d := range f.svc.List() {
		if d.Status == StatusRejected {
			n++
		}
	}
	if n > keepDecided {
		t.Fatalf("kept %d decided", n)
	}
}

func TestNewRequiresSettingsAndBadDir(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("settings are required")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Dir: filepath.Join(file, "sub"), Settings: defaultSettings}); err == nil {
		t.Fatal("an unusable dir must fail")
	}
	s, err := New(Options{Settings: defaultSettings})
	if err != nil || s.Settings().DefaultRepo != "hivecommons/hive" {
		t.Fatal(err)
	}
	if id := randomID(); !strings.HasPrefix(id, "act_") || len(id) != len("act_")+16 {
		t.Fatal(id)
	}
}

func TestMeetingSink(t *testing.T) {
	f := newFixture(t, "")
	sink := f.svc.ForMeeting("mtg-9", "Retro", "https://hive.example/#section-meetings-section")
	if !sink.Settings().Allows(TypeIssue) {
		t.Fatal("settings")
	}
	d, err := sink.Submit(Draft{MeetingID: "spoofed", Type: TypeIssue, Title: "x"})
	if err != nil || d.MeetingID != "mtg-9" || d.MeetingTitle != "Retro" || d.MeetingLink == "" {
		t.Fatalf("sink %+v %v", d, err)
	}
}

func TestTypesAndSettings(t *testing.T) {
	if _, ok := ParseType("PR"); ok {
		t.Fatal("unknown type parsed")
	}
	if ty, ok := ParseType(" RFC "); !ok || ty != TypeRFC {
		t.Fatal(ty)
	}
	want := map[Type][2]string{
		TypeIssue: {"issue", "an issue"}, TypeRFC: {"RFC", "an RFC"},
		TypeSpek: {"Spek session", "a Spek session"}, TypeKnowledge: {"knowledge note", "a knowledge note"},
		Type("x"): {"x", "a x"},
	}
	for ty, w := range want {
		if ty.Noun() != w[0] || ty.WithArticle() != w[1] {
			t.Errorf("%s: %q %q", ty, ty.Noun(), ty.WithArticle())
		}
	}
	r := config.ResolvedMeetingsActions{Enabled: true, Types: []string{"issue", "bogus"}, AutoApprove: map[string]bool{"knowledge": true, "bogus": true}}
	s := SettingsFrom(r, "a/b", []string{"a/b"})
	if !s.Allows(TypeIssue) || s.Allows(TypeRFC) || !s.AutoApproves(TypeKnowledge) || s.AutoApproves(TypeIssue) {
		t.Fatalf("settings %+v", s)
	}
	s.Enabled = false
	if s.Allows(TypeIssue) {
		t.Fatal("disabled allows")
	}
	if _, ok := s.knownRepo(""); ok {
		t.Fatal("empty repo known")
	}
}

func TestResultTextAndBody(t *testing.T) {
	d := draft(TypeIssue)
	d.ResultURL = "https://x/1"
	if got := ResultText(d); got != `Approved: an issue "Lobby timeout" is open: https://x/1` {
		t.Fatal(got)
	}
	d.ResultURL = ""
	if got := ResultText(d); !strings.Contains(got, "was created") {
		t.Fatal(got)
	}
	d.Type = TypeSpek
	d.ResultNote = "handed to agent scout to draft with spek and publish to a/b"
	if got := ResultText(d); got != `Approved: the Spek session on "Lobby timeout" was handed to agent scout to draft with spek and publish to a/b.` {
		t.Fatal(got)
	}
	d.ResultNote = ""
	if got := ResultText(d); !strings.Contains(got, "handed to a hive agent") {
		t.Fatal(got)
	}
	d.ResultURL = "https://x/2"
	if got := ResultText(d); !strings.Contains(got, "started: https://x/2") {
		t.Fatal(got)
	}
	d.Type = TypeKnowledge
	if got := ResultText(d); !strings.Contains(got, "Saved the knowledge note") || !strings.Contains(got, "https://x/2") {
		t.Fatal(got)
	}
	d.ResultURL = ""
	if got := ResultText(d); got != `Saved the knowledge note "Lobby timeout" to the hive.` {
		t.Fatal(got)
	}

	b := draft(TypeIssue)
	b.MeetingTitle, b.MeetingLink, b.DecidedBy = "Weekly sync", "https://hive.example/#m", "andy"
	b.Excerpt = "Ana: run ```rm -rf```"
	body := RenderBody(b)
	for _, want := range []string{"It gives up too soon.", "[Weekly sync](https://hive.example/#m)", "asked for by Ana (meeting display name)",
		"Approved in the dashboard by andy.", "```text\nAna: run '''rm -rf'''\n```"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	b.MeetingTitle, b.MeetingLink, b.Excerpt, b.RequestedBy, b.AutoApproved = "", "", "", "", true
	body = RenderBody(b)
	if !strings.Contains(body, "during meeting mtg-1.") || !strings.Contains(body, "Auto-approved.") || strings.Contains(body, "Transcript excerpt") {
		t.Fatal(body)
	}
	if got := clipRunes("abcdef", 4); got != "abc…" {
		t.Fatal(got)
	}
}

func TestStoreFileShape(t *testing.T) {
	dir := t.TempDir()
	f := newFixture(t, dir)
	_, _ = f.svc.Submit(draft(TypeIssue))
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	var sf storeFile
	if err := json.Unmarshal(data, &sf); err != nil || sf.Version != fileVersion || len(sf.Drafts) != 1 {
		t.Fatalf("file %s %v", data, err)
	}
	if err := writeAtomic(filepath.Join(dir, "missing", FileName), data); err == nil {
		t.Fatal("writing into a missing dir must fail")
	}
}
