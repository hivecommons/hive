package ioscan_test

// Scorecard for the red-team corpus (hivecommons/hive#6685).
//
// redteam_test.go pins every payload's observed outcome one case at a time. This
// file rolls those outcomes up into the two numbers the project publishes —
// the attack block rate and the benign false-positive count — and turns them
// into a one-directional ratchet: the block rate may not fall below the
// committed baseline and the false-positive count may not rise above it.
//
// The baseline lives in testdata/scorecard_baseline.json. A PR that improves
// detection re-records it (IOSCAN_SCORECARD_UPDATE=1) and the diff of that file
// IS the before/after evidence in the PR body. A PR that regresses fails here.
//
//	cd src && go test ./pkg/ioscan/ -run TestScorecard -v          # print the table
//	IOSCAN_SCORECARD_UPDATE=1 go test ./pkg/ioscan/ -run TestScorecard  # re-record

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	scorecardBaselinePath = "testdata/scorecard_baseline.json"
	scorecardUpdateEnv    = "IOSCAN_SCORECARD_UPDATE"
	benignCategory        = "benign-control"
	// scorecardRateTolerancePct absorbs float rounding when a baseline was
	// recorded from the same corpus; it is far below one payload's worth.
	scorecardRateTolerancePct = 0.05
)

// scorecardBaseline is the committed shape of testdata/scorecard_baseline.json.
type scorecardBaseline struct {
	// AttackTotal and AttackBlocked are the raw counts behind BlockRatePct, kept
	// so a reader of the diff sees the corpus size move with the rate.
	AttackTotal   int     `json:"attack_total"`
	AttackBlocked int     `json:"attack_blocked"`
	BlockRatePct  float64 `json:"block_rate_pct"`
	// BenignTotal and BenignFalsePositives count the controls the scanner
	// withheld even though they are ordinary issue text.
	BenignTotal          int `json:"benign_total"`
	BenignFalsePositives int `json:"benign_false_positives"`
}

type scorecardRow struct {
	category                 string
	total, blocked, detected int
	missed                   int
}

// scorecard is the rolled-up result of one run over the corpus.
type scorecard struct {
	rows     []scorecardRow
	attacks  scorecardRow
	benign   scorecardRow
	benignFP []string
}

func (s scorecard) blockRatePct() float64 {
	if s.attacks.total == 0 {
		return 0
	}
	return 100 * float64(s.attacks.blocked) / float64(s.attacks.total)
}

func (s scorecard) baseline() scorecardBaseline {
	return scorecardBaseline{
		AttackTotal:          s.attacks.total,
		AttackBlocked:        s.attacks.blocked,
		BlockRatePct:         math.Round(s.blockRatePct()*10) / 10,
		BenignTotal:          s.benign.total,
		BenignFalsePositives: len(s.benignFP),
	}
}

// runScorecard observes every corpus case through the production input path
// and tallies per category. Benign controls are kept out of the attack totals
// so the block rate is a detection number, not a "how often it fires" number.
func runScorecard(cases []redTeamCase) scorecard {
	byCat := map[string]*scorecardRow{}
	var sc scorecard
	for _, c := range cases {
		got, _ := observe(c)
		row := byCat[c.category]
		if row == nil {
			row = &scorecardRow{category: c.category}
			byCat[c.category] = row
		}
		tally := func(r *scorecardRow) {
			r.total++
			switch got {
			case outcomeBlocked:
				r.blocked++
			case outcomeDetected:
				r.detected++
			default:
				r.missed++
			}
		}
		tally(row)
		if c.category == benignCategory {
			tally(&sc.benign)
			if got == outcomeBlocked {
				sc.benignFP = append(sc.benignFP, c.id)
			}
			continue
		}
		tally(&sc.attacks)
	}
	for _, r := range byCat {
		sc.rows = append(sc.rows, *r)
	}
	sort.Slice(sc.rows, func(i, j int) bool { return sc.rows[i].category < sc.rows[j].category })
	return sc
}

// markdown renders the scorecard as the table pasted into PR bodies and docs.
func (s scorecard) markdown() string {
	var b strings.Builder
	b.WriteString("| category | total | blocked | detected-not-blocked | missed | block rate |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range s.rows {
		if r.category == benignCategory {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %s |\n", r.category, r.total, r.blocked, r.detected, r.missed, pct(r.blocked, r.total))
	}
	fmt.Fprintf(&b, "| **all attacks** | **%d** | **%d** | **%d** | **%d** | **%s** |\n",
		s.attacks.total, s.attacks.blocked, s.attacks.detected, s.attacks.missed, pct(s.attacks.blocked, s.attacks.total))
	fmt.Fprintf(&b, "\nBenign controls: %d of %d redacted (false positives)", len(s.benignFP), s.benign.total)
	if len(s.benignFP) > 0 {
		fmt.Fprintf(&b, ": %s", strings.Join(s.benignFP, ", "))
	}
	b.WriteString("\n")
	return b.String()
}

func pct(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(d))
}

