package proxy

import (
	"os"

	"github.com/hivecommons/hive/crux/agent"
)

// loadedUIDMap returns the UID map the proxy currently attributes callers
// with, or nil when none was loaded at construction. Every reader goes through
// here because refreshUIDMap may swap the pointer while requests are in
// flight.
func (p *GitHubProxy) loadedUIDMap() *agent.UIDMap {
	p.uidMapMu.RLock()
	defer p.uidMapMu.RUnlock()
	return p.uidMap
}

// refreshUIDMap re-reads uid-map.json when it changed since the proxy last
// loaded it and returns the new map, or nil when nothing changed or no reload
// is possible.
//
// WHY (#18184). The manager allocates a UID for an agent added at runtime
// (AddAgent, ReconcileAgents) and saves the map, but the proxy used to read the
// file once at construction. Until a restart such an agent's own socket was
// refused as uid_not_mapped, and — worse — LookupByName returned 0 for it, so
// the #18133 check that stops a proxy-UID caller from claiming a UID-isolated
// agent's name accepted the claim. The file, not the manager's in-memory map,
// is the source here because in sidecar mode the manager runs in another
// process.
//
// The callers reload only on a lookup MISS, so a known agent costs nothing
// and an unknown UID or name costs one stat. A map absent at construction
// stays absent (uidMapPath is empty): that is the no-entrypoint deployment
// whose ADVISORY semantics must not flip mid-run. An unreadable or malformed
// file keeps the previous map; its stamp is still recorded so a bad file is
// warned about once per change rather than on every request.
func (p *GitHubProxy) refreshUIDMap() *agent.UIDMap {
	if p.uidMapPath == "" {
		return nil
	}
	p.uidMapReloadMu.Lock()
	defer p.uidMapReloadMu.Unlock()

	info, err := os.Stat(p.uidMapPath)
	if err != nil {
		return nil
	}
	if prev := p.uidMapInfo; prev != nil && os.SameFile(prev, info) &&
		prev.ModTime().Equal(info.ModTime()) && prev.Size() == info.Size() {
		return nil
	}
	p.uidMapInfo = info
	loaded, err := agent.LoadUIDMap(p.uidMapPath)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("proxy: UID map changed but could not be reloaded; keeping the previous map", "path", p.uidMapPath, "error", err)
		}
		return nil
	}
	p.uidMapMu.Lock()
	p.uidMap = loaded
	p.uidMapMu.Unlock()
	if p.logger != nil {
		p.logger.Info("proxy reloaded UID map", "path", p.uidMapPath, "agents", len(loaded.Agents))
	}
	return loaded
}

// lookupAgentByUID names the agent that owns uid, reloading the UID map once
// when uid is not in it so an agent added at runtime is attributed without a
// restart. Returns "" when no map is loaded or uid belongs to no agent.
func (p *GitHubProxy) lookupAgentByUID(uid int) string {
	m := p.loadedUIDMap()
	if m == nil {
		return ""
	}
	if name := m.LookupByUID(uid); name != "" {
		return name
	}
	if fresh := p.refreshUIDMap(); fresh != nil {
		return fresh.LookupByUID(uid)
	}
	return ""
}

// lookupUIDByName returns the per-agent UID allocated to name, reloading the
// UID map once on a miss (see lookupAgentByUID). Returns 0 when no map is
// loaded or name has no per-agent UID.
func (p *GitHubProxy) lookupUIDByName(name string) int {
	m := p.loadedUIDMap()
	if m == nil {
		return 0
	}
	if uid := m.LookupByName(name); uid > 0 {
		return uid
	}
	if fresh := p.refreshUIDMap(); fresh != nil {
		return fresh.LookupByName(name)
	}
	return 0
}
