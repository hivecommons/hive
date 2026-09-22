package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentTaskMCPJSONOmitEmpty(t *testing.T) {
	data, err := json.Marshal(AgentConfig{Backend: "claude"})
	if err != nil {
		t.Fatalf("marshal default agent: %v", err)
	}
	if strings.Contains(string(data), "task_mcp") {
		t.Fatalf("default agent JSON includes task_mcp: %s", data)
	}

	data, err = json.Marshal(AgentConfig{Backend: "claude", TaskMCP: &AgentTaskMCPConfig{DropStuffedContext: true}})
	if err != nil {
		t.Fatalf("marshal task MCP agent: %v", err)
	}
	if !strings.Contains(string(data), `"task_mcp"`) || !strings.Contains(string(data), `"drop_stuffed_context":true`) {
		t.Fatalf("task MCP agent JSON missing enabled flag: %s", data)
	}
}
