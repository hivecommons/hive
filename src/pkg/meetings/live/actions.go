package live

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
)

// Meeting action requests (hivecommons/hive#18406): the persona's drafts
// go to an approval queue (pkg/meetings/actions); once one is approved its
// result is announced back to the meeting through the runner.

// ActionSinkFactory builds one meeting's draft sink; nil leaves action
// requests off for that meeting.
type ActionSinkFactory func(m meetings.Meeting) betty.ActionSink

// actionsHook holds the factory; it is set after the runner is built.
type actionsHook struct {
	mu sync.Mutex
	f  ActionSinkFactory
}

// SetPersonaActions turns action requests on for meetings that start from
// now on (nil turns them off).
func (r *Runner) SetPersonaActions(f ActionSinkFactory) {
	r.actions.mu.Lock()
	defer r.actions.mu.Unlock()
	r.actions.f = f
}

// attachActions gives a new session's persona its draft sink.
func (r *Runner) attachActions(e *betty.Engine, m meetings.Meeting) {
	if e == nil {
		return
	}
	r.actions.mu.Lock()
	f := r.actions.f
	r.actions.mu.Unlock()
	if f == nil {
		return
	}
	if sink := f(m); sink != nil {
		e.SetActions(sink)
	}
}

// Announce destinations.
const (
	AnnouncedChat  = "chat"
	AnnouncedNotes = "notes"
)

// ErrChatUnavailable is returned when a live meeting's chat refused a
// result; the draft keeps it unannounced rather than racing the session's
// own notes.
var ErrChatUnavailable = errors.New("meetings: the live meeting's chat is unavailable")

// Announce reports a draft's result to its meeting (actions.Announcer):
// in the chat while the meeting is live, otherwise appended to its notes
// (the stored live state and the notes fact in the vault).
func (r *Runner) Announce(ctx context.Context, meetingID, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if s, ok := r.session(meetingID); ok {
		m, err := r.opts.Meetings.Get(meetingID)
		if err == nil && !m.State.Terminal() {
			cctx, cancel := context.WithTimeout(ctx, r.opts.CallTimeout)
			err = r.opts.Meetings.PostChat(cctx, meetingID, truncate(text, ChatMaxChars))
			cancel()
			if err != nil {
				return "", errors.Join(ErrChatUnavailable, err)
			}
			r.recordAnnounce(meetingID, text)
			return AnnouncedChat, nil
		}
		// The meeting is ending: let the session write its last state
		// first so the note is not overwritten.
		select {
		case <-s.done:
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(r.opts.CallTimeout):
			return "", errors.New("meetings: the meeting is still closing; result not added to its notes")
		}
	}
	return AnnouncedNotes, r.appendStoredNote(ctx, meetingID, text)
}

// appendStoredNote adds a line to an ended meeting's notes.
func (r *Runner) appendStoredNote(ctx context.Context, meetingID, text string) error {
	m, err := r.opts.Meetings.Get(meetingID)
	if err != nil {
		return err
	}
	var st State
	if err := r.opts.Store.LoadDoc(meetingID, StateDoc, &st); err != nil && !errors.Is(err, meetings.ErrNotFound) {
		return err
	}
	if st.Notes == nil {
		st.Notes = map[string][]string{}
	}
	st.Notes[SectionNotes] = append(st.Notes[SectionNotes], text)
	if err := r.opts.Store.SaveDoc(meetingID, StateDoc, st); err != nil {
		return err
	}
	if r.opts.Notes == nil {
		return nil
	}
	name := strings.TrimSpace(r.opts.PersonaName)
	if name == "" {
		name = defaultPersonaName
	}
	cctx, cancel := context.WithTimeout(ctx, r.opts.CallTimeout)
	defer cancel()
	tr, _ := r.opts.Store.Transcript(meetingID)
	return r.opts.Notes.Upsert(cctx, NotesTitle(m), RenderNotesWithReplies(name, m, st.Notes, tr), []string{"meetings", meetingID, "meeting-notes"})
}

// TriggerAnnounce is the trigger of a draft's result posted in the meeting.
const TriggerAnnounce = "announce"

// recordAnnounce keeps an announced result in the meeting's transcript as
// one of the bot's own lines.
func (r *Runner) recordAnnounce(meetingID, text string) {
	name := strings.TrimSpace(r.opts.PersonaName)
	if name == "" {
		name = defaultPersonaName
	}
	info := meetings.ReplyInfo{Kind: meetings.ReplyKindChat, Trigger: TriggerAnnounce, Source: meetings.ReplySourceFixed, Delivered: true}
	if _, err := r.opts.Store.AppendReply(meetingID, name, truncate(text, ChatMaxChars), r.opts.Now(), info); err != nil {
		r.log.Warn("[meetings] recording an announced result", "meeting", meetingID, "error", err)
	}
}
