package persona

import (
	"fmt"
	"strings"
)

const (
	DepthOutcomes  = "outcomes"
	DepthTechnical = "technical"

	SummaryShort    = "short"
	SummaryStandard = "standard"
	SummaryDetailed = "detailed"
)

// Record is the per-user communication persona. It intentionally contains only
// presentation preferences; autonomy and agent mode settings live elsewhere.
type Record struct {
	Depth         string `json:"depth,omitempty"`
	SummaryLength string `json:"summary_length,omitempty"`
	Notes         string `json:"notes,omitempty"`
}

func (r Record) Normalize() Record {
	r.Depth = NormalizeDepth(r.Depth)
	r.SummaryLength = NormalizeSummaryLength(r.SummaryLength)
	r.Notes = strings.TrimSpace(r.Notes)
	return r
}

func NormalizeDepth(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case DepthTechnical, "tech", "technical-details", "details":
		return DepthTechnical
	default:
		return DepthOutcomes
	}
}

func NormalizeSummaryLength(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case SummaryShort, "brief":
		return SummaryShort
	case SummaryDetailed, "long", "full":
		return SummaryDetailed
	default:
		return SummaryStandard
	}
}

func (r Record) Set(key, value string) (Record, error) {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "depth", "persona.depth":
		r.Depth = NormalizeDepth(value)
	case "summary_length", "summary-length", "summary", "persona.summary_length":
		r.SummaryLength = NormalizeSummaryLength(value)
	case "notes", "note", "persona.notes":
		r.Notes = strings.TrimSpace(value)
	default:
		return r, fmt.Errorf("unknown persona key %q", key)
	}
	return r.Normalize(), nil
}

func (r Record) Empty() bool {
	return strings.TrimSpace(r.Depth) == "" && strings.TrimSpace(r.SummaryLength) == "" && strings.TrimSpace(r.Notes) == ""
}
