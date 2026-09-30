package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

func withOMPAdvisorDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	orig := ompAdvisorDir
	ompAdvisorDir = func(name string) string { return filepath.Join(root, name, ompAdvisorDirName) }
	t.Cleanup(func() { ompAdvisorDir = orig })
	return root
}

func TestOMPAdvisorExtensionShape(t *testing.T) {
	orig := advisorHookRunnerBinary
	advisorHookRunnerBinary = func() string { return "/usr/local/bin/hive" }
	defer func() { advisorHookRunnerBinary = orig }()

	src := ompAdvisorExtension()
	for _, want := range []string{
		`pi.on("session_stop"`,
		`"/usr/local/bin/hive"`,
		`"` + advisor.HookSubcommand + `"`,
		`decision: "block"`,
		`transcript_path: event.session_file`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("extension missing %q", want)
		}
	}
}

func TestOMPAdvisorRolesOverlay(t *testing.T) {
	if got := ompAdvisorRolesOverlay(nil); got != "" {
		t.Errorf("no roles must render nothing: %q", got)
	}
	got := ompAdvisorRolesOverlay(map[string]config.ModelRole{
		"advisor":  {Model: "openai/gpt-5.2", ReasoningEffort: "medium"},
		"reader":   {Backend: "omp", Model: "anthropic/claude-haiku-4.5"},
		"claudeup": {Backend: "claude", Model: "opus"},
		"empty":    {Backend: "omp"},
	})
	want := "modelRoles:\n  \"advisor\": \"openai/gpt-5.2:medium\"\n  \"reader\": \"anthropic/claude-haiku-4.5\"\n"
	if got != want {
		t.Errorf("overlay = %q, want %q", got, want)
	}
}

func TestOMPAdvisorLaunchFlag(t *testing.T) {
	root := withOMPAdvisorDir(t)
	m := advisorTestManager()
	agent := &AgentProcess{Name: "scout"}

	// Advisor off: nothing projected, nothing written.
	if got := m.advisorLaunchFlag(agent, config.AdvisorOMPBackend, false); got != "" {
		t.Errorf("advisor off must project nothing: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "scout")); err == nil {
		t.Error("advisor off must write nothing")
	}

	m.SetAdvisorEnabledResolver(func(name string) bool { return name == "scout" })

	dir := filepath.Join(root, "scout", ompAdvisorDirName)
	ext := filepath.Join(dir, ompAdvisorExtensionFile)
	flag := m.advisorLaunchFlag(agent, "OMP", false)
	if flag != " --extension '"+ext+"'" {
		t.Fatalf("no roles: flag = %q", flag)
	}
	if data, err := os.ReadFile(ext); err != nil || !strings.Contains(string(data), "session_stop") {
		t.Errorf("extension not rendered: %v", err)
	}

	m.SetAdvisorRolesResolver(func() map[string]config.ModelRole {
		return map[string]config.ModelRole{"advisor": {Model: "openai/gpt-5.2"}}
	})
	flag = m.advisorLaunchFlag(agent, config.AdvisorOMPBackend, false)
	roles := filepath.Join(dir, ompAdvisorRolesFile)
	if flag != " --extension '"+ext+"' --config '"+roles+"'" {
		t.Fatalf("with roles: flag = %q", flag)
	}
	if data, err := os.ReadFile(roles); err != nil || !strings.Contains(string(data), "modelRoles:") {
		t.Errorf("roles overlay not rendered: %v", err)
	}

	// Other agents stay unadvised.
	if got := m.advisorLaunchFlag(&AgentProcess{Name: "other"}, config.AdvisorOMPBackend, false); got != "" {
		t.Errorf("unadvised agent must project nothing: %q", got)
	}
}
