package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

func TestInstallMutationSelfReportHooksObservesAndClears(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return now })
	fleetreport.SetDefaultTrackerForTest(t, tr)
	b := &mutation.Boundary{}
	installMutationSelfReportHooks(b)
	if b.OnStuck == nil || b.OnResolved == nil {
		t.Fatal("hooks not installed")
	}
	op := mutation.Operation{
		LogicalID: "op-abc",
		Status:    mutation.StatusUnknown,
		Effect:    mutation.Effect{Kind: "issue_create", Inputs: map[string]string{"repo": "owner/repo", "target": "d1g3st"}},
		Attempts:  []mutation.Attempt{{Epoch: 1}},
		UpdatedAt: now.Add(-45 * time.Minute),
	}
	b.OnStuck(op, 45*time.Minute)
	ev := tr.Evidence()
	if len(ev) != 1 || ev[0].Class != fleetreport.ClassEffectLedgerOpStuck || ev[0].Agent != "op-abc" || ev[0].Repo != "owner/repo" || ev[0].Ref != "" {
		t.Fatalf("evidence = %+v", ev)
	}
	if !strings.Contains(ev[0].Message, "issue_create") || !strings.Contains(ev[0].Message, "Unknown") || !strings.Contains(ev[0].Diagnostics, "target=d1g3st") {
		t.Fatalf("message/diagnostics = %q / %q", ev[0].Message, ev[0].Diagnostics)
	}
	b.OnResolved("op-abc")
	if ev := tr.Evidence(); len(ev) != 0 {
		t.Fatalf("resolved op still reported: %+v", ev)
	}
	installMutationSelfReportHooks(nil) // nil boundary is a no-op
}
