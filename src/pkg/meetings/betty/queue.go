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
	// overlapped: another request was in flight at the same time, so its
	// answer names who and what (attribution.go).
	overlapped bool
	// stages are the request's stage marks (timing.go).
	stages stageTimes
}

type retrieval struct {
	id    int
	snips []Snippet
}

type answered struct {
	id  int
	ans Answer
	err error
	// done is when the model call returned.
	done time.Time
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

// participants are the people heard in the recent transcript (never the
// bot): a misheard wake word that is one of their names is left to them.
func (e *Engine) participants() []string {
	return speakers(e.recentLines(), e.s.Persona.DisplayName)
}

// speakers are the distinct speakers of lines, in first-heard order,
// without the bot.
func speakers(lines []recentLine, bot string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		who := strings.TrimSpace(l.Who)
		k := strings.ToLower(who)
		if who == "" || seen[k] || meetings.IsBotName(who, bot) {
			continue
		}
		seen[k] = true
		out = append(out, who)
	}
	return out
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
	// Known mishearings are corrected only in a line addressed to her:
	// the trigger is matched on the corrected text, and anyone else's line
	// stays as heard in the recent transcript the model sees. (This is the
	// engine's own copy; the stored transcript and what Spek gets are the
	// line as Quorum sent it.)
	now := e.d.Now()
	corrected := NormalizeDomain(l.Text)
	trig := e.match.MatchWith(corrected, l.Spoken, e.participants())
	if trig.Addressed || trig.Intro {
		l.Text = corrected
	}
	if trig.Addressed && !trig.Intro {
		if parts := splitQuestions(l.Text); len(parts) > 1 {
			// Two questions in one line ("Betty, how many open issues are
			// there? How many pull requests are open?", one segment when
			// asked a second apart): each gets its own attributed answer.
			for i, part := range parts {
				if i > 0 {
					part = e.s.Persona.Name + ", " + part
				}
				e.onLine(Line{Who: l.Who, Text: part, Spoken: l.Spoken})
			}
			return
		}
	}
	if trig.Misheard {
		e.log.Info("[meetings] addressed through a misheard name", "who", l.Who)
	}
	if l.Spoken && (trig.Addressed || trig.Intro) && e.repeatedRequest(l.Who, requestKey(trig), now) {
		// Said again because the first answer was slow (Meet test: a
		// second intro and a research ack talked over the first intro).
		e.drop("repeated_request")
		return
	}
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
		// A stock line, timed like any other: heard now, ready now. Asked
		// by voice she says the short intro and the full one goes to chat
		// once (the session posts Text after saying Spoken); asked in chat
		// the full intro is posted only.
		rep := Reply{Kind: replyKind(trig), Text: e.s.Persona.Intro(), Trigger: TriggerIntro, Question: l.Text, VoiceWanted: trig.VoiceWanted,
			Timing: fixedTiming(now)}
		if rep.Kind == KindVoice {
			rep.Spoken = e.s.Persona.SpokenIntro()
		}
		e.emit(rep)
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
			e.listening(l, trig, now)
			e.emit(Reply{Kind: replyKind(trig), Text: fallbackHello, Trigger: TriggerAddressed, Question: l.Text, VoiceWanted: trig.VoiceWanted,
				Timing: fixedTiming(now)})
			return
		}
		thoughts = true
	}
	if e.exhausted.Load() {
		e.budgetSkip(l, true)
		return
	}
	e.listening(l, trig, now)
	if q := trig.Clarify; q != "" {
		// An unsure correction of what was heard: she asks which was meant
		// instead of spending a model call on a guess.
		e.emit(Reply{Kind: replyKind(trig), Text: q, Trigger: TriggerAddressed, Question: l.Text, VoiceWanted: trig.VoiceWanted,
			Timing: fixedTiming(now)})
		return
	}
	early := e.hadEarlyAck(l.Who, now)
	immediate := e.s.AckDelay <= 0
	acked := false
	if trig.VoiceWanted && e.d.LLM != nil && !e.takeEarlyAck(l.Who, now) && immediate {
		// Speak at once so the room knows she heard; the answer follows.
		e.emit(Reply{Kind: KindVoice, Text: e.ackFor(l.Who), Trigger: TriggerAck, Question: l.Text, VoiceWanted: true})
		acked = true
	}
	p := e.add(l, trig, TopicKey(trig.Rest), true, now)
	p.waited, p.thoughts = true, thoughts
	e.noteQueue(now)
	if trig.VoiceWanted && e.d.LLM != nil {
		if immediate || early {
			// Acknowledged from the partial transcript, or just now.
			p.stages.ackAt, p.stages.earlyAck = now, early
		} else if !acked {
			e.armAckWatchdog(p)
		}
	}
	e.retrieve(p, trig.Rest, true)
}

// armAckWatchdog acks p after AckDelay unless its answer is out by then
// (answered requests leave e.pending, so the timer finds nothing).
func (e *Engine) armAckWatchdog(p *pending) {
	id := p.id
	time.AfterFunc(e.ackDelay(p), func() {
		select {
		case e.ackTimers <- id:
		case <-e.ctx.Done():
		}
	})
}

