package authz

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/hive/crux/logscrub"
)

const (
	// maxAttrBytes bounds every free-text attribute copied into an event.
	maxAttrBytes = 256
	// attrPreScrubBytes bounds the text handed to the credential scrubber, so a
	// hostile oversized value cannot make recording expensive.
	attrPreScrubBytes = 1024
	// decisionIDRandomBytes is the entropy in a decision id.
	decisionIDRandomBytes = 8
	// decisionIDPrefix marks a decision id.
	decisionIDPrefix = "dec_"
	// ParityMatch and ParityMismatch are the Event.Parity values.
	ParityMatch    = "match"
	ParityMismatch = "mismatch"
)

// Event is the versioned decision record, one per Observe call. It holds
// attributes only. There is deliberately no field that could carry a request
// body, a diff, file contents, a credential or a path list: the changed files
// appear only as a count.
type Event struct {
	V             int           `json:"v"`
	TS            time.Time     `json:"ts"`
	DecisionID    string        `json:"decision_id"`
	Surface       string        `json:"surface"`
	Effect        string        `json:"effect"`
	Shadow        bool          `json:"shadow"`
	RuleID        string        `json:"rule_id"`
	ReasonCode    string        `json:"reason_code"`
	PolicyVersion string        `json:"policy_version"`
	Actor         EventActor    `json:"actor"`
	Action        EventAction   `json:"action"`
	Resource      EventResource `json:"resource"`
	// LatencyUS is how long Decide took, in microseconds.
	LatencyUS int64 `json:"latency_us"`
	// Legacy is what the existing gate decided.
	Legacy EventLegacy `json:"legacy"`
	// Parity is ParityMatch or ParityMismatch.
	Parity    string `json:"parity"`
	RequestID string `json:"request_id,omitempty"`
}

// EventActor is the actor attributes of an event.
type EventActor struct {
	Agent      string `json:"agent,omitempty"`
	Mode       string `json:"mode,omitempty"`
	ACMMLevel  int    `json:"acmm_level,omitempty"`
	Identified bool   `json:"identified"`
}

// EventAction is the action attributes of an event.
type EventAction struct {
	Kind  string `json:"kind"`
	Forge string `json:"forge,omitempty"`
}

// EventResource is the resource attributes of an event. PathsCount is how many
// changed paths the request carried and MatchedCount how many a path rule
// matched; the paths themselves are never recorded.
type EventResource struct {
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	PathsCount   int    `json:"paths_count"`
	MatchedCount int    `json:"matched_count,omitempty"`
}

// EventLegacy is the existing gate's verdict.
type EventLegacy struct {
	Effect     string `json:"effect"`
	ReasonCode string `json:"reason_code"`
}

var decisionSeq atomic.Uint64

// newDecisionID returns a unique id for a decision. If the system random
// source fails it falls back to a process-local sequence, which is still
// unique within the process.
func newDecisionID() string {
	buf := make([]byte, decisionIDRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%sseq%d", decisionIDPrefix, decisionSeq.Add(1))
	}
	return decisionIDPrefix + hex.EncodeToString(buf)
}

// attr makes free text safe to record: control characters dropped, credential
// shapes masked, length bounded. It is applied to every string that originates
// outside the evaluator (agent, repo, branch, request id).
func attr(s string) string {
	s = clip(s, attrPreScrubBytes)
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return clip(logscrub.ScrubString(s), maxAttrBytes)
}

// clip truncates s to at most limit bytes without splitting a rune.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func buildEvent(ts time.Time, req Request, d Decision, legacy Verdict, latency time.Duration) Event {
	parity := ParityMismatch
	if legacy.Matches(d) {
		parity = ParityMatch
	}
	return Event{
		V:             SchemaVersion,
		TS:            ts.UTC(),
		DecisionID:    newDecisionID(),
		Surface:       attr(string(req.Action.Surface)),
		Effect:        string(d.Effect),
		Shadow:        d.Shadow,
		RuleID:        d.RuleID,
		ReasonCode:    d.ReasonCode,
		PolicyVersion: d.PolicyVersion,
		Actor: EventActor{
			Agent:      attr(req.Actor.Agent),
			Mode:       attr(req.Actor.Mode),
			ACMMLevel:  req.Actor.ACMMLevel,
			Identified: req.Actor.Identified,
		},
		Action:    EventAction{Kind: attr(req.Action.Kind), Forge: attr(req.Action.Forge)},
		Resource:  EventResource{Repo: attr(req.Resource.Repo), Branch: attr(req.Resource.Branch), PathsCount: len(req.Resource.Paths), MatchedCount: d.MatchedPaths},
		LatencyUS: latency.Microseconds(),
		Legacy:    EventLegacy{Effect: string(legacy.Effect), ReasonCode: legacy.ReasonCode},
		Parity:    parity,
		RequestID: attr(req.Context.RequestID),
	}
}
