//go:build !windows

package integrated

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func tryLockProductionRunLease(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return false, nil
	}
	return err == nil, err
}

func unlockProductionRunLease(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func legacyProductionRunOwnerMatches(pid int, startedAt time.Time) bool {
	if pid <= 0 || startedAt.IsZero() {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	processStartedAt, ok := legacyProductionRunProcessStartedAt(pid)
	if !ok {
		return true // a live, uninspectable legacy owner must fail closed
	}
	// A v1 owner necessarily existed before it wrote StartedAt. A process with
	// the same PID that began later is a reused PID, regardless of the binary's
	// filename (Hive may be renamed or launched through a packaged wrapper).
	return !processStartedAt.After(startedAt.Add(3 * time.Second))
}

func legacyProductionRunProcessStartedAt(pid int) (time.Time, bool) {
	ps := ""
	for _, candidate := range []string{"/bin/ps", "/usr/bin/ps"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			ps = candidate
			break
		}
	}
	if ps == "" {
		return time.Time{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, ps, "-o", "lstart=", "-o", "etime=", "-p", strconv.Itoa(pid))
	environment := make([]string, 0, len(os.Environ())+2)
	for _, pair := range os.Environ() {
		key, _, _ := strings.Cut(pair, "=")
		if key != "LC_ALL" && key != "LANG" {
			environment = append(environment, pair)
		}
	}
	command.Env = append(environment, "LC_ALL=C", "LANG=C")
	observedAt := time.Now()
	output, err := command.Output()
	if err != nil {
		return time.Time{}, false
	}
	fields := strings.Fields(string(output))
	if len(fields) != 6 {
		return time.Time{}, false
	}
	started, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(fields[:5], " "), time.Local)
	if err != nil {
		return time.Time{}, false
	}
	elapsed, ok := parseLegacyProcessElapsed(fields[5])
	if !ok {
		return time.Time{}, false
	}
	estimated := observedAt.Add(-elapsed)
	drift := started.Sub(estimated)
	if drift < 0 {
		drift = -drift
	}
	// lstart is wall-clock based while etime is elapsed time. Requiring them to
	// agree prevents a clock or timezone discontinuity from being mistaken for
	// safe PID reuse. Ambiguity deliberately fails closed.
	if drift > 2*time.Minute || started.After(time.Now().Add(time.Minute)) {
		return time.Time{}, false
	}
	return started.UTC(), true
}

func parseLegacyProcessElapsed(value string) (time.Duration, bool) {
	dayCount := int64(0)
	clock := value
	if before, after, found := strings.Cut(value, "-"); found {
		parsed, err := strconv.ParseInt(before, 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		dayCount, clock = parsed, after
	}
	parts := strings.Split(clock, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, false
	}
	values := make([]int64, len(parts))
	for index, part := range parts {
		parsed, err := strconv.ParseInt(part, 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		values[index] = parsed
	}
	hours := int64(0)
	minutes, seconds := values[0], values[1]
	if len(values) == 3 {
		hours, minutes, seconds = values[0], values[1], values[2]
	}
	if minutes >= 60 || seconds >= 60 || dayCount > 36500 || hours > 24*36500 {
		return 0, false
	}
	totalSeconds := (((dayCount*24)+hours)*60+minutes)*60 + seconds
	if totalSeconds < 0 || totalSeconds > int64((100*365*24*time.Hour)/time.Second) {
		return 0, false
	}
	return time.Duration(totalSeconds) * time.Second, true
}
