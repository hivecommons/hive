package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// prWarnOnce picks Warn the first time a per-PR condition is seen and Debug
// while it persists. Retain re-arms keys whose condition cleared, so a later
// recurrence warns again.
type prWarnOnce struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func prWarnKey(repo string, pr int) string {
	return strings.ToLower(fmt.Sprintf("%s#%d", repo, pr))
}

func (w *prWarnOnce) level(key string) slog.Level {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.seen[key]; ok {
		return slog.LevelDebug
	}
	if w.seen == nil {
		w.seen = map[string]struct{}{}
	}
	w.seen[key] = struct{}{}
	return slog.LevelWarn
}

// retain forgets every key not in active.
func (w *prWarnOnce) retain(active map[string]struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.seen {
		if _, ok := active[k]; !ok {
			delete(w.seen, k)
		}
	}
}

var (
	humanLabelWarn     prWarnOnce
	reviewExpiredWarn  prWarnOnce
	reviewPromotedWarn prWarnOnce
)
