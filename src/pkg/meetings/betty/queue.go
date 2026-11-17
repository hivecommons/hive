package betty

import (
	"context"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
)

// Queue thresholds.
const (
	// answeredOverlap is the share of a question's topic words a person's
	// reply must cover to count as answering it.
	answeredOverlap = 0.3
	// dedupeJaccard is the similarity at which two topics are the same.
	dedupeJaccard = 0.5
	// offerContainment also treats an offer as covered when most of the
	// smaller topic is in the larger one (an answer repeats the question's
	// words but adds its own).
	offerContainment = 0.6
)

// Fallback replies when addressed and no model answer is available.
const (
	fallbackUnsure  = "Sorry, I'm not sure about that one."
	fallbackNoModel = "Sorry, I can't answer questions on this hive yet: no meetings model is configured."
	fallbackHello   = "Hi, I'm here. What can I help with?"
)

// pending is one candidate waiting for retrieval, the answer wait or the
// model.
type pending struct {
	id        int
	line      Line
	trig      Trigger
	topic     []string
	addressed bool
	created   time.Time
	// waited: the answer wait is over; retrieved: retrieval returned.
	waited, retrieved bool
	// answered: someone else answered first.
	answered bool
	asking   bool
	snips    []Snippet
	// thoughts: a spoken request with no question.
	thoughts bool
}

type retrieval struct {
	id    int
	snips []Snippet
}

type answered struct {
	id  int
	ans Answer
	err error
}

// rememberTopic records a topic as answered, for dedupe.
func (e *Engine) rememberTopic(t []string) {
	if len(t) == 0 {
		return
	}
	e.topics = append(e.topics, t)
	if len(e.topics) > maxAnsweredTopics {
		e.topics = e.topics[len(e.topics)-maxAnsweredTopics:]
	}
}

func (e *Engine) topicAnswered(t []string) bool {
	for _, a := range e.topics {
		if Jaccard(a, t) >= dedupeJaccard {
			return true
		}
	}
	return false
}

func (e *Engine) remember(l Line, now time.Time) {
	e.recent = append(e.recent, timedLine{recentLine{Who: l.Who, Text: l.Text}, now})
	cut := now.Add(-e.s.RecentWindow)
	i := 0
	for i < len(e.recent) && (e.recent[i].at.Before(cut) || len(e.recent)-i > maxRecentLines) {
		i++
	}
	e.recent = e.recent[i:]
}

func (e *Engine) recentLines() []recentLine {
	out := make([]recentLine, len(e.recent))
	for i, l := range e.recent {
		out[i] = l.recentLine
	}
	return out
}

func (e *Engine) onLine(l Line) {
	if strings.TrimSpace(l.Text) == "" || meetings.IsBotName(l.Who, e.s.Persona.DisplayName) || e.leave.Match(l.Text) {
		return
	}
	now := e.d.Now()
	trig := e.match.Match(l.Text, l.Spoken)
	if !trig.Addressed {
		// Their final line did not address her after all: the next one
		// that does gets its own ack.
		delete(e.earlyAck, l.Who)
	}
	topic := TopicKey(l.Text)
	question := IsQuestion(l.Text)
	if !trig.Addressed && !question {
		e.markAnswered(l, topic)
	}
	e.remember(l, now)
	if e.onActionLine(l, trig, now) {
		return
	}
	switch {
	case trig.Intro:
		e.emit(Reply{Kind: replyKind(trig), Text: e.s.Persona.Intro(), Trigger: TriggerIntro, Question: l.Text, VoiceWanted: trig.VoiceWanted})
	case trig.Addressed:
		e.addressed(l, trig, now)
	case question:
		e.question(l, topic, now)
	}
}

// markAnswered drops waiting room questions another person just answered:
// a non-question line from someone else covering enough of the topic.
func (e *Engine) markAnswered(l Line, topic []string) {
	for _, p := range e.pending {
		if p.addressed || p.answered || strings.EqualFold(p.line.Who, l.Who) {
			continue
		}
		if Overlap(p.topic, topic) >= answeredOverlap {
			p.answered = true
			e.rememberTopic(p.topic)
		}
	}
}

