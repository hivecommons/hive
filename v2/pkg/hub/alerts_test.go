package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedNow is the reference "now" every table-driven case is evaluated at, so
// boundary conditions are exact rather than racing the wall clock.
var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

// rfc3339 formats a time the way the spoke reports StartedAt / LastHeartbeat.
func rfc3339(t time.Time) string { return t.Format(time.RFC3339) }

// findAlert returns the alert of a given type for a hive, if present.
func findAlert(alerts []Alert, hiveID, alertType string) (Alert, bool) {
	for _, a := range alerts {
		if a.HiveID == hiveID && a.Type == alertType {
			return a, true
		}
	}
	return Alert{}, false
}

// hasAlert is the boolean form of findAlert.
func hasAlert(alerts []Alert, hiveID, alertType string) bool {
	_, ok := findAlert(alerts, hiveID, alertType)
	return ok
}

// baseHive is a claimed, online, healthy hive with nothing wrong with it. Each
// test mutates only the field under test, so a rule firing means that field
// caused it.
func baseHive(id string) alertHive {
	return alertHive{
		ID:             id,
		Name:           "org/" + id,
		Online:         true,
		LastHeartbeat:  rfc3339(fixedNow.Add(-time.Minute)),
		StartedAt:      rfc3339(fixedNow.Add(-48 * time.Hour)),
		RegisteredAt:   rfc3339(fixedNow.Add(-30 * 24 * time.Hour)),
		TotalTokens24h: 5000,
	}
}

func TestEvaluateAlerts_OfflineRule(t *testing.T) {
	tests := []struct {
		name          string
		online        bool
		lastHeartbeat string
		want          bool
	}{
		{
			name:          "online hive never alerts even with an old heartbeat",
			online:        true,
			lastHeartbeat: rfc3339(fixedNow.Add(-2 * time.Hour)),
			want:          false,
		},
		{
			name:          "offline just under the threshold does not alert",
			online:        false,
			lastHeartbeat: rfc3339(fixedNow.Add(-alertOfflineThreshold + time.Second)),
			want:          false,
		},
		{
			name:          "offline exactly at the threshold does not alert (strict >)",
			online:        false,
			lastHeartbeat: rfc3339(fixedNow.Add(-alertOfflineThreshold)),
			want:          false,
		},
		{
			name:          "offline just past the threshold alerts",
			online:        false,
			lastHeartbeat: rfc3339(fixedNow.Add(-alertOfflineThreshold - time.Second)),
			want:          true,
		},
		{
			name:          "offline with an unparseable heartbeat does not alert",
			online:        false,
			lastHeartbeat: "not-a-timestamp",
			want:          false,
		},
		{
			name:          "offline with no heartbeat ever does not alert",
			online:        false,
			lastHeartbeat: "",
			want:          false,
		},
		{
			name:          "offline with a future heartbeat does not alert",
			online:        false,
			lastHeartbeat: rfc3339(fixedNow.Add(time.Hour)),
			want:          false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("h1")
			h.Online = tc.online
			h.LastHeartbeat = tc.lastHeartbeat
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			if hasAlert(got.Alerts, "h1", AlertTypeOffline) != tc.want {
				t.Fatalf("offline alert = %v, want %v (alerts: %+v)",
					!tc.want, tc.want, got.Alerts)
			}
		})
	}
}

func TestEvaluateAlerts_CrashLoopRule(t *testing.T) {
	// Each restart is a distinct StartedAt observed at a distinct time. The
	// evaluator only counts what it has OBSERVED, so the state is driven
	// through repeated evaluations rather than fabricated.
	tests := []struct {
		name string
		// restarts is how many distinct start times to feed in.
		restarts int
		// spacing between observations.
		spacing time.Duration
		// finalUptime is the uptime reported on the LAST observation.
		finalUptime time.Duration
		want        bool
	}{
		{
			name:        "single restart does not alert",
			restarts:    1,
			spacing:     time.Minute,
			finalUptime: time.Minute,
			want:        false,
		},
		{
			name:        "one below the minimum does not alert",
			restarts:    alertCrashLoopMinRestarts - 1,
			spacing:     time.Minute,
			finalUptime: time.Minute,
			want:        false,
		},
		{
			name:        "exactly the minimum alerts",
			restarts:    alertCrashLoopMinRestarts,
			spacing:     time.Minute,
			finalUptime: time.Minute,
			want:        true,
		},
		{
			name:        "above the minimum alerts",
			restarts:    alertCrashLoopMinRestarts + 3,
			spacing:     time.Minute,
			finalUptime: time.Minute,
			want:        true,
		},
		{
			name: "enough restarts but now settled past the uptime ceiling does not alert",
			// A hive that flapped and then recovered must stop alerting.
			restarts:    alertCrashLoopMinRestarts,
			spacing:     time.Minute,
			finalUptime: alertCrashLoopUptimeCeiling + time.Minute,
			want:        false,
		},
		{
			name:        "uptime exactly at the ceiling does not alert (strict <)",
			restarts:    alertCrashLoopMinRestarts,
			spacing:     time.Minute,
			finalUptime: alertCrashLoopUptimeCeiling,
			want:        false,
		},
		{
			name: "restarts spread beyond the counting window age out and do not alert",
			// Spacing each observation more than a full window apart means only
			// the newest survives the prune on each pass.
			restarts:    alertCrashLoopMinRestarts + 2,
			spacing:     alertCrashLoopWindow + time.Minute,
			finalUptime: time.Minute,
			want:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := newAlertState()
			var got AlertSummary
			// Walk time forward, reporting a new StartedAt on each pass.
			start := fixedNow.Add(-tc.spacing * time.Duration(tc.restarts))
			for i := 0; i < tc.restarts; i++ {
				now := start.Add(tc.spacing * time.Duration(i+1))
				h := baseHive("h1")
				if i == tc.restarts-1 {
					now = fixedNow
					h.StartedAt = rfc3339(now.Add(-tc.finalUptime))
				} else {
					// Each pass reports a distinct, recent start time.
					h.StartedAt = rfc3339(now.Add(-time.Minute))
				}
				got = evaluateAlerts(state, []alertHive{h}, nil, now)
			}
			if hasAlert(got.Alerts, "h1", AlertTypeCrashLoop) != tc.want {
				t.Fatalf("crash-loop alert = %v, want %v (alerts: %+v)",
					!tc.want, tc.want, got.Alerts)
			}
		})
	}
}

