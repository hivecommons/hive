package github

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Sandbox-side view of write_surface.enforce (hivecommons/hive#9587).
//
// The GitHub proxy is where an enforced lane's direct writes are refused
// (pkg/proxy/write_surface_enforce.go, #9772). That refusal only covers
// traffic that goes THROUGH the proxy: a sandbox whose forced egress is off,
// or a tool holding a token of its own, never reaches it. The file published
// here carries the same per-lane decision into the sandbox, where the `gh`
// wrapper refuses a direct write before it is ever sent.
//
// It is a second lock on the same door, not a new policy: the list is derived
// from the operator's write_surface.enforce, so a hive that lists no lane
// publishes an empty list and nothing changes. The file is written by the hive
// process into a directory no agent UID may write, and the wrapper only ever
// reads it.

const (
	// WriteSurfaceEnforceAll is the lane entry that enforces every lane. It
	// mirrors config.WriteSurfaceAllowAll, spelled out here so the published
	// file and the wrapper that reads it share one vocabulary.
	WriteSurfaceEnforceAll = "*"

	// WriteSurfaceEnforceFilePath is where the hive publishes the resolved
	// enforced-lane list for the agent sandbox. It is a constant for the same
	// reason the wrapper's copy of it is: a path an agent could redirect
	// (through the environment) would let an enforced lane point the check at
	// a file it controls.
	WriteSurfaceEnforceFilePath = "/var/run/hive-metrics/write-surface-enforce.json"

	// WriteSurfaceEnforceFileVersion is the schema version of the published
	// file. A reader that does not recognize the version treats the file as
	// carrying no lanes (enforcement stays off) rather than guessing.
	WriteSurfaceEnforceFileVersion = 1

	// writeSurfaceEnforceFilePerms make the file readable by every agent UID
	// and writable by none: the wrapper in each sandbox reads it, and a list
	// the enforced party could edit would enforce nothing.
	writeSurfaceEnforceFilePerms = 0o644
)

// WriteSurfaceEnforceFile is the published document. Lanes holds the RESOLVED
// lane names (each configured agent and replica the operator's list enforces),
// so the sandbox matches a name instead of re-implementing replica resolution.
// A literal "*" is kept as-is and means every lane.
type WriteSurfaceEnforceFile struct {
	Version   int      `json:"version"`
	UpdatedAt string   `json:"updated_at"`
	Lanes     []string `json:"lanes"`
}

// NormalizeEnforcedLanes returns lanes trimmed, lower-cased, de-duplicated and
// sorted, dropping empties. A list containing "*" collapses to ["*"]: it
// already covers every lane, and keeping the rest would only invite a reader
// to believe the named ones are special.
func NormalizeEnforcedLanes(lanes []string) []string {
	seen := make(map[string]bool, len(lanes))
	out := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		lane = strings.ToLower(strings.TrimSpace(lane))
		if lane == "" || seen[lane] {
			continue
		}
		if lane == WriteSurfaceEnforceAll {
			return []string{WriteSurfaceEnforceAll}
		}
		seen[lane] = true
		out = append(out, lane)
	}
	sort.Strings(out)
	return out
}

// PublishWriteSurfaceEnforce writes the enforced-lane list to path, replacing
// whatever was there. It is called at boot and on every config reload, so an
// operator listing (or unlisting) a lane reaches the sandboxes without a
// restart.
//
// The file is ALWAYS written, including when no lane is enforced: an empty
// list is the authoritative "nothing is enforced here", and publishing it is
// what lets an operator's removal of a lane take effect. Write-temp-then-
// rename so a wrapper reading concurrently sees a complete document, never a
// truncated one.
func PublishWriteSurfaceEnforce(path string, lanes []string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	doc := WriteSurfaceEnforceFile{
		Version:   WriteSurfaceEnforceFileVersion,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Lanes:     NormalizeEnforcedLanes(lanes),
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	_, werr := tmp.Write(data)
	if werr == nil {
		// Chmod through the open descriptor, not the path: CreateTemp makes
		// the file 0600 and a path-based chmod after Close could be applied to
		// a swapped pathname.
		werr = tmp.Chmod(writeSurfaceEnforceFilePerms)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), path)
}
