package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// TestExperimentAgentEnv pins what an experimenter is handed at launch: the
// Nous root and dispatcher resolved with the same precedence the hive itself
// uses (env > yaml > default), so a config written before RFC 11585 part 5
// yields inline and /data/nous.
func TestExperimentAgentEnv(t *testing.T) {
	if experimentAgentEnv(nil, nil) != nil {
		t.Fatal("nil config must yield nil env")
	}
	noEnv := func(string) string { return "" }

	got := experimentAgentEnv(&config.Config{}, noEnv)
	if got[config.NousDispatcherEnv] != config.NousDispatcherInline || got[nousstore.EnvRoot] != nousstore.DefaultRoot {
		t.Fatalf("legacy config env = %v", got)
	}

	cfg := &config.Config{}
	cfg.Nous.Dispatcher = config.NousDispatcherSDK
	cfg.Data.NousDir = "/srv/nous"
	got = experimentAgentEnv(cfg, noEnv)
	if got[config.NousDispatcherEnv] != config.NousDispatcherSDK || got[nousstore.EnvRoot] != "/srv/nous" {
		t.Fatalf("yaml env = %v", got)
	}

	env := map[string]string{config.NousDispatcherEnv: "inline", nousstore.EnvRoot: "/mnt/nous"}
	got = experimentAgentEnv(cfg, func(k string) string { return env[k] })
	if got[config.NousDispatcherEnv] != config.NousDispatcherInline || got[nousstore.EnvRoot] != "/mnt/nous" {
		t.Fatalf("env override = %v", got)
	}
}