func TestEvaluateAlerts_CrashLoopIgnoresUnparseableStart(t *testing.T) {
	state := newAlertState()
	for i := 0; i < alertCrashLoopMinRestarts+3; i++ {
		h := baseHive("h1")
		h.StartedAt = "garbage"
		got := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(time.Duration(i)*time.Minute))
		if hasAlert(got.Alerts, "h1", AlertTypeCrashLoop) {
			t.Fatal("unparseable StartedAt must never produce a crash-loop alert")
		}
	}
}

func TestEvaluateAlerts_StuckUpgradeRule(t *testing.T) {
	tests := []struct {
		name       string
		upgrading  bool
		startedAgo time.Duration
		zeroStart  bool
		want       bool
	}{
		{
			name:      "not upgrading does not alert",
			upgrading: false,
			// Even with a long-past start time, the flag is what matters.
			startedAgo: staleUpgradeTimeout * 10,
			want:       false,
		},
		{
			name:       "upgrading well within the timeout does not alert",
			upgrading:  true,
			startedAgo: time.Minute,
			want:       false,
		},
		{
			name:       "upgrading exactly at the timeout does not alert (strict >)",
			upgrading:  true,
			startedAgo: staleUpgradeTimeout,
			want:       false,
		},
		{
			name:       "upgrading just past the timeout alerts",
			upgrading:  true,
			startedAgo: staleUpgradeTimeout + time.Second,
			want:       true,
		},
		{
			name:      "upgrading with a zero start time does not alert",
			upgrading: true,
			zeroStart: true,
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("h1")
			h.Upgrading = tc.upgrading
			if !tc.zeroStart {
				h.UpgradeStartedAt = fixedNow.Add(-tc.startedAgo)
			}
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			if hasAlert(got.Alerts, "h1", AlertTypeStuckUpgrade) != tc.want {
				t.Fatalf("stuck-upgrade alert = %v, want %v (alerts: %+v)",
					!tc.want, tc.want, got.Alerts)
			}
		})
	}
}

func TestFailingHealthChecks(t *testing.T) {
	tests := []struct {
		name   string
		health map[string]any
		want   []string
	}{
		{name: "nil health yields nothing", health: nil, want: nil},
		{name: "no checks key yields nothing", health: map[string]any{"status": "ok"}, want: nil},
		{name: "nil checks value yields nothing", health: map[string]any{"checks": nil}, want: nil},
		{
			name:   "checks of an unexpected scalar type yields nothing",
			health: map[string]any{"checks": "broken"},
			want:   nil,
		},
		{
			name: "array shape: the failing check is named",
			// This is the shape the heartbeat actually carries (HealthSummary).
			health: map[string]any{"checks": []any{
				map[string]any{"name": "ready", "status": "pass"},
				map[string]any{"name": "github_auth", "status": "fail", "detail": "token expired"},
			}},
			want: []string{"github_auth"},
		},
		{
			name: "array shape: warn and skip are not failures",
			health: map[string]any{"checks": []any{
				map[string]any{"name": "a", "status": "warn"},
				map[string]any{"name": "b", "status": "skip"},
				map[string]any{"name": "c", "status": "pass"},
			}},
			want: nil,
		},
		{
			name: "array shape: results are sorted so the reason string is stable",
			health: map[string]any{"checks": []any{
				map[string]any{"name": "zebra", "status": "fail"},
				map[string]any{"name": "alpha", "status": "fail"},
			}},
			want: []string{"alpha", "zebra"},
		},
		{
			name: "array shape: a failing check with no name is still reported",
			health: map[string]any{"checks": []any{
				map[string]any{"status": "fail"},
			}},
			want: []string{unnamedHealthCheck},
		},
		{
			name: "array shape: non-object elements are skipped, not fatal",
			health: map[string]any{"checks": []any{
				"garbage", 42, nil,
				map[string]any{"name": "real", "status": "fail"},
			}},
			want: []string{"real"},
		},
		{
			name: "array shape: a non-string status is skipped",
			health: map[string]any{"checks": []any{
				map[string]any{"name": "weird", "status": 500},
			}},
			want: nil,
		},
		{
			name: "map shape: the failing check is named",
			health: map[string]any{"checks": map[string]any{
				"ready":       map[string]any{"status": "pass"},
				"github_auth": map[string]any{"status": "fail"},
			}},
			want: []string{"github_auth"},
		},
		{
			name: "map shape: a nested failing sub-check is qualified by its parent",
			health: map[string]any{"checks": map[string]any{
				"agents": map[string]any{
					"scanner":  map[string]any{"status": "fail"},
					"reviewer": map[string]any{"status": "pass"},
				},
			}},
			want: []string{"agents/scanner"},
		},
		{
			name: "map shape: nesting deeper than one level is not walked",
			health: map[string]any{"checks": map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{"status": "fail"},
					},
				},
			}},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := failingHealthChecks(tc.health)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestEvaluateAlerts_HealthCheckFailingRule(t *testing.T) {
	h := baseHive("h1")
	h.Health = map[string]any{"checks": []any{
		map[string]any{"name": "github_auth", "status": "fail", "detail": "token expired"},
	}}
	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	a, ok := findAlert(got.Alerts, "h1", AlertTypeHealthCheckFailing)
	if !ok {
		t.Fatalf("expected a health-check alert, got %+v", got.Alerts)
	}
	if a.Severity != AlertSeverityWarning {
		t.Fatalf("severity = %q, want %q", a.Severity, AlertSeverityWarning)
	}
	// The specific failing check name must appear in the reason — that is the
	// whole point of the rule over a generic "degraded".
	if !contains(a.Reason, "github_auth") {
		t.Fatalf("reason %q must name the failing check", a.Reason)
	}
}

func TestEvaluateAlerts_HealthCheckReasonTruncatesButCountsAll(t *testing.T) {
	var checks []any
	total := maxNamedFailingChecks + 4
	for i := 0; i < total; i++ {
		checks = append(checks, map[string]any{
			"name":   string(rune('a'+i)) + "-check",
			"status": "fail",
		})
	}
	h := baseHive("h1")
	h.Health = map[string]any{"checks": checks}
	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	a, ok := findAlert(got.Alerts, "h1", AlertTypeHealthCheckFailing)
	if !ok {
		t.Fatal("expected a health-check alert")
	}
	// The true total is reported even though only a few names are listed.
	if !contains(a.Reason, itoa(total)) {
		t.Fatalf("reason %q must report the full count %d", a.Reason, total)
	}
	if !contains(a.Reason, "more") {
		t.Fatalf("reason %q must indicate the list was truncated", a.Reason)
	}
}

