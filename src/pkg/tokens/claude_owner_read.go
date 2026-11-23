package tokens

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Reading per-UID Claude session trees as their owner (hivecommons/hive#19307).
//
// Since hivecommons/hive#17031 each per-UID agent's Claude transcripts live
// under its own HOME (<home>/agents/<name>/.claude/projects). Claude Code
// creates that tree 0700 with 0600 transcripts, owned by the agent's UID, so
// the hive process cannot even list it: filepath.Glob swallows the EACCES and
// the collector saw zero Claude usage for every per-UID agent, which left the
// fleet cost estimate (and the RUNNING tile's "this hr") flat at $0.00 once
// the fleet ran on the Claude CLI.
//
// The fix reads such a tree through su-exec as the tree's owner, the same
// privileged-read path crux/agent uses for agent-owned Claude sessions
// (claude_session_adopt.go). Widening the tree's permissions is not an
// option: every agent shares the same primary group, so a group-readable tree
// would expose one agent's transcripts to its siblings.

const (
	// suExecBinary is the setuid helper (gated to the hive-launch group) that
	// runs a command as another UID.
	suExecBinary = "su-exec"
	// ownerListTimeout bounds one su-exec listing of a tree. Listing is a
	// shell loop over one directory level: anything slower is wedged.
	ownerListTimeout = 30 * time.Second
	// ownerReadTimeout bounds streaming one transcript through su-exec cat.
	// Transcripts reach hundreds of MB, so this is generous.
	ownerReadTimeout = 2 * time.Minute
	// ownerListFields is "size mtime path" per listing line.
	ownerListFields = 3
	// ownerStderrLimit caps how much helper stderr is quoted in an error.
	ownerStderrLimit = 512
)

// ownerListScript prints "size mtime path" for every transcript the direct
// scan's glob patterns would match (<dir>/*/*.jsonl and
// <dir>/*/subagents/*.jsonl), skipping symlinks. $1 is the tree. It always
// exits 0: an unmatched glob stays literal and fails the -f test.
const ownerListScript = `for f in "$1"/*/*.jsonl "$1"/*/subagents/*.jsonl; do [ -f "$f" ] && [ ! -L "$f" ] && stat -c '%s %Y %n' "$f"; done; exit 0`

// ownerFile is one transcript in an owner listing.
type ownerFile struct {
	path  string
	size  int64
	mtime int64 // unix seconds
}

// ownerCacheEntry is a parsed transcript, valid while its size and mtime are
// unchanged. A nil summary records a file that parsed to zero tokens, so it is
// not streamed again until it changes either.
type ownerCacheEntry struct {
	size    int64
	mtime   int64
	summary *SessionSummary
}

// ownerSessionCache holds parsed per-UID transcripts across scans, keyed by
// path. The direct scan re-reads every file each scan; through su-exec that
// would stream gigabytes every 30 seconds, so here only changed files are
// re-read.
var ownerSessionCache = struct {
	sync.Mutex
	entries map[string]ownerCacheEntry
}{entries: map[string]ownerCacheEntry{}}

// claudeTreeOwnerSpec reports whether dir must be read as its owner, and the
// su-exec user spec to do it with. A readable (or missing) dir is read
// directly: ok is false and err is nil. A permission-denied dir whose owner
// cannot be used returns an error rather than an empty scan, so the failure is
// visible instead of reading as "no usage". A var so tests can force the
// owner path without files owned by another UID.
var claudeTreeOwnerSpec = func(dir string) (spec string, ok bool, err error) {
	if _, rerr := os.ReadDir(dir); rerr == nil || !errors.Is(rerr, fs.ErrPermission) {
		return "", false, nil
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", false, err
	}
	spec, err = ownerSpecForDir(dir, info)
	if err != nil {
		return "", false, err
	}
	return spec, true, nil
}

// ownerSpecForDir returns the "uid:gid" su-exec spec for the owner of the
// directory described by info. It fails closed: a symlink, a non-directory, a
// missing unix owner, or a root-owned tree is a refusal, never a guess, since
// the result decides whose identity a subprocess runs under.
func ownerSpecForDir(dir string, info fs.FileInfo) (string, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("claude session tree %s is a symlink; refusing to read it as its owner", dir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("claude session tree %s is not a directory (mode %s)", dir, info.Mode())
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("claude session tree %s is unreadable and has no unix owner", dir)
	}
	if st.Uid == 0 {
		return "", fmt.Errorf("claude session tree %s is unreadable and owned by uid 0; refusing to read it as root", dir)
	}
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid), nil
}

