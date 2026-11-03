package proxy

import "testing"

func TestWriteCounters(t *testing.T) {
	var p GitHubProxy
	if got := p.WriteCounts(); len(got) != 0 {
		t.Fatalf("zero value snapshot = %v, want empty", got)
	}
	p.writes.record("ISSUES_ONLY", "")
	p.writes.record("ISSUES_ONLY", DenyReasonMode)
	p.writes.record("ISSUES_ONLY", DenyReasonMode)
	p.writes.record("ADVISORY", DenyReasonRepoPaused)

	got := p.WriteCounts()
	io := got["ISSUES_ONLY"]
	if io.Attempted != 3 || io.Blocked != 2 || io.BlockedByReason[DenyReasonMode] != 2 {
		t.Errorf("ISSUES_ONLY = %+v", io)
	}
	adv := got["ADVISORY"]
	if adv.Attempted != 1 || adv.Blocked != 1 || adv.BlockedByReason[DenyReasonRepoPaused] != 1 {
		t.Errorf("ADVISORY = %+v", adv)
	}

	got["ADVISORY"].BlockedByReason[DenyReasonMode] = 99
	if p.WriteCounts()["ADVISORY"].BlockedByReason[DenyReasonMode] != 0 {
		t.Error("snapshot aliases internal state")
	}
}