func TestEvaluateAlerts_ProvisionErrorRule(t *testing.T) {
	tests := []struct {
		name       string
		provStatus string
		provError  string
		want       bool
		wantDetail string
	}{
		{name: "no provisioning status does not alert", provStatus: "", want: false},
		{name: "available placeholder does not alert", provStatus: statusAvailable, want: false},
		{name: "provisioning in progress does not alert", provStatus: "provisioning", want: false},
		{
			name:       "error alerts and surfaces the error text",
			provStatus: alertProvStatusError,
			provError:  "cluster quota exceeded",
			want:       true,
			wantDetail: "cluster quota exceeded",
		},
		{
			name:       "error with no detail still alerts",
			provStatus: alertProvStatusError,
			want:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("h1")
			h.ProvStatus = tc.provStatus
			h.ProvError = tc.provError
			// A placeholder must still be able to raise a provision error.
			h.IsPlaceholder = tc.provStatus == statusAvailable
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			a, ok := findAlert(got.Alerts, "h1", AlertTypeProvisionError)
			if ok != tc.want {
				t.Fatalf("provision-error alert = %v, want %v", ok, tc.want)
			}
			if tc.wantDetail != "" && !contains(a.Reason, tc.wantDetail) {
				t.Fatalf("reason %q must include %q", a.Reason, tc.wantDetail)
			}
			if ok && a.Severity != AlertSeverityCritical {
				t.Fatalf("severity = %q, want critical", a.Severity)
			}
		})
	}
}

// TestEvaluateAlerts_PlaceholderExclusion is the rule the brief calls out
// explicitly: an unassigned pool slot must not be flagged for conditions that
// only make sense for a CLAIMED hive.
func TestEvaluateAlerts_PlaceholderExclusion(t *testing.T) {
	// Build a fleet big enough for the token-burn median to be meaningful, so
	// the placeholder is excluded by the rule rather than by the fleet-size
	// guard.
	var hives []alertHive
	for i := 0; i < alertTokenBurnMinFleetSize; i++ {
		h := baseHive("busy" + itoa(i))
		h.TotalTokens24h = 100000
		hives = append(hives, h)
	}

	// An unassigned placeholder: online, zero tokens, long registered. A claimed
	// hive in this exact state WOULD raise a zero-token alert.
	ph := baseHive("placeholder-1")
	ph.IsPlaceholder = true
	ph.TotalTokens24h = 0
	hives = append(hives, ph)

	// The control: identical, but claimed.
	claimed := baseHive("claimed-1")
	claimed.TotalTokens24h = 0
	hives = append(hives, claimed)

	got := evaluateAlerts(newAlertState(), hives, nil, fixedNow)

	if hasAlert(got.Alerts, "placeholder-1", AlertTypeTokenBurn) {
		t.Fatal("an unassigned placeholder must not raise a token-burn alert")
	}
	if !hasAlert(got.Alerts, "claimed-1", AlertTypeTokenBurn) {
		t.Fatal("the claimed control hive should raise a zero-token alert; " +
			"if not, the placeholder exclusion is not what is being tested")
	}
}

func TestEvaluateAlerts_PlaceholderStillAlertsOnOperationalFailure(t *testing.T) {
	// The exclusion is scoped: a placeholder that has gone OFFLINE is genuinely
	// broken and must still be surfaced.
	ph := baseHive("placeholder-1")
	ph.IsPlaceholder = true
	ph.Online = false
	ph.LastHeartbeat = rfc3339(fixedNow.Add(-alertOfflineThreshold - time.Hour))

	got := evaluateAlerts(newAlertState(), []alertHive{ph}, nil, fixedNow)
	if !hasAlert(got.Alerts, "placeholder-1", AlertTypeOffline) {
		t.Fatal("an offline placeholder must still raise an offline alert")
	}
}

func TestEvaluateAlerts_TokenBurnRule(t *testing.T) {
	// makeFleet builds n ordinary hives each spending `each` tokens.
	makeFleet := func(n int, each int64) []alertHive {
		var out []alertHive
		for i := 0; i < n; i++ {
			h := baseHive("fleet" + itoa(i))
			h.TotalTokens24h = each
			out = append(out, h)
		}
		return out
	}

	const normal = int64(10000)

	tests := []struct {
		name      string
		fleetSize int
		subject   func(h *alertHive)
		want      bool
	}{
		{
			name:      "a fleet too small to have a meaningful median never alerts on burn",
			fleetSize: alertTokenBurnMinFleetSize - 2,
			subject: func(h *alertHive) {
				h.TotalTokens24h = normal * 1000
			},
			want: false,
		},
		{
			name:      "burn just under the multiple does not alert",
			fleetSize: alertTokenBurnMinFleetSize,
			subject: func(h *alertHive) {
				h.TotalTokens24h = normal * alertTokenBurnMedianMultiple
			},
			want: false,
		},
		{
			name:      "burn past the multiple alerts",
			fleetSize: alertTokenBurnMinFleetSize,
			subject: func(h *alertHive) {
				h.TotalTokens24h = normal*alertTokenBurnMedianMultiple + 1
			},
			want: true,
		},
		{
			name:      "a claimed hive online at zero tokens alerts",
			fleetSize: alertTokenBurnMinFleetSize,
			subject: func(h *alertHive) {
				h.TotalTokens24h = 0
			},
			want: true,
		},
		{
			name:      "a freshly registered hive at zero tokens does not alert yet",
			fleetSize: alertTokenBurnMinFleetSize,
			subject: func(h *alertHive) {
				h.TotalTokens24h = 0
				h.RegisteredAt = rfc3339(fixedNow.Add(-alertZeroTokenMinAge + time.Hour))
			},
			want: false,
		},
		{
			name:      "an offline hive at zero tokens does not alert on burn",
			fleetSize: alertTokenBurnMinFleetSize,
			subject: func(h *alertHive) {
				h.TotalTokens24h = 0
				h.Online = false
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hives := makeFleet(tc.fleetSize, normal)
			subject := baseHive("subject")
			tc.subject(&subject)
			hives = append(hives, subject)

			got := evaluateAlerts(newAlertState(), hives, nil, fixedNow)
			if hasAlert(got.Alerts, "subject", AlertTypeTokenBurn) != tc.want {
				t.Fatalf("token-burn alert = %v, want %v (alerts: %+v)",
					!tc.want, tc.want, got.Alerts)
			}
		})
	}
}

func TestEvaluateAlerts_TokenBurnMedianFloor(t *testing.T) {
	// A fleet that has barely spent anything must not make an ordinary hive
	// look like a runaway just because it is a multiple of a tiny median.
	var hives []alertHive
	for i := 0; i < alertTokenBurnMinFleetSize; i++ {
		h := baseHive("tiny" + itoa(i))
		h.TotalTokens24h = 1
		hives = append(hives, h)
	}
	subject := baseHive("subject")
	subject.TotalTokens24h = alertTokenBurnMinMedian - 1
	hives = append(hives, subject)

	got := evaluateAlerts(newAlertState(), hives, nil, fixedNow)
	if hasAlert(got.Alerts, "subject", AlertTypeTokenBurn) {
		t.Fatalf("a median below alertTokenBurnMinMedian must suppress the burn rule (alerts: %+v)",
			got.Alerts)
	}
}

