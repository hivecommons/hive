package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/betty"
	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

// sessionInbox is the depth of a session's input queue.
const sessionInbox = 1024

// Session errors.
var (
	ErrUnknownResearch = errors.New("meetings: unknown research item")
	ErrNotPending      = errors.New("meetings: research item is not waiting for approval")
	ErrSessionEnded    = errors.New("meetings: live session has ended")
)

// Spektacular live mode statuses shown on the Meetings panel
// (hivecommons/hive#18590 finding 8; live mode itself is
// hivecommons/spektacular#126). They lead with what the room loses.
const (
	// spekStoppedStatus: the child exited for good for this meeting.
	spekStoppedStatus = "Notes and clarifying questions are off for this meeting: Spektacular live mode is not running (it needs a spektacular with the meeting command, hivecommons/spektacular#126). The transcript, the persona and research still work."
	// spekPausedStatus: hive could not reach the child (it is restarting,
	// or never came up); cleared when Spek writes again.
	spekPausedStatus = "Notes and clarifying questions are paused: Spektacular live mode is not running right now (restarting, or it needs a spektacular with the meeting command, hivecommons/spektacular#126). The transcript, the persona and research still work."
)

// spekUnsupported reports whether spek says the installed spektacular has no
// live meeting command (spektacular.Stream.Unsupported). A Spek without that
// method is never unsupported.
func spekUnsupported(spek Spek) bool {
	u, ok := spek.(interface{ Unsupported() bool })
	return ok && u.Unsupported()
}

// setSpekStatus updates the panel's Spektacular status when it changes.
func (s *session) setSpekStatus(status string) {
	if s.state.SpekStatus == status {
		return
	}
	s.state.SpekStatus = status
	s.emit(Event{Kind: EventSpek, Reason: status})
	s.save()
}

// defaultPersonaName names the attendee in hive's notices without a
// persona.
const defaultPersonaName = "Hive"

type approveReq struct {
	rid   string
	reply chan error
}

// session runs one meeting's live loop on a single goroutine: every input
// (meeting updates, Spek intents, research signals, approvals) is applied in
// order, so the state needs no lock beyond the snapshot copy.
type session struct {
	r     *Runner
	id    string
	spek  Spek
	state State
	rate  rateLimiter
	// spekGone is set once Spek is down for good for this meeting (its
	// lines closed, or the installed spektacular has no meeting command):
	// nothing more is forwarded to it, so a meeting without live mode does
	// not log a warning per transcript line (hivecommons/hive#18630).
	spekGone bool
	// lastPartial is the last partial spoken line from anyone (tone.go).
	lastPartial partialHeard
	// burst is the extra chat room for answers to addressed requests;
	// voice is the estimate of queued speech (voicequeue.go).
	burst rateLimiter
	voice voiceQueue
	// persona is the meeting's attendee persona, nil when it is off. With
	// it on, it owns the bot's voice in the meeting: Spek's say intents are
	// dropped, its chat posts are offered to the persona, and its
	// clarifying questions are restyled in the persona's voice.
	persona *betty.Engine
	// fileTokens is the usage-ledger total already charged.
	fileTokens int
	// skips carries questions dropped for budget from any goroutine;
	// skipWake wakes the session to merge them; nextSkipNotice is the
	// skipped count at which the budget notice is next repeated (budget.go).
	skips          skipQueue
	skipWake       chan struct{}
	nextSkipNotice int
	// voiceQ and chatQ are the ordered queues of lines going out by voice
	// and in chat, each drained by its own worker so a slow say or post
	// never stops the session; outDone brings each result back. async is
	// set once the workers run (outbound.go).
	voiceQ, chatQ chan *outJob
	outDone       chan outDone
	async         bool
	// heard is when the last final lines reached the session (timing.go).
	heard []heardLine
	// leaveTried is when leaving an unreachable meeting was last tried.
	leaveTried time.Time
	// spekQueue keeps Spek's asks and chat offers in order while an ask is
	// being restyled by the persona off the session goroutine; restyled
	// carries the results back.
	spekQueue []queuedSpek
	nextSeq   int
	restyled  chan restyledAsk
	// warmedAt is when the research pool was last warmed.
	warmedAt time.Time

	updates  chan meetings.Update
	signals  chan ResearchSignal
	approves chan approveReq
	snaps    chan chan State
	ended    chan struct{}
	done     chan struct{}
}

