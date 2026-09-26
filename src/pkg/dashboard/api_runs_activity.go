package dashboard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/timeline"
)

const runActivityLogTailBytes = 16 * 1024
const runActivityLastLineMax = 120

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

type runActivitySignal struct {
	stageStartedAt time.Time
	lastActivityAt time.Time
	lastEvent      string
	agentPID       int
}

type runActivityProvider interface {
	RunActivitySnapshot(runKey, stage string, gen uint64) (runActivitySignal, bool)
}

func (s *Server) runLiveActivity(run Run, lease runLeaseSnapshot, events []timeline.Event, now time.Time) *RunActivity {
	if run.State != "active" || run.Stage == "" {
		return nil
	}
	started := firstNonZeroTime(lease.stageStarted, parseRunTime(run.StageStartedAt), timelineStageTime(events, run.Stage), lease.leaseHeartbeat)
	out := &RunActivity{
		Alive:            run.WaitingOn != RunWaitingOnHuman,
		Phase:            run.Stage,
		StageStartedAt:   formatRunTime(started),
		ElapsedSeconds:   elapsedSeconds(started, now),
		LeaseHeartbeatAt: formatRunTime(lease.leaseHeartbeat),
	}
	var statusAt time.Time
	var statusEvent string
	if provider := s.runActivityProvider(); provider != nil {
		if snap, ok := provider.RunActivitySnapshot(run.Key, run.Stage, run.Gen); ok {
			if !snap.stageStartedAt.IsZero() {
				out.StageStartedAt = formatRunTime(snap.stageStartedAt)
				out.ElapsedSeconds = elapsedSeconds(snap.stageStartedAt, now)
			}
			statusAt, statusEvent = snap.lastActivityAt, snap.lastEvent
			out.AgentPIDAlive = processAlive(snap.agentPID)
			out.Alive = out.Alive || out.AgentPIDAlive || !snap.lastActivityAt.IsZero()
		}
	}
	timelineAt, timelineEvent := newestRunActivityEvent(parseRunTime(run.LastActivity), run.ActivitySummary)
	receiptAt, receiptEvent := newestRunReceiptActivity(run.Key, run.Stage)
	outputAt, outputEvent, outputIdle := newestAgentOutputActivity(lease.identity, run.Key, run.Stage, run.Gen, now)
	out.AgentOutputAt = formatRunTime(outputAt)
	out.AgentOutputIdleSeconds = outputIdle

	var latestAt time.Time
	var latestEvent string
	switch {
	case !outputAt.IsZero():
		latestAt, latestEvent = outputAt, outputEvent
	case !statusAt.IsZero():
		latestAt, latestEvent = statusAt, statusEvent
	case !receiptAt.IsZero():
		latestAt, latestEvent = receiptAt, receiptEvent
	case !timelineAt.IsZero():
		latestAt, latestEvent = timelineAt, timelineEvent
	case !lease.serverSideLease && !lease.leaseHeartbeat.IsZero():
		latestAt, latestEvent = lease.leaseHeartbeat, "relay heartbeat"
	case !lease.leaseHeartbeat.IsZero():
		latestAt, latestEvent = lease.leaseHeartbeat, "hub lease renewed (no agent output)"
	default:
		latestAt, latestEvent = started, "waiting for stage activity"
	}
	out.LastActivityAt = formatRunTime(latestAt)
	out.LastEvent = latestEvent
	return out
}

func (s *Server) runActivityProvider() runActivityProvider {
	if s == nil {
		return nil
	}
	s.stageExecutorMu.Lock()
	defer s.stageExecutorMu.Unlock()
	p, _ := s.stageExecutor.(runActivityProvider)
	return p
}

func newestRunActivityEvent(at time.Time, event string) (time.Time, string) {
	event = strings.TrimSpace(event)
	if event == "" {
		event = "spektacular progress observed"
	}
	return at, event
}

func newestAgentOutputActivity(identity, runKey, stage string, gen uint64, now time.Time) (time.Time, string, int64) {
	logPath := filepath.Join(spekHubRunWorktreePath(identity, runKey), ".hive", fmt.Sprintf("spek-stage-%s-%d.log", sanitizeRunPromptPath(stage), gen))
	info, err := os.Lstat(logPath)
	if err != nil || !info.Mode().IsRegular() {
		return time.Time{}, "", 0
	}
	at := info.ModTime()
	line := lastNonEmptyLogLine(logPath)
	idle := elapsedSeconds(at, now)
	if line == "" {
		return at, fmt.Sprintf("agent output idle %s", shortDuration(idle)), idle
	}
	return at, fmt.Sprintf("agent output idle %s (last: %q)", shortDuration(idle), line), idle
}

func lastNonEmptyLogLine(path string) string {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ""
	}
	f := os.NewFile(uintptr(fd), filepath.Base(path))
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > runActivityLogTailBytes {
		if _, err := f.Seek(-runActivityLogTailBytes, io.SeekEnd); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(ansiEscapeRE.ReplaceAllString(lines[i], ""))
		if line == "" {
			continue
		}
		return truncateRunActivityLine(line, runActivityLastLineMax)
	}
	return ""
}

func truncateRunActivityLine(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + "…"
}

func shortDuration(seconds int64) string {
	if seconds < 60 {
		return strconv.FormatInt(seconds, 10) + "s"
	}
	mins := seconds / 60
	if mins < 60 {
		return strconv.FormatInt(mins, 10) + "m"
	}
	hours := mins / 60
	return strconv.FormatInt(hours, 10) + "h " + strconv.FormatInt(mins%60, 10) + "m"
}

func newestRunReceiptActivity(runKey, stage string) (time.Time, string) {
	dir := filepath.Join(runReceiptsDir, sanitizeReceiptSegment(runKey))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, ""
	}
	prefix := stage + "-gen"
	var newest time.Time
	var newestName string
	var newestSize int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest, newestName, newestSize = info.ModTime(), entry.Name(), info.Size()
		}
	}
	if newest.IsZero() {
		return time.Time{}, ""
	}
	return newest, fmt.Sprintf("wrote %s (%s)", newestName, humanBytes(newestSize))
}

func humanBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	kb := float64(n) / 1024
	if kb < 100 {
		return fmt.Sprintf("%.1f KB", kb)
	}
	return fmt.Sprintf("%.0f KB", kb)
}

func parseRunTime(value string) time.Time {
	if strings.TrimSpace(value) == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}

func elapsedSeconds(started, now time.Time) int64 {
	if started.IsZero() || now.Before(started) {
		return 0
	}
	return int64(now.Sub(started).Seconds())
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
