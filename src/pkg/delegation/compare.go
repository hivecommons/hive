package delegation

import (
	"sort"
	"strings"
	"time"
)

// THE COMPARISON HARNESS — the actual product of the observe phase.
//
// Minting chains nobody reads would prove nothing. The point of shipping
// observe-only is to answer, with data rather than argument, a question we
// cannot currently answer: DOES THE CHAIN AGREE WITH THE ATTRIBUTION HIVE
// ALREADY RECORDS, and where it disagrees, which one is right?
//
// The audit log (src/docs/audit-log.md) is the incumbent attribution record:
// one line per action with a single `user` field. The chain is the challenger:
// a signed, typed, multi-link statement about the same action. Running them
// side by side surfaces four outcomes, and every one of them is information:
//
//   - AGREE — the chain's human root matches the audit user. The chain adds
//     the delegation path but contradicts nothing.
//   - CHAIN_ONLY — the audit log recorded a pseudo-user ("system", "") where
//     the chain has a real machine principal. These are the cases where the
//     audit log is LOSING information today, and they are the expected bulk of
//     the findings: every cadence kick lands here.
//   - AUDIT_ONLY — the audit log named a human where the chain declined to
//     emit. Each of these is a place our root resolution is incomplete OR the
//     audit log is over-attributing; both are worth knowing before anything
//     enforces.
//   - CONFLICT — both name a human and they DIFFER. This is the finding that
//     would matter most and that we most expect to be empty. A non-empty
//     conflict set is a hard blocker on ever enforcing.
//
// WHY A HARNESS AND NOT A DASHBOARD CARD. The output is meant to be diffed over
// time and read in bulk, not glanced at. It is a pure function over two slices
// with no I/O, so it runs identically in a test, in a one-off analysis over a
// downloaded audit.jsonl, and in a future periodic job — and it can be unit
// tested against synthetic disagreements, which a UI could not.

// Verdict is the outcome of comparing one chain against one audit entry.
type Verdict string

const (
	VerdictAgree     Verdict = "agree"
	VerdictChainOnly Verdict = "chain_only"
	VerdictAuditOnly Verdict = "audit_only"
	VerdictConflict  Verdict = "conflict"
)

