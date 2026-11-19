package betty

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/meetings/actions"
)

// Action requests (hivecommons/hive#18406): "Betty, open an issue for the
// lobby timeout", "make that an RFC", "start a Spek session on calendar
// sync", "save that as a knowledge note". The persona drafts the item from
// the request and the recent transcript and hands it to the approval queue;
// she never creates anything herself, and her confirmation says the draft
// is waiting for approval. An unclear request gets one clarifying question
// first.

// Action reply triggers.
const (
	TriggerAction        = "action"
	TriggerActionClarify = "action_clarify"
)

// Action timing and limits.
const (
	// DefaultDraftTimeout bounds the model call that drafts an item.
	DefaultDraftTimeout = 6 * time.Second
	// clarifyWindow is how long a clarifying question waits for its answer.
	clarifyWindow = 2 * time.Minute
	// excerptLines is the transcript kept with a draft.
	excerptLines = 8
	// maxDraftTitleRunes bounds a drafted title.
	maxDraftTitleRunes = 90
	// maxDraftBodyRunes bounds a drafted body.
	maxDraftBodyRunes = 1500
	// maxQuotedTitleRunes bounds the title quoted in a chat confirmation.
	maxQuotedTitleRunes = 120
)

// Action replies. None of them says the item exists: only an approval
// creates it, and the result is posted separately.
const (
	actionWaiting   = "Drafted %s: '%s'. It's waiting for approval in the hive."
	actionAuto      = "Drafted %s: '%s'. Knowledge notes are saved without approval on this hive; I'll post here once it's saved."
	actionFailed    = "Sorry, I couldn't queue that draft for approval."
	actionTypeOff   = "Sorry, drafting %ss from meetings is turned off on this hive."
	actionClarify   = "Happy to draft %s. What should it be about?"
	actionCancelled = "Okay, no draft then."
)

// ActionSink is the approval queue as one meeting sees it
// (*actions.MeetingSink).
type ActionSink interface {
	Settings() actions.Settings
	Submit(d actions.Draft) (actions.Draft, error)
}

// actionState is the engine's action-request state.
type actionState struct {
	sink ActionSink
	mu   sync.Mutex
	// ask is the clarifying question waiting for its answer.
	ask *actionAsk
}

// actionAsk is one clarifying question.
type actionAsk struct {
	typ     actions.Type
	line    Line
	at      time.Time
	context []recentLine
}

// SetActions turns action requests on with a sink; call it before Start.
func (e *Engine) SetActions(s ActionSink) { e.act.sink = s }

// actionVerbs ask for something to be made.
var actionVerbs = map[string]bool{
	"open": true, "file": true, "create": true, "make": true, "start": true, "raise": true, "log": true,
	"write": true, "draft": true, "save": true, "add": true, "record": true, "store": true, "turn": true,
	"put": true, "kick": true, "begin": true, "launch": true, "capture": true, "keep": true, "spin": true,
	"take": true, "jot": true, "submit": true, "note": true,
}

// notActionStarts begin a question about an item, not a request for one.
var notActionStarts = map[string]bool{
	"what": true, "whats": true, "what's": true, "why": true, "how": true, "when": true, "where": true,
	"who": true, "which": true, "is": true, "are": true, "was": true, "were": true, "does": true, "did": true,
	"has": true, "have": true, "had": true,
}

// negations cancel a request ("don't open an issue").
var negations = map[string]bool{"don't": true, "dont": true, "not": true, "never": true, "no": true, "without": true}

// subjectLead is the one leading word dropped from a subject.
var subjectLead = map[string]bool{
	"for": true, "on": true, "about": true, "to": true, "regarding": true, "re": true, "covering": true,
	"called": true, "titled": true, "named": true, "saying": true, "of": true, "that": true,
}

// pronounSubjects point at the discussion rather than naming a subject.
var pronounSubjects = map[string]bool{
	"": true, "that": true, "this": true, "it": true, "that one": true, "this one": true, "those": true,
	"these": true, "them": true, "the above": true, "what we just said": true, "what we discussed": true,
}

// trailingFiller is dropped from the end of a subject.
var trailingFiller = map[string]bool{"please": true, "thanks": true, "now": true, "too": true}

