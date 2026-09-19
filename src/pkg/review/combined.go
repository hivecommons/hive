package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ValidateReports is ValidateReport for a reviewer that judged a PR from every
// perspective in one sitting.
//
// The one-perspective-per-verdict shape was not a schema decision, it was a
// dispatch decision that leaked into the schema: one kick asked for one
// perspective, so one file carried one report. Running all five in a single
// session -- which is what lets the hive leave ONE comment covering all five
// instead of five comments -- needs a verdict payload that can carry five.
//
// A bare object is still accepted, and is still exactly what it was. Every
// reviewer already deployed emits one, the relay cannot tell which kick a
// verdict answers, and a schema change that invalidated the old shape would
// discard live reviews mid-rollout for no gain.
//
// Each element is validated by ValidateReport itself, so a combined verdict can
// never assert anything a single one could not.
func ValidateReports(raw []byte) ([]PerspectiveReport, error) {
	return ValidateReportsFor(raw, PerspectiveSet{})
}

// ValidateReportsFor is ValidateReports against the perspectives this hive
// actually reviews with.
func ValidateReportsFor(raw []byte, set PerspectiveSet) ([]PerspectiveReport, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("review report must be a non-empty JSON object or array")
	}
	if trimmed[0] != '[' {
		report, err := ValidateReportFor(trimmed, set)
		if err != nil {
			return nil, err
		}
		return []PerspectiveReport{*report}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var elems []json.RawMessage
	if err := dec.Decode(&elems); err != nil {
		return nil, fmt.Errorf("decode review reports: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("review report must contain exactly one JSON array")
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("review report array must contain at least one report")
	}

	reports := make([]PerspectiveReport, 0, len(elems))
	seen := make(map[Perspective]bool, len(elems))
	for i, elem := range elems {
		report, err := ValidateReportFor(elem, set)
		if err != nil {
			// Name the offending element: with five reports in one payload,
			// "verdict must be one of ..." on its own is not enough to act on.
			return nil, fmt.Errorf("report %d: %w", i, err)
		}
		// One perspective, one verdict. Two reports for the same perspective
		// are not a richer review, they are a contradiction the aggregate would
		// silently resolve by map-overwrite -- last write wins, and which one
		// that is depends on array order.
		if seen[report.Perspective] {
			return nil, fmt.Errorf("report %d: perspective %q appears more than once", i, report.Perspective)
		}
		seen[report.Perspective] = true
		// Every report in one payload must judge one PR. Without this a
		// reviewer authorized to comment on PR 7 could attach a binding
		// requires_human to some other PR by appending it to the array -- the
		// per-report check downstream only compares the first one.
		if err := sameSubject(reports, *report, i); err != nil {
			return nil, err
		}
		reports = append(reports, *report)
	}
	return reports, nil
}

func sameSubject(prior []PerspectiveReport, report PerspectiveReport, i int) error {
	if len(prior) == 0 {
		return nil
	}
	first := prior[0]
	if !strings.EqualFold(strings.TrimSpace(first.Repo), strings.TrimSpace(report.Repo)) || first.Number != report.Number {
		return fmt.Errorf("report %d: every report must judge the same PR (%s#%d), got %s#%d",
			i, first.Repo, first.Number, report.Repo, report.Number)
	}
	// A blank head SHA is tolerated (it always has been), but two different
	// non-blank ones mean the reports describe different revisions, and the
	// aggregate keeps only the first -- so the rest would be recorded against
	// code they did not read.
	a, b := strings.TrimSpace(first.HeadSHA), strings.TrimSpace(report.HeadSHA)
	if a != "" && b != "" && !strings.EqualFold(a, b) {
		return fmt.Errorf("report %d: every report must judge the same head SHA (%s), got %s", i, a, b)
	}
	return nil
}
