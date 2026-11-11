package workflowmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// APIVersion is the schema version every Model carries.
const APIVersion = "workflow.hivecommons.dev/v1alpha1"

// KindWorkflowModel is the Kind of a Model document.
const KindWorkflowModel = "WorkflowModel"

// Owner names who moves work out of a state.
type Owner string

// Owners. ExternalObservation is a state Hive only watches (merged,
// released); ExternalService is CI or another system that acts on its own.
const (
	OwnerHuman               Owner = "human"
	OwnerRegisteredAgent     Owner = "registered-agent"
	OwnerExternalService     Owner = "external-service"
	OwnerExternalObservation Owner = "external-observation"
)

// StateType distinguishes plain states from parallel and final ones.
type StateType string

// State types. The empty type is an ordinary sequential state.
const (
	StateSequential StateType = ""
	StateParallel   StateType = "parallel"
	StateFinal      StateType = "final"
)

// Origin records where a state came from, so re-import can keep human
// edits and replace ingested parts (design §3 "Re-import without erasing
// people's work").
type Origin string

// Origins.
const (
	OriginHiveDefault Origin = "hive-default"
	OriginIngested    Origin = "ingested"
	OriginHuman       Origin = "human"
)

// System names a CI definition syntax an ingester understands.
type System string

// Systems.
const (
	SystemGitHubActions System = "github-actions"
	SystemTekton        System = "tekton"
)

// Role is what a pipeline is for, derived from its triggers.
type Role string

// Roles. PRVerify gates pull requests; PushVerify runs on the default
// branch; Release and Deploy are post-merge; Scheduled and Manual never
// gate anything by themselves.
const (
	RolePRVerify   Role = "pr-verify"
	RolePushVerify Role = "push-verify"
	RoleRelease    Role = "release"
	RoleDeploy     Role = "deploy"
	RoleScheduled  Role = "scheduled"
	RoleManual     Role = "manual"
	RoleOther      Role = "other"
)

// Model is one declared workflow model document.
type Model struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata names the model.
type Metadata struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Spec is the model body: the process graph (states, transitions, guards,
// lanes) and the execution graphs (pipelines) bound into it.
type Spec struct {
	Meta        Meta         `json:"meta,omitzero"`
	Scope       Scope        `json:"scope"`
	Sources     []Source     `json:"sources,omitempty"`
	Initial     string       `json:"initial"`
	Columns     []Column     `json:"columns,omitempty"`
	States      []State      `json:"states"`
	Transitions []Transition `json:"transitions,omitempty"`
	Guards      []Guard      `json:"guards,omitempty"`
	Lanes       []Lane       `json:"lanes,omitempty"`
	Derivation  Derivation   `json:"derivation,omitzero"`
	Events      []Event      `json:"events,omitempty"`
	Gates       []Gate       `json:"gates,omitempty"`
	Roles       []RoleDef    `json:"roles,omitempty"`
	Agents      []AgentGrant `json:"agents,omitempty"`
	Checks      []Check      `json:"checks,omitempty"`
	Bookkeeping []Effect     `json:"bookkeeping,omitempty"`
	Labels      LabelSet     `json:"labels,omitzero"`
	Pipelines   []Pipeline   `json:"pipelines,omitempty"`
	Coverage    Coverage     `json:"coverage"`
}

// Scope is the repository identity the model applies to. Forge is the
// forge instance (github.com, a GHES host, a GitLab host), never implied
// from the owner/name alone.
type Scope struct {
	Forge      string   `json:"forge"`
	Repository string   `json:"repository"`
	WorkTypes  []string `json:"workTypes,omitempty"`
}

// Source is one file the ingest read, with its content hash.
type Source struct {
	System System `json:"system"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// State is a node of the process graph.
type State struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Owner    Owner     `json:"owner"`
	Type     StateType `json:"type,omitempty"`
	Regions  []State   `json:"regions,omitempty"`
	GuardRef string    `json:"guardRef,omitempty"`
	// Pipelines bound into this state (by Pipeline.ID).
	Pipelines []string `json:"pipelines,omitempty"`
	Origin    Origin   `json:"origin,omitempty"`
	// Lifecycle is the board/label half of the state (lifecycle.go).
	Lifecycle StateLifecycle `json:"lifecycle,omitzero"`
}

// Transition moves work between states on an event, if its guard holds.
type Transition struct {
	From     string `json:"from"`
	To       string `json:"to"`
	On       string `json:"on"`
	GuardRef string `json:"guardRef,omitempty"`
	// GateRef is the lifecycle Gate (lifecycle.go) the move needs.
	GateRef string `json:"gateRef,omitempty"`
	// Note is the human-readable reason for the edge.
	Note string `json:"note,omitempty"`
}

// GuardKind is the family a guard belongs to.
type GuardKind string

// Guard kinds.
const (
	GuardRequiredChecks      GuardKind = "required-checks"
	GuardReviewPolicy        GuardKind = "review-policy"
	GuardEnvironmentApproval GuardKind = "environment-approval"
	GuardManual              GuardKind = "manual"
	GuardExternal            GuardKind = "external"
)

// Guard is a named condition a transition or region completion needs.
type Guard struct {
	ID     string            `json:"id"`
	Kind   GuardKind         `json:"kind"`
	Params map[string]string `json:"params,omitempty"`
	Source string            `json:"source,omitempty"`
}

// Lane is a bounded kind of work an actor may do while a state holds.
type Lane struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	StateRef string   `json:"stateRef"`
	Actor    Owner    `json:"actor"`
	Effects  []string `json:"effects,omitempty"`
}

// Pipeline is one execution graph: a GitHub Actions workflow file or a
// Tekton Pipeline.
type Pipeline struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	System      System       `json:"system"`
	Path        string       `json:"path"`
	Role        Role         `json:"role"`
	Triggers    []Trigger    `json:"triggers,omitempty"`
	Nodes       []Node       `json:"nodes"`
	Concurrency *Concurrency `json:"concurrency,omitempty"`
	// Levels is the number of topological levels in Nodes (0 when empty).
	Levels int `json:"levels"`
}

// Trigger is one event that starts a pipeline.
type Trigger struct {
	Event    string   `json:"event"`
	Types    []string `json:"types,omitempty"`
	Branches []string `json:"branches,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Schedule []string `json:"schedule,omitempty"`
}

// NodeKind is the kind of execution node.
type NodeKind string

// Node kinds.
const (
	NodeJob              NodeKind = "job"
	NodeReusableWorkflow NodeKind = "reusable-workflow"
	NodeTask             NodeKind = "task"
	NodeFinally          NodeKind = "finally"
)

// Node is a job or task in a pipeline.
type Node struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Kind  NodeKind `json:"kind"`
	Needs []string `json:"needs,omitempty"`
	// Ref is the reusable workflow or task reference, when Kind says so.
	Ref         string   `json:"ref,omitempty"`
	Matrix      *Matrix  `json:"matrix,omitempty"`
	Environment string   `json:"environment,omitempty"`
	If          string   `json:"if,omitempty"`
	When        []string `json:"when,omitempty"`
	RunsOn      []string `json:"runsOn,omitempty"`
	// Level is the node's topological level (0 = no needs).
	Level int `json:"level"`
}