// cancelPhrases answer a clarifying question with "never mind".
var cancelPhrases = []string{"never mind", "nevermind", "cancel", "forget it", "no thanks", "skip it"}

// DetectAction reads an addressed request (the words after the persona's
// name) for an action: the type and the subject it names, empty when it
// points at the discussion ("make that an RFC").
func DetectAction(rest string) (actions.Type, string, bool) {
	w := strings.Fields(strings.ToLower(rest))
	if len(w) == 0 || notActionStarts[w[0]] {
		return "", "", false
	}
	for i := range w {
		typ, skip, ok := actionNoun(w, i)
		if !ok {
			continue
		}
		verb, neg := false, false
		for _, x := range w[:i] {
			verb = verb || actionVerbs[x]
			neg = neg || negations[x]
		}
		if !verb || neg {
			return "", "", false
		}
		return typ, subjectOf(w[i+1+skip:]), true
	}
	return "", "", false
}

// actionNoun reports whether w[i] names an action type, and how many words
// after it belong to the noun ("spek session", "knowledge note").
func actionNoun(w []string, i int) (actions.Type, int, bool) {
	next := ""
	if i+1 < len(w) {
		next = w[i+1]
	}
	switch w[i] {
	case "issue", "bug", "ticket":
		return actions.TypeIssue, 0, true
	case "rfc":
		return actions.TypeRFC, 0, true
	case "spek", "speck", "spektacular", "inception":
		if next == "session" {
			return actions.TypeSpek, 1, true
		}
		return actions.TypeSpek, 0, true
	case "spec":
		if next == "session" {
			return actions.TypeSpek, 1, true
		}
		if plainSpekRequest(w, i) {
			return actions.TypeSpek, 0, true
		}
	case "specs", "design", "designs":
		if plainSpekRequest(w, i) {
			if next == "doc" || next == "document" || next == "session" {
				return actions.TypeSpek, 1, true
			}
			return actions.TypeSpek, 0, true
		}
	case "knowledge":
		switch next {
		case "note", "item", "fact", "entry", "base":
			return actions.TypeKnowledge, 1, true
		}
		return actions.TypeKnowledge, 0, true
	case "note", "fact":
		return actions.TypeKnowledge, 0, true
	}
	return "", 0, false
}

// maxWordsBeforeSpekNoun bounds the words between the verb and "spec" or
// "design" ("write up a quick spec").
const maxWordsBeforeSpekNoun = 4

// requestLeads may open a request before its verb ("can you please start").
var requestLeads = map[string]bool{
	"please": true, "can": true, "could": true, "would": true, "will": true, "you": true,
	"go": true, "ahead": true, "and": true, "just": true, "now": true, "also": true,
	"let's": true, "lets": true, "let": true, "us": true,
}

// notSpekDesignFollows are words after "design" that name something else
// ("design review", "design meeting").
var notSpekDesignFollows = map[string]bool{
	"review": true, "reviews": true, "meeting": true, "meetings": true, "decision": true, "decisions": true,
	"choice": true, "choices": true, "pattern": true, "patterns": true,
}

// plainSpekRequest reports whether "spec" or "design" at w[i] is the object of
// a request: only request lead words, then an action verb, then a few words.
// "the spec says" and "per the design" are talk about a spec, not a request.
func plainSpekRequest(w []string, i int) bool {
	if i+1 < len(w) && notSpekDesignFollows[w[i+1]] {
		return false
	}
	for v := 0; v < i; v++ {
		if actionVerbs[w[v]] {
			for _, x := range w[:v] {
				if !requestLeads[x] {
					return false
				}
			}
			return i-v-1 <= maxWordsBeforeSpekNoun
		}
	}
	return false
}

func subjectOf(w []string) string {
	if len(w) > 0 && subjectLead[w[0]] {
		w = w[1:]
	}
	for len(w) > 0 && trailingFiller[w[len(w)-1]] {
		w = w[:len(w)-1]
	}
	s := strings.Join(w, " ")
	if pronounSubjects[s] {
		return ""
	}
	return s
}

