package main

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/pauseprov"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// pauseProvenanceFile is the durable last-resume record under the hive's data
// root (hivecommons/hive#18916).
const pauseProvenanceFile = "pause-provenance.json"

// installPauseProvenance opens the last-resume store and makes it the
// process's. A corrupt or unreadable file starts it empty with a warning: a
// lost record must not stop the hive.
func installPauseProvenance(cfg *config.Config, logger *slog.Logger) *pauseprov.Store {
	path := ""
	if cfg != nil {
		path = filepath.Join(wfstore.Root(cfg.Data.WorkflowsDir), pauseProvenanceFile)
	}
	s, err := pauseprov.Open(path, logger)
	if err != nil && logger != nil {
		logger.Warn("pause provenance unreadable; starting empty", "path", path, "error", err)
	}
	pauseprov.SetDefault(s)
	dashboard.SetPauseProvenanceSource(pauseProvenanceAdapter{s})
	return s
}

// observePauseTransition files a resume the runtime reported. A pause needs no
// record here: the runtime keeps its trigger, user, reason and time with the
// persisted pause state.
func observePauseTransition(event crux.PauseTransitionEvent) {
	if event.Paused {
		return
	}
	pauseprov.Default().ObserveResume(event.Agent, pauseprov.Event{Trigger: event.Trigger, By: event.By, Reason: event.Reason, At: event.At})
}

// pauseProvenanceAdapter is the dashboard's PauseProvenanceSource, so the
// dashboard takes no import of pkg/pauseprov.
type pauseProvenanceAdapter struct{ s *pauseprov.Store }

func (a pauseProvenanceAdapter) Describe(agent string, paused bool, trigger, by, reason string, at time.Time) (any, string) {
	p := pauseprov.Describe(paused, pauseprov.PauseFacts{Trigger: trigger, By: by, Reason: reason, At: at}, a.s.LastResume(agent))
	if p == nil {
		return nil, ""
	}
	return p, pauseprov.RefusalMessage(agent, p)
}

func (a pauseProvenanceAdapter) IntendResume(agent, by string) { a.s.IntendResume(agent, by) }
