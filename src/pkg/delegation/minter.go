package delegation

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

// EnvChainsEnabled is the feature flag. DEFAULT OFF.
//
// WHAT THE FLAG GATES — precisely, because a vague answer here is how a
// "disabled" feature turns out to have been half-running.
//
// OFF (the default, and the state of every hive in the fleet on merge):
//   - No chain is constructed. Enabled() short-circuits before any situation
//     constructor runs, so no identity resolution happens on any hot path.
//   - No chain is signed. No Ed25519 operation occurs.
//   - No chain is logged or emitted. No log line changes, no audit field
//     changes, no HTTP response body changes.
//   - The published-key endpoint reports enabled:false and an EMPTY key list.
//   - Behavior is BYTE-IDENTICAL to the commit before this one. That is not an
//     aspiration; it is pinned by TestFlagOffIsByteIdenticalToBaseline.
//
// ON:
//   - Chains are minted and emitted alongside existing records.
//   - The published-key endpoint serves the verification material.
//   - NOTHING ELSE. No request is refused, no behavior degrades, no code path
//     branches on a chain. Observe-only is not a property of the flag being
//     off — it holds in BOTH states, and is pinned separately by
//     TestObserveOnlyInvariant_NoEnforcementConsultsChain.
//
// The flag exists to control BLAST RADIUS OF COMPUTATION (identity resolution
// and signing on hot paths across ~65 spokes), not to control enforcement.
// There is no enforcement to control.
//
// Value parsing matches HIVE_METRICS_ENABLED (pkg/dashboard) exactly —
// 1/true/yes/on — so an operator who has enabled one feature already knows the
// spelling for this one.
const EnvChainsEnabled = "HIVE_DELEGATION_CHAIN_ENABLED"

// Enabled reports whether chain minting is turned on.
//
// Read from the environment on each call rather than cached. The cost is a map
// lookup on paths that are already doing identity resolution and an Ed25519
// signature, and a cached value would mean an operator disabling the flag on a
// misbehaving spoke has to wait for a pod roll — which is the one moment the
// flag most needs to work.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvChainsEnabled))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Minter mints chains for one hive under one master secret and generation.
//
// A zero Minter (or a nil one) mints nothing and returns "" from every method,
// so a caller that failed to construct one degrades to emitting no chain rather
// than panicking. Given that emit sites sit on the agent-launch and PR-open
// paths, "cannot mint" must never be able to break the work being described.
type Minter struct {
	// seedHex is the PRIVATE Ed25519 seed. Never logged, never returned, never
	// placed in a response or a Deployment env var.
	seedHex string

	// generation is the key generation that seedHex came from. Non-secret; it
	// is stamped into the chain so a verifier can select the right published
	// key. A generation ID names a key, it is not a key.
	generation int

	hiveID string
	logger *slog.Logger
}

// NewMinter builds a Minter from a master secret and the generation it belongs
// to.
//
// Takes the master rather than a seed so the ONLY place the delegation seed is
// derived is SeedFromMaster — one derivation site, one label, no opportunity
// for a caller to pass the wrong domain's key by accident.
//
// Returns nil for an empty master or hive ID: with no key there is nothing to
// sign with, and with no hive ID the chain could not be scoped to a tenant,
// which is the property the whole multi-tenant story rests on. Both are
// fail-closed rather than "mint an unscoped chain".
func NewMinter(master, hiveID string, generation int, logger *slog.Logger) *Minter {
	seed := SeedFromMaster(master)
	if seed == "" || strings.TrimSpace(hiveID) == "" {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Minter{seedHex: seed, generation: generation, hiveID: hiveID, logger: logger}
}

// PublicKey returns the hex Ed25519 PUBLIC key for this minter's generation.
//
// Safe to publish; this is the value a third party verifies with. The private
// half is expanded and discarded inside PublicKeyFromSeed.
func (m *Minter) PublicKey() string {
	if m == nil {
		return ""
	}
	return PublicKeyFromSeed(m.seedHex)
}

// Generation returns the key generation this minter signs under.
func (m *Minter) Generation() int {
	if m == nil {
		return 0
	}
	return m.generation
}

// Mint signs a chain and returns the wire token, or "" when no chain should be
// emitted.
//
// Returns "" — meaning EMIT NOTHING — for every one of: the flag being off, a
// nil minter, a chain that does not validate, and an unsignable seed. Callers
// therefore need exactly one check (`if tok != ""`), and there is no way for a
// caller to distinguish "disabled" from "no honest root" and be tempted to
// handle them differently. Both mean the same thing at the emit site: there is
// no chain to attach.
func (m *Minter) Mint(c Chain, action string, now time.Time) string {
	if m == nil || !Enabled() {
		return ""
	}
	c.Action = action
	c.Generation = m.generation
	if c.HiveID == "" {
		c.HiveID = m.hiveID
	}
	return MintToken(m.seedHex, c, now)
}

// MintFor is the convenience path used by emit sites: build-and-sign, absorbing
// ErrNoHonestRoot into the same "" that every other no-chain case produces.
//
// The `build` closure is a situation constructor from situations.go. Passing a
// closure rather than a pre-built Chain matters when the flag is off: the
// constructor never runs, so the identity resolution it would have done (a
// bot-login file read, a session lookup) does not happen either. That is what
// makes flag-off genuinely free rather than merely silent.
func (m *Minter) MintFor(action string, now time.Time, build func() (Chain, error)) string {
	if m == nil || !Enabled() {
		return ""
	}
	c, err := build()
	if err != nil {
		// ErrNoHonestRoot is the expected, correct outcome for any situation
		// whose root did not resolve. It is logged at DEBUG, not WARN: on a
		// spoke where the bot-login file is absent this fires on every action,
		// and a warning per action would train operators to ignore the log.
		// The comparison harness is where this becomes visible as data.
		m.logger.Debug("delegation: no chain emitted", "action", action, "reason", err)
		return ""
	}
	return m.Mint(c, action, now)
}

// Observe logs a minted chain alongside the action it describes.
//
// THE ENTIRE WRITE PATH OF THIS FEATURE, and it is a log line. Nothing here
// returns a decision, and no caller may use its output to branch. The token is
// logged so a tenant can extract and verify it independently; Describe() is
// logged so an operator reading the log by eye can see the shape without
// running a verifier.
//
// Carries identifiers and generations only — never key material. The token
// contains a signature, which is public by construction (it is the thing third
// parties check), and no secret.
func (m *Minter) Observe(token string, action string) {
	if m == nil || token == "" {
		return
	}
	c, err := VerifyToken(m.PublicKey(), token, time.Now())
	if err != nil {
		// Self-verification failing means we minted something we cannot read
		// back — a wiring fault worth a warning, and it is the ONLY warning
		// this package emits.
		m.logger.Warn("delegation: minted chain failed self-verification", "action", action, "error", err)
		return
	}
	m.logger.Info("delegation chain",
		"action", action,
		"hive_id", c.HiveID,
		"chain", c.Describe(),
		"root_is_human", c.HasHumanRoot(),
		"depth", c.Depth(),
		"generation", c.Generation,
		"token", token,
	)
}