func (e *Engine) addressed(l Line, trig Trigger, now time.Time) {
	thoughts := false
	if strings.TrimSpace(trig.Rest) == "" {
		// A spoken request with no question gets one clarifying question
		// or a thought on the recent discussion; anything else, a hello.
		if !l.Spoken || e.d.LLM == nil || e.exhausted.Load() || len(e.recent) < minThoughtLines {
			e.emit(Reply{Kind: replyKind(trig), Text: fallbackHello, Trigger: TriggerAddressed, Question: l.Text, VoiceWanted: trig.VoiceWanted})
			return
		}
		thoughts = true
	}
	if e.exhausted.Load() {
		e.drop("budget")
		return
	}
	if trig.VoiceWanted && e.d.LLM != nil && !e.takeEarlyAck(l.Who, now) {
		// Speak at once so the room knows she heard; the answer follows.
		e.emit(Reply{Kind: KindVoice, Text: e.ack(), Trigger: TriggerAck, Question: l.Text, VoiceWanted: true})
	}
	p := e.add(l, trig, TopicKey(trig.Rest), true, now)
	p.waited, p.thoughts = true, thoughts
	e.retrieve(p, trig.Rest)
}

// Early acknowledgement from partial transcripts (hivecommons/hive#18590):
// the ack is spoken as soon as a partial line names her with a request,
// seconds before the final line arrives.
const (
	// earlyAckMinWords is how much of a request a partial must carry
	// after her name before she acknowledges it: "Betty" alone may still
	// become a hello, which gets no ack.
	earlyAckMinWords = 2
	// earlyAckWindow is how long an early ack stands in for the ack of
	// the same speaker's next final line.
	earlyAckWindow = 30 * time.Second
)

// onPartial speaks the acknowledgement for a partial spoken line that
// already addresses her with a request, once per speaker until their
// final line arrives.
func (e *Engine) onPartial(l Line) {
	if meetings.IsBotName(l.Who, e.s.Persona.DisplayName) || e.leave.Match(l.Text) {
		return
	}
	if e.d.LLM == nil || e.exhausted.Load() {
		return
	}
	trig := e.match.Match(l.Text, true)
	if !trig.Addressed || trig.Intro || !trig.VoiceWanted || len(strings.Fields(trig.Rest)) < earlyAckMinWords {
		return
	}
	if _, ok := e.actionAddress(l.Text); ok {
		return
	}
	now := e.d.Now()
	if at, ok := e.earlyAck[l.Who]; ok && now.Sub(at) < earlyAckWindow {
		return
	}
	if e.emit(Reply{Kind: KindVoice, Text: e.ack(), Trigger: TriggerAck, Question: l.Text, VoiceWanted: true}) {
		e.earlyAck[l.Who] = now
	}
}

// takeEarlyAck reports whether this speaker was already acknowledged from
// a partial of the line now final, and clears the mark.
func (e *Engine) takeEarlyAck(who string, now time.Time) bool {
	at, ok := e.earlyAck[who]
	delete(e.earlyAck, who)
	return ok && now.Sub(at) < earlyAckWindow
}

// minThoughtLines is the transcript a spoken "Betty?" needs before she
// offers thoughts instead of a hello: the request itself plus one line.
const minThoughtLines = 2

// replyKind is a reply's kind: voice when it was asked for.
func replyKind(t Trigger) string {
	if t.VoiceWanted {
		return KindVoice
	}
	return KindChat
}

func (e *Engine) question(l Line, topic []string, now time.Time) {
	switch {
	case len(topic) == 0:
		e.drop("no_topic")
		return
	case e.exhausted.Load():
		e.drop("budget")
		return
	case e.d.LLM == nil:
		e.drop("no_model")
		return
	case e.topicAnswered(topic):
		e.drop("already_answered")
		return
	}
	waiting := 0
	for _, p := range e.pending {
		if p.addressed {
			continue
		}
		if Jaccard(p.topic, topic) >= dedupeJaccard {
			e.drop("duplicate")
			return
		}
		waiting++
	}
	if e.s.QueueDepth > 0 && waiting >= e.s.QueueDepth {
		e.drop("queue_full")
		return
	}
	p := e.add(l, Trigger{}, topic, false, now)
	e.retrieve(p, l.Text)
	id := p.id
	time.AfterFunc(e.s.AnswerWait, func() {
		select {
		case e.timers <- id:
		case <-e.ctx.Done():
		}
	})
}

