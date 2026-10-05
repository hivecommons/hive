package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// historyPushRe matches an append onto any client-side history array, e.g.
// `historyData.push(`, `chatCommandHistory.push(`, `history.push(`.
var historyPushRe = regexp.MustCompile(`\b(\w*[Hh]istory\w*)\.push\(`)

// TestDashboardHistoryPushSitesAreCapped guards #10557: every in-memory
// history array the dashboard appends to must be trimmed right after the push,
// otherwise a tab left open for hours grows until the browser reloads it.
func TestDashboardHistoryPushSitesAreCapped(t *testing.T) {
	html := indexHTML(t)
	lines := strings.Split(html, "\n")
	found := 0
	for i, line := range lines {
		m := historyPushRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found++
		end := i + 3
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[i:end], "\n")
		name := regexp.QuoteMeta(m[1])
		capped := regexp.MustCompile(`capHistory\(` + name + `\b|` + name + `(\[[^\]]*\])?\s*=\s*[^;\n]*\.slice\(-`)
		if !capped.MatchString(window) {
			t.Errorf("index.html:%d: %s.push( is not followed by capHistory(%s) or a .slice(-N) trim", i+1, m[1], m[1])
		}
	}
	if found == 0 {
		t.Fatal("no history push sites found; guard regex is stale")
	}
}

func TestDashboardHistoryCapHelpersBounded(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const CLIENT_HISTORY_MAX_SAMPLES = ",
		"function capHistory(arr, max)",
		"historyData = capHistory(evalEntries || [])",
		"const SPARKLINE_KEY_CACHE_MAX = ",
		"function rememberSparklineKey(id, key)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("history cap contract missing %q", want)
		}
	}
	merge := jsFunctionBody(t, html, "function mergeTrendHistory()")
	if !strings.Contains(merge, "capHistory(historyData)") {
		t.Fatal("mergeTrendHistory does not cap historyData after injecting trend points")
	}
	replay := jsFunctionBody(t, html, "function replaySparklinesIn(root, opts)")
	if strings.Contains(replay, "_sparklineLastKeys.set(") || !strings.Contains(replay, "rememberSparklineKey(id, key)") {
		t.Fatal("replaySparklinesIn must record keys through the bounded rememberSparklineKey helper")
	}
	animate := jsFunctionBody(t, html, "function animateSparklineSvg(svg, length)")
	if !strings.Contains(animate, "svg.isConnected") {
		t.Fatal("animateSparklineSvg must skip svgs detached by a re-render")
	}
}

func TestDashboardLiveConnectionsCloseBeforeReconnect(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, "new EventSource('/api/events')"); got != 1 {
		t.Fatalf("EventSource('/api/events') count = %d, want 1", got)
	}
	conn := jsFunctionBody(t, html, "function connect()")
	for _, want := range []string{
		"clearTimeout(_eventSourceReconnectTimer)",
		"_eventSource.close()",
		"if (!_eventSourceReconnectTimer)",
	} {
		if !strings.Contains(conn, want) {
			t.Fatalf("connect() missing %q", want)
		}
	}
	jam := jsFunctionBody(t, html, "function connectCampaignJamLive(id)")
	for _, want := range []string{"prev.onclose = null", "prev.close()"} {
		if !strings.Contains(jam, want) {
			t.Fatalf("connectCampaignJamLive missing %q", want)
		}
	}
}
