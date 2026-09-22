package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestTaskMCPConnectionAddedForClaudeLaunch(t *testing.T) {
	cfg := withTaskMCPConnection(config.AgentConfig{Backend: "claude"}, "http://hub/api/contribute/mcp")
	flags := connectionMCPFlags(cfg.Connections, "claude")
	if !strings.Contains(flags, "http://hub/api/contribute/mcp") {
		t.Fatalf("flags = %q, want task MCP URL", flags)
	}
}

func TestTaskMCPConnectionDoesNotDuplicate(t *testing.T) {
	cfg := config.AgentConfig{Connections: []config.ConnectionConfig{{Name: "hive-task", Type: "mcp", URI: "http://old"}}}
	got := withTaskMCPConnection(cfg, "http://new")
	if len(got.Connections) != 1 || got.Connections[0].URI != "http://old" {
		t.Fatalf("connections = %#v", got.Connections)
	}
}
