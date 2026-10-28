package autoscale

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Cgroup paths the resource probe reads (cgroup v2, which every supported
// pod runtime mounts). Missing files mean Resources.Known=false.
const (
	defaultCgroupRoot = "/sys/fs/cgroup"
	cpuMaxFile        = "cpu.max"
	cpuStatFile       = "cpu.stat"
	memMaxFile        = "memory.max"
	memCurrentFile    = "memory.current"
	cpuStatUsageKey   = "usage_usec"
	microsPerSecond   = 1e6
)

// Prober reads the pod's cgroup budget once per cycle. CPU use is the
// usage_usec delta since the previous call divided by wall time, so the first
// call reports zero use and nothing ever sleeps.
type Prober struct {
	root      string
	lastUsage float64
	lastAt    time.Time
}

// NewProber reads from the default cgroup root.
func NewProber() *Prober { return &Prober{root: defaultCgroupRoot} }

// NewProberAt reads from root; tests point it at a fixture directory.
func NewProberAt(root string) *Prober { return &Prober{root: root} }

// Probe returns the current budget. now is the sample time.
func (p *Prober) Probe(now time.Time) Resources {
	if p == nil {
		return Resources{}
	}
	var res Resources
	if limit, ok := p.cpuLimit(); ok {
		res.Known = true
		res.CPULimit = limit
	}
	if usage, ok := p.cpuUsageSeconds(); ok {
		if !p.lastAt.IsZero() {
			if wall := now.Sub(p.lastAt).Seconds(); wall > 0 {
				res.CPUUsed = (usage - p.lastUsage) / wall
				if res.CPUUsed < 0 {
					res.CPUUsed = 0
				}
			}
		}
		p.lastUsage, p.lastAt = usage, now
	}
	if limitMB, set, ok := p.memLimitMB(); ok {
		res.Known = true
		res.MemLimitSet = set
		res.MemLimitMB = limitMB
	}
	if cur, ok := p.readUint(memCurrentFile); ok {
		res.MemUsedMB = int(cur / mibBytes)
	}
	return res
}

func (p *Prober) read(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(p.root, name))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func (p *Prober) readUint(name string) (uint64, bool) {
	s, ok := p.read(name)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// cpuLimit parses cpu.max ("quota period" or "max period") into cores.
func (p *Prober) cpuLimit() (float64, bool) {
	s, ok := p.read(cpuMaxFile)
	if !ok {
		return 0, false
	}
	fields := strings.Fields(s)
	if len(fields) < 2 || fields[0] == "max" {
		return 0, false
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || period <= 0 {
		return 0, false
	}
	return quota / period, true
}

func (p *Prober) cpuUsageSeconds() (float64, bool) {
	s, ok := p.read(cpuStatFile)
	if !ok {
		return 0, false
	}
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == cpuStatUsageKey {
			v, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return 0, false
			}
			return v / microsPerSecond, true
		}
	}
	return 0, false
}

// memLimitMB parses memory.max; "max" means unlimited (set=false).
func (p *Prober) memLimitMB() (limitMB int, set bool, ok bool) {
	s, found := p.read(memMaxFile)
	if !found {
		return 0, false, false
	}
	if s == "max" {
		return 0, false, true
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, false
	}
	return int(v / mibBytes), true, true
}