func (e *Engine) add(l Line, trig Trigger, topic []string, addressed bool, now time.Time) *pending {
	e.nextID++
	p := &pending{id: e.nextID, line: l, trig: trig, topic: topic, addressed: addressed, created: now}
	e.pending[p.id] = p
	return p
}

// retrieve starts retrieval at once, in parallel with the answer wait.
func (e *Engine) retrieve(p *pending, query string) {
	id, terms := p.id, p.topic
	if len(e.d.Retrievers) == 0 || len(terms) == 0 {
		p.retrieved = true
		if p.waited {
			e.proceed(p)
		}
		return
	}
	go func() {
		snips := Gather(e.ctx, e.d.Retrievers, query, terms, e.s.RetrieveTimeout)
		select {
		case e.retrieved <- retrieval{id: id, snips: snips}:
		case <-e.ctx.Done():
		}
	}()
}

func (e *Engine) finish(p *pending, reason string) {
	delete(e.pending, p.id)
	if reason != "" {
		e.drop(reason)
	}
}

// capBlocked names the unprompted cap a post now would break, or "".
func (e *Engine) capBlocked(now time.Time) string {
	cut := now.Add(-e.s.CapWindow)
	kept := e.posts[:0]
	for _, t := range e.posts {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.posts = kept
	switch {
	case e.s.PerMeeting > 0 && e.total >= e.s.PerMeeting:
		return "cap_per_meeting"
	case e.s.Per10m > 0 && len(e.posts) >= e.s.Per10m:
		return "cap_per_10m"
	case e.s.MinGap > 0 && !e.lastPost.IsZero() && now.Sub(e.lastPost) < e.s.MinGap:
		return "cap_min_gap"
	}
	return ""
}

// unpromptedBlocked is every reason not to answer a room question now.
func (e *Engine) unpromptedBlocked(p *pending, now time.Time) string {
	switch {
	case p.answered:
		return "answered_by_person"
	case e.s.Stale > 0 && now.Sub(p.created) > e.s.Stale:
		return "stale"
	case e.topicAnswered(p.topic):
		return "already_answered"
	}
	return e.capBlocked(now)
}

// proceed moves a candidate on once the wait and retrieval are both done.
func (e *Engine) proceed(p *pending) {
	if !p.waited || !p.retrieved || p.asking {
		return
	}
	now := e.d.Now()
	if !p.addressed {
		if reason := e.unpromptedBlocked(p, now); reason != "" {
			e.finish(p, reason)
			return
		}
		if len(p.snips) == 0 {
			e.finish(p, "no_hits")
			return
		}
	}
	if e.exhausted.Load() {
		e.finish(p, "budget")
		return
	}
	if e.d.LLM == nil {
		e.emit(Reply{Kind: replyKind(p.trig), Text: fallbackNoModel, Trigger: TriggerAddressed, Question: p.line.Text, VoiceWanted: p.trig.VoiceWanted})
		e.finish(p, "")
		return
	}
	p.asking = true
	mode, timeout := modeQuestion, e.s.UnpromptedTimeout
	var recent []recentLine
	if p.addressed {
		mode, timeout, recent = modeAddressed, e.s.AddressedTimeout, e.recentLines()
	}
	system := SystemPrompt(e.s.Persona)
	prompt := answerPrompt(mode, p.line.Who, p.line.Text, p.snips, recent, p.addressed && p.trig.VoiceWanted)
	id := p.id
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, timeout)
		defer cancel()
		raw, err := e.d.LLM.Complete(ctx, system, prompt)
		var a Answer
		if err == nil {
			a, err = ParseAnswer(raw)
		}
		select {
		case e.answers <- answered{id: id, ans: a, err: err}:
		case <-e.ctx.Done():
		}
	}()
}

