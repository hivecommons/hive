package stageengine

import "github.com/hivecommons/hive/pkg/stagelabels"

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

const reasonKindMismatch = "stage engine: facts are for the other item kind"

var (
	issueDeriverF = DeriverFunc(stagelabels.IssueItem, deriveIssue)
	prDeriverF    = DeriverFunc(stagelabels.PRItem, derivePR)
)

// DeriveIssue derives an issue's stage from its facts, without forge I/O.
func DeriveIssue(f Facts) Decision { return issueDeriverF.Derive(f) }

// DerivePR derives a pull request's stage from its facts, without forge I/O.
func DerivePR(f Facts) Decision { return prDeriverF.Derive(f) }

// IssueDeriver is DeriveIssue as a Deriver (the Options.Issue default).
func IssueDeriver() Deriver { return issueDeriverF }

// PRDeriver is DerivePR as a Deriver (the Options.PR default).
func PRDeriver() Deriver { return prDeriverF }
