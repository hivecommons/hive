package autoscale

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

var t0 = time.Date(2026, 10, 27, 19, 0, 0, 0, time.UTC)

func testCfg() config.ReviewerAutoscaleConfig {
	return config.ReviewerAutoscaleConfig{
		Enabled:                  true,
		Min:                      1,
		Max:                      3,
		TargetBacklogPerReviewer: 2,
		ScaleUpCycles:            2,
		ScaleDownCycles:          3,
		Cooldown:                 "5m",
	}
}

func reviewers(n int) []Reviewer {
	out := make([]Reviewer, 0, n)
	for i := 1; i <= n; i++ {
		name := "reviewer"
		if i > 1 {
			name = "reviewer-" + string(rune('0'+i))
		}
		out = append(out, Reviewer{Name: name, Index: i})
	}
	return out
}

func keys(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "o/r#"+string(rune('a'+i))+"@sha")
	}
	return out
}

func obsAt(step int, rs []Reviewer, backlog int) Observation {
	return Observation{
		Now:                 t0.Add(time.Duration(step) * time.Minute),
		Reviewers:           rs,
		WaitingKeys:         keys(backlog),
		InFlight:            0,
		ProviderHeadroomPct: -1,
	}
}

// drive feeds n identical cycles and returns the last decision.
func drive(c *Controller, from, n int, rs []Reviewer, backlog int, mutate func(*Observation)) Decision {
	var d Decision
	for i := 0; i < n; i++ {
		o := obsAt(from+i, rs, backlog)
		if mutate != nil {
			mutate(&o)
		}
		d = c.Observe(o)
	}
	return d
}

func TestScaleUpDecisionTable(t *testing.T) {
	cases := []struct {
		name      string
		reviewers []Reviewer
		backlog   int
		cycles    int
		mutate    func(*Observation)
		want      Action
		reason    string
	}{
		{name: "sustained backlog scales up", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionScaleUp},
		{name: "single behind cycle holds", reviewers: reviewers(2), backlog: 5, cycles: 1, want: ActionHold},
		{name: "backlog within target holds", reviewers: reviewers(2), backlog: 4, cycles: 3, want: ActionHold},
		{name: "never beyond max", reviewers: reviewers(3), backlog: 9, cycles: 2, want: ActionBlocked, reason: GuardAtMax},
		{name: "cpu guard blocks", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionBlocked, reason: GuardCPU,
			mutate: func(o *Observation) { o.Resources = Resources{Known: true, CPULimit: 8, CPUUsed: 7.5} }},
		{name: "memory guard blocks", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionBlocked, reason: GuardMemory,
			mutate: func(o *Observation) {
				o.Resources = Resources{Known: true, CPULimit: 8, MemLimitSet: true, MemLimitMB: 4096, MemUsedMB: 3000}
			}},
		{name: "unknown resources do not block", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionScaleUp,
			mutate: func(o *Observation) { o.Resources = Resources{} }},
		{name: "saturated kicks block", reviewers: []Reviewer{{Name: "reviewer", Index: 1, Saturated: true}, {Name: "reviewer-2", Index: 2}},
			backlog: 5, cycles: 2, want: ActionBlocked, reason: GuardSaturated},
		{name: "provider budget latch blocks", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionBlocked, reason: GuardProviderBudget,
			mutate: func(o *Observation) { o.ProviderBudgetExceeded = true }},
		{name: "low provider headroom blocks", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionBlocked, reason: GuardProviderHeadroom,
			mutate: func(o *Observation) { o.ProviderHeadroomPct = 5 }},
		{name: "max parallel reviews blocks", reviewers: reviewers(2), backlog: 5, cycles: 2, want: ActionBlocked, reason: GuardMaxParallelReviews,
			mutate: func(o *Observation) { o.MaxParallelReviews = 2 }},
		{name: "old waiting PR scales up below ratio", reviewers: reviewers(2), backlog: 1, cycles: 13, want: ActionScaleUp,
			mutate: func(o *Observation) { o.DwellThreshold = 10 * time.Minute }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := New(testCfg(), "reviewer")
			d := drive(c, 0, tc.cycles, tc.reviewers, tc.backlog, tc.mutate)
			if d.Action != tc.want {
				t.Fatalf("action = %s (%s), want %s", d.Action, d.Reason, tc.want)
			}
			if tc.reason != "" && d.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", d.Reason, tc.reason)
			}
			if tc.want == ActionScaleUp && d.To != len(tc.reviewers)+1 {
				t.Fatalf("to = %d, want %d", d.To, len(tc.reviewers)+1)
			}
		})
	}
}

func TestCooldownBlocksSecondScaleUp(t *testing.T) {
	c := New(testCfg(), "reviewer")
	if d := drive(c, 0, 2, reviewers(1), 5, nil); d.Action != ActionScaleUp {
		t.Fatalf("first: %s", d.Action)
	}
	// Two more behind cycles one minute later: wanted, but inside cooldown.
	if d := drive(c, 2, 2, reviewers(2), 9, nil); d.Action != ActionBlocked || d.Reason != GuardCooldown {
		t.Fatalf("second: %s %s", d.Action, d.Reason)
	}
	// After the cooldown the streak is still live, so it scales.
	if d := drive(c, 10, 1, reviewers(2), 9, nil); d.Action != ActionScaleUp {
		t.Fatalf("third: %s %s", d.Action, d.Reason)
	}
}

