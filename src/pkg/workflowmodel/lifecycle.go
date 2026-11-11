package workflowmodel

// This file holds the lifecycle half of the schema (hivecommons/hive#17620
// "The model"): columns, stage numbers, events, gates, roles, agents,
// checks, bookkeeping and labels. The ingest half (pipelines, coverage) is
// in model.go. A model that only carries the ingest half still validates;
// Compile needs the lifecycle half too.

// Meta describes a model for the library and, later, the marketplace.
type Meta struct {
	Version     string     `json:"version"`
	Author      string     `json:"author,omitempty"`
	Description string     `json:"description,omitempty"`
	License     string     `json:"license,omitempty"`
	Source      MetaSource `json:"source,omitempty"`
	// Provenance is how much a human has vouched for the model.
	Provenance Provenance `json:"provenance,omitempty"`
}

// MetaSource is where the model came from.
type MetaSource string

// Sources a model can be ingested from or written by.
const (
	SourceGitHubActions  MetaSource = "github-actions"
	SourceTekton         MetaSource = "tekton"
	SourceIssueTemplates MetaSource = "issue-templates"
	SourceSpec           MetaSource = "spec"
	SourceHand           MetaSource = "hand"
	SourceBuiltIn        MetaSource = "built-in"
)

// Provenance tiers. Ratings and adoption never change a tier; only a
// human review does.
type Provenance string

// Provenance values.
const (
	ProvenanceBuiltIn            Provenance = "built-in"
	ProvenanceCurated            Provenance = "curated"
	ProvenanceIngestedUnreviewed Provenance = "ingested-unreviewed"
	// ProvenanceLocal is a model authored or edited on this hive and not
	// (yet) published anywhere.
	ProvenanceLocal Provenance = "local"
)

// Confidence marks an element a human has not confirmed yet.
type Confidence string

// Confidence values. The empty value means confirmed (hand-written or
// edited by a person).
const (
	ConfidenceConfirmed Confidence = ""
	ConfidenceInferred  Confidence = "inferred"
)

// Citation points at the source paragraph or file an inferred element
// came from.
type Citation struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

// Column is one Activity Monitor column; several states may share it.
type Column struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	States   []string `json:"states"`
	Terminal bool     `json:"terminal,omitempty"`
}

// ItemKind is the kind of forge item a state or rule applies to.
type ItemKind string

// Item kinds. The empty kind means both.
const (
	KindAny   ItemKind = ""
	KindIssue ItemKind = "issue"
	KindPR    ItemKind = "pr"
)

// StateLifecycle is the lifecycle data on a State: where it sits on the
// board, which stage label it maps to and what a row in it may say.
type StateLifecycle struct {
	// Column is the Column.ID the state is displayed in.
	Column string `json:"column,omitempty"`
	// Number is the stage/NNN label number; zero for states that carry
	// no label (terminal observation states).
	Number int `json:"number,omitempty"`
	// Kinds says which item kinds may be in this state.
	Kinds []ItemKind `json:"kinds,omitempty"`
	// Waiting are the waiting reasons the monitor may show in this state.
	Waiting []string `json:"waiting,omitempty"`
	// Replies are the human-gate verbs offered in this state
	// (decide:approve, release, …).
	Replies    []string   `json:"replies,omitempty"`
	Confidence Confidence `json:"confidence,omitempty"`
	Citations  []Citation `json:"citations,omitempty"`
}

// DerivationMode says how an item's state is computed from facts.
type DerivationMode string

// Derivation modes. Builtin delegates to the hive's Go derivers (the
// hive-default model); Rules evaluates the model's own ordered rules.
const (
	DerivationBuiltin DerivationMode = "builtin"
	DerivationRules   DerivationMode = "rules"
)

// Derivation is how the engine maps facts to a state.
type Derivation struct {
	Mode  DerivationMode `json:"mode"`
	Rules []Rule         `json:"rules,omitempty"`
}

// Rule puts an item in State when every predicate holds. Rules are
// evaluated in order, first match wins; an item no rule matches is "not
// covered" and the engine abstains.
type Rule struct {
	ID    string      `json:"id,omitempty"`
	Kind  ItemKind    `json:"kind,omitempty"`
	State string      `json:"state"`
	When  []Predicate `json:"when"`
	// Reason is the text stored on the record and shown on the monitor.
	Reason string `json:"reason,omitempty"`
}

// Predicate is one test over a fact the engine already has.
type Predicate struct {
	Fact  string `json:"fact"`
	Op    Op     `json:"op"`
	Value string `json:"value,omitempty"`
}

// Op is a predicate operator.
type Op string

// Operators. Is/Not compare a scalar fact; Has/Lacks test membership in a
// set fact (labels, checks); True/False test a boolean fact.
const (
	OpIs    Op = "is"
	OpNot   Op = "not"
	OpHas   Op = "has"
	OpLacks Op = "lacks"
	OpTrue  Op = "true"
	OpFalse Op = "false"
)