// repoMention finds a monitored repo named in a request: its owner/repo
// anywhere, or its short name right after "in", "to", "on" or "for", or
// before "repo".
func repoMention(text string, repos []string) string {
	low := strings.ToLower(text)
	for _, r := range repos {
		if strings.Contains(low, strings.ToLower(r)) {
			return r
		}
	}
	w := words(text)
	for _, r := range repos {
		short := strings.ToLower(r[strings.LastIndex(r, "/")+1:])
		for i, x := range w {
			if x != short {
				continue
			}
			before := i > 0 && (w[i-1] == "in" || w[i-1] == "to" || w[i-1] == "on" || w[i-1] == "for")
			after := i+1 < len(w) && (w[i+1] == "repo" || w[i+1] == "repository")
			if before || after {
				return r
			}
		}
	}
	return ""
}

// actionAddress reads a line that opens with one of the persona's names
// followed straight by an action verb, the way speech-to-text writes a
// request without the comma ("betty open an issue for the lobby timeout").
func (e *Engine) actionAddress(text string) (string, bool) {
	w := normalize(words(text))
	for _, name := range e.match.names {
		start := 0
		if len(w) > 0 && greetings[w[0]] && hasPrefix(w[1:], name) {
			start = 1
		}
		if !hasPrefix(w[start:], name) {
			continue
		}
		after := w[start+len(name):]
		if len(after) > 1 && after[0] == "please" {
			after = after[1:]
		}
		if len(after) > 0 && actionVerbs[after[0]] {
			return strings.Join(after, " "), true
		}
	}
	return "", false
}

// contextLines is the recent transcript before the current line.
func (e *Engine) contextLines() []recentLine {
	all := e.recentLines()
	if len(all) == 0 {
		return nil
	}
	return all[:len(all)-1]
}

// send posts a reply from a draft goroutine; it gives up when the engine
// stops.
func (e *Engine) send(r Reply) {
	if r.Kind == "" {
		r.Kind = KindChat
	}
	select {
	case e.out <- r:
	case <-e.ctx.Done():
	}
}

// onActionLine handles a line as an action request, or as the answer to
// the persona's clarifying question. It reports whether it took the line.
func (e *Engine) onActionLine(l Line, trig Trigger, now time.Time) bool {
	if e.act.sink == nil {
		return false
	}
	if !trig.Addressed && !trig.Intro {
		if rest, ok := e.actionAddress(l.Text); ok {
			trig.Addressed, trig.Rest = true, rest
		}
	}
	typ, subject, isAction := "", "", false
	if trig.Addressed && !trig.Intro {
		var t actions.Type
		t, subject, isAction = DetectAction(trig.Rest)
		typ = string(t)
	}
	e.act.mu.Lock()
	ask := e.act.ask
	if ask != nil && now.Sub(ask.at) > clarifyWindow {
		ask, e.act.ask = nil, nil
	}
	if ask != nil && !isAction && strings.EqualFold(strings.TrimSpace(ask.line.Who), strings.TrimSpace(l.Who)) {
		e.act.ask = nil
		e.act.mu.Unlock()
		answer := l.Text
		if trig.Addressed {
			answer = trig.Rest
		}
		low := strings.ToLower(answer)
		for _, p := range cancelPhrases {
			if strings.Contains(low, p) {
				e.emit(Reply{Text: actionCancelled, Trigger: TriggerActionClarify, Question: l.Text})
				return true
			}
		}
		subject := strings.TrimSpace(answer)
		if pronounSubjects[strings.ToLower(strings.Trim(subject, ".!?,; "))] {
			subject = ""
		}
		e.startDraft(ask.typ, ask.line, subject, ask.context, &recentLine{Who: l.Who, Text: l.Text}, false)
		return true
	}
	e.act.mu.Unlock()
	if !isAction {
		return false
	}
	t := actions.Type(typ)
	st := e.act.sink.Settings()
	if !st.Enabled {
		// Turned off: an ordinary addressed request.
		return false
	}
	if !st.Allows(t) {
		e.emit(Reply{Text: fmt.Sprintf(actionTypeOff, t.Noun()), Trigger: TriggerAction, Question: l.Text, VoiceWanted: trig.VoiceWanted})
		return true
	}
	ctxLines := e.contextLines()
	if subject == "" && len(ctxLines) == 0 {
		e.emit(e.askClarify(t, l, ctxLines, now))
		return true
	}
	e.startDraft(t, l, subject, ctxLines, nil, true)
	return true
}

