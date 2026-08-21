// Package proof implements the first exact-subject GitHub proof vertical
// accepted on kubestellar/hive#4252 (parent epic #3845, issue #4253).
//
// It owns exactly one predicate — github.checks.exact-head-green/v1: "all
// required (non-meta) check runs for exact head SHA X of PR N in repo R
// concluded success" — and one durable receipt shape: a proof Record binding
// the complete accepted fingerprint (outcome key, predicate ID, desired
// generation, repo, PR, exact candidate head SHA, bounded base observation,
// check-policy identity, producer authority) to a tri-state result, a bounded
// provenance handle, and an observation instant with a freshness rule.
//
// Only a valid CURRENT proof whose complete fingerprint matches the exact
// evaluation context satisfies the predicate. A repaired head Y can never be
// authorized by evidence for head X; generation G's receipt can never satisfy
// G+1; movement of any declared input (base, check policy) invalidates only
// receipts declaring that input. Missing, stale, malformed, unauthorized, or
// conflicting evidence is Unknown — never satisfaction.
//
// The package is inert by default. Nothing records or consults proofs unless
// the convergence mode toggle (convergence.mode / HIVE_CONVERGENCE_MODE,
// #4246/#4263) resolves to a non-off mode; and even then, shadow mode only
// records and reports — a proof verdict gates a dependent transition ONLY in
// enforce mode, and only for the one transition explicitly declared to depend
// on it. Existing merge, CI, review, queue-approval (#3152), and automerge
// guards are mandatory positive guards this receipt never replaces.
package proof

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PredicateExactHeadGreen is the ONE predicate this vertical implements, as
// selected and accepted on #4252. The identifier is versioned: a schema or
// semantics change is a NEW predicate ID, never a silent reinterpretation of
// receipts recorded under this one.
const PredicateExactHeadGreen = "github.checks.exact-head-green/v1"

// ProducerGitHubChecksAPI is the sole accepted producer authority for the
// exact-head-green predicate: the GitHub Checks API of the subject repository.
// Producer identity is explicit — another vendor's checks, or Hive's own
// review harness, is NOT this producer, and multiplicity of producers is
// never inferred to be independence.
const ProducerGitHubChecksAPI = "github-checks-api"

// keyFieldSeparator joins the load-bearing fingerprint fields into the
// receipt key. "|" cannot appear in an outcome key ("project/repo@outcome"
// forbids whitespace and reserves separators), a predicate ID, a decimal
// generation, or a hex SHA, so the key is unambiguous by construction.
const keyFieldSeparator = "|"

// shaPattern is the exact shape of a full git object name. Fingerprints pin
// FULL 40-hex SHAs: an abbreviated SHA is a lookup convenience, not an
// immutable subject identity.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Result is the producer's classified conclusion for the predicate's check
// set at the fingerprinted head. It is deliberately the same three-way
// classification EnrichCIStatus already computes from check-run conclusions.
type Result string

const (
	// ResultSuccess: every required (non-meta) check run for the exact head
	// completed with a passing conclusion.
	ResultSuccess Result = "success"
	// ResultFailure: at least one required check run for the exact head
	// concluded failure or action_required.
	ResultFailure Result = "failure"
	// ResultPending: required check runs exist but have not all completed,
	// or no counted check run exists at all (no evidence is never success).
	ResultPending Result = "pending"
)

// validResult reports whether r is one of the three declared classifications.
// Anything else in a persisted or constructed record is malformed evidence,
// and malformed evidence can never satisfy.
func validResult(r Result) bool {
	return r == ResultSuccess || r == ResultFailure || r == ResultPending
}

