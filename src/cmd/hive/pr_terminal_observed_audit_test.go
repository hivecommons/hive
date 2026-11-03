package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

type recordedAudit struct {
	ts, user, action, detail, agent, repo string
	target                                int
}

type fakePRTerminalAudit struct{ got []recordedAudit }

func (f *fakePRTerminalAudit) AuditLogRecordAt(ts, user, action, detail, agent, repo string, target int) {
	f.got = append(f.got, recordedAudit{ts, user, action, detail, agent, repo, target})
}

// The PR poller's audit entries carry owner/repo, the form the hive's merge
// paths record, so one merge has one PR key on the throughput card.
func TestRecordPRTerminalObservedAuditQualifiesRepo(t *testing.T) {
	at := time.Date(2026, 10, 30, 8, 4, 13, 0, time.UTC)
	f := &fakePRTerminalAudit{}
	recordPRTerminalObservedAudit(f, "acme", github.PRTerminalObservation{Repo: "widgets", Number: 821, State: github.PRStateMerged, ObservedAt: at, Actor: "hive-app[bot]", Attribution: github.PRMergeAttributionHive})
	recordPRTerminalObservedAudit(f, "acme", github.PRTerminalObservation{Repo: "other/gears", Number: 7, State: github.PRStateClosed, ObservedAt: at, Actor: "alice", Attribution: "human"})
	recordPRTerminalObservedAudit(f, "acme", github.PRTerminalObservation{Repo: "widgets", Number: 822, State: github.PRStateMerged, ObservedAt: at})
	recordPRTerminalObservedAudit(f, "acme", github.PRTerminalObservation{Repo: "widgets", Number: 823, State: "open", ObservedAt: at})

	want := []recordedAudit{
		{"2026-10-30T08:04:13Z", "system", github.AuditActionPRMerged, "repo=acme/widgets, number=821, path=hive, actor=hive-app[bot]", github.AttributionAgentGovernor, "acme/widgets", 821},
		{"2026-10-30T08:04:13Z", "system", github.AuditActionPRClosed, "repo=other/gears, number=7, path=human, reason=observed_closed, actor=alice", github.AttributionAgentGovernor, "other/gears", 7},
		{"2026-10-30T08:04:13Z", "system", github.AuditActionPRMerged, "repo=acme/widgets, number=822, path=unknown, actor=", github.AttributionAgentGovernor, "acme/widgets", 822},
	}
	if len(f.got) != len(want) {
		t.Fatalf("recorded %d entries, want %d: %+v", len(f.got), len(want), f.got)
	}
	for i := range want {
		if f.got[i] != want[i] {
			t.Errorf("entry %d = %+v\nwant %+v", i, f.got[i], want[i])
		}
	}
}