func TestFleetMedianTokens(t *testing.T) {
	tests := []struct {
		name        string
		vals        []int64
		placeholder bool
		offline     bool
		wantMedian  int64
		wantCount   int
	}{
		{name: "empty fleet", vals: nil, wantMedian: 0, wantCount: 0},
		{name: "single value", vals: []int64{7}, wantMedian: 7, wantCount: 1},
		{name: "odd count takes the middle", vals: []int64{1, 5, 100}, wantMedian: 5, wantCount: 3},
		{name: "even count means the two middles", vals: []int64{1, 3, 5, 100}, wantMedian: 4, wantCount: 4},
		{name: "unsorted input is handled", vals: []int64{100, 1, 5}, wantMedian: 5, wantCount: 3},
		{
			name: "placeholders are excluded from the median",
			vals: []int64{10, 10, 10}, placeholder: true,
			wantMedian: 0, wantCount: 0,
		},
		{
			name: "offline hives are excluded from the median",
			vals: []int64{10, 10, 10}, offline: true,
			wantMedian: 0, wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hives []alertHive
			for i, v := range tc.vals {
				h := baseHive("h" + itoa(i))
				h.TotalTokens24h = v
				h.IsPlaceholder = tc.placeholder
				h.Online = !tc.offline
				hives = append(hives, h)
			}
			median, count := fleetMedianTokens(hives)
			if median != tc.wantMedian || count != tc.wantCount {
				t.Fatalf("got (%d, %d), want (%d, %d)", median, count, tc.wantMedian, tc.wantCount)
			}
		})
	}
}

func TestEvaluateAlerts_SeverityOrderingAndCounts(t *testing.T) {
	// One hive of each severity, fed in an order that is NOT the output order.
	info := baseHive("info-hive")
	info.TotalTokens24h = 0
	// Pad the fleet so the zero-token rule is live.
	var hives []alertHive
	for i := 0; i < alertTokenBurnMinFleetSize; i++ {
		h := baseHive("pad" + itoa(i))
		h.TotalTokens24h = 50000
		hives = append(hives, h)
	}

	warn := baseHive("warn-hive")
	warn.Health = map[string]any{"checks": []any{
		map[string]any{"name": "governor", "status": "fail"},
	}}

	crit := baseHive("crit-hive")
	crit.ProvStatus = alertProvStatusError

	hives = append(hives, info, warn, crit)

	got := evaluateAlerts(newAlertState(), hives, nil, fixedNow)

	if len(got.Alerts) < 3 {
		t.Fatalf("expected at least 3 alerts, got %+v", got.Alerts)
	}
	// Most urgent first.
	if got.Alerts[0].Severity != AlertSeverityCritical {
		t.Fatalf("first alert severity = %q, want critical (alerts: %+v)",
			got.Alerts[0].Severity, got.Alerts)
	}
	// Severity must be non-decreasing in rank across the whole list.
	for i := 1; i < len(got.Alerts); i++ {
		if severityRank(got.Alerts[i-1].Severity) > severityRank(got.Alerts[i].Severity) {
			t.Fatalf("alerts not ordered by severity at index %d: %+v", i, got.Alerts)
		}
	}

	if got.CountsBySeverity[AlertSeverityCritical] < 1 {
		t.Fatal("critical count must be at least 1")
	}
	if got.CountsBySeverity[AlertSeverityWarning] < 1 {
		t.Fatal("warning count must be at least 1")
	}
	if got.CountsByType[AlertTypeProvisionError] != 1 {
		t.Fatalf("provision-error type count = %d, want 1", got.CountsByType[AlertTypeProvisionError])
	}
	if got.Total != len(got.Alerts) {
		t.Fatalf("Total = %d but %d alerts and none acknowledged", got.Total, len(got.Alerts))
	}
}

func TestEvaluateAlerts_SortIsStableAcrossRuns(t *testing.T) {
	// Map iteration order is randomised in Go; the output must not be.
	var hives []alertHive
	for i := 0; i < 6; i++ {
		h := baseHive("h" + itoa(i))
		h.ProvStatus = alertProvStatusError
		hives = append(hives, h)
	}
	var first []string
	for run := 0; run < 5; run++ {
		got := evaluateAlerts(newAlertState(), hives, nil, fixedNow)
		var order []string
		for _, a := range got.Alerts {
			order = append(order, a.HiveID+"/"+a.Type)
		}
		if run == 0 {
			first = order
			continue
		}
		if len(order) != len(first) {
			t.Fatalf("run %d produced %d alerts, first run produced %d", run, len(order), len(first))
		}
		for i := range order {
			if order[i] != first[i] {
				t.Fatalf("run %d order %v differs from first run %v", run, order, first)
			}
		}
	}
}

func TestEvaluateAlerts_UnknownSeveritySortsLast(t *testing.T) {
	// A drift-sourced alert with an unrecognised severity must not sort as if
	// it were critical.
	h := baseHive("h1")
	drift := []Alert{{HiveID: "h1", Type: AlertTypeOffline, Severity: "bogus", Reason: "x"}}
	crit := baseHive("h2")
	crit.ProvStatus = alertProvStatusError

	got := evaluateAlerts(newAlertState(), []alertHive{h, crit}, drift, fixedNow)
	if len(got.Alerts) < 2 {
		t.Fatalf("expected 2 alerts, got %+v", got.Alerts)
	}
	if got.Alerts[len(got.Alerts)-1].Severity != "bogus" {
		t.Fatalf("unknown severity must sort last, got %+v", got.Alerts)
	}
}

