package main

import "github.com/hivecommons/hive/pkg/config"

// submitConfigReload is the config watcher's callback. The governor loop
// holds cfgReloadMu for a whole tick (minutes on a busy spoke), so waiting
// for it starved hot-reload, and every save made from the in-memory config
// in the meantime (the tick's persistState, whose
// Config.ReconcilePausedAndSave rewrites hive.yaml) wrote the pre-reload
// config over the operator's edit (hivecommons/hive#17467). The reload is
// parked instead: applied now when the lock is free, otherwise by the holder
// at its next safe point, which is always before the tick's persist. Only the
// newest parked reload is applied; each one is a full file.
//
// Savers that run without cfgReloadMu (heartbeat callbacks, dashboard
// mutations, the pause callback) can still write the pre-reload config to
// hive.yaml while the reload is parked. config.SaveSeq() is recorded at park
// time so applyPendingReloadLocked can detect that and save the reloaded
// config back to disk.
func (b *boot) submitConfigReload(newCfg *config.Config) {
	b.pendingReloadMu.Lock()
	if b.pendingReload == nil {
		b.parkedSaveSeq = b.currentSaveSeq()
	}
	b.pendingReload = newCfg
	b.pendingReloadMu.Unlock()
	if !b.cfgReloadMu.TryLock() {
		if b.logger != nil {
			b.logger.Info("config reload deferred until the governor cycle reaches a safe point")
		}
		return
	}
	b.unlockCfg()
}

// lockCfg acquires cfgReloadMu and applies any parked reload, so the work
// that follows reads the newest config.
func (b *boot) lockCfg() {
	b.cfgReloadMu.Lock()
	b.applyPendingReloadLocked()
}

// unlockCfg applies any parked reload, releases cfgReloadMu and fires the
// ACMM transitions those reloads caused. A reload parked after the last check
// but before the release (its TryLock failed against us) is picked up by the
// loop, so a parked reload is never stranded.
func (b *boot) unlockCfg() {
	for {
		b.applyPendingReloadLocked()
		transitions := b.pendingACMM
		b.pendingACMM = nil
		b.cfgReloadMu.Unlock()
		for _, t := range transitions {
			b.onACMMLevelChanged(t[0], t[1])
		}
		if !b.hasPendingReload() || !b.cfgReloadMu.TryLock() {
			return
		}
	}
}

func (b *boot) hasPendingReload() bool {
	b.pendingReloadMu.Lock()
	defer b.pendingReloadMu.Unlock()
	return b.pendingReload != nil && b.reloadApply != nil
}

func (b *boot) currentSaveSeq() uint64 {
	if b.configSaveSeq != nil {
		return b.configSaveSeq()
	}
	return config.SaveSeq()
}

func (b *boot) saveReloadedConfig() error {
	if b.saveConfig != nil {
		return b.saveConfig()
	}
	return b.cfg.Save()
}

// applyPendingReloadLocked swaps in the parked reload, if any. Caller holds
// cfgReloadMu.
//
// If any config save started while the reload was parked, hive.yaml now
// holds the pre-reload config, not the operator's edit. The reloaded config
// is saved back so disk matches memory; the save arms the watcher's SkipNext
// through the save observer, so it does not trigger another reload.
func (b *boot) applyPendingReloadLocked() {
	if b.reloadApply == nil {
		return
	}
	b.pendingReloadMu.Lock()
	newCfg := b.pendingReload
	parkedSeq := b.parkedSaveSeq
	b.pendingReload = nil
	b.pendingReloadMu.Unlock()
	if newCfg == nil {
		return
	}
	prev, next := b.reloadApply(newCfg)
	if prev != next {
		b.pendingACMM = append(b.pendingACMM, [2]int{prev, next})
	}
	if seq := b.currentSaveSeq(); seq != parkedSeq {
		if b.logger != nil {
			b.logger.Info("config was saved while a reload was pending; saving the reloaded config back to disk",
				"saves_while_pending", seq-parkedSeq)
		}
		if err := b.saveReloadedConfig(); err != nil && b.logger != nil {
			b.logger.Error("failed to save reloaded config back to disk", "error", err)
		}
	}
}

// persistCycleLocked is a governor tick's persist. persistState writes the
// governor snapshot and reach counters and ends in
// Config.ReconcilePausedAndSave, which rewrites hive.yaml from the in-memory
// config. Any parked reload is applied first, so the save writes the
// reloaded config rather than the one the tick started from, and the
// governor state is still saved this tick. Caller holds cfgReloadMu.
func (b *boot) persistCycleLocked(persist func(*boot)) {
	b.applyPendingReloadLocked()
	persist(b)
}
