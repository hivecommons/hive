package api

import "testing"

func TestLaneErrorCodesPreserveWireValues(t *testing.T) {
	for _, test := range []struct {
		code string
		wire string
	}{
		{CodeLaneUnavailable, "lane_unavailable"},
		{CodeLaneForbidden, "lane_forbidden"},
		{CodeLaneInvalid, "lane_invalid"},
		{CodeLaneConflict, "lane_conflict"},
		{CodeLaneCursor, "lane_cursor"},
		{CodeLaneStale, "lane_stale"},
		{CodeLaneCapacity, "lane_capacity"},
		{CodeLaneTimeout, "lane_timeout"},
		{CodeLaneCanceled, "lane_canceled"},
	} {
		t.Run(test.wire, func(t *testing.T) {
			if test.code != test.wire {
				t.Fatalf("code = %q, want %q", test.code, test.wire)
			}
		})
	}
}
