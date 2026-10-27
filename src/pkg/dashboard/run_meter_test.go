package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/tokens"
)

func TestRunMeterDeltaResetsOnNewKick(t *testing.T) {
	m := newRunMeter()
	t0 := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	est := tokens.EstimatedCost{TotalUSD: 10, ByAgent: map[string]tokens.ModelCost{
		"dev": {USD: 1.00, Input: 1000, Output: 200, Priced: true},
	}}
	m.observeEstimate(est, t0)

	r := m.run("dev", t0, t0.Add(30*time.Second))
	if r == nil || !r.Priced || r.CostUSD != 0 || r.Tokens != 0 || r.ElapsedS != 30 {
		t.Fatalf("baseline run: %+v", r)
	}

	est.ByAgent["dev"] = tokens.ModelCost{USD: 1.25, Input: 1500, Output: 300, CacheRead: 200, Priced: true}
	est.TotalUSD = 10.25
	m.observeEstimate(est, t0.Add(time.Minute))
	r = m.run("dev", t0, t0.Add(time.Minute))
	if r.CostUSD < 0.249 || r.CostUSD > 0.251 || r.Tokens != 800 || r.ElapsedS != 60 {
		t.Fatalf("delta run: %+v", r)
	}

	// A new kick (different BusySince) restarts from the current cumulative.
	t1 := t0.Add(2 * time.Minute)
	r = m.run("dev", t1, t1.Add(5*time.Second))
	if r.CostUSD != 0 || r.Tokens != 0 || r.ElapsedS != 5 || r.Since != t1.Format(time.RFC3339) {
		t.Fatalf("new kick run: %+v", r)
	}

	// Idle drops the baseline.
	if got := m.run("dev", time.Time{}, t1); got != nil {
		t.Fatalf("idle run should be nil, got %+v", got)
	}
	if _, ok := m.baselines["dev"]; ok {
		t.Fatal("baseline kept after idle")
	}
}

func TestRunMeterUnpricedAgentAndCounterDrop(t *testing.T) {
	m := newRunMeter()
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	m.observeEstimate(tokens.EstimatedCost{ByAgent: map[string]tokens.ModelCost{"dev": {USD: 2, Input: 100}}}, now)
	r := m.run("ghost", now, now)
	if r == nil || r.Priced || r.CostUSD != 0 || r.FilesChanged != -1 {
		t.Fatalf("unpriced agent: %+v", r)
	}
	// Counter falls (pod roll): the run never goes negative.
	m.run("dev", now, now)
	m.observeEstimate(tokens.EstimatedCost{ByAgent: map[string]tokens.ModelCost{"dev": {USD: 0.5, Input: 10}}}, now)
	r = m.run("dev", now, now.Add(-time.Second))
	if r.CostUSD != 0 || r.Tokens != 0 || r.ElapsedS != 0 {
		t.Fatalf("counter drop: %+v", r)
	}
}

func TestRunMeterFleetHourRollsAndClamps(t *testing.T) {
	m := newRunMeter()
	t0 := time.Date(2026, 10, 27, 14, 10, 0, 0, time.UTC)
	if f := m.fleet(0); f.HourPriced || f.HourUSD != 0 {
		t.Fatalf("empty fleet: %+v", f)
	}
	m.observeEstimate(tokens.EstimatedCost{TotalUSD: 100}, t0)
	m.observeEstimate(tokens.EstimatedCost{TotalUSD: 101.5}, t0.Add(10*time.Minute))
	f := m.fleet(3)
	if f.Running != 3 || !f.HourPriced || f.HourUSD < 1.49 || f.HourUSD > 1.51 {
		t.Fatalf("hour spend: %+v", f)
	}
	// New hour: baseline resets.
	m.observeEstimate(tokens.EstimatedCost{TotalUSD: 102}, t0.Add(time.Hour))
	if f := m.fleet(1); f.HourUSD != 0 {
		t.Fatalf("new hour should reset: %+v", f)
	}
	// Total drops within the hour: baseline follows it down, never negative.
	m.observeEstimate(tokens.EstimatedCost{TotalUSD: 50}, t0.Add(time.Hour+time.Minute))
	m.observeEstimate(tokens.EstimatedCost{TotalUSD: 50.2}, t0.Add(time.Hour+2*time.Minute))
	if f := m.fleet(1); f.HourUSD < 0.19 || f.HourUSD > 0.21 {
		t.Fatalf("after drop: %+v", f)
	}
}

