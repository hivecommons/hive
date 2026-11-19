package dashboard

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// newMinimalServer builds a Server with just enough Dependencies to register
// the API. (It used to live in the retired inception handler tests.)
func newMinimalServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{},
		},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config: cfg,
		Logger: slog.Default(),
		Ctx:    context.Background(),
	}
	srv.RegisterAPI(srv.deps)
	t.Cleanup(srv.CloseContributeHub)
	return srv
}
