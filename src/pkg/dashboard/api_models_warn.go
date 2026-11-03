package dashboard

import (
	"log/slog"
	"sync"
	"time"
)

// noModelsWarnInterval is how long an empty-discovery warning for one backend
// stays quiet after it fires. The dashboard re-polls the model list, so a
// persistent condition would otherwise WARN on every poll and trip the fleet
// report's repeated-error catch-all.
const noModelsWarnInterval = time.Hour

type noModelsWarnTracker struct {
	mu   sync.Mutex
	last map[string]time.Time
	now  func() time.Time
}

var noModelsWarned = &noModelsWarnTracker{last: map[string]time.Time{}, now: time.Now}

// shouldWarn reports whether backend's warning is due, and records it if so.
func (t *noModelsWarnTracker) shouldWarn(backend string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if last, ok := t.last[backend]; ok && now.Sub(last) < noModelsWarnInterval {
		return false
	}
	t.last[backend] = now
	return true
}

func (t *noModelsWarnTracker) clear(backend string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, backend)
}

func warnNoModelsOnce(logger *slog.Logger, backend string, endpoints int) {
	if logger == nil {
		return
	}
	if noModelsWarned.shouldWarn(backend) {
		logger.Warn("no models found from any endpoint", "backend", backend, "endpoints", endpoints)
		return
	}
	logger.Debug("no models found from any endpoint", "backend", backend, "endpoints", endpoints)
}
