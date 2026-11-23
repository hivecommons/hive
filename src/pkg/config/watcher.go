package config

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceDelay is the time to wait after a file change event before
// reloading, to avoid reloading mid-write (e.g. atomic rename has
// multiple events).
const debounceDelay = 500 * time.Millisecond

// configMapDataLink is the symlink the kubelet swaps atomically when it
// updates a mounted ConfigMap; the mounted file names never get an event of
// their own.
const configMapDataLink = "..data"

// Watcher monitors a config file (and, on Kubernetes, the dashboard overlay
// merged over it) for changes and calls onChange with the freshly loaded
// Config when one of them is modified by someone other than this process.
//
// It watches the files' DIRECTORIES, not the files (hivecommons/hive#19561).
// Every Save replaces the config file by rename (#17862), and so do editors
// and `mv`; an inotify watch on the file itself follows the old inode, so
// the first save after boot silently ended the watch and every later hand
// edit was never seen. A directory watch sees in-place writes
// (`cat new > file`) as Write and renames onto the file as Create.
//
// Its own writes are recognized by content, not by a "skip the next event"
// flag: a reload is skipped only when every watched file holds exactly what
// this process last wrote to it (see writtenByThisProcess). The old flag was
// armed on every save and consumed by whichever reload fired next, so an
// operator edit landing within the debounce window of a save (or after a
// save whose own event never arrived) was swallowed.
type Watcher struct {
	path     string
	extra    []string
	onChange func(*Config)
	logger   *slog.Logger

	mu      sync.Mutex
	timer   *time.Timer
	stopped bool

	// ready is closed once Start has registered its directory watches (or
	// given up), so tests can write without guessing a start-up delay.
	ready     chan struct{}
	readyOnce sync.Once
}

// SkipNext is kept for callers that still announce programmatic saves. It
// is a no-op: the watcher now recognizes its own writes by content
// (hivecommons/hive#19561), which cannot swallow a concurrent operator edit.
func (w *Watcher) SkipNext() {}

// NewWatcher creates a Watcher that will reload the config at path
// whenever the file changes on disk, then invoke onChange with the
// new Config.
func NewWatcher(path string, onChange func(*Config), logger *slog.Logger) *Watcher {
	return &Watcher{
		path:     path,
		onChange: onChange,
		logger:   logger,
		ready:    make(chan struct{}),
	}
}

func (w *Watcher) markReady() { w.readyOnce.Do(func() { close(w.ready) }) }

// AlsoWatch adds a file whose changes also trigger a reload of the config.
// Call before Start. Start adds the dashboard overlay itself on Kubernetes.
func (w *Watcher) AlsoWatch(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = append(w.extra, path)
}

// Trigger schedules a reload as if the file had changed. Save calls it
// (through SetExternalChangeObserver) when it refuses to overwrite a file
// that changed on disk, so the edit is loaded even if no event arrived.
func (w *Watcher) Trigger() {
	w.scheduleReload()
}

// watchedFiles returns the watched file paths: the config path, its
// symlink target, and the extra files, cleaned and de-duplicated.
func (w *Watcher) watchedFiles() []string {
	w.mu.Lock()
	extra := append([]string(nil), w.extra...)
	w.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, p := range append([]string{w.path, ResolvedConfigPath(w.path)}, extra...) {
		p = filepath.Clean(p)
		if p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// relevant reports whether a directory event concerns a watched file.
func (w *Watcher) relevant(name string) bool {
	name = filepath.Clean(name)
	for _, p := range w.watchedFiles() {
		if name == p {
			return true
		}
	}
	// A ConfigMap mount updates by swapping ..data next to the config path.
	return filepath.Dir(name) == filepath.Dir(filepath.Clean(w.path)) &&
		strings.HasPrefix(filepath.Base(name), configMapDataLink)
}

// Start begins watching the config file. It blocks until ctx is
// cancelled or a fatal watcher error occurs. Callers should run
// this in a goroutine.
func (w *Watcher) Start(ctx context.Context) {
	defer w.markReady()
	if IsKubernetesPod() {
		w.AlsoWatch(DashboardOverlayFile)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.logger.Error("failed to create fsnotify watcher", "error", err)
		return
	}
	defer func() { _ = fsw.Close() }() // best-effort; process/goroutine is tearing down either way

	// The config file's own directory is required; the others (a symlink
	// target's, the overlay's) are best-effort.
	primaryDir := filepath.Dir(filepath.Clean(w.path))
	if err := fsw.Add(primaryDir); err != nil {
		w.logger.Error("failed to watch config file", "path", w.path, "dir", primaryDir, "error", err)
		return
	}
	dirs := map[string]bool{primaryDir: true}
	for _, p := range w.watchedFiles() {
		dir := filepath.Dir(p)
		if dirs[dir] {
			continue
		}
		dirs[dir] = true
		if err := fsw.Add(dir); err != nil {
			w.logger.Warn("failed to watch config layer directory", "path", p, "dir", dir, "error", err)
		}
	}

	w.logger.Info("config file watcher started", "path", w.path, "files", w.watchedFiles())
	w.markReady()

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
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 || !w.relevant(event.Name) {
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

// ownWritesOnly reports whether every watched file that exists holds exactly
// what this process last wrote to it, i.e. the event was our own save.
func (w *Watcher) ownWritesOnly() bool {
	seen := false
	for _, p := range w.watchedFiles() {
		sum, ok := sumFile(p)
		if !ok {
			continue
		}
		seen = true
		if !writtenByThisProcess(ResolvedConfigPath(p), sum) {
			return false
		}
	}
	return seen
}

func (w *Watcher) reload() {
	if w.ownWritesOnly() {
		w.logger.Debug("config reload skipped: the files hold this process's own last save", "path", w.path)
		return
	}

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
