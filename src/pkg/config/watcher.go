package config

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceDelay is the time to wait after a file change event before
// reloading, to avoid reloading mid-write (e.g. atomic rename has
// multiple events).
const debounceDelay = 500 * time.Millisecond

// Watcher monitors a config file for changes and calls onChange with
// the freshly loaded Config when the file is modified.
type Watcher struct {
	path     string
	onChange func(*Config)
	logger   *slog.Logger

	mu       sync.Mutex
	timer    *time.Timer
	stopped  bool
	skipNext bool
}

// SkipNext tells the watcher to ignore the next file change event.
// Call this before a programmatic Config.Save() to prevent the watcher
// from reloading the file we just wrote (the in-memory config is already
// correct, and a reload could race with concurrent mutations).
func (w *Watcher) SkipNext() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.skipNext = true
}

// NewWatcher creates a Watcher that will reload the config at path
// whenever the file changes on disk, then invoke onChange with the
// new Config.
func NewWatcher(path string, onChange func(*Config), logger *slog.Logger) *Watcher {
	return &Watcher{
		path:     path,
		onChange: onChange,
		logger:   logger,
	}
}

// Start begins watching the config file. It blocks until ctx is
// cancelled or a fatal watcher error occurs. Callers should run
// this in a goroutine.
func (w *Watcher) Start(ctx context.Context) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.logger.Error("failed to create fsnotify watcher", "error", err)
		return
	}
	defer func() { _ = fsw.Close() }() // best-effort; process/goroutine is tearing down either way

	if err := fsw.Add(w.path); err != nil {
		w.logger.Error("failed to watch config file", "path", w.path, "error", err)
		return
	}

	w.logger.Info("config file watcher started", "path", w.path)

	for {
		select {
		case <-ctx.Done():
			w.cancelTimer()
			return

		case event, ok := <-fsw.Events:
			if !ok {
				w.logger.Warn("config watcher: events channel closed")
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			w.scheduleReload()

		case err, ok := <-fsw.Errors:
			if !ok {
				w.logger.Warn("config watcher: errors channel closed")
				return
			}
			w.logger.Warn("config watcher error", "error", err)
		}
	}
}

// scheduleReload debounces reload calls: it resets the timer each
// time a new event arrives, so the reload fires only after
// debounceDelay of quiet.
func (w *Watcher) scheduleReload() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stopped {
		return
	}

	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(debounceDelay, w.reload)
}

func (w *Watcher) cancelTimer() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *Watcher) reload() {
	w.mu.Lock()
	if w.skipNext {
		w.skipNext = false
		w.mu.Unlock()
		w.logger.Debug("config reload skipped (programmatic save)")
		return
	}
	w.mu.Unlock()

	// Load the seed AND re-apply the dashboard overlay, mirroring the
	// entrypoint's boot merge. A raw Load(w.path) would drop runtime-reconciled
	// agent fields (kick_template/mode/model persisted to the overlay by
	// ApplyPack) whenever a ConfigMap remount rewrites the seed and fires this
	// watcher — silently reverting a hive's agents to their provision-time
	// (lower-ACMM) behavior.
	cfg, err := LoadWithDashboardOverlay(w.path)
	if err != nil {
		w.logger.Warn("config reload failed", "path", w.path, "error", err)
		return
	}

	w.logger.Info("config reloaded from file", "path", w.path)
	w.onChange(cfg)
}