func TestScaleDownDrainsThenRetiresOnlyWhenIdle(t *testing.T) {
	c := New(testCfg(), "reviewer")
	rs := reviewers(3)
	// Idle for fewer cycles than required: hold.
	if d := drive(c, 10, 2, rs, 0, nil); d.Action != ActionHold {
		t.Fatalf("early: %s", d.Action)
	}
	d := drive(c, 12, 1, rs, 0, nil)
	if d.Action != ActionDrain || d.Agent != "reviewer-3" || d.To != 2 {
		t.Fatalf("drain: %+v", d)
	}
	if !c.Draining("reviewer-3") {
		t.Fatal("reviewer-3 should be draining")
	}
	// Mid-turn: never retired.
	busy := []Reviewer{rs[0], rs[1], {Name: "reviewer-3", Index: 3, Busy: true}}
	for i := 0; i < 4; i++ {
		if d := drive(c, 13+i, 1, busy, 0, nil); d.Action != ActionHold {
			t.Fatalf("busy cycle %d: %s", i, d.Action)
		}
	}
	// Turn ended: retire.
	d = drive(c, 20, 1, rs, 0, nil)
	if d.Action != ActionRetire || d.Agent != "reviewer-3" || d.To != 2 {
		t.Fatalf("retire: %+v", d)
	}
	if c.Draining("reviewer-3") {
		t.Fatal("retired reviewer still draining")
	}
}

func TestNeverDrainsBelowMin(t *testing.T) {
	cfg := testCfg()
	cfg.Min = 2
	c := New(cfg, "reviewer")
	if d := drive(c, 0, 10, reviewers(2), 0, nil); d.Action != ActionHold {
		t.Fatalf("got %s", d.Action)
	}
	if st := c.Status(); st.Target != 2 || st.Current != 2 {
		t.Fatalf("status %+v", st)
	}
}

func TestInFlightWorkIsNotIdle(t *testing.T) {
	c := New(testCfg(), "reviewer")
	d := drive(c, 0, 10, reviewers(2), 0, func(o *Observation) { o.InFlight = 2 })
	if d.Action != ActionHold {
		t.Fatalf("got %s", d.Action)
	}
}

func TestDemandDuringDrainUndrains(t *testing.T) {
	c := New(testCfg(), "reviewer")
	rs := reviewers(2)
	if d := drive(c, 0, 3, rs, 0, nil); d.Action != ActionDrain {
		t.Fatalf("drain: %s", d.Action)
	}
	busy := []Reviewer{rs[0], {Name: "reviewer-2", Index: 2, Busy: true}}
	d := drive(c, 3, 2, busy, 5, nil)
	if d.Action != ActionUndrain || d.Agent != "reviewer-2" {
		t.Fatalf("undrain: %+v", d)
	}
	if c.Draining("reviewer-2") {
		t.Fatal("still draining")
	}
}

func TestOldestWaitAgesAcrossCycles(t *testing.T) {
	c := New(testCfg(), "reviewer")
	drive(c, 0, 5, reviewers(2), 1, nil)
	st := c.Status()
	if st.Backlog != 1 || st.OldestWait != "4m0s" {
		t.Fatalf("status %+v", st)
	}
	// Key leaves the backlog: age resets.
	drive(c, 5, 1, reviewers(2), 0, nil)
	if st := c.Status(); st.OldestWait != "" || st.Backlog != 0 {
		t.Fatalf("status after clear %+v", st)
	}
}

func TestDecisionString(t *testing.T) {
	d := Decision{Action: ActionScaleUp, From: 2, To: 3, Reason: "backlog=7 oldest=41m0s"}
	if got := d.String(); got != "reviewer autoscale: 2→3 reason=backlog=7 oldest=41m0s" {
		t.Fatalf("got %q", got)
	}
}

func TestProberReadsCgroupV2(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(cpuMaxFile, "800000 100000\n")
	write(cpuStatFile, "usage_usec 1000000\nuser_usec 1\n")
	write(memMaxFile, "4294967296\n")
	write(memCurrentFile, "1073741824\n")
	p := NewProberAt(dir)
	r := p.Probe(t0)
	if !r.Known || r.CPULimit != 8 || r.MemLimitMB != 4096 || r.MemUsedMB != 1024 || r.CPUUsed != 0 {
		t.Fatalf("first %+v", r)
	}
	write(cpuStatFile, "usage_usec 61000000\n")
	r = p.Probe(t0.Add(time.Minute))
	if r.CPUUsed != 1 {
		t.Fatalf("cpu used = %v, want 1", r.CPUUsed)
	}
	write(memMaxFile, "max\n")
	r = p.Probe(t0.Add(2 * time.Minute))
	if r.MemLimitSet || r.FreeMemMB() < 1<<40 {
		t.Fatalf("unlimited mem %+v", r)
	}
	if r := NewProberAt(filepath.Join(dir, "missing")).Probe(t0); r.Known {
		t.Fatalf("missing cgroup should be unknown: %+v", r)
	}
}