// Matrix summarises a job's strategy.matrix without expanding it.
type Matrix struct {
	Keys    []string `json:"keys,omitempty"`
	Include int      `json:"include,omitempty"`
	Exclude int      `json:"exclude,omitempty"`
	// Dynamic is set when the matrix is an expression (fromJSON etc).
	Dynamic bool `json:"dynamic,omitempty"`
}

// Concurrency is a pipeline's concurrency group.
type Concurrency struct {
	Group            string `json:"group,omitempty"`
	CancelInProgress bool   `json:"cancelInProgress,omitempty"`
}

// Coverage says how completely the sources were understood.
type Coverage struct {
	Files    int       `json:"files"`
	Parsed   int       `json:"parsed"`
	Findings []Finding `json:"findings,omitempty"`
}

// Severity of a finding.
type Severity string

// Severities. Unsupported means the construct is recognised and skipped;
// Unknown means the ingest could not tell what it was; Warning is advice.
const (
	SeverityUnsupported Severity = "unsupported"
	SeverityUnknown     Severity = "unknown"
	SeverityWarning     Severity = "warning"
)

// Finding is one thing the ingest could not fully model.
type Finding struct {
	Path     string   `json:"path"`
	Node     string   `json:"node,omitempty"`
	Code     string   `json:"code"`
	Detail   string   `json:"detail,omitempty"`
	Severity Severity `json:"severity"`
}

// New returns an empty model for scope.
func New(name string, scope Scope) *Model {
	return &Model{APIVersion: APIVersion, Kind: KindWorkflowModel, Metadata: Metadata{Name: name}, Spec: Spec{Scope: scope}}
}

