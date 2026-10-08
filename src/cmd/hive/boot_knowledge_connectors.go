package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// connectorConfigs converts the `knowledge.connectors` YAML entries to the
// connector package's config. Config validation (ValidateKnowledgeConnectors)
// has already rejected bad intervals, so a parse error here maps to the
// connector default.
func connectorConfigs(entries []config.KnowledgeConnector) []connector.ConnectorConfig {
	out := make([]connector.ConnectorConfig, 0, len(entries))
	for _, e := range entries {
		interval, _ := e.IntervalDuration()
		out = append(out, connector.ConnectorConfig{
			Name:     e.Name,
			Type:     e.Type,
			Enabled:  e.IsEnabled(),
			Interval: interval,
			Layer:    e.Layer,
			Scope:    e.Scope,
			Auth:     connector.Auth{Env: e.Auth.Env, File: e.Auth.File},
		})
	}
	return out
}

// newKnowledgeConnectorSyncer builds the syncer for `knowledge.connectors`
// (#11069). Facts land in <baseDir>/connectors/<layer>, a vault connected to
// api on first use and reindexed after every sync, so connector pages are
// searchable like any other vault. Cursors and status persist under
// <baseDir>/connector-state. A nil reg uses connector.DefaultRegistry().
// Returns nil when no connectors are configured.
func newKnowledgeConnectorSyncer(entries []config.KnowledgeConnector, reg *connector.Registry, api *knowledge.KnowledgeAPI, baseDir string, logger *slog.Logger) (*connector.Syncer, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	stateDir := filepath.Join(baseDir, "connector-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating connector state dir: %w", err)
	}
	vaultDir := func(layer string) (string, error) {
		dir := filepath.Join(baseDir, "connectors", layer)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		if api != nil && api.GetVaultStore(dir) == nil {
			if err := api.ConnectVault(dir, "connectors-"+layer); err != nil && api.GetVaultStore(dir) == nil {
				logger.Warn("knowledge connectors: vault not connected", "dir", dir, "error", err)
			}
		}
		return dir, nil
	}
	return connector.NewSyncer(connectorConfigs(entries), connector.SyncerOptions{
		Registry: reg,
		Deps: connector.Deps{
			KnowledgeDir: baseDir,
			StateDir:     stateDir,
			Logger:       logger,
		},
		VaultDir:  vaultDir,
		StatePath: filepath.Join(stateDir, "status.json"),
		OnSync: func(st connector.Status, _ error) {
			if api == nil {
				return
			}
			if store := api.GetVaultStore(filepath.Join(baseDir, "connectors", st.Layer)); store != nil {
				store.Reindex()
			}
		},
	})
}

// bootKnowledgeConnectors builds and starts the connector syncer. A failure
// is logged and leaves connectors off; it never blocks the rest of boot.
func (b *boot) bootKnowledgeConnectors(deps bootKnowledgeDeps) {
	s, err := newKnowledgeConnectorSyncer(b.cfg.Knowledge.Connectors, nil, b.knowledgeAPI, knowledge.BaseDir(), b.logger)
	if err != nil {
		b.logger.Warn("knowledge connectors disabled", "error", err)
		return
	}
	if s == nil {
		return
	}
	b.knowledgeConnectors = s
	if deps.startConnectorSyncer != nil {
		deps.startConnectorSyncer(b.ctx, s)
	}
	b.logger.Info("knowledge connectors started", "count", len(b.cfg.Knowledge.Connectors))
}
