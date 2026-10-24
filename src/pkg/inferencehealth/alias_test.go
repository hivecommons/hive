package inferencehealth

import (
	"testing"
	"time"

	cruxinferencehealth "github.com/hivecommons/hive/pkg/crux/inferencehealth"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

// TestAliasesAreTheCruxTypes pins the alias direction: a Store built through
// this package is the Crux ring's store and a GatewayStatus is the same type
// the proxy snapshots, so hub and dashboard decode what the runtime emits.
func TestAliasesAreTheCruxTypes(t *testing.T) {
	var s *cruxinferencehealth.Store = NewStore()
	s.RecordHTTPError("gw", 503, "down", time.Date(2026, 9, 2, 8, 30, 0, 0, time.UTC))
	var snap []GatewayStatus = s.Snapshot()
	if len(snap) != 1 || snap[0].ErrorClass != Class5xx || Class5xx != cruxinferencehealth.Class5xx {
		t.Fatalf("snapshot through alias = %#v", snap)
	}
	if got, ok := MostRecent(snap); !ok || Reason(got) == "" {
		t.Fatalf("MostRecent/Reason through alias = %#v, %v", got, ok)
	}
}

// A 401/403 is an operator/upstream condition: recording it must not file
// runtime-class hive-code evidence (#12284).
func TestStoreAuthFaultIsNotSelfReported(t *testing.T) {
	fleetreport.SetDefaultTrackerForTest(t, fleetreport.NewRuntimeTracker(nil))
	at := time.Date(2026, 9, 2, 8, 30, 0, 0, time.UTC)
	for _, status := range []int{401, 403} {
		s := NewStore()
		s.RecordHTTPError("gw", status, "unauthorized", at)
		if snap := s.Snapshot(); len(snap) != 1 || snap[0].ErrorClass != ClassAuth {
			t.Fatalf("status %d: snapshot = %#v, want one auth fault", status, snap)
		}
		if ev := fleetreport.DefaultTracker().Evidence(); len(ev) != 0 {
			t.Fatalf("status %d: evidence = %#v, want none", status, ev)
		}
	}
}
