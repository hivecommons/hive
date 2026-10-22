// Package evidence defines the per-PR review evidence bundle: one
// self-contained JSON document per owner/repo#number@headSHA recording what
// Hive reviewed, under which policy, what it found, what CI said and who
// merged. The package supplies the schema as Go types, a deterministic
// canonical-JSON encoding, a content hash and an optional detached Ed25519
// signature so a bundle can be verified offline.
//
// It is evidence, not certification: nothing here asserts compliance with any
// framework. This package is stdlib-only and imports nothing else in the
// module.
package evidence

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the value of Bundle.SchemaVersion for bundles produced by
// this package. Any incompatible change to the JSON shape bumps it.
const SchemaVersion = "v1"

// SchemaJSON is the JSON Schema (draft 2020-12) describing a v1 bundle.
//
//go:embed schema/v1.json
var SchemaJSON []byte

// Control-window assignments. These are observations, not dispatch or merge gates.
const (
	ControlWindowOn  = "on"
	ControlWindowOff = "off"
)

// Author kinds.
const (
	AuthorHuman = "human"
	AuthorAgent = "agent"
	AuthorBot   = "bot"
)

// Bundle is the review evidence record for a single PR head SHA.
//
// RevertOf and FollowupOf are the bundle ids of earlier merged PRs this PR
// reverts or fixes up; RevertedBy and FollowedUpBy are the reverse links
// written on those earlier bundles. They are a change-failure-rate proxy, not
// a verdict on the change (hivecommons/hive#12067). IssueReopens records
// issues this PR closed that a person reopened after the merge, the other
// change-failure signal (hivecommons/hive#13241).
type Bundle struct {
	SchemaVersion    string            `json:"schema_version"`
	ID               string            `json:"id"`
	Repo             string            `json:"repo"`
	Number           int               `json:"number"`
	Author           Author            `json:"author"`
	BaseSHA          string            `json:"base_sha"`
	HeadSHA          string            `json:"head_sha"`
	PreviousBundleID string            `json:"previous_bundle_id,omitempty"`
	Policy           Policy            `json:"policy"`
	Verdicts         []Verdict         `json:"verdicts,omitempty"`
	PostedReviews    []PostedReview    `json:"posted_reviews,omitempty"`
	CI               CI                `json:"ci"`
	Sentinel         []SentinelFinding `json:"sentinel,omitempty"`
	HumanActions     []Action          `json:"human_actions,omitempty"`
	Merge            *MergeEvent       `json:"merge,omitempty"`
	RevertOf         []string          `json:"revert_of,omitempty"`
	FollowupOf       []string          `json:"followup_of,omitempty"`
	RevertedBy       []string          `json:"reverted_by,omitempty"`
	FollowedUpBy     []string          `json:"followed_up_by,omitempty"`
	IssueReopens     []IssueReopen     `json:"issue_reopens,omitempty"`
	IssueClosures    []IssueClosure    `json:"issue_closures,omitempty"`
	Shadow           *Shadow           `json:"shadow,omitempty"`
	// ControlWindow is the prospectively assigned dispatch arm at PR open
	// time: on = Hive dispatch enabled, off = scheduled control window.
	// Empty means unassigned/unknown, never an inferred on-arm observation.
	ControlWindow string    `json:"control_window,omitempty"`
	Backfilled    bool      `json:"backfilled,omitempty"`
	Backfill      *Backfill `json:"backfill,omitempty"`
	Outcome       *Outcome  `json:"outcome,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Signed        bool      `json:"signed"`
	Hash          string    `json:"hash,omitempty"`
	Signature     string    `json:"signature,omitempty"`
}

// Author identifies the PR author. Kind is one of human, agent or bot. For
// an agent-authored PR, Agent, Backend and Model are the launch metadata the
// hive recorded when the agent opened it; empty when unknown.
type Author struct {
	Login   string `json:"login"`
	Kind    string `json:"kind"`
	Agent   string `json:"agent,omitempty"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Policy snapshots the review policy in force when the bundle was written.
type Policy struct {
	ACMMLevel          int      `json:"acmm_level"`
	Perspectives       []string `json:"perspectives,omitempty"`
	RequireApproval    bool     `json:"require_approval"`
	MinPriority        string   `json:"min_priority,omitempty"`
	HumanMergePaths    []string `json:"human_merge_paths,omitempty"`
	SentinelConfigHash string   `json:"sentinel_config_hash,omitempty"`
}

// Verdict is one reviewer's verdict for the head SHA.
type Verdict struct {
	Perspective       string    `json:"perspective"`
	Model             string    `json:"model"`
	Backend           string    `json:"backend"`
	Verdict           string    `json:"verdict"`
	Confidence        float64   `json:"confidence"`
	ConfidenceRaw     *float64  `json:"confidence_raw,omitempty"`
	ConfidenceDerived *float64  `json:"confidence_derived,omitempty"`
	Findings          []Finding `json:"findings,omitempty"`
	RecordedAt        time.Time `json:"recorded_at"`
	// AuthorModel is the model the verdict's report named as the PR's
	// author (the attribution trailer), when it did. The bundle's Author
	// carries the audit-recorded model; this is the reviewer's view of it.
	AuthorModel string `json:"author_model,omitempty"`
	// SameFamily is whether the reviewing model and the author's model share
	// a vendor family (modelfamily.SameFamily). Nil when either side is
	// unknown, so cross-family is never confused with cannot-say.
	SameFamily *bool `json:"same_family,omitempty"`
}

// Finding is a single review finding.
type Finding struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// PostedReview links a review Hive posted on the PR.
type PostedReview struct {
	URL  string    `json:"url"`
	Head string    `json:"head"`
	At   time.Time `json:"at"`
}

// CI is the check-run summary captured for the head SHA.
type CI struct {
	// Status is the merge-gate verdict (success, failure or pending) at
	// CapturedAt; empty on records written before it was captured.
	Status     string    `json:"status,omitempty"`
	Checks     []Check   `json:"checks,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
}

// Check is one CI check run.
type Check struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
}

// SentinelFinding records why the sentinel sweep flagged the PR.
type SentinelFinding struct {
	Rule    string   `json:"rule"`
	Summary string   `json:"summary"`
	Paths   []string `json:"paths,omitempty"`
}

// Action is a human action on the PR: approval, label change, hold lift.
type Action struct {
	Actor  string    `json:"actor"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// MergeEvent records who or what merged the PR. It is nil until merge.
// OpenedAt is when the PR was opened on GitHub, so At minus OpenedAt is its
// time to merge; zero on bundles recorded before it was captured. It is not
// the bundle's CreatedAt, which for a live bundle is when review started.
type MergeEvent struct {
	Actor    string    `json:"actor"`
	Method   string    `json:"method"`
	At       time.Time `json:"at"`
	SHA      string    `json:"sha"`
	OpenedAt time.Time `json:"opened_at,omitzero"`
}

// Shadow records what shadow-mode gates would have done for the head SHA.
// It is pure recording: nothing reads it to decide a merge.
type Shadow struct {
	ReviewGate *ShadowGate `json:"review_gate,omitempty"`
	Intent     *ShadowGate `json:"intent,omitempty"`
}

// ShadowGate is one shadow gate observation. ReviewGate sets WouldHaveHeld
// and Intent sets WouldHaveBlocked; a false value is kept (the gate ran and
// would not have acted), which is why they are pointers.
type ShadowGate struct {
	WouldHaveHeld    *bool  `json:"would_have_held,omitempty"`
	WouldHaveBlocked *bool  `json:"would_have_blocked,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Tier             string `json:"tier,omitempty"`
	// Lane is the merge lane that evaluated the gate: "self-merge" for the
	// self-authored sweep's ReviewGate (#12592); empty means the
	// merge-eligible lane.
	Lane        string    `json:"lane,omitempty"`
	Confidence  float64   `json:"confidence"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// Backfill states, as reconstructed from forge history.
const (
	BackfillStateOpen   = "open"
	BackfillStateClosed = "closed"
	BackfillStateMerged = "merged"
)

// Backfill exclusion flags (hivecommons/hive#12371). They mirror the
// pre-registration's population rules: a flagged bundle is kept on disk and
// every reader decides whether to exclude or annotate it. Nothing is dropped
// silently.
const (
	BackfillFlagPlaceholderHive = "placeholder_hive"
	BackfillFlagHiveOnHive      = "hive_on_hive"
	BackfillFlagUnderMinMerged  = "under_min_merged"
)

// BackfillFlags is every valid Backfill.Flags value.
var BackfillFlags = []string{BackfillFlagPlaceholderHive, BackfillFlagHiveOnHive, BackfillFlagUnderMinMerged}

// Backfill describes a bundle reconstructed after the fact from forge
// history rather than recorded live (hivecommons/hive#12371). Such a bundle
// has Backfilled set and carries only what the forge still knows: author,
// outcome, timestamps and change links. Verdicts, CI, sentinel and shadow
// observations are absent, never fabricated; policy.acmm_level is 0 unless
// the operator stamped a known level for the window.
type Backfill struct {
	// Source names the history the bundle was built from, e.g. "github-api".
	Source string `json:"source"`
	// At is when the backfill ran.
	At time.Time `json:"at"`
	// State is the PR's outcome at At: open, closed (without merge) or merged.
	State string `json:"state,omitempty"`
	// ClosedAt is when a closed PR was closed; zero when open or merged.
	ClosedAt time.Time `json:"closed_at,omitzero"`
	// AuthorKindRule names the heuristic that decided author.kind.
	AuthorKindRule string `json:"author_kind_rule,omitempty"`
	// Flags are the pre-registration exclusion flags that apply to the repo.
	Flags []string `json:"flags,omitempty"`
}

// OutcomeSourceLedger names the review-outcome ledger (pkg/review) as the
// source of a recorded Outcome: the governor noticed the PR had left its
// open list and asked the forge what became of it.
const OutcomeSourceLedger = "review-outcome-ledger"

// MergeUnrecorded is the Actor and Method of a merge event this hive did not
// perform and only learned about from the forge after the fact.
const MergeUnrecorded = "unrecorded"

// Outcome records what the forge said happened to a PR this hive reviewed
// but did not merge: closed without merge, or merged by someone else. A live
// bundle otherwise only ever states a merge the hive itself performed, so
// without this the PR would read as "outcome unknown" forever. It is written
// by the review-outcome ledger once the PR leaves the governor's open list
// (no extra forge call: the ledger already fetched the state) and is
// record-only; nothing reads it back into a merge decision. A bundle that
// carries a merged Outcome also carries a Merge event with Actor set to the
// merger the forge reported and Method MergeUnrecorded.
type Outcome struct {
	// Source names where the outcome was observed, e.g. OutcomeSourceLedger.
	Source string `json:"source"`
	// State is the PR's state at At: BackfillStateClosed or
	// BackfillStateMerged on disk; BackfillStateOpen only as a read-time hint.
	State string `json:"state"`
	// At is when the outcome was recorded.
	At time.Time `json:"at"`
	// MergedBy is the login the forge reported as the merger; empty when not
	// merged or unknown.
	MergedBy string `json:"merged_by,omitempty"`
	// MergedAt and ClosedAt are the forge's timestamps for the outcome.
	MergedAt time.Time `json:"merged_at,omitzero"`
	ClosedAt time.Time `json:"closed_at,omitzero"`
}

// BundleID returns the canonical bundle identifier owner/repo#number@head.
func BundleID(repo string, number int, head string) string {
	return fmt.Sprintf("%s#%d@%s", repo, number, head)
}

// Validate reports every missing or malformed required field, joined into
// one error, or nil when the bundle is well formed.
func (b *Bundle) Validate() error {
	if b == nil {
		return errors.New("evidence: nil bundle")
	}
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("evidence: "+format, args...))
	}
	if b.SchemaVersion != SchemaVersion {
		bad("schema_version %q, want %q", b.SchemaVersion, SchemaVersion)
	}
	if owner, name, ok := strings.Cut(b.Repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		bad("repo %q must be owner/name", b.Repo)
	}
	if b.ControlWindow != "" && b.ControlWindow != ControlWindowOn && b.ControlWindow != ControlWindowOff {
		bad("control_window %q must be on or off", b.ControlWindow)
	}
	if b.Number <= 0 {
		bad("number %d must be positive", b.Number)
	}
	if b.HeadSHA == "" {
		bad("head_sha is required")
	}
	if b.BaseSHA == "" {
		bad("base_sha is required")
	}
	if want := BundleID(b.Repo, b.Number, b.HeadSHA); b.ID != want {
		bad("id %q, want %q", b.ID, want)
	}
	if b.Author.Login == "" {
		bad("author.login is required")
	}
	switch b.Author.Kind {
	case AuthorHuman, AuthorAgent, AuthorBot:
	default:
		bad("author.kind %q must be human, agent or bot", b.Author.Kind)
	}
	if b.CreatedAt.IsZero() {
		bad("created_at is required")
	}
	if b.UpdatedAt.IsZero() {
		bad("updated_at is required")
	}
	for i, v := range b.Verdicts {
		if v.Perspective == "" || v.Model == "" || v.Verdict == "" {
			bad("verdicts[%d]: perspective, model and verdict are required", i)
		}
		if v.Confidence < 0 || v.Confidence > 1 {
			bad("verdicts[%d]: confidence %v outside [0,1]", i, v.Confidence)
		}
		if v.ConfidenceDerived != nil && (math.IsNaN(*v.ConfidenceDerived) || math.IsInf(*v.ConfidenceDerived, 0) || *v.ConfidenceDerived < 0 || *v.ConfidenceDerived > 1) {
			bad("verdicts[%d]: confidence_derived outside [0,1]", i)
		}
		if v.ConfidenceRaw != nil && (math.IsNaN(*v.ConfidenceRaw) || math.IsInf(*v.ConfidenceRaw, 0)) {
			bad("verdicts[%d]: confidence_raw must be finite", i)
		}
		if v.RecordedAt.IsZero() {
			bad("verdicts[%d]: recorded_at is required", i)
		}
		for j, f := range v.Findings {
			if f.Path == "" || f.Severity == "" || f.Summary == "" {
				bad("verdicts[%d].findings[%d]: path, severity and summary are required", i, j)
			}
		}
	}
	for i, r := range b.PostedReviews {
		if r.URL == "" || r.Head == "" {
			bad("posted_reviews[%d]: url and head are required", i)
		}
	}
	for i, c := range b.CI.Checks {
		if c.Name == "" || c.Conclusion == "" {
			bad("ci.checks[%d]: name and conclusion are required", i)
		}
	}
	for i, s := range b.Sentinel {
		if s.Rule == "" {
			bad("sentinel[%d]: rule is required", i)
		}
	}
	for i, a := range b.HumanActions {
		if a.Actor == "" || a.Kind == "" {
			bad("human_actions[%d]: actor and kind are required", i)
		}
	}
	if m := b.Merge; m != nil && (m.Actor == "" || m.Method == "" || m.SHA == "") {
		bad("merge: actor, method and sha are required")
	}
	if m := b.Merge; m != nil && !m.OpenedAt.IsZero() && m.OpenedAt.After(m.At) {
		bad("merge.opened_at must not be after merge.at")
	}
	if sh := b.Shadow; sh != nil {
		if g := sh.ReviewGate; g != nil {
			if g.WouldHaveHeld == nil || g.WouldHaveBlocked != nil {
				bad("shadow.review_gate: exactly would_have_held is required")
			}
			validateShadowGate("shadow.review_gate", g, bad)
		}
		if g := sh.Intent; g != nil {
			if g.WouldHaveBlocked == nil || g.WouldHaveHeld != nil {
				bad("shadow.intent: exactly would_have_blocked is required")
			}
			validateShadowGate("shadow.intent", g, bad)
		}
	}
	if bf := b.Backfill; bf != nil {
		if !b.Backfilled {
			bad("backfill: present but backfilled is false")
		}
		if bf.Source == "" {
			bad("backfill.source is required")
		}
		if bf.At.IsZero() {
			bad("backfill.at is required")
		}
		switch bf.State {
		case "", BackfillStateOpen, BackfillStateClosed, BackfillStateMerged:
		default:
			bad("backfill.state %q must be open, closed or merged", bf.State)
		}
		for i, f := range bf.Flags {
			if !slices.Contains(BackfillFlags, f) {
				bad("backfill.flags[%d]: unknown flag %q", i, f)
			}
		}
	}
	if o := b.Outcome; o != nil {
		if o.Source == "" {
			bad("outcome.source is required")
		}
		if o.At.IsZero() {
			bad("outcome.at is required")
		}
		switch o.State {
		case BackfillStateOpen, BackfillStateClosed, BackfillStateMerged:
		default:
			bad("outcome.state %q must be open, closed or merged", o.State)
		}
		if o.State == BackfillStateMerged && b.Merge == nil {
			bad("outcome.state merged requires a merge event")
		}
	}
	for _, l := range []struct {
		name string
		ids  []string
	}{
		{"revert_of", b.RevertOf}, {"followup_of", b.FollowupOf},
		{"reverted_by", b.RevertedBy}, {"followed_up_by", b.FollowedUpBy},
	} {
		for i, id := range l.ids {
			if id == "" {
				bad("%s[%d]: bundle id is required", l.name, i)
			}
		}
	}
	for i, c := range b.IssueClosures {
		if strings.TrimSpace(c.Repo) == "" || c.Issue <= 0 || c.At.IsZero() {
			bad("issue_closures[%d]: repo, positive issue and at are required", i)
		}
	}
	for i, r := range b.IssueReopens {
		if r.Issue <= 0 {
			bad("issue_reopens[%d]: issue must be positive", i)
		}
		if r.At.IsZero() {
			bad("issue_reopens[%d]: at is required", i)
		}
		if r.Source == "" {
			bad("issue_reopens[%d]: source is required", i)
		}
	}
	return errors.Join(errs...)
}

func validateShadowGate(name string, g *ShadowGate, bad func(string, ...any)) {
	if g.Confidence < 0 || g.Confidence > 1 {
		bad("%s: confidence %v outside [0,1]", name, g.Confidence)
	}
	if g.EvaluatedAt.IsZero() {
		bad("%s: evaluated_at is required", name)
	}
}

// Canonical returns the deterministic JSON encoding of b: object keys sorted
// lexically, no insignificant whitespace, no HTML escaping. Two bundles with
// the same content always yield the same bytes.
func Canonical(b *Bundle) ([]byte, error) {
	if b == nil {
		return nil, errors.New("evidence: nil bundle")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("evidence: marshal bundle: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("evidence: decode bundle: %w", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Maps encode with sorted keys, which is what makes this canonical.
	if err := enc.Encode(generic); err != nil {
		return nil, fmt.Errorf("evidence: encode canonical: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
