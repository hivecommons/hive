package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/auditchain"
)

// AuditActionChainGenesis is the checkpoint row that begins the audit log's
// hash chain (#19439). It is written once, at the first boot that finds no
// chained row on disk: the upgrade that introduces the chain, or a fresh
// /data volume. Every row after it links back to it.
//
// A genesis row on a hive whose chain already ran is itself the evidence: the
// chained history was removed. The hub sees it as the heartbeat's
// audit_chain_seq going backwards.
const AuditActionChainGenesis = "audit_chain_genesis"

// startChain loads the chain head from the newest chained row in the audit
// files at path, or writes a genesis row when there is none. Called once from
// newAuditLog, before any other writer can run.
func (a *AuditLog) startChain(path string) {
	if head, ok := auditChainHeadFromFiles(path); ok {
		a.mu.Lock()
		a.chain = head
		a.mu.Unlock()
		return
	}
	a.logRecordAt(time.Now().UTC().Format(time.RFC3339), "system", AuditActionChainGenesis,
		"reason=no_chained_rows_on_disk", "", "", 0)
}

// auditChainHeadFromFiles returns the seq and row_hash of the newest row that
// carries a row_hash, searching the current file and then each rotated
// backup, newest first. A torn or hand-edited final line is skipped: the
// chain continues from the last good row and audit-verify reports the rest.
func auditChainHeadFromFiles(path string) (auditchain.Link, bool) {
	files := auditLogFiles(path)
	for i := len(files) - 1; i >= 0; i-- {
		data, err := readAuditLogFile(files[i])
		if err != nil {
			continue
		}
		lines := bytes.Split(data, []byte("\n"))
		for j := len(lines) - 1; j >= 0; j-- {
			line := bytes.TrimSpace(lines[j])
			if len(line) == 0 {
				continue
			}
			var e AuditEntry
			if json.Unmarshal(line, &e) != nil || e.RowHash == "" || e.Seq <= 0 {
				continue
			}
			return auditchain.Link{Seq: e.Seq, Hash: e.RowHash}, true
		}
	}
	return auditchain.Link{}, false
}

// chainEntryLocked stamps entry with the next seq, prev_hash and its row_hash,
// and returns the row to write. If hashing fails (it cannot for a row this
// package marshalled) the row is still written, unchained, rather than lost;
// audit-verify then reports it. Callers hold a.mu.
func (a *AuditLog) chainEntryLocked(entry *AuditEntry) []byte {
	entry.Seq, entry.PrevHash = a.chain.Next()
	entry.RowHash = ""
	data, err := json.Marshal(entry)
	if err != nil {
		return nil
	}
	hash, err := auditchain.RowHash(data)
	if err == nil {
		entry.RowHash = hash
		data, err = json.Marshal(entry)
	}
	if err != nil {
		slog.Error("audit log: cannot hash-chain entry; writing it unchained", "error", err)
		entry.Seq, entry.PrevHash, entry.RowHash = 0, "", ""
		data, _ = json.Marshal(entry)
	}
	return data
}

// ChainHead returns the head of the on-disk hash chain: the seq and row_hash
// of the newest row written. Zero when nothing has been chained (no /data).
func (a *AuditLog) ChainHead() auditchain.Link {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chain
}

// AuditChainHead exposes the audit chain head for the heartbeat sender, so
// the hub holds a copy a writer on the data volume cannot reach.
func (s *Server) AuditChainHead() auditchain.Link {
	return s.audit.ChainHead()
}

// ErrNoAuditLog is returned by VerifyAuditLog when there is no file to check.
var ErrNoAuditLog = errors.New("no audit log files found")

// VerifyAuditLog checks the hash chain across the audit log at path ("" →
// /data/audit.jsonl) and its rotated backups, oldest first, exactly as the
// rows were written.
func VerifyAuditLog(path string) (auditchain.Report, error) {
	if path == "" {
		path = auditLogPath
	}
	files := auditLogFiles(path)
	if len(files) == 0 {
		return auditchain.Report{}, ErrNoAuditLog
	}
	var v auditchain.Verifier
	for _, f := range files {
		data, err := readAuditLogFile(f)
		if err != nil {
			return v.Report(), err
		}
		for i, line := range bytes.Split(data, []byte("\n")) {
			v.Add(f, i+1, line)
		}
	}
	return v.Report(), nil
}
