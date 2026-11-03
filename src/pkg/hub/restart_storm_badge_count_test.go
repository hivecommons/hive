package hub

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#12466: the dashboard rendered "agent restarts: guide
// ×0/24h" beside a hub verdict whose restartProblem said ×23. The badge read
// `restarts.last_24h` off the verdict row, and the verdict never carried it.

func TestAgentVerdictCarriesRestartLedger(t *testing.T) {
	a := AgentSummary{
		Name:           "guide",
		Enabled:        true,
		ExpectedActive: true,
		Restarts: AgentRestartTelemetry{
			Total:      40,
			Last24h:    23,
			LastReason: "copilot hang diagnostic timed out",
		},
	}
	out := buildAgentVerdicts([]AgentSummary{a, {Name: "quiet"}}, hiveBlockers{}, 0, time.Now())
	if len(out) != 2 {
		t.Fatalf("got %d verdicts, want 2", len(out))
	}
	var guide, quiet AgentVerdictJSON
	for _, v := range out {
		switch v.Name {
		case "guide":
			guide = v
		case "quiet":
			quiet = v
		}
	}
	if guide.Restarts == nil || guide.Restarts.Last24h != 23 {
		t.Fatalf("guide verdict restarts = %+v, want last_24h=23", guide.Restarts)
	}
	if guide.RestartProblem == "" {
		t.Fatal("guide verdict has no restartProblem; 23/24h is over the storm bar")
	}
	if quiet.Restarts != nil {
		t.Fatalf("quiet verdict restarts = %+v, want omitted for an empty ledger", quiet.Restarts)
	}
	raw, _ := json.Marshal(guide)
	if !strings.Contains(string(raw), `"restarts":{`) || !strings.Contains(string(raw), `"last_24h":23`) {
		t.Fatalf("verdict JSON lacks the ledger the badge reads:\n%s", raw)
	}
}

func TestRestartStormBadgeCountNeverZeroBesideAStorm(t *testing.T) {
	src := strings.Join([]string{
		jsFunc(t, "restartStormCount"),
		"var result = {",
		"  ledger: restartStormCount({restartProblem: 'agent restarts: guide ×23/24h (x)', restarts: {last_24h: 23}}),",
		"  fallback: restartStormCount({restartProblem: 'agent restarts: guide ×23/24h (x)'}),",
		"  nullLedger: restartStormCount({restartProblem: 'agent restarts: guide ×7/24h (x)', restarts: null}),",
		"  none: restartStormCount({}),",
		"};",
		"process.stdout.write(JSON.stringify(result));",
	}, "\n")
	cmd := exec.Command("node", "-e", src)
	stdout, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Skipf("node unavailable: %v", err)
	}
	var got struct{ Ledger, Fallback, NullLedger, None int }
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if got.Ledger != 23 || got.Fallback != 23 || got.NullLedger != 7 || got.None != 0 {
		t.Fatalf("restartStormCount = %+v, want ledger=23 fallback=23 nullLedger=7 none=0", got)
	}
}
