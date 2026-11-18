// Package sessiontaint is the observe-only core of session taint
// (hivecommons/hive#18987, src/docs/design/session-taint.md, phase 0).
//
// It remembers, per agent session, which coarse labels the hive has observed
// ("this session has seen a secret-shaped marker", "this session has read a
// private repository") and evaluates a small fixed rule table against later
// outbound actions. In this phase the result is only ever RECORDED: the
// evaluator returns a would-be verdict, the observer logs it and counts it,
// and nothing in this package can allow, hold, deny, restrict or change a
// request or an action. The observer methods return no value on purpose, so a
// caller has nothing to branch on.
//
// Nothing here stores a credential or any content: a label is a closed kind,
// an origin that is a category name or an owner/repo id, a source name and a
// timestamp. Free-form input is reduced to a short identifier alphabet or
// replaced by a fixed placeholder before it is stored or logged.
//
// The package is stdlib only so the Crux ring (proxy, push broker) can take
// the same observer through a setter in a later phase.
package sessiontaint

import (
	"strings"
	"time"
)

// Label is the closed set of taint labels (design: Labels).
type Label string

const (
	// LabelSecret: a credential-shaped value or a canary was seen. The origin
	// is a category name, never the value.
	LabelSecret Label = "secret"
	// LabelPrivateRepo: content from, or the working target of, a private
	// repository. The origin is owner/repo.
	LabelPrivateRepo Label = "private-repo"
	// LabelCredentialedResponse: a response fetched with a brokered
	// credential from a host the operator lists as sensitive. The origin is
	// the host.
	LabelCredentialedResponse Label = "credentialed-response"
	// LabelUnknown: state could not be established. Only fail-closed rules
	// use it, so it matters from the enforcement phases on.
	LabelUnknown Label = "unknown"
)

// Source names which observation set a label (design: Sources).
type Source string

const (
	// SourceCanaryHit is S3: the existing canary leak handler saw a planted
	// token.
	SourceCanaryHit Source = "canary-hit"
)

// SinkKind names a place Hive already mediates an outbound action.
type SinkKind string

const (
	// SinkRelayIssue is a hive-open-issue relay request.
	SinkRelayIssue SinkKind = "relay-issue"
	// SinkRelayComment is a hive-comment relay request.
	SinkRelayComment SinkKind = "relay-comment"
	// SinkRelayPR is a hive-open-pr relay request.
	SinkRelayPR SinkKind = "relay-pr"
	// SinkRelayReview is a hive-review relay request.
	SinkRelayReview SinkKind = "relay-review"
)

// Audience is who can read a sink's destination. The zero value is public, so
// an unresolved destination restricts more, never less (design: Visibility
// lookups).
type Audience int

const (
	// AudiencePublic means readers beyond the repository's collaborators, or
	// that the audience could not be established.
	AudiencePublic Audience = iota
	// AudiencePrivate means the destination is a private repository.
	AudiencePrivate
)

// Verdict is what a rule would do were it enforced. Phase 0 has no deny and no
// enforcement: the first enforced verdict is Hold (design: decision 4), and
// Deny arrives with the enforcement phases, so it does not exist yet.
type Verdict string

const (
	// VerdictAllow means no rule fired.
	VerdictAllow Verdict = "allow"
	// VerdictHold means the rule would put the action in front of an operator
	// if it were enforced. It is recorded, never acted on.
	VerdictHold Verdict = "hold"
)

// Rule ids (design: Rule shape v1). The ids are the stable names that appear
// in the log, the audit detail and the counters.
const (
	RuleSecretPublicWrite = "taint.secret.public-write"
	RulePrivateAudience   = "taint.private.audience"
	RuleCredPublicWrite   = "taint.cred.public-write"
	RuleUnknownAnyWrite   = "taint.unknown.any-write"
)

// LabelRecord is one label a session holds. Labels only grow inside a session.
type LabelRecord struct {
	Label     Label
	Origin    string
	Source    Source
	FirstSeen time.Time
}

// Sink describes one outbound action for evaluation: its kind, the destination
// repository (owner/repo) and the destination's audience.
type Sink struct {
	Kind     SinkKind
	Repo     string
	Audience Audience
}

// Finding is one rule that would act: the rule id, the verdict, and the label
// and origin that triggered it.
type Finding struct {
	Rule    string
	Verdict Verdict
	Label   Label
	Origin  string
}

// Evaluate is the pure rule table: from a session's labels and one sink to the
// findings, at most one per rule, in rule order. It never mutates its inputs,
// never consults a clock or any state, and can only return VerdictHold
// findings: a session with no matching label yields none. Taint adds a verdict
// to an action every earlier gate allowed; it cannot grant anything.
func Evaluate(labels []LabelRecord, sink Sink) []Finding {
	var out []Finding
	add := func(rule string, l LabelRecord) {
		out = append(out, Finding{Rule: rule, Verdict: VerdictHold, Label: l.Label, Origin: l.Origin})
	}
	if l, ok := firstLabel(labels, func(l LabelRecord) bool {
		return l.Label == LabelSecret && sink.Audience == AudiencePublic
	}); ok {
		add(RuleSecretPublicWrite, l)
	}
	if l, ok := firstLabel(labels, func(l LabelRecord) bool {
		return l.Label == LabelPrivateRepo && !strings.EqualFold(l.Origin, sink.Repo)
	}); ok {
		add(RulePrivateAudience, l)
	}
	if l, ok := firstLabel(labels, func(l LabelRecord) bool {
		return l.Label == LabelCredentialedResponse && sink.Audience == AudiencePublic
	}); ok {
		add(RuleCredPublicWrite, l)
	}
	if l, ok := firstLabel(labels, func(l LabelRecord) bool { return l.Label == LabelUnknown }); ok {
		add(RuleUnknownAnyWrite, l)
	}
	return out
}

func firstLabel(labels []LabelRecord, match func(LabelRecord) bool) (LabelRecord, bool) {
	for _, l := range labels {
		if match(l) {
			return l, true
		}
	}
	return LabelRecord{}, false
}
