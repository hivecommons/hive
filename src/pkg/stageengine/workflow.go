package stageengine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// DeriveForRepo returns a Deriver of fallback's kind that derives through
// the model lookup returns for the item's repository, or through fallback
// when lookup returns nil. Profiles are compiled per lookup result and
// cached by hash, so a rebinding takes effect on the next derivation.
func DeriveForRepo(lookup func(owner, repo string) *workflowmodel.Profile, fallback Deriver) Deriver {
	if lookup == nil || fallback == nil {
		return fallback
	}
	kind := fallback.Kind()
	var mu sync.Mutex
	cache := map[string]Deriver{}
	return DeriverFunc(kind, func(f Facts) Decision {
		p := lookup(f.Ref.Owner, f.Ref.Repo)
		if p == nil {
			return fallback.Derive(f)
		}
		mu.Lock()
		d, ok := cache[p.Hash]
		if !ok {
			d = DeriveFromModel(p, kind)
			cache[p.Hash] = d
		}
		mu.Unlock()
		return d.Derive(f)
	})
}

// DeriveFromModel is the third Deriver (hivecommons/hive#17620 "One
// engine, one writer"): it derives an item's stage under a compiled
// workflow model and produces the same Decision the built-in derivers do,
// so the Reconciler stays the only label writer.
//
// Under derivation mode builtin the built-in deriver for kind decides and
// the model only has to cover the stage it produced; a stage the model has
// no state for is "not covered" and the deriver abstains (fail closed,
// never a silent fall-through). Under mode rules the model's ordered rules
// are evaluated over Facts; the first rule whose predicates all hold names
// the state, and an item no rule matches is not covered.
func DeriveFromModel(p *workflowmodel.Profile, kind stagelabels.Item) Deriver {
	if p == nil {
		return builtinDeriver(kind)
	}
	return modelDeriver{p: p, kind: kind, inner: builtinDeriver(kind)}
}

func builtinDeriver(kind stagelabels.Item) Deriver {
	if kind == stagelabels.PRItem {
		return prDeriverF
	}
	return issueDeriverF
}

type modelDeriver struct {
	p     *workflowmodel.Profile
	kind  stagelabels.Item
	inner Deriver
}

func (d modelDeriver) Kind() stagelabels.Item { return d.kind }

// ReasonNotCovered prefixes every abstention the model forces.
const ReasonNotCovered = "workflow: not covered"

func (d modelDeriver) Derive(f Facts) Decision {
	if f.Ref.Kind != d.kind {
		return abstain(reasonKindMismatch)
	}
	switch d.p.Mode {
	case workflowmodel.DerivationBuiltin:
		dec := d.inner.Derive(f)
		if dec.Abstain || dec.Stage == stagelabels.None {
			return dec
		}
		if _, ok := d.p.StateForNumber(dec.Stage.Number()); !ok {
			return abstain(fmt.Sprintf("%s: %s@%s has no state for stage %s", ReasonNotCovered, d.p.Name, d.p.Version, dec.Stage))
		}
		return dec
	case workflowmodel.DerivationRules:
		return d.rules(f)
	}
	return abstain(fmt.Sprintf("%s: %s@%s has derivation mode %q", ReasonNotCovered, d.p.Name, d.p.Version, d.p.Mode))
}

// rules evaluates the profile's rules in order.
func (d modelDeriver) rules(f Facts) Decision {
	want := workflowmodel.KindIssue
	if d.kind == stagelabels.PRItem {
		want = workflowmodel.KindPR
	}
	for i, r := range d.p.Rules {
		if r.Kind != workflowmodel.KindAny && r.Kind != want {
			continue
		}
		if !holdsAll(f, r.When) {
			continue
		}
		id := r.ID
		if id == "" {
			id = fmt.Sprintf("rules[%d]", i)
		}
		reason := r.Reason
		if reason == "" {
			reason = fmt.Sprintf("workflow %s@%s: rule %s", d.p.Name, d.p.Version, id)
		}
		return d.decisionFor(r.State, reason, f)
	}
	return abstain(fmt.Sprintf("%s: %s@%s has no rule for this %s", ReasonNotCovered, d.p.Name, d.p.Version, want))
}

// decisionFor turns a model state into a Decision: its stage number maps
// to the stage label, a terminal state with no number is Stage None.
func (d modelDeriver) decisionFor(state, reason string, f Facts) Decision {
	st := d.p.States[state]
	if st == nil {
		return abstain(fmt.Sprintf("%s: state %q is not in %s@%s", ReasonNotCovered, state, d.p.Name, d.p.Version))
	}
	dec := Decision{Reason: reason, Kind: f.Author.Kind, Role: f.Author.Role}
	if st.Lifecycle.Number == 0 {
		if d.p.Terminal[state] {
			dec.Terminal = true
			return dec
		}
		return abstain(fmt.Sprintf("%s: state %q carries no stage number and is not terminal", ReasonNotCovered, state))
	}
	stage, ok := stageByNumber(st.Lifecycle.Number)
	if !ok {
		return abstain(fmt.Sprintf("%s: state %q stage number %d is not in the stage table", ReasonNotCovered, state, st.Lifecycle.Number))
	}
	if !stage.Applies(d.kind) {
		return abstain(fmt.Sprintf("%s: state %q stage number %d is not a %s stage", ReasonNotCovered, state, st.Lifecycle.Number, d.kind))
	}
	dec.Stage = stage
	if f.PR != nil {
		dec.DCO = f.PR.DCO
	}
	return dec
}

