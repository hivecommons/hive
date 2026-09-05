package hub

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- parseHeartbeatTime (server.go) ----

// A heartbeat timestamp must round-trip RFC3339 and collapse both "not
// reported" (empty) and "unparseable" to the zero time, because every caller
// treats the zero time as the single "no answer" sentinel.
func TestParseHeartbeatTime(t *testing.T) {
	valid := "2026-08-29T03:00:00Z"
	want, _ := time.Parse(time.RFC3339, valid)

	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"empty means not reported", "", time.Time{}},
		{"valid RFC3339", valid, want},
		{"garbage collapses to zero", "yesterday-ish", time.Time{}},
		{"date-only is not RFC3339", "2026-08-29", time.Time{}},
		{"unix seconds are not RFC3339", "1756450000", time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseHeartbeatTime(tc.in); !got.Equal(tc.want) {
				t.Errorf("parseHeartbeatTime(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// ---- reportURLHealth (auth_audit.go) ----

// captureLogHub returns a HubServer whose slog output is captured, so tests
// can assert which reachability lines were emitted.
func captureLogHub() (*HubServer, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	return &HubServer{logger: logger}, &buf
}

// A cluster where every probe failed must roll up into the single
// cluster-wide line, and a partial failure must produce the per-hive line —
// the same split urlUnreachableAlerts applies, so log and alerts agree.
func TestReportURLHealth(t *testing.T) {
	t.Run("cluster-wide failure rolls up", func(t *testing.T) {
		s, buf := captureLogHub()
		s.reportURLHealth(nil,
			map[string]int{"c1": 4},
			map[string]int{"c1": 4})
		out := buf.String()
		if !strings.Contains(out, "cluster-wide failure") {
			t.Errorf("want cluster-wide roll-up line, got %q", out)
		}
		if strings.Contains(out, "hives whose public URL did not serve") {
			t.Errorf("cluster outage must suppress the per-hive line, got %q", out)
		}
	})

	t.Run("partial failure logs per-hive line", func(t *testing.T) {
		s, buf := captureLogHub()
		s.reportURLHealth(nil,
			map[string]int{"c1": 4},
			map[string]int{"c1": 1})
		out := buf.String()
		if !strings.Contains(out, "hives whose public URL did not serve") {
			t.Errorf("want per-hive failure line, got %q", out)
		}
		if strings.Contains(out, "cluster-wide failure") {
			t.Errorf("partial failure must not claim a cluster outage, got %q", out)
		}
	})

	t.Run("zero failures log nothing", func(t *testing.T) {
		s, buf := captureLogHub()
		s.reportURLHealth(nil,
			map[string]int{"c1": 4},
			map[string]int{"c1": 0})
		if got := buf.String(); got != "" {
			t.Errorf("healthy cluster must be silent, got %q", got)
		}
	})
}

// ---- ssoError (hub_pubkey_generations.go) ----

// The SSO sentinels are compared by identity with errors.Is; their Error()
// text is what operators see in logs, so pin both message passthrough and
// identity distinctness.
func TestSSOErrorMessageAndIdentity(t *testing.T) {
	if got := errSSONoVerificationKey.Error(); got != "sso: no verification key configured" {
		t.Errorf("errSSONoVerificationKey.Error() = %q", got)
	}
	if got := errSSORejected.Error(); got != "sso: handoff token rejected" {
		t.Errorf("errSSORejected.Error() = %q", got)
	}
	if errSSONoVerificationKey == errSSORejected {
		t.Error("the two SSO sentinels must be distinct")
	}
}

// ---- SetUpgradePauseObserver / emitUpgradePause (upgrade_pause.go) ----

// The observer must receive exactly the event that was emitted, and a nil
// observer must make emission a no-op rather than a panic.
func TestUpgradePauseObserverDelivery(t *testing.T) {
	s := &HubServer{}

	// No observer registered: must not panic.
	s.emitUpgradePause(UpgradePauseEvent{Target: "hub", Paused: true})

	var (
		mu  sync.Mutex
		got []UpgradePauseEvent
	)
	done := make(chan struct{})
	s.SetUpgradePauseObserver(func(e UpgradePauseEvent) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		close(done)
	})

	want := UpgradePauseEvent{Target: "spokes", Paused: true, By: "operator", At: "2026-08-29T03:00:00Z"}
	s.emitUpgradePause(want)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("observer was never invoked")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != want {
		t.Errorf("observer got %+v, want exactly [%+v]", got, want)
	}
}