// FactType is the shape of a fact in the vocabulary.
type FactType string

// Fact types.
const (
	FactBool   FactType = "bool"
	FactScalar FactType = "scalar"
	FactSet    FactType = "set"
)

// Facts is the vocabulary a Rule may test. The engine implements every
// entry (pkg/stageengine); the compiler refuses a fact outside it.
var Facts = map[string]FactType{
	"open":                 FactBool,
	"merged":               FactBool,
	"draft":                FactBool,
	"labels":               FactSet,
	"origin":               FactScalar,
	"author.kind":          FactScalar,
	"author.login":         FactScalar,
	"hold":                 FactBool,
	"park":                 FactBool,
	"issue.bucket":         FactScalar,
	"issue.claimed":        FactBool,
	"issue.covered":        FactBool,
	"issue.stalled":        FactBool,
	"issue.open_children":  FactBool,
	"pr.checks":            FactScalar,
	"pr.review":            FactScalar,
	"pr.changes_requested": FactBool,
	"pr.conflicting":       FactBool,
	"pr.dco":               FactScalar,
	"pr.head_fresh":        FactBool,
	"pr.base":              FactScalar,
	// pipeline.status is the latest post-merge run of the pipelines bound
	// to the item's state: green | red | running | unknown. The engine does
	// not observe post-merge runs yet and always reports unknown, so a
	// transition on it never fires; it exists so ingested post-merge chains
	// compile honestly instead of inventing a fact.
	"pipeline.status": FactScalar,
}

// Event is something that moves work: a fact the engine observes, named
// so transitions and bookkeeping can refer to it.
type Event struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	// Fact is the predicate that fires the event. Two events may not bind
	// the same predicate.
	Fact Predicate `json:"fact"`
	// Actor is the Role that may cause the event; empty for observations.
	Actor      string     `json:"actor,omitempty"`
	Confidence Confidence `json:"confidence,omitempty"`
	Citations  []Citation `json:"citations,omitempty"`
}

// Gate is a condition on a transition released by a role.
type Gate struct {
	ID         string      `json:"id"`
	Conditions []Predicate `json:"conditions,omitempty"`
	// ReleasedBy is the Role that may release the gate. Required.
	ReleasedBy string `json:"releasedBy"`
	// Reply is the text posted when the gate is released.
	Reply string `json:"reply,omitempty"`
	// MinApprovals is how many distinct approving reviews release the
	// gate; zero means one release suffices.
	MinApprovals int        `json:"minApprovals,omitempty"`
	Confidence   Confidence `json:"confidence,omitempty"`
	Citations    []Citation `json:"citations,omitempty"`
}

// RoleKind is how a role's members are determined.
type RoleKind string

// Role kinds. Permission is a repository permission (admin, maintain,
// write); FrozenSet is a set named at an earlier event (the reviewers at
// request time); AuthorKind is authorkind (human, clanker, hive); Bot is a
// named bot login; Members is a fixed list of logins and teams (params
// "members", comma-separated) such as a CODEOWNERS or OWNERS file names.
const (
	RolePermission RoleKind = "permission"
	RoleFrozenSet  RoleKind = "frozen-set"
	RoleAuthorKind RoleKind = "author-kind"
	RoleBot        RoleKind = "bot"
	RoleMembers    RoleKind = "members"
)

// RoleDef is who may fire which events and release which gates.
type RoleDef struct {
	ID     string            `json:"id"`
	Label  string            `json:"label,omitempty"`
	Kind   RoleKind          `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
	// Human is true for roles a person fills; the compiler needs at least
	// one human-released gate after any agent action.
	Human bool `json:"human,omitempty"`
}

// AgentGrant says which agent profile may act in which states.
type AgentGrant struct {
	Profile  string   `json:"profile"`
	States   []string `json:"states"`
	Backend  string   `json:"backend,omitempty"`
	Tools    []string `json:"tools,omitempty"`
	Produces []string `json:"produces,omitempty"`
}

// CheckKind is how a check is satisfied.
type CheckKind string

// Check kinds. CI is a status context a service reports; Manual is a box
// a person ticks (a pull-request template checklist item).
const (
	CheckCI     CheckKind = "ci"
	CheckManual CheckKind = "manual"
)

// Check is a CI check or manual checklist item a gate may require.
type Check struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Kind     CheckKind `json:"kind,omitempty"`
	Required bool      `json:"required,omitempty"`
	// Source is where the check was read from: a file:line or an API path.
	Source string `json:"source,omitempty"`
}

// Effect is a deterministic bookkeeping action on an event.
type Effect struct {
	On     string            `json:"on"`
	Action string            `json:"action"`
	Params map[string]string `json:"params,omitempty"`
}

// LabelSet is the label vocabulary reconciled against the repository.
type LabelSet struct {
	Prefix  string       `json:"prefix,omitempty"`
	Entries []LabelEntry `json:"entries,omitempty"`
}

// LabelEntry is one label; State is the state it marks, if any.
type LabelEntry struct {
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
}
