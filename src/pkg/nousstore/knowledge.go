package nousstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Knowledge schema (RFC #11585 part 4, #11697).
//
// The Experiment Lab's history timeline and principle list key on two
// fields the writers and the reader never agreed on. Nous's own
// ledger.schema.json has no `type` or `status` at all — a row is "one record
// per completed iteration" whose outcome is `h_main_result`; its
// principles.schema.json carries `status` (active/updated/pruned) and
// `category` (domain/meta). The dashboard, written against the earlier
// governor experiment loop, grouped ledger rows by `type` with values that
// mixed kinds (dry_run, analysis) with states (active, completed, aborted).
//
// This file is the one schema both sides use:
//
//   - every ledger entry served by GET /api/nous/ledger carries KeyType (what
//     kind of row it is) and KeyStatus (where it is in its life), plus
//     KeyOutcome when the row recorded one;
//   - every principle served by GET /api/nous/principles carries KeyType
//     (Nous's category) and KeyStatus.
//
// Rows written without the fields are normalized on read with the defaults
// below, so files written by any earlier Nous or governor loop load
// unchanged. Rows that do carry them are validated against the constant
// sets; an unknown value is reported and the default substituted, so one
// bad row never blanks the panel.

// JSON keys the schema adds to, or reads from, every entry.
const (
	KeyType    = "type"
	KeyStatus  = "status"
	KeyOutcome = "outcome"

	// Nous-native ledger keys consulted when KeyType/KeyStatus are absent.
	keyFamily      = "family"
	keyCandidateID = "candidate_id"
	keyHMainResult = "h_main_result"

	// Nous-native principle keys.
	keyID         = "id"
	keyStatement  = "statement"
	keyConfidence = "confidence"
	keyCategory   = "category"

	// nousBaselineFamily marks the seed row of a Nous ledger.
	nousBaselineFamily = "baseline"
)

// Ledger entry types: what kind of row this is.
const (
	// LedgerTypeBaseline is the seed row recording the untouched system.
	LedgerTypeBaseline = "baseline"
	// LedgerTypeExperiment is one round of the scientific loop (the Nous
	// default; also the type behind the legacy active/completed/aborted rows).
	LedgerTypeExperiment = "experiment"
	// LedgerTypeDryRun is a proposal that was logged but never executed
	// (observe mode's "would have tested").
	LedgerTypeDryRun = "dry_run"
	// LedgerTypeAnalysis is a pure analysis pass over collected data.
	LedgerTypeAnalysis = "analysis"
	// LedgerTypeShadowAnalysis is an analysis of a change that was never
	// applied to the live system.
	LedgerTypeShadowAnalysis = "shadow_analysis"
)

// Ledger entry statuses: where the row is in its life.
const (
	LedgerStatusPending   = "pending"
	LedgerStatusActive    = "active"
	LedgerStatusCompleted = "completed"
	LedgerStatusAborted   = "aborted"
)

// Principle types, mirroring Nous's `category` enum.
const (
	// PrincipleTypeDomain is about the system under study.
	PrincipleTypeDomain = "domain"
	// PrincipleTypeMeta is about the investigation process itself.
	PrincipleTypeMeta = "meta"
)

// Principle statuses, mirroring Nous's `status` enum.
const (
	PrincipleStatusActive  = "active"
	PrincipleStatusUpdated = "updated"
	PrincipleStatusPruned  = "pruned"
)

// Defaults applied when a file does not carry a field.
const (
	DefaultLedgerType      = LedgerTypeExperiment
	DefaultLedgerStatus    = LedgerStatusCompleted
	DefaultPrincipleType   = PrincipleTypeDomain
	DefaultPrincipleStatus = PrincipleStatusActive
)

// Confidence scores for Nous's low/medium/high enum, and for a principle
// whose confidence is missing or unrecognised.
const (
	ConfidenceHigh    = 0.9
	ConfidenceMedium  = 0.7
	ConfidenceLow     = 0.4
	ConfidenceUnknown = 0.5
)

var (
	ledgerTypes       = set(LedgerTypeBaseline, LedgerTypeExperiment, LedgerTypeDryRun, LedgerTypeAnalysis, LedgerTypeShadowAnalysis)
	ledgerStatuses    = set(LedgerStatusPending, LedgerStatusActive, LedgerStatusCompleted, LedgerStatusAborted)
	principleTypes    = set(PrincipleTypeDomain, PrincipleTypeMeta)
	principleStatuses = set(PrincipleStatusActive, PrincipleStatusUpdated, PrincipleStatusPruned)
)

func set(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

// ValidLedgerType reports whether s is one of the LedgerType* constants.
func ValidLedgerType(s string) bool { return ledgerTypes[s] }

// ValidLedgerStatus reports whether s is one of the LedgerStatus* constants.
func ValidLedgerStatus(s string) bool { return ledgerStatuses[s] }

// ValidPrincipleType reports whether s is one of the PrincipleType* constants.
func ValidPrincipleType(s string) bool { return principleTypes[s] }

// ValidPrincipleStatus reports whether s is one of the PrincipleStatus*
// constants.
func ValidPrincipleStatus(s string) bool { return principleStatuses[s] }

// ErrSchema is wrapped by every type/status validation error.
var ErrSchema = errors.New("nous knowledge schema")

// LedgerEntry is one ledger row as served to the panel: the row's own
// fields plus the normalized KeyType / KeyStatus (and KeyOutcome).
type LedgerEntry = map[string]interface{}

// NormalizeLedgerEntry returns a copy of row carrying a valid KeyType and
// KeyStatus. Missing fields are derived: a Nous row whose family or
// candidate_id is "baseline" is LedgerTypeBaseline; a legacy row whose
// `type` is one of the dashboard's old state words (active, completed,
// aborted) becomes an experiment with that status; anything else gets the
// defaults. A Nous `h_main_result` is surfaced as a lowercase KeyOutcome.
// An explicit but unknown type or status is replaced by the default and
// reported through the returned error (wrapping ErrSchema); the entry is
// still usable.
func NormalizeLedgerEntry(row map[string]interface{}) (LedgerEntry, error) {
	out := make(LedgerEntry, len(row)+3)
	for k, v := range row {
		out[k] = v
	}
	typ := stringAt(row, KeyType)
	status := stringAt(row, KeyStatus)
	var errs []error

	if status == "" && ledgerStatuses[typ] {
		status, typ = typ, LedgerTypeExperiment
	}
	switch {
	case typ == "":
		if stringAt(row, keyFamily) == nousBaselineFamily || stringAt(row, keyCandidateID) == nousBaselineFamily {
			typ = LedgerTypeBaseline
		} else {
			typ = DefaultLedgerType
		}
	case !ledgerTypes[typ]:
		errs = append(errs, fmt.Errorf("%w: ledger %s %q unknown, using %q", ErrSchema, KeyType, typ, DefaultLedgerType))
		typ = DefaultLedgerType
	}
	switch {
	case status == "":
		status = DefaultLedgerStatus
	case !ledgerStatuses[status]:
		errs = append(errs, fmt.Errorf("%w: ledger %s %q unknown, using %q", ErrSchema, KeyStatus, status, DefaultLedgerStatus))
		status = DefaultLedgerStatus
	}
	if stringAt(row, KeyOutcome) == "" {
		if r := stringAt(row, keyHMainResult); r != "" {
			out[KeyOutcome] = strings.ToLower(r)
		}
	}
	out[KeyType] = typ
	out[KeyStatus] = status
	return out, errors.Join(errs...)
}

// Principle is one learned principle as served to the panel. Confidence is
// the numeric score the panel renders (see ConfidenceScore); Category is
// Nous's raw category string, kept for display provenance; Type is the
// validated schema type derived from it.
type Principle struct {
	ID         string  `json:"id"`
	Statement  string  `json:"statement"`
	Confidence float64 `json:"confidence"`
	Category   string  `json:"category,omitempty"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
}

// NormalizePrinciple maps a raw principle object onto Principle. Type is
// the explicit `type` when present, else the Nous `category` when it is one
// of the PrincipleType* constants, else DefaultPrincipleType. Status
// defaults to DefaultPrincipleStatus. As for ledger rows, an explicit but
// unknown value is replaced by the default and reported.
func NormalizePrinciple(raw map[string]interface{}) (Principle, error) {
	p := Principle{
		ID:         stringAt(raw, keyID),
		Statement:  stringAt(raw, keyStatement),
		Confidence: ConfidenceScore(raw[keyConfidence]),
		Category:   stringAt(raw, keyCategory),
	}
	var errs []error
	switch typ := stringAt(raw, KeyType); {
	case typ != "" && principleTypes[typ]:
		p.Type = typ
	case typ != "":
		errs = append(errs, fmt.Errorf("%w: principle %q %s %q unknown, using %q", ErrSchema, p.ID, KeyType, typ, DefaultPrincipleType))
		p.Type = DefaultPrincipleType
	case principleTypes[p.Category]:
		p.Type = p.Category
	default:
		p.Type = DefaultPrincipleType
	}
	switch status := stringAt(raw, KeyStatus); {
	case status == "":
		p.Status = DefaultPrincipleStatus
	case principleStatuses[status]:
		p.Status = status
	default:
		errs = append(errs, fmt.Errorf("%w: principle %q %s %q unknown, using %q", ErrSchema, p.ID, KeyStatus, status, DefaultPrincipleStatus))
		p.Status = DefaultPrincipleStatus
	}
	return p, errors.Join(errs...)
}

// ConfidenceScore turns a principle's confidence — Nous's low/medium/high
// enum, or a number in 0..1 from the governor loop — into the 0..1 score
// the panel renders. Anything else scores ConfidenceUnknown.
func ConfidenceScore(v interface{}) float64 {
	switch c := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "high":
			return ConfidenceHigh
		case "medium":
			return ConfidenceMedium
		case "low":
			return ConfidenceLow
		}
	case float64:
		if c >= 0 && c <= 1 {
			return c
		}
	}
	return ConfidenceUnknown
}

func stringAt(m map[string]interface{}, key string) string {
	s, _ := m[key].(string)
	return s
}

// Knowledge is the normalized content of the ledger and principles files.
type Knowledge struct {
	Ledger     []LedgerEntry
	Principles []Principle
	// Dir is the governor directory the files were read from (the durable
	// dir, or the legacy fallback).
	Dir string
	// Schema collects the per-entry type/status problems found while
	// normalizing; the files still loaded.
	Schema []error
}

// ParseLedger decodes a Nous ledger document ({"iterations": [...]}) and
// normalizes every row. Schema problems are returned alongside the entries,
// a malformed document as a plain error.
func ParseLedger(data []byte) ([]LedgerEntry, []error, error) {
	var doc struct {
		Iterations []map[string]interface{} `json:"iterations"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse ledger: %w", err)
	}
	entries := make([]LedgerEntry, 0, len(doc.Iterations))
	var schema []error
	for _, row := range doc.Iterations {
		e, err := NormalizeLedgerEntry(row)
		if err != nil {
			schema = append(schema, err)
		}
		entries = append(entries, e)
	}
	return entries, schema, nil
}

// ParsePrinciples decodes a Nous principles document ({"principles": [...]})
// and normalizes every principle, like ParseLedger.
func ParsePrinciples(data []byte) ([]Principle, []error, error) {
	var doc struct {
		Principles []map[string]interface{} `json:"principles"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse principles: %w", err)
	}
	principles := make([]Principle, 0, len(doc.Principles))
	var schema []error
	for _, raw := range doc.Principles {
		p, err := NormalizePrinciple(raw)
		if err != nil {
			schema = append(schema, err)
		}
		principles = append(principles, p)
	}
	return principles, schema, nil
}

// fileSig identifies a version of a file cheaply (one stat, no read).
type fileSig struct {
	exists  bool
	size    int64
	modTime time.Time
}

func statSig(path string) fileSig {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fileSig{}
	}
	return fileSig{exists: true, size: info.Size(), modTime: info.ModTime()}
}

// KnowledgeReader serves the ledger and principles live: every Load stats
// the two files and re-parses only the ones that changed since the last
// call, so the panel sees a new round as soon as the agent writes it
// without the hive re-reading the files on every poll. It is safe for
// concurrent use. Build one with Layout.KnowledgeReader.
type KnowledgeReader struct {
	layout    Layout
	legacyDir string

	mu         sync.Mutex
	dir        string
	ledgerSig  fileSig
	princSig   fileSig
	ledger     []LedgerEntry
	principles []Principle
	// ledgerSchema / princSchema are the schema problems of the cached
	// content of each file, kept apart so a reload of one file does not
	// drop or duplicate the other's.
	ledgerSchema []error
	princSchema  []error
	loaded       bool
}

// KnowledgeReader returns a live reader over l's governor directory. When
// the durable dir has neither file, legacyDir is read instead (see
// ResolveGovernorDir); pass "" to disable the fallback.
func (l Layout) KnowledgeReader(legacyDir string) *KnowledgeReader {
	return &KnowledgeReader{layout: l, legacyDir: legacyDir}
}

// Load returns the current knowledge and whether it differs from the
// previous Load (the first call always reports changed). A file that
// exists but cannot be parsed is an error; the last good content for that
// file is kept and still returned, so a half-written file never blanks the
// panel. A missing file is simply empty.
func (r *KnowledgeReader) Load() (Knowledge, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dir, _ := r.layout.ResolveGovernorDir(r.legacyDir)
	ledgerPath := filepath.Join(dir, LedgerFile)
	princPath := filepath.Join(dir, PrinciplesFile)
	ledgerSig, princSig := statSig(ledgerPath), statSig(princPath)

	changed := !r.loaded || dir != r.dir
	var errs []error
	if changed || ledgerSig != r.ledgerSig {
		entries, schema, err := readLedger(ledgerPath)
		if err != nil {
			errs = append(errs, err)
		} else {
			r.ledger, r.ledgerSig, r.ledgerSchema = entries, ledgerSig, schema
			changed = true
		}
	}
	if changed || princSig != r.princSig {
		principles, schema, err := readPrinciples(princPath)
		if err != nil {
			errs = append(errs, err)
		} else {
			r.principles, r.princSig, r.princSchema = principles, princSig, schema
			changed = true
		}
	}
	r.dir, r.loaded = dir, true

	k := Knowledge{
		Ledger:     copyEntries(r.ledger),
		Principles: append([]Principle(nil), r.principles...),
		Dir:        dir,
		Schema:     append(append([]error(nil), r.ledgerSchema...), r.princSchema...),
	}
	return k, changed, errors.Join(errs...)
}

// copyEntries clones the ledger so a caller editing an entry (the panel
// adds run links to the pending one) cannot corrupt the cache.
func copyEntries(in []LedgerEntry) []LedgerEntry {
	out := make([]LedgerEntry, len(in))
	for i, e := range in {
		c := make(LedgerEntry, len(e))
		for k, v := range e {
			c[k] = v
		}
		out[i] = c
	}
	return out
}

func readLedger(path string) ([]LedgerEntry, []error, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read ledger: %w", err)
	}
	entries, schema, err := ParseLedger(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, schema, nil
}

func readPrinciples(path string) ([]Principle, []error, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read principles: %w", err)
	}
	principles, schema, err := ParsePrinciples(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return principles, schema, nil
}
