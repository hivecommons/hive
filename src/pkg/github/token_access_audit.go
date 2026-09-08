package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Token-access audit trail (#6287).
//
// THE DEFECT: GET /api/token-access is an operator-only security audit trail
// ("every gh CLI command an agent issued", gated at owner role, CWE-284) and
// it was fed by the audited parties writing straight into the log file.
// bin/gh-wrapper.sh and bin/git-credential-hive.sh run AS the agent UID and
// used to `>>` the JSONL file directly, which only works when the file is
// writable by every agent (v4 pre-created it dev:node 0664, and every agent's
// primary group is node). Append is indistinguishable from write at the file
// permission level, so any prompt-injected agent could truncate the trail of
// its own token use, rewrite lines to attribute its activity to a peer, or
// forge entries wholesale. An audit log the audited party can edit is not an
// audit log.
//
// THE FIX: the same drop-box relay every other agent->hive write already uses
// (pr-requests, issue-requests, merge-requests). The wrappers write one JSON
// event per gh call / credential lookup into TokenAccessSpoolDir, a directory
// agents can create files in but cannot list, and only the file's owner (or
// the hive, which owns the directory) can unlink. The hive process ingests
// those events into TokenAccessLogPath, which is owned by the hive UID with
// mode 0600: no agent can open it for writing, truncating, or appending, and
// no agent can read the fleet-wide history either (the endpoint is owner-only
// for exactly that reason). While ingesting, the hive replaces the event's
// self-reported "uid" with the spool file's OWNING uid, the same trust anchor
// the PR-request watcher uses: an agent can only ever speak for itself, so an
// entry forged in a peer's name lands attributed to the forger, with the
// claimed uid preserved alongside as evidence.
//
// What this does NOT close: an agent can still delete its OWN event during
// the poll window before the hive picks it up (a hive-side unix socket with
// SO_PEERCRED would close that too, and is the natural next step), and it can
// still flood the log with real gh calls. Neither lets it alter a line that
// has already been ingested, which is the invariant the tests assert.

// TokenAccessSpoolDir is where the per-agent wrappers drop token-access
// events. Mirrors PRRequestDir. Changing it requires changing the constant in
// bin/gh-wrapper.sh and bin/git-credential-hive.sh, which deliberately carry
// no environment override: an override would let an agent redirect its own
// audit events into the void.
const TokenAccessSpoolDir = "/var/run/hive-metrics/token-access-events"

// TokenAccessLogPath is the durable, hive-owned audit log the dashboard
// serves. Kept at its historical path so existing operator tooling and
// pkg/dashboard's reader keep working.
const TokenAccessLogPath = "/var/run/hive-metrics/token-access.jsonl"

// tokenAccessLogMode is the ONLY acceptable permission set for the durable
// log: owner (the hive UID) read/write, nobody else anything. Every agent UID
// shares the hive's primary group ("node"), so any group bit at all is an
// agent bit. Re-asserted on every boot and on every ingest pass so a file
// left behind by a v4 image (dev:node 0664) is tightened rather than trusted.
const tokenAccessLogMode os.FileMode = 0o600

// tokenAccessSpoolDirMode is the drop-box mode for the spool directory:
// owner rwx, group write+search WITHOUT read. Agents (group node) can create
// and rename their own event files but cannot enumerate the directory, so
// one agent cannot discover a peer's not-yet-ingested events. setgid makes
// every dropped file inherit the node group so the hive (also group node)
// can read it regardless of the agent's umask; sticky keeps deletion to the
// file's owner and the directory's owner, as on /tmp. See requestDirMode for
// why the special bits must be os.Mode* and not raw octal.
const tokenAccessSpoolDirMode = 0o730 | os.ModeSetgid | os.ModeSticky

// tokenAccessSpoolTmpSuffix marks an event still being written. The wrappers
// write to <name>.tmp and rename into place, so the ingester never reads a
// half-written line.
const tokenAccessSpoolTmpSuffix = ".tmp"

// tokenAccessSpoolStaleTmpAge is how old an abandoned .tmp file must be before
// the ingester removes it (a wrapper killed between write and rename).
const tokenAccessSpoolStaleTmpAge = 10 * time.Minute

// tokenAccessMaxEventBytes bounds one event. A real event is under 1 KiB; the
// cap stops an agent turning the spool into a disk-filling channel or pushing
// a multi-megabyte "line" into the log the dashboard splits in memory.
const tokenAccessMaxEventBytes = 8 * 1024

// tokenAccessPollInterval is how often the ingester scans the spool. Short,
// because an event sitting in the spool is still deletable by its author; the
// window between drop and ingest is the only tamper window left.
var tokenAccessPollInterval = 2 * time.Second

// tokenAccessSpoolDirForTest / tokenAccessLogPathForTest let tests redirect
// the ingester into a temp dir. Empty means production paths.
var (
	tokenAccessSpoolDirForTest string
	tokenAccessLogPathForTest  string
)

func tokenAccessSpoolDir() string {
	if tokenAccessSpoolDirForTest != "" {
		return tokenAccessSpoolDirForTest
	}
	return TokenAccessSpoolDir
}

func tokenAccessLogPath() string {
	if tokenAccessLogPathForTest != "" {
		return tokenAccessLogPathForTest
	}
	return TokenAccessLogPath
}

// PrepareTokenAccessAudit creates the spool drop-box and the hive-owned log,
// and tightens the log to tokenAccessLogMode. Runs at boot regardless of
// GitHub App state: the wrappers emit events whenever an agent touches a
// token, App or not, and the trail must exist to receive them. Returns false
// only when the spool cannot be created, in which case the ingester has
// nothing to watch and disables itself.
func PrepareTokenAccessAudit(logger *slog.Logger) bool {
	spool, logPath := tokenAccessSpoolDir(), tokenAccessLogPath()
	if err := os.MkdirAll(spool, 0o777); err != nil {
		if logger != nil {
			logger.Warn("token-access audit: cannot create event spool; agent token use will not be recorded",
				slog.String("dir", spool), slog.String("error", err.Error()))
		}
		return false
	}
	if err := os.Chmod(spool, tokenAccessSpoolDirMode); err != nil && logger != nil {
		logger.Warn("token-access audit: could not set drop-box perms on event spool; agents may be unable to record token use",
			slog.String("dir", spool), slog.String("error", err.Error()))
	}
	f, err := openTokenAccessLog(logPath)
	if err != nil {
		if logger != nil {
			logger.Warn("token-access audit: cannot open audit log",
				slog.String("path", logPath), slog.String("error", err.Error()))
		}
		return true
	}
	_ = f.Close()
	return true
}

// openTokenAccessLog opens the durable log append-only as the hive UID and
// enforces tokenAccessLogMode on it. The mode is enforced with an explicit
// Chmod, not just the create mode, because the file may pre-exist with the
// v4 group-writable mode and O_CREATE does not touch an existing file's bits.
func openTokenAccessLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, tokenAccessLogMode)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != tokenAccessLogMode {
		if err := f.Chmod(tokenAccessLogMode); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// StartTokenAccessAuditWatcher runs the ingest loop until ctx is cancelled.
// Same contract as StartPRRequestWatcher: the returned channel closes when
// the loop exits, and the watcher disables itself when the spool cannot be
// created.
func StartTokenAccessAuditWatcher(ctx context.Context, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if !PrepareTokenAccessAudit(logger) {
		close(done)
		return done
	}
	spool, logPath := tokenAccessSpoolDir(), tokenAccessLogPath()
	// Captured before spawning for the same race-detector reason as
	// StartPRRequestWatcher.
	interval := tokenAccessPollInterval
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ctx.Err() != nil {
					return
				}
				IngestTokenAccessEventsOnce(logger, spool, logPath, time.Now())
			}
		}
	}()
	return done
}