// fastAckDelayFactor widens the ack watchdog for a request expected to be
// answered fast: a question about the hive's own state needs no search
// (in-memory status and one model call, HiveStatusBudget), so "let me
// check on that" is held until the answer is clearly late. With the
// default delay that is 6 s, past the 4 s budget; before it an ack only
// delays the answer by its own length.
const fastAckDelayFactor = 2

// ackDelay is how long p may go unanswered before its acknowledgement.
func (e *Engine) ackDelay(p *pending) time.Duration {
	if IsHiveStatusQuestion(p.trig.Rest) {
		return e.s.AckDelay * fastAckDelayFactor
	}
	return e.s.AckDelay
}

// watchdogAck says the one ack for a request still being answered.
func (e *Engine) watchdogAck(p *pending) {
	if !p.stages.ackAt.IsZero() || e.exhausted.Load() {
		return
	}
	if e.emit(Reply{Kind: KindVoice, Text: e.ackFor(p.line.Who), Trigger: TriggerAck, Question: p.line.Text, VoiceWanted: true}) {
		p.stages.ackAt = e.d.Now()
	}
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
	if e.d.LLM == nil || e.exhausted.Load() || e.s.AckDelay > 0 {
		// With the ack watchdog on, a partial line never acks: the ack
		// waits to see whether the answer is quick.
		return
	}
	l.Text = NormalizeDomain(l.Text)
	trig := e.match.MatchWith(l.Text, true, e.participants())
	if !trig.Addressed || trig.Intro || !trig.VoiceWanted || len(strings.Fields(trig.Rest)) < earlyAckMinWords {
		return
	}
	if mayBecomeIntro(trig.Rest) || e.repeatPending(l.Who, trig.Rest, e.d.Now()) {
		// "Betty, please introduce" is an intro still being said: an
		// intro never gets a research ack ("Let me check on that."). A
		// request repeated while the first is answered gets none either.
		return
	}
	if _, ok := e.actionAddress(l.Text); ok {
		return
	}
	now := e.d.Now()
	if at, ok := e.earlyAck[l.Who]; ok && now.Sub(at) < earlyAckWindow {
		return
	}
	if e.emit(Reply{Kind: KindVoice, Text: e.ackFor(l.Who), Trigger: TriggerAck, Question: l.Text, VoiceWanted: true}) {
		e.earlyAck[l.Who] = now
	}
}

// hadEarlyAck reports whether this speaker has an early ack standing,
// without clearing it.
func (e *Engine) hadEarlyAck(who string, now time.Time) bool {
	at, ok := e.earlyAck[who]
	return ok && now.Sub(at) < earlyAckWindow
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
		e.budgetSkip(l, false)
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
	// A question nobody addressed to her is overheard, not asked of her:
	// it never leaves the hive for the public web unless the speaker asked
	// for a web search by name.
	e.retrieve(p, l.Text, cleanQuery(l.Text).Explicit)
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
	if addressed {
		e.markOverlap(p)
	}
	return p
}

// retrieve starts retrieval at once, in parallel with the answer wait.
func (e *Engine) retrieve(p *pending, query string, allowPublic bool) {
	id, terms := p.id, p.topic
	rs := e.d.Retrievers
	if !allowPublic {
		rs = withoutPublic(rs)
	}
	if len(rs) == 0 || len(terms) == 0 {
		p.retrieved = true
		if p.waited {
			e.proceed(p)
		}
		return
	}
	go func() {
		snips := Gather(e.ctx, rs, query, terms, e.s.RetrieveTimeout)
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
		delete(e.pending, p.id)
		e.budgetSkip(p.line, p.addressed)
		return
	}
	if e.d.LLM == nil {
		rep := Reply{Kind: replyKind(p.trig), Text: fallbackNoModel, Trigger: TriggerAddressed, Question: p.line.Text, VoiceWanted: p.trig.VoiceWanted,
			Timing: e.timing(p, true, now)}
		e.attributeReply(p, &rep, "", now)
		e.emit(rep)
		e.finish(p, "")
		return
	}
	p.asking = true
	if !p.addressed {
		e.markOverlap(p)
	}
	mode, timeout := modeQuestion, e.s.UnpromptedTimeout
	var recent []recentLine
	if p.addressed {
		mode, timeout, recent = modeAddressed, e.s.AddressedTimeout, e.recentLines()
	}
	system := SystemPrompt(e.s.Persona)
	prompt := answerPrompt(mode, p.line.Who, p.line.Text, p.snips, recent, p.addressed && p.trig.VoiceWanted) + e.degradedNote()
	id := p.id
	p.stages.modelStart = now
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, timeout)
		defer cancel()
		raw, err := e.d.LLM.Complete(ctx, system, prompt)
		var a Answer
		if err == nil {
			a, err = ParseAnswer(raw)
		}
		select {
		case e.answers <- answered{id: id, ans: a, err: err, done: e.d.Now()}:
		case <-e.ctx.Done():
		}
	}()
}