// TestEvaluateAlerts_DriftAlertsMergeThroughSamePipeline verifies the
// composition contract with the config-drift work: externally-sourced signals
// get FirstSeen tracking, acknowledgement and counting for free.
func TestEvaluateAlerts_DriftAlertsMergeThroughSamePipeline(t *testing.T) {
	state := newAlertState()
	drift := []Alert{{
		HiveID:   "h1",
		HiveName: "org/h1",
		Type:     AlertTypeOffline,
		Severity: AlertSeverityWarning,
		Reason:   "drift-sourced signal",
	}}
	got := evaluateAlerts(state, []alertHive{baseHive("h1")}, drift, fixedNow)

	a, ok := findAlert(got.Alerts, "h1", AlertTypeOffline)
	if !ok {
		t.Fatalf("drift alert was dropped: %+v", got.Alerts)
	}
	if a.FirstSeen.IsZero() {
		t.Fatal("a merged drift alert must get FirstSeen tracking")
	}
	if got.CountsByType[AlertTypeOffline] != 1 {
		t.Fatalf("merged drift alert must be counted, got %+v", got.CountsByType)
	}

	// And it must be acknowledgeable like any other.
	if !state.setAck("h1", AlertTypeOffline, "admin", fixedNow) {
		t.Fatal("a merged drift alert must be acknowledgeable")
	}
	got = evaluateAlerts(state, []alertHive{baseHive("h1")}, drift, fixedNow)
	a, _ = findAlert(got.Alerts, "h1", AlertTypeOffline)
	if !a.Acknowledged {
		t.Fatal("acknowledgement must apply to a merged drift alert")
	}
}

func TestEvaluateAlerts_DriftAlertsWithMissingFieldsAreDropped(t *testing.T) {
	drift := []Alert{
		{HiveID: "", Type: AlertTypeOffline},
		{HiveID: "h1", Type: ""},
	}
	got := evaluateAlerts(newAlertState(), []alertHive{baseHive("h1")}, drift, fixedNow)
	if len(got.Alerts) != 0 {
		t.Fatalf("malformed drift alerts must be dropped, got %+v", got.Alerts)
	}
}

func TestEvaluateAlerts_FirstSeenIsStableWhileConditionHolds(t *testing.T) {
	state := newAlertState()
	h := baseHive("h1")
	h.ProvStatus = alertProvStatusError

	first := evaluateAlerts(state, []alertHive{h}, nil, fixedNow)
	a1, _ := findAlert(first.Alerts, "h1", AlertTypeProvisionError)

	// An hour later, same condition: FirstSeen must not have moved.
	later := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(time.Hour))
	a2, _ := findAlert(later.Alerts, "h1", AlertTypeProvisionError)

	if !a1.FirstSeen.Equal(a2.FirstSeen) {
		t.Fatalf("FirstSeen moved while the condition held: %v -> %v", a1.FirstSeen, a2.FirstSeen)
	}
}

func TestEvaluateAlerts_FirstSeenResetsAfterConditionClears(t *testing.T) {
	state := newAlertState()
	broken := baseHive("h1")
	broken.ProvStatus = alertProvStatusError
	healthy := baseHive("h1")

	first := evaluateAlerts(state, []alertHive{broken}, nil, fixedNow)
	a1, ok := findAlert(first.Alerts, "h1", AlertTypeProvisionError)
	if !ok {
		t.Fatal("expected the condition to fire initially")
	}

	// Condition clears.
	cleared := evaluateAlerts(state, []alertHive{healthy}, nil, fixedNow.Add(time.Hour))
	if hasAlert(cleared.Alerts, "h1", AlertTypeProvisionError) {
		t.Fatal("the alert must disappear once the condition clears")
	}

	// And returns later: it must read as NEW, not inherit the old timestamp.
	again := evaluateAlerts(state, []alertHive{broken}, nil, fixedNow.Add(2*time.Hour))
	a2, ok := findAlert(again.Alerts, "h1", AlertTypeProvisionError)
	if !ok {
		t.Fatal("expected the condition to fire again")
	}
	if a1.FirstSeen.Equal(a2.FirstSeen) {
		t.Fatal("a re-occurrence must get a fresh FirstSeen")
	}
}

// --- Acknowledgement ---

func TestAcknowledgement_SilencesAndIsExcludedFromCounts(t *testing.T) {
	state := newAlertState()
	h := baseHive("h1")
	h.ProvStatus = alertProvStatusError

	before := evaluateAlerts(state, []alertHive{h}, nil, fixedNow)
	if before.Total != 1 || before.AcknowledgedTotal != 0 {
		t.Fatalf("pre-ack: Total=%d Acked=%d, want 1/0", before.Total, before.AcknowledgedTotal)
	}

	if !state.setAck("h1", AlertTypeProvisionError, "admin", fixedNow) {
		t.Fatal("setAck should succeed for a live condition")
	}

	after := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(time.Minute))
	a, ok := findAlert(after.Alerts, "h1", AlertTypeProvisionError)
	if !ok {
		t.Fatal("an acknowledged alert must still be listed, just not counted")
	}
	if !a.Acknowledged || a.AckBy != "admin" {
		t.Fatalf("alert not marked acknowledged: %+v", a)
	}
	// The whole point: it stops dominating the view.
	if after.Total != 0 {
		t.Fatalf("Total = %d, want 0 — an acknowledged alert must not count", after.Total)
	}
	if after.AcknowledgedTotal != 1 {
		t.Fatalf("AcknowledgedTotal = %d, want 1", after.AcknowledgedTotal)
	}
	if after.CountsBySeverity[AlertSeverityCritical] != 0 {
		t.Fatal("an acknowledged alert must not appear in the severity counts")
	}
}

func TestAcknowledgement_CannotPreSilenceAnAlertThatHasNotFired(t *testing.T) {
	state := newAlertState()
	if state.setAck("h1", AlertTypeOffline, "admin", fixedNow) {
		t.Fatal("acknowledging a condition that is not live must be refused")
	}
}

func TestAcknowledgement_ExpiresAfterMaxAge(t *testing.T) {
	tests := []struct {
		name     string
		elapsed  time.Duration
		wantAckd bool
	}{
		{name: "well within the window stays acknowledged", elapsed: time.Hour, wantAckd: true},
		{name: "exactly at max age stays acknowledged (strict >)", elapsed: alertAckMaxAge, wantAckd: true},
		{name: "one second past max age expires", elapsed: alertAckMaxAge + time.Second, wantAckd: false},
		{name: "long past max age expires", elapsed: alertAckMaxAge * 3, wantAckd: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := newAlertState()
			h := baseHive("h1")
			h.ProvStatus = alertProvStatusError

			// Establish the condition, then acknowledge it.
			evaluateAlerts(state, []alertHive{h}, nil, fixedNow)
			if !state.setAck("h1", AlertTypeProvisionError, "admin", fixedNow) {
				t.Fatal("setAck failed")
			}

			// The condition persists continuously the whole time.
			got := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(tc.elapsed))
			a, ok := findAlert(got.Alerts, "h1", AlertTypeProvisionError)
			if !ok {
				t.Fatal("condition should still be firing")
			}
			if a.Acknowledged != tc.wantAckd {
				t.Fatalf("Acknowledged = %v, want %v after %v", a.Acknowledged, tc.wantAckd, tc.elapsed)
			}
		})
	}
}

