// Package live is the Spektacular live loop for a meeting
// (hivecommons/hive#18291, quorum.contract/v1 section 2): one
// `spektacular meeting --from stdin --format jsonl` child per live meeting,
// fed final segments, chat, answers and research updates, whose intents hive
// applies through a policy layer. Spek never talks to Quorum; every intent
// passes the policy here first (chat rate limit, question tracking, notes
// into the knowledge vault, research caps and approval, voice rules, LLM
// budget).
package live

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
)

// ResearchDispatcher starts one research item as a kick to the research
// agent pool and returns the kick ID its signals will carry. The real pool
// (hivecommons/hive#18288) plugs in behind this interface.
type ResearchDispatcher interface {
	Dispatch(ctx context.Context, query string, meta map[string]string) (kickID string, err error)
}

// ResearchWarmer is a dispatcher that can boot its agents ahead of the
// first request: a live meeting warms the pool when it starts and keeps it
// warm while it runs, so a question is not answered "I couldn't look that
// up" because the agent was still cold (live Discord test).
type ResearchWarmer interface {
	Warm(ctx context.Context)
}

// ResearchLoad is a dispatcher that can say its agents all have work
// already: a request dispatched then is queued behind another question, and
// the persona says so instead of leaving the room to guess
// (research.Pool.Busy).
type ResearchLoad interface {
	Busy() bool
}

// Research signal kinds: the agent's [FINDING], [COMPLETE] and [ERROR]
// output for a kick, and SignalRunning when the kick reached its agent.
const (
	SignalRunning  = "running"
	SignalFinding  = "finding"
	SignalComplete = "complete"
	SignalError    = "error"
)

// ResearchSignal is one output signal for a research kick.
type ResearchSignal struct {
	KickID string
	Kind   string
	Text   string
}

// NotesSink stores a knowledge fact, replacing an earlier fact with the same
// title (one fact per meeting, updated in place).
type NotesSink interface {
	Upsert(ctx context.Context, title, body string, tags []string) error
}

// Meetings is the slice of *meetings.Manager the loop uses.
type Meetings interface {
	Get(id string) (meetings.Meeting, error)
	PostChat(ctx context.Context, id, text string) error
	Say(ctx context.Context, id, text string) (meetings.SayResult, error)
	Subscribe(fn func(meetings.Update)) func()
}

// ConfirmedPoster is a Meetings that can confirm a chat line landed in the
// meeting (Manager.PostChatConfirmed over Quorum's chat.post confirm:true).
// With it, a post the platform accepted but never showed counts as not
// delivered, and the reply goes out by voice instead.
type ConfirmedPoster interface {
	PostChatConfirmed(ctx context.Context, id, text string) (bool, error)
}

// Leaver is a Meetings that can remove the bot from a meeting
// (Manager.Leave); the session uses it to leave a meeting it cannot reach.
type Leaver interface {
	Leave(ctx context.Context, id, reason string) (meetings.Meeting, error)
}

// Spek is one live Spektacular child (*spektacular.Stream).
type Spek interface {
	Start(ctx context.Context) error
	Send(v any) error
	SendLine(line []byte) error
	Lines() <-chan []byte
	Close(last any)
}

// SpekFactory builds the child for one meeting.
type SpekFactory func(m meetings.Meeting) Spek

// Research item statuses. pending_approval is hive-side: the item is past
// the per-meeting cap and waits for the owner in the dashboard.
const (
	ResearchPendingApproval = "pending_approval"
	ResearchQueued          = "queued"
	ResearchRunning         = "running"
	ResearchDone            = "done"
	ResearchFailed          = "failed"
)

// ResearchItem is one research request from the meeting.
type ResearchItem struct {
	RID      string   `json:"rid"`
	Query    string   `json:"query"`
	Reason   string   `json:"reason,omitempty"`
	Status   string   `json:"status"`
	KickID   string   `json:"kick_id,omitempty"`
	Findings []string `json:"findings,omitempty"`
	Summary  string   `json:"summary,omitempty"`
	// Error is why a failed item failed: the dispatch error's reason, the
	// research agent's own [ERROR] text, or "timed out".
	Error  string `json:"error,omitempty"`
	Posted bool   `json:"posted,omitempty"`
	// Betty marks a deep dive the persona started on an addressed
	// question (hivecommons/hive#18393): Spek never hears of it, and its
	// result is reported once, restyled by the persona.
	Betty bool `json:"betty,omitempty"`
	// Voice records that the persona's deep dive was asked by voice: its
	// follow-up, progress note and failure notice are spoken (chat when the
	// meeting cannot hear her).
	Voice bool `json:"voice,omitempty"`
	// ProgressSent records the one "still looking" note for a deep dive
	// that runs long (hivecommons/hive#18590).
	ProgressSent bool `json:"progress_sent,omitempty"`
	// DispatchedAt is when the item went to the research pool (after any
	// approval); the deep dive's progress note and timeout count from it.
	DispatchedAt *time.Time `json:"dispatched_at,omitempty"`
	// Waiting marks an item dispatched while every research agent had work:
	// it is queued behind another question until StartedAt.
	Waiting bool `json:"waiting,omitempty"`
	// StartedAt is when the item's kick reached its research agent.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// HeardAt is when the question behind a persona deep dive was heard;
	// its follow-up's timing counts from it.
	HeardAt   *time.Time `json:"heard_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Ask is a clarifying question hive posted for Spek.
type Ask struct {
	QID        string     `json:"qid"`
	Text       string     `json:"text"`
	AskedAt    time.Time  `json:"asked_at"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredBy string     `json:"answered_by,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`
	Expired    bool       `json:"expired,omitempty"`
}