// Fingerprint is the complete immutable subject binding accepted on #4252.
// EVERY field is REQUIRED and load-bearing: a receipt satisfies the predicate
// only when every field matches the current evaluation context exactly, so
// removing or ignoring any one of them transfers proof across subjects,
// generations, or assumptions — the exact failure the invariant forbids.
type Fingerprint struct {
	// OutcomeKey is the canonical "project/repo@outcome" identity of the
	// accepted #4251 outcome this proof serves. A receipt for another outcome
	// never transfers.
	OutcomeKey string `json:"outcome_key"`
	// PredicateID names the predicate and its schema version. This vertical
	// only ever writes PredicateExactHeadGreen.
	PredicateID string `json:"predicate_id"`
	// DesiredGeneration is the outcome ledger's current desired generation at
	// evidence time. G's receipt can never satisfy G+1.
	DesiredGeneration int `json:"desired_generation"`
	// Repo is the canonical "owner/repo" spelling, byte-identical to the
	// spelling worksource.Ref.Repo and outcome.Ref.Repo use.
	Repo string `json:"repo"`
	// PRNumber is the exact pull request the candidate head belongs to.
	PRNumber int `json:"pr_number"`
	// HeadSHA is the exact candidate head — THE immutability anchor. A commit
	// SHA is content-addressed, so evidence bound to X can never describe a
	// repaired head Y.
	HeadSHA string `json:"head_sha"`
	// BaseSHA is the bounded base-reference observation: the PR base ref's
	// commit SHA at observation time. It declares the dependency — base
	// movement invalidates ONLY receipts declaring this field. It is an
	// observation bound, not a claim that CI ran against that merge-base.
	BaseSHA string `json:"base_sha"`
	// CheckPolicyID identifies WHICH check runs count: see CheckPolicyID().
	// A policy or classifier change invalidates receipts declaring the old
	// policy — nothing else.
	CheckPolicyID string `json:"check_policy_id"`
	// Producer is the explicit producer authority; this vertical only ever
	// writes ProducerGitHubChecksAPI.
	Producer string `json:"producer"`
}

// Validate reports why a Fingerprint cannot serve as an immutable subject
// binding. Every field is required; a fingerprint that fails Validate has no
// key and can never be recorded or satisfied.
func (f Fingerprint) Validate() error {
	if f.OutcomeKey == "" {
		return fmt.Errorf("proof fingerprint requires an outcome key")
	}
	if strings.ContainsAny(f.OutcomeKey, keyFieldSeparator+" \t") {
		return fmt.Errorf("outcome key %q may not contain %q or whitespace", f.OutcomeKey, keyFieldSeparator)
	}
	if f.PredicateID != PredicateExactHeadGreen {
		return fmt.Errorf("predicate %q is not implemented by this vertical (only %s)", f.PredicateID, PredicateExactHeadGreen)
	}
	if f.DesiredGeneration < 1 {
		return fmt.Errorf("desired generation %d is impossible (generations start at 1)", f.DesiredGeneration)
	}
	if f.Repo == "" || strings.Count(f.Repo, "/") != 1 || strings.ContainsAny(f.Repo, keyFieldSeparator+"@#! \t") {
		return fmt.Errorf("repo %q is not a canonical owner/repo spelling", f.Repo)
	}
	if f.PRNumber < 1 {
		return fmt.Errorf("pr number %d is not a real pull request", f.PRNumber)
	}
	if !shaPattern.MatchString(f.HeadSHA) {
		return fmt.Errorf("head sha %q is not a full 40-hex commit id", f.HeadSHA)
	}
	if !shaPattern.MatchString(f.BaseSHA) {
		return fmt.Errorf("base sha %q is not a full 40-hex commit id", f.BaseSHA)
	}
	if f.CheckPolicyID == "" {
		return fmt.Errorf("proof fingerprint requires a check-policy id")
	}
	if strings.ContainsAny(f.CheckPolicyID, keyFieldSeparator+" \t") {
		return fmt.Errorf("check-policy id %q may not contain %q or whitespace", f.CheckPolicyID, keyFieldSeparator)
	}
	if f.Producer != ProducerGitHubChecksAPI {
		return fmt.Errorf("producer %q is not the accepted authority %s", f.Producer, ProducerGitHubChecksAPI)
	}
	return nil
}

// Key returns the accepted dedup/serialization key —
// "outcomeKey|predicateID|generation|headSHA" — or "" for a fingerprint that
// fails Validate, mirroring the worksource/outcome contract that "" means
// "not identifiable, never a key". Duplicate identical evidence deduplicates
// deterministically on this key, and concurrent writers serialize on it.
func (f Fingerprint) Key() string {
	if err := f.Validate(); err != nil {
		return ""
	}
	return strings.Join([]string{
		f.OutcomeKey, f.PredicateID, fmt.Sprintf("%d", f.DesiredGeneration), f.HeadSHA,
	}, keyFieldSeparator)
}

// Provenance is the bounded replay handle: enough to re-issue the exact
// observation and audit which runs were counted — deliberately NOT a log
// archive.
type Provenance struct {
	// CheckRunIDs are the check-run IDs counted toward the result, sorted.
	CheckRunIDs []int64 `json:"check_run_ids,omitempty"`
	// Query names the observation call shape, e.g.
	// "ListCheckRunsForRef@<headSHA>".
	Query string `json:"query"`
}

