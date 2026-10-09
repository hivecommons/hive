package main

import (
	"os"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// codeMapVaultDefaultPath is where generated repository code maps live when
// knowledge.code_maps.vault_path is unset.
const codeMapVaultDefaultPath = "/data/vaults/code-maps"

// codeMapConfigFromHive maps the hive.yaml code_maps block onto the knowledge
// package's config. Enabled stays a pointer so absent means off.
func codeMapConfigFromHive(c config.KnowledgeCodeMaps) knowledge.CodeMapConfig {
	out := knowledge.CodeMapConfig{
		Enabled:  c.Enabled,
		Schedule: c.Schedule,
		Layer:    knowledge.LayerType(c.Layer),
		Caps:     knowledge.CodeMapCaps{MaxTotalBytes: c.MaxBytes},
	}
	for _, r := range c.Repos {
		out.Repos = append(out.Repos, knowledge.CodeMapRepoConfig{
			Name: r.Name, Path: r.Path, URL: r.URL, Branch: r.Branch,
		})
	}
	return out
}

// startCodeMaps connects the code map vault and starts regeneration (#11106).
// It is a no-op unless knowledge.code_maps.enabled is true. The vault is
// connected before the primer registers stores so maps are served like any
// other repo-scoped fact.
func (b *boot) startCodeMaps() {
	cm := b.cfg.Knowledge.CodeMaps
	if !cm.IsEnabled() {
		if len(cm.Repos) > 0 {
			b.logger.Info("knowledge.code_maps.repos is set but code map generation is disabled",
				"hint", "set knowledge.code_maps.enabled: true to opt in")
		}
		return
	}
	if b.knowledgeAPI == nil || len(cm.Repos) == 0 {
		return
	}
	vaultPath := cm.VaultPath
	if vaultPath == "" {
		vaultPath = codeMapVaultDefaultPath
	}
	if err := os.MkdirAll(vaultPath, 0o755); err != nil {
		b.logger.Warn("failed to create code map vault dir", "path", vaultPath, "error", err)
		return
	}
	if err := b.knowledgeAPI.ConnectVault(vaultPath, "code-maps"); err != nil {
		b.logger.Warn("code map vault connect", "path", vaultPath, "error", err)
	}
	var store *knowledge.FileStore
	for _, s := range b.knowledgeAPI.FileStores() {
		if s.RootDir() == vaultPath {
			store = s
			break
		}
	}
	reindex := func() {}
	if store != nil {
		reindex = store.Reindex
	}
	sched := knowledge.NewCodeMapScheduler(codeMapConfigFromHive(cm), vaultPath, reindex, b.logger)
	sched.StartBackground(b.ctx)
}