func (e *Engine) onAnswer(p *pending, a answered) {
	now := e.d.Now()
	text := ClipChat(a.ans.Answer)
	if p.addressed {
		e.onAddressedAnswer(p, a)
		return
	}
	switch {
	case a.err != nil:
		e.finish(p, "model_failed")
		return
	case text == "" || a.ans.Confidence < e.s.MinConfidence:
		e.finish(p, "low_confidence")
		return
	}
	if reason := e.unpromptedBlocked(p, now); reason != "" {
		e.finish(p, reason)
		return
	}
	if e.emit(Reply{Text: text, Trigger: TriggerQuestion, Question: p.line.Text, NeedsResearch: a.ans.NeedsResearch, Sources: a.ans.Sources}) {
		e.posts = append(e.posts, now)
		e.lastPost = now
		e.total++
	}
	e.rememberTopic(p.topic)
	e.rememberTopic(TopicKey(text))
	e.finish(p, "")
}

// onOffer handles one of Spek's chat posts: posted unless the topic is
// covered, and it settles room questions on the same topic so the meeting
// never gets two answers.
func (e *Engine) onOffer(text string) {
	topic := TopicKey(text)
	for _, t := range e.topics {
		if len(topic) > 0 && (Jaccard(t, topic) >= dedupeJaccard || containment(t, topic) >= offerContainment) {
			e.drop("offer_duplicate")
			return
		}
	}
	for _, p := range e.pending {
		if !p.addressed && (Jaccard(p.topic, topic) >= dedupeJaccard || containment(p.topic, topic) >= offerContainment) {
			p.answered = true
			e.rememberTopic(p.topic)
		}
	}
	if e.emit(Reply{Text: text, Trigger: TriggerOffer}) {
		e.rememberTopic(topic)
	}
}

// onAddressedAnswer replies to an addressed request: spoken (two
// sentences, no links) when voice was wanted, else in chat. An addressed
// question the model could not answer well becomes a deep dive when the
// meeting has a research path: she says she will dig in, the session
// dispatches the research, and FollowUp reports back once.
func (e *Engine) onAddressedAnswer(p *pending, a answered) {
	clipFn := ClipChat
	if p.trig.VoiceWanted {
		clipFn = ClipVoice
	}
	text := clipFn(a.ans.Answer)
	if a.err != nil || text == "" {
		e.log.Info("[meetings] persona answer unavailable; replying unsure", "error", a.err)
		text = fallbackUnsure
	}
	rep := Reply{Kind: replyKind(p.trig), Text: text, Trigger: TriggerAddressed, Question: p.line.Text, VoiceWanted: p.trig.VoiceWanted,
		NeedsResearch: a.ans.NeedsResearch, Sources: a.ans.Sources}
	weak := a.ans.NeedsResearch || a.ans.Confidence < e.s.MinConfidence
	// The hive's own state was in the context: the answer is the hive's
	// live data, and the research agents (public web only) know less.
	live := hasHiveStatus(p.snips)
	if a.err == nil && weak && !live && e.d.CanResearch && !p.thoughts && e.isQuestion(p) {
		dig := phrasesFor(e.s.Persona.Language).digIn
		rep.Text = dig
		if answer := clipFn(a.ans.Answer); answer != "" && a.ans.Confidence >= e.s.MinConfidence {
			rep.Text = answer + " " + dig
		}
		rep.NeedsResearch = true
		rep.Research = clip(researchQuery(p), maxResearchQuery)
	}
	e.emit(rep)
	e.rememberTopic(p.topic)
	e.rememberTopic(TopicKey(text))
	e.finish(p, "")
}

// maxResearchQuery bounds a deep dive's query.
const maxResearchQuery = 300

// isQuestion reports whether an addressed request asks something.
func (e *Engine) isQuestion(p *pending) bool {
	return strings.Contains(p.line.Text, "?") || IsQuestion(p.trig.Rest) || IsQuestion(p.line.Text)
}

// researchQuery is the request as said, for the research agent.
func researchQuery(p *pending) string {
	return strings.TrimSpace(p.line.Text)
}