// ownerRun runs argv as spec through su-exec and returns its stdout. A var so
// tests can stand in for the privileged helper.
var ownerRun = func(spec string, timeout time.Duration, argv ...string) ([]byte, error) {
	if _, err := exec.LookPath(suExecBinary); err != nil {
		return nil, fmt.Errorf("%s unavailable: %w", suExecBinary, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, suExecBinary, append([]string{spec}, argv...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, ownerHelperErr(spec, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// ownerOpen streams path as spec through su-exec cat. Close reports a helper
// failure, so a truncated stream is never cached as the file's contents. A
// var so tests can stand in for the privileged helper.
var ownerOpen = func(spec, path string) (io.ReadCloser, error) {
	if _, err := exec.LookPath(suExecBinary); err != nil {
		return nil, fmt.Errorf("%s unavailable: %w", suExecBinary, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ownerReadTimeout)
	cmd := exec.CommandContext(ctx, suExecBinary, spec, "cat", "--", path)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, ownerHelperErr(spec, err, nil)
	}
	return &ownerStream{ReadCloser: out, cmd: cmd, cancel: cancel, stderr: stderr, spec: spec}, nil
}

// ownerStream is the stdout of a running su-exec cat.
type ownerStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *bytes.Buffer
	spec   string
}

func (s *ownerStream) Close() error {
	defer s.cancel()
	// Drain so the helper never blocks on a full pipe, then reap it.
	_, _ = io.Copy(io.Discard, s.ReadCloser)
	if err := s.cmd.Wait(); err != nil {
		return ownerHelperErr(s.spec, err, s.stderr.Bytes())
	}
	return nil
}

func ownerHelperErr(spec string, err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	if len(msg) > ownerStderrLimit {
		msg = msg[:ownerStderrLimit]
	}
	if msg == "" {
		return fmt.Errorf("%s %s: %w", suExecBinary, spec, err)
	}
	return fmt.Errorf("%s %s: %w: %s", suExecBinary, spec, err, msg)
}

// parseOwnerListing parses ownerListScript output. Malformed lines are
// skipped; a path outside dir is dropped so a helper can never widen the scan.
func parseOwnerListing(dir string, out []byte) []ownerFile {
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	var files []ownerFile
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), " ", ownerListFields)
		if len(parts) != ownerListFields {
			continue
		}
		size, serr := strconv.ParseInt(parts[0], 10, 64)
		mtime, merr := strconv.ParseInt(parts[1], 10, 64)
		path := filepath.Clean(parts[2])
		if serr != nil || merr != nil || !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, ".jsonl") {
			continue
		}
		files = append(files, ownerFile{path: path, size: size, mtime: mtime})
	}
	return files
}

// scanClaudeTreeAsOwner is ScanClaudeSessionTree for a tree only its owner
// can read: it lists the tree as spec, re-reads only transcripts whose size
// or mtime changed since the last scan, and folds every live transcript into
// agg. Cache entries for files no longer listed under dir are dropped.
func scanClaudeTreeAsOwner(agg *AggregateSummary, dir, defaultAgent, spec string) (*AggregateSummary, error) {
	out, err := ownerRun(spec, ownerListTimeout, "sh", "-c", ownerListScript, "sh", dir)
	if err != nil {
		return nil, fmt.Errorf("list claude session tree %s as its owner: %w", dir, err)
	}
	files := parseOwnerListing(dir, out)
	cutoff := time.Now().Add(-maxSessionAgeDays * 24 * time.Hour).Unix()

	ownerSessionCache.Lock()
	defer ownerSessionCache.Unlock()

	listed := make(map[string]bool, len(files))
	var firstErr error
	for _, f := range files {
		listed[f.path] = true
		if f.mtime < cutoff {
			continue
		}
		entry, cached := ownerSessionCache.entries[f.path]
		if !cached || entry.size != f.size || entry.mtime != f.mtime {
			summary, rerr := readClaudeSessionAsOwner(spec, f.path, defaultAgent)
			if rerr != nil {
				// Keep the last good parse, if any, rather than dropping the
				// session's usage for one failed read.
				if firstErr == nil {
					firstErr = rerr
				}
				if !cached {
					continue
				}
			} else {
				entry = ownerCacheEntry{size: f.size, mtime: f.mtime, summary: summary}
				ownerSessionCache.entries[f.path] = entry
			}
		}
		if entry.summary != nil {
			addClaudeSession(agg, entry.summary)
		}
	}
	prefix := filepath.Clean(dir) + string(filepath.Separator)
	for path := range ownerSessionCache.entries {
		if strings.HasPrefix(path, prefix) && !listed[path] {
			delete(ownerSessionCache.entries, path)
		}
	}
	agg.SessionCount = len(agg.Sessions)
	if firstErr != nil && agg.SessionCount == 0 {
		return nil, firstErr
	}
	return agg, nil
}

// readClaudeSessionAsOwner streams one transcript through su-exec and parses
// it. A transcript with no tokens returns a nil summary and no error.
func readClaudeSessionAsOwner(spec, path, defaultAgent string) (*SessionSummary, error) {
	rc, err := ownerOpen(spec, path)
	if err != nil {
		return nil, fmt.Errorf("read %s as its owner: %w", path, err)
	}
	detector := HiveAgentDetector(path)
	if defaultAgent != "" {
		detector = treeAgentDetector(path, defaultAgent)
	}
	summary, perr := parseClaudeSessionReader(rc, path, detector)
	if cerr := rc.Close(); cerr != nil {
		return nil, fmt.Errorf("read %s as its owner: %w", path, cerr)
	}
	if perr != nil {
		return nil, perr
	}
	if summary == nil || summary.TotalTokens == 0 {
		return nil, nil
	}
	return summary, nil
}
