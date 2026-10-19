package stageengine

import (
	"sync"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Deriver maps Facts to a Decision. Pure: same Facts → same Decision.
type Deriver interface {
	Kind() stagelabels.Item
	Derive(f Facts) Decision
}

// DeriverFunc adapts a function to the Deriver interface for kind k.
func DeriverFunc(k stagelabels.Item, fn func(Facts) Decision) Deriver {
	return funcDeriver{kind: k, fn: fn}
}

type funcDeriver struct {
	kind stagelabels.Item
	fn   func(Facts) Decision
}

func (d funcDeriver) Kind() stagelabels.Item { return d.kind }
func (d funcDeriver) Derive(f Facts) Decision {
	if f.Ref.Kind != d.kind {
		return abstain(reasonKindMismatch)
	}
	return d.fn(f)
}

const (
	reasonKindMismatch = "stage engine: facts are for the other item kind"
	reasonNoDeriver    = "stage engine: no deriver registered for this kind"
)

var (
	deriversMu    sync.RWMutex
	issueDeriver  Deriver
	prDeriver     Deriver
	issueDeriverF = DeriverFunc(stagelabels.IssueItem, func(f Facts) Decision { return delegate(stagelabels.IssueItem, f) })
	prDeriverF    = DeriverFunc(stagelabels.PRItem, func(f Facts) Decision { return delegate(stagelabels.PRItem, f) })
)

// SetDerivers installs the kind-specific derivers DeriveIssue and DerivePR
// delegate to. Until the derivation bodies move into this package (plan
// PR-F) these are the adapters in pkg/github (issue) and
// pkg/github/automerge (PR), wired by cmd/hive. A nil argument leaves that
// kind's registration unchanged.
func SetDerivers(issue, pr Deriver) {
	deriversMu.Lock()
	defer deriversMu.Unlock()
	if issue != nil {
		issueDeriver = issue
	}
	if pr != nil {
		prDeriver = pr
	}
}

// Derivers returns the registered derivers (nil when none).
func Derivers() (issue, pr Deriver) {
	deriversMu.RLock()
	defer deriversMu.RUnlock()
	return issueDeriver, prDeriver
}

func delegate(k stagelabels.Item, f Facts) Decision {
	issue, pr := Derivers()
	d := issue
	if k == stagelabels.PRItem {
		d = pr
	}
	if d == nil {
		return abstain(reasonNoDeriver)
	}
	return d.Derive(f)
}

// DeriveIssue derives an issue's stage. It asserts f.Ref.Kind is IssueItem
// and delegates to the registered issue deriver.
func DeriveIssue(f Facts) Decision { return issueDeriverF.Derive(f) }

// DerivePR derives a pull request's stage. It asserts f.Ref.Kind is PRItem
// and delegates to the registered PR deriver.
func DerivePR(f Facts) Decision { return prDeriverF.Derive(f) }

// IssueDeriver is DeriveIssue as a Deriver (the Options.Issue default).
func IssueDeriver() Deriver { return issueDeriverF }

// PRDeriver is DerivePR as a Deriver (the Options.PR default).
func PRDeriver() Deriver { return prDeriverF }