// askClarify remembers the one clarifying question and returns it; the
// caller posts it.
func (e *Engine) askClarify(t actions.Type, l Line, ctxLines []recentLine, now time.Time) Reply {
	e.act.mu.Lock()
	e.act.ask = &actionAsk{typ: t, line: l, at: now, context: ctxLines}
	e.act.mu.Unlock()
	return Reply{Text: fmt.Sprintf(actionClarify, t.WithArticle()), Trigger: TriggerActionClarify, Question: l.Text}
}

// draftReply is the model's draft.
type draftReply struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Repo     string `json:"repo"`
	Clear    *bool  `json:"clear"`
	Question string `json:"question"`
}

// startDraft drafts the item off the engine goroutine, queues it and
// confirms. answer is the reply to the clarifying question, when there was
// one; allowAsk lets the model ask that one question.
func (e *Engine) startDraft(t actions.Type, req Line, subject string, ctxLines []recentLine, answer *recentLine, allowAsk bool) {
	st := e.act.sink.Settings()
	repo := repoMention(req.Text+" "+subject, st.Repos)
	excerpt := renderExcerpt(ctxLines, req, answer)
	useModel := e.d.LLM != nil && !e.exhausted.Load()
	at := e.d.Now()
	go func() {
		title, body := ruleDraft(t, req, subject, ctxLines)
		if useModel {
			ctx, cancel := context.WithTimeout(e.ctx, DefaultDraftTimeout)
			raw, err := e.d.LLM.Complete(ctx, SystemPrompt(e.s.Persona), draftPrompt(t, req, subject, ctxLines, answer, st.Repos, allowAsk))
			cancel()
			if dr, ok := parseDraft(raw); err == nil && ok {
				if allowAsk && dr.Clear != nil && !*dr.Clear && strings.TrimSpace(dr.Question) != "" {
					e.act.mu.Lock()
					e.act.ask = &actionAsk{typ: t, line: req, at: at, context: ctxLines}
					e.act.mu.Unlock()
					e.send(Reply{Text: ClipChat(dr.Question), Trigger: TriggerActionClarify, Question: req.Text})
					return
				}
				if dr.Title != "" {
					title = dr.Title
				}
				if dr.Body != "" {
					body = dr.Body
				}
				if repo == "" {
					repo = dr.Repo
				}
			} else if err != nil {
				e.log.Info("[meetings] persona draft model unavailable; drafting from rules", "error", err)
			}
		}
		if title == "" && allowAsk {
			e.send(e.askClarify(t, req, ctxLines, at))
			return
		}
		if title == "" {
			title = "Follow-up from the meeting"
		}
		d, err := e.act.sink.Submit(actions.Draft{
			Type: t, Title: clip(title, maxDraftTitleRunes), Body: clipKeepLines(body, maxDraftBodyRunes), Repo: repo,
			Excerpt: excerpt, RequestedBy: req.Who, Request: req.Text, Spoken: req.Spoken,
		})
		if err != nil {
			e.log.Warn("[meetings] persona draft not queued", "type", t, "error", err)
			e.send(Reply{Text: actionFailed, Trigger: TriggerAction, Question: req.Text})
			return
		}
		format := actionWaiting
		if d.AutoApproved {
			format = actionAuto
		}
		e.send(Reply{Text: fmt.Sprintf(format, t.WithArticle(), clip(d.Title, maxQuotedTitleRunes)), Trigger: TriggerAction, Question: req.Text})
	}()
}

