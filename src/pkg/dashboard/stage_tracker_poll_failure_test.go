package dashboard

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerUpdatedChipReportsPollFailure executes the "updated … ago"
// chip renderer under node. A payload that arrived as "fresh" must stop
// claiming so once the 30s poll fails: the chip goes amber, says "cache" and
// names the failure, so "updated 8m ago · fresh" can no longer be mistaken
// for a quiet hive when the server has stopped answering (#14416).
func TestStageTrackerUpdatedChipReportsPollFailure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the tracker's updated chip was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"function formatDashboardTime(d){return new Date(d).toISOString().slice(11,16)+' UTC';}",
		"function formatDashboardDate(d){return new Date(d).toISOString().slice(0,10);}",
		"function formatDashboardDateTime(d){return new Date(d).toISOString().slice(0,16);}",
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "agentNowClockTime"),
		jsFunc(t, html, "stageTrackerShortAgo"),
		jsFunc(t, html, "stageTrackerUpdatedHtml"),
		`const now = Date.parse('2026-10-26T19:34:00Z');`,
		`const d = { generated_at: '2026-10-26T19:26:00Z', cache_ttl_s: 120, source: 'fresh' };`,
		`const out = {`,
		`  healthy: stageTrackerUpdatedHtml({ generated_at: '2026-10-26T19:33:30Z', cache_ttl_s: 120, source: 'fresh' }, now, '', 0),`,
		`  staleOk: stageTrackerUpdatedHtml(d, now, '', 0),`,
		`  failing: stageTrackerUpdatedHtml(d, now, 'HTTP 504', Date.parse('2026-10-26T19:27:00Z')),`,
		`  unreachable: stageTrackerUpdatedHtml({ generated_at: '2026-10-26T19:33:50Z', cache_ttl_s: 120, source: 'cache' }, now, 'unreachable', 0),`,
		`};`,
		`process.stdout.write(JSON.stringify(out));`,
	}, "\n")
	out, err := exec.Command(node, "-e", script).Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	got := map[string]string{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}

	// Healthy: a young fresh payload reads as before.
	if h := got["healthy"]; !strings.Contains(h, "updated 30s ago · fresh") || strings.Contains(h, "ham-stale") || strings.Contains(h, "poll failing") {
		t.Errorf("healthy chip = %s", h)
	}
	// Polls succeeding but the payload is older than two TTLs: amber, still
	// honest about its source, no failure tag.
	if h := got["staleOk"]; !strings.Contains(h, "ham-stale") || !strings.Contains(h, "updated 8m ago · fresh") || strings.Contains(h, "poll failing") {
		t.Errorf("stale-but-polling chip = %s", h)
	}
	// Polls failing: amber, "fresh" demoted to "cache", failure named with
	// its start time, and the tooltip explains the payload is being held.
	h := got["failing"]
	for _, want := range []string{
		"ham-stale",
		"updated 8m ago · cache",
		`<span class="ham-poll-failing">poll failing (HTTP 504)</span>`,
		"Polling is failing (HTTP 504, since 07:27 PM UTC)",
		"until the server answers again",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("failing chip lacks %q: %s", want, h)
		}
	}
	if strings.Contains(h, "· fresh") {
		t.Errorf("failing chip must not claim the payload is fresh: %s", h)
	}
	// A network failure on a young payload is still flagged immediately.
	if h := got["unreachable"]; !strings.Contains(h, "ham-stale") || !strings.Contains(h, "poll failing (unreachable)") || !strings.Contains(h, "updated 10s ago · cache") {
		t.Errorf("unreachable chip = %s", h)
	}

	// The fetch path feeds the chip: every failure branch reports, every
	// success branch clears, and the clock re-stamps with the poll state.
	fetchFn := jsFunc(t, html, "fetchStageTracker")
	for _, want := range []string{
		`if (!r.ok) { stageTrackerPollFailed('HTTP ' + r.status); return; }`,
		`stageTrackerPollFailed('unreachable');`,
		`stageTrackerPollOk();`,
	} {
		if !strings.Contains(fetchFn, want) {
			t.Errorf("fetchStageTracker lacks %q", want)
		}
	}
	if !strings.Contains(jsFunc(t, html, "stageTrackerTickClock"), "stageTrackerUpdatedHtml(_stageTracker, Date.now(), _stageTrackerPollError, _stageTrackerPollErrorSince)") {
		t.Error("stageTrackerTickClock must re-stamp the chip with the poll state")
	}
	if !strings.Contains(html, ".ham-meta .ham-updated .ham-poll-failing {") {
		t.Error("index.html lacks the .ham-poll-failing rule")
	}
}