// TestAcknowledgement_ClearsWhenConditionResolves is the anti-permanent-mute
// guarantee: silencing an alert once must not silence its recurrence.
func TestAcknowledgement_ClearsWhenConditionResolves(t *testing.T) {
	state := newAlertState()
	broken := baseHive("h1")
	broken.ProvStatus = alertProvStatusError
	healthy := baseHive("h1")

	evaluateAlerts(state, []alertHive{broken}, nil, fixedNow)
	if !state.setAck("h1", AlertTypeProvisionError, "admin", fixedNow) {
		t.Fatal("setAck failed")
	}

	// Condition resolves — the ack goes with it.
	evaluateAlerts(state, []alertHive{healthy}, nil, fixedNow.Add(time.Hour))

	// Condition returns, well inside alertAckMaxAge so expiry is NOT what is
	// being tested here.
	got := evaluateAlerts(state, []alertHive{broken}, nil, fixedNow.Add(2*time.Hour))
	a, ok := findAlert(got.Alerts, "h1", AlertTypeProvisionError)
	if !ok {
		t.Fatal("expected the condition to fire again")
	}
	if a.Acknowledged {
		t.Fatal("a re-occurrence must NOT be silenced by the previous acknowledgement")
	}
	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1 — the recurrence must be counted", got.Total)
	}
}

func TestAcknowledgement_ClearAckRestoresTheAlert(t *testing.T) {
	state := newAlertState()
	h := baseHive("h1")
	h.ProvStatus = alertProvStatusError

	evaluateAlerts(state, []alertHive{h}, nil, fixedNow)
	if !state.setAck("h1", AlertTypeProvisionError, "admin", fixedNow) {
		t.Fatal("setAck failed")
	}
	state.clearAck("h1", AlertTypeProvisionError)

	got := evaluateAlerts(state, []alertHive{h}, nil, fixedNow.Add(time.Minute))
	a, _ := findAlert(got.Alerts, "h1", AlertTypeProvisionError)
	if a.Acknowledged {
		t.Fatal("clearAck must un-silence the alert")
	}
	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1 after clearing the ack", got.Total)
	}
}

func TestAcknowledgement_IsScopedToOneHiveAndType(t *testing.T) {
	state := newAlertState()
	a := baseHive("h1")
	a.ProvStatus = alertProvStatusError
	b := baseHive("h2")
	b.ProvStatus = alertProvStatusError

	evaluateAlerts(state, []alertHive{a, b}, nil, fixedNow)
	if !state.setAck("h1", AlertTypeProvisionError, "admin", fixedNow) {
		t.Fatal("setAck failed")
	}

	got := evaluateAlerts(state, []alertHive{a, b}, nil, fixedNow.Add(time.Minute))
	h1, _ := findAlert(got.Alerts, "h1", AlertTypeProvisionError)
	h2, _ := findAlert(got.Alerts, "h2", AlertTypeProvisionError)
	if !h1.Acknowledged {
		t.Fatal("h1 should be acknowledged")
	}
	if h2.Acknowledged {
		t.Fatal("acknowledging h1 must not silence h2")
	}
}

// --- Persistence ---

func TestAlertAcksPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "hub-alert-acks.json")
	defer func() { alertAcksPath = orig }()

	s := newTestAlertServer()
	h := baseHive("h1")
	h.ProvStatus = alertProvStatusError

	evaluateAlerts(s.alerts, []alertHive{h}, nil, fixedNow)
	if !s.alerts.setAck("h1", AlertTypeProvisionError, "admin", fixedNow) {
		t.Fatal("setAck failed")
	}
	s.saveAlertAcks()

	if _, err := os.Stat(alertAcksPath); err != nil {
		t.Fatalf("acks file was not written: %v", err)
	}

	// A fresh server (i.e. a hub restart) must honor the persisted ack.
	s2 := newTestAlertServer()
	s2.loadAlertAcks()
	got := evaluateAlerts(s2.alerts, []alertHive{h}, nil, fixedNow.Add(time.Minute))
	a, ok := findAlert(got.Alerts, "h1", AlertTypeProvisionError)
	if !ok {
		t.Fatal("condition should still fire after restart")
	}
	if !a.Acknowledged {
		t.Fatal("an acknowledgement must survive a hub restart")
	}
	if got.Total != 0 {
		t.Fatalf("Total = %d, want 0 — the restored ack must suppress the count", got.Total)
	}
}

func TestAlertAcksLoadPrunesExpired(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "hub-alert-acks.json")
	defer func() { alertAcksPath = orig }()

	stored := map[string]alertAck{
		alertAckKey("fresh", AlertTypeOffline): {
			HiveID: "fresh", Type: AlertTypeOffline, By: "admin",
			At: time.Now(), ConditionFirstSeen: time.Now(),
		},
		alertAckKey("stale", AlertTypeOffline): {
			HiveID: "stale", Type: AlertTypeOffline, By: "admin",
			At:                 time.Now().Add(-alertAckMaxAge - time.Hour),
			ConditionFirstSeen: time.Now().Add(-alertAckMaxAge - time.Hour),
		},
		alertAckKey("", ""): {},
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alertAcksPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestAlertServer()
	s.loadAlertAcks()

	acks := s.alerts.snapshotAcks()
	if _, ok := acks[alertAckKey("fresh", AlertTypeOffline)]; !ok {
		t.Fatal("a fresh ack must be loaded")
	}
	if _, ok := acks[alertAckKey("stale", AlertTypeOffline)]; ok {
		t.Fatal("an expired ack must be pruned on load")
	}
	if _, ok := acks[alertAckKey("", "")]; ok {
		t.Fatal("a malformed ack must be pruned on load")
	}

	// The pruned set must have been written back.
	raw, err := os.ReadFile(alertAcksPath)
	if err != nil {
		t.Fatal(err)
	}
	var rewritten map[string]alertAck
	if err := json.Unmarshal(raw, &rewritten); err != nil {
		t.Fatal(err)
	}
	if len(rewritten) != 1 {
		t.Fatalf("rewritten file has %d entries, want 1", len(rewritten))
	}
}

func TestAlertAcksLoadMissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "does-not-exist.json")
	defer func() { alertAcksPath = orig }()

	s := newTestAlertServer()
	s.loadAlertAcks() // must not panic
	if len(s.alerts.snapshotAcks()) != 0 {
		t.Fatal("no acks should be loaded from a missing file")
	}
}

func TestAlertAcksLoadCorruptFileIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "corrupt.json")
	defer func() { alertAcksPath = orig }()

	if err := os.WriteFile(alertAcksPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestAlertServer()
	s.loadAlertAcks() // must not panic
	if len(s.alerts.snapshotAcks()) != 0 {
		t.Fatal("no acks should be loaded from a corrupt file")
	}
}

