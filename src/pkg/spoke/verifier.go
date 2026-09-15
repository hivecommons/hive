package spoke

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Mode selects how the spoke treats the outcome of heartbeat-response
// verification. The DEFAULT is ModeLogOnly, and that default is load-bearing:
// see the staged-rollout note on Verify.
type Mode int

const (
	// ModeLogOnly verifies every response and emits a structured log/metric on
	// any failure, but STILL ACCEPTS the response. This is the default and the
	// only safe first step: a spoke whose hub has not yet shipped signing must
	// keep working, so a spoke can never hard-fail against an un-upgraded hub.
	ModeLogOnly Mode = iota

	// ModeEnforce REJECTS unsigned/mis-bound/stale responses — but ONLY once the
	// spoke has already accepted a valid signed one (trust-on-first-signed).
	// Before that first signed response, even ModeEnforce accepts an unsigned
	// response, so enabling enforcement on a spoke whose hub has not yet shipped
	// signing does not brick it; it simply stays in the pre-trust state until a
	// signed response arrives.
	ModeEnforce

	// ModeOff disables verification entirely. An operator escape hatch for an
	// incident; verification is skipped and every response is accepted with no
	// logging. Not reachable from the default configuration.
	ModeOff
)

// EnvVerifyMode is the spoke env var that selects the mode. Unset or any
// unrecognised value means ModeLogOnly — enforcement is strictly opt-in.
const EnvVerifyMode = "HIVE_HEARTBEAT_VERIFY"

// ModeFromString maps the env value to a Mode. Enforcement is opt-in: only the
// explicit "enforce" turns it on; everything else (including the empty string
// and typos) is log-only, except the explicit "off" escape hatch.
func ModeFromString(v string) Mode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "enforce":
		return ModeEnforce
	case "off", "disabled", "none":
		return ModeOff
	default:
		return ModeLogOnly
	}
}

// Result is the outcome of verifying one heartbeat response.
type Result struct {
	// Accepted is whether the caller should ACT on the response. In ModeLogOnly
	// this is true even on a verification failure; in ModeEnforce a failure
	// (once trust is established) makes it false.
	Accepted bool
	// Signed is whether a valid hub signature bound to this hive with a fresh
	// seq was present — i.e. this response established/renewed trust.
	Signed bool
	// Reason is a stable machine code for logs/metrics and tests.
	Reason string
}

// Reason codes.
const (
	ReasonOK                = "ok"                 // valid signed response, accepted
	ReasonUnsignedUntrusted = "unsigned-untrusted" // no signature, trust not yet established -> accepted (no bricking)
	ReasonDowngrade         = "unsigned-downgrade" // no signature AFTER a valid signed one was seen
	ReasonBadSignature      = "bad-signature"      // signature present but did not verify (tamper/wrong key)
	ReasonHiveMismatch      = "hive-mismatch"      // valid signature, but bound to a DIFFERENT hive (cross-hive replay)
	ReasonStaleSeq          = "stale-seq"          // valid signature for this hive, but seq <= last accepted (rollback replay)
	ReasonWrongVersion      = "wrong-version"      // valid signature, but an unknown sig format version
	ReasonDisabled          = "verification-off"   // ModeOff
)

// sigEnvelope mirrors the sig_* fields hub.HeartbeatResponse marshals into the
// signed body. It is a minimal copy so pkg/spoke need not import pkg/hub (which
// imports pkg/spoke to sign — a dependency the other way would cycle). The json
// tags MUST match hub.HeartbeatResponse's sig_* tags; hub.TestHeartbeatSigTagsMatch
// pins the two together.
type sigEnvelope struct {
	HiveID   string `json:"sig_hive_id"`
	Seq      int64  `json:"sig_seq"`
	SignedAt int64  `json:"sig_ts"`
	Version  int    `json:"sig_v"`
}

// Verifier is the spoke-side heartbeat-response verifier. One instance per
// spoke process (a spoke serves exactly one hive), so a single lastSeq and
// seenSigned are sufficient. Safe for concurrent use.
type Verifier struct {
	mu         sync.Mutex
	mode       Mode
	seenSigned bool  // has a valid signed response ever been accepted?
	lastSeq    int64 // highest accepted seq (rollback floor)

	logger *slog.Logger

	// Observability counters (also surfaced to tests). Atomic so a reader need
	// not take mu.
	cAccepted       atomic.Int64
	cRejected       atomic.Int64
	cLoggedFailures atomic.Int64 // failures tolerated because of ModeLogOnly / pre-trust
	cSignedAccepted atomic.Int64
}

// NewVerifier builds a verifier in the given mode. A nil logger falls back to
// slog.Default so a caller never has to guard the logger.
func NewVerifier(mode Mode, logger *slog.Logger) *Verifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Verifier{mode: mode, logger: logger}
}

// Mode reports the verifier's configured mode.
func (v *Verifier) Mode() Mode { return v.mode }

// Counters is a snapshot of the verifier's observability counters.
type Counters struct {
	Accepted       int64
	Rejected       int64
	LoggedFailures int64
	SignedAccepted int64
}

// Counters returns a snapshot for tests and any readiness surface.
func (v *Verifier) Counters() Counters {
	return Counters{
		Accepted:       v.cAccepted.Load(),
		Rejected:       v.cRejected.Load(),
		LoggedFailures: v.cLoggedFailures.Load(),
		SignedAccepted: v.cSignedAccepted.Load(),
	}
}

