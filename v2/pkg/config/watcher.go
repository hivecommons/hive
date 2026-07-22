package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	onChange func(*Config, string)
	logger   *slog.Logger

	mu               sync.Mutex
	timer            *time.Timer
	stopped          bool
	skipNext         bool
	suppressedDigest string
	publishedDigest  string
}

// ErrConcurrentConfigEdit means the file changed outside Hive after the last
// config snapshot was published. The external edit must be reloaded before a
// dashboard mutation may overwrite the file.
var ErrConcurrentConfigEdit = errors.New("config changed externally")

// SkipNext retains the legacy event-count API for embedded callers. New code
// must use ProgrammaticSave: SkipNext cannot distinguish Hive's write from an
// external editor and therefore is not used by the production service.
func (w *Watcher) SkipNext() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.skipNext = true
}

// ProgrammaticSave serializes an in-process config write with watcher
// bookkeeping. Suppression is keyed to the candidate's intended bytes rather
// than a post-write read, so an external editor that wins the race immediately
// after Save is never mistaken for Hive's own write.
func (w *Watcher) ProgrammaticSave(candidate *Config) error {
	if candidate == nil {
		return fmt.Errorf("cannot save nil config")
	}
	saveMu.Lock()
	defer saveMu.Unlock()

	data, err := candidate.persistencePayload()
	if err != nil {
		return fmt.Errorf("preparing config for watcher persistence: %w", err)
	}
	return w.programmaticSave(digestBytes(data), func() error {
		return candidate.savePersistenceBytes(data)
	})
}

func (w *Watcher) programmaticSave(expectedDigest string, save func() error) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.publishedDigest != "" {
		currentDigest, err := fileDigest(w.path)
		if err != nil {
			return fmt.Errorf("checking config before save: %w", err)
		}
		if currentDigest != w.publishedDigest {
			return fmt.Errorf("%w; reload the external edit before saving", ErrConcurrentConfigEdit)
		}
	}
	if err := save(); err != nil {
		return err
	}
	w.suppressedDigest = expectedDigest
	w.publishedDigest = expectedDigest
	return nil
}

// NewWatcher creates a Watcher that will reload the config at path
// whenever the file changes on disk, then invoke onChange with the
// new Config.
func NewWatcher(path string, onChange func(*Config), logger *slog.Logger) *Watcher {
	return NewDigestWatcher(path, func(cfg *Config, _ string) {
		onChange(cfg)
	}, logger)
}

// NewDigestWatcher is NewWatcher with the stable source-file digest supplied
// to the callback. Coordinated runtimes use it to reject a reload that became
// stale while waiting for the config publication lock.
func NewDigestWatcher(path string, onChange func(*Config, string), logger *slog.Logger) *Watcher {
	w := &Watcher{
		path:     path,
		onChange: onChange,
		logger:   logger,
	}
	if digest, err := fileDigest(path); err == nil {
		w.publishedDigest = digest
	}
	return w
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
	defer fsw.Close()

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
	if w.suppressedDigest != "" {
		digest, err := fileDigest(w.path)
		if err == nil && digest == w.suppressedDigest {
			w.suppressedDigest = ""
			w.mu.Unlock()
			if w.logger != nil {
				w.logger.Debug("config reload skipped (matching programmatic save)")
			}
			return
		}
		// The file no longer contains our write. Clear the stale marker and
		// deliver the external edit instead of consuming it.
		w.suppressedDigest = ""
	}
	w.mu.Unlock()

	digestBefore, err := fileDigest(w.path)
	if err != nil {
		w.logger.Warn("config reload fingerprint failed", "path", w.path, "error", err)
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
	digestAfter, err := fileDigest(w.path)
	if err != nil || digestBefore != digestAfter {
		w.logger.Warn("config changed while reloading; waiting for a stable edit", "path", w.path, "error", err)
		w.scheduleReload()
		return
	}

	w.logger.Info("config reloaded from file", "path", w.path)
	w.onChange(cfg, digestAfter)
	if err := w.AcceptExternalDigest(digestAfter); err != nil && w.logger != nil {
		w.logger.Debug("config reload became stale before publication", "path", w.path, "error", err)
	}
}

// AcceptExternalDigest records a stable external file version as the config
// snapshot currently published by the caller. It verifies the file again so a
// reload that waited behind a newer programmatic save cannot publish stale
// bytes over the newer runtime snapshot.
func (w *Watcher) AcceptExternalDigest(expectedDigest string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	currentDigest, err := fileDigest(w.path)
	if err != nil {
		return err
	}
	if currentDigest != expectedDigest {
		return fmt.Errorf("%w while publishing reload", ErrConcurrentConfigEdit)
	}
	w.publishedDigest = expectedDigest
	return nil
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
