package betty

import (
	"strings"
	"time"
)

// Per-request stage timings (hivecommons/hive#18590): every answer carries
// how long each stage took, so the meeting detail shows where the seconds
// went and whether the answer met its budget.

// Answer sources, for Timing.Source.
const (
	// SourceHiveStatus: answered from the hive's live state.
	SourceHiveStatus = "hive-status"
	// SourceRetrieval: answered from knowledge, beads or code maps.
	SourceRetrieval = "knowledge"
	// SourceWeb: answered from web search hits.
	SourceWeb = "web"
	// SourceResearch: a deep dive's result from the research agents.
	SourceResearch = "research"
	// SourceModel: answered by the model with no retrieval hits.
	SourceModel = "model"
	// SourceFallback: the model failed or is missing; a fixed reply.
	SourceFallback = "fallback"
	// SourceFixed: a stock line that needs no model: the intro, a hello,
	// an acknowledgement, a queue note.
	SourceFixed = "fixed"
)

// Answer budgets: the time from hearing a request to having its answer.
const (
	// HiveStatusBudget is the target for a question about the hive's live
	// state, which needs no search: in-memory status and one model call.
	HiveStatusBudget = 4 * time.Second
	// AnswerBudget is the target for any other answer.
	AnswerBudget = 8 * time.Second
)

// Timing is one answer's stage timings, in milliseconds from when the
// request was heard (its final line).
type Timing struct {
	HeardAt time.Time `json:"heard_at"`
	// AckMS is when the spoken acknowledgement went out; EarlyAck marks one
	// said from the partial transcript, before the final line (AckMS 0).
	AckMS    int64 `json:"ack_ms,omitempty"`
	EarlyAck bool  `json:"early_ack,omitempty"`
	// RetrieveMS is how long retrieval took; ModelMS the model call.
	RetrieveMS int64 `json:"retrieve_ms"`
	ModelMS    int64 `json:"model_ms"`
	// AnswerMS is heard to answer ready.
	AnswerMS int64  `json:"answer_ms"`
	Source   string `json:"source"`
	BudgetMS int64  `json:"budget_ms"`
	// OverBudget reports AnswerMS above BudgetMS.
	OverBudget bool `json:"over_budget,omitempty"`
}

// stageTimes are a pending request's stage marks.
type stageTimes struct {
	ackAt       time.Time
	earlyAck    bool
	retrievedAt time.Time
	modelStart  time.Time
	modelDone   time.Time
}

func ms(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// since is the milliseconds from a to b, zero when either is unset.
func since(a, b time.Time) int64 {
	if a.IsZero() || b.IsZero() {
		return 0
	}
	return ms(b.Sub(a))
}

// timing is p's stage timings for an answer ready at now.
func (e *Engine) timing(p *pending, failed bool, now time.Time) *Timing {
	t := &Timing{
		HeardAt:    p.created,
		EarlyAck:   p.stages.earlyAck,
		RetrieveMS: since(p.created, p.stages.retrievedAt),
		ModelMS:    since(p.stages.modelStart, p.stages.modelDone),
		AnswerMS:   since(p.created, now),
	}
	if !p.stages.earlyAck {
		t.AckMS = since(p.created, p.stages.ackAt)
	}
	switch {
	case failed:
		t.Source = SourceFallback
	case hasHiveStatus(p.snips):
		t.Source = SourceHiveStatus
	case hasWeb(p.snips):
		t.Source = SourceWeb
	case len(p.snips) > 0:
		t.Source = SourceRetrieval
	default:
		t.Source = SourceModel
	}
	budget := AnswerBudget
	if t.Source == SourceHiveStatus {
		budget = HiveStatusBudget
	}
	t.BudgetMS = budget.Milliseconds()
	t.OverBudget = t.AnswerMS > t.BudgetMS
	if t.OverBudget {
		e.log.Info("[meetings] persona answer over budget", "source", t.Source, "answer_ms", t.AnswerMS,
			"budget_ms", t.BudgetMS, "retrieve_ms", t.RetrieveMS, "model_ms", t.ModelMS)
	}
	return t
}

// hasWeb reports whether any snippet came from web search (web.go names
// them "web <host>").
func hasWeb(snips []Snippet) bool {
	for _, s := range snips {
		if s.Source == webSource || strings.HasPrefix(s.Source, webSource+" ") {
			return true
		}
	}
	return false
}

// fixedTiming is the timing of a stock line produced at now: heard and
// ready at once, with no budget to meet.
func fixedTiming(now time.Time) *Timing {
	return &Timing{HeardAt: now, Source: SourceFixed}
}