func (r *Runner) newSession(m meetings.Meeting, spek Spek) *session {
	s := &session{
		r: r, id: m.ID, spek: spek,
		rate:     rateLimiter{n: r.opts.Policy.ChatPostsPerMinute, window: rateWindow},
		burst:    answerBurst(r.opts.Policy),
		updates:  make(chan meetings.Update, sessionInbox),
		signals:  make(chan ResearchSignal, sessionInbox),
		approves: make(chan approveReq),
		snaps:    make(chan chan State),
		restyled: make(chan restyledAsk, sessionInbox),
		skipWake: make(chan struct{}, 1),
		outDone:  make(chan outDone, sessionInbox),
		ended:    make(chan struct{}),
		done:     make(chan struct{}),
	}
	if err := r.opts.Store.LoadDoc(m.ID, StateDoc, &s.state); err != nil && !errors.Is(err, meetings.ErrNotFound) {
		r.log.Warn("[meetings] live state unreadable; starting fresh", "meeting", m.ID, "error", err)
	}
	if s.state.Notes == nil {
		s.state.Notes = map[string][]string{}
	}
	s.state.SpekMode, s.state.SpekStatus = r.opts.SpekMode, ""
	if r.opts.Persona != nil {
		s.persona = r.opts.Persona(m)
		r.attachActions(s.persona, m)
	}
	if s.persona != nil {
		s.state.Persona = s.persona.Name()
		if s.state.BudgetExhausted {
			s.persona.SetExhausted()
		}
		if s.state.BudgetDegraded {
			s.persona.SetDegraded()
		}
		s.persona.OnBudgetSkip(func(q betty.BudgetSkip) { s.queueSkip(q.Who, q.Text, q.Addressed) })
	}
	s.nextSkipNotice = nextNoticeAfter(s.state.SkippedCount)
	if r.opts.UsageFile != nil {
		s.fileTokens, _ = meetllm.UsageTotal(r.opts.UsageFile(m.ID))
	}
	return s
}

func (s *session) log() *slog.Logger { return s.r.log.With("meeting", s.id) }

// personaName names the attendee in hive's own notices.
func (s *session) personaName() string {
	if s.persona != nil {
		return s.persona.Name()
	}
	if n := strings.TrimSpace(s.r.opts.PersonaName); n != "" {
		return n
	}
	return defaultPersonaName
}

// isLeave reports a leave command.
func (s *session) isLeave(text string) bool {
	if s.r.opts.IsLeave != nil {
		return s.r.opts.IsLeave(text)
	}
	return meetings.IsLeaveCommand(text)
}

func (s *session) now() time.Time { return s.r.opts.Now() }

func (s *session) save() {
	s.mergeSkips()
	if err := s.r.opts.Store.SaveDoc(s.id, StateDoc, s.state); err != nil {
		s.log().Warn("[meetings] saving live state", "error", err)
	}
}

// run is the session goroutine.
func (s *session) run(ctx context.Context) {
	defer close(s.done)
	lines := s.spek.Lines()
	ending := false
	if spekUnsupported(s.spek) {
		// The installed spektacular has no `meeting` command: live mode is
		// off from the start; the stream already logged it once per binary.
		s.spekGone, lines = true, nil
		s.setSpekStatus(spekStoppedStatus)
	}
	s.startOutbound(ctx)
	s.warmResearch(ctx)
	var replies <-chan betty.Reply
	if s.persona != nil {
		s.persona.Start(ctx)
		defer s.persona.Close()
		replies = s.persona.Replies()
	}
	tick := time.NewTicker(s.r.opts.ResearchCheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if !ending {
				if s.persona != nil {
					s.checkDeepDives(ctx)
					s.warmResearch(ctx)
				}
				s.checkReach(ctx)
			}
		case <-s.skipWake:
			s.onSkips(ctx)
		case d := <-s.outDone:
			d.job.after(ctx, d.res)
		case rep := <-replies:
			if !ending {
				s.onPersonaReply(ctx, rep)
				s.drainPersona(ctx, replies)
			}
		case <-ctx.Done():
			s.spek.Close(nil)
			s.save()
			return
		case u := <-s.updates:
			if ending {
				continue
			}
			if u.Meeting.State.Terminal() {
				ending = true
				close(s.ended)
				if s.spekGone {
					s.save()
					return
				}
				// Close blocks until Spek has flushed its last notes, which
				// arrive on lines; keep draining them here meanwhile.
				go s.spek.Close(map[string]string{"type": "end"})
				continue
			}
			s.onUpdate(u)
		case line, ok := <-lines:
			if !ok {
				if ending {
					s.save()
					return
				}
				// Spek stopped (or this spektacular has no `meeting` verb)
				// while the meeting goes on: keep the session for the panel,
				// research signals and approvals; only Spek's questions and
				// notes stop.
				s.spekGone, lines = true, nil
				if spekUnsupported(s.spek) {
					s.log().Info("[meetings] spektacular live mode unavailable; continuing without it")
				} else {
					s.log().Warn("[meetings] spektacular live mode stopped; continuing without it")
				}
				s.setSpekStatus(spekStoppedStatus)
				continue
			}
			s.onIntent(ctx, line)
		case rs := <-s.restyled:
			if !ending {
				s.onRestyled(ctx, rs)
			}
		case sig := <-s.signals:
			s.onSignal(ctx, sig)
		case a := <-s.approves:
			a.reply <- s.approve(ctx, a.rid)
		case reply := <-s.snaps:
			reply <- s.snapshot()
		}
	}
}

