package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/sandbox"
)

// reviewSpoolDrainTimeout bounds how long a finished kick waits for the
// review relay to deliver requests the sandbox left in its spool. The wait
// matters most for a run-stage kick, whose worktree — spool included — is
// removed as soon as Run returns. Vars so tests can shorten them.
var (
	reviewSpoolDrainTimeout = 60 * time.Second
	reviewSpoolDrainPoll    = time.Second
)

// reviewSpoolHandle is one kick's registration with the review relay
// (hivecommons/hive#14986): the spool inside the workspace and the manifest,
// outside it, that names the agent the relay attributes the spool to.
type reviewSpoolHandle struct {
	manifest string
	entry    crux.SandboxReviewSpool
}

// openReviewSpool creates the workspace spool and registers it. A nil handle
// with a nil error means spooling is not configured for this kick.
func (e *SandboxExecutor) openReviewSpool(manifestDir, agentName, workspace string) (*reviewSpoolHandle, error) {
	if strings.TrimSpace(manifestDir) == "" {
		return nil, nil
	}
	spool := filepath.Join(workspace, crux.SandboxReviewSpoolRelPath)
	if err := os.MkdirAll(spool, 0o770); err != nil {
		return nil, fmt.Errorf("create review spool: %w", err)
	}
	// Owner-only: the relay trusts a manifest's agent name, so nothing but
	// hive may write here. The relay refuses a group- or world-writable dir.
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		return nil, fmt.Errorf("create review spool manifest dir: %w", err)
	}
	if err := os.Chmod(manifestDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure review spool manifest dir: %w", err)
	}
	// The run's spool key: the only thing that lets the relay tell this
	// sandbox's requests from ones another agent drops into the
	// group-writable spool. It reaches the sandbox only through its
	// environment, never through the workspace.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("mint review spool key: %w", err)
	}
	h := &reviewSpoolHandle{
		manifest: filepath.Join(manifestDir, fmt.Sprintf("%s-%d.json", sanitizeBranchPart(agentName), time.Now().UnixNano())),
		entry:    crux.SandboxReviewSpool{Agent: agentName, Spool: spool, Key: hex.EncodeToString(key), StartedAt: e.now().UTC()},
	}
	if err := h.write(); err != nil {
		return nil, err
	}
	return h, nil
}

// sandboxEnv is what the sandbox needs to use the spool: hive-review writes
// to HIVE_REVIEW_REQUEST_DIR when it is set, and MACs each request with
// HIVE_REVIEW_SPOOL_KEY.
func (h *reviewSpoolHandle) sandboxEnv() map[string]string {
	if h == nil {
		return nil
	}
	return map[string]string{
		crux.SandboxReviewSpoolEnv:    path.Join(sandbox.DefaultWorkspaceMount, crux.SandboxReviewSpoolRelPath),
		crux.SandboxReviewSpoolKeyEnv: h.entry.Key,
	}
}

func (h *reviewSpoolHandle) write() error {
	b, err := json.Marshal(h.entry)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(h.manifest), filepath.Base(h.manifest)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, h.manifest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// end marks the kick finished. The relay keeps delivering what is still in
// the spool and drops the manifest once nothing is left.
func (h *reviewSpoolHandle) end(now time.Time) error {
	if h == nil {
		return nil
	}
	at := now.UTC()
	h.entry.EndedAt = &at
	return h.write()
}

// awaitDrain waits, bounded by reviewSpoolDrainTimeout, until the relay has
// consumed every request in the spool. A request the sandbox wrote in its
// last seconds would otherwise race the run-stage worktree removal.
func (h *reviewSpoolHandle) awaitDrain(ctx context.Context) bool {
	if h == nil || !reviewSpoolPending(h.entry.Spool) {
		return true
	}
	deadline := time.NewTimer(reviewSpoolDrainTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(reviewSpoolDrainPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return !reviewSpoolPending(h.entry.Spool)
		case <-tick.C:
			if !reviewSpoolPending(h.entry.Spool) {
				return true
			}
		}
	}
}

// reviewSpoolPending reports whether the spool still holds a request the
// relay has not consumed. Result files and quarantined requests do not
// count; neither does an in-progress "<name>.json.tmp".
func reviewSpoolPending(spool string) bool {
	entries, err := os.ReadDir(spool)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".result.json") {
			return true
		}
	}
	return false
}