// Record is one durable proof receipt: the complete fingerprint, the
// producer's classified result, the bounded provenance handle, and the
// observation instant its freshness rule is anchored to. Records are
// immutable once written; a re-observation writes a superseding record under
// the same key, never an in-place edit.
type Record struct {
	Fingerprint Fingerprint `json:"fingerprint"`
	Result      Result      `json:"result"`
	Provenance  Provenance  `json:"provenance"`
	// ObservedAt is the observation instant, RFC3339 in the persisted file.
	ObservedAt time.Time `json:"observed_at"`
}

// Validate reports why a Record cannot serve as evidence. Malformed evidence
// is refused at the write seam and treated as Unknown at the read seam — it
// can never become satisfaction.
func (r Record) Validate() error {
	if err := r.Fingerprint.Validate(); err != nil {
		return err
	}
	if !validResult(r.Result) {
		return fmt.Errorf("result %q is not a known classification", r.Result)
	}
	if r.ObservedAt.IsZero() {
		return fmt.Errorf("proof record requires an observation instant")
	}
	return nil
}

// Key returns the record's dedup/serialization key (see Fingerprint.Key).
func (r Record) Key() string { return r.Fingerprint.Key() }

// clone returns a detached deep copy so readers can never race or mutate the
// store's own record.
func (r Record) clone() Record {
	out := r
	out.Provenance.CheckRunIDs = append([]int64(nil), r.Provenance.CheckRunIDs...)
	return out
}

// equivalent reports whether two records are the SAME evidence — identical
// fingerprint, result, and provenance. ObservedAt is deliberately excluded:
// re-reading unchanged evidence a second later is a duplicate, and duplicates
// deduplicate without duplicating state or effects.
func (r Record) equivalent(other Record) bool {
	if r.Fingerprint != other.Fingerprint || r.Result != other.Result || r.Provenance.Query != other.Provenance.Query {
		return false
	}
	if len(r.Provenance.CheckRunIDs) != len(other.Provenance.CheckRunIDs) {
		return false
	}
	for i, id := range r.Provenance.CheckRunIDs {
		if other.Provenance.CheckRunIDs[i] != id {
			return false
		}
	}
	return true
}

// CheckPolicyID derives the check-policy identity field: a sha256 over the
// classifier version and the SORTED set of check-run names that counted. The
// classifier version names the meta/ignorable ruleset revision (the
// isMetaCheck family), so EITHER a change to which checks a repo runs OR a
// change to the classifier itself yields a different policy ID — and
// therefore invalidates exactly the receipts that declared the old policy.
func CheckPolicyID(classifierVersion string, checkNames []string) string {
	names := append([]string(nil), checkNames...)
	sort.Strings(names)
	h := sha256.New()
	h.Write([]byte(classifierVersion))
	for _, n := range names {
		h.Write([]byte{0})
		h.Write([]byte(n))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// CheckConclusion is one counted check run's state as reported by the
// producer, in the shape the existing EnrichCIStatus classification reads.
type CheckConclusion struct {
	// ID is the producer's check-run ID (provenance).
	ID int64
	// Name is the check-run name (policy identity).
	Name string
	// Completed reports whether the run reached a terminal status.
	Completed bool
	// Conclusion is the terminal conclusion when Completed
	// ("success", "failure", "action_required", "neutral", "skipped", ...).
	Conclusion string
}

// NormalizeCheckEvidence turns one authoritative check-run observation for an
// exact head into a proof Record, classifying conclusions with EXACTLY the
// semantics EnrichCIStatus applies to real CI checks: any failure or
// action_required conclusion is failure; any incomplete run is pending; an
// EMPTY counted set is pending (no evidence is never success); otherwise
// success. The caller has already excluded meta/ignorable checks — the
// fingerprint's CheckPolicyID is derived from the counted names, so the
// exclusion decision is itself part of the immutable subject binding.
func NormalizeCheckEvidence(fp Fingerprint, checks []CheckConclusion, observedAt time.Time) (Record, error) {
	if err := fp.Validate(); err != nil {
		return Record{}, err
	}
	result := ResultSuccess
	if len(checks) == 0 {
		result = ResultPending
	}
	ids := make([]int64, 0, len(checks))
	for _, c := range checks {
		ids = append(ids, c.ID)
		switch {
		case c.Completed && (c.Conclusion == "failure" || c.Conclusion == "action_required"):
			result = ResultFailure
		case !c.Completed && result != ResultFailure:
			result = ResultPending
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rec := Record{
		Fingerprint: fp,
		Result:      result,
		Provenance: Provenance{
			CheckRunIDs: ids,
			Query:       "ListCheckRunsForRef@" + fp.HeadSHA,
		},
		ObservedAt: observedAt,
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}
