package extwork

import (
	"fmt"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// Verdict is Hive's second decision about a receipt (#8201 section C). A
// receipt is a claim; the verdict is what Hive concludes from it under current
// authorization. no_change and blocked are their own classes, never success.
type Verdict string

const (
	VerdictAccepted  Verdict = "accepted"
	VerdictNoChange  Verdict = "no_change"
	VerdictBlocked   Verdict = "blocked"
	VerdictRejected  Verdict = "rejected"
	VerdictUncertain Verdict = "uncertain"
)

// Decision pairs the verdict with its reason. ExecutionFact is always true
// when a receipt was presented: a rejected candidate still happened, and the
// evidence is retained rather than suppressed (#8201 row 12).
type Decision struct {
	Verdict       Verdict `json:"verdict"`
	Reason        string  `json:"reason"`
	ExecutionFact bool    `json:"execution_fact"`
}

// Predicate is the independently configured acceptance check Hive applies to a
// completed receipt after binding. It returns nil to accept the candidate.
type Predicate func(adm Admission, receipt *outputschema.StageReceipt) error

// AcceptBound is the minimal predicate: the receipt is bound to the admission
// and nothing else is required. Callers layer stricter checks on top.
func AcceptBound(adm Admission, receipt *outputschema.StageReceipt) error {
	return BindReceipt(adm, receipt)
}

// Decide is the acceptance step. authorityCurrent is whether the lease that
// admitted the run is still the current authority; late output after
// revocation can never resurrect it (#8201 row 8).
func Decide(adm Admission, receipt *outputschema.StageReceipt, predicate Predicate, authorityCurrent bool) Decision {
	if receipt == nil {
		return Decision{Verdict: VerdictUncertain, Reason: "no receipt"}
	}
	if err := BindReceipt(adm, receipt); err != nil {
		return Decision{Verdict: VerdictRejected, Reason: err.Error(), ExecutionFact: true}
	}
	if !authorityCurrent {
		return Decision{Verdict: VerdictRejected, Reason: "authority revoked before acceptance", ExecutionFact: true}
	}
	switch receipt.ResultClass {
	case outputschema.ReceiptResultNoChange:
		return Decision{Verdict: VerdictNoChange, Reason: "engine reported no change", ExecutionFact: true}
	case outputschema.ReceiptResultBlocked:
		return Decision{Verdict: VerdictBlocked, Reason: "engine reported blocked", ExecutionFact: true}
	case outputschema.ReceiptResultFailed:
		return Decision{Verdict: VerdictRejected, Reason: "engine reported failure", ExecutionFact: true}
	case outputschema.ReceiptResultUnknown:
		return Decision{Verdict: VerdictUncertain, Reason: "engine reported unknown result", ExecutionFact: true}
	case outputschema.ReceiptResultCompleted:
		if predicate == nil {
			predicate = AcceptBound
		}
		if err := predicate(adm, receipt); err != nil {
			return Decision{Verdict: VerdictRejected, Reason: fmt.Sprintf("predicate rejected candidate: %v", err), ExecutionFact: true}
		}
		return Decision{Verdict: VerdictAccepted, Reason: "receipt bound and predicate satisfied", ExecutionFact: true}
	default:
		return Decision{Verdict: VerdictRejected, Reason: fmt.Sprintf("unknown result class %q", receipt.ResultClass), ExecutionFact: true}
	}
}
