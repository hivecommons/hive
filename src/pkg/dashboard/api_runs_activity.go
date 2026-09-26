package dashboard

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/pkg/timeline"
)

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
	latestAt, latestEvent := newestRunActivityEvent(parseRunTime(run.LastActivity), run.ActivitySummary)
	if !lease.leaseHeartbeat.IsZero() && lease.leaseHeartbeat.After(latestAt) {
		latestAt, latestEvent = lease.leaseHeartbeat, "lease renewed"
	}
	if receiptAt, receiptEvent := newestRunReceiptActivity(run.Key, run.Stage); receiptAt.After(latestAt) {
		latestAt, latestEvent = receiptAt, receiptEvent
	}
	if provider := s.runActivityProvider(); provider != nil {
		if snap, ok := provider.RunActivitySnapshot(run.Key, run.Stage, run.Gen); ok {
			if !snap.stageStartedAt.IsZero() {
				out.StageStartedAt = formatRunTime(snap.stageStartedAt)
				out.ElapsedSeconds = elapsedSeconds(snap.stageStartedAt, now)
			}
			if snap.lastActivityAt.After(latestAt) {
				latestAt, latestEvent = snap.lastActivityAt, snap.lastEvent
			}
			out.AgentPIDAlive = processAlive(snap.agentPID)
			out.Alive = out.Alive || out.AgentPIDAlive || !snap.lastActivityAt.IsZero()
		}
	}
	if latestAt.IsZero() {
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
