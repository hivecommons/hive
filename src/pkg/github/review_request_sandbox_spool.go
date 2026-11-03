package github

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Sandbox review spools (hivecommons/hive#14986). A reviewer whose kicks run
// in a sandbox — a Podman container or, on a hardware spoke, a Kubernetes Job
// on the accelerator node — cannot reach ReviewRequestDir. The sandbox
// executor gives every kick a spool inside its workspace and registers it
// with a manifest under crux.SandboxReviewSpoolsDir; this file is the relay's
// half. Each pass it:
//
//  1. moves every request out of the spool into a hive-owned staging dir
//     next to the manifest,
//  2. runs the staging dir through the SAME handleOneReviewRequest the
//     pod-local drop box uses, so per-head caps, repo pause, the lane write
//     allowlist, the sentinel block and the audit trail apply unchanged, and
//  3. copies each result file back into the spool, where the sandboxed agent
//     reads it exactly as it would in-pod.
//
// The spool is sandbox-writable, so the relay touches it only through an
// os.Root opened at the workspace: a symlink the sandbox plants can never
// steer a read, rename or write outside its own workspace. Everything that
// renames, quarantines or retries happens in staging, which only hive writes.
//
// Attribution differs from the pod-local path. There a request names its
// agent and the file's owning UID proves it. Inside a sandbox the UID proves
// nothing (the Job runs as whatever its image says) and the "agent" field is
// the sandbox's to write, so a spooled request is attributed to the
// manifest's agent and only that. The manifest is trusted because the
// sandbox cannot reach it: it lives outside every workspace, in an
// owner-only directory, and the relay ignores one that is not owned by its
// own uid or that anyone else could write.
//
// The manifest says which workspace a request came from, not who wrote it:
// the spool is group-writable, so any agent sharing hive's group could drop
// a file there. So the request must also carry a MAC under the run's spool
// key (crux.SandboxReviewEnvelope), which the executor mints per kick, keeps
// in the manifest and hands to the sandbox only through its environment. A
// request without a valid MAC, or one whose MAC this spool has already
// accepted (a copied request), is set aside as .rejected and never staged.
// That is what stands in for the file-owner forge check the pod-local path
// relies on.

// sandboxReviewSpoolsDirForTest lets tests point the relay at a temp dir.
var sandboxReviewSpoolsDirForTest string

func sandboxReviewSpoolsDir() string {
	if sandboxReviewSpoolsDirForTest != "" {
		return sandboxReviewSpoolsDirForTest
	}
	return crux.SandboxReviewSpoolsDir(crux.WorkDir())
}

// sandboxReviewSpoolMaxAge drops a manifest whatever its state. A kick never
// outlives it, so an older manifest belongs to a run whose executor died
// before marking it ended.
const sandboxReviewSpoolMaxAge = 24 * time.Hour

// sandboxReviewSpoolKeyMinLen is the shortest spool key a manifest may carry:
// 128 bits, hex-encoded. The executor mints 256.
const sandboxReviewSpoolKeyMinLen = 32

// sandboxReviewManifestMaxBytes caps a manifest read; a real one is a few
// hundred bytes.
const sandboxReviewManifestMaxBytes = 16 << 10

// reviewRequestSource says who a request file is attributed to. The zero
// value is the pod-local drop box: the request's own agent field, checked
// against the file's owning UID.
type reviewRequestSource struct {
	// agent, when set, is the manifest-attributed owner of a sandbox spool.
	// It replaces the request's agent field, and the UID check is skipped.
	agent string
}

type sandboxReviewSpool struct {
	path string
	crux.SandboxReviewSpool
}

// staging is the hive-owned dir a spool's requests are processed in.
func (s sandboxReviewSpool) staging() string {
	return strings.TrimSuffix(s.path, ".json") + ".d"
}

// workspace is the kick workspace the spool lives in, and rel the spool's
// path inside it.
func (s sandboxReviewSpool) workspace() (dir, rel string) {
	rel = filepath.FromSlash(crux.SandboxReviewSpoolRelPath)
	return strings.TrimSuffix(s.Spool, string(filepath.Separator)+rel), rel
}

// processSandboxReviewSpools delivers every registered spool, then drops the
// manifests of kicks that have ended and left nothing to deliver.
func (c *Client) processSandboxReviewSpools(ctx context.Context, nowFn func() time.Time) {
	for _, s := range c.loadSandboxReviewSpools(nowFn()) {
		if ctx.Err() != nil {
			return
		}
		staging := s.staging()
		if err := os.MkdirAll(staging, 0o700); err != nil {
			c.logger.Warn("review-request watcher: cannot create sandbox spool staging dir",
				slog.String("dir", staging), slog.String("error", err.Error()))
			continue
		}
		// A missing workspace (a run-stage worktree is removed when its kick
		// returns) leaves nothing to ingest or reply to, but whatever was
		// already staged is still delivered.
		ws, rel := s.workspace()
		root, err := os.OpenRoot(ws)
		if err == nil {
			c.ingestSandboxSpool(root, rel, staging, s.Key, nowFn())
		}
		c.processReviewRequestDir(ctx, staging, reviewRequestSource{agent: s.Agent}, nowFn)
		spoolPending := false
		if root != nil {
			mirrorSandboxResults(root, rel, staging)
			spoolPending = rootSpoolPending(root, rel)
			_ = root.Close()
		}
		if s.EndedAt != nil && !spoolPending && !dirHasReviewRequest(staging) {
			_ = os.RemoveAll(staging)
			_ = os.Remove(s.path)
		}
	}
}

// hasSandboxReviewSpools reports whether any manifest is registered in dir.
// A dir the relay does not trust is never read or pruned, so it counts as
// holding none; otherwise the watcher would poll fast forever.
// While one is, the watcher keeps its short poll cadence: a Job writes its
// spool over a shared volume from another node, where a change notification
// never fires.
func hasSandboxReviewSpools(dir string) bool {
	if fi, err := os.Lstat(dir); err != nil || untrustedOwnerReason(fi) != "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}

// loadSandboxReviewSpools returns the registered spools, pruning manifests
// that are stale or malformed.
func (c *Client) loadSandboxReviewSpools(now time.Time) []sandboxReviewSpool {
	dir := sandboxReviewSpoolsDir()
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil
	}
	if reason := untrustedOwnerReason(fi); reason != "" {
		c.logger.Warn("review-request watcher: ignoring sandbox review spools; manifest dir is not hive-only",
			slog.String("dir", dir), slog.String("reason", reason))
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sandboxReviewSpool
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, mfi, err := readUntrustedFile(path, sandboxReviewManifestMaxBytes)
		if err != nil {
			if errors.Is(err, errDropBoxFileRejected) {
				_ = os.Remove(path)
			}
			continue
		}
		if reason := untrustedOwnerReason(mfi); reason != "" {
			c.logger.Warn("review-request watcher: ignoring sandbox review manifest",
				slog.String("path", path), slog.String("reason", reason))
			continue
		}
		s := sandboxReviewSpool{path: path}
		if err := json.Unmarshal(data, &s.SandboxReviewSpool); err != nil {
			// Manifests are written by rename, so a parse failure past the
			// in-flight grace is corruption, not a torn write.
			if quarantinable(path, now) {
				_ = os.RemoveAll(s.staging())
				_ = os.Remove(path)
			}
			continue
		}
		if reason := sandboxSpoolRefusal(s.SandboxReviewSpool, now); reason != "" {
			c.logger.Warn("review-request watcher: dropping sandbox review manifest",
				slog.String("path", path), slog.String("agent", s.Agent), slog.String("reason", reason))
			_ = os.RemoveAll(s.staging())
			_ = os.Remove(path)
			continue
		}
		out = append(out, s)
	}
	return out
}

// untrustedOwnerReason is why fi cannot be trusted to carry attribution, or
// "" when it is a regular file or directory owned by this process's uid that
// no one else may write.
func untrustedOwnerReason(fi fs.FileInfo) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return "symbolic link"
	case !fi.IsDir() && !fi.Mode().IsRegular():
		return "not a regular file or directory"
	case fileOwnerUID(fi) != os.Geteuid():
		return "not owned by the relay's uid"
	case fi.Mode().Perm()&0o022 != 0:
		return "group- or world-writable"
	}
	return ""
}

