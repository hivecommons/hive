package main

import (
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// writeSurfaceEnforcedLanes resolves write_surface.enforce against the
// configured agents and returns the lane names whose direct sandbox writes are
// refused (hivecommons/hive#9587).
//
// Resolution happens HERE, not in the sandbox: config decides what a replica
// ("scanner-2") inherits from its base agent and how a name is matched, and a
// shell re-implementation of that would be a second answer to the same
// question. The sandbox only compares its own lane name against this list.
//
// An enforce list containing "*" is published as "*" rather than expanded, so
// a lane the process table has not seen yet (an agent added between reloads)
// is still enforced.
func writeSurfaceEnforcedLanes(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	for _, lane := range cfg.WriteSurfaceEnforceLanes() {
		if strings.TrimSpace(lane) == github.WriteSurfaceEnforceAll {
			return []string{github.WriteSurfaceEnforceAll}
		}
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	lanes := make([]string, 0, len(names))
	for _, name := range names {
		if cfg.WriteSurfaceEnforced(name) {
			lanes = append(lanes, name)
		}
	}
	return lanes
}

// publishWriteSurfaceEnforce hands the resolved enforced-lane list to the
// agent sandboxes, where the gh wrapper refuses a direct write for a listed
// lane even if the request would never have reached the GitHub proxy.
//
// Best effort by design: the proxy refusal is the enforcement this feature
// rests on, and a hive that cannot publish the file must keep running rather
// than fail to boot over a defence-in-depth copy. The failure is logged with
// the path so an operator can see the sandbox check is not armed.
func (b *boot) publishWriteSurfaceEnforce() {
	lanes := writeSurfaceEnforcedLanes(b.cfg)
	if err := github.PublishWriteSurfaceEnforce(github.WriteSurfaceEnforceFilePath, lanes); err != nil {
		b.logger.Warn("cannot publish write_surface.enforce for the agent sandbox — the gh wrapper's direct-write refusal will not be armed (the GitHub proxy still refuses)",
			"path", github.WriteSurfaceEnforceFilePath, "lanes", len(lanes), "error", err)
		return
	}
	b.logger.Info("write surface enforcement published", "path", github.WriteSurfaceEnforceFilePath, "lanes", lanes)
}