func TestParseTestSignal(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"none", []string{"$ ls", "foo.go"}, ""},
		{"go ok", []string{"$ go test ./...", "ok  \tgithub.com/x/y\t0.123s"}, "pass"},
		{"go fail", []string{"--- FAIL: TestX (0.00s)", "FAIL", "FAIL\tgithub.com/x/y\t0.1s"}, "fail"},
		{"last wins", []string{"FAIL\tgithub.com/x/y", "$ go test ./y", "ok  \tgithub.com/x/y\t0.1s"}, "pass"},
		{"pytest pass", []string{"\x1b[32m========== 12 passed in 1.20s ==========\x1b[0m"}, "pass"},
		{"pytest fail", []string{"=========== 1 failed, 11 passed in 1.20s ==========="}, "fail"},
		{"jest fail", []string{"Tests:       1 failed, 3 passed, 4 total"}, "fail"},
		{"vitest pass", []string{"Tests  7 passed (7)"}, "pass"},
		{"PASS alone", []string{"PASS", "coverage: 91.0% of statements"}, "pass"},
	}
	for _, c := range cases {
		if got := parseTestSignal(c.lines); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParseFilesChanged(t *testing.T) {
	if n := parseFilesChanged([]string{"nothing here"}); n != -1 {
		t.Fatalf("no summary: %d", n)
	}
	lines := []string{" 2 files changed, 4 insertions(+)", "later", " 1 file changed, 1 deletion(-)"}
	if n := parseFilesChanged(lines); n != 1 {
		t.Fatalf("last summary wins: %d", n)
	}
	if n := parseFilesChanged([]string{"\x1b[1m 13 files changed\x1b[0m, 200 insertions(+)"}); n != 13 {
		t.Fatalf("ansi: %d", n)
	}
}

func TestApplyRunMeterAttachesOnlyToWorkingAgents(t *testing.T) {
	m := newRunMeter()
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	since := now.Add(-90 * time.Second)
	m.observeEstimate(tokens.EstimatedCost{TotalUSD: 1, ByAgent: map[string]tokens.ModelCost{"dev": {USD: 1, Input: 10}}}, now)

	working := &crux.Process{Name: "dev", BusySince: since}
	working.SetPaneCapture([]string{"ok  \tpkg\t0.1s", " 3 files changed, 9 insertions(+)"}, func(l []string, n int) []string { return l })
	idle := &crux.Process{Name: "idle"}
	statuses := map[string]*crux.Process{"dev": working, "idle": idle}
	agents := []FrontendAgent{{Name: "dev", Busy: "working"}, {Name: "idle", Busy: "idle"}, {Name: "orphan", Busy: "working"}}

	fleet := applyRunMeter(m, agents, statuses, nil, now)
	if fleet.Running != 1 {
		t.Fatalf("running: %+v", fleet)
	}
	r := agents[0].Run
	if r == nil || r.ElapsedS != 90 || r.Tests != "pass" || r.FilesChanged != 3 || !r.Priced {
		t.Fatalf("working run: %+v", r)
	}
	if agents[1].Run != nil || agents[2].Run != nil {
		t.Fatalf("idle/orphan must carry no run: %+v %+v", agents[1].Run, agents[2].Run)
	}

	// A kick time wins over BusySince (the busy-pane watcher's stamp).
	lk := now.Add(-10 * time.Second)
	working.LastKick = &lk
	applyRunMeter(m, agents, statuses, nil, now)
	if agents[0].Run == nil || agents[0].Run.ElapsedS != 10 {
		t.Fatalf("lastKick preferred: %+v", agents[0].Run)
	}
}