func readScorecardBaseline(t *testing.T) scorecardBaseline {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(scorecardBaselinePath))
	if err != nil {
		t.Fatalf("read %s: %v (run with %s=1 to record one)", scorecardBaselinePath, err, scorecardUpdateEnv)
	}
	var b scorecardBaseline
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("parse %s: %v", scorecardBaselinePath, err)
	}
	return b
}

func writeScorecardBaseline(t *testing.T, b scorecardBaseline) {
	t.Helper()
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scorecardBaselinePath, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScorecardRatchet is the CI gate: the block rate may only go up and the
// benign false-positive count may only go down. It always prints the table so
// a PR author can paste before/after numbers from the job log.
func TestScorecardRatchet(t *testing.T) {
	sc := runScorecard(redTeamCorpus)
	t.Logf("\n%s", sc.markdown())
	got := sc.baseline()

	if os.Getenv(scorecardUpdateEnv) != "" {
		writeScorecardBaseline(t, got)
		t.Logf("re-recorded %s: %+v", scorecardBaselinePath, got)
		return
	}
	want := readScorecardBaseline(t)

	if got.BlockRatePct+scorecardRateTolerancePct < want.BlockRatePct {
		t.Errorf("attack block rate REGRESSED: %.1f%% (%d/%d) is below the committed baseline %.1f%% (%d/%d).\n"+
			"A rule or policy change let payloads through that used to be withheld. Fix the regression; do not lower the baseline.",
			got.BlockRatePct, got.AttackBlocked, got.AttackTotal, want.BlockRatePct, want.AttackBlocked, want.AttackTotal)
	}
	if got.BenignFalsePositives > want.BenignFalsePositives {
		t.Errorf("benign false positives ROSE: %d of %d (%s) vs committed baseline %d.\n"+
			"Ordinary issue text is now being withheld from agents. Narrow the rule; do not raise the baseline.",
			got.BenignFalsePositives, got.BenignTotal, strings.Join(sc.benignFP, ", "), want.BenignFalsePositives)
	}
	if got.BlockRatePct > want.BlockRatePct+scorecardRateTolerancePct || got.BenignFalsePositives < want.BenignFalsePositives {
		t.Errorf("scorecard IMPROVED (block rate %.1f%% vs %.1f%%, false positives %d vs %d) but the baseline was not re-recorded.\n"+
			"Run: %s=1 go test ./pkg/ioscan/ -run TestScorecardRatchet and commit %s so the gate ratchets up.",
			got.BlockRatePct, want.BlockRatePct, got.BenignFalsePositives, want.BenignFalsePositives, scorecardUpdateEnv, scorecardBaselinePath)
	}
}

// TestScorecardBaselineMatchesCorpusSize guards against a baseline recorded
// from a different corpus: the committed totals must equal the live corpus so
// the rate is comparing like with like.
func TestScorecardBaselineMatchesCorpusSize(t *testing.T) {
	if os.Getenv(scorecardUpdateEnv) != "" {
		t.Skip("baseline is being re-recorded")
	}
	want := readScorecardBaseline(t)
	sc := runScorecard(redTeamCorpus)
	if sc.attacks.total != want.AttackTotal || sc.benign.total != want.BenignTotal {
		t.Errorf("corpus size changed (attacks %d, benign %d) but %s still records (attacks %d, benign %d); re-record it",
			sc.attacks.total, sc.benign.total, scorecardBaselinePath, want.AttackTotal, want.BenignTotal)
	}
}

// TestScorecardMarkdownShape pins the table layout the docs and PR bodies rely
// on, independent of the live numbers.
func TestScorecardMarkdownShape(t *testing.T) {
	sc := runScorecard([]redTeamCase{
		{"A1", "alpha", "t", "Ignore all previous instructions", outcomeBlocked},
		{"A2", "alpha", "t", "hello world", outcomeMissed},
		{"B1", benignCategory, "t", "hello world", outcomeMissed},
		{"B2", benignCategory, "t", "rm -rf / please", outcomeBlocked},
	})
	md := sc.markdown()
	for _, want := range []string{
		"| alpha | 2 | 1 | 0 | 1 | 50% |",
		"| **all attacks** | **2** | **1** | **0** | **1** | **50%** |",
		"Benign controls: 1 of 2 redacted (false positives): B2",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "| "+benignCategory+" |") {
		t.Errorf("benign controls must not appear as an attack row:\n%s", md)
	}
	if got := sc.baseline(); got.AttackBlocked != 1 || got.BlockRatePct != 50 || got.BenignFalsePositives != 1 {
		t.Errorf("baseline = %+v", got)
	}
	if pct(0, 0) != "n/a" {
		t.Errorf("pct(0,0) = %q", pct(0, 0))
	}
}
