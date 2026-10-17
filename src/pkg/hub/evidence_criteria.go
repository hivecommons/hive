package hub

import "time"

// Per-axis criteria table of the evidence drill-down (#12463). It is derived
// hub-side from the same quadrant criteria the Fleet quadrant card scores
// (quadrant_axes.go), so the spoke sends nothing new and the table can never
// disagree with the ⓘ explainers on the card.

// Criterion statuses. A criterion the hive has no input for is
// "not_reported", never "unmet": absent evidence is not a failure.
const (
	EvidenceCriterionMet         = "met"
	EvidenceCriterionUnmet       = "unmet"
	EvidenceCriterionNotReported = "not_reported"
)

// evidenceCriterionMetScore is the 0–100 criterion score at or above which a
// measured criterion counts as met: at least halfway from the documented
// worst to the documented best value of its curve (quadrant_curve.go).
const evidenceCriterionMetScore = 50

// EvidenceCriterion is one criterion of one axis for one hive. Display and
// Score are set only when Status is met or unmet; Reason says why a criterion
// is not reported.
type EvidenceCriterion struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Display string `json:"display,omitempty"`
	Score   *int   `json:"score,omitempty"`
	Good    string `json:"good,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// EvidenceAxisCriteria is one quadrant axis of one hive with its criteria in
// the quadrant's stable order. Scored mirrors AxisScore.Scored; an unscored
// axis lists every criterion as not reported.
type EvidenceAxisCriteria struct {
	Axis     string              `json:"axis"`
	Label    string              `json:"label"`
	Scored   bool                `json:"scored"`
	Criteria []EvidenceCriterion `json:"criteria"`
}

// Reasons the whole hive is not reported in the criteria table.
const (
	evidenceCriteriaOffline    = "hive is offline"
	evidenceCriteriaNoEvidence = "hive has not reported an evidence summary yet"
	evidenceCriteriaUnscored   = "no quadrant axis has enough evidence yet"
)

// evidenceCriteriaFor derives one hive's per-axis criteria table, always in
// QuadrantAxisOrder. An offline hive, one that never sent an evidence
// summary, or one with no scored axis reports every criterion as not
// reported, so stale or missing data never reads as a failure. (Work source
// is always measured, so without the evidence check a hive that reported
// nothing else would still show an "unmet" criterion.)
func evidenceCriteriaFor(e RegistryEntry, now time.Time) []EvidenceAxisCriteria {
	in := quadrantInputsFor(e, now)
	axes := make([]AxisScore, 0, len(QuadrantAxisOrder))
	scored := 0
	for _, axis := range QuadrantAxisOrder {
		as := scoreAxis(axis, criteriaFor(axis, in))
		if as.Scored {
			scored++
		}
		axes = append(axes, as)
	}
	hiveReason := ""
	switch {
	case !e.Online:
		hiveReason = evidenceCriteriaOffline
	case e.Evidence == nil:
		hiveReason = evidenceCriteriaNoEvidence
	case scored == 0:
		hiveReason = evidenceCriteriaUnscored
	}
	out := make([]EvidenceAxisCriteria, 0, len(axes))
	for _, as := range axes {
		row := EvidenceAxisCriteria{Axis: as.Axis, Label: QuadrantAxisLabels[as.Axis],
			Scored: as.Scored && hiveReason == "", Criteria: make([]EvidenceCriterion, 0, len(as.Criteria))}
		for _, c := range as.Criteria {
			ec := EvidenceCriterion{Name: c.Name, Label: c.Label, Good: c.Good}
			switch {
			case hiveReason != "":
				ec.Status, ec.Reason = EvidenceCriterionNotReported, hiveReason
			case !as.Scored || !c.Measured:
				ec.Status, ec.Reason = EvidenceCriterionNotReported, c.Reason
				if ec.Reason == "" {
					ec.Reason = as.Reason
				}
			default:
				score := c.Score
				ec.Display, ec.Score = c.Display, &score
				ec.Status = EvidenceCriterionUnmet
				if score >= evidenceCriterionMetScore {
					ec.Status = EvidenceCriterionMet
				}
			}
			row.Criteria = append(row.Criteria, ec)
		}
		out = append(out, row)
	}
	return out
}