// IngestTokenAccessEventsOnce moves every complete event in spool into the
// durable log, oldest first, and returns how many were appended. Events that
// are not a single JSON object, or exceed tokenAccessMaxEventBytes, are
// dropped with a warning rather than appended: the log must only ever hold
// lines the dashboard can hand back as json.RawMessage.
func IngestTokenAccessEventsOnce(logger *slog.Logger, spool, logPath string, now time.Time) int {
	entries, err := os.ReadDir(spool)
	if err != nil {
		if logger != nil && !os.IsNotExist(err) {
			logger.Warn("token-access audit: cannot read event spool",
				slog.String("dir", spool), slog.String("error", err.Error()))
		}
		return 0
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, tokenAccessSpoolTmpSuffix) {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > tokenAccessSpoolStaleTmpAge {
				_ = os.Remove(filepath.Join(spool, name))
			}
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return 0
	}
	// Wrapper file names start with a nanosecond timestamp, so lexical order
	// is arrival order.
	sort.Strings(names)

	out, err := openTokenAccessLog(logPath)
	if err != nil {
		if logger != nil {
			logger.Warn("token-access audit: cannot open audit log; leaving events in spool",
				slog.String("path", logPath), slog.String("error", err.Error()))
		}
		return 0
	}
	defer out.Close()

	appended := 0
	for _, name := range names {
		path := filepath.Join(spool, name)
		line, ok := readTokenAccessEvent(logger, path)
		if ok {
			if _, err := out.Write(append(line, '\n')); err != nil {
				if logger != nil {
					logger.Warn("token-access audit: append failed; leaving event in spool",
						slog.String("event", name), slog.String("error", err.Error()))
				}
				return appended
			}
			appended++
		}
		// Consumed (or rejected): the hive owns the spool directory, so the
		// sticky bit does not stop it unlinking an agent-owned file.
		if err := os.Remove(path); err != nil && logger != nil {
			logger.Warn("token-access audit: cannot remove consumed event",
				slog.String("event", name), slog.String("error", err.Error()))
		}
	}
	return appended
}