// --- Ack HTTP handler ---

func TestHandleAlertAck(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "acks.json")
	defer func() { alertAcksPath = orig }()

	s := newTestAlertServer()
	// Establish a live condition to acknowledge.
	e := MyHiveEntry{ProvStatus: alertProvStatusError}
	e.ID = "h1"
	s.fleetAlerts([]MyHiveEntry{e})

	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/alert-ack", strings.NewReader(body))
		s.handleAlertAck(rec, req)
		return rec
	}

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{name: "acknowledging a live condition succeeds", body: `{"hiveId":"h1","type":"provision-error"}`, wantCode: http.StatusOK},
		{name: "an unknown alert type is rejected", body: `{"hiveId":"h1","type":"made-up"}`, wantCode: http.StatusBadRequest},
		{name: "a missing hiveId is rejected", body: `{"type":"offline"}`, wantCode: http.StatusBadRequest},
		{name: "a missing type is rejected", body: `{"hiveId":"h1"}`, wantCode: http.StatusBadRequest},
		{name: "a malformed body is rejected", body: `not json`, wantCode: http.StatusBadRequest},
		{name: "acknowledging a condition that is not live conflicts", body: `{"hiveId":"h9","type":"offline"}`, wantCode: http.StatusConflict},
		{name: "clearing succeeds", body: `{"hiveId":"h1","type":"provision-error","clear":true}`, wantCode: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(tc.body); rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

func TestHandleAlertAckPersists(t *testing.T) {
	dir := t.TempDir()
	orig := alertAcksPath
	alertAcksPath = filepath.Join(dir, "acks.json")
	defer func() { alertAcksPath = orig }()

	s := newTestAlertServer()
	e := MyHiveEntry{ProvStatus: alertProvStatusError}
	e.ID = "h1"
	s.fleetAlerts([]MyHiveEntry{e})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/saas/admin/alert-ack",
		strings.NewReader(`{"hiveId":"h1","type":"provision-error"}`))
	s.handleAlertAck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if _, err := os.Stat(alertAcksPath); err != nil {
		t.Fatalf("the acknowledgement was not persisted: %v", err)
	}
}

