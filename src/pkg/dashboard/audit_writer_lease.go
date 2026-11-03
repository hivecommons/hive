package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Audit writer lease (#13046).
//
// A hosted spoke rolls with maxSurge=1/maxUnavailable=0, so for a few minutes
// two pods mount the same ReadWriteMany /data. NFSv3 emulates O_APPEND on the
// client (seek to the cached EOF, then write), so two pods appending to one
// audit.jsonl overwrite each other's bytes and the outgoing pod's entries were
// silently lost. flock is not dependable on NFSv3, so instead exactly one pod —
// the holder of a small lease file next to the log — appends to audit.jsonl.
// Any other pod appends to its own spill file (audit-spill-<id>.jsonl), which
// no one else writes. When a pod acquires the lease it folds its own spill
// into audit.jsonl, and the holder periodically folds in spill files left by
// pods that have gone quiet (released or died). A live non-holder touches its
// spill on every renewal tick, so only a pod that stopped ticking ever looks
// quiet. audit.jsonl stays the
// canonical file; the audit readers already glob audit-*.jsonl, so spilled
// entries are visible before they are merged.
//
// The TTL sits well above the NFS client's default attribute/dentry cache
// ceiling (acregmax/acdirmax = 60s, #13072): a peer may read a lease or spill
// mtime that is up to that old, and must still see it as fresh.
const (
	auditLeaseRenewInterval = 10 * time.Second
	auditLeaseTTL           = 90 * time.Second
)

const (
	auditLeaseFileName = "audit-writer.lease"
	auditSpillInfix    = "-spill-"
)

type auditWriterLease struct {
	Holder  string    `json:"holder"`
	Renewed time.Time `json:"renewed"`
}

func auditLeasePath(logPath string) string {
	return filepath.Join(filepath.Dir(logPath), auditLeaseFileName)
}

func auditSpillGlob(logPath string) string {
	ext := filepath.Ext(logPath)
	prefix := strings.TrimSuffix(filepath.Base(logPath), ext)
	return filepath.Join(filepath.Dir(logPath), prefix+auditSpillInfix+"*"+ext)
}

func auditSpillPath(logPath, id string) string {
	ext := filepath.Ext(logPath)
	prefix := strings.TrimSuffix(filepath.Base(logPath), ext)
	return filepath.Join(filepath.Dir(logPath), prefix+auditSpillInfix+id+ext)
}

// auditWriterID identifies this process: the pod name (hostname) plus the
// start time, so a restarted container never mistakes its predecessor's lease
// or spill file for its own.
func auditWriterID() string {
	host, _ := os.Hostname()
	host = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, host)
	if host == "" {
		host = "hive"
	}
	return host + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func newAuditLumberjack(path string, compress bool) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    auditMaxSizeMB,
		MaxBackups: auditMaxBackups,
		MaxAge:     auditMaxAgeDays,
		Compress:   compress,
	}
}

func readAuditLease(path string) (auditWriterLease, bool) {
	var l auditWriterLease
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &l) != nil || l.Holder == "" {
		return auditWriterLease{}, false
	}
	return l, true
}

