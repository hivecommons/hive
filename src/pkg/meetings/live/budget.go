package live

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Budget steps for a meeting (hivecommons/hive#18914). They are fixed
// behaviour, not settings: the owner sets the budget itself
// (meetings.budget.max_tokens_per_meeting and the other ceilings).
const (
	// BudgetWarnPercent is where the meeting gets one warn log line and the
	// dashboard shows a badge.
	BudgetWarnPercent = 80
	// BudgetDegradePercent is where the attendee shortens answers and stops
	// starting long research runs, saying so once.
	BudgetDegradePercent = 90
	// SkippedKept bounds the skipped questions kept with their text per
	// meeting; SkippedCount keeps counting past it.
	SkippedKept = 200
	// maxSkippedText bounds one kept question.
	maxSkippedText = 500
	// percentBase turns a ratio into a percent.
	percentBase = 100
)

// Budget levels, as the meter reports them.
const (
	BudgetLevelOK        = "ok"
	BudgetLevelWarning   = "warning"
	BudgetLevelDegraded  = "degraded"
	BudgetLevelExhausted = "exhausted"
)

// Budget exhaustion reasons.
const (
	BudgetReasonTokens      = "tokens"
	BudgetReasonIntents     = "intents"
	BudgetReasonHiveCeiling = "hive_ceiling"
)

// SkippedQuestion is a question the meeting asked that was not answered
// because the budget was spent. The text is meeting speech: it belongs in
// the meeting's own records (session.json, the inbox record), never in
// logs or audit, which carry counts only.
type SkippedQuestion struct {
	At        time.Time `json:"at"`
	Who       string    `json:"who,omitempty"`
	Text      string    `json:"text"`
	Addressed bool      `json:"addressed,omitempty"`
}

// BudgetView is one meeting's budget meter. It carries numbers only, never
// question text, so any role may read it.
type BudgetView struct {
	MeetingID       string `json:"meeting_id,omitempty"`
	TokensUsed      int    `json:"tokens_used"`
	TokenBudget     int    `json:"token_budget"`
	Percent         int    `json:"percent"`
	IntentsAnswered int    `json:"intents_answered"`
	IntentBudget    int    `json:"intent_budget"`
	Level           string `json:"level"`
	Warned          bool   `json:"warned"`
	Degraded        bool   `json:"degraded"`
	Exhausted       bool   `json:"exhausted"`
	ExhaustedBy     string `json:"exhausted_by,omitempty"`
	// Skipped counts the questions not answered because of the budget.
	Skipped int `json:"skipped"`
	// Dropped counts everything the live loop dropped, for any reason.
	Dropped int `json:"dropped"`
}

// BudgetOverview is the hive's meetings budget: the per-meeting limits, the
// hive-wide hourly and daily ceilings with what is used, and each meeting's
// meter.
type BudgetOverview struct {
	TokenBudget  int          `json:"token_budget"`
	IntentBudget int          `json:"intent_budget"`
	Ledger       LedgerUsage  `json:"ledger"`
	Meetings     []BudgetView `json:"meetings"`
}

// budgetPercent is how much of the meeting budget is used: the larger of
// the token and intent shares, 100 once exhausted, never above 100.
func budgetPercent(p Policy, st State) int {
	if st.BudgetExhausted {
		return percentBase
	}
	pct := 0
	if p.MaxTokensPerMeeting > 0 {
		pct = st.Tokens * percentBase / p.MaxTokensPerMeeting
	}
	if p.MaxIntentsPerMeeting > 0 {
		if i := st.Intents * percentBase / p.MaxIntentsPerMeeting; i > pct {
			pct = i
		}
	}
	if pct > percentBase {
		pct = percentBase
	}
	return pct
}

// BudgetLevelFor names the level a percent (and exhaustion) is at.
func BudgetLevelFor(percent int, exhausted bool) string {
	switch {
	case exhausted:
		return BudgetLevelExhausted
	case percent >= BudgetDegradePercent:
		return BudgetLevelDegraded
	case percent >= BudgetWarnPercent:
		return BudgetLevelWarning
	}
	return BudgetLevelOK
}

// NewBudgetView builds the meter for a meeting's state under a policy.
func NewBudgetView(p Policy, st State, id string) BudgetView {
	pct := budgetPercent(p, st)
	return BudgetView{
		MeetingID: id, TokensUsed: st.Tokens, TokenBudget: p.MaxTokensPerMeeting, Percent: pct,
		IntentsAnswered: st.Intents, IntentBudget: p.MaxIntentsPerMeeting,
		Level:  BudgetLevelFor(pct, st.BudgetExhausted),
		Warned: st.BudgetWarned, Degraded: st.BudgetDegraded, Exhausted: st.BudgetExhausted,
		ExhaustedBy: st.BudgetReason, Skipped: st.SkippedCount, Dropped: st.Dropped,
	}
}

// NewBudgetOverview assembles the hive view from the per-meeting states
// (keyed by meeting id, in the order of ids).
func (r *Runner) NewBudgetOverview(ids []string, state func(id string) (State, bool)) BudgetOverview {
	out := BudgetOverview{
		TokenBudget: r.opts.Policy.MaxTokensPerMeeting, IntentBudget: r.opts.Policy.MaxIntentsPerMeeting,
		Ledger: r.opts.Ledger.Usage(r.opts.Now()), Meetings: []BudgetView{},
	}
	for _, id := range ids {
		if st, ok := state(id); ok {
			out.Meetings = append(out.Meetings, NewBudgetView(r.opts.Policy, st, id))
		}
	}
	return out
}

// RenderSkippedNote renders the "not answered: budget" note for a
// meeting's inbox record; empty when nothing was skipped.
func RenderSkippedNote(st State) string {
	if st.SkippedCount == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Not answered: budget\n\n")
	fmt.Fprintf(&b, "The meeting budget ran out, so %d question(s) were not answered.\n\n", st.SkippedCount)
	for _, q := range st.Skipped {
		who := strings.TrimSpace(q.Who)
		if who == "" {
			who = "someone"
		}
		fmt.Fprintf(&b, "- %s: %s\n", who, strings.TrimSpace(q.Text))
	}
	if more := st.SkippedCount - len(st.Skipped); more > 0 {
		fmt.Fprintf(&b, "- and %d more not listed\n", more)
	}
	return b.String()
}

// skipQueue carries skipped questions from any goroutine (the persona
// engine's, the session's own) to the session goroutine, which owns State.
type skipQueue struct {
	mu sync.Mutex
	in []SkippedQuestion
}

func (q *skipQueue) push(sq SkippedQuestion) {
	q.mu.Lock()
	q.in = append(q.in, sq)
	q.mu.Unlock()
}

func (q *skipQueue) take() []SkippedQuestion {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.in
	q.in = nil
	return out
}

// queueSkip records a question dropped for budget. It is safe from any
// goroutine and never loses the question: the session merges it into its
// state on its next turn (onSkips), save or snapshot.
func (s *session) queueSkip(who, text string, addressed bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.skips.push(SkippedQuestion{At: s.now(), Who: who, Text: truncate(text, maxSkippedText), Addressed: addressed})
	select {
	case s.skipWake <- struct{}{}:
	default:
	}
}

// mergeSkips moves queued skipped questions into the state.
func (s *session) mergeSkips() {
	for _, q := range s.skips.take() {
		s.state.SkippedCount++
		if len(s.state.Skipped) < SkippedKept {
			s.state.Skipped = append(s.state.Skipped, q)
		}
		// Counts only on the panel's stream: the text stays in the state.
		s.emit(Event{Kind: EventDropped, Reason: "budget"})
	}
}

// onSkips merges queued skips and, once exhausted, repeats the budget
// notice with the running count each time the count doubles (1, 2, 4, ...),
// so the room learns questions are being skipped without a post per one.
func (s *session) onSkips(ctx context.Context) {
	s.mergeSkips()
	if s.state.BudgetExhausted && s.state.SkippedCount >= s.nextSkipNotice {
		for s.nextSkipNotice <= s.state.SkippedCount {
			s.nextSkipNotice *= 2
		}
		s.sayLine(ctx, TriggerNotice, false, budgetSkipNotice(s.personaName(), s.state.SkippedCount))
	}
	s.save()
}

// nextNoticeAfter is the first notice count above n: the powers of two.
func nextNoticeAfter(n int) int {
	next := 1
	for next <= n {
		next *= 2
	}
	return next
}

// budgetSteps takes the 80 and 90 percent steps once each as the meeting
// uses its budget; exhaustion is handled by account.
func (s *session) budgetSteps(ctx context.Context) {
	pct := budgetPercent(s.r.opts.Policy, s.state)
	if pct >= BudgetWarnPercent && !s.state.BudgetWarned {
		s.state.BudgetWarned = true
		// Ids and numbers only: no transcript or question text.
		s.log().Warn(fmt.Sprintf("[meetings] meeting budget at %d%%", BudgetWarnPercent),
			"tokens", s.state.Tokens, "budget", s.r.opts.Policy.MaxTokensPerMeeting,
			"intents", s.state.Intents, "percent", pct)
		s.emit(Event{Kind: EventBudget, Reason: BudgetLevelWarning})
	}
	if pct >= BudgetDegradePercent && !s.state.BudgetDegraded {
		s.state.BudgetDegraded = true
		s.log().Info(fmt.Sprintf("[meetings] meeting budget at %d%%; shortening answers and not starting research", BudgetDegradePercent),
			"tokens", s.state.Tokens, "percent", pct)
		s.emit(Event{Kind: EventBudget, Reason: BudgetLevelDegraded})
		if s.persona != nil {
			s.persona.SetDegraded()
			s.sayLine(ctx, TriggerNotice, false, budgetDegradeNotice(s.personaName()))
		}
	}
}

// budgetNotice is posted once when a meeting's budget runs out.
func budgetNotice(name string) string {
	return name + " reached the budget for this meeting and keeps taking notes, but will not ask or answer questions or start research. " + budgetNextStep
}

// budgetSkipNotice repeats it with the questions skipped so far.
func budgetSkipNotice(name string, skipped int) string {
	return fmt.Sprintf("%s is out of budget for this meeting: %d question(s) not answered so far. %s", name, skipped, budgetNextStep)
}

// budgetNextStep is what to do about it.
const budgetNextStep = "Ask the owner to raise the budget or start a new meeting."

// budgetDegradeNotice is said once at BudgetDegradePercent.
func budgetDegradeNotice(name string) string {
	return fmt.Sprintf("%s has used %d%% of this meeting's budget: answers will be shorter and no new long research runs will start.", name, BudgetDegradePercent)
}

// Budget is one meeting's meter: the running session's, or the stored
// one after the meeting ended.
func (r *Runner) Budget(meetingID string) (BudgetView, error) {
	st, err := r.State(meetingID)
	if err != nil {
		return BudgetView{}, err
	}
	return NewBudgetView(r.opts.Policy, st, meetingID), nil
}

// Policy is the policy the runner applies to every meeting.
func (r *Runner) Policy() Policy { return r.opts.Policy }
