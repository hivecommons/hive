package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
)

// Oct 23 hub dashboard: "Fleet quadrant · all 101 hives · median of 101"
// showed PROD 0 while its own line said the median hive merged 0.5
// merges/repo/week across 47 hives. 47 hives were assigned and online and
// reporting repo activity; the other 54 were unassigned placeholders or
// offline. Every one of the 54 still scored Productivity 0, because the
// work-source criterion was always "measured" (a configuration fact), so
// 54 zeros out of 101 made the median 0.
func fleetOf101(now time.Time) []MyHiveEntry {
	rows := make([]MyHiveEntry, 0, 101)
	for i := 0; i < 47; i++ {
		rows = append(rows, MyHiveEntry{RegistryEntry: RegistryEntry{
			ID: "active", Online: true, ProvStatus: statusAssigned, ACMMLevel: 2,
			// 1 merge per repo in a 14-day window = 0.5 merges/repo/week.
			RepoActivity:            []RepoActivityWire{{Repo: "o/r", Merges: ActivityStatWire{Count: 1}}},
			RepoActivityWindowHours: 14 * 24,
		}})
	}
	for i := 0; i < 44; i++ {
		rows = append(rows, MyHiveEntry{RegistryEntry: RegistryEntry{
			ID: "placeholder", Org: placeholderOrgPrefix + "x", ProvStatus: statusAvailable, Online: true, ACMMLevel: 1,
		}})
	}
	for i := 0; i < 10; i++ {
		rows = append(rows, MyHiveEntry{RegistryEntry: RegistryEntry{
			ID: "offline", Online: false, ProvStatus: statusAssigned, ACMMLevel: 1,
			LastHeartbeat: now.Add(-48 * time.Hour).Format(time.RFC3339),
		}})
	}
	return rows
}

func TestFleetQuadrantPopulationIsAssignedAndActive(t *testing.T) {
	now := time.Now()
	rows := fleetOf101(now)
	fleet := attachQuadrants(rows, true, now)

	if fleet.N != 47 || fleet.Excluded != 54 {
		t.Fatalf("fleet N/Excluded = %d/%d, want 47 active and 54 excluded (44 placeholders + 10 offline)", fleet.N, fleet.Excluded)
	}
	prod, _ := axisOf(fleet, AxisProductivity)
	if !prod.Scored || prod.N != 47 {
		t.Fatalf("productivity = %+v, want scored over the 47 active hives", prod)
	}
	// 0.5 merges/repo/week on the log 0.1→10 curve is 35 (×3); the hives work
	// human-filed issues (work source 0, ×1): (35·3 + 0·1) / 4 = 26. Not 0.
	if prod.Score != 26 {
		t.Errorf("productivity median = %d, want 26 for 0.5 merges/repo/week", prod.Score)
	}
	th := criterionOf(t, prod, critThroughput)
	if !th.Measured || th.N != 47 || th.Display != "0.5 merges/repo/week" {
		t.Errorf("throughput criterion = %+v", th)
	}
	if !strings.Contains(prod.Summary, "median over 47 of 47 hives") {
		t.Errorf("summary should name the population: %q", prod.Summary)
	}
	// Trust is fed by ACMM level only here (L2 → 20); the offline L1 hives
	// and placeholders must not pull it down.
	tr, _ := axisOf(fleet, AxisTrust)
	if !tr.Scored || tr.N != 47 || tr.Score != 20 {
		t.Errorf("trust = %+v, want median 20 over 47 active hives", tr)
	}
}

// Before the population filter the same rows produced the reported card.
// This pins the diagnosis: run the median over all 101 and PROD is 0 with
// n=101 even though only 47 hives report any throughput.
func TestFleetQuadrantUnfilteredReproducesZeroProductivity(t *testing.T) {
	now := time.Now()
	rows := fleetOf101(now)
	inputs := make([]quadrantInputs, len(rows))
	for i := range rows {
		inputs[i] = quadrantInputsFor(rows[i].RegistryEntry, now)
	}
	all := FleetMedian(ScoreFleet(inputs))
	prod, _ := axisOf(all, AxisProductivity)
	// With work source no longer scored for hives that report no activity,
	// even the unfiltered median is honest: 47 hives score the axis, not 101.
	if prod.N != 47 || prod.Score != 26 {
		t.Errorf("unfiltered productivity = n=%d score=%d; a non-reporting hive must not enter the axis as a zero", prod.N, prod.Score)
	}
}

// Work source is a setting, not throughput: alone it must not score the axis.
func TestWorkSourceAloneDoesNotScoreProductivity(t *testing.T) {
	a := scoreAxis(AxisProductivity, productivityCriteria(quadrantInputs{workSource: "issues"}))
	if a.Scored {
		t.Fatalf("a hive reporting nothing but its work source scored Productivity %d", a.Score)
	}
	ws := criterionOf(t, a, critWorkSource)
	if ws.Measured || ws.Reason != reasonNoActivityForSource {
		t.Errorf("work source = %+v, want unmeasured with reason", ws)
	}
	b := scoreAxis(AxisProductivity, productivityCriteria(quadrantInputs{workSource: "issues", prsMerged90d: iptr(0)}))
	if c := criterionOf(t, b, critWorkSource); !c.Measured {
		t.Errorf("once PR counts are reported the work source is a real answer, got %+v", c)
	}
}

// A zero fleet axis must say what produced it, not just "0".
func TestFleetSummaryNamesWeakestCriterion(t *testing.T) {
	in := fleetOf(5, func(i int, in *quadrantInputs) {
		// 7-day hive state with every sample in SLA breach: SLA kept 0%.
		e := evidenceWith(0, 0, 0, 0, 0)
		e.HiveState = &hivestate.Trends{Last7d: hivestate.Window{Samples: 24, SLAViolationRate: 1.0}}
		in.evidence = e
		in.acmmLevel = 1
	})
	fleet := FleetMedian(ScoreFleet(in))
	sat, _ := axisOf(fleet, AxisSatisfaction)
	if !sat.Scored || sat.Score != 0 {
		t.Fatalf("satisfaction = %+v, want scored 0", sat)
	}
	if !strings.Contains(sat.Summary, "sla kept (7d) 0.0% (5 hives)") {
		t.Errorf("summary must cite the fact behind the zero: %q", sat.Summary)
	}
	if !strings.Contains(sat.Summary, "not measured by any hive yet") {
		t.Errorf("summary must say how much of the axis is unmeasured: %q", sat.Summary)
	}
}

// Deltas in the rows are re-based on the fleet median actually drawn, so
// "+15" means 15 above the active-hive median, not the all-rows one.
func TestAttachQuadrantsAlignsDeltasWithActivePopulation(t *testing.T) {
	now := time.Now()
	rows := fleetOf101(now)
	rows = append(rows, MyHiveEntry{Role: "owner", RegistryEntry: RegistryEntry{
		ID: "strong", Online: true, ProvStatus: statusAssigned, ACMMLevel: 5}})
	fleet := attachQuadrants(rows, true, now)
	tr, _ := axisOf(fleet, AxisTrust)
	q := rows[len(rows)-1].Quadrant
	if q == nil {
		t.Fatal("the strong hive should carry a quadrant")
	}
	mine, _ := axisOf(*q, AxisTrust)
	if mine.Delta != mine.Score-tr.Score {
		t.Errorf("delta = %d, want %d (score %d − fleet median %d)", mine.Delta, mine.Score-tr.Score, mine.Score, tr.Score)
	}
}
