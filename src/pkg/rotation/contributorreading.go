package rotation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Contributor quota reading publisher (kubestellar/hive#6967).
//
// This is the missing link between the Go-side rotation probers (which
// normalize provider usage into LimitWindows) and the JS contributor quota
// guard in bin/contributor-relay.js (which reads a JSON reading from a file and
// makes the pre-acceptance admission decision). Nothing wrote that file before
// this: every default install ran with the guard's `unprovisioned` admit state
// because no source was ever populated. This publisher populates it.
//
// Three properties are load-bearing and each has a mutation test:
//
//  1. A FAILED probe never publishes a healthy reading. failOpen() marks a
//     failed measurement as Available:true so rotation keeps choosing the
//     provider, but publishing that as a quota reading would be a fail-open
//     disaster — the guard would admit work while blind. A probe error maps to
//     state "unknown", which the relay HOLDS on. This is the single most
//     important correctness property here (kubestellar/hive#6909, #6951).
//
//  2. Writes are ATOMIC (temp file + rename). The relay treats a torn read as
//     "unknown" and holds, but detection is a safety net, not a licence to tear
//     — a torn read costs a full retry interval. rename() within one directory
//     is atomic, so a reader sees the old file or the new one, never a partial.
//     Same discipline as bin/lib/quota-pool-store.js (kubestellar/hive#6953).
//
//  3. The reading is keyed by the local quota POOL (backend + account), not by
//     the writing process, so it lands exactly where the relay for that pool
//     reads it — the cross-process keying #6833 criterion 7 asks for. The key
//     derivation matches derivePoolKey() in quota-pool-store.js byte for byte.
//
// This file publishes DATA only. It deliberately makes no admission decision in
// Go: src/pkg/contributorquota was removed in #6955 to make the JS relay the
// single authority, and a test fails if it reappears.

// contributorGuardProviders is the set of rotation providers whose normalized
// windowed readings the contributor quota guard understands: the subscription
// backends with an adapter (Codex/#6964, Claude/#6965, Agy/#6966). Metered
// providers like DeepSeek report a credit balance, not reset windows, so they
// are not published as quota readings.
var contributorGuardProviders = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"google":    true,
}

// contributorReadingFileSuffix names the per-pool reading file inside the pool
// directory. It shares the directory with quota-pool-store.js's override,
// status and reservation files but uses its own suffix so they never collide.
const contributorReadingFileSuffix = ".reading.json"

// contributorPoolDirName is the per-install subdirectory, under the user config
// dir's "hive" tree, where the reading is published when no explicit
// HIVE_CONTRIBUTOR_QUOTA_POOL_DIR is set (kubestellar/hive#6987). It lives
// beside the hivectl session cache (src/pkg/hivectl/session.go) so all
// per-install hive state shares one predictable root.
const contributorPoolDirName = "contributor-quota"

// DefaultContributorPoolDir is the pool directory the publisher writes into, and
// the relay reads from, when the operator has NOT set
// HIVE_CONTRIBUTOR_QUOTA_POOL_DIR. It makes publishing default-on for supported
// backends (kubestellar/hive#6987 / #6967 criterion 1): a default install gets a
// reading with no hand-set env var.
//
// The location follows the existing per-install precedent in this repo
// (DefaultSessionStore in src/pkg/hivectl/session.go): $XDG_CONFIG_HOME is
// honoured EXPLICITLY first — Go's os.UserConfigDir ignores it on darwin, and a
// variable that redirects the path on Linux but silently not on a Mac would make
// the location impossible to reason about and every test that redirects it flaky
// by platform — then the platform user config dir, joined with "hive" and the
// pool subdirectory. bin/contributor-relay.js derives the identical path in
// defaultContributorPoolDir(); the two MUST agree or the publisher writes where
// the relay never reads. An unresolvable config dir returns "", which leaves
// publishing off and the relay on its unprovisioned/admit default — never a hold
// with no route, the fleet-wide stop #6951's ruling guards against.
func DefaultContributorPoolDir() string {
	dir := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if dir == "" {
		d, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		dir = d
	}
	return filepath.Join(dir, "hive", contributorPoolDirName)
}

// ContributorLimitWindow is one normalized window in the reading the relay
// consumes. Field names match exactly what readContributorQuotaReading() /
// evaluateContributorQuota() parse in bin/contributor-relay.js: pct_remaining,
// kind, id, and a numeric reset_epoch (milliseconds) the guard reads first for
// until-reset override matching, with resets_at kept as a human-readable RFC3339
// mirror.
type ContributorLimitWindow struct {
	ID           string `json:"id,omitempty"`
	Kind         string `json:"kind"`
	PctRemaining int    `json:"pct_remaining"`
	ResetEpoch   int64  `json:"reset_epoch,omitempty"`
	ResetsAt     string `json:"resets_at,omitempty"`
}

