package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

func TestSpecialistManagerCharacterizationUsesGenericCallerMessage(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"quality": {
			Enabled: true, Backend: "codex", Mode: "ADVISORY", Role: "quality",
			KickTemplate: "quality-real-policy.md",
			Tools:        &config.ToolsConfig{Preset: "advisory"},
			Connections:  []config.ConnectionConfig{{Name: "project-wiki", Type: "knowledge", URI: "/project/wiki"}},
		},
	})
	runtime, counters := fakeSpecialistRuntime(manager)
	taskID := "swo-" + strings.Repeat("a", 64)
	message := "GENERIC_CALLER_MESSAGE\n" + SpecialistDispatchReadyMarker(taskID)

	if _, err := manager.dispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{
		TaskID: taskID, Specialist: SpecialistQuality, Message: message,
	}, runtime); err != nil {
		t.Fatal(err)
	}
	if counters.lastKick != message {
		t.Fatalf("specialist dispatch changed caller message:\n%s", counters.lastKick)
	}
	for _, absent := range []string{"quality-real-policy.md", "project-wiki", "/project/wiki"} {
		if strings.Contains(counters.lastKick, absent) {
			t.Errorf("generic specialist dispatch unexpectedly composed %q", absent)
		}
	}

	command := codexSpecialistLaunchCmd("/opt/codex", "gpt-specialist", filepath.Join(manager.workDir, "quality"))
	for _, marker := range []string{
		"project_doc_max_bytes=0", "project_doc_fallback_filenames=[]",
		`permissions.hive_specialist_mailbox.filesystem={":minimal"="read"}`,
	} {
		if !strings.Contains(command, marker) {
			t.Errorf("specialist launch no longer characterizes disabled project/repository context; missing %q: %s", marker, command)
		}
	}
	if strings.Contains(command, "--sandbox workspace-write") {
		t.Fatalf("generic specialist unexpectedly received repository write view: %s", command)
	}
}
