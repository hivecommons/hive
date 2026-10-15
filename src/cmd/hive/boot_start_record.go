package main

import (
	"log/slog"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/logread"
	"github.com/hivecommons/hive/pkg/startrec"
)

// withStartID is the one place both hive loggers (the early stdout logger
// and the configured file logger) gain the start identifier (#11945). It is
// applied to whatever deps.fileLogger returns, so a replaced file logger
// carries it too.
func withStartID(logger *slog.Logger, id string) *slog.Logger {
	if logger == nil || id == "" {
		return logger
	}
	return logger.With(startrec.LogKey, id)
}

// beginStartRecord records this start and how the previous run ended
// (#11945), starts its heartbeat and pushes its close on the cleanup stack.
// A start record that cannot be written is logged and never stops the boot.
func (b *boot) beginStartRecord(deps bootConfigDeps) {
	if deps.beginStart == nil {
		return
	}
	rec, err := deps.beginStart(b.startID, deps.getenv)
	if err != nil {
		b.logger.Warn("start record not written; the next start cannot say how this run ended", "error", err)
	}
	if rec == nil {
		return
	}
	if cerr := rec.CrashOutputErr(); cerr != nil {
		b.logger.Warn("crash output not kept; a crash of this run leaves no trace in the crash log source", "error", cerr)
	}
	b.startRecord = rec
	prev := rec.Previous()
	b.logger.Info("hive start recorded",
		"previous_run", prev.Verdict,
		"previous_start", prev.Start,
		"previous_last_seen_alive", prev.LastSeenAlive,
		"previous_crash_trace", prev.CrashTrace,
	)
	rec.StartHeartbeat(startrec.HeartbeatInterval)
	b.cleanup.push(func() {
		if err := rec.Close(startrec.VerdictClean); err != nil {
			b.logger.Warn("start record not closed", "error", err)
		}
	})
}

// recordStart is the production beginStart: the start record lives in the
// output log directory, with the container's memory reading.
func recordStart(id string, getenv func(string) string) (*startrec.Recorder, error) {
	return startrec.Begin(startrec.Options{
		Dir:    logread.OutputDir(getenv),
		ID:     id,
		Memory: containerMemoryReading,
	})
}

// containerMemoryReading is the reading fleet status reports, kept with the
// start record so it survives a container restart.
func containerMemoryReading() (startrec.MemoryReading, bool) {
	ev := dashboard.ReadMemoryEvidence()
	return startrec.MemoryReading{
		UsedBytes:  ev.UsedBytes,
		Limit:      ev.Limit,
		LimitBytes: ev.LimitBytes,
		OOMKills:   ev.OOMKills,
	}, true
}
