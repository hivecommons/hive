package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

func agentConfigLaunchCmdServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	s.RegisterAPI(&Dependencies{
		Config:      cfg,
		AgentMgr:    mgr,
		Governor:    gov,
		Logger:      logger,
		Ctx:         context.Background(),
		RefreshFunc: func() {},
		PersistFunc: func() {},
	})
	return s
}

func TestHandleAgentConfigGet_InferenceBackendUsesClaudeLaunchDefault(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"ci-maintainer": {Backend: "litellm", Model: "aws/claude-sonnet-5", Enabled: true},
				"gateway":       {Backend: "corp-gw", Model: "granite", Enabled: true},
			},
		},
		Governor: config.GovernorConfig{
			AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{
				Gateways: []config.GatewayConfig{{Name: "corp-gw", Kind: config.GatewayKindCustom, Endpoint: "https://gw.example"}},
			},
			Modes: map[string]config.ModeConfig{"idle": {Threshold: 0}},
		},
	}
	s := agentConfigLaunchCmdServer(t, cfg)
	for _, name := range []string{"ci-maintainer", "gateway"} {
		rec := doGet(s, "/api/config/agent/"+name)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", name, rec.Code)
		}
		general := decodeJSON(t, rec)["general"].(map[string]interface{})
		launchCmd, _ := general["launchCmd"].(string)
		if !strings.HasPrefix(launchCmd, "claude --model ") || strings.Contains(launchCmd, "litellm --model") || strings.Contains(launchCmd, "corp-gw --model") {
			t.Fatalf("%s launchCmd = %q, want claude CLI display default", name, launchCmd)
		}
	}
}

func TestHandleAgentConfigGeneral_RejectsInferenceBackendBinaryLaunchCmd(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"ci-maintainer": {Backend: "litellm", Model: "aws/claude-sonnet-5", Enabled: true},
			},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Threshold: 0}}},
	}
	s := agentConfigLaunchCmdServer(t, cfg)
	rec := doPut(s, "/api/config/agent/ci-maintainer/general", map[string]any{
		"launchCmd": "litellm --model aws/claude-sonnet-5",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := cfg.Agents["ci-maintainer"].LaunchCmd; got != "" {
		t.Fatalf("LaunchCmd persisted as %q, want empty after rejected save", got)
	}
}

func TestHandleAgentConfigGeneral_NormalizesDisplayDefaultLaunchCmd(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "myorg", Repos: []string{"repo1"}, AIAuthor: "bot"},
		GitHub:  config.GitHubConfig{Token: "tok"},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"ci-maintainer": {Backend: "litellm", Model: "aws/claude-sonnet-5", Enabled: true},
			},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{"idle": {Threshold: 0}}},
	}
	s := agentConfigLaunchCmdServer(t, cfg)
	rec := doPut(s, "/api/config/agent/ci-maintainer/general", map[string]any{
		"launchCmd": "claude --model aws/claude-sonnet-5 --dangerously-skip-permissions",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := cfg.Agents["ci-maintainer"].LaunchCmd; got != "" {
		t.Fatalf("LaunchCmd persisted as %q, want empty display default", got)
	}
}