// Note sections (contract section 2).
const (
	SectionDecisions = "decisions"
	SectionActions   = "actions"
	SectionQuestions = "questions"
	SectionSummary   = "summary"
	SectionNotes     = "notes"
)

// noteSections is the render order.
var noteSections = []string{SectionSummary, SectionDecisions, SectionActions, SectionQuestions, SectionNotes}

// State is a meeting's live-loop state, persisted as session.json beside
// the meeting record.
type State struct {
	Notes           map[string][]string `json:"notes,omitempty"`
	Research        []ResearchItem      `json:"research,omitempty"`
	Asks            []Ask               `json:"asks,omitempty"`
	Intents         int                 `json:"intents"`
	Tokens          int                 `json:"tokens"`
	BudgetExhausted bool                `json:"budget_exhausted,omitempty"`
	Dropped         int                 `json:"dropped"`
	// BudgetWarned and BudgetDegraded record that the 80 and 90 percent
	// steps were taken, so each happens once per meeting even across a
	// restart (budget.go).
	BudgetWarned   bool `json:"budget_warned,omitempty"`
	BudgetDegraded bool `json:"budget_degraded,omitempty"`
	// BudgetReason is what ran out: "tokens", "intents" or "hive_ceiling".
	BudgetReason string `json:"budget_reason,omitempty"`
	// SkippedCount counts every question dropped for budget; Skipped keeps
	// the first SkippedKept of them with their text for the inbox record.
	SkippedCount  int               `json:"skipped_count,omitempty"`
	Skipped       []SkippedQuestion `json:"skipped,omitempty"`
	LastHeartbeat *time.Time        `json:"last_heartbeat,omitempty"`
	// SpekMode describes the model Spek reasons with ("model X",
	// "Anthropic ...", or "rules only, no model configured").
	SpekMode string `json:"spek_mode,omitempty"`
	// SpekStatus is set when Spektacular live mode is not running for the
	// meeting; the transcript and the panel keep working without it.
	SpekStatus string `json:"spek_status,omitempty"`
	// Persona is the attendee persona's name when it is on.
	Persona string `json:"persona,omitempty"`
	// PersonaReplies counts the persona's posted replies.
	PersonaReplies int `json:"persona_replies,omitempty"`
	// PersonaTimings are the last answers' stage timings, newest last;
	// PersonaLatency sums them (timing.go, #18590).
	PersonaTimings []PersonaTiming `json:"persona_timings,omitempty"`
	PersonaLatency *PersonaLatency `json:"persona_latency,omitempty"`
	// Undelivered counts the replies in a row that reached the meeting in
	// neither chat nor voice, and UnreachableSince is when the first of
	// them failed; both clear on the next delivery. Unreachable is set once
	// that has lasted UnreachableWarnAfter: the meeting view then says the
	// bot cannot reach the participants (reach.go).
	Undelivered      int        `json:"undelivered,omitempty"`
	UnreachableSince *time.Time `json:"unreachable_since,omitempty"`
	Unreachable      bool       `json:"unreachable,omitempty"`
	// UndeliveredReason is why the last of them was not delivered.
	UndeliveredReason string `json:"undelivered_reason,omitempty"`
}

// StateDoc is the side document name the state is stored under.
const StateDoc = "session.json"

// Event kinds for live-panel observers.
const (
	EventIntent   = "intent"
	EventResearch = "research"
	EventNotes    = "notes"
	EventDropped  = "dropped"
	EventSpek     = "spek"
	// EventPersona is a persona reply; Intent carries it as JSON and Line
	// the reply as recorded in the transcript.
	EventPersona = "persona"
	// EventReach reports a change in whether the bot can reach the meeting
	// (State.Undelivered, State.Unreachable).
	EventReach = "reach"
	// EventBudget reports a budget step (80 percent warning, 90 percent
	// degrade, exhaustion); Reason is the level.
	EventBudget = "budget"
)

// Event is what live-panel observers receive.
type Event struct {
	MeetingID string          `json:"meeting_id"`
	Kind      string          `json:"kind"`
	Intent    json.RawMessage `json:"intent,omitempty"`
	Research  *ResearchItem   `json:"research,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	// Line is the transcript line a persona event added (a reply event).
	Line *meetings.Event `json:"line,omitempty"`
	At   time.Time       `json:"at"`
}

// intent is one Spek stdout line.
type intent struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	QID     string `json:"qid,omitempty"`
	Section string `json:"section,omitempty"`
	RID     string `json:"rid,omitempty"`
	Query   string `json:"query,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Usage   *usage `json:"usage,omitempty"`
}

// usage is optional LLM token usage Spek may attach to an intent.
type usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

func (u usage) total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.InputTokens + u.OutputTokens
}

// Intent types (contract section 2, stdout).
const (
	intentChatPost    = "chat.post"
	intentAsk         = "ask"
	intentNotesAppend = "notes.append"
	intentResearch    = "research"
	intentSay         = "say"
	intentHeartbeat   = "heartbeat"
	// intentPersona charges a persona reply; it never comes from Spek.
	intentPersona = "persona"
)