// Verify decides whether the spoke should act on a heartbeat response.
//
// STAGED ROLLOUT (issue #7082) — the single most important property here:
//
//	A spoke that starts rejecting responses it cannot verify would BRICK every
//	already-deployed spoke whose hub has not yet been upgraded. So:
//	  - ModeLogOnly (the default) NEVER rejects; it only logs.
//	  - ModeEnforce rejects, but only AFTER a valid signed response has been
//	    accepted (trust-on-first-signed). Before that, an unsigned response is
//	    accepted even under enforcement, so a spoke never hard-fails against a
//	    hub that simply has not shipped the feature yet.
//
// AUTHENTICATE BEFORE TRUST (copied from pkg/delegation.VerifyToken): the
// signature over the RAW body bytes is checked first; only then are the bound
// hive_id/seq/version — which live inside those same bytes — read back and
// enforced. Nothing below the signature check is trusted above it.
//
// Parameters:
//   - pubKeys: the hub SSO public keys the spoke holds (current-then-previous).
//   - wantHiveID: the hive this spoke serves (the hive_id it sent in the beat).
//   - body: the EXACT response body bytes received (not re-encoded).
//   - sigHeader: the detached signature from SigHeader ("" = unsigned response).
//   - now: injected clock (zero => time.Now()).
func (v *Verifier) Verify(pubKeys []string, wantHiveID string, body []byte, sigHeader string, now time.Time) Result {
	_ = nowOrDefault(now) // reserved for a future SignedAt freshness window; seq is the rollback floor today
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.mode == ModeOff {
		v.cAccepted.Add(1)
		return Result{Accepted: true, Signed: false, Reason: ReasonDisabled}
	}

	enforce := v.mode == ModeEnforce
	sigHeader = strings.TrimSpace(sigHeader)

	// --- Unsigned response ---------------------------------------------------
	if sigHeader == "" {
		if !v.seenSigned {
			// Trust not yet established: this is an un-upgraded (or keyless) hub.
			// Accept unconditionally in EVERY mode so we never brick a spoke
			// against a hub that has not shipped signing.
			v.cAccepted.Add(1)
			v.logger.Debug("heartbeat verify: unsigned response accepted (trust not yet established)",
				"reason", ReasonUnsignedUntrusted, "hive_id", wantHiveID, "mode", v.modeName())
			return Result{Accepted: true, Signed: false, Reason: ReasonUnsignedUntrusted}
		}
		// Downgrade attack: the hub previously signed for us, now a response
		// arrives unsigned. Reject under enforcement.
		return v.fail(enforce, ReasonDowngrade, wantHiveID,
			"heartbeat verify: unsigned response AFTER a signed one — possible downgrade/strip attack")
	}

	// --- Signed response: authenticate the bytes FIRST -----------------------
	if !verifyBodyAcrossKeys(pubKeys, sigHeader, body) {
		return v.fail(enforce, ReasonBadSignature, wantHiveID,
			"heartbeat verify: signature did not verify against any published hub key (tamper or wrong key)")
	}

	// Signature is good: the body — including its sig_* binding fields — is now
	// authenticated and safe to parse.
	var env sigEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return v.fail(enforce, ReasonBadSignature, wantHiveID,
			"heartbeat verify: signed body did not parse")
	}

	if env.Version != SigVersion {
		return v.fail(enforce, ReasonWrongVersion, wantHiveID,
			"heartbeat verify: signed response carries an unknown sig format version")
	}

	// Cross-hive replay: a genuine hub signature, but minted for a DIFFERENT
	// hive. wantHiveID is the identity this spoke sent; the authenticated
	// binding must name the same hive.
	if env.HiveID == "" || env.HiveID != wantHiveID {
		return v.fail(enforce, ReasonHiveMismatch, wantHiveID,
			"heartbeat verify: signed response bound to a different hive_id (cross-hive replay)")
	}

	// Rollback replay: reject a seq at or below the highest already accepted.
	if env.Seq <= v.lastSeq {
		return v.fail(enforce, ReasonStaleSeq, wantHiveID,
			"heartbeat verify: signed response seq is not newer than the last accepted (rollback/replay)")
	}

	// Fully valid signed response: establish/renew trust and advance the floor.
	v.seenSigned = true
	v.lastSeq = env.Seq
	v.cAccepted.Add(1)
	v.cSignedAccepted.Add(1)
	v.logger.Debug("heartbeat verify: signed response accepted",
		"reason", ReasonOK, "hive_id", wantHiveID, "seq", env.Seq)
	return Result{Accepted: true, Signed: true, Reason: ReasonOK}
}

// fail centralises the log-only-vs-enforce decision for a verification failure.
// In ModeLogOnly it logs a WARN and ACCEPTS; in ModeEnforce it logs a WARN and
// REJECTS. Either way it emits the same structured line so an operator watching
// the staged rollout sees identical evidence whichever stage they are in.
func (v *Verifier) fail(enforce bool, reason, hiveID, msg string) Result {
	if enforce {
		v.cRejected.Add(1)
		v.logger.Warn(msg, "reason", reason, "hive_id", hiveID, "mode", "enforce", "action", "rejected")
		return Result{Accepted: false, Signed: false, Reason: reason}
	}
	v.cLoggedFailures.Add(1)
	v.cAccepted.Add(1)
	v.logger.Warn(msg, "reason", reason, "hive_id", hiveID, "mode", "log-only", "action", "accepted")
	return Result{Accepted: true, Signed: false, Reason: reason}
}

func (v *Verifier) modeName() string {
	switch v.mode {
	case ModeEnforce:
		return "enforce"
	case ModeOff:
		return "off"
	default:
		return "log-only"
	}
}
