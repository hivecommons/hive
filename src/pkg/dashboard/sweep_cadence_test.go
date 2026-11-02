package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthSweepCadence(t *testing.T) {
	s := NewServer(0, slog.Default())
	s.statusMu.Lock()
	s.ready = true
	s.statusMu.Unlock()
	at := time.Now().Add(-30 * time.Second)
	s.RecordSweepCadence("sentinel", at, time.Minute, true, true)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest("GET", "/api/health", nil))
	var body struct {
		Sweeps map[string]sweepCadence `json:"sweeps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	st, ok := body.Sweeps["sentinel"]
	if !ok || st.LastRunAt == nil || !st.LastRunAt.Equal(at) || !st.Running || st.NextDueInS < 20 || st.NextDueInS > 30 {
		t.Fatalf("cadence: %+v", st)
	}
	s.RecordSweepCadence("sentinel", at, time.Minute, true, false)
	// A skipped attempt must not replace the last actual pass's timestamp.
	s.RecordSweepCadence("sentinel", at.Add(time.Minute), time.Minute, false, false)
	st = s.sweepCadences(at.Add(3 * time.Minute))["sentinel"]
	if !st.LastRunAt.Equal(at) || st.Running || !st.Skipped || st.NextDueInS != 0 {
		t.Fatalf("skipped cadence: %+v", st)
	}
}