// Hash is the content hash of the spec: the same spec always hashes the
// same (JSON field order is fixed by the struct), independent of metadata.
func (m *Model) Hash() string {
	b, _ := json.Marshal(m.Spec)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FindState returns the state with id at any depth.
func (m *Model) FindState(id string) *State {
	return findState(m.Spec.States, id)
}

func findState(states []State, id string) *State {
	for i := range states {
		if states[i].ID == id {
			return &states[i]
		}
		if s := findState(states[i].Regions, id); s != nil {
			return s
		}
	}
	return nil
}

// Pipeline returns the pipeline with id, or nil.
func (m *Model) Pipeline(id string) *Pipeline {
	for i := range m.Spec.Pipelines {
		if m.Spec.Pipelines[i].ID == id {
			return &m.Spec.Pipelines[i]
		}
	}
	return nil
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("workflow model invalid")

// Validate checks the model is self-consistent: version and kind, unique
// state ids, initial and every reference resolving, and acyclic pipelines.
func (m *Model) Validate() error {
	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if m.APIVersion != APIVersion {
		fail("apiVersion %q, want %q", m.APIVersion, APIVersion)
	}
	if m.Kind != KindWorkflowModel {
		fail("kind %q, want %q", m.Kind, KindWorkflowModel)
	}
	if m.Metadata.Name == "" {
		fail("metadata.name is empty")
	}
	if m.Spec.Scope.Repository == "" {
		fail("spec.scope.repository is empty")
	}
	ids := map[string]bool{}
	var walk func(states []State, parallelParent bool)
	walk = func(states []State, parallelParent bool) {
		for _, s := range states {
			switch {
			case s.ID == "":
				fail("state with empty id (label %q)", s.Label)
			case ids[s.ID]:
				fail("duplicate state id %q", s.ID)
			}
			ids[s.ID] = true
			if s.Owner == "" {
				fail("state %q has no owner", s.ID)
			}
			if s.Type == StateParallel && len(s.Regions) == 0 {
				fail("parallel state %q has no regions", s.ID)
			}
			if s.Type != StateParallel && len(s.Regions) > 0 {
				fail("state %q has regions but is not parallel", s.ID)
			}
			walk(s.Regions, s.Type == StateParallel)
		}
	}
	walk(m.Spec.States, false)
	if len(m.Spec.States) == 0 {
		fail("spec.states is empty")
	}
	if m.Spec.Initial == "" {
		fail("spec.initial is empty")
	} else if !ids[m.Spec.Initial] {
		fail("spec.initial %q is not a state", m.Spec.Initial)
	}
	guards := map[string]bool{}
	for _, g := range m.Spec.Guards {
		if guards[g.ID] {
			fail("duplicate guard id %q", g.ID)
		}
		guards[g.ID] = true
	}
	pipelines := map[string]bool{}
	for _, p := range m.Spec.Pipelines {
		if pipelines[p.ID] {
			fail("duplicate pipeline id %q", p.ID)
		}
		pipelines[p.ID] = true
		if _, err := Levels(p.Nodes); err != nil {
			fail("pipeline %q: %v", p.ID, err)
		}
	}
	var checkRefs func(states []State)
	checkRefs = func(states []State) {
		for _, s := range states {
			if s.GuardRef != "" && !guards[s.GuardRef] {
				fail("state %q guardRef %q is not a guard", s.ID, s.GuardRef)
			}
			for _, p := range s.Pipelines {
				if !pipelines[p] {
					fail("state %q binds unknown pipeline %q", s.ID, p)
				}
			}
			checkRefs(s.Regions)
		}
	}
	checkRefs(m.Spec.States)
	for _, t := range m.Spec.Transitions {
		if !ids[t.From] {
			fail("transition from unknown state %q", t.From)
		}
		if !ids[t.To] {
			fail("transition to unknown state %q", t.To)
		}
		if t.GuardRef != "" && !guards[t.GuardRef] {
			fail("transition %s->%s guardRef %q is not a guard", t.From, t.To, t.GuardRef)
		}
	}
	for _, l := range m.Spec.Lanes {
		if !ids[l.StateRef] {
			fail("lane %q stateRef %q is not a state", l.ID, l.StateRef)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
}

// Levels assigns each node its topological level (longest path from a
// root) in place and returns the number of levels. It fails on a cycle or
// a need that names no node.
func Levels(nodes []Node) (int, error) {
	idx := make(map[string]int, len(nodes))
	for i, n := range nodes {
		idx[n.ID] = i
	}
	const unvisited, visiting, done = 0, 1, 2
	state := make([]int, len(nodes))
	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("cycle through node %q", nodes[i].ID)
		}
		state[i] = visiting
		level := 0
		for _, need := range nodes[i].Needs {
			j, ok := idx[need]
			if !ok {
				return fmt.Errorf("node %q needs unknown node %q", nodes[i].ID, need)
			}
			if err := visit(j); err != nil {
				return err
			}
			if nodes[j].Level+1 > level {
				level = nodes[j].Level + 1
			}
		}
		nodes[i].Level = level
		state[i] = done
		return nil
	}
	max := -1
	for i := range nodes {
		if err := visit(i); err != nil {
			return 0, err
		}
		if nodes[i].Level > max {
			max = nodes[i].Level
		}
	}
	return max + 1, nil
}

// ByLevel groups node ids by level, each level sorted by id.
func ByLevel(nodes []Node) [][]string {
	var out [][]string
	for _, n := range nodes {
		for len(out) <= n.Level {
			out = append(out, nil)
		}
		out[n.Level] = append(out[n.Level], n.ID)
	}
	for _, l := range out {
		sort.Strings(l)
	}
	return out
}

// SHA256Hex is the hex digest ingesters record in Source.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
