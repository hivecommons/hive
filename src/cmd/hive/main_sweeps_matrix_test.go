package main

import (
	"io"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// The Matrix line source must only see sentinel patches when
// dashboard.matrix is active (#15934).
func TestSentinelSweepOptionsMatrixObserverWiring(t *testing.T) {
	ghClient := hgithub.NewClient("", "hivecommons", []string{"hivecommons/hive"}, nil, "")
	tests := []struct {
		name    string
		matrix  config.MatrixConfig
		withSrv bool
		want    bool
	}{
		{name: "default off", withSrv: true},
		{name: "enabled without allowlist", matrix: config.MatrixConfig{Enabled: true}, withSrv: true},
		{name: "allowlist without enabled", matrix: config.MatrixConfig{Repos: []string{"hivecommons/hive"}}, withSrv: true},
		{name: "active without dashboard", matrix: config.MatrixConfig{Enabled: true, Repos: []string{"hivecommons/hive"}}},
		{name: "active", matrix: config.MatrixConfig{Enabled: true, Repos: []string{"hivecommons/hive"}}, withSrv: true, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Dashboard.Matrix = tc.matrix
			var dashSrv *dashboard.Server
			if tc.withSrv {
				dashSrv = dashboard.NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}
			opts := sentinelSweepOptions(cfg, ghClient, dashSrv)
			if got := opts.PatchObserver != nil; got != tc.want {
				t.Fatalf("PatchObserver set = %v, want %v", got, tc.want)
			}
		})
	}
}