// ruleDraft drafts without a model: the subject (or the last thing said)
// as the title, the request and the discussion as the body.
func ruleDraft(t actions.Type, req Line, subject string, ctxLines []recentLine) (string, string) {
	title := subject
	last := ""
	if len(ctxLines) > 0 {
		last = strings.TrimSpace(ctxLines[len(ctxLines)-1].Text)
	}
	if title == "" {
		title = last
	}
	title = capitalize(clip(title, maxDraftTitleRunes))
	var b strings.Builder
	who := strings.TrimSpace(req.Who)
	if who == "" {
		who = "Someone"
	}
	fmt.Fprintf(&b, "%s asked for %s in the meeting: %q.", who, t.WithArticle(), clip(req.Text, maxDraftTitleRunes*2))
	if last != "" && last != subject {
		fmt.Fprintf(&b, "\n\nFrom the discussion: %s", clip(last, maxDraftBodyRunes/2))
	}
	return title, b.String()
}

func capitalize(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// clipKeepLines bounds text but keeps its line breaks.
func clipKeepLines(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// renderExcerpt is the transcript kept with a draft: the last few lines,
// the request and the answer to a clarifying question.
func renderExcerpt(ctxLines []recentLine, req Line, answer *recentLine) string {
	lines := ctxLines
	if len(lines) > excerptLines {
		lines = lines[len(lines)-excerptLines:]
	}
	all := append(append([]recentLine{}, lines...), recentLine{Who: req.Who, Text: req.Text})
	if answer != nil {
		all = append(all, *answer)
	}
	var b strings.Builder
	for _, l := range all {
		who := strings.TrimSpace(l.Who)
		if who == "" {
			who = "unknown"
		}
		fmt.Fprintf(&b, "%s: %s\n", who, strings.Join(strings.Fields(l.Text), " "))
	}
	return strings.TrimSpace(b.String())
}

// draftPrompt asks the model for a draft. The request and transcript are
// data; the type is fixed by the request and the repo is checked against
// the monitored list afterwards, so neither can be steered from the data.
func draftPrompt(t actions.Type, req Line, subject string, ctxLines []recentLine, answer *recentLine, repos []string, allowAsk bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Someone in the meeting asked you to draft %s for the hive. Draft it from their request and the recent transcript. It will wait for a person to approve it; do not say it was created.\n", t.WithArticle())
	b.WriteString("Write a short, specific title (under 80 characters, no prefix like \"Issue:\") and a short plain Markdown body (two to five sentences) saying what was discussed and what is wanted. Do not invent facts that are not in the data.\n")
	if t.NeedsRepo() && len(repos) > 0 {
		fmt.Fprintf(&b, "If the request or transcript clearly names one of these repositories, set repo to it; otherwise leave repo empty: %s.\n", strings.Join(repos, ", "))
	}
	if allowAsk {
		b.WriteString("If the data does not say what the item is about, set clear to false and write one short clarifying question.\n")
	} else {
		b.WriteString("Set clear to true and draft from what you have.\n")
	}
	b.WriteString("\n" + dataOpen + "\n")
	fmt.Fprintf(&b, "Request from %s: %s\n", sanitize(req.Who), sanitize(req.Text))
	if subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", sanitize(subject))
	}
	if answer != nil {
		fmt.Fprintf(&b, "Their answer to your clarifying question: %s\n", sanitize(answer.Text))
	}
	if len(ctxLines) > 0 {
		b.WriteString("\nRecent transcript:\n")
		for _, l := range ctxLines {
			fmt.Fprintf(&b, "%s: %s\n", sanitize(l.Who), sanitize(l.Text))
		}
	}
	b.WriteString(dataClose + "\n\n")
	b.WriteString(`Reply with only a JSON object: {"title": "<title>", "body": "<body>", "repo": "<owner/repo or empty>", "clear": <true or false>, "question": "<clarifying question or empty>"}`)
	return b.String()
}

// parseDraft reads the model's JSON draft.
func parseDraft(raw string) (draftReply, bool) {
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		return draftReply{}, false
	}
	var d draftReply
	if err := json.Unmarshal([]byte(raw[i:j+1]), &d); err != nil {
		return draftReply{}, false
	}
	d.Title = strings.Trim(strings.Join(strings.Fields(d.Title), " "), "\"'")
	d.Body = strings.TrimSpace(d.Body)
	d.Repo = strings.Trim(strings.TrimSpace(d.Repo), "/")
	return d, true
}