// researchWarmEvery is how often a running meeting re-warms the research
// pool (a no-op while its agents run).
const researchWarmEvery = time.Minute

// warmResearch boots the research agents when the dispatcher can, at most
// once per researchWarmEvery.
func (s *session) warmResearch(ctx context.Context) {
	w, ok := s.r.opts.Research.(ResearchWarmer)
	if !ok || w == nil {
		return
	}
	now := s.now()
	if !s.warmedAt.IsZero() && now.Sub(s.warmedAt) < researchWarmEvery {
		return
	}
	s.warmedAt = now
	w.Warm(ctx)
}

func (s *session) snapshot() State {
	s.mergeSkips()
	b, _ := json.Marshal(s.state)
	var out State
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *session) emit(ev Event) {
	ev.MeetingID, ev.At = s.id, s.now()
	s.r.emit(ev)
}

// onUpdate forwards people's final segments and chat lines to Spek and the
// persona (without blocking) and matches replies to open asks. Hive's own
// lines never reach here: the manager drops them before notifying.
func (s *session) onUpdate(u meetings.Update) {
	ev := u.Event
	if ev == nil {
		return
	}
	switch {
	case ev.Type == meetings.EventSegment && ev.Final, ev.Type == meetings.EventChat:
	case ev.Type == meetings.EventSegment:
		// A partial transcript only lets the persona acknowledge a
		// request early; Spek and open asks wait for the final line.
		if s.persona != nil {
			s.notePartial(ev.Who())
			s.persona.ObservePartial(betty.Line{Who: ev.Who(), Text: ev.Text, Spoken: true})
		}
		return
	default:
		return
	}
	if len(ev.Raw) > 0 && !s.spekGone {
		if err := s.spek.SendLine(ev.Raw); err != nil {
			// The child is down (between restarts, or a spektacular
			// without the meeting command): say so on the panel now rather
			// than only once it gives up. Warn on that transition only, not
			// on every line while it stays down.
			if s.state.SpekStatus == "" {
				s.log().Warn("[meetings] forwarding to spek", "error", err)
				s.setSpekStatus(spekPausedStatus)
			}
		}
	}
	s.noteHeard(ev.Text)
	if s.persona != nil {
		s.persona.Observe(betty.Line{Who: ev.Who(), Text: ev.Text, Spoken: ev.Type == meetings.EventSegment})
	}
	s.matchAnswer(ev)
}

// matchAnswer is the answer heuristic: the first line from a person after
// an ask, within the answer window, answers the oldest open ask. Asks past
// the window expire unanswered. Lines addressed to the persona and leave
// commands never count as answers. It is deliberately simple; Spek
// receives the whole transcript and can re-ask if the match was wrong.
func (s *session) matchAnswer(ev *meetings.Event) {
	now := s.now()
	skip := strings.TrimSpace(ev.Text) == "" || s.isLeave(ev.Text) || (s.persona != nil && s.persona.Skip(ev.Text))
	window := s.r.opts.Policy.AnswerWindow
	changed := false
	answered := false
	for i := range s.state.Asks {
		a := &s.state.Asks[i]
		if a.AnsweredAt != nil || a.Expired {
			continue
		}
		if now.Sub(a.AskedAt) > window {
			a.Expired, changed = true, true
			continue
		}
		if answered || skip {
			continue
		}
		a.Answer, a.AnsweredBy, a.AnsweredAt = ev.Text, ev.Who(), &now
		answered, changed = true, true
		if s.spekGone {
			continue
		}
		if err := s.spek.Send(map[string]string{"type": "answer", "qid": a.QID, "text": ev.Text, "by": ev.Who()}); err != nil {
			s.log().Warn("[meetings] sending answer to spek", "error", err)
		}
	}
	if changed {
		s.save()
	}
}

