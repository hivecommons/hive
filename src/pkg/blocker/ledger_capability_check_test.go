package blocker

import (
	"testing"
	"time"
)

// projectbluefin/testsuite#1059: "the hive app cannot push
// .github/workflows/" became a workflows blocker with no check, though the
// App pushed e2e.yml the same day. With a capability check installed, a
// report the hive refutes keeps its reason and parks nothing.
func TestObserveSkipsRefutedCapabilityBlocker(t *testing.T) {
	report := "needs-human: the hive app cannot push `.github/workflows/e2e.yml`"
	at := time.Date(2026, 11, 1, 5, 9, 13, 0, time.UTC)

	l := NewLedger("")
	var asked []Kind
	l.SetCapabilityCheck(func(repo string, number int, k Kind) bool {
		asked = append(asked, k)
		return repo == "o/held" && number != 4
	})
	if e, _ := l.Observe("o/held", 1, report, at); e.Blocks() || e.Reason == "" {
		t.Fatalf("refuted report: %+v, want no blocker and the reason kept", e)
	}
	if e, _ := l.Observe("o/lacking", 2, report, at); !e.Blocks() || e.Blocker.Kind != CapabilityWorkflowsPermission {
		t.Fatalf("unrefuted report: %+v, want a workflows blocker", e)
	}
	if e, _ := l.Observe("o/held", 4, report, at); !e.Blocks() {
		t.Fatalf("the check is asked per item: %+v, want #4 blocked", e)
	}
	asked = nil
	if e, _ := l.Observe("o/held", 3, "needs-human: the contributor must sign the CLA", at); !e.Blocks() || len(asked) != 0 {
		t.Fatalf("human-only report: %+v asked=%v, want a blocker and no capability check", e, asked)
	}
	if e, _ := NewLedger("").Observe("o/held", 1, report, at); !e.Blocks() {
		t.Fatalf("no check installed: %+v, want the blocker as before", e)
	}
}

func TestRecheckCapabilitiesReleasesOnlyRefuted(t *testing.T) {
	l := NewLedger("")
	since := time.Date(2026, 11, 1, 5, 9, 13, 0, time.UTC)
	l.Record("o/a", 1, Blocker{Kind: CapabilityWorkflowsPermission, Since: since}, "r")
	l.Record("o/b", 2, Blocker{Kind: CapabilityWorkflowsPermission, Since: since}, "r")
	l.Record("o/a", 3, Blocker{Kind: HumanOnlyCLA, Since: since}, "cla")
	released := l.RecheckCapabilities(func(repo string, number int, k Kind) bool { return repo == "o/a" })
	if len(released) != 1 || released[0].Number != 1 {
		t.Fatalf("released = %+v, want o/a#1 only", released)
	}
	if _, ok := l.Blocking("o/a", 1); ok {
		t.Fatal("o/a#1 still blocked")
	}
	if _, ok := l.Blocking("o/b", 2); !ok {
		t.Fatal("o/b#2 released though the check did not refute it")
	}
	if _, ok := l.Blocking("o/a", 3); !ok {
		t.Fatal("human-only blocker released")
	}
	// A report from before the release stays stale.
	if e, _ := l.Observe("o/a", 1, "needs-human: the App cannot push workflow files", since); e.Blocks() {
		t.Fatalf("stale report re-recorded: %+v", e)
	}
	if l.RecheckCapabilities(nil) != nil {
		t.Fatal("nil check must release nothing")
	}
}
