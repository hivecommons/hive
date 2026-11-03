// Package integration records coordination observations. Its ledger is not a
// merge permission: callers must retain their live exact-head guards.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

const SchemaVersion = 1
const fullSHABytes = 20

type State string

const (
	Unknown    State = "unknown"
	Waiting    State = "waiting"
	Running    State = "running"
	Passed     State = "passed"
	Rejected   State = "rejected"
	Superseded State = "superseded"
)

type Member struct {
	PR         int    `json:"pr"`
	Head       string `json:"head"`
	Entry      string `json:"entry"`
	Position   int    `json:"position"`
	BaseCommit string `json:"base_commit,omitempty"`
	HeadCommit string `json:"head_commit,omitempty"`
}

// Subject is immutable. Order, policy, producer, attempt and actual tested
// result are part of its identity, not mutable annotations on a PR record.
type Subject struct {
	Version            int      `json:"version"`
	Repo               string   `json:"repo"`
	Base               string   `json:"base"`
	Engine             string   `json:"engine"`
	Authority          string   `json:"authority"`
	Members            []Member `json:"members"`
	BaseSHA            string   `json:"base_sha"`
	TestedSHA          string   `json:"tested_sha"`
	TestedTree         string   `json:"tested_tree"`
	Policy             string   `json:"policy"`
	Producer           string   `json:"producer"`
	Workflow           string   `json:"workflow"`
	WorkflowSourceRepo string   `json:"workflow_source_repo,omitempty"`
	Attempt            int      `json:"attempt"`
	Generation         uint64   `json:"generation"`
}

func fullSHA(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == fullSHABytes
}

func (s Subject) Validate() error {
	if s.Version != SchemaVersion || s.Repo == "" || s.Base == "" ||
		(s.Engine != "direct" && s.Engine != "native") || s.Authority == "" ||
		!fullSHA(s.BaseSHA) || !fullSHA(s.TestedSHA) || !fullSHA(s.TestedTree) ||
		s.Policy == "" || s.Producer == "" || s.Workflow == "" || s.Attempt < 1 || s.Generation == 0 || len(s.Members) == 0 {
		return fmt.Errorf("incomplete exact integration subject")
	}
	seen := map[int]bool{}
	for i, m := range s.Members {
		if m.PR <= 0 || seen[m.PR] || !fullSHA(m.Head) || m.Position != i+1 || m.Entry == "" {
			return fmt.Errorf("invalid ordered integration member %d", i)
		}
		seen[m.PR] = true
	}
	return nil
}

func (s Subject) ID() string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Evidence struct {
	Subject       Subject   `json:"subject"`
	State         State     `json:"state"`
	ObservedState State     `json:"observed_state,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	Source        string    `json:"source"`
	RunID         int64     `json:"run_id"`
	SuiteID       int64     `json:"suite_id"`
	CheckIDs      []int64   `json:"check_ids"`
	InvalidatedBy string    `json:"invalidated_by,omitempty"`
	Reason        string    `json:"reason,omitempty"`
}

type Prerequisite struct {
	Key string `json:"key"`
	// PlanNode is the existing approved DAG node, never an inferred text edge.
	PlanNode  string `json:"plan_node"`
	Condition string `json:"condition,omitempty"`
}

type Intent struct {
	Version       int                  `json:"version"`
	Key           string               `json:"key"`
	Repo          string               `json:"repo"`
	Base          string               `json:"base"`
	Generation    uint64               `json:"generation"`
	Plan          string               `json:"plan"`
	PlanNode      string               `json:"plan_node"`
	PlanRevision  int                  `json:"plan_revision"`
	Head          string               `json:"head,omitempty"`
	Components    []string             `json:"components"`
	Contracts     []string             `json:"contracts"`
	Paths         []string             `json:"paths"`
	Generated     []string             `json:"generated"`
	ScopeKnown    bool                 `json:"scope_known"`
	Confidence    string               `json:"confidence"`
	Provenance    string               `json:"provenance"`
	Prerequisites []Prerequisite       `json:"prerequisites"`
	Relations     []RelationAssessment `json:"relations,omitempty"`
	ObservedAt    time.Time            `json:"observed_at"`
}

type RelationAssessment struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	Disposition string `json:"disposition"`
	Provenance  string `json:"provenance"`
}

type Drift struct {
	Key                string    `json:"key"`
	Repo               string    `json:"repo"`
	Head               string    `json:"head"`
	IntentGeneration   uint64    `json:"intent_generation"`
	ObservedAt         time.Time `json:"observed_at"`
	Source             string    `json:"source"`
	HeadVerified       bool      `json:"head_verified"`
	Actual             []string  `json:"actual"`
	OutsideAnticipated []string  `json:"outside_anticipated"`
}

func (i Intent) Validate() error {
	ref, ok := worksource.ParseKey(i.Key)
	if !ok || !strings.EqualFold(ref.Repo, i.Repo) || i.Version != SchemaVersion ||
		i.Base == "" || i.Generation == 0 || i.Plan == "" || i.PlanNode == "" ||
		i.Provenance == "" || i.ObservedAt.IsZero() || (i.Head != "" && !fullSHA(i.Head)) {
		return fmt.Errorf("intent requires canonical work, plan, generation and provenance")
	}
	keys, nodes := map[string]bool{}, map[string]bool{}
	for _, relation := range i.Relations {
		if _, valid := worksource.ParseKey(relation.Key); !valid || relation.Key == i.Key || relation.Provenance == "" ||
			relation.Kind != "semantic" && relation.Kind != "file" || relation.Disposition != "confirmed" && relation.Disposition != "false-positive" && relation.Disposition != "unreviewed" {
			return fmt.Errorf("invalid advisory relation assessment")
		}
	}
	for _, p := range i.Prerequisites {
		if _, ok := worksource.ParseKey(p.Key); !ok || p.PlanNode == "" || p.Key == i.Key {
			return fmt.Errorf("invalid prerequisite %q", p.Key)
		}
		if p.Condition != "" && p.Condition != "developed" && p.Condition != "integrated" && p.Condition != "deployed" {
			return fmt.Errorf("invalid prerequisite lifecycle %q", p.Condition)
		}
		if keys[p.Key] || nodes[p.PlanNode] {
			return fmt.Errorf("duplicate prerequisite ownership")
		}
		keys[p.Key], nodes[p.PlanNode] = true, true
	}
	return nil
}

type Reservation struct {
	Key              string    `json:"key"`
	Claim            string    `json:"claim"`
	ClaimGeneration  uint64    `json:"claim_generation"`
	IntentGeneration uint64    `json:"intent_generation"`
	ExpiresAt        time.Time `json:"expires_at"`
	RunnerMinutes    float64   `json:"runner_minutes"`
	Jobs             int       `json:"jobs"`
}

type Job struct {
	Repo        string    `json:"repo"`
	Base        string    `json:"base"`
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Attempt     int       `json:"attempt"`
	Head        string    `json:"head"`
	Workflow    string    `json:"workflow"`
	RunnerID    int64     `json:"runner_id"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	ObservedAt  time.Time `json:"observed_at"`
	// Candidates can share one physical job. Accounting counts the job once.
	Candidates []string `json:"candidates"`
}

