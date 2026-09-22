package agent

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

func TestTaskMCPConnectionAddedForClaudeLaunch(t *testing.T) {
	cfg := withTaskMCPConnection(config.AgentConfig{Backend: "claude"}, "http://hub/api/contribute/mcp?token=tok", taskmcp.LaunchScope{
		TaskID: "scanner:org/repo#7:2",
		Repo:   "org/repo",
		Number: 7,
	})
	flags := connectionMCPFlags(cfg.Connections, "claude")
	if !strings.Contains(flags, "http://hub/api/contribute/mcp") {
		t.Fatalf("flags = %q, want task MCP URL", flags)
	}
	u, err := url.Parse(cfg.Connections[0].URI)
	if err != nil {
		t.Fatalf("parse URI: %v", err)
	}
	q := u.Query()
	if q.Get("token") != "tok" || q.Get("task_id") != "scanner:org/repo#7:2" || q.Get("repo") != "org/repo" || q.Get("number") != "7" {
		t.Fatalf("query = %v, want token and launch scope params", q)
	}
}

func TestTaskMCPConnectionDoesNotDuplicate(t *testing.T) {
	cfg := config.AgentConfig{Connections: []config.ConnectionConfig{{Name: "hive-task", Type: "mcp", URI: "http://old"}}}
	got := withTaskMCPConnection(cfg, "http://new", taskmcp.LaunchScope{TaskID: "t", Repo: "r"})
	if len(got.Connections) != 1 || got.Connections[0].URI != "http://old" {
		t.Fatalf("connections = %#v", got.Connections)
	}
}

func TestTaskMCPConnectionUpdatesOwnScopedURIIdempotently(t *testing.T) {
	cfg := withTaskMCPConnection(config.AgentConfig{}, "http://hub/api/contribute/mcp?token=tok", taskmcp.LaunchScope{TaskID: "t1", Repo: "org/repo", Number: 1})
	got := withTaskMCPConnection(cfg, "http://hub/api/contribute/mcp?token=tok", taskmcp.LaunchScope{TaskID: "t2", Repo: "org/repo", Number: 2})
	if len(got.Connections) != 1 {
		t.Fatalf("connections = %#v, want one hive-task connection", got.Connections)
	}
	q, _ := url.Parse(got.Connections[0].URI)
	if q.Query().Get("task_id") != "t2" || q.Query().Get("number") != "2" {
		t.Fatalf("URI = %q, want updated scoped params", got.Connections[0].URI)
	}
}

func TestTaskMCPURIWithScopeIncludesZeroNumber(t *testing.T) {
	got := taskMCPURIWithScope("http://hub/api/contribute/mcp?token=tok", taskmcp.LaunchScope{TaskID: "t0", Repo: "org/repo"})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse URI: %v", err)
	}
	q := u.Query()
	if q.Get("token") != "tok" || q.Get("task_id") != "t0" || q.Get("repo") != "org/repo" || q.Get("number") != "0" {
		t.Fatalf("query = %v, want token and zero-number launch scope", q)
	}
	if !sameTaskMCPEndpoint(got, "http://hub/api/contribute/mcp?token=tok") {
		t.Fatalf("sameTaskMCPEndpoint(%q, base) = false, want true ignoring scope params", got)
	}
	if sameTaskMCPEndpoint(got, "http://hub/api/other?token=tok") {
		t.Fatalf("sameTaskMCPEndpoint accepted a different path")
	}
}

func TestTaskMCPConnectionEmptyURLUnchanged(t *testing.T) {
	cfg := config.AgentConfig{Backend: "claude"}
	got := withTaskMCPConnection(cfg, "", taskmcp.LaunchScope{TaskID: "t", Repo: "r"})
	if len(got.Connections) != 0 || got.Backend != cfg.Backend {
		t.Fatalf("config changed for empty task MCP URL: %#v", got)
	}
}

func TestActiveLaunchesDerivesDeterministicTaskID(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{
		Org:             "hivecommons",
		PrimaryRepoName: "hive",
	})
	started := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	m.mu.Lock()
	a := m.agents["scanner"]
	a.State = StateRunning
	a.StartedAt = &started
	a.launchGen = 3
	m.mu.Unlock()

	launches := m.ActiveLaunches()
	if len(launches) != 1 {
		t.Fatalf("ActiveLaunches len = %d, want 1: %#v", len(launches), launches)
	}
	got := launches[0]
	if got.TaskID != "scanner:hivecommons/hive#0:3" || got.Repo != "hivecommons/hive" || got.Agent != "scanner" || got.Generation != 3 || !got.StartedAt.Equal(started) {
		t.Fatalf("launch scope = %#v", got)
	}
}

func TestActiveLaunchesSkipsInactiveAndNilManager(t *testing.T) {
	var nilMgr *Manager
	if got := nilMgr.ActiveLaunches(); got != nil {
		t.Fatalf("nil ActiveLaunches = %#v, want nil", got)
	}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{
		Org:             "hivecommons",
		PrimaryRepoName: "hive",
	})
	if got := m.ActiveLaunches(); len(got) != 0 {
		t.Fatalf("inactive ActiveLaunches = %#v, want empty", got)
	}
	m.mu.Lock()
	m.agents["scanner"].State = StateRunning
	m.mu.Unlock()
	if got := m.ActiveLaunches(); len(got) != 0 {
		t.Fatalf("running without StartedAt ActiveLaunches = %#v, want empty", got)
	}
}