func TestNilControllerAndProberAreInert(t *testing.T) {
	var c *Controller
	if c.Draining("reviewer") || c.DrainingNames() != nil || c.Status().Enabled {
		t.Fatal("nil controller must report nothing")
	}
	var p *Prober
	if r := p.Probe(t0); r.Known {
		t.Fatalf("nil prober must be unknown: %+v", r)
	}
	if NewProber().root != defaultCgroupRoot {
		t.Fatal("NewProber must read the default cgroup root")
	}
}

func TestReconfigureAndDrainingNamesAndPrune(t *testing.T) {
	c := New(testCfg(), "reviewer")
	rs := reviewers(3)
	if d := drive(c, 0, 3, rs, 0, nil); d.Action != ActionDrain || d.Agent != "reviewer-3" {
		t.Fatalf("drain: %+v", d)
	}
	if got := c.DrainingNames(); len(got) != 1 || got[0] != "reviewer-3" {
		t.Fatalf("draining names = %v", got)
	}
	st := c.Status()
	if st.Draining == nil || st.LastDecision == nil || st.LastDecision.Action != ActionDrain {
		t.Fatalf("status = %+v", st)
	}
	if s := st.LastDecision.String(); s != "reviewer autoscale: 3→2 drain reviewer-3 reason=idle for 3 cycles" {
		t.Fatalf("drain string = %q", s)
	}

	cfg := testCfg()
	cfg.Max = 2
	c.Reconfigure(cfg, "reviewer-x")
	if st := c.Status(); st.Max != 2 || st.Agent != "reviewer-x" {
		t.Fatalf("reconfigure not reflected: %+v", st)
	}

	// The draining replica disappears from the roster (an operator removed
	// it); the controller forgets it rather than retiring a ghost.
	if d := drive(c, 3, 1, reviewers(2), 0, nil); d.Action == ActionRetire {
		t.Fatalf("retired a reviewer that is not present: %+v", d)
	}
	if c.Draining("reviewer-3") {
		t.Fatal("absent reviewer should be pruned from draining")
	}
}

func TestDecisionStringVariants(t *testing.T) {
	cases := map[Decision]string{
		{Action: ActionHold, From: 2, Reason: "idle"}:                               "reviewer autoscale: hold at 2 reason=idle",
		{Action: ActionBlocked, From: 2, To: 3, Reason: GuardCPU}:                   "reviewer autoscale: 2→3 blocked by cpu",
		{Action: ActionRetire, From: 3, To: 2, Agent: "reviewer-3", Reason: "idle"}: "reviewer autoscale: 3→2 retire reviewer-3 reason=idle",
		{Action: ActionScaleUp, From: 2, To: 3, Reason: "backlog=7"}:                "reviewer autoscale: 2→3 reason=backlog=7",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("%+v => %q, want %q", d, got, want)
		}
	}
}

func TestProberToleratesMalformedCgroupFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// "max" quota, unparsable stat, bad memory values: nothing known, nothing crashes.
	write(cpuMaxFile, "max 100000\n")
	write(cpuStatFile, "usage_usec notanumber\n")
	write(memMaxFile, "lots\n")
	write(memCurrentFile, "many\n")
	if r := NewProberAt(dir).Probe(t0); r.Known || r.CPUUsed != 0 || r.MemUsedMB != 0 {
		t.Fatalf("malformed cgroup should be unknown: %+v", r)
	}
	write(cpuMaxFile, "abc 100000\n")
	write(cpuStatFile, "nr_periods 5\n")
	if r := NewProberAt(dir).Probe(t0); r.Known {
		t.Fatalf("unparsable quota should be unknown: %+v", r)
	}
	write(cpuMaxFile, "100000 0\n")
	if r := NewProberAt(dir).Probe(t0); r.Known {
		t.Fatalf("zero period should be unknown: %+v", r)
	}
	write(cpuMaxFile, "400000\n")
	if r := NewProberAt(dir).Probe(t0); r.Known {
		t.Fatalf("single field should be unknown: %+v", r)
	}
	// A usage counter that goes backwards clamps to zero rather than going negative.
	write(cpuMaxFile, "400000 100000\n")
	write(cpuStatFile, "usage_usec 5000000\n")
	p := NewProberAt(dir)
	p.Probe(t0)
	write(cpuStatFile, "usage_usec 1000000\n")
	if r := p.Probe(t0.Add(time.Second)); r.CPUUsed != 0 || r.CPULimit != 4 {
		t.Fatalf("backwards counter: %+v", r)
	}
	if r := p.Probe(t0.Add(time.Second)); r.CPUUsed != 0 {
		t.Fatalf("zero wall time must not divide: %+v", r)
	}
}
