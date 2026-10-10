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

// Author kinds.
const (
	AuthorHuman = "human"
	AuthorAgent = "agent"
	AuthorBot   = "bot"
)

// Bundle is the review evidence record for a single PR head SHA.
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
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Signed           bool              `json:"signed"`
	Hash             string            `json:"hash,omitempty"`
	Signature        string            `json:"signature,omitempty"`
}

// Author identifies the PR author. Kind is one of human, agent or bot.
type Author struct {
	Login string `json:"login"`
	Kind  string `json:"kind"`
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
	Perspective string    `json:"perspective"`
	Model       string    `json:"model"`
	Backend     string    `json:"backend"`
	Verdict     string    `json:"verdict"`
	Confidence  float64   `json:"confidence"`
	Findings    []Finding `json:"findings,omitempty"`
	RecordedAt  time.Time `json:"recorded_at"`
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
type MergeEvent struct {
	Actor  string    `json:"actor"`
	Method string    `json:"method"`
	At     time.Time `json:"at"`
	SHA    string    `json:"sha"`
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
	return errors.Join(errs...)
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