func stageByNumber(n int) (stagelabels.Stage, bool) {
	for _, def := range stagelabels.Table {
		if def.Number == n {
			return def.Stage, true
		}
	}
	return stagelabels.None, false
}

func holdsAll(f Facts, preds []workflowmodel.Predicate) bool {
	for _, p := range preds {
		if !holds(f, p) {
			return false
		}
	}
	return true
}

// holds evaluates one predicate from the workflowmodel.Facts vocabulary.
// An unknown fact or a mismatched operator is false: the compiler has
// already refused such a model, so this is defence in depth.
func holds(f Facts, p workflowmodel.Predicate) bool {
	switch workflowmodel.Facts[p.Fact] {
	case workflowmodel.FactBool:
		v, ok := boolFact(f, p.Fact)
		if !ok {
			return false
		}
		return (p.Op == workflowmodel.OpTrue) == v
	case workflowmodel.FactScalar:
		v, ok := scalarFact(f, p.Fact)
		if !ok {
			return false
		}
		eq := strings.EqualFold(v, p.Value)
		return (p.Op == workflowmodel.OpIs) == eq
	case workflowmodel.FactSet:
		set, ok := setFact(f, p.Fact)
		if !ok {
			return false
		}
		has := false
		for _, s := range set {
			if matchGlob(p.Value, s) {
				has = true
				break
			}
		}
		return (p.Op == workflowmodel.OpHas) == has
	}
	return false
}

// matchGlob matches value against pattern, where a trailing `*` matches
// any suffix. Comparison is case-insensitive, as labels are.
func matchGlob(pattern, value string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(strings.TrimSuffix(pattern, "*")))
	}
	return strings.EqualFold(pattern, value)
}

func boolFact(f Facts, name string) (bool, bool) {
	switch name {
	case "open":
		return f.Open, true
	case "merged":
		return f.Merged, true
	case "draft":
		return f.Draft, true
	case "hold":
		return len(f.Park.Holds) > 0, true
	case "park":
		return f.Park.NeedsHuman || f.Park.DecisionLabel != "" || f.Park.PossiblyExists, true
	case "issue.claimed":
		return f.Issue != nil && f.Issue.Claim != nil, true
	case "issue.covered":
		return f.Issue != nil && f.Issue.CoveringPR != nil, true
	case "issue.stalled":
		return f.Issue != nil && f.Issue.Stalled, true
	case "issue.open_children":
		return f.Issue != nil && f.Issue.OpenChildren > 0, true
	case "pr.changes_requested":
		return f.PR != nil && f.PR.ChangesRequested, true
	case "pr.conflicting":
		return f.PR != nil && f.PR.Conflicting, true
	case "pr.head_fresh":
		return f.PR != nil && f.PR.HeadFresh, true
	}
	return false, false
}

// Scalar fact values. pr.checks: green | red | pending | missing | unknown.
// pr.review: pending | approved | changes_requested | unavailable.
// pr.dco: yes | no | unknown. author.kind: human | hive | clanker | unknown.
func scalarFact(f Facts, name string) (string, bool) {
	switch name {
	case "origin":
		return f.Origin, true
	case "author.kind":
		return authorKindName(f.Author.Kind), true
	case "author.login":
		return f.Author.Login, true
	case "issue.bucket":
		if f.Issue == nil {
			return "", true
		}
		return string(f.Issue.Bucket), true
	case "pr.checks":
		if f.PR == nil {
			return "", true
		}
		return checksName(f.PR.Checks), true
	case "pr.review":
		if f.PR == nil {
			return "", true
		}
		return reviewName(f.PR.Review.State), true
	case "pr.dco":
		if f.PR == nil {
			return "", true
		}
		return dcoName(f.PR.DCO), true
	case "pr.base":
		if f.PR == nil {
			return "", true
		}
		return f.PR.BaseRef, true
	case "pipeline.status":
		// Post-merge runs are not observed yet (see workflowmodel.Facts).
		return "unknown", true
	}
	return "", false
}

func setFact(f Facts, name string) ([]string, bool) {
	if name == "labels" {
		return f.Labels, true
	}
	return nil, false
}

func authorKindName(k authorkind.Kind) string {
	switch k {
	case authorkind.Human:
		return "human"
	case authorkind.Hive:
		return "hive"
	case authorkind.Clanker:
		return "clanker"
	}
	return "unknown"
}

func checksName(c stagelabels.CheckSummary) string {
	switch {
	case !c.Known:
		return "unknown"
	case len(c.Red) > 0:
		return "red"
	case len(c.Missing) > 0:
		return "missing"
	case c.Pending > 0:
		return "pending"
	}
	return "green"
}

func reviewName(s stagelabels.VerdictState) string {
	switch s {
	case stagelabels.VerdictApproved:
		return "approved"
	case stagelabels.VerdictChangesRequested:
		return "changes_requested"
	case stagelabels.VerdictUnavailable:
		return "unavailable"
	}
	return "pending"
}

func dcoName(d stagelabels.DCOState) string {
	switch d {
	case stagelabels.DCOYes:
		return "yes"
	case stagelabels.DCONo:
		return "no"
	}
	return "unknown"
}
