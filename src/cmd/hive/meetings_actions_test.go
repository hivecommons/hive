package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/actions"
)

type fakeFiler struct {
	got []github.CreateIssueRequest
	err error
}

func (f *fakeFiler) CreateIssue(_ context.Context, req github.CreateIssueRequest) (github.CreateIssueResult, error) {
	if f.err != nil {
		return github.CreateIssueResult{}, f.err
	}
	f.got = append(f.got, req)
	return github.CreateIssueResult{Number: 7, URL: "https://github.com/" + req.Repo + "/issues/7"}, nil
}

type fakeDest struct {
	name string
	it   inbox.Item
	c    inbox.Content
	req  inbox.Request
	err  error
}

func (f *fakeDest) Name() string { return f.name }
func (f *fakeDest) Deliver(_ context.Context, it inbox.Item, c inbox.Content, req inbox.Request) (inbox.Result, error) {
	f.it, f.c, f.req = it, c, req
	if f.err != nil {
		return inbox.Result{}, f.err
	}
	return inbox.Result{Note: "handed to agent scout to draft with spek and publish to " + req.Repo}, nil
}

type fakeVault struct {
	title, body string
	tags        []string
	err         error
}

func (f *fakeVault) Upsert(_ context.Context, title, body string, tags []string) error {
	f.title, f.body, f.tags = title, body, tags
	return f.err
}

func meetingDraft(ty actions.Type) actions.Draft {
	return actions.Draft{ID: "act_1", MeetingID: "mtg-1", MeetingTitle: "Weekly sync", Type: ty, Title: "Lobby timeout", Body: "Too short.",
		Repo: "hivecommons/hive", Excerpt: "Ana: too short", DecidedBy: "andy"}
}

func TestMeetingIssueExecutor(t *testing.T) {
	f := &fakeFiler{}
	issue := meetingIssueExecutor(func() meetingIssueFiler { return f }, false)
	res, err := issue.Execute(context.Background(), meetingDraft(actions.TypeIssue))
	if err != nil || res.URL != "https://github.com/hivecommons/hive/issues/7" {
		t.Fatalf("issue %+v %v", res, err)
	}
	req := f.got[0]
	if req.Repo != "hivecommons/hive" || req.Title != "Lobby timeout" || len(req.Labels) != 0 || req.Origin != config.OriginSourceDashboard ||
		!strings.Contains(req.Body, "Too short.") || !strings.Contains(req.Body, "Approved in the dashboard by andy.") {
		t.Fatalf("issue request %+v", req)
	}
	rfc := meetingIssueExecutor(func() meetingIssueFiler { return f }, true)
	if _, err := rfc.Execute(context.Background(), meetingDraft(actions.TypeRFC)); err != nil {
		t.Fatal(err)
	}
	if req := f.got[1]; req.Title != "RFC: Lobby timeout" || len(req.Labels) != 1 || req.Labels[0] != "rfc" {
		t.Fatalf("rfc request %+v", req)
	}
	d := meetingDraft(actions.TypeRFC)
	d.Title = "RFC: already prefixed"
	if _, err := rfc.Execute(context.Background(), d); err != nil || f.got[2].Title != "RFC: already prefixed" {
		t.Fatalf("prefixed %+v %v", f.got[2], err)
	}
	f.err = errors.New("403")
	if _, err := issue.Execute(context.Background(), meetingDraft(actions.TypeIssue)); err == nil {
		t.Fatal("filer error must surface")
	}
	none := meetingIssueExecutor(func() meetingIssueFiler { return nil }, false)
	if _, err := none.Execute(context.Background(), meetingDraft(actions.TypeIssue)); !errors.Is(err, errNoGitHub) {
		t.Fatalf("no GitHub = %v", err)
	}
}

func TestMeetingSpekAndKnowledgeExecutors(t *testing.T) {
	dest := &fakeDest{name: inbox.DestSpekRFC}
	res, err := meetingSpekExecutor(dest).Execute(context.Background(), meetingDraft(actions.TypeSpek))
	if err != nil || !strings.Contains(res.Note, "handed to agent scout") {
		t.Fatalf("spek %+v %v", res, err)
	}
	if dest.req.Destination != inbox.DestSpekRFC || dest.req.Repo != "hivecommons/hive" || dest.it.Source != meetingDraftSource ||
		dest.it.ID != "act_1" || dest.c.Provenance != "hive://meetings/mtg-1/drafts/act_1" || !strings.Contains(dest.c.Text(), "Too short.") {
		t.Fatalf("spek delivery %+v %+v %+v", dest.req, dest.it, dest.c)
	}
	dest.err = errors.New("no agent")
	if _, err := meetingSpekExecutor(dest).Execute(context.Background(), meetingDraft(actions.TypeSpek)); err == nil {
		t.Fatal("delivery error must surface")
	}
	if _, err := meetingSpekExecutor(nil).Execute(context.Background(), meetingDraft(actions.TypeSpek)); err == nil {
		t.Fatal("no destination must fail")
	}
	if spekDestination([]inbox.Destination{&fakeDest{name: inbox.DestDibs}, dest}) != dest || spekDestination(nil) != nil {
		t.Fatal("spekDestination")
	}

	v := &fakeVault{}
	res, err = meetingKnowledgeExecutor(v).Execute(context.Background(), meetingDraft(actions.TypeKnowledge))
	if err != nil || res.Note != "saved to the meetings vault" || v.title != "Lobby timeout" || !strings.Contains(v.body, "Too short.") ||
		strings.Join(v.tags, ",") != "meetings,meeting-draft,mtg-1" {
		t.Fatalf("knowledge %+v %v %+v", res, err, v)
	}
	v.err = errors.New("vault down")
	if _, err := meetingKnowledgeExecutor(v).Execute(context.Background(), meetingDraft(actions.TypeKnowledge)); err == nil {
		t.Fatal("vault error must surface")
	}
	if _, err := meetingKnowledgeExecutor(nil).Execute(context.Background(), meetingDraft(actions.TypeKnowledge)); err == nil {
		t.Fatal("no vault must fail")
	}
}