// tokenAccessClaimedUIDKey is where a mismatching self-reported uid is kept
// when the ingester overrides it with the spool file's owner.
const tokenAccessClaimedUIDKey = "claimed_uid"

// readTokenAccessEvent validates one spool file and returns the compact JSON
// line to append. The event's "uid" is replaced by the file's owning uid
// where the platform reports one (Linux in production); a self-reported uid
// that disagrees is preserved under "claimed_uid" so a forgery attempt is
// itself on the record.
func readTokenAccessEvent(logger *slog.Logger, path string) ([]byte, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, false
	}
	if fi.Size() == 0 || fi.Size() > tokenAccessMaxEventBytes {
		if logger != nil {
			logger.Warn("token-access audit: dropping event of unacceptable size",
				slog.String("event", filepath.Base(path)), slog.Int64("bytes", fi.Size()))
		}
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if logger != nil {
			logger.Warn("token-access audit: cannot read event",
				slog.String("event", filepath.Base(path)), slog.String("error", err.Error()))
		}
		return nil, false
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil || event == nil {
		if logger != nil {
			logger.Warn("token-access audit: dropping event that is not a JSON object",
				slog.String("event", filepath.Base(path)))
		}
		return nil, false
	}
	if owner := fileOwnerUID(fi); owner >= 0 {
		ownerJSON, _ := json.Marshal(owner)
		if claimed, present := event["uid"]; present && string(claimed) != string(ownerJSON) {
			event[tokenAccessClaimedUIDKey] = claimed
		}
		event["uid"] = json.RawMessage(ownerJSON)
	}
	line, err := json.Marshal(event)
	if err != nil {
		return nil, false
	}
	return line, true
}