// AuditRecord is the subset of pkg/dashboard's AuditEntry this harness needs.
//
// A local struct rather than an import, for the same near-leaf reason as
// deriveDomainKey: pkg/dashboard imports pkg/delegation. The field names match
// the documented JSONL keys exactly (src/docs/audit-log.md), so a raw
// audit.jsonl line unmarshals straight into this type — which is what lets the
// harness run over a downloaded log with no hive running.
type AuditRecord struct {
	Timestamp string `json:"ts"`
	User      string `json:"user"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

// ObservedChain pairs a minted chain with the action and agent it described, so
// it can be joined against an audit record.
type ObservedChain struct {
	Timestamp string
	Action    string
	Agent     string
	Chain     Chain
}

// Finding is one compared pair.
type Finding struct {
	Verdict Verdict `json:"verdict"`
	Action  string  `json:"action"`
	Agent   string  `json:"agent,omitempty"`

	// AuditUser is the audit log's `user` verbatim, pseudo-users included —
	// NOT normalized away. "system" appearing here is the finding, so
	// collapsing it to "" would erase the thing being measured.
	AuditUser string `json:"audit_user,omitempty"`

	// ChainRoot is the chain's root rendered with its type prefix, e.g.
	// "hive_authority:acme(cadence:scanner)". Always type-prefixed so a reader
	// comparing it against AuditUser cannot mistake a machine for a person.
	ChainRoot string `json:"chain_root,omitempty"`

	// ChainPath is the full root-first delegation path — the information the
	// audit log structurally cannot hold.
	ChainPath string `json:"chain_path,omitempty"`

	// RootIsHuman distinguishes "the chain names a person" from "the chain
	// names something". It is the typed answer to the question the audit log
	// can only answer by string-matching against auditPseudoUsers.
	RootIsHuman bool `json:"root_is_human"`
}

// ComparisonReport is the aggregate.
type ComparisonReport struct {
	Findings []Finding      `json:"findings"`
	Counts   map[string]int `json:"counts"`

	// UnmatchedChains counts chains with no corresponding audit entry. A
	// non-zero value is not necessarily wrong — a chain can describe an action
	// that was never audited, which is itself a gap worth seeing — but it does
	// mean the join is incomplete, so it is reported rather than hidden.
	UnmatchedChains int `json:"unmatched_chains"`
}

// Compare joins observed chains against audit records and classifies each pair.
//
// THE JOIN. On (action, agent, timestamp-to-the-second). Deliberately strict:
// a loose join that fuzzy-matched on action alone would manufacture agreement
// between unrelated events, and a harness that reports false agreement is worse
// than no harness — it would be cited as evidence the chain is safe to enforce.
// Records that do not join cleanly are reported as unmatched rather than
// forced.
//
// PURE. No I/O, no clock, no environment. `now` is not a parameter because
// nothing here depends on the present: this compares two historical records.
func Compare(chains []ObservedChain, audits []AuditRecord) ComparisonReport {
	report := ComparisonReport{Counts: map[string]int{}}

	// Index audit records by join key. A slice per key, because the same
	// (action, agent, second) can legitimately occur more than once and
	// silently dropping duplicates would undercount.
	byKey := map[string][]AuditRecord{}
	for _, a := range audits {
		k := joinKey(a.Action, a.Agent, a.Timestamp)
		byKey[k] = append(byKey[k], a)
	}
	matched := map[string]int{}

	for _, oc := range chains {
		k := joinKey(oc.Action, oc.Agent, oc.Timestamp)
		candidates := byKey[k]
		idx := matched[k]
		if idx >= len(candidates) {
			report.UnmatchedChains++
			continue
		}
		matched[k]++
		report.Findings = append(report.Findings, classify(oc, candidates[idx]))
	}

	// Audit entries with no chain. Counted only where a chain COULD have
	// existed — i.e. the audit named a real human — because an unmatched
	// "system" entry is the everyday case (the flag may be off, or the action
	// may have no emit site yet) and reporting all of them would bury the
	// signal under noise.
	for k, recs := range byKey {
		for i := matched[k]; i < len(recs); i++ {
			a := recs[i]
			if isAuditPseudoUser(a.User) {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Verdict:   VerdictAuditOnly,
				Action:    a.Action,
				Agent:     a.Agent,
				AuditUser: a.User,
			})
		}
	}

	sort.SliceStable(report.Findings, func(i, j int) bool {
		if report.Findings[i].Verdict != report.Findings[j].Verdict {
			return report.Findings[i].Verdict < report.Findings[j].Verdict
		}
		return report.Findings[i].Action < report.Findings[j].Action
	})
	for _, f := range report.Findings {
		report.Counts[string(f.Verdict)]++
	}
	return report
}

// classify decides the verdict for one joined pair.
func classify(oc ObservedChain, a AuditRecord) Finding {
	root, ok := oc.Chain.Root()
	f := Finding{
		Action:      oc.Action,
		Agent:       oc.Agent,
		AuditUser:   a.User,
		ChainPath:   oc.Chain.Describe(),
		RootIsHuman: oc.Chain.HasHumanRoot(),
	}
	if ok {
		f.ChainRoot = root.String()
	}

	auditIsPerson := !isAuditPseudoUser(a.User)
	switch {
	case !auditIsPerson:
		// The audit log has no person; the chain has SOMETHING. This is the
		// information-gain case, and it covers every cadence-triggered action.
		f.Verdict = VerdictChainOnly
	case !f.RootIsHuman:
		// The audit log named a person but the chain's root is a machine.
		//
		// Classified as CONFLICT and not as a lesser verdict, deliberately.
		// These two records disagree about the most consequential question
		// there is — whether a human authorized this — and burying that in a
		// softer bucket is how it would be overlooked. It may well turn out
		// that the audit log is the one over-attributing (a dashboard-initiated
		// action whose real authority is the hive's), but that conclusion has to
		// be reached by reading the finding, not assumed by the classifier.
		f.Verdict = VerdictConflict
	case strings.EqualFold(root.ID, a.User):
		f.Verdict = VerdictAgree
	default:
		// Both name a person and they differ. The finding that would block
		// enforcement outright.
		f.Verdict = VerdictConflict
	}
	return f
}

// joinKey builds the (action, agent, second) join key.
//
// Timestamps are normalized to whole seconds because the audit log records
// RFC 3339 at second granularity (src/docs/audit-log.md) while a chain's
// IssuedAt is a Unix second — the two are already the same resolution, and
// normalizing makes that explicit rather than incidental. A timestamp that does
// not parse contributes an empty component rather than failing the whole join;
// a malformed line should cost one comparison, not the report.
func joinKey(action, agent, ts string) string {
	sec := ""
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		sec = t.UTC().Format(time.RFC3339)
	}
	return action + "\x00" + agent + "\x00" + sec
}

// HasBlockingFindings reports whether the report contains anything that must be
// resolved before enforcement could be considered.
//
// OBSERVE-ONLY NOTE: this is a REPORTING predicate over a report an operator is
// reading. It does not run in any request path and nothing in hive calls it to
// decide anything — it exists so a future analysis job, or a human, can ask
// "is the conflict set empty yet?" without reimplementing the question. The
// observe-only invariant test permits it precisely because its input is a
// ComparisonReport and never a live Chain.
func (r ComparisonReport) HasBlockingFindings() bool {
	return r.Counts[string(VerdictConflict)] > 0
}