func TestMeetingsActionsError(t *testing.T) {
	cases := map[error]error{
		fmt.Errorf("%w: x", actions.ErrNotFound): dashboard.ErrMeetingNotFound,
		fmt.Errorf("%w: x", actions.ErrInvalid):  dashboard.ErrMeetingInvalid,
		fmt.Errorf("%w: x", actions.ErrConflict): dashboard.ErrMeetingConflict,
		fmt.Errorf("%w: x", actions.ErrDisabled): dashboard.ErrMeetingActionsDisabled,
	}
	for in, want := range cases {
		if got := meetingsActionsError(in); !errors.Is(got, want) {
			t.Errorf("%v mapped to %v", in, got)
		}
	}
	if meetingsActionsError(nil) != nil {
		t.Fatal("nil mapped")
	}
	other := errors.New("disk full")
	if meetingsActionsError(other) != other {
		t.Fatal("unknown error rewrapped")
	}
}

func TestMeetingsActionSettings(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive", "spektacular"}}}
	s := meetingsActionSettings(cfg)
	if !s.Enabled || s.DefaultRepo != "hivecommons/hive" || len(s.Repos) != 2 || len(s.Types) != 4 || s.AutoApproves(actions.TypeKnowledge) {
		t.Fatalf("defaults %+v", s)
	}
	off := false
	cfg.Meetings.Persona.Enabled = &off
	if meetingsActionSettings(cfg).Enabled {
		t.Fatal("persona off turns actions off")
	}
}

func TestMeetingsActionsAdapterAndWiring(t *testing.T) {
	if newMeetingsActions(meetingsActionsDeps{}) != nil {
		t.Fatal("nil deps must not build a queue")
	}
	dir := t.TempDir()
	store, err := meetings.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: meetings.NewClient("http://127.0.0.1:1", "t", nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	cfg := &config.Config{
		Meetings:  config.MeetingsConfig{Enabled: true},
		Project:   config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive"}},
		Dashboard: config.DashboardConfig{PublicURL: "https://hive.example/"},
	}
	logger := slog.New(slog.DiscardHandler)
	runner := wireMeetingsLive(cfg, mgr, store, &knowledge.KnowledgeAPI{}, nil, nil, logger, nil)
	if runner == nil {
		t.Fatal("want a live runner")
	}
	defer runner.Close()
	b := &boot{cfg: cfg, logger: logger, dashSrv: dashboard.NewServer(0, logger), meetingsLive: runner}
	b.wireMeetingsActions(store)
	if len(b.cleanup.fns) != 1 {
		t.Fatalf("cleanup %d", len(b.cleanup.fns))
	}

	filer := &fakeFiler{}
	svc := newMeetingsActions(meetingsActionsDeps{cfg: cfg, store: store, gh: func() meetingIssueFiler { return filer }, logger: logger})
	if svc == nil {
		t.Fatal("want a queue")
	}
	sink := meetingDraftSinks(svc, cfg.Dashboard.PublicURL)(meetings.Meeting{ID: "mtg-1", Title: " Weekly sync "})
	d, err := sink.Submit(actions.Draft{Type: actions.TypeIssue, Title: "Lobby timeout", Body: "Too short."})
	if err != nil || d.MeetingLink != "https://hive.example/#section-meetings-section" || d.MeetingTitle != "Weekly sync" || d.Repo != "hivecommons/hive" {
		t.Fatalf("draft %+v %v", d, err)
	}
	if sink := meetingDraftSinks(svc, "")(meetings.Meeting{ID: "m"}); sink == nil {
		t.Fatal("sink without a public URL")
	}

	a := &meetingsActions{svc: svc}
	list, _ := a.List()
	if l := list.(meetingsActionsList); l.Pending != 1 || len(l.Drafts) != 1 {
		t.Fatalf("list %+v", l)
	}
	title := "Lobby timeout is too short"
	if _, err := a.Edit(context.Background(), d.ID, dashboard.MeetingActionEditRequest{Title: &title}, "bo"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Edit(context.Background(), "nope", dashboard.MeetingActionEditRequest{}, "bo"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("edit unknown = %v", err)
	}
	if len(filer.got) != 0 {
		t.Fatal("nothing is filed before approval")
	}
	out, err := a.Act(context.Background(), d.ID, dashboard.MeetingActionApprove, "andy")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(actions.Draft); got.Status != actions.StatusDone || len(filer.got) != 1 || filer.got[0].Title != title {
		t.Fatalf("approved %+v %+v", got, filer.got)
	}
	if _, err := a.Act(context.Background(), d.ID, dashboard.MeetingActionReject, "andy"); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("reject done = %v", err)
	}
	if _, err := a.Act(context.Background(), d.ID, "explode", "andy"); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("unknown action = %v", err)
	}
	d2, _ := sink.Submit(actions.Draft{Type: actions.TypeIssue, Title: "Second"})
	if out, err := a.Act(context.Background(), d2.ID, dashboard.MeetingActionReject, "andy"); err != nil || out.(actions.Draft).Status != actions.StatusRejected {
		t.Fatalf("reject %+v %v", out, err)
	}
	if len(filer.got) != 1 {
		t.Fatal("a rejected draft is never filed")
	}
}

func TestKickAgentWithoutRuntime(t *testing.T) {
	b := &boot{}
	if err := b.kickAgent("scout", "hi"); err == nil {
		t.Fatal("no agent runtime must fail")
	}
}