func (e *Engine) onAnswer(p *pending, a answered) {
	now := e.d.Now()
	text := e.clipper(false)(a.ans.Answer)
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
	rep := Reply{Text: citeSources(e.s.Persona.Language, text, a.ans.Sources, p.snips), Trigger: TriggerQuestion, Question: p.line.Text,
		NeedsResearch: a.ans.NeedsResearch, Sources: a.ans.Sources, Timing: e.timing(p, false, now)}
	e.attributeReply(p, &rep, a.ans.Topic, now)
	if e.emit(rep) {
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
	clipFn := e.clipper(p.trig.VoiceWanted)
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
	// A question about the hive itself never goes to the research agents
	// (public web only, and slow): it is answered from the live state, or
	// she says she does not have the number.
	if a.err == nil && weak && !live && e.d.CanResearch && !e.degraded.Load() && !p.thoughts && e.isQuestion(p) && !IsHiveStatusQuestion(p.trig.Rest) &&
		!AnsweredLocally(p.snips) {
		dig := phrasesFor(e.s.Persona.Language).digIn
		rep.Text = dig
		if answer := clipFn(a.ans.Answer); answer != "" && a.ans.Confidence >= e.s.MinConfidence {
			rep.Text = answer + " " + dig
		}
		rep.NeedsResearch = true
		rep.Research = clip(researchQuery(p), maxResearchQuery)
		if rep.Text == dig && !p.stages.ackAt.IsZero() {
			// She already said "one moment" out loud: a spoken "I'll dig
			// into that" right after is a second ack back to back (Meet
			// test). It goes to chat; the follow-up is still spoken.
			rep.Kind = KindChat
		}
	}
	if a.err == nil && rep.Research == "" && text != "" {
		rep.Text = e.vouch(p, a.ans, rep.Text, live)
		if p.trig.VoiceWanted {
			rep.Links = buildLinks(a.ans.Answer, a.ans.Sources, p.snips)
		}
	}
	e.attributeReply(p, &rep, a.ans.Topic, e.d.Now())
	rep.Timing = e.timing(p, a.err != nil || a.ans.Answer == "", e.d.Now())
	e.emit(rep)
	e.rememberTopic(p.topic)
	e.rememberTopic(TopicKey(text))
	e.finish(p, "")
}

// vouch is an addressed model answer as said (#18590 accuracy): a
// low-confidence answer that is not from the hive's live state is marked
// unsure rather than stated as fact, and a chat answer names the sources
// it used.
func (e *Engine) vouch(p *pending, ans Answer, text string, live bool) string {
	lang := e.s.Persona.Language
	if !live && ans.Confidence < e.s.MinConfidence {
		text = hedge(lang, text)
	}
	if !p.trig.VoiceWanted {
		text = citeSources(lang, text, ans.Sources, p.snips)
	}
	return text
}

// maxResearchQuery bounds a deep dive's query.
const maxResearchQuery = 300

// isQuestion reports whether an addressed request asks something.
func (e *Engine) isQuestion(p *pending) bool {
	return strings.Contains(p.line.Text, "?") || IsQuestion(p.trig.Rest) || IsQuestion(p.line.Text)
}

// researchQuery is the request as said, for the research agent.
func researchQuery(p *pending) string {
	if q := strings.TrimSpace(p.trig.Normalised); q != "" {
		return q
	}
	return strings.TrimSpace(p.line.Text)
}

// minSplitWords is the least a question must have to be answered on its
// own when a line holds several.
const minSplitWords = 3

// splitQuestions splits a line holding two or more questions into them;
// a line with one question (or fragments too short to stand alone) is
// returned whole.
func splitQuestions(text string) []string {
	var parts []string
	rest := text
	for {
		i := strings.Index(rest, "?")
		if i < 0 {
			break
		}
		if q := strings.TrimSpace(rest[:i+1]); q != "" {
			parts = append(parts, q)
		}
		rest = rest[i+1:]
	}
	if tail := strings.TrimSpace(rest); tail != "" {
		parts = append(parts, tail)
	}
	if len(parts) < 2 {
		return []string{text}
	}
	for _, p := range parts {
		if len(words(p)) < minSplitWords {
			return []string{text}
		}
	}
	return parts
}

// Degraded answers: one short sentence, and a prompt note saying so, once
// the meeting has used BudgetDegradePercent of its budget.
const (
	DegradedMaxSentences = 1
	DegradedMaxChars     = 160
	degradedPromptNote   = "\n\nThe meeting's budget is nearly used: answer in one short sentence."
)

// clipper is the answer clip: the normal chat or voice clip, tightened
// when the meeting's budget is nearly used.
func (e *Engine) clipper(voice bool) func(string) string {
	base := ClipChat
	if voice {
		base = ClipVoice
	}
	if !e.degraded.Load() {
		return base
	}
	return func(text string) string {
		return clipSentences(speakable(base(text)), DegradedMaxSentences, DegradedMaxChars)
	}
}

// degradedNote is added to the answer prompt in degraded mode.
func (e *Engine) degradedNote() string {
	if e.degraded.Load() {
		return degradedPromptNote
	}
	return ""
}