func (j Job) Key() string { return fmt.Sprintf("%s/%d/%d", strings.ToLower(j.Repo), j.ID, j.Attempt) }

type Coverage struct {
	Repo           string    `json:"repo"`
	Base           string    `json:"base"`
	From           time.Time `json:"from"`
	Through        time.Time `json:"through"`
	Complete       bool      `json:"complete"`
	Source         string    `json:"source"`
	Reason         string    `json:"reason"`
	MergedComplete bool      `json:"merged_complete"`
	MergeReason    string    `json:"merge_reason,omitempty"`
}

type Event struct {
	Version           int                 `json:"version"`
	ID                string              `json:"id"`
	At                time.Time           `json:"at"`
	Intent            *Intent             `json:"intent,omitempty"`
	Evidence          *Evidence           `json:"evidence,omitempty"`
	Job               *Job                `json:"job,omitempty"`
	Coverage          *Coverage           `json:"coverage,omitempty"`
	Reservation       *Reservation        `json:"reservation,omitempty"`
	Release           string              `json:"release,omitempty"`
	ReleaseClaim      string              `json:"release_claim,omitempty"`
	ReleaseGeneration uint64              `json:"release_generation,omitempty"`
	Invalidate        string              `json:"invalidate,omitempty"`
	Reason            string              `json:"reason,omitempty"`
	MemberChange      *MemberChange       `json:"member_change,omitempty"`
	Integrated        *IntegrationReceipt `json:"integrated,omitempty"`
	GroupChange       *GroupChange        `json:"group_change,omitempty"`
	PolicySet         *PolicySet          `json:"policy_set,omitempty"`
	Drift             *Drift              `json:"drift,omitempty"`
	Execution         *ExecutionEvent     `json:"execution,omitempty"`
}

type GroupChange struct {
	Repo      string `json:"repo"`
	Authority string `json:"authority"`
	Head      string `json:"head"`
}

type PolicyBinding struct {
	Repo        string `json:"repo"`
	Base        string `json:"base"`
	Fingerprint string `json:"fingerprint"`
}

type PolicySet struct {
	Active []PolicyBinding `json:"active"`
}

type MemberChange struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
	Head string `json:"head,omitempty"`
}

type IntegrationReceipt struct {
	Repo     string    `json:"repo"`
	Base     string    `json:"base,omitempty"`
	PR       int       `json:"pr"`
	Head     string    `json:"head"`
	MergeSHA string    `json:"merge_sha"`
	MergedAt time.Time `json:"merged_at"`
	Source   string    `json:"source"`
}
