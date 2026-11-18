package adminmcp

import (
	"strings"
	"testing"
	"time"
)

func TestSpendReadPath(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "no window reads the all-time endpoint", args: nil, want: "/api/cost"},
		{name: "limit alone keeps the all-time endpoint", args: map[string]any{"limit": float64(5)}, want: "/api/cost"},
		{name: "hours", args: map[string]any{"hours": float64(24)}, want: "/api/cost/agents?hours=24"},
		{name: "hours as a string", args: map[string]any{"hours": "48"}, want: "/api/cost/agents?hours=48"},
		{name: "since and until", args: map[string]any{"since": "2026-11-17T00:00:00Z", "until": "2026-11-18T00:00:00Z"}, want: "/api/cost/agents?since=2026-11-17T00%3A00%3A00Z&until=2026-11-18T00%3A00%3A00Z"},
		{name: "since alone", args: map[string]any{"since": "2026-11-17T12:00:00-05:00"}, want: "/api/cost/agents?since=2026-11-17T17%3A00%3A00Z"},
		{name: "blank since is no window", args: map[string]any{"since": " "}, want: "/api/cost"},
	} {
		got, err := spendReadPathAt(tc.args, now)
		if err != nil || got != tc.want {
			t.Errorf("%s: path = %q, err %v; want %q", tc.name, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "negative hours", args: map[string]any{"hours": float64(-1)}, want: "hours must be"},
		{name: "zero hours", args: map[string]any{"hours": float64(0)}, want: "hours must be"},
		{name: "hours over the cap", args: map[string]any{"hours": float64(721)}, want: "hours must be"},
		{name: "fractional hours", args: map[string]any{"hours": float64(2.5)}, want: "hours must be"},
		{name: "hours of the wrong type", args: map[string]any{"hours": true}, want: "hours must be"},
		{name: "since with hours", args: map[string]any{"since": "2026-11-17T00:00:00Z", "hours": float64(3)}, want: "either since or hours"},
		{name: "until before since", args: map[string]any{"since": "2026-11-18T00:00:00Z", "until": "2026-11-17T00:00:00Z"}, want: "until must not be before since"},
		{name: "span over the cap", args: map[string]any{"since": "2026-09-01T00:00:00Z", "until": "2026-11-18T00:00:00Z"}, want: "at most 720 hours"},
		{name: "since not RFC3339", args: map[string]any{"since": "yesterday"}, want: "since must be an RFC3339"},
		{name: "until of the wrong type", args: map[string]any{"until": float64(1)}, want: "until must be an RFC3339"},
	} {
		if _, err := spendReadPathAt(tc.args, now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if path, ok := ReadPath(ToolSpendRead, 0); !ok || path != "/api/cost" {
		t.Errorf("ReadPath(spend_read) = %q %v", path, ok)
	}
	if props := inputSchema(ToolSpendRead)["properties"].(map[string]any); props["hours"] == nil || props["since"] == nil || props["until"] == nil {
		t.Errorf("spend_read schema lacks the window arguments: %v", props)
	}
}
