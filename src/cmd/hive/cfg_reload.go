package main

import "github.com/hivecommons/hive/pkg/config"

// submitConfigReload is the config watcher's callback. The governor loop
// holds cfgReloadMu for a whole tick (minutes on a busy spoke), so waiting
// for it starved hot-reload and let the tick's persist rewrite hive.yaml from
// the stale config (hivecommons/hive#17467). The reload is parked instead:
// applied now when the lock is free, otherwise by the holder at its next safe
// point. Only the newest parked reload is applied; each one is a full file.
func (b *boot) submitConfigReload(newCfg *config.Config) {
	b.pendingReloadMu.Lock()
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

// applyPendingReloadLocked swaps in the parked reload, if any. Caller holds
// cfgReloadMu.
func (b *boot) applyPendingReloadLocked() {
	if b.reloadApply == nil {
		return
	}
	b.pendingReloadMu.Lock()
	newCfg := b.pendingReload
	b.pendingReload = nil
	b.pendingReloadMu.Unlock()
	if newCfg == nil {
		return
	}
	prev, next := b.reloadApply(newCfg)
	b.cfgGeneration++
	if prev != next {
		b.pendingACMM = append(b.pendingACMM, [2]int{prev, next})
	}
}

// persistCycleLocked is a governor tick's persist. It first applies any
// parked reload, then skips the save when a reload landed since the tick
// started at cycleGen: the tick's state was built against the replaced
// config, and the operator's edit must reach disk untouched. The next tick
// persists from the reloaded config. Caller holds cfgReloadMu.
func (b *boot) persistCycleLocked(persist func(*boot), cycleGen uint64) {
	b.applyPendingReloadLocked()
	if b.cfgGeneration != cycleGen {
		if b.logger != nil {
			b.logger.Info("config reloaded during governor cycle; skipping this cycle's persist so the reload is not overwritten",
				"cycle_generation", cycleGen, "generation", b.cfgGeneration)
		}
		return
	}
	persist(b)
}
