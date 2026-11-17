package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/inbox"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/actions"
	"github.com/hivecommons/hive/pkg/meetings/betty"
	"github.com/hivecommons/hive/pkg/meetings/live"
	"github.com/hivecommons/hive/pkg/rfcnotify"
)

// Meeting action requests (hivecommons/hive#18406): drafts people ask the
// persona for in a meeting wait in the approval queue (pkg/meetings/actions)
// until an owner or read-write user approves them in the dashboard. Then an
// issue or RFC is filed through the hive's GitHub issue path, a Spek session
// goes to the Inbox spek_rfc destination, and a knowledge note is written
// to the meetings vault the way meeting notes are.

const (
	// meetingsSectionFragment is the dashboard hash of the Meetings section.
	meetingsSectionFragment = "/#section-meetings-section"
	// meetingDraftSource is the inbox item source of a Spek session draft.
	meetingDraftSource = "meetings"
	// meetingRFCTitlePrefix is the RFC title convention rfcnotify reads.
	meetingRFCTitlePrefix = "RFC: "
)

// meetingDraftTags tag a knowledge note drafted in a meeting.
var meetingDraftTags = []string{"meetings", "meeting-draft"}

// meetingsActions implements dashboard.MeetingsActionsService.
type meetingsActions struct{ svc *actions.Service }

// meetingsActionsList is GET /api/meetings/actions.
type meetingsActionsList struct {
	Drafts  []actions.Draft `json:"drafts"`
	Pending int             `json:"pending"`
}

func meetingsActionsError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, actions.ErrNotFound):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingNotFound, err)
	case errors.Is(err, actions.ErrInvalid):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingInvalid, err)
	case errors.Is(err, actions.ErrConflict):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingConflict, err)
	case errors.Is(err, actions.ErrDisabled):
		return fmt.Errorf("%w: %v", dashboard.ErrMeetingActionsDisabled, err)
	default:
		return err
	}
}

func (m *meetingsActions) List() (any, error) {
	out := meetingsActionsList{Drafts: m.svc.List()}
	for _, d := range out.Drafts {
		if d.Status == actions.StatusPending {
			out.Pending++
		}
	}
	return out, nil
}

func (m *meetingsActions) Edit(_ context.Context, id string, req dashboard.MeetingActionEditRequest, by string) (any, error) {
	d, err := m.svc.Edit(id, actions.Edit{Type: req.Type, Title: req.Title, Body: req.Body, Repo: req.Repo}, by)
	if err != nil {
		return nil, meetingsActionsError(err)
	}
	return d, nil
}

func (m *meetingsActions) Act(ctx context.Context, id, action, by string) (any, error) {
	var (
		d   actions.Draft
		err error
	)
	switch action {
	case dashboard.MeetingActionApprove:
		d, err = m.svc.Approve(ctx, id, by)
	case dashboard.MeetingActionReject:
		d, err = m.svc.Reject(id, by)
	default:
		return nil, fmt.Errorf("%w: unknown action %q", dashboard.ErrMeetingInvalid, action)
	}
	if err != nil {
		return nil, meetingsActionsError(err)
	}
	return d, nil
}

// meetingsActionSettings is the queue's settings, read from the live config
// on every call so a saved change applies at once. With the persona off no
// draft is ever made, and approvals stop too.
func meetingsActionSettings(cfg *config.Config) actions.Settings {
	r := cfg.Meetings.Persona.Actions.Resolve()
	if !cfg.Meetings.Persona.IsEnabled() {
		r.Enabled = false
	}
	return actions.SettingsFrom(r, cfg.MeetingsActionDefaultRepo(), cfg.MeetingsActionRepos())
}

// meetingIssueFiler is the slice of *github.Client the issue executor uses.
type meetingIssueFiler interface {
	CreateIssue(ctx context.Context, req github.CreateIssueRequest) (github.CreateIssueResult, error)
}

// errNoGitHub is returned when the hive has no GitHub client.
var errNoGitHub = errors.New("GitHub is not connected on this hive, so the issue cannot be filed")

// meetingIssueExecutor files an approved issue or RFC as the hive's App
// through the one issue path every hive filer uses (provenance label,
// exact-title dedupe, secret scrubbing). An RFC follows the convention
// rfcnotify reads: an "RFC: " title and the rfc label.
func meetingIssueExecutor(gh func() meetingIssueFiler, rfc bool) actions.Executor {
	return actions.ExecutorFunc(func(ctx context.Context, d actions.Draft) (actions.Result, error) {
		c := gh()
		if c == nil {
			return actions.Result{}, errNoGitHub
		}
		title := d.Title
		var labels []string
		if rfc {
			if !rfcnotify.IsRFC(title, nil) {
				title = meetingRFCTitlePrefix + title
			}
			labels = []string{rfcnotify.RFCLabel}
		}
		res, err := c.CreateIssue(ctx, github.CreateIssueRequest{ // writeback:solicited an owner or read-write user approved this meeting draft in the dashboard
			Repo: d.Repo, Title: title, Body: actions.RenderBody(d), Labels: labels, Origin: config.OriginSourceDashboard,
		})
		if err != nil {
			return actions.Result{}, err
		}
		return actions.Result{URL: res.URL}, nil
	})
}

