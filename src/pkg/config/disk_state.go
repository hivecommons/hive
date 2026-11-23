package config

import (
	"crypto/sha256"
	"errors"
	"log"
	"os"
	"sync"
)

// Config file content tracking (hivecommons/hive#19561).
//
// Every persist rewrites the config file from the in-memory config. When an
// operator hand-edits that file (on Kubernetes, /data/hive.yaml.runtime and
// the dashboard overlay) and the in-memory config has not picked the edit up
// yet, that rewrite silently reverts the edit. To stop that, a Config records
// a content hash of each layer file it was loaded from or last saved to, and
// the process records the hash of every layer file it writes. A save that
// finds a layer whose content matches neither was edited by someone else: it
// refuses to overwrite it and asks for a reload instead. The watcher uses
// the process record to tell its own writes from an operator's.

// ErrConfigChangedOnDisk is returned by Save when a config layer file was
// changed on disk since this Config last loaded or saved it. The save writes
// nothing, so the edit survives; the config is reloaded from the file instead.
var ErrConfigChangedOnDisk = errors.New("config file changed on disk since the last load or save; not overwriting it")

type contentSum [sha256.Size]byte

func sumBytes(data []byte) contentSum { return sha256.Sum256(data) }

// sumFile hashes the file at path. ok is false when it cannot be read
// (missing, or unreadable), in which case there is no edit to protect.
func sumFile(path string) (sum contentSum, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sum, false
	}
	return sumBytes(data), true
}

// diskLayers is what one Config last read from or wrote to its layer files,
// keyed by resolved path. Guarded by saveMu once the Config is in use.
type diskLayers struct {
	sums map[string]contentSum
	// warned is the on-disk content the "persist skipped" warning was last
	// logged for, per path, so a file that stays changed (for example one
	// that does not load) is reported once rather than on every persist.
	warned map[string]contentSum
}

func (c *Config) recordLayerSum(path string, sum contentSum) {
	if c.disk == nil {
		c.disk = &diskLayers{sums: map[string]contentSum{}, warned: map[string]contentSum{}}
	}
	c.disk.sums[ResolvedConfigPath(path)] = sum
}

// processWrites records the content this process last wrote to each config
// layer file, keyed by resolved path. Any Config instance's save counts, so a
// second instance saving the same file is not mistaken for an external edit.
var (
	processWritesMu sync.Mutex
	processWrites   = map[string]contentSum{}
)

func recordProcessWrite(path string, sum contentSum) {
	processWritesMu.Lock()
	defer processWritesMu.Unlock()
	processWrites[ResolvedConfigPath(path)] = sum
}

// writtenByThisProcess reports whether sum is the content this process last
// wrote to the file at the resolved path.
func writtenByThisProcess(resolved string, sum contentSum) bool {
	processWritesMu.Lock()
	defer processWritesMu.Unlock()
	last, ok := processWrites[resolved]
	return ok && last == sum
}

var (
	externalChangeObserverMu sync.Mutex
	externalChangeObserver   func(path string)
)

// SetExternalChangeObserver registers a callback invoked when a save refuses
// to overwrite a layer file that changed on disk. The main process uses it to
// schedule a config reload, so the edit reaches memory even when the file
// watcher missed it. Passing nil clears the observer.
func SetExternalChangeObserver(fn func(path string)) {
	externalChangeObserverMu.Lock()
	defer externalChangeObserverMu.Unlock()
	externalChangeObserver = fn
}

func notifyExternalChange(path string) {
	externalChangeObserverMu.Lock()
	fn := externalChangeObserver
	externalChangeObserverMu.Unlock()
	if fn != nil {
		fn(path)
	}
}

// changedLayerLocked returns the first layer file whose on-disk content is
// neither what this Config last loaded or saved nor what this process last
// wrote. Caller holds saveMu. A Config built in code (never loaded or
// saved) tracks nothing and never reports a change.
func (c *Config) changedLayerLocked() (path string, onDisk contentSum, changed bool) {
	if c.disk == nil {
		return "", onDisk, false
	}
	for p, recorded := range c.disk.sums {
		cur, ok := sumFile(p)
		if !ok || cur == recorded || writtenByThisProcess(p, cur) {
			continue
		}
		return p, cur, true
	}
	return "", onDisk, false
}

// ChangedOnDisk reports whether a config layer file this Config was loaded
// from or saved to has been changed on disk by someone other than this
// process since then, and which file. A save would refuse to overwrite it;
// the caller should reload the config from disk first.
func (c *Config) ChangedOnDisk() (path string, changed bool) {
	if c == nil {
		return "", false
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	path, _, changed = c.changedLayerLocked()
	return path, changed
}

// refuseSaveIfChangedLocked is saveLocked's guard. Caller holds saveMu.
func (c *Config) refuseSaveIfChangedLocked() error {
	path, onDisk, changed := c.changedLayerLocked()
	if !changed {
		return nil
	}
	if last, ok := c.disk.warned[path]; !ok || last != onDisk {
		c.disk.warned[path] = onDisk
		log.Printf("[config] config persist skipped: %s changed on disk since the last load or save; reloading it instead of overwriting the edit", path)
	}
	notifyExternalChange(path)
	return ErrConfigChangedOnDisk
}