func (s *session) onIntent(ctx context.Context, line []byte) {
	var in intent
	if err := json.Unmarshal(line, &in); err != nil {
		s.log().Warn("[meetings] spek wrote a non-JSON line; skipped", "error", err)
		return
	}
	// Spek is talking again after a restart: notes and questions are back.
	if s.state.SpekStatus == spekPausedStatus {
		s.setSpekStatus("")
	}
	if in.Type == intentHeartbeat {
		now := s.now()
		s.state.LastHeartbeat = &now
		return
	}
	s.emit(Event{Kind: EventIntent, Intent: append(json.RawMessage(nil), line...)})
	s.account(ctx, in)
	switch in.Type {
	case intentChatPost:
		if s.spendable() {
			if s.persona != nil {
				// The persona posts it unless she already covered the topic;
				// it waits behind any ask still being restyled.
				s.enqueueSpek(ctx, queuedSpek{offer: in.Text})
			} else {
				s.post(ctx, in.Text)
			}
		}
	case intentAsk:
		if s.spendable() {
			if s.persona != nil {
				s.restyleAsk(ctx, in)
			} else {
				s.postAsk(ctx, in.QID, in.Text)
			}
		}
	case intentNotesAppend:
		s.appendNote(ctx, in.Section, in.Text)
	case intentResearch:
		if s.spendable() {
			s.research(ctx, in)
		}
	case intentSay:
		switch {
		case s.persona != nil:
			// The persona owns the bot's voice; Spek's say is dropped.
			s.drop("persona_owns_voice")
		case s.spendable():
			s.say(ctx, in.Text)
		}
	default:
		s.log().Info("[meetings] unknown spek intent; skipped", "type", in.Type)
	}
	s.save()
}

// account charges an intent to the budget: reported tokens when Spek
// sends usage, else one intent.
func (s *session) account(ctx context.Context, in intent) {
	p := s.r.opts.Policy
	if s.r.opts.UsageFile != nil {
		if total, err := meetllm.UsageTotal(s.r.opts.UsageFile(s.id)); err == nil && total > s.fileTokens {
			s.state.Tokens += total - s.fileTokens
			s.r.opts.Ledger.Add(s.now(), total-s.fileTokens)
			s.fileTokens = total
		}
	}
	if in.Usage != nil {
		t := in.Usage.total()
		s.state.Tokens += t
		s.r.opts.Ledger.Add(s.now(), t)
	} else {
		s.state.Intents++
	}
	reason := ""
	switch {
	case p.MaxTokensPerMeeting > 0 && s.state.Tokens > p.MaxTokensPerMeeting:
		reason = BudgetReasonTokens
	case p.MaxIntentsPerMeeting > 0 && s.state.Intents > p.MaxIntentsPerMeeting:
		reason = BudgetReasonIntents
	case s.r.opts.Ledger.Exceeded(s.now()):
		reason = BudgetReasonHiveCeiling
	}
	if reason == "" {
		if !s.state.BudgetExhausted {
			s.budgetSteps(ctx)
		}
		return
	}
	if !s.state.BudgetExhausted {
		s.state.BudgetExhausted, s.state.BudgetReason = true, reason
		// The earlier steps are moot: no repeat warn or degrade later.
		s.state.BudgetWarned, s.state.BudgetDegraded = true, true
		s.log().Warn("[meetings] meeting budget exhausted", "tokens", s.state.Tokens, "intents", s.state.Intents, "reason", reason)
		s.emit(Event{Kind: EventBudget, Reason: BudgetLevelExhausted})
		if s.persona != nil {
			s.persona.SetExhausted()
		}
		s.sayLine(ctx, TriggerNotice, false, budgetNotice(s.personaName()))
	}
}

// spendable reports whether intents that cost the meeting attention or LLM
// budget are still allowed. Notes always are.
func (s *session) spendable() bool {
	if s.state.BudgetExhausted {
		s.drop("budget")
		return false
	}
	return true
}

func (s *session) drop(reason string) {
	s.state.Dropped++
	s.emit(Event{Kind: EventDropped, Reason: reason})
}

// say speaks only when the meeting has voice on and voice_on_address is
// allowed; otherwise, or when the bot has no voice, it answers in chat.
func (s *session) say(ctx context.Context, text string) {
	s.send(ctx, &line{rep: own(TriggerSay, betty.KindVoice, text, SourceSpek), text: text, voice: true, rate: rateChat})
}

func (s *session) appendNote(ctx context.Context, section, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	known := false
	for _, k := range noteSections {
		if k == section {
			known = true
		}
	}
	if !known {
		section = SectionNotes
	}
	s.state.Notes[section] = append(s.state.Notes[section], text)
	s.emit(Event{Kind: EventNotes})
	m, _ := s.r.opts.Meetings.Get(s.id)
	// The notes fact carries what the attendee said as well as her notes.
	tr, _ := s.r.opts.Store.Transcript(s.id)
	s.upsertFact(ctx, NotesTitle(m), RenderNotesWithReplies(s.personaName(), m, s.state.Notes, tr), []string{"meeting-notes"})
}

func (s *session) upsertFact(ctx context.Context, title, body string, tags []string) {
	if s.r.opts.Notes == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.r.opts.CallTimeout)
	defer cancel()
	if err := s.r.opts.Notes.Upsert(cctx, title, body, append([]string{"meetings", s.id}, tags...)); err != nil {
		s.log().Warn("[meetings] writing knowledge fact", "title", title, "error", err)
	}
}