// meetingSpekExecutor hands an approved Spek session to the Inbox spek_rfc
// destination: a hive agent drafts the spec with spektacular and publishes
// the RFC to the draft's repo.
func meetingSpekExecutor(dest inbox.Destination) actions.Executor {
	return actions.ExecutorFunc(func(ctx context.Context, d actions.Draft) (actions.Result, error) {
		if dest == nil {
			return actions.Result{}, errors.New("the Inbox spek_rfc destination is not available on this hive")
		}
		it := inbox.Item{Source: meetingDraftSource, ID: d.ID, Title: d.Title}
		c := inbox.Content{
			Title: d.Title, Summary: actions.RenderBody(d),
			Provenance: "hive://meetings/" + d.MeetingID + "/drafts/" + d.ID,
		}
		res, err := dest.Deliver(ctx, it, c, inbox.Request{Destination: inbox.DestSpekRFC, Repo: d.Repo})
		if err != nil {
			return actions.Result{}, err
		}
		return actions.Result{URL: res.URL, Note: res.Note}, nil
	})
}

// meetingKnowledgeExecutor writes an approved knowledge note to the
// meetings vault through the same KnowledgeAPI write as meeting notes.
func meetingKnowledgeExecutor(notes live.NotesSink) actions.Executor {
	return actions.ExecutorFunc(func(ctx context.Context, d actions.Draft) (actions.Result, error) {
		if notes == nil {
			return actions.Result{}, errors.New("the knowledge base is not available on this hive")
		}
		tags := append(append([]string{}, meetingDraftTags...), d.MeetingID)
		if err := notes.Upsert(ctx, d.Title, actions.RenderBody(d), tags); err != nil {
			return actions.Result{}, err
		}
		return actions.Result{Note: "saved to the " + live.DefaultVault + " vault"}, nil
	})
}

// spekDestination picks the spek_rfc destination.
func spekDestination(dests []inbox.Destination) inbox.Destination {
	for _, d := range dests {
		if d != nil && d.Name() == inbox.DestSpekRFC {
			return d
		}
	}
	return nil
}

// meetingDraftSinks gives each meeting's persona its sink: drafts carry the
// meeting's title and its dashboard link.
func meetingDraftSinks(svc *actions.Service, publicURL string) live.ActionSinkFactory {
	link := ""
	if base := strings.TrimRight(strings.TrimSpace(publicURL), "/"); base != "" {
		link = base + meetingsSectionFragment
	}
	return func(m meetings.Meeting) betty.ActionSink {
		return svc.ForMeeting(m.ID, strings.TrimSpace(m.Title), link)
	}
}

// meetingsActionsDeps are the collaborators the queue is wired with.
type meetingsActionsDeps struct {
	cfg    *config.Config
	store  *meetings.Store
	runner *live.Runner
	gh     func() meetingIssueFiler
	spek   inbox.Destination
	notes  live.NotesSink
	logger *slog.Logger
}

// newMeetingsActions opens the queue (<meetings.data_dir>/actions.json)
// and connects it to the live runner. It returns nil (logged) when the
// queue file cannot be opened.
func newMeetingsActions(d meetingsActionsDeps) *actions.Service {
	if d.cfg == nil || d.store == nil {
		return nil
	}
	cfg := d.cfg
	svc, err := actions.New(actions.Options{
		Dir: d.store.Dir(),
		Executors: map[actions.Type]actions.Executor{
			actions.TypeIssue:     meetingIssueExecutor(d.gh, false),
			actions.TypeRFC:       meetingIssueExecutor(d.gh, true),
			actions.TypeSpek:      meetingSpekExecutor(d.spek),
			actions.TypeKnowledge: meetingKnowledgeExecutor(d.notes),
		},
		Settings: func() actions.Settings { return meetingsActionSettings(cfg) },
		Logger:   d.logger,
	})
	if err != nil {
		if d.logger != nil {
			d.logger.Error("[meetings] meeting drafts unavailable; action requests are off", "error", err)
		}
		return nil
	}
	if d.runner != nil {
		svc.SetAnnouncer(d.runner)
		d.runner.SetPersonaActions(meetingDraftSinks(svc, cfg.Dashboard.PublicURL))
	}
	return svc
}

// wireMeetingsActions starts the meeting drafts queue and installs it in
// the dashboard.
func (b *boot) wireMeetingsActions(store *meetings.Store) {
	var notes live.NotesSink
	if b.knowledgeAPI != nil {
		notes = live.KnowledgeNotes{API: b.knowledgeAPI}
	}
	svc := newMeetingsActions(meetingsActionsDeps{
		cfg: b.cfg, store: store, runner: b.meetingsLive, logger: b.logger, notes: notes,
		gh: func() meetingIssueFiler {
			if b.ghClient == nil {
				return nil
			}
			return b.ghClient
		},
		spek: spekDestination(dashboard.NewInboxDestinations(b.cfg, b.kickAgent)),
	})
	if svc != nil {
		b.cleanup.push(svc.Wait)
		b.dashSrv.SetMeetingsActionsService(&meetingsActions{svc: svc})
	}
}

// kickAgent sends a prompt to a named agent (the Inbox's kick path).
func (b *boot) kickAgent(agent, prompt string) error {
	if b.agentMgr == nil {
		return fmt.Errorf("agent runtime is not available")
	}
	started, err := b.agentMgr.SendKickAsync(agent, prompt)
	if err != nil {
		return err
	}
	if !started {
		return fmt.Errorf("agent %s did not accept the kick", agent)
	}
	return nil
}
