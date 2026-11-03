package crux

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"time"
)

// A sandboxed kick has no path to the pod-local review relay
// (/var/run/hive-metrics/review-requests): the container or Job sees only its
// own workspace. So hive gives every sandbox kick a review SPOOL inside that
// workspace, and registers it with a MANIFEST the relay reads
// (hivecommons/hive#14986). The manifest lives under workDir, outside every
// workspace, so the sandbox can neither see nor forge it — which is why the
// relay attributes a spooled request to the manifest's agent and never to
// the request file's own "agent" field.
const (
	// SandboxReviewSpoolRelPath is the spool, relative to the kick
	// workspace. hive-review writes there when HIVE_REVIEW_REQUEST_DIR
	// names it.
	SandboxReviewSpoolRelPath = ".hive/review-requests"
	// SandboxReviewSpoolEnv is the environment variable hive sets inside the
	// sandbox to the spool's in-sandbox path.
	SandboxReviewSpoolEnv = "HIVE_REVIEW_REQUEST_DIR"
	// SandboxReviewSpoolKeyEnv carries the kick's spool key into the
	// sandbox. The name must not contain TOKEN or SECRET: the launchers
	// strip those (sandbox.IsCredentialName).
	SandboxReviewSpoolKeyEnv = "HIVE_REVIEW_SPOOL_KEY"
	sandboxReviewSpoolsDir   = "sandbox-review-spools"
)

// SandboxReviewSpoolsDir is where the sandbox executor writes one manifest
// per kick and the review relay reads them. Host and runtime share it
// through /data, exactly as they share SandboxRunsDir.
func SandboxReviewSpoolsDir(workDir string) string {
	return filepath.Join(workDir, sandboxReviewSpoolsDir)
}

// SandboxReviewSpool is one kick's manifest. Spool is the host-side path of
// the workspace spool. EndedAt is set once the sandbox has exited; the relay
// drops the manifest after that when nothing is left to deliver.
//
// Key is the kick's spool key: random, minted by the executor for this run
// only, and handed to the sandbox solely through its environment. The spool
// is group-writable, so knowing WHICH workspace a request came from does not
// say WHO wrote it; the relay accepts only a request MACed with this key
// (SandboxReviewMAC), which nothing outside this run's sandbox holds.
type SandboxReviewSpool struct {
	Agent     string     `json:"agent"`
	Spool     string     `json:"spool"`
	Key       string     `json:"key"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// SandboxReviewEnvelope is what a sandbox writes to its spool: the request
// JSON, verbatim, and its MAC under the kick's spool key.
type SandboxReviewEnvelope struct {
	Request string `json:"request"`
	MAC     string `json:"mac"`
}

// SandboxReviewMAC is hex(HMAC-SHA256(key, request)), with the key's text as
// the HMAC key. bin/hive-review.sh computes the same value.
func SandboxReviewMAC(key string, request []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(request)
	return hex.EncodeToString(m.Sum(nil))
}