// NotesTitle is the title of a meeting's notes fact.
func NotesTitle(m meetings.Meeting) string {
	return "Meeting notes: " + meetingLabel(m)
}

func meetingLabel(m meetings.Meeting) string {
	name := strings.TrimSpace(m.Title)
	if name == "" {
		name = strings.TrimSpace(strings.ReplaceAll(m.Platform, "_", " ") + " meeting")
	}
	day := m.CreatedAt.UTC().Format("2006-01-02")
	return fmt.Sprintf("%s %s (%s)", name, day, m.ID)
}

// RenderNotes renders the notes sections as Markdown.
func RenderNotes(m meetings.Meeting, notes map[string][]string) string {
	return RenderNotesBy("hive", m, notes)
}

// RenderNotesBy renders the notes with the attendee's name in the header.
func RenderNotesBy(name string, m meetings.Meeting, notes map[string][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Notes %s took in %s.\n", name, meetingLabel(m))
	for _, sec := range noteSections {
		items := notes[sec]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n", strings.ToUpper(sec[:1])+sec[1:])
		for _, it := range items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	return b.String()
}

func (s *session) find(rid string) *ResearchItem {
	for i := range s.state.Research {
		if s.state.Research[i].RID == rid {
			return &s.state.Research[i]
		}
	}
	return nil
}

func (s *session) findKick(kick string) *ResearchItem {
	for i := range s.state.Research {
		if s.state.Research[i].KickID == kick {
			return &s.state.Research[i]
		}
	}
	return nil
}

func (s *session) dispatchedCount() int {
	n := 0
	for _, it := range s.state.Research {
		if it.Status != ResearchPendingApproval {
			n++
		}
	}
	return n
}

func (s *session) update(it *ResearchItem, status, summary string) {
	it.Status, it.UpdatedAt = status, s.now()
	if status == ResearchFailed {
		// Kept on the item: the panel and session.json say why it failed.
		it.Error = strings.TrimSpace(summary)
		if it.Error == "" {
			it.Error = researchNoAnswer
		}
	}
	msg := map[string]string{"type": "research_update", "rid": it.RID, "status": status}
	if summary != "" {
		msg["summary"] = summary
	}
	// Spek never heard of the persona's deep dives.
	if !it.Betty {
		if err := s.spek.Send(msg); err != nil {
			s.log().Warn("[meetings] sending research update to spek", "error", err)
		}
	}
	cp := *it
	s.emit(Event{Kind: EventResearch, Research: &cp})
}

// research applies a research intent: dispatch within the per-meeting cap,
// else hold for owner approval.
func (s *session) research(ctx context.Context, in intent) {
	rid, query := strings.TrimSpace(in.RID), strings.TrimSpace(in.Query)
	if s.state.BudgetDegraded {
		// At BudgetDegradePercent no new research starts.
		s.drop("budget_degraded")
		if rid != "" {
			_ = s.spek.Send(map[string]string{"type": "research_update", "rid": rid, "status": ResearchFailed, "summary": "not started: the meeting budget is nearly used"})
		}
		return
	}
	if rid == "" || query == "" || s.find(rid) != nil {
		s.drop("research_invalid_or_duplicate")
		return
	}
	now := s.now()
	limit := s.r.opts.Policy.ResearchPerMeeting
	overCap := limit > 0 && s.dispatchedCount() >= limit
	s.state.Research = append(s.state.Research, ResearchItem{RID: rid, Query: query, Reason: in.Reason, CreatedAt: now})
	it := &s.state.Research[len(s.state.Research)-1]
	if overCap {
		s.update(it, ResearchPendingApproval, "")
		// Spek only knows the contract statuses; tell it the item is queued
		// behind an approval.
		_ = s.spek.Send(map[string]string{"type": "research_update", "rid": rid, "status": ResearchQueued, "summary": "waiting for approval in the hive dashboard"})
		return
	}
	s.dispatch(ctx, it)
}

func (s *session) dispatch(ctx context.Context, it *ResearchItem) {
	if s.r.opts.Research == nil {
		s.update(it, ResearchFailed, "research is not available on this hive")
		s.deepDiveFailed(ctx, it, "research is not available on this hive")
		return
	}
	meta := map[string]string{
		"source": "meeting", "meeting_id": s.id, "rid": it.RID, "reason": it.Reason,
		// The query is meeting speech: untrusted input for a read-only,
		// propose-only research agent.
		"untrusted": "true",
	}
	if it.Betty {
		meta["betty"] = "1"
	}
	// Asked before the dispatch: afterwards this item is part of the load.
	busy := false
	if l, ok := s.r.opts.Research.(ResearchLoad); ok && l != nil {
		busy = l.Busy()
	}
	cctx, cancel := context.WithTimeout(ctx, s.r.opts.CallTimeout)
	kick, err := s.r.opts.Research.Dispatch(cctx, it.Query, meta)
	cancel()
	if err != nil {
		s.log().Warn("[meetings] research dispatch failed", "rid", it.RID, "error", err)
		reason := researchFailReason(err)
		s.update(it, ResearchFailed, "could not start research: "+reason)
		s.deepDiveFailed(ctx, it, reason)
		return
	}
	it.KickID = kick
	at := s.now()
	it.DispatchedAt = &at
	it.Waiting = busy
	s.r.registerKick(kick, s.id)
	s.update(it, ResearchQueued, "")
	if busy && it.Betty && s.persona != nil {
		// Every research agent has work: the request waits its turn, and
		// she says so rather than fail it or leave the room guessing.
		s.log().Info("[meetings] research agents busy; the request is queued behind another", "rid", it.RID)
		s.sayLine(ctx, TriggerBusy, it.Voice, researchBusyText)
	}
}

// researchBusyText is what she says when a deep dive is queued behind
// another question because every research agent has work.
const researchBusyText = "I'm busy with another question, yours is next."

// researchNoAnswer is the failure reason when the research agent gave none.
const researchNoAnswer = "the research agent stopped without an answer"

func (s *session) approve(ctx context.Context, rid string) error {
	it := s.find(rid)
	if it == nil {
		return ErrUnknownResearch
	}
	if it.Status != ResearchPendingApproval {
		return ErrNotPending
	}
	s.dispatch(ctx, it)
	s.save()
	return nil
}

// onSignal applies a research kick's [FINDING]/[COMPLETE]/[ERROR].
func (s *session) onSignal(ctx context.Context, sig ResearchSignal) {
	it := s.findKick(sig.KickID)
	if it == nil {
		return
	}
	text := strings.TrimSpace(sig.Text)
	switch sig.Kind {
	case SignalRunning:
		// The kick reached its agent: the wait behind other questions is
		// over, and the deep dive's clock starts now.
		if it.StartedAt != nil || (it.Status != ResearchQueued && it.Status != ResearchRunning) {
			return
		}
		at := s.now()
		it.StartedAt, it.Waiting = &at, false
		s.update(it, ResearchRunning, "")
	case SignalFinding:
		if text != "" && len(it.Findings) < maxFindings {
			it.Findings = append(it.Findings, text)
		}
		s.update(it, ResearchRunning, text)
	case SignalComplete:
		it.Summary = text
		s.update(it, ResearchDone, text)
		if !it.Posted {
			it.Posted = true
			summary := text
			if summary == "" && len(it.Findings) > 0 {
				summary = it.Findings[len(it.Findings)-1]
			}
			if it.Betty && s.persona != nil {
				// Exactly one follow-up, restyled by the persona off the
				// session goroutine; it comes back as a persona reply.
				s.persona.FollowUpIn(it.Query, summary, it.Voice)
			} else {
				text := fmt.Sprintf("Research on %q: %s", it.Query, summary)
				s.send(ctx, &line{rep: own(TriggerResearch, betty.KindChat, text, betty.SourceResearch), text: text})
			}
		}
		s.upsertFact(ctx, researchTitle(s.id, it), renderResearch(it), []string{"meeting-research"})
		s.r.unregisterKick(sig.KickID)
	case SignalError:
		reason := text
		if reason == "" {
			reason = researchNoAnswer
		}
		s.log().Warn("[meetings] research failed", "rid", it.RID, "kick", sig.KickID, "reason", reason)
		s.update(it, ResearchFailed, reason)
		s.deepDiveFailed(ctx, it, reason)
		s.r.unregisterKick(sig.KickID)
	default:
		return
	}
	s.save()
}

func researchTitle(meetingID string, it *ResearchItem) string {
	return fmt.Sprintf("Meeting research: %s (%s %s)", truncate(it.Query, maxTitleQuery), meetingID, it.RID)
}

// maxTitleQuery bounds the query quoted in a fact title.
const maxTitleQuery = 80

func renderResearch(it *ResearchItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Research requested in a meeting: %s\n", it.Query)
	if it.Reason != "" {
		fmt.Fprintf(&b, "\nReason: %s\n", it.Reason)
	}
	if it.Summary != "" {
		fmt.Fprintf(&b, "\n## Summary\n\n%s\n", it.Summary)
	}
	if len(it.Findings) > 0 {
		b.WriteString("\n## Findings\n\n")
		for _, f := range it.Findings {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	return b.String()
}

// onPersonaReply applies one persona reply through the same policy as
// Spek's intents: it is charged to the budget (one intent, plus any model
// tokens in the usage ledger), then posted through the chat rate limit (or
// spoken, with the chat fallback, for a voice reply). A spoken
// acknowledgement is never charged and never falls back to chat; a deep
// dive's follow-up is delivered like a research summary; a reply carrying
// Research starts the deep dive once the reply is on its way. Nothing here
// waits for the meeting: every line goes out through the outbound queues
// and is recorded when its call returns (outbound.go, timing.go).
func (s *session) onPersonaReply(ctx context.Context, rep betty.Reply) {
	if rep.Trigger == betty.TriggerTone {
		s.playTone(rep)
		return
	}
	text := strings.TrimSpace(rep.Text)
	if text == "" {
		return
	}
	voice := rep.Kind == betty.KindVoice
	switch rep.Trigger {
	case betty.TriggerQueue:
		// The "I have N questions" note: chat only, through the rate
		// limit, never charged (no model call made it).
		if !s.state.BudgetExhausted {
			s.send(ctx, &line{rep: rep, text: text, rate: rateChat, quiet: true})
		}
		return
	case betty.TriggerAck:
		if s.state.BudgetExhausted {
			return
		}
		if !s.speakAck(ctx, rep, text) {
			s.drop("ack_no_voice")
		}
		return
	case betty.TriggerFollowUp:
		// In the channel the question was asked in: spoken when it was
		// spoken and the meeting can hear her, else chat.
		s.followUpTiming(&rep)
		s.sendReply(ctx, &line{rep: rep, text: text, voice: voice})
		s.save()
		return
	case betty.TriggerOffer:
		// A Spek offer was charged when Spek sent it.
	default:
		s.account(ctx, intent{Type: intentPersona})
	}
	if !s.spendable() {
		// The model answered but the budget ran out before it could be
		// posted: the question is still recorded as not answered.
		if rep.Question != "" && (rep.Trigger == betty.TriggerAddressed || rep.Trigger == betty.TriggerQuestion) {
			s.queueSkip(rep.Asker, rep.Question, rep.Trigger == betty.TriggerAddressed)
		}
		s.save()
		return
	}
	dive := func(accepted bool) {
		if accepted && strings.TrimSpace(rep.Research) != "" {
			s.deepDive(ctx, rep, voice || rep.VoiceWanted)
		}
	}
	switch {
	case voice:
		// Spoken with the other answers ready now, shortest first
		// (flushVoice), falling back to chat.
		s.queueVoiceThen(rep, text, dive)
	case rep.Trigger == betty.TriggerAddressed:
		s.cancelAcks()
		dive(s.sendReply(ctx, &line{rep: rep, text: text, rate: rateAnswer}))
	default:
		dive(s.sendReply(ctx, &line{rep: rep, text: text, rate: rateChat}))
	}
	s.save()
}

// bettyRIDPrefix names the persona's research items.
const bettyRIDPrefix = "betty-"

// deepDive starts the persona's research on an addressed question through
// the meeting's research path (same cap and approval as Spek's research),
// marked betty=1.
func (s *session) deepDive(ctx context.Context, rep betty.Reply, voice bool) {
	if s.state.BudgetDegraded {
		s.drop("budget_degraded")
		return
	}
	query := rep.Research
	n := 1
	for _, it := range s.state.Research {
		if it.Betty {
			n++
		}
	}
	now := s.now()
	limit := s.r.opts.Policy.ResearchPerMeeting
	overCap := limit > 0 && s.dispatchedCount() >= limit
	s.state.Research = append(s.state.Research, ResearchItem{
		RID: bettyRIDPrefix + strconv.Itoa(n), Query: query, Reason: "asked of " + s.personaName(), Betty: true, Voice: voice, CreatedAt: now,
	})
	it := &s.state.Research[len(s.state.Research)-1]
	if rep.Timing != nil && !rep.Timing.HeardAt.IsZero() {
		heard := rep.Timing.HeardAt
		it.HeardAt = &heard
	}
	if overCap {
		s.update(it, ResearchPendingApproval, "")
		return
	}
	s.dispatch(ctx, it)
}

// researchFailReason is the sayable cause of a dispatch failure: the
// error's own Reason when it has one (research.ErrNoAgents), else a plain
// summary. The raw error stays in the log.
func researchFailReason(err error) string {
	var r interface{ Reason() string }
	switch {
	case errors.As(err, &r) && strings.TrimSpace(r.Reason()) != "":
		return r.Reason()
	case errors.Is(err, context.DeadlineExceeded):
		return "the research agents did not answer in time"
	default:
		return "the research could not be started"
	}
}

// deepDiveFailed closes the loop on a persona deep dive that could not
// start or finish: she says so, once, in the channel she was asked in.
// Spek's research items are reported on the panel only.
func (s *session) deepDiveFailed(ctx context.Context, it *ResearchItem, reason string) {
	if !it.Betty || it.Posted || s.persona == nil {
		return
	}
	it.Posted = true
	s.sayLine(ctx, TriggerResearchFailed, it.Voice, s.persona.ResearchFailedText(reason))
}

// checkDeepDives runs on the session's research tick: a persona deep dive
// still queued or running past ResearchProgressAfter gets one progress
// note; past ResearchTimeout it is given up, and she says so.
func (s *session) checkDeepDives(ctx context.Context) {
	now, changed := s.now(), false
	for i := range s.state.Research {
		it := &s.state.Research[i]
		if !it.Betty || it.Posted || it.DispatchedAt == nil || (it.Status != ResearchQueued && it.Status != ResearchRunning) {
			continue
		}
		// The clock runs from when the agent took the kick; an item queued
		// behind another question (Waiting) gets ResearchQueueWait on top
		// to reach the head of the queue, and no progress note (she said
		// "yours is next").
		age := now.Sub(*it.DispatchedAt)
		switch {
		case it.StartedAt != nil:
			age = now.Sub(*it.StartedAt)
		case it.Waiting:
			age -= s.r.opts.ResearchQueueWait
		}
		switch {
		case age >= s.r.opts.ResearchTimeout:
			s.log().Warn("[meetings] persona deep dive timed out", "rid", it.RID, "after", age.Round(time.Second))
			s.update(it, ResearchFailed, "timed out")
			if it.KickID != "" {
				s.r.unregisterKick(it.KickID)
			}
			s.deepDiveFailed(ctx, it, "it took too long")
			changed = true
		case age >= s.r.opts.ResearchProgressAfter && !it.ProgressSent:
			it.ProgressSent = true
			s.sayLine(ctx, TriggerProgress, it.Voice, s.persona.ResearchProgressText())
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// queuedSpek is one of Spek's asks or chat offers waiting its turn while an
// earlier ask is restyled.
type queuedSpek struct {
	seq   int
	offer string
	// ask fields: qid and the text, ready once restyled.
	ask   bool
	qid   string
	text  string
	ready bool
}

// restyledAsk is a restyle result.
type restyledAsk struct {
	seq  int
	text string
}

// restyleAsk rewrites one of Spek's clarifying questions in the persona's
// voice without blocking the session: the model call runs off the
// goroutine (bounded by the persona's restyle timeout, raw text on
// failure), and the ask keeps its place among Spek's posts.
func (s *session) restyleAsk(ctx context.Context, in intent) {
	s.nextSeq++
	q := queuedSpek{seq: s.nextSeq, ask: true, qid: in.QID, text: in.Text}
	s.spekQueue = append(s.spekQueue, q)
	persona, seq, raw := s.persona, q.seq, in.Text
	go func() {
		text := persona.Restyle(ctx, raw)
		select {
		case s.restyled <- restyledAsk{seq: seq, text: text}:
		case <-ctx.Done():
		}
	}()
}

// enqueueSpek offers a Spek chat post to the persona now, or after the
// asks queued before it.
func (s *session) enqueueSpek(ctx context.Context, q queuedSpek) {
	if len(s.spekQueue) == 0 {
		s.persona.Offer(q.offer)
		return
	}
	s.nextSeq++
	q.seq = s.nextSeq
	s.spekQueue = append(s.spekQueue, q)
	s.flushSpek(ctx)
}

func (s *session) onRestyled(ctx context.Context, rs restyledAsk) {
	for i := range s.spekQueue {
		if s.spekQueue[i].seq == rs.seq {
			if strings.TrimSpace(rs.text) != "" {
				s.spekQueue[i].text = rs.text
			}
			s.spekQueue[i].ready = true
		}
	}
	s.flushSpek(ctx)
	s.save()
}

// flushSpek posts queued items from the head while they are ready.
func (s *session) flushSpek(ctx context.Context) {
	for len(s.spekQueue) > 0 {
		q := s.spekQueue[0]
		if q.ask && !q.ready {
			return
		}
		s.spekQueue = s.spekQueue[1:]
		if q.ask {
			if s.spendable() {
				s.postAsk(ctx, q.qid, q.text)
			}
			continue
		}
		s.persona.Offer(q.offer)
	}
}

// postAsk posts a clarifying question and tracks it for answers: from the
// moment it is accepted, so an answer that arrives right behind it is
// matched, and no longer if the post then fails.
func (s *session) postAsk(ctx context.Context, qid, text string) {
	tracked, failed := false, false
	l := &line{rep: own(TriggerAsk, betty.KindChat, text, SourceSpek), text: text, rate: rateChat}
	l.done = func(delivered bool) {
		failed = !delivered
		if !failed || !tracked {
			return
		}
		for i := range s.state.Asks {
			if s.state.Asks[i].QID == qid && s.state.Asks[i].AnsweredAt == nil {
				s.state.Asks = append(s.state.Asks[:i], s.state.Asks[i+1:]...)
				return
			}
		}
	}
	if s.send(ctx, l) && !failed {
		s.state.Asks = append(s.state.Asks, Ask{QID: qid, Text: text, AskedAt: s.now()})
		tracked = true
	}
}
