package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// cgroupOOMKillFiles are the files, in preference order, that expose the
// cumulative count of processes the kernel OOM-killed inside this container's
// memory cgroup: cgroup v2 memory.events, then the cgroup v1 oom_control. Both
// carry an "oom_kill N" line. When the pod's memory limit is hit the kernel
// kills the largest process — almost always an agent CLI — without restarting
// the container, so from hive's side it looks like an ordinary CLI crash.
var cgroupOOMKillFiles = []string{
	"/sys/fs/cgroup/memory.events",
	"/sys/fs/cgroup/memory/memory.oom_control",
}

// readCgroupOOMKills returns the cumulative oom_kill counter from the first
// readable file in files, and false when none exposes it.
func readCgroupOOMKills(files []string) (int, bool) {
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		n, ok := parseOOMKillLine(f)
		f.Close()
		if ok {
			return n, true
		}
	}
	return 0, false
}

func parseOOMKillLine(f *os.File) (int, bool) {
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "oom_kill" {
			n, err := strconv.Atoi(fields[1])
			return n, err == nil
		}
	}
	return 0, false
}

// noteOOMKillsForCrashes compares the cgroup oom_kill counter with the value
// seen on the previous check. A rise while crashed agents were just found
// marks each of them OOM-suspect and logs the true cause, so the operator
// sees "memory limit" instead of an unexplained crash. The first observation
// only primes the baseline.
func (m *Manager) noteOOMKillsForCrashes(crashed []string) {
	n, ok := readCgroupOOMKills(m.oomKillFiles())
	if !ok {
		return
	}
	m.oomMu.Lock()
	defer m.oomMu.Unlock()
	primed, prev := m.oomKillsPrimed, m.oomKillsSeen
	m.oomKillsPrimed, m.oomKillsSeen = true, n
	if !primed || n <= prev || len(crashed) == 0 {
		return
	}
	if m.crashOOMSuspect == nil {
		m.crashOOMSuspect = map[string]bool{}
	}
	for _, name := range crashed {
		m.crashOOMSuspect[name] = true
	}
	m.logger.Warn("agent crash coincides with cgroup OOM kills: the container hit its memory limit",
		"agents", crashed,
		"oom_kills_before", prev,
		"oom_kills_now", n,
	)
}

func (m *Manager) oomKillFiles() []string {
	if m.oomKillFilesOverride != nil {
		return m.oomKillFilesOverride
	}
	return cgroupOOMKillFiles
}

// CrashOOMSuspected reports whether name's most recent crash coincided with a
// cgroup OOM kill, and consumes the flag so one crash yields one hint.
func (m *Manager) CrashOOMSuspected(name string) bool {
	m.oomMu.Lock()
	defer m.oomMu.Unlock()
	if !m.crashOOMSuspect[name] {
		return false
	}
	delete(m.crashOOMSuspect, name)
	return true
}
