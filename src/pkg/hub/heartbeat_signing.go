package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/spoke"
)

// Hub-side signing for the heartbeat RESPONSE (issue #7082, cncf/toc#2286).
//
// The hub->spoke lane used to be authenticated by the TLS channel alone, so a
// TLS-terminating middlebox or a misconfigured HIVE_HUB_URL could push
// arbitrary config/credentials to a spoke, and a captured response could be
// replayed to another hive or to roll config back. This signs the response with
// the hub's EXISTING Ed25519 SSO key (ssoSigningSeed(), derived from the master
// via infoSSOEd25519Seed) — no new key, no new distribution, no new primitive —
// and binds it to the hive_id plus a monotonic seq. Spokes verify with the
// public key they already hold as HIVE_SSO_PUBLIC_KEY; see pkg/spoke.
//
// The signature is DETACHED, carried in the spoke.SigHeader response header and
// computed over the exact marshalled body bytes, so the verifier re-hashes the
// bytes it received rather than re-encoding JSON (same rationale as
// pkg/delegation/token.go signing over a fixed string).

// heartbeatSeqState hands out a strictly-increasing seq per hive.
//
// Seeded from the process start time in unix-nanoseconds and advanced by at
// least one each call, so the sequence is monotonic across the process lifetime
// AND (because a restarted hub reseeds from a later wall-clock time) does not
// regress across a hub restart under normal clock conditions — a spoke's
// rollback floor therefore keeps advancing. A per-hive map keeps one hive's
// counter from being perturbed by another's beat rate.
type heartbeatSeqState struct {
	mu   sync.Mutex
	last map[string]int64
}

var hbSeq = &heartbeatSeqState{last: map[string]int64{}}

// next returns the next monotonic seq for hiveID at time now.
func (h *heartbeatSeqState) next(hiveID string, now time.Time) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last == nil {
		h.last = map[string]int64{}
	}
	candidate := now.UnixNano()
	if prev := h.last[hiveID]; candidate <= prev {
		candidate = prev + 1
	}
	h.last[hiveID] = candidate
	return candidate
}

// signHeartbeatResponse fills the response's authenticated binding fields
// (SigHiveID/SigSeq/SigSignedAt/SigVersion) and sets the detached signature
// header over the resulting body bytes.
//
// FAIL-OPEN BY DESIGN, and only in the one safe direction: a hub with no signing
// seed (no master secret configured) emits an UNSIGNED response, exactly as it
// did before this change. Spokes accept unsigned responses until they have seen
// a signed one (pkg/spoke trust-on-first-signed), so a keyless hub does not
// brick anyone. A hub WITH a seed always signs, so once a spoke has seen one
// signed response it can enforce.
//
// It is called with the SAME resp that is about to be marshalled to the wire,
// and it marshals resp once here to sign the exact bytes; the caller marshals
// again to write. The two marshals are byte-identical (same struct, same
// process), so the signature covers what the spoke receives.
func (s *HubServer) signHeartbeatResponse(w http.ResponseWriter, resp *HeartbeatResponse, hiveID string) {
	seed := s.ssoSigningSeed()
	if seed == "" || hiveID == "" {
		// No key or no identity: leave the response unsigned (legacy behaviour).
		return
	}
	now := time.Now()
	resp.SigHiveID = hiveID
	resp.SigSeq = hbSeq.next(hiveID, now)
	resp.SigSignedAt = now.Unix()
	resp.SigVersion = spoke.SigVersion

	body, err := json.Marshal(resp)
	if err != nil {
		// Should never happen for a struct with no unmarshalable fields; if it
		// does, fall back to an unsigned response rather than a wrong signature.
		resp.SigHiveID = ""
		resp.SigSeq = 0
		resp.SigSignedAt = 0
		resp.SigVersion = 0
		return
	}
	if sig := spoke.SignBody(seed, body); sig != "" {
		w.Header().Set(spoke.SigHeader, sig)
	}
}

// spokeHeartbeatVerifier is the process-wide spoke-side verifier for hub
// heartbeat responses (issue #7082). One instance suffices: a spoke process
// serves exactly one hive, so a single monotonic-seq floor and one
// trust-on-first-signed flag cover it.
//
// The MODE is read once from HIVE_HEARTBEAT_VERIFY (spoke.EnvVerifyMode) and
// defaults to LOG-ONLY, so upgrading a spoke to a build that carries this code
// does NOT begin rejecting anything: it verifies and logs. An operator opts in
// to enforcement explicitly, and even then a spoke only rejects once it has
// already accepted a valid signed response (so it cannot hard-fail against a
// hub that has not shipped signing yet).
var (
	spokeVerifierOnce sync.Once
	spokeVerifier     *spoke.Verifier
)

func spokeHeartbeatVerifier(logger *slog.Logger) *spoke.Verifier {
	spokeVerifierOnce.Do(func() {
		spokeVerifier = spoke.NewVerifier(spoke.ModeFromString(os.Getenv(spoke.EnvVerifyMode)), logger)
	})
	return spokeVerifier
}
