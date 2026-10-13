package main

import (
	"path/filepath"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousdeps"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// nousStatusDependenciesKey is the GET /api/nous/status key under which the
// pinned-dependency settings are surfaced (RFC #11585 part 7, #11700).
const nousStatusDependenciesKey = "dependencies"

// publishNousDependencyStatus adds the resolved fetch allowlist, cache
// location and record path to the Nous status map so an operator can see
// from the dashboard what `hive nous-setup` will enforce. It adds a key only;
// every existing /api/nous/status field is unchanged.
func publishNousDependencyStatus(state *dashboard.NousState, layout nousstore.Layout, allowlist []string) {
	if state == nil {
		return
	}
	if allowlist == nil {
		allowlist = []string{}
	}
	state.Mu.Lock()
	defer state.Mu.Unlock()
	if state.Status == nil {
		state.Status = map[string]interface{}{}
	}
	state.Status[nousStatusDependenciesKey] = map[string]interface{}{
		"fetch_allowlist": allowlist,
		"cache_dir":       filepath.Join(layout.Root, nousdeps.CacheSubdir),
		"record_file":     nousdeps.RecordPath(layout.Root, nousstore.GovernorSubdir),
		"setup_command":   "hive " + nousdeps.Subcommand,
	}
}
