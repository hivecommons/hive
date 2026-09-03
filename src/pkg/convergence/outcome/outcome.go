// Package outcome implements the canonical outcome identity and
// desired-generation authority accepted on kubestellar/hive#4249 and consumed
// per the ownership contract accepted on #4250 (parent epic #3845, issue
// #4251).
//
// It owns exactly one durable truth: for each declared, bounded GitHub
// outcome, WHICH revision of its desired state is current. It is a small typed
// ledger — one JSON file under the durable data root — modeled on the two
// persistence idioms already proven in-tree: the atomic tmp+rename write of
// beads.Store.persist and the explicit acceptance/retirement generation
// lifecycle of the hub key-generation set (hub_generations_store.go).
//
// GitHub is a write-only audit mirror: every create/accept/supersede/retire
// may post a human-readable receipt through the Mirror hook, but nothing in
// this package ever parses GitHub content back as authority. One authority,
// one mirror, no reconciliation loop.
//
// The package is inert by default. Nothing reads the ledger unless the
// convergence mode toggle (convergence.mode / HIVE_CONVERGENCE_MODE, #4246)
// resolves to a non-off mode; see ModeEnabled and Record.AdmissionStatus.
package outcome

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

// keySeparator joins the (project, repo) scope to the outcome-local slug. It
// is "@" because that character cannot appear in either persisted work-key
// form — GitHub's "repo#number" or the string-keyed "repo!externalID"
// (worksource/key.go reserves "#" and "!") — so an outcome key can never be
// mistaken for, or collide with, any existing work key. This is the same
// argument externalKeySeparator already documents for "!".
const keySeparator = "@"

// scopeSeparator joins Project to Repo inside a key.
const scopeSeparator = "/"

// slugPattern is what an outcome-local name must look like: a stable
// lowercase slug, unique within (Project, Repo). Enforced at declaration time
// so a key is never ambiguous and never needs escaping.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)

// Ref is the minimal canonical identity of one declared outcome — exactly the
// dimensions the accepted #4249 contract names, and no more (issue non-goal:
// no broad ontology). Adding a dimension later widens the key without
// migrating the GitHub-only positive control.
type Ref struct {
	// Project is the hive-instance scope; the positive control uses one fixed
	// value ("default").
	Project string `json:"project"`
	// Repo is the canonical owner/name spelling — the SAME spelling
	// worksource.Ref.Repo uses, so outcome scope and work identity never
	// disagree about what a repository is called.
	Repo string `json:"repo"`
	// Outcome is a stable slug, unique within (Project, Repo).
	Outcome string `json:"outcome"`
}

// Key returns the canonical "project/repo@outcome" identity string, or "" for
// a Ref that fails Validate — mirroring worksource.Ref.Key's contract that ""
// means "not identifiable, never a key".
func (r Ref) Key() string {
	if err := r.Validate(); err != nil {
		return ""
	}
	return r.Project + scopeSeparator + r.Repo + keySeparator + r.Outcome
}

// Validate reports why a Ref cannot serve as a canonical identity. Every
// dimension is required, and no dimension may contain a character that would
// make the key ambiguous against itself or against a persisted work key.
func (r Ref) Validate() error {
	if r.Project == "" || r.Repo == "" || r.Outcome == "" {
		return fmt.Errorf("outcome ref requires project, repo, and outcome: %+v", r)
	}
	if strings.ContainsAny(r.Project, "@#!/ \t") {
		return fmt.Errorf("outcome project %q may not contain '@', '#', '!', '/', or whitespace", r.Project)
	}
	if strings.ContainsAny(r.Repo, "@#! \t") {
		return fmt.Errorf("outcome repo %q may not contain '@', '#', '!', or whitespace", r.Repo)
	}
	if !slugPattern.MatchString(r.Outcome) {
		return fmt.Errorf("outcome name %q is not a stable slug (%s)", r.Outcome, slugPattern.String())
	}
	return nil
}

// State is the lifecycle state of the CURRENT desired generation. Prior
// generations survive only in History; a record's State never resurrects them.
type State string

const (
	// StateProposed: the generation exists but has not been accepted by an
	// authorized principal. A proposed generation is still the desired
	// generation for comparison purposes — an observation of any OTHER
	// generation can never authorize a transition.
	StateProposed State = "proposed"
	// StateAccepted: an authorized principal recorded acceptance of exactly
	// this generation (actor and date are in History).
	StateAccepted State = "accepted"
	// StateRetired is terminal. No further mutation is possible.
	StateRetired State = "retired"
)

// Transition is one appended history entry. History only grows; superseded
// generations are retained here exactly as the hub generation set retains
// retired key generations, so acceptance can never be silently rewritten by
// culling.
type Transition struct {
	// Verb is create, accept, supersede, or retire.
	Verb string `json:"verb"`
	// Generation is the desired generation AFTER this transition.
	Generation int `json:"generation"`
	// Actor is the authorized principal who performed the transition.
	Actor string `json:"actor"`
	// Date is the transition time, RFC3339 in the persisted file.
	Date time.Time `json:"date"`
	// Spec snapshots the desired-state text carried by create/supersede, so a
	// superseded generation's content remains inspectable forever.
	Spec string `json:"spec,omitempty"`
}

// Record is the durable declaration of one outcome: its canonical identity,
// its monotonic desired generation, and the full transition history.
type Record struct {
	Ref Ref `json:"ref"`
	// Generation is the current desired generation. Monotonic: create sets 1,
	// supersede sets N+1, nothing ever decreases it. This is the number an
	// observed generation must equal for a gated transition to proceed.
	Generation int   `json:"generation"`
	State      State `json:"state"`
	// Spec is the accepted desired-state text or pointer. It may reference a
	// GitHub issue by its worksource key — as provenance, never as authority.
	Spec string `json:"spec"`
	// WorkRefs are worksource.Ref.Key() strings linking existing work items to
	// this outcome. They compose with the merged #4245/#4292 identity: legacy
	// GitHub "repo#number" keys are carried byte-identically, never rewritten.
	WorkRefs []string     `json:"work_refs,omitempty"`
	History  []Transition `json:"history"`
}

// clone returns a detached deep copy so readers can never race or mutate the
// ledger's own record (projection uses copied immutable values).
func (rec Record) clone() Record {
	out := rec
	out.WorkRefs = append([]string(nil), rec.WorkRefs...)
	out.History = append([]Transition(nil), rec.History...)
	return out
}

// validateWorkRefs refuses any work reference that is not a well-formed
// worksource key. A fabricated key here would be exactly the "#0" collapse
// worksource.Ref exists to prevent.
func validateWorkRefs(workRefs []string) error {
	for _, w := range workRefs {
		if _, ok := worksource.ParseKey(w); !ok {
			return fmt.Errorf("work ref %q is not a canonical worksource key", w)
		}
	}
	return nil
}
