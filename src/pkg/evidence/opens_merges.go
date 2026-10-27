package evidence

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ---- Issue opens : PR merges ---------------------------------------------
//
// The Evidence card "Issue opens : PR merges" reads how much new work the hive
// files against how much it lands: issues the hive itself opened ÷ PRs the hive
// merged (or wrote), per repository and fleet-wide, over the selected window.
// The expected ratio depends on the governor's cadence mode, so the card
// shows deviation from the mode's band rather than from a flat 1.0:
//
//   - surge / busy (drain): merges should outpace opens, ratio ≤ 1;
//   - quiet / idle (scout): opens may exceed merges, but no more than 2:1 so an
//     idle hive does not manufacture backlog;
//   - unknown mode: parity within ±25%.
//
// Over a rolling 30 days the ratio is expected to regress to parity. Everything
// here is descriptive; nothing that decides a merge or a dispatch reads it.

// Cadence modes as the card names them (the governor's Mode, lower-cased).
const (
	CadenceSurge   = "surge"
	CadenceBusy    = "busy"
	CadenceQuiet   = "quiet"
	CadenceIdle    = "idle"
	CadenceUnknown = "unknown"
)

// Band thresholds in hive issue opens per hive PR merge.
const (
	// OpensMergesParity is the long-run target the rolling window regresses to.
	OpensMergesParity = 1.0
	// OpensMergesDrainMax caps the ratio in surge/busy: the queue is being
	// worked down, so merges should at least keep pace with opens.
	OpensMergesDrainMax = 1.0
	// OpensMergesScoutMin / OpensMergesScoutMax bound the ratio in quiet/idle:
	// scouting and backfill may file more than lands, up to twice as much.
	OpensMergesScoutMin = 1.0
	OpensMergesScoutMax = 2.0
	// OpensMergesParityTolerance is the ± band around parity used when the
	// mode is unknown and for the rolling 30-day regression check.
	OpensMergesParityTolerance = 0.25
	// OpensMergesRollingDays is the fixed trailing span of the sparkline.
	OpensMergesRollingDays = 30
)

// Band verdicts for a ratio against an expected band.
const (
	OpensMergesInBand = "in_band"
	OpensMergesAbove  = "above"   // more opens per merge than the band allows: backlog growing
	OpensMergesBelow  = "below"   // fewer opens per merge than the band expects: under-scouting
	OpensMergesNoData = "no_data" // neither opens nor merges
)