// sandboxSpoolRefusal is why a manifest must be dropped, or "" to keep it.
func sandboxSpoolRefusal(m crux.SandboxReviewSpool, now time.Time) string {
	suffix := string(filepath.Separator) + filepath.FromSlash(crux.SandboxReviewSpoolRelPath)
	switch {
	case strings.TrimSpace(m.Agent) == "":
		return "manifest names no agent"
	case !filepath.IsAbs(m.Spool) || filepath.Clean(m.Spool) != m.Spool || !strings.HasSuffix(m.Spool, suffix):
		return "spool is not <workspace>/" + crux.SandboxReviewSpoolRelPath
	case len(m.Key) < sandboxReviewSpoolKeyMinLen:
		return "manifest carries no spool key"
	case !m.StartedAt.IsZero() && now.Sub(m.StartedAt) > sandboxReviewSpoolMaxAge:
		return "manifest is older than " + sandboxReviewSpoolMaxAge.String()
	}
	return ""
}

// ingestSandboxSpool moves each request out of the spool into staging. A
// name already staged (a request still retrying) waits in the spool for a
// later pass rather than replacing it. A file that is not yet valid JSON is
// left alone for inFlightGrace, in case the sandbox is still writing it.
// Only the request inside an envelope MACed with this run's key is staged;
// anything else is set aside (sandboxSpoolEnvelope).
func (c *Client) ingestSandboxSpool(root *os.Root, rel, staging, key string, now time.Time) {
	d, err := root.Open(rel)
	if err != nil {
		return
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return
	}
	for _, e := range entries {
		if !isReviewRequestFile(e) {
			continue
		}
		name := e.Name()
		dst := filepath.Join(staging, name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		src := filepath.Join(rel, name)
		data, fi, err := readRootFile(root, src, requestMaxBytes)
		if err != nil {
			if errors.Is(err, errDropBoxFileRejected) {
				_ = root.Rename(src, src+".rejected")
				c.logger.Warn("review-request watcher: REJECTED sandbox spool file (unsafe file)",
					slog.String("path", src), slog.String("reason", err.Error()))
			}
			continue
		}
		if !json.Valid(data) && now.Sub(fi.ModTime()) < inFlightGrace {
			continue
		}
		request, mac, reason := sandboxSpoolEnvelope(data, key)
		if reason != "" {
			c.rejectSandboxSpoolFile(root, src, reason, now)
			continue
		}
		marker, err := markSpoolMACSeen(staging, mac)
		if errors.Is(err, fs.ErrExist) {
			c.rejectSandboxSpoolFile(root, src, "replay of a request this spool already accepted", now)
			continue
		}
		if err != nil {
			continue // left in the spool for the next pass
		}
		if err := writeRequestFile(dst, request); err != nil {
			_ = os.Remove(marker)
			continue
		}
		_ = root.Remove(src)
	}
}

// sandboxSpoolEnvelope returns the request inside data and its MAC, or why
// data is not a request this run's sandbox wrote. A request without the key
// is indistinguishable from one another agent planted, so it is refused.
func sandboxSpoolEnvelope(data []byte, key string) (request []byte, mac, reason string) {
	var env crux.SandboxReviewEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.Request == "" || env.MAC == "" {
		return nil, "", "not a signed spool envelope"
	}
	request = []byte(env.Request)
	want := crux.SandboxReviewMAC(key, request)
	if !hmac.Equal([]byte(env.MAC), []byte(want)) {
		return nil, "", "MAC does not match this run's spool key"
	}
	return request, want, ""
}

// markSpoolMACSeen records that this spool accepted a request with mac, so
// a copy of it (the spool is group-readable) cannot be submitted twice. It
// returns the marker path; an fs.ErrExist error means a replay.
func markSpoolMACSeen(staging, mac string) (string, error) {
	seen := filepath.Join(staging, "seen")
	if err := os.MkdirAll(seen, 0o700); err != nil {
		return "", err
	}
	marker := filepath.Join(seen, mac)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	return marker, nil
}

// rejectSandboxSpoolFile sets an unsigned, mis-signed or replayed request
// aside as .rejected and leaves a result file saying why, so a sandbox
// running an old hive-review is not left waiting.
func (c *Client) rejectSandboxSpoolFile(root *os.Root, src, reason string, now time.Time) {
	_ = root.Rename(src, src+".rejected")
	c.logger.Warn("review-request watcher: REJECTED sandbox spool file (not from this run's sandbox)",
		slog.String("path", src), slog.String("reason", reason))
	result := strings.TrimSuffix(src, ".json") + ".result.json"
	if _, err := root.Lstat(result); err == nil {
		return // never overwrite a real result with this one
	}
	resp := ReviewResponse{OK: false, Error: "rejected: " + reason, At: now.UTC().Format(time.RFC3339)}
	if b, err := json.Marshal(resp); err == nil {
		_ = writeRootFile(root, result, b)
	}
}

// mirrorSandboxResults copies each staged result into the spool. A result
// whose request has left staging is final, so its staged copy is dropped
// once mirrored; one still retrying is mirrored again on the next pass.
func mirrorSandboxResults(root *os.Root, rel, staging string) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".result.json") {
			continue
		}
		staged := filepath.Join(staging, name)
		data, err := os.ReadFile(staged)
		if err != nil {
			continue
		}
		if err := writeRootFile(root, filepath.Join(rel, name), data); err != nil {
			continue
		}
		request := strings.TrimSuffix(staged, ".result.json") + ".json"
		if _, err := os.Lstat(request); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(staged)
		}
	}
}