// TestAlertJSONOmitsAckFieldsWhenUnacknowledged guards the pointer on AckAt:
// as a value, `omitempty` would not elide a zero time and every unacknowledged
// alert would carry a bogus "0001-01-01T00:00:00Z".
func TestAlertJSONOmitsAckFieldsWhenUnacknowledged(t *testing.T) {
	s := newTestAlertServer()
	e := MyHiveEntry{ProvStatus: alertProvStatusError}
	e.ID = "h1"
	summary := s.fleetAlerts([]MyHiveEntry{e})
	b, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	// Inspect the ALERT object's own keys, not the whole document — the
	// summary legitimately carries "acknowledgedTotal", whose name contains
	// "acknowledged" as a substring.
	var doc struct {
		Alerts []map[string]any `json:"alerts"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Alerts) != 1 {
		t.Fatalf("want 1 alert, got %s", b)
	}
	for _, k := range []string{"ackAt", "ackBy", "acknowledged"} {
		if _, present := doc.Alerts[0][k]; present {
			t.Fatalf("unacknowledged alert must not serialise %q: %s", k, b)
		}
	}
	// And the keys the client DOES read must be present.
	for _, k := range []string{"hiveId", "type", "severity", "reason", "firstSeen"} {
		if _, present := doc.Alerts[0][k]; !present {
			t.Fatalf("alert missing %q: %s", k, b)
		}
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"alerts", "countsBySeverity", "countsByType", "total"} {
		if _, present := top[k]; !present {
			t.Fatalf("summary missing %q: %s", k, b)
		}
	}
}

// --- Placeholder classification ---

// TestIsPlaceholderEntryForAlerts covers the alerts-side expectations of the
// shared isPlaceholderEntry (defined in drift.go). TestIsPlaceholderEntry in
// drift_test.go covers the same helper from the drift side; both are kept
// because each names the cases that matter to its own caller.
func TestIsPlaceholderEntryForAlerts(t *testing.T) {
	// This MUST agree with the dashboard's isPlaceholderHive(): provStatus
	// 'available' is authoritative, with the 'available-' org prefix as the
	// fallback.
	tests := []struct {
		name       string
		provStatus string
		org        string
		want       bool
	}{
		{name: "provStatus available is a placeholder", provStatus: statusAvailable, want: true},
		{name: "available- org prefix is a placeholder", org: "available-pool-a", want: true},
		{name: "a claimed hive is not a placeholder", provStatus: "", org: "kubestellar", want: false},
		{name: "provisioning is not a placeholder", provStatus: "provisioning", org: "kubestellar", want: false},
		{name: "an org merely containing available- is not a placeholder", org: "not-available-here", want: false},
		{name: "empty everything is not a placeholder", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := MyHiveEntry{ProvStatus: tc.provStatus}
			e.Org = tc.org
			if got := isPlaceholderEntry(e); got != tc.want {
				t.Fatalf("isPlaceholderEntry = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- Helpers ---

func TestIsKnownAlertType(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{AlertTypeCrashLoop, true},
		{AlertTypeOffline, true},
		{AlertTypeStuckUpgrade, true},
		{AlertTypeHealthCheckFailing, true},
		{AlertTypeTokenBurn, true},
		{AlertTypeProvisionError, true},
		{"", false},
		{"made-up", false},
	} {
		if got := isKnownAlertType(tc.in); got != tc.want {
			t.Fatalf("isKnownAlertType(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestItoa64(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{-7, "-7"},
		{1234567890, "1234567890"},
		{-1234567890, "-1234567890"},
		// The negative-accumulator implementation must not overflow here.
		{-9223372036854775808, "-9223372036854775808"},
		{9223372036854775807, "9223372036854775807"},
	} {
		if got := itoa64(tc.in); got != tc.want {
			t.Fatalf("itoa64(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEvaluateAlerts_EmptyAndNilInputs(t *testing.T) {
	// A nil state must not panic and must return a usable (non-nil-map) summary.
	got := evaluateAlerts(nil, []alertHive{baseHive("h1")}, nil, fixedNow)
	if got.CountsBySeverity == nil || got.CountsByType == nil {
		t.Fatal("counts maps must never be nil")
	}

	got = evaluateAlerts(newAlertState(), nil, nil, fixedNow)
	if got.Total != 0 || len(got.Alerts) != 0 {
		t.Fatalf("an empty fleet must produce no alerts, got %+v", got)
	}

	// A hive with no ID is skipped rather than producing keyless alerts.
	blank := alertHive{ProvStatus: alertProvStatusError}
	got = evaluateAlerts(newAlertState(), []alertHive{blank}, nil, fixedNow)
	if len(got.Alerts) != 0 {
		t.Fatalf("a hive with no ID must be skipped, got %+v", got.Alerts)
	}
}

func TestAlertHiveFromEntry(t *testing.T) {
	e := MyHiveEntry{ProvStatus: statusAvailable, ProvError: "boom"}
	e.ID = "h1"
	e.Name = "org/h1"
	e.Online = true
	e.TotalTokens24h = 42
	e.Upgrading = true
	e.UpgradeStartedAt = fixedNow

	got := alertHiveFromEntry(e)
	if got.ID != "h1" || got.Name != "org/h1" || !got.Online || got.TotalTokens24h != 42 {
		t.Fatalf("projection lost fields: %+v", got)
	}
	if !got.IsPlaceholder {
		t.Fatal("provStatus available must project as a placeholder")
	}
	if !got.Upgrading || !got.UpgradeStartedAt.Equal(fixedNow) {
		t.Fatal("upgrade fields must be carried through")
	}
	if got.ProvError != "boom" {
		t.Fatal("ProvError must be carried through")
	}
}

func TestFleetAlertsOnServer(t *testing.T) {
	s := newTestAlertServer()
	e := MyHiveEntry{ProvStatus: alertProvStatusError, ProvError: "quota"}
	e.ID = "h1"
	e.Name = "org/h1"

	got := s.fleetAlerts([]MyHiveEntry{e})
	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1 (alerts: %+v)", got.Total, got.Alerts)
	}
	if !hasAlert(got.Alerts, "h1", AlertTypeProvisionError) {
		t.Fatalf("expected a provision-error alert, got %+v", got.Alerts)
	}

	// A server with no alert state must degrade gracefully rather than panic.
	var nilState HubServer
	empty := nilState.fleetAlerts([]MyHiveEntry{e})
	if empty.Total != 0 || empty.CountsByType == nil {
		t.Fatalf("a server without alert state must return an empty usable summary, got %+v", empty)
	}
}

// newTestAlertServer builds the minimal HubServer the alert code touches:
// a logger (persistence failures are logged) and an alert state.
func newTestAlertServer() *HubServer {
	return &HubServer{logger: slog.Default(), alerts: newAlertState()}
}

// contains is a tiny substring helper so the assertions read clearly.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestEvaluateAlerts_AdvisoryStaleRule(t *testing.T) {
	h := baseHive("h1")
	h.AdvisoryStale = true
	h.AdvisoryStaleReason = "No advisory digest posted for 3h20m"

	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	a, ok := findAlert(got.Alerts, "h1", AlertTypeAdvisoryStale)
	if !ok {
		t.Fatalf("expected a stale-advisory alert, got %+v", got.Alerts)
	}
	if a.Severity != AlertSeverityWarning {
		t.Fatalf("severity = %q, want %q", a.Severity, AlertSeverityWarning)
	}
	// The SERVER-supplied reason must be surfaced verbatim rather than replaced
	// by generic wording — advisoryStale() owns the explanation.
	if a.Reason != h.AdvisoryStaleReason {
		t.Fatalf("reason = %q, want the server-supplied %q", a.Reason, h.AdvisoryStaleReason)
	}
}

func TestEvaluateAlerts_AdvisoryStaleNotRaisedWhenFlagUnset(t *testing.T) {
	// The gating (advisory-mode participation, app-can-write, unknown
	// timestamps) lives entirely in advisoryStale(). The evaluator must trust
	// the flag and never re-derive it, so a cleared flag means no alert even
	// though a reason string is present.
	h := baseHive("h1")
	h.AdvisoryStale = false
	h.AdvisoryStaleReason = "stale-looking but not flagged"

	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	if hasAlert(got.Alerts, "h1", AlertTypeAdvisoryStale) {
		t.Fatal("a hive the hub did not flag stale must not raise a stale-advisory alert")
	}
}

func TestEvaluateAlerts_AdvisoryStaleFallsBackWhenReasonEmpty(t *testing.T) {
	h := baseHive("h1")
	h.AdvisoryStale = true
	h.AdvisoryStaleReason = ""

	got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
	a, ok := findAlert(got.Alerts, "h1", AlertTypeAdvisoryStale)
	if !ok {
		t.Fatalf("expected a stale-advisory alert, got %+v", got.Alerts)
	}
	if a.Reason == "" {
		t.Fatal("reason must never be empty — the panel renders it as the detail line")
	}
}

func TestEvaluateAlerts_AdvisoryStalePlaceholderExclusion(t *testing.T) {
	// An unassigned pool slot posts no advisory digests by definition, so it
	// must never be flagged, matching every other claimed-hive rule.
	ph := baseHive("placeholder-1")
	ph.IsPlaceholder = true
	ph.AdvisoryStale = true
	ph.AdvisoryStaleReason = "stale"

	claimed := baseHive("claimed-1")
	claimed.AdvisoryStale = true
	claimed.AdvisoryStaleReason = "stale"

	got := evaluateAlerts(newAlertState(), []alertHive{ph, claimed}, nil, fixedNow)
	if hasAlert(got.Alerts, "placeholder-1", AlertTypeAdvisoryStale) {
		t.Fatal("an unassigned placeholder must not raise a stale-advisory alert")
	}
	if !hasAlert(got.Alerts, "claimed-1", AlertTypeAdvisoryStale) {
		t.Fatal("the claimed control hive should raise the alert; " +
			"if not, the placeholder exclusion is not what is being tested")
	}
}

func TestAdvisoryStaleIsAKnownAlertType(t *testing.T) {
	// The ack endpoint rejects unknown types, so omitting this would leave the
	// new alert permanently un-acknowledgeable from the panel.
	if !isKnownAlertType(AlertTypeAdvisoryStale) {
		t.Fatal("AlertTypeAdvisoryStale must be ack-able via the alert ack endpoint")
	}
}

func TestAlertHiveFromEntry_CarriesAdvisoryStaleness(t *testing.T) {
	// The projection is the only path from MyHiveEntry into the evaluator; if
	// it drops these fields the rule can never fire in production even though
	// every unit test above passes.
	got := alertHiveFromEntry(MyHiveEntry{
		RegistryEntry:       RegistryEntry{ID: "h1"},
		AdvisoryStale:       true,
		AdvisoryStaleReason: "why",
	})
	if !got.AdvisoryStale || got.AdvisoryStaleReason != "why" {
		t.Fatalf("advisory staleness not projected: %+v", got)
	}
}
