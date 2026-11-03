package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestTranscriptScanOverrunWarningHysteresis(t *testing.T) {
	cases := []struct {
		name  string
		takes []time.Duration
		want  int
	}{
		{"marginal flapping", []time.Duration{10700 * time.Millisecond, 9500 * time.Millisecond, 10700 * time.Millisecond, 9500 * time.Millisecond, 10700 * time.Millisecond, 9500 * time.Millisecond, 10700 * time.Millisecond, 9500 * time.Millisecond}, 0},
		{"sustained overrun warns once", []time.Duration{11 * time.Second, 11 * time.Second, 11 * time.Second, 11 * time.Second, 11 * time.Second}, 1},
		{"two slow passes stay quiet", []time.Duration{11 * time.Second, 11 * time.Second, 2 * time.Second}, 0},
		{"clear fast pass re-arms", []time.Duration{11 * time.Second, 11 * time.Second, 11 * time.Second, 2 * time.Second, 11 * time.Second, 11 * time.Second, 11 * time.Second}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			m, _ := transcriptRefreshTestManager(t)
			m.logger = slog.New(slog.NewTextHandler(&buf, nil))
			base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			caches := map[string]*transcriptSessionCache{}
			pace := &transcriptScanPace{}
			for i, took := range tc.takes {
				start := base.Add(time.Duration(i) * time.Minute)
				calls := 0
				clock := func() time.Time {
					calls++
					if calls == 1 {
						return start
					}
					return start.Add(took)
				}
				if n := m.refreshTranscriptActivity(caches, clock, pace); n != 1 {
					t.Fatalf("pass %d scanned %d agents, want 1", i, n)
				}
			}
			if got := strings.Count(buf.String(), "transcript activity scan outlived its interval"); got != tc.want {
				t.Fatalf("warnings = %d, want %d\n%s", got, tc.want, buf.String())
			}
		})
	}
}