// ContributorReading is the JSON document bin/contributor-relay.js reads. The
// state vocabulary is the relay's: "available" carries evaluatable windows,
// "unknown" makes the guard HOLD (used for a failed measurement — never a
// fabricated healthy reading).
type ContributorReading struct {
	State string `json:"state"`
	// CapturedAt records when the probe result was observed, in the same RFC3339
	// UTC format used by per-window resets_at. Older publishers omitted it, so it
	// stays optional on the wire and consumers must tolerate it missing.
	CapturedAt string `json:"captured_at,omitempty"`
	// Cause categorizes an "unknown" state so an operator — and the relay, if it
	// ever consumes it — can tell a DEAD ADAPTER (one that parsed nothing a real
	// payload contains, kubestellar/hive#6986) from a host with no credentials
	// and from a CLI that is not installed. Without this, all three publish an
	// identical `unknown` and a silently-broken adapter is invisible: it looks
	// exactly like "no credentials on this host". Empty for an available
	// reading; a new optional field, so the relay ignores it until it opts in.
	Cause  string                   `json:"cause,omitempty"`
	Limits []ContributorLimitWindow `json:"limits"`
}

// HeadroomToContributorReading normalizes a probed Headroom into the reading the
// relay consumes.
//
// A failed probe (ProbeErr != nil) becomes state "unknown" with NO windows,
// regardless of the Available flag failOpen() set — publishing "plenty of
// headroom" off a measurement that failed is the fail-open bug this guard
// exists to prevent. A successful probe becomes state "available" with its
// normalized windows carried through verbatim.
func HeadroomToContributorReading(h Headroom) ContributorReading {
	capturedAt := time.Now().UTC().Format(time.RFC3339)
	if h.ProbeErr != nil {
		// Carry WHY the measurement failed so a dead adapter stays visible as a
		// different condition than "no credentials"/"not installed"
		// (kubestellar/hive#6986). An uncategorized failure defaults to the
		// generic probe_failed rather than claiming a cause it does not know.
		cause := h.ProbeErrCause
		if cause == ProbeCauseUnspecified {
			cause = ProbeCauseProbeFailed
		}
		return ContributorReading{State: "unknown", CapturedAt: capturedAt, Cause: string(cause), Limits: []ContributorLimitWindow{}}
	}
	limits := make([]ContributorLimitWindow, 0, len(h.Limits))
	for _, w := range h.Limits {
		lw := ContributorLimitWindow{
			ID:           w.ID,
			Kind:         w.Kind,
			PctRemaining: w.PctRemaining,
		}
		if !w.ResetAt.IsZero() {
			lw.ResetEpoch = w.ResetAt.UnixMilli()
			lw.ResetsAt = w.ResetAt.UTC().Format(time.RFC3339)
		}
		limits = append(limits, lw)
	}
	return ContributorReading{State: "available", CapturedAt: capturedAt, Limits: limits}
}

// deriveContributorPoolKey turns backend + account identity into the same
// opaque 16-hex key derivePoolKey() produces in bin/lib/quota-pool-store.js:
// sha256("<backend-lowercased-trimmed>\x00<account-trimmed>") truncated to 16
// hex chars. Keeping the two implementations identical is what lets the relay
// for a pool find the file the publisher wrote for it. A backend-list parity
// style test pins the shared vectors.
func deriveContributorPoolKey(backend, account string) string {
	material := fmt.Sprintf("%s\x00%s", strings.ToLower(strings.TrimSpace(backend)), strings.TrimSpace(account))
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])[:16]
}

// ContributorReadingPath is the absolute path of the reading file for a pool,
// inside dir. The relay computes the identical path from the same inputs.
func ContributorReadingPath(dir, backend, account string) string {
	return filepath.Join(dir, deriveContributorPoolKey(backend, account)+contributorReadingFileSuffix)
}

// publishContributorReadingAtomic writes r to path via a unique temp sibling
// then rename, so a concurrent relay read never observes a partial file. The
// temp file is same-directory to keep the rename on one filesystem.
func publishContributorReadingAtomic(path string, r ContributorReading) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp-%d-%s", filepath.Base(path), os.Getpid(), hex.EncodeToString(nonce[:])))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// EnableContributorReadingPublish turns on publishing to dir, keyed by account
// (which may be empty — then the pool keys off the backend name alone, matching
// the relay's default). Called from the spoke wiring when the operator sets the
// pool directory. Empty dir leaves publishing off.
func (m *Manager) EnableContributorReadingPublish(dir, account string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contributorPublishDir = dir
	m.contributorPublishAccount = account
}

// publishContributorReading writes h as a normalized reading to every
// guard-supported backend that this provider fronts, so the relay for each such
// pool finds its file. No-op when publishing is disabled or the provider is not
// one the guard understands. Best-effort: a publish error is swallowed rather
// than allowed to disrupt the rotation loop — the relay's own missing/torn-file
// HOLD is the safety net if a write does not land.
func (m *Manager) publishContributorReading(h Headroom) {
	dir := m.contributorPublishDir
	if dir == "" || !contributorGuardProviders[h.Provider] {
		return
	}
	account := m.contributorPublishAccount
	reading := HeadroomToContributorReading(h)
	backends := m.cfg.Providers[h.Provider].Backends
	for _, backend := range backends {
		if strings.TrimSpace(backend) == "" {
			continue
		}
		path := ContributorReadingPath(dir, backend, account)
		_ = publishContributorReadingAtomic(path, reading)
	}
}
