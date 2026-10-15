package dashboard

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Out-of-memory evidence (hivecommons/hive#11945). The fleet status read
// (GET /api/status, the admin MCP fleet_status tool) reports the container's
// own memory limit and out-of-memory kill count beside the existing memory
// fields, so an operator with no host shell can tell a hive that ran out of
// memory. The start record (pkg/startrec) keeps the last reading taken
// before a run ended, because a restarted container's counters start again.
//
// The agent manager's cgroup override does not reach this collector; this
// one has its own seam, memoryEvidenceRoot.

const (
	// MemLimitUnlimited is the memLimit value when the container has no
	// memory limit; MemLimitUnknown when no memory controller is readable.
	MemLimitUnlimited = "unlimited"
	MemLimitUnknown   = "unknown"

	cgroupMemEventsName = "memory.events"
	cgroupV1Dir         = "memory"
	cgroupV1OOMControl  = "memory.oom_control"
	cgroupV1LimitName   = "memory.limit_in_bytes"
	cgroupV1UsageName   = "memory.usage_in_bytes"
	cgroupMemMaxName    = "memory.max"
	cgroupMemCurName    = "memory.current"
	oomKillKey          = "oom_kill"
)

// memoryEvidenceRoot is the cgroup mount the evidence is read from; tests
// point it at a directory of fixture files.
var memoryEvidenceRoot = "/sys/fs/cgroup"

// MemoryEvidence is one reading of the container's memory controller.
type MemoryEvidence struct {
	// UsedBytes is the container's current memory use; 0 when unreadable.
	UsedBytes int64
	// Limit is MemLimitUnlimited, MemLimitUnknown or a set limit such as
	// "2048 MiB"; LimitBytes is a set limit in bytes, else 0.
	Limit      string
	LimitBytes int64
	// OOMKills is the out-of-memory kill count; nil when not reported.
	OOMKills *int64
}

// ReadMemoryEvidence reads the hive container's memory controller.
func ReadMemoryEvidence() MemoryEvidence { return readMemoryEvidence(memoryEvidenceRoot) }

// readMemoryEvidence reads cgroup v2 files under root, falling back to the
// cgroup v1 memory controller under root/memory.
func readMemoryEvidence(root string) MemoryEvidence {
	ev := MemoryEvidence{Limit: MemLimitUnknown}
	if limit, unlimited, ok := readLimitFile(filepath.Join(root, cgroupMemMaxName)); ok {
		ev.setLimit(limit, unlimited)
		ev.UsedBytes = max(readCgroupInt64(filepath.Join(root, cgroupMemCurName)), 0)
		ev.OOMKills = readKeyedCount(filepath.Join(root, cgroupMemEventsName), oomKillKey)
		return ev
	}
	v1 := filepath.Join(root, cgroupV1Dir)
	if limit, unlimited, ok := readLimitFile(filepath.Join(v1, cgroupV1LimitName)); ok {
		ev.setLimit(limit, unlimited)
		ev.UsedBytes = max(readCgroupInt64(filepath.Join(v1, cgroupV1UsageName)), 0)
		ev.OOMKills = readKeyedCount(filepath.Join(v1, cgroupV1OOMControl), oomKillKey)
	}
	return ev
}

func (ev *MemoryEvidence) setLimit(limit int64, unlimited bool) {
	if unlimited {
		ev.Limit = MemLimitUnlimited
		return
	}
	ev.Limit = strconv.FormatInt(limit/int64(bytesPerMB), 10) + " MiB"
	ev.LimitBytes = limit
}

// readLimitFile reads a memory limit file: "max", or a value at or above
// the cgroup "no limit" sentinels, is unlimited.
func readLimitFile(path string) (limit int64, unlimited, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, true, true
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, false, false
	}
	if v >= maxCgroupMemBytes || v >= cgroupV1UnlimitedMemBytes {
		return 0, true, true
	}
	return v, false, true
}

// readKeyedCount reads "<key> <n>" from a flat-keyed cgroup file such as
// memory.events or memory.oom_control.
func readKeyedCount(path, key string) *int64 {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || fields[0] != key {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || v < 0 {
			return nil
		}
		return &v
	}
	return nil
}