// OpensMergesBand is the ratio range a cadence mode expects, inclusive at
// both ends. Min is 0 when the band has no floor.
type OpensMergesBand struct {
	Mode  string  `json:"mode"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Label string  `json:"label"`
	Why   string  `json:"why"`
}

// ExpectedOpensMergesBand maps a governor mode (any case) to its band.
func ExpectedOpensMergesBand(mode string) OpensMergesBand {
	switch m := strings.ToLower(strings.TrimSpace(mode)); m {
	case CadenceSurge, CadenceBusy:
		return OpensMergesBand{Mode: m, Min: 0, Max: OpensMergesDrainMax, Label: fmt.Sprintf("≤ %s", ratioText(OpensMergesDrainMax)), Why: "drain: merges should outpace opens"}
	case CadenceQuiet, CadenceIdle:
		return OpensMergesBand{Mode: m, Min: OpensMergesScoutMin, Max: OpensMergesScoutMax, Label: fmt.Sprintf("%s–%s", ratioText(OpensMergesScoutMin), ratioText(OpensMergesScoutMax)), Why: "scout: opens may exceed merges, bounded"}
	}
	return OpensMergesBand{Mode: CadenceUnknown, Min: OpensMergesParity - OpensMergesParityTolerance, Max: OpensMergesParity + OpensMergesParityTolerance, Label: fmt.Sprintf("%s–%s", ratioText(OpensMergesParity-OpensMergesParityTolerance), ratioText(OpensMergesParity+OpensMergesParityTolerance)), Why: "mode unknown: parity"}
}

// parityBand is the rolling 30-day expectation regardless of mode.
func parityBand() OpensMergesBand {
	b := ExpectedOpensMergesBand(CadenceUnknown)
	b.Why = "rolling 30 d should regress to parity"
	return b
}

// Verdict places a ratio against the band. A nil ratio (no merges) with opens
// is above every band; with no opens either it is no data.
func (b OpensMergesBand) Verdict(opens, merges int) string {
	switch {
	case opens == 0 && merges == 0:
		return OpensMergesNoData
	case merges == 0:
		return OpensMergesAbove
	}
	r := float64(opens) / float64(merges)
	switch {
	case r > b.Max:
		return OpensMergesAbove
	case r < b.Min:
		return OpensMergesBelow
	}
	return OpensMergesInBand
}

// OpensMergesEvent is one hive issue open (Merge false) or hive PR merge
// (Merge true) in a repository.
type OpensMergesEvent struct {
	Repo  string
	At    time.Time
	Merge bool
}

// OpensMergesInput is everything BuildOpensMerges reads. InWindow is the
// selected window's membership test; Now anchors the rolling 30 days.
type OpensMergesInput struct {
	Events   []OpensMergesEvent
	Mode     string
	InWindow func(time.Time) bool
	Now      time.Time
}

// OpensMergesRow is one repository's (or the fleet's) counts. Ratio is nil
// when there are no merges.
type OpensMergesRow struct {
	Repo   string   `json:"repo,omitempty"`
	Opens  int      `json:"opens"`
	Merges int      `json:"merges"`
	Ratio  *float64 `json:"ratio"`
}

// OpensMergesDay is one UTC day of the rolling series.
type OpensMergesDay struct {
	Day    string `json:"day"`
	Opens  int    `json:"opens"`
	Merges int    `json:"merges"`
}

// OpensMerges is the card's API shape (`issue_opens_pr_merges`).
type OpensMerges struct {
	Mode     string          `json:"mode"`
	Expected OpensMergesBand `json:"expected"`
	// Status is the fleet ratio's verdict against Expected; Headline is the
	// one-line reading of it ("0.8 opens per merge · surge mode expects ≤ 1 ✓").
	Status   string           `json:"status"`
	Headline string           `json:"headline"`
	N        int              `json:"n"`
	Total    OpensMergesRow   `json:"total"`
	Repos    []OpensMergesRow `json:"repos"`
	// Rolling30d is the trailing OpensMergesRollingDays regardless of the
	// selected window, judged against parity, with the per-day series behind it.
	Rolling30d       OpensMergesRow   `json:"rolling_30d"`
	Rolling30dBand   OpensMergesBand  `json:"rolling_30d_expected"`
	Rolling30dStatus string           `json:"rolling_30d_status"`
	Rolling30dDays   []OpensMergesDay `json:"rolling_30d_days"`
}

func ratioText(r float64) string {
	if math.IsInf(r, 1) {
		return "∞"
	}
	s := fmt.Sprintf("%.2f", r)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

func rowRatio(row *OpensMergesRow) {
	if row.Merges > 0 {
		r := float64(row.Opens) / float64(row.Merges)
		row.Ratio = &r
	}
}

// BuildOpensMerges aggregates the events into per-repo rows, the fleet total
// and the rolling 30-day series, and judges each against its band.
func BuildOpensMerges(in OpensMergesInput) OpensMerges {
	now := in.Now.UTC()
	inWindow := in.InWindow
	if inWindow == nil {
		inWindow = func(time.Time) bool { return true }
	}
	band := ExpectedOpensMergesBand(in.Mode)
	out := OpensMerges{Mode: band.Mode, Expected: band, Rolling30dBand: parityBand()}

	byRepo := map[string]*OpensMergesRow{}
	rollingStart := now.Truncate(24*time.Hour).AddDate(0, 0, -(OpensMergesRollingDays - 1))
	days := make([]OpensMergesDay, OpensMergesRollingDays)
	for i := range days {
		days[i].Day = rollingStart.AddDate(0, 0, i).Format("2006-01-02")
	}
	for _, ev := range in.Events {
		repo := strings.ToLower(strings.TrimSpace(ev.Repo))
		if repo == "" || ev.At.IsZero() {
			continue
		}
		if inWindow(ev.At) {
			row := byRepo[repo]
			if row == nil {
				row = &OpensMergesRow{Repo: repo}
				byRepo[repo] = row
			}
			if ev.Merge {
				row.Merges++
				out.Total.Merges++
			} else {
				row.Opens++
				out.Total.Opens++
			}
		}
		at := ev.At.UTC()
		if !at.Before(rollingStart) && !at.After(now) {
			i := int(at.Sub(rollingStart).Hours() / 24)
			if i >= 0 && i < len(days) {
				if ev.Merge {
					days[i].Merges++
					out.Rolling30d.Merges++
				} else {
					days[i].Opens++
					out.Rolling30d.Opens++
				}
			}
		}
	}
	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	out.Repos = make([]OpensMergesRow, 0, len(repos))
	for _, r := range repos {
		row := byRepo[r]
		rowRatio(row)
		out.Repos = append(out.Repos, *row)
	}
	rowRatio(&out.Total)
	rowRatio(&out.Rolling30d)
	out.N = out.Total.Opens + out.Total.Merges
	out.Status = band.Verdict(out.Total.Opens, out.Total.Merges)
	out.Headline = opensMergesHeadline(out.Total, band, out.Status)
	out.Rolling30dDays = days
	out.Rolling30dStatus = out.Rolling30dBand.Verdict(out.Rolling30d.Opens, out.Rolling30d.Merges)
	return out
}

// opensMergesHeadline renders the deviation-vs-expectation line.
func opensMergesHeadline(total OpensMergesRow, band OpensMergesBand, status string) string {
	if status == OpensMergesNoData {
		return fmt.Sprintf("no hive issue opens or PR merges · %s mode expects %s", band.Mode, band.Label)
	}
	ratio := math.Inf(1)
	if total.Ratio != nil {
		ratio = *total.Ratio
	}
	mark := "✓"
	switch status {
	case OpensMergesAbove:
		mark = "⚠ (backlog growing)"
	case OpensMergesBelow:
		mark = "⚠ (scouting below cadence)"
	}
	return fmt.Sprintf("%s opens per merge · %s mode expects %s %s", ratioText(ratio), band.Mode, band.Label, mark)
}

// opensMergesDeviation is the relative distance of the ratio outside the
// band (0 inside; 1 when there are opens but no merges).
func opensMergesDeviation(row OpensMergesRow, band OpensMergesBand) float64 {
	if row.Merges == 0 {
		if row.Opens == 0 {
			return 0
		}
		return 1
	}
	r := float64(row.Opens) / float64(row.Merges)
	var d float64
	switch {
	case r > band.Max && band.Max > 0:
		d = (r - band.Max) / band.Max
	case r < band.Min && band.Min > 0:
		d = (band.Min - r) / band.Min
	}
	// Round so 11:10 in drain mode is exactly 10% out, not 10.000000000000009%.
	return math.Min(1, math.Round(d*opensMergesDeviationScale)/opensMergesDeviationScale)
}

// opensMergesDeviationScale rounds deviations to four decimals before scoring.
const opensMergesDeviationScale = 1e4

var opensMergesRubric = fmt.Sprintf("hive issue opens ÷ hive PR merges against the cadence band (surge/busy ≤ %s, quiet/idle %s–%s, unknown %s–%s): score = 1 − relative distance outside the band, letter from the score table (inside A+, 10%% outside A-, 20%% B-, 30%% or no merges C-); needs %d opens + merges",
	ratioText(OpensMergesDrainMax), ratioText(OpensMergesScoutMin), ratioText(OpensMergesScoutMax), ratioText(OpensMergesParity-OpensMergesParityTolerance), ratioText(OpensMergesParity+OpensMergesParityTolerance), GradeMinN)

// GradeOpensMerges grades the fleet ratio by how far it sits outside the
// mode's band, not by its distance from 1.0.
func GradeOpensMerges(s OpensMerges) Grade {
	if s.N < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d hive issue opens + PR merges in the window (have %d)", GradeMinN, s.N), opensMergesRubric)
	}
	letter := letterFromScore(1 - opensMergesDeviation(s.Total, s.Expected))
	return graded(letter, s.Headline, opensMergesRubric)
}

// SummarizeOpensMerges is the plain sentence under the card header.
func SummarizeOpensMerges(s OpensMerges) string {
	if s.Status == OpensMergesNoData || s.Total.Opens+s.Total.Merges == 0 {
		return "No hive issue opens or PR merges in this window."
	}
	ratio := "∞"
	if s.Total.Ratio != nil {
		ratio = ratioText(*s.Total.Ratio)
	}
	verdict := "inside"
	switch s.Status {
	case OpensMergesAbove:
		verdict = "above"
	case OpensMergesBelow:
		verdict = "below"
	}
	return fmt.Sprintf("%d opened for %d merged (%s per merge), %s the %s band %s.", s.Total.Opens, s.Total.Merges, ratio, verdict, s.Expected.Mode, s.Expected.Label)
}
