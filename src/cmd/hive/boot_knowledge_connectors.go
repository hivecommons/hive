package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
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

// knowledgeConnectorRuntime exposes the syncer to the dashboard. A nil
// *connector.Syncer must stay a nil interface so the dashboard reports
// "no connectors configured" instead of calling methods on a nil pointer.
func (b *boot) knowledgeConnectorRuntime() dashboard.KnowledgeConnectorRuntime {
	if b.knowledgeConnectors == nil {
		return nil
	}
	return b.knowledgeConnectors
}

// newKnowledgePublishMirror builds the `knowledge.publish` mirror (#11076)
// through the named knowledge.connectors entry. Facts are read from the
// local `path` of each published knowledge.layers entry; a layer without a
// path has nothing to publish. Publish state lives under
// <baseDir>/connector-state/_publish (connector names cannot start with "_").
// A nil reg uses connector.DefaultRegistry(). Returns nil when publishing is
// not configured.
func newKnowledgePublishMirror(pub config.KnowledgePublish, entries []config.KnowledgeConnector, layers []config.KnowledgeLayer, reg *connector.Registry, baseDir string, logger *slog.Logger) (*connector.Mirror, error) {
	if !pub.IsConfigured() {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	name := strings.TrimSpace(pub.Connector)
	var entry []config.KnowledgeConnector
	for _, e := range entries {
		if e.Name == name {
			entry = append(entry, e)
			break
		}
	}
	if len(entry) == 0 {
		return nil, fmt.Errorf("knowledge.publish.connector %q is not a knowledge.connectors entry", name)
	}
	stateDir := filepath.Join(baseDir, "connector-state", "_publish")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating publish state dir: %w", err)
	}
	pubr, err := connector.NewPublisher(reg, connectorConfigs(entry)[0], connector.Deps{
		KnowledgeDir: baseDir,
		StateDir:     filepath.Join(stateDir, name),
		Logger:       logger,
	})
	if err != nil {
		return nil, err
	}
	layerDirs := map[string]string{}
	for _, l := range layers {
		if p := strings.TrimSpace(l.Path); p != "" {
			layerDirs[strings.TrimSpace(l.Type)] = p
		}
	}
	publishLayers := make([]string, 0, len(pub.Layers))
	for _, l := range pub.Layers {
		publishLayers = append(publishLayers, strings.TrimSpace(l))
	}
	return connector.NewMirror(connector.MirrorOptions{
		Config: connector.PublishConfig{
			Connector:    name,
			Layers:       publishLayers,
			Root:         pub.Root,
			IncludeTypes: pub.IncludeTypes,
			DryRun:       pub.DryRun,
			ProposeVia:   pub.ProposeVia,
		},
		Publisher: pubr,
		VaultDir:  func(layer string) (string, error) { return layerDirs[layer], nil },
		StatePath: filepath.Join(stateDir, "state.json"),
		Logger:    logger,
	})
}

// bootKnowledgePublish builds and starts the publish mirror and hooks it to
// the promotion scheduler when one was built. A failure is logged and leaves
// publishing off; it never blocks the rest of boot. A nil reg uses
// connector.DefaultRegistry().
func (b *boot) bootKnowledgePublish(deps bootKnowledgeDeps, reg *connector.Registry) {
	k := b.cfg.Knowledge
	m, err := newKnowledgePublishMirror(k.Publish, k.Connectors, k.Layers, reg, knowledge.BaseDir(), b.logger)
	if err != nil {
		b.logger.Warn("knowledge publish mirror disabled", "error", err)
		return
	}
	if m == nil {
		return
	}
	b.knowledgePublish = m
	if b.promotionScheduler != nil {
		b.promotionScheduler.OnPromoted(func(int) { m.Trigger() })
	}
	if deps.startPublishMirror != nil {
		deps.startPublishMirror(b.ctx, m)
	}
	b.logger.Info("knowledge publish mirror started", "connector", k.Publish.Connector, "layers", k.Publish.Layers, "dry_run", k.Publish.DryRun)
}

// knowledgePublishRuntime exposes the mirror to the dashboard with the same
// nil-interface discipline as knowledgeConnectorRuntime.
func (b *boot) knowledgePublishRuntime() dashboard.KnowledgePublishRuntime {
	if b.knowledgePublish == nil {
		return nil
	}
	return b.knowledgePublish
}
