package compliance

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PosturePoint is one check's status in one posture run.
type PosturePoint struct {
	At     time.Time     `json:"at"`
	Status PostureStatus `json:"status"`
}

// PostureSeries is one check's history over a set of runs: the points the
// Compliance tab draws as a sparkline, status counts, and the most recent
// result (hivecommons/hive#11081).
type PostureSeries struct {
	CheckID    string         `json:"check_id"`
	Title      string         `json:"title"`
	ControlIDs []string       `json:"control_ids"`
	Points     []PosturePoint `json:"points"`
	Summary    PostureSummary `json:"summary"`
	LastFailAt *time.Time     `json:"last_fail_at,omitempty"`
	Last       *Result        `json:"last,omitempty"`
}

// BuildPostureSeries groups runs (oldest first) into one series per check,
// in catalogue order followed by any check that is no longer catalogued.
func BuildPostureSeries(runs []PostureRun) []PostureSeries {
	idx := map[string]int{}
	out := []PostureSeries{}
	for _, c := range PostureCatalogue() {
		idx[c.ID] = len(out)
		out = append(out, PostureSeries{CheckID: c.ID, Title: c.Title, ControlIDs: c.ControlIDs, Points: []PosturePoint{}})
	}
	for _, run := range runs {
		for _, res := range run.Results {
			i, ok := idx[res.CheckID]
			if !ok {
				i = len(out)
				idx[res.CheckID] = i
				out = append(out, PostureSeries{CheckID: res.CheckID, Title: res.Title, ControlIDs: res.ControlIDs, Points: []PosturePoint{}})
			}
			s := &out[i]
			at := res.At
			if at.IsZero() {
				at = run.At
			}
			s.Points = append(s.Points, PosturePoint{At: at, Status: res.Status})
			switch res.Status {
			case PosturePass:
				s.Summary.Pass++
			case PostureFail:
				s.Summary.Fail++
				t := at
				s.LastFailAt = &t
			case PostureSkip:
				s.Summary.Skip++
			default:
				s.Summary.Error++
			}
			r := res
			s.Last = &r
		}
	}
	return out
}

// ReportMarkdown renders a control-mapping report as Markdown, stamped with
// the hive id, generation time and the profile version of each framework.
func ReportMarkdown(r Report, hiveID string, generated time.Time) string {
	var b strings.Builder
	b.WriteString("# Compliance control-mapping report\n\n")
	fmt.Fprintf(&b, "> %s\n\n", mdCell(r.Disclaimer))
	if hiveID == "" {
		hiveID = "(unset)"
	}
	fmt.Fprintf(&b, "- Hive: `%s`\n", mdCell(hiveID))
	fmt.Fprintf(&b, "- Generated: %s\n", generated.UTC().Format(time.RFC3339))
	versions := map[string]string{}
	for _, f := range r.Available {
		versions[f.ID] = f.Name + " " + f.Version
	}
	if len(r.Frameworks) == 0 {
		b.WriteString("- Frameworks: none selected\n")
	}
	for _, id := range r.Frameworks {
		fmt.Fprintf(&b, "- Framework: `%s` (%s)\n", id, mdCell(versions[id]))
	}
	s := r.Summary
	fmt.Fprintf(&b, "- Summary: %d meet, %d deviate, %d off, %d not covered by Hive\n\n", s.Meets, s.Deviates, s.Off, s.NotCovered)
	if len(r.Controls) == 0 {
		return b.String()
	}
	b.WriteString("| Framework | Control | Domain | Status | Setting | Current | Recommended |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, c := range r.Controls {
		control := mdCell(c.ControlID + " — " + c.Title)
		status := string(c.Status)
		if c.NotCovered {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | _not covered by Hive_ | — | — |\n", c.Framework, control, mdCell(c.Domain), mdCell(status))
			continue
		}
		if len(c.Settings) == 0 {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | — | — | — |\n", c.Framework, control, mdCell(c.Domain), mdCell(status))
			continue
		}
		for _, st := range c.Settings {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | `%s` | `%s` | `%s` |\n", c.Framework, control, mdCell(c.Domain), mdCell(status),
				mdCell(st.SettingPath), mdCell(st.Current), mdCell(st.Recommended))
		}
	}
	return b.String()
}

// csvSafe neutralises spreadsheet formula injection in a CSV cell.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func writeCSV(rows [][]string) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, c := range row {
			safe[i] = csvSafe(c)
		}
		_ = w.Write(safe)
	}
	w.Flush()
	return buf.Bytes()
}

// PostureHistoryCSV renders runs as CSV, one row per check result.
func PostureHistoryCSV(runs []PostureRun) []byte {
	rows := [][]string{{"run_at", "trigger", "check_id", "title", "status", "pass", "control_ids", "detail", "evidence_refs"}}
	for _, run := range runs {
		for _, res := range run.Results {
			rows = append(rows, []string{
				run.At.UTC().Format(time.RFC3339), run.Trigger, res.CheckID, res.Title, string(res.Status),
				strconv.FormatBool(res.Pass), strings.Join(res.ControlIDs, " "), res.Detail, strings.Join(res.EvidenceRefs, " "),
			})
		}
	}
	return writeCSV(rows)
}

// AttestationsCSV renders attestations as CSV.
func AttestationsCSV(items []Attestation) []byte {
	rows := [][]string{{"framework", "reviewed_on", "by", "at", "note"}}
	for _, a := range items {
		rows = append(rows, []string{a.Framework, a.ReviewedOn, a.By, a.At.UTC().Format(time.RFC3339), a.Note})
	}
	return writeCSV(rows)
}
