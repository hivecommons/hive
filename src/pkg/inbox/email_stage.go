package inbox

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Staging for large mail-in deliveries (hivecommons/hive#18564). The hub
// relays a delivery larger than one request in signed pieces; each piece
// is written under <spool>/.incoming/<delivery id>/ and, when the last
// arrives, the pieces are joined into one body that the dashboard decodes,
// streaming its files into the same staging directory before Add moves
// them into their items. Nothing is held in memory but one piece.
const (
	// emailStageDirName is the staging root under the spool; its name is
	// not an item id, so the spool listing skips it.
	emailStageDirName = ".incoming"
	// EmailStageMaxAge is how long an incomplete delivery is kept.
	EmailStageMaxAge = time.Hour
	// emailStageBody is the joined body of a staged delivery.
	emailStageBody = "body"
	// emailStageIDLen is a hive-mx delivery id (16 random bytes, hex).
	emailStageIDLen = 32
)

// Staging errors.
var (
	ErrEmailChunk    = errors.New("invalid mail-in piece")
	ErrEmailTooLarge = errors.New("the mail-in delivery is too large")
)

func validStageID(id string) bool {
	if len(id) != emailStageIDLen {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (e *EmailSource) stageRoot() string { return filepath.Join(e.Dir, emailStageDirName) }

// StageDir is the staging directory of a delivery.
func (e *EmailSource) StageDir(id string) string { return filepath.Join(e.stageRoot(), id) }

func chunkFile(i int) string { return "chunk-" + strconv.Itoa(i) }

// stagedBytesLocked is the size of everything staged.
func (e *EmailSource) stagedBytesLocked() int64 {
	var n int64
	_ = filepath.WalkDir(e.stageRoot(), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, ierr := d.Info(); ierr == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// pruneStageLocked removes staged deliveries older than EmailStageMaxAge.
func (e *EmailSource) pruneStageLocked(now time.Time) {
	entries, err := os.ReadDir(e.stageRoot())
	if err != nil {
		return
	}
	for _, de := range entries {
		fi, err := de.Info()
		if err == nil && now.Sub(fi.ModTime()) > EmailStageMaxAge {
			_ = os.RemoveAll(filepath.Join(e.stageRoot(), de.Name()))
		}
	}
}

// StageChunk stores piece i of n of delivery id. maxBytes bounds the whole
// delivery. When the last piece arrives and every piece is present, the
// pieces are joined and the path of the whole body is returned; before
// that the path is "". A delivery over maxBytes is dropped with
// ErrEmailTooLarge, and one that would overfill the spool with
// ErrEmailQuota.
func (e *EmailSource) StageChunk(id string, i, n int, data []byte, maxBytes int64) (string, error) {
	if !validStageID(id) || i < 0 || n < 1 || i >= n {
		return "", ErrEmailChunk
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.pruneStageLocked(now)
	dir := e.StageDir(id)
	if err := os.MkdirAll(dir, emailDirMode); err != nil {
		return "", err
	}
	var have int64
	for j := 0; j < n; j++ {
		if j == i {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, chunkFile(j))); err == nil {
			have += fi.Size()
		}
	}
	if have+int64(len(data)) > maxBytes {
		_ = os.RemoveAll(dir)
		return "", ErrEmailTooLarge
	}
	if e.usageLocked()+e.stagedBytesLocked()+int64(len(data)) > e.quota() {
		_ = os.RemoveAll(dir)
		return "", ErrEmailQuota
	}
	if err := os.WriteFile(filepath.Join(dir, chunkFile(i)), data, emailFileMode); err != nil {
		return "", err
	}
	_ = os.Chtimes(dir, now, now)
	if i != n-1 {
		return "", nil
	}
	return e.joinLocked(dir, n)
}

// joinLocked concatenates pieces 0..n-1 into the body file.
func (e *EmailSource) joinLocked(dir string, n int) (string, error) {
	for j := 0; j < n; j++ {
		if _, err := os.Stat(filepath.Join(dir, chunkFile(j))); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("%w: piece %d of %d is missing", ErrEmailChunk, j, n)
		}
	}
	path := filepath.Join(dir, emailStageBody)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, emailFileMode)
	if err != nil {
		return "", err
	}
	for j := 0; j < n; j++ {
		piece := filepath.Join(dir, chunkFile(j))
		in, err := os.Open(piece)
		if err != nil {
			_ = out.Close()
			return "", err
		}
		_, err = io.Copy(out, in)
		_ = in.Close()
		if err != nil {
			_ = out.Close()
			return "", err
		}
		_ = os.Remove(piece)
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// StageFile writes one decoded file of a staged delivery and returns its
// path. Add later moves it into its item.
func (e *EmailSource) StageFile(id string, i int, body io.Reader) (string, error) {
	if !validStageID(id) {
		return "", ErrEmailChunk
	}
	path := filepath.Join(e.StageDir(id), "file-"+strconv.Itoa(i))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, emailFileMode)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

// DropStage removes a delivery's staging directory.
func (e *EmailSource) DropStage(id string) {
	if !validStageID(id) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = os.RemoveAll(e.StageDir(id))
}
