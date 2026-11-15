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

// ResearchObserver optionally supplies a dispatcher's findings and completion.
// The remover stops delivery when the live runner closes.
type ResearchObserver interface {
	ObserveResearch(func(ResearchSignal)) func()
}

// Research signal kinds: the agent's [FINDING], [COMPLETE] and [ERROR]
// output for a kick.
const (
	SignalFinding  = "finding"
	SignalComplete = "complete"
	SignalError    = "error"
)

// ResearchSignal is one output signal for a research kick.
type ResearchSignal struct {
	KickID string
	// MeetingID routes signals emitted before Dispatch returns its kick ID.
	MeetingID string
	Kind      string
	Text      string
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
	Say(ctx context.Context, id, text string) error
	Subscribe(fn func(meetings.Update)) func()
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
	RID       string    `json:"rid"`
	Query     string    `json:"query"`
	Reason    string    `json:"reason,omitempty"`
	Status    string    `json:"status"`
	KickID    string    `json:"kick_id,omitempty"`
	Findings  []string  `json:"findings,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Posted    bool      `json:"posted,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	LastHeartbeat   *time.Time          `json:"last_heartbeat,omitempty"`
	// SpekMode describes the model Spek reasons with ("model X",
	// "Anthropic ...", or "rules only, no model configured").
	SpekMode string `json:"spek_mode,omitempty"`
	// SpekStatus is set when Spektacular live mode is not running for the
	// meeting; the transcript and the panel keep working without it.
	SpekStatus string `json:"spek_status,omitempty"`
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
)

// Event is what live-panel observers receive.
type Event struct {
	MeetingID string          `json:"meeting_id"`
	Kind      string          `json:"kind"`
	Intent    json.RawMessage `json:"intent,omitempty"`
	Research  *ResearchItem   `json:"research,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	At        time.Time       `json:"at"`
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
)
