package dashboard

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/crux/logread"
)

// Settings → Logging → Read scan bound round-trips through the logging PUT,
// reports the built-in ceiling it can only lower, and rejects negatives
// (hivecommons/hive#11942).
func TestHandleGovernorLogging_ReadScanMaxMBRoundTrip(t *testing.T) {
	s, deps := kickLogSourceServer(t)

	l := governorLoggingSection(t, s)
	if l["readScanMaxMB"] != float64(0) {
		t.Fatalf("default readScanMaxMB = %v, want 0", l["readScanMaxMB"])
	}
	if want := float64(logread.DefaultWorkBoundBytes >> 20); l["readScanMaxMBDefault"] != want {
		t.Fatalf("readScanMaxMBDefault = %v, want %v", l["readScanMaxMBDefault"], want)
	}

	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"readScanMaxMB": 8}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 8: status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Governor.Logging.ReadScanMaxMB; got != 8 {
		t.Fatalf("saved read_scan_max_mb = %d, want 8", got)
	}
	if opts := s.logReadOptions(nil); opts.WorkBoundBytes != 8<<20 {
		t.Fatalf("logReadOptions WorkBoundBytes = %d, want %d (live apply)", opts.WorkBoundBytes, 8<<20)
	}
	if l = governorLoggingSection(t, s); l["readScanMaxMB"] != float64(8) {
		t.Fatalf("GET after PUT = %v", l["readScanMaxMB"])
	}

	// Absent key leaves the setting untouched; 0 resets to the built-in bound.
	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"level": "warn"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT level: status = %d", rec.Code)
	}
	if got := deps.Config.Governor.Logging.ReadScanMaxMB; got != 8 {
		t.Fatalf("absent key changed read_scan_max_mb to %d", got)
	}
	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"readScanMaxMB": 0}); rec.Code != http.StatusOK {
		t.Fatalf("PUT reset: status = %d", rec.Code)
	}
	if got := deps.Config.Governor.Logging.ReadScanMaxMB; got != 0 {
		t.Fatalf("reset left read_scan_max_mb = %d", got)
	}
	if opts := s.logReadOptions(nil); opts.WorkBoundBytes != 0 {
		t.Fatalf("reset logReadOptions WorkBoundBytes = %d, want 0 (built-in)", opts.WorkBoundBytes)
	}

	rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"readScanMaxMB": -1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT -1: status = %d, want 400", rec.Code)
	}
}
