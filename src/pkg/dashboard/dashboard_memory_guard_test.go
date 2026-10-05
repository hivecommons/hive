package dashboard

import (
	"os"
	"strings"
	"testing"
)

func TestDashboardIntervalsAreGuardedOrDocumented(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	lines := strings.Split(string(b), "\n")
	whitelist := map[string]string{
		"loadFeedbackReports(false); }, FEEDBACK_POLL_MS":                            "single top-level feedback poll created once during initial script evaluation",
		"_ocPaneTimer = setInterval(ocFetchPane, OC_PANE_POLL_MS)":                   "ocStartPanePoll calls ocStopPanePoll before recreating the pane poller",
		"_overviewCarouselTimers.push(setInterval":                                   "renderOverviewCharts clears _overviewCarouselTimers before scheduling carousel timers",
		"window._agentsCollapsedCountdownInterval = setInterval":                     "agents collapsed countdown ticker has a window-level idempotence guard",
		"ghPollTimer = setInterval(pollGHAuth, ghPollIntervalMs)":                    "GitHub device flow clears ghPollTimer before start, on slowdown, and on completion/error",
		"overlay._pollInterval = setInterval(pollCopilotAuth, COPILOT_AUTH_POLL_MS)": "Copilot device overlay stores the handle and cancelCopilotLogin clears it",
		"var countdownInterval = setInterval(function()":                             "Claude device countdown is modal-scoped and cleared on completion/cancel",
		"const timer = setInterval(() =>":                                            "NPS engagement probe clears the interval and removes listeners once enough usage is observed",
	}
	for i, line := range lines {
		if !strings.Contains(line, "setInterval(") || strings.Contains(line, "// setInterval(") {
			continue
		}
		if strings.Contains(line, "dashboardSetInterval(") || strings.Contains(line, "DASHBOARD_INTERVALS[key] = setInterval") {
			continue
		}
		ok := false
		start := i - 8
		if start < 0 {
			start = 0
		}
		end := i + 8
		if end >= len(lines) {
			end = len(lines) - 1
		}
		nearby := strings.Join(lines[start:end+1], "\n")
		for snippet, reason := range whitelist {
			if strings.Contains(line, snippet) {
				if reason == "" {
					t.Fatalf("setInterval whitelist for %q needs a reason", snippet)
				}
				if snippet == "const timer = setInterval(() =>" && (!strings.Contains(nearby, "npsHasEnoughUsage") || !strings.Contains(nearby, "clearInterval(timer)")) {
					t.Fatalf("generic timer whitelist at static/index.html:%d no longer points at the self-clearing NPS probe", i+1)
				}
				ok = true
				break
			}
		}
		if ok {
			continue
		}
		if strings.Contains(nearby, "clearInterval(") {
			continue
		}
		t.Fatalf("setInterval at static/index.html:%d is not guarded by dashboardSetInterval/clearInterval or whitelisted: %s", i+1, strings.TrimSpace(line))
	}
}

func TestDashboardHistoriesAreCapped(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		"const DASHBOARD_HISTORY_CAP = SPARKLINE_MAX_POINTS;",
		"const DASHBOARD_SERVER_HISTORY_CAP = 10000;",
		"const DASHBOARD_LOG_CAP = 500;",
		"function capDashboardArray(arr, cap)",
		"function pushDashboardRing(arr, item, cap)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("index.html missing dashboard cap helper %q", want)
		}
	}
	checks := map[string]string{
		"historyData":           "pushDashboardRing(historyData, snap, DASHBOARD_HISTORY_CAP)",
		"_factHistory":          "_factHistory = capDashboardArray(await r.json(), DASHBOARD_SERVER_HISTORY_CAP)",
		"_costHistory":          "_costHistory = capDashboardArray(await r.json(), DASHBOARD_SERVER_HISTORY_CAP)",
		"_serverTrendHistory":   "_serverTrendHistory = capDashboardArray(data, DASHBOARD_SERVER_HISTORY_CAP)",
		"_overviewKPIHistory":   "_overviewKPIHistory = capDashboardArray(data, DASHBOARD_SERVER_HISTORY_CAP)",
		"_promptHistoryEntries": "_promptHistoryEntries = capDashboardArray((data && data.entries) || [], DASHBOARD_LOG_CAP)",
		"chatContext":           "if (chatContext.length > CHAT_CONTEXT_LIMIT) chatContext = chatContext.slice(-CHAT_CONTEXT_LIMIT)",
		"chatTranscript":        "chatTranscript = chatTranscript.slice(-CHAT_MAX_TRANSCRIPT)",
		"chatCommandHistory":    "chatCommandHistory = chatCommandHistory.slice(-CHAT_MAX_COMMAND_HISTORY)",
	}
	for name, trim := range checks {
		declared := strings.Contains(html, name+" = []") ||
			strings.Contains(html, "let "+name+" = []") ||
			strings.Contains(html, "var "+name+" = []") ||
			strings.Contains(html, "let "+name+" = (() =>")
		if !declared {
			t.Fatalf("history guard no longer finds declaration for %s", name)
		}
		if !strings.Contains(html, trim) {
			t.Fatalf("%s is not trimmed with its cap; missing %q", name, trim)
		}
	}
	if !strings.Contains(html, "overviewKPISaveLocal(entries)") || !strings.Contains(html, "slice(-OVERVIEW_KPI_LOCAL_MAX)") {
		t.Fatalf("overview KPI local history is not persisted with OVERVIEW_KPI_LOCAL_MAX")
	}
}
