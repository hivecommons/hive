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
	// SandboxSpoolEnv and SandboxSpoolKeyEnv are the generic names of the
	// same two values (hivecommons/hive#18572, #18573): the spool now carries
	// issue and topic requests too, so hive-open-issue and hive-topic read
	// these first. Hive sets both pairs; hive-review keeps reading the old
	// names so an older copy of it still works.
	SandboxSpoolEnv        = "HIVE_SANDBOX_REQUEST_DIR"
	SandboxSpoolKeyEnv     = "HIVE_SANDBOX_SPOOL_KEY"
	sandboxReviewSpoolsDir = "sandbox-review-spools"
)

// Sandbox request kinds (hivecommons/hive#18572, #18573). The spool started
// as hive-review's alone; an envelope with no kind is still a review. Each
// kind is staged apart and handed to the relay handler that already serves
// it in-pod, with every gate that handler applies.
const (
	// SandboxSpoolKindReview is a PR review (hive-review), the original kind.
	SandboxSpoolKindReview = "review"
	// SandboxSpoolKindIssue is a hive-open-issue request: open, comment,
	// claim, close, label or request-review.
	SandboxSpoolKindIssue = "issue"
	// SandboxSpoolKindTopic is a hive-topic request against the agent's own
	// topic claims (bd topic).
	SandboxSpoolKindTopic = "topic"
)

// SandboxSpoolKinds lists every kind the relay accepts, review first.
var SandboxSpoolKinds = []string{SandboxSpoolKindReview, SandboxSpoolKindIssue, SandboxSpoolKindTopic}

// NormalizeSandboxSpoolKind maps an envelope's kind to a known kind: empty
// is a review. ok is false for a kind the relay does not serve.
func NormalizeSandboxSpoolKind(kind string) (string, bool) {
	if kind == "" {
		return SandboxSpoolKindReview, true
	}
	for _, k := range SandboxSpoolKinds {
		if kind == k {
			return k, true
		}
	}
	return "", false
}

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
//
// Repo and Branch name the kick's target ("owner/name") and the branch hive
// opens the kick's PR from. A topic claim made without a PR number binds to
// that PR once it exists (hivecommons/hive#18572). Branch is empty for a
// kick that never opens a PR (a review-only kick).
type SandboxReviewSpool struct {
	Agent     string     `json:"agent"`
	Repo      string     `json:"repo,omitempty"`
	Branch    string     `json:"branch,omitempty"`
	Spool     string     `json:"spool"`
	Key       string     `json:"key"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// SandboxReviewEnvelope is what a sandbox writes to its spool: the request
// JSON, verbatim, its kind, and its MAC under the kick's spool key
// (SandboxSpoolMAC). An envelope without a kind is a review.
type SandboxReviewEnvelope struct {
	Kind    string `json:"kind,omitempty"`
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

// SandboxSpoolMAC is the MAC an envelope of kind carries. A review (kind
// empty or "review") keeps SandboxReviewMAC, so an older hive-review still
// verifies. Every other kind MACs "<kind>\n<request>", which binds the kind:
// a request signed as one kind cannot be replayed as another. The relays in
// bin/ compute the same value.
func SandboxSpoolMAC(key, kind string, request []byte) string {
	if kind == "" || kind == SandboxSpoolKindReview {
		return SandboxReviewMAC(key, request)
	}
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(kind + "\n"))
	m.Write(request)
	return hex.EncodeToString(m.Sum(nil))
}
