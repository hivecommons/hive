package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/taskmcp"
)

func TestTaskMCPSnapshotMatchingFallsThroughToActiveLaunches(t *testing.T) {
	started := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	connAssigned := started.Add(-time.Minute)
	s := &Server{
		deps: &Dependencies{TaskMCPActiveLaunches: func() []taskmcp.LaunchScope {
			return []taskmcp.LaunchScope{{TaskID: "scanner:hivecommons/hive#7:2", Repo: "hivecommons/hive", Number: 7, Agent: "scanner", Generation: 2, StartedAt: started}}
		}},
		contributeHub: &ContributeWSHub{connections: map[string]*ContributorConnection{
			"conn": {
				currentTask:    &WSTaskAssign{TaskID: "relay-task", Repo: "hivecommons/hive", Number: 42, Title: "relay wins"},
				currentLabels:  []string{"relay"},
				currentTaskGen: 9,
				taskAssignedAt: connAssigned,
			},
		}},
	}
	p := dashboardTaskMCPProvider{server: s}

	t.Run("connection match wins", func(t *testing.T) {
		snap, err := p.snapshotMatching("relay-task", "hivecommons/hive", 42)
		if err != nil {
			t.Fatalf("snapshotMatching relay: %v", err)
		}
		if snap.assign.Title != "relay wins" || snap.generation != 9 {
			t.Fatalf("snapshot = %#v, want relay assignment", snap)
		}
	})

	t.Run("launch match", func(t *testing.T) {
		snap, err := p.snapshotMatching("scanner:hivecommons/hive#7:2", "hivecommons/hive", 7)
		if err != nil {
			t.Fatalf("snapshotMatching launch: %v", err)
		}
		if snap.assign.TaskID != "scanner:hivecommons/hive#7:2" || snap.assign.Repo != "hivecommons/hive" || snap.assign.Number != 7 || snap.generation != 2 {
			t.Fatalf("snapshot = %#v, want active launch assignment", snap)
		}
	})

	t.Run("no match forbidden", func(t *testing.T) {
		_, err := p.snapshotMatching("missing", "hivecommons/hive", 1)
		if err == nil || !strings.Contains(err.Error(), taskmcp.ErrForbidden.Error()) {
			t.Fatalf("snapshotMatching no match err = %v, want forbidden", err)
		}
	})
}

func TestTaskMCPSnapshotQueryFallbackPrecedence(t *testing.T) {
	started := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	p := dashboardTaskMCPProvider{server: &Server{
		deps: &Dependencies{TaskMCPActiveLaunches: func() []taskmcp.LaunchScope {
			return []taskmcp.LaunchScope{{TaskID: "scanner:hivecommons/hive#99:4", Repo: "hivecommons/hive", Number: 99, Agent: "scanner", Generation: 4, StartedAt: started}}
		}},
		contributeHub: &ContributeWSHub{connections: map[string]*ContributorConnection{}},
	}}

	req := httptest.NewRequest("POST", "/api/contribute/mcp?task_id=scanner:hivecommons/hive%2399:4&repo=hivecommons/hive&number=99", nil)
	if _, err := p.snapshot(req, nil); err != nil {
		t.Fatalf("snapshot query fallback = %v", err)
	}

	req = httptest.NewRequest("POST", "/api/contribute/mcp?task_id=scanner:hivecommons/hive%2399:4&repo=hivecommons/hive", nil)
	if _, err := p.snapshot(req, map[string]any{"task_id": "args-wins", "repo": "hivecommons/hive"}); err == nil {
		t.Fatalf("snapshot args-vs-query unexpectedly matched; args task_id must win over query")
	}

	req = httptest.NewRequest("POST", "/api/contribute/mcp?task_id=query-loses&repo=hivecommons/hive", nil)
	req.Header.Set(taskmcp.HeaderTaskID, "scanner:hivecommons/hive#99:4")
	if _, err := p.snapshot(req, map[string]any{"task_id": "args-loses", "repo": "hivecommons/hive"}); err != nil {
		t.Fatalf("snapshot header precedence = %v", err)
	}
}