// rootSpoolPending reports whether the spool still holds a request.
func rootSpoolPending(root *os.Root, rel string) bool {
	d, err := root.Open(rel)
	if err != nil {
		return false
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if isReviewRequestFile(e) {
			return true
		}
	}
	return false
}

// dirHasReviewRequest reports whether dir holds a request the relay has not
// consumed (result files and quarantined requests do not count).
func dirHasReviewRequest(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if isReviewRequestFile(e) {
			return true
		}
	}
	return false
}

// isReviewRequestFile is the drop-box filter: a "<name>.json" request, not
// its "<name>.result.json" reply and not a directory.
func isReviewRequestFile(e fs.DirEntry) bool {
	name := e.Name()
	return !e.IsDir() && strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".result.json")
}

// readRootFile is readUntrustedFile for a file inside a sandbox-owned tree.
// The Lstat names a symlink or FIFO as such so it can be set aside; the
// open flags close the window in which one could be swapped in after it.
func readRootFile(root *os.Root, name string, maxBytes int64) ([]byte, fs.FileInfo, error) {
	lfi, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, lfi, dropBoxRejectedError(fmt.Sprintf("not a regular file (%s)", lfi.Mode().Type()))
	}
	f, err := root.OpenFile(name, os.O_RDONLY|untrustedRootOpenFlags, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("not a regular file (%s)", fi.Mode().Type()))
	}
	if fi.Size() > maxBytes {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("%d bytes exceeds cap of %d", fi.Size(), maxBytes))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fi, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fi, dropBoxRejectedError(fmt.Sprintf("grew past cap of %d bytes during read", maxBytes))
	}
	return data, fi, nil
}

// writeRootFile publishes name inside root atomically, the os.Root analogue
// of writeRequestFile: the agent never sees a half-written result.
func writeRootFile(root *os.Root, name string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", name, time.Now().UnixNano())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|untrustedRootOpenFlags, 0o660)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