func writeAuditLease(path, holder string, now time.Time) error {
	data, err := json.Marshal(auditWriterLease{Holder: holder, Renewed: now.UTC()})
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + holder
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// initWriterLease points the writer at logPath when this process wins the
// lease, or at its own spill file when another live pod holds it. It does not
// start the renewal loop; newAuditLog does.
func (a *AuditLog) initWriterLease(logPath, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.logPath = logPath
	a.writerID = id
	a.stopLease = make(chan struct{})
	if a.tryAcquireLeaseLocked() {
		a.owner = true
		a.writer = newAuditLumberjack(logPath, true)
		a.mergeSpillsLocked()
		return
	}
	a.writer = newAuditLumberjack(auditSpillPath(logPath, id), false)
	slog.Info("audit log held by another pod; spilling to a per-pod file until it is released",
		"spill", a.writer.Filename)
}

// tryAcquireLeaseLocked takes the lease when it is absent, stale, or already
// ours. Callers must hold a.mu.
func (a *AuditLog) tryAcquireLeaseLocked() bool {
	path := auditLeasePath(a.logPath)
	now := time.Now()
	if cur, ok := readAuditLease(path); ok && cur.Holder != a.writerID && now.Sub(cur.Renewed) < auditLeaseTTL {
		return false
	}
	if err := writeAuditLease(path, a.writerID, now); err != nil {
		// Never append to audit.jsonl without a lease: the real holder may
		// still be writing it. Spill instead and retry on the next tick.
		slog.Warn("audit writer lease write failed; spilling to a per-pod file", "path", path, "error", err)
		return false
	}
	// Two pods that saw the same stale lease both wrote one; the rename that
	// landed last wins and the other backs off.
	if cur, ok := readAuditLease(path); ok && cur.Holder != a.writerID {
		return false
	}
	return true
}

func (a *AuditLog) switchWriterLocked(path string, compress bool) {
	if a.writer != nil {
		_ = a.writer.Close()
	}
	a.writer = newAuditLumberjack(path, compress)
}

// reopenSpillIfRemovedLocked drops the spill writer's open descriptor when its
// file has been merged and removed, so the next write recreates the file
// instead of landing on an unlinked inode. Callers must hold a.mu.
func (a *AuditLog) reopenSpillIfRemovedLocked() {
	if a.logPath == "" || a.owner || a.writer == nil {
		return
	}
	if _, err := os.Stat(a.writer.Filename); os.IsNotExist(err) {
		a.switchWriterLocked(a.writer.Filename, false)
	}
}

// touchOwnSpillLocked is a live non-holder's heartbeat: it creates its spill
// file if needed and sets its mtime to now, so the holder never mistakes a
// pod that is merely not logging for one that has gone. Callers must hold a.mu.
func (a *AuditLog) touchOwnSpillLocked() {
	a.reopenSpillIfRemovedLocked()
	spill := auditSpillPath(a.logPath, a.writerID)
	f, err := os.OpenFile(spill, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Warn("audit spill heartbeat failed", "path", spill, "error", err)
		return
	}
	_ = f.Close()
	now := time.Now()
	if err := os.Chtimes(spill, now, now); err != nil {
		slog.Warn("audit spill heartbeat failed", "path", spill, "error", err)
	}
}

// mergeSpillsLocked appends spill files into the canonical log and removes
// them: this process's own spill unconditionally, other pods' only once they
// have not been written for auditLeaseTTL. Callers must hold a.mu and own the
// lease.
func (a *AuditLog) mergeSpillsLocked() {
	matches, err := filepath.Glob(auditSpillGlob(a.logPath))
	if err != nil {
		return
	}
	own := auditSpillPath(a.logPath, a.writerID)
	ownPrefix := strings.TrimSuffix(own, filepath.Ext(own))
	now := time.Now()
	for _, p := range matches {
		// Own spill and lumberjack-rotated backups of it
		// (audit-spill-<id>-<timestamp>.jsonl) merge unconditionally;
		// anything else belongs to another pod and must have gone quiet.
		if p != own && !strings.HasPrefix(p, ownPrefix+"-") {
			info, err := os.Stat(p)
			if err != nil || now.Sub(info.ModTime()) < auditLeaseTTL {
				continue
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("audit spill read failed", "path", p, "error", err)
			continue
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if _, ok := parseAuditLine(line); !ok {
				continue
			}
			if _, err := a.writer.Write(append(line, '\n')); err != nil {
				slog.Error("audit spill merge failed", "path", p, "error", err)
				return
			}
		}
		if err := os.Remove(p); err != nil {
			slog.Warn("audit spill remove failed", "path", p, "error", err)
		}
	}
}

// leaseTick renews (and sweeps quiet spills) while holding the lease, and
// otherwise tries to take it over, keeping its own spill fresh until it does.
func (a *AuditLog) leaseTick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logPath == "" || a.leaseReleased {
		return
	}
	path := auditLeasePath(a.logPath)
	if a.owner {
		if cur, ok := readAuditLease(path); ok && cur.Holder != a.writerID && time.Now().Sub(cur.Renewed) < auditLeaseTTL {
			slog.Warn("audit writer lease taken over by another pod; spilling", "holder", cur.Holder)
			a.owner = false
			a.switchWriterLocked(auditSpillPath(a.logPath, a.writerID), false)
			return
		}
		if err := writeAuditLease(path, a.writerID, time.Now()); err != nil {
			slog.Warn("audit writer lease renew failed", "path", path, "error", err)
		}
		a.mergeSpillsLocked()
		return
	}
	if a.tryAcquireLeaseLocked() {
		a.owner = true
		a.switchWriterLocked(a.logPath, true)
		a.mergeSpillsLocked()
		slog.Info("audit writer lease acquired; merged spilled entries into the audit log", "path", a.logPath)
		return
	}
	a.touchOwnSpillLocked()
}

func (a *AuditLog) runWriterLease(stop <-chan struct{}) {
	t := time.NewTicker(auditLeaseRenewInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			a.leaseTick()
		}
	}
}

// ReleaseWriterLease is called once shutdown begins: it hands the canonical
// audit log to the replacement pod and sends this process's remaining entries
// to its spill file, which the new holder merges once it goes quiet.
func (a *AuditLog) ReleaseWriterLease() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logPath == "" || a.leaseReleased {
		return
	}
	a.leaseReleased = true
	close(a.stopLease)
	if !a.owner {
		return
	}
	a.owner = false
	path := auditLeasePath(a.logPath)
	if cur, ok := readAuditLease(path); ok && cur.Holder == a.writerID {
		if err := os.Remove(path); err != nil {
			slog.Warn("audit writer lease release failed", "path", path, "error", err)
		}
	}
	a.switchWriterLocked(auditSpillPath(a.logPath, a.writerID), false)
}
