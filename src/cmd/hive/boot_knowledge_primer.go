package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/reviewrules"
)

// beadSynthVaultPath is where the bead synthesizer writes its facts.
func (b *boot) beadSynthVaultPath() string {
	if p := b.cfg.Knowledge.BeadSynthesizer.VaultPath; p != "" {
		return p
	}
	return beadSynthVaultDefaultPath
}

// registerKnowledgeStores registers every store the knowledge API has
// connected — configured vaults, ready git sources and the bead-synth vault
// — with p, and attaches the graph store and Context7 suggester when the
// graph store is already open. bootKnowledge and the live dashboard toggle
// share it, so a primer built after boot primes exactly what a boot-time
// one would (#9231). Callers register before publishing p to the scheduler:
// Primer is not safe for concurrent AddFileStore and Prime.
func (b *boot) registerKnowledgeStores(p *knowledge.Primer) {
	api := b.knowledgeAPI
	if api == nil {
		return
	}
	for _, vc := range b.cfg.Knowledge.Vaults {
		if store := api.GetVaultStore(vc.Path); store != nil {
			p.AddFileStore(vc.Name, store, knowledge.LayerPersonal)
			b.logger.Info("vault registered with primer", "name", vc.Name)
		}
	}
	for _, gsc := range b.cfg.Knowledge.GitSources {
		if store := api.GetGitSourceStore(gsc.Name); store != nil {
			p.AddFileStore(gsc.Name, store, knowledge.LayerType(gsc.Layer))
		}
	}
	// The bead-synth vault also receives CLI-memory drafts (#17033), so it is
	// primed whenever either writer is on; the Primer's approved-only default
	// keeps the drafts out of kicks.
	if len(b.beadStores) > 0 || b.cliMemoryDrainEnabled() {
		if store := api.GetVaultStore(b.beadSynthVaultPath()); store != nil {
			layer := knowledge.LayerType(b.cfg.Knowledge.BeadSynthesizer.TargetLayer)
			if layer == "" {
				layer = knowledge.LayerPersonal
			}
			p.AddFileStore(beadSynthVaultName, store, layer)
			b.logger.Info("bead-synth vault registered with primer", "layer", layer)
		}
	}
	b.registerReviewRuleVault(api, p)
	if gs := api.GraphStore(); gs != nil {
		p.SetGraphStore(gs)
		api.WireContext7Suggester(p)
	}
}

// registerReviewRuleVault connects the local channel the review-feedback sweep
// writes reviewer-stated rules into (hivecommons/hive#19046) and registers it
// with p as a pinned store: an approved rule whose scope admits a kick reaches
// that kick whatever its keywords are. The channel shows in the dashboard's
// knowledge list like any other, with each rule's source comment and reviewer.
func (b *boot) registerReviewRuleVault(api *knowledge.KnowledgeAPI, p *knowledge.Primer) {
	if !reviewrules.Enabled() {
		return
	}
	dir := reviewrules.VaultDir()
	store := api.GetVaultStore(dir)
	if store == nil {
		// Create the channel only beside the hive's other vaults. A host
		// with no vaults directory gets the channel on the first capture
		// and connects it at the next boot.
		if _, err := os.Stat(filepath.Dir(dir)); err != nil {
			if _, derr := os.Stat(dir); derr != nil {
				return
			}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.logger.Debug("review-rule vault not created; reviewer rules are not primed", "dir", dir, "error", err)
			return
		}
		if err := api.ConnectVault(dir, reviewrules.VaultName); err != nil {
			b.logger.Warn("review-rule vault not connected; reviewer rules are not primed", "dir", dir, "error", err)
			return
		}
		store = api.GetVaultStore(dir)
	}
	if store != nil {
		p.AddPinnedFileStore(reviewrules.VaultName, store, knowledge.LayerProject)
	}
}

// knowledgeRestartReason names what a live-registered primer cannot apply
// because knowledgeAPI is the file-only fallback bootKnowledge builds when
// knowledge.enabled is false at boot. Empty when nothing waits on a restart.
func (b *boot) knowledgeRestartReason() string {
	if !b.knowledgeAPIFallback {
		return ""
	}
	k := b.cfg.Knowledge
	var pending []string
	if n := len(k.Vaults); n > 0 {
		pending = append(pending, fmt.Sprintf("%d configured vault(s) are connected only at boot, so kicks do not see them yet", n))
	}
	if n := len(k.Layers); n > 0 {
		pending = append(pending, fmt.Sprintf("the dashboard does not browse the %d configured wiki layer(s) yet (kicks already query them)", n))
	}
	if k.Engine != "" && k.Engine != "file" {
		pending = append(pending, fmt.Sprintf("the dashboard still runs the file engine instead of %q", k.Engine))
	}
	if k.Curator.IsEnabled() {
		pending = append(pending, "scheduled promotion still ignores the knowledge.curator settings")
	}
	if len(pending) == 0 {
		return ""
	}
	return "kicks are primed now; restart the hive to finish enabling knowledge: " + strings.Join(pending, "; ")
}

// knowledgePrimerControl is the dashboard's handle on the scheduler's kick
// primer (dashboard.KnowledgePrimerControl). knowledge.enabled only gates
// the primer, which used to be built once in bootGovernor, so flipping the
// dashboard toggle persisted the flag and changed nothing until a restart
// (#9231).
type knowledgePrimerControl struct{ b *boot }

func (c knowledgePrimerControl) SetKnowledgePrimer(enabled bool) dashboard.KnowledgePrimerStatus {
	b := c.b
	b.knowledgePrimerMu.Lock()
	defer b.knowledgePrimerMu.Unlock()
	current := b.sched.GetPrimer()
	switch {
	case !enabled && current != nil:
		// In-flight kicks keep the primer they already read.
		b.sched.SetPrimer(nil)
		b.logger.Info("knowledge primer unregistered via dashboard toggle")
	case enabled && current == nil:
		p := b.newKnowledgePrimer()
		b.registerKnowledgeStores(p)
		b.sched.SetPrimer(p)
		b.logger.Info("knowledge primer registered via dashboard toggle", "sources", len(p.Sources()))
	}
	return c.KnowledgePrimerStatus()
}

func (c knowledgePrimerControl) KnowledgePrimerStatus() dashboard.KnowledgePrimerStatus {
	st := dashboard.KnowledgePrimerStatus{Sources: []knowledge.PrimerSource{}}
	p := c.b.sched.GetPrimer()
	if p == nil {
		return st
	}
	st.Registered = true
	st.Sources = p.Sources()
	if reason := c.b.knowledgeRestartReason(); reason != "" {
		st.RestartRequired = true
		st.RestartReason = reason
	}
	return st
}
