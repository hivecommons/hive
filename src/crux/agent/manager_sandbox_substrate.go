package agent

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// sandbox.runtime: substrate wiring (ADR-0026 phase 2); the sibling of the
// openshell helpers in manager_sandbox.go.

// substratePreflightWarnings checks each distinct control plane the
// substrate agents resolve to, once.
func substratePreflightWarnings(blocks []config.SandboxSubstrateConfig) []string {
	var out []string
	seen := map[string]bool{}
	for _, b := range blocks {
		opts := SubstrateOptionsFromConfig(b)
		key := opts.Endpoint + "\x00" + opts.Atespace + "\x00" + opts.Token
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sandbox.DefaultSubstratePreflighter.Check(context.Background(), opts)...)
	}
	return out
}

func (m *Manager) setSandboxSubstrateLauncherFactoryForTest(f func(config.SandboxSubstrateConfig) sandbox.Launcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxSubstrateLauncherFactory = f
}

// substrateLauncherLocked returns the launcher for a sandbox.runtime:
// substrate kick: the injected factory's, or a real Control-API-backed
// sandbox.SubstrateLauncher.
func (m *Manager) substrateLauncherLocked(cfg config.SandboxSubstrateConfig) sandbox.Launcher {
	if m.sandboxSubstrateLauncherFactory != nil {
		return m.sandboxSubstrateLauncherFactory(cfg)
	}
	opts := SubstrateOptionsFromConfig(cfg)
	opts.Logger = m.logger
	return &sandbox.SubstrateLauncher{Options: opts}
}

// SubstrateOptionsFromConfig converts the operator's substrate block into
// the launcher's options. A token named by token_env is read from the
// environment so it never sits in hive.yaml; otherwise the launcher reads
// the projected token file per request.
func SubstrateOptionsFromConfig(cfg config.SandboxSubstrateConfig) sandbox.SubstrateOptions {
	opts := sandbox.SubstrateOptions{
		Endpoint:           cfg.Endpoint,
		Atespace:           cfg.Atespace,
		TokenFile:          cfg.TokenFile,
		CAFile:             cfg.CAFile,
		InsecureTLS:        cfg.InsecureTLS,
		Timeout:            time.Duration(cfg.TimeoutS) * time.Second,
		RouterURL:          cfg.RouterURL,
		RouterClientBundle: cfg.RouterClientBundle,
		RouterCAFile:       cfg.RouterCAFile,
		BridgePort:         cfg.BridgePort,
		Image:              cfg.Image,
		Command:            append([]string(nil), cfg.BridgeCommand...),
		CPUMillicores:      cfg.CPUMillicores,
		MemoryBytes:        cfg.MemoryBytes,
		WorkerSelector:     cfg.WorkerSelector,
		SnapshotOnIdle:     cfg.SnapshotOnIdle,
		AllowHosts:         append([]string(nil), cfg.AllowHosts...),
		PolicyProfile:      cfg.PolicyProfile,
	}
	if env := strings.TrimSpace(cfg.TokenEnv); env != "" {
		opts.Token = os.Getenv(env)
	}
	return opts
}
