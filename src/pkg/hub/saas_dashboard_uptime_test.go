package hub

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestUptimeCellTreatsGoZeroTimeAsUnknown(t *testing.T) {
	src := strings.Join([]string{
		"function esc(s) { return String(s == null ? '' : s); }",
		jsFunc(t, "hiveStartedTime"),
		jsFunc(t, "uptimeCell"),
		"Date.now = function() { return Date.parse('2026-09-10T20:49:54Z'); };",
		"var result = uptimeCell({startedAt:'0001-01-01T00:00:00Z'});",
		"process.stdout.write(JSON.stringify(result));",
	}, "\n")
	out, err := exec.Command("node", "-e", src).Output()
	if err != nil {
		t.Fatalf("node uptimeCell harness failed: %v", err)
	}
	var rendered string
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatalf("decode uptimeCell output %q: %v", out, err)
	}
	if !strings.Contains(rendered, ">—</span>") {
		t.Fatalf("uptimeCell rendered %q, want muted placeholder", rendered)
	}
	if strings.Contains(rendered, "739") || strings.Contains(rendered, "d</span>") {
		t.Fatalf("uptimeCell rendered bogus day duration for zero time: %q", rendered)
	}
}
