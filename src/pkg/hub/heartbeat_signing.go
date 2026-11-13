package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/spoke"
)

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
//
// It also applies the self-service credential fence (#18060) first, and
// before the keyless early return, because every heartbeat response passes
// through here on its way to the wire: a self-service hive never receives a
// fenced field, signed or not.
func (s *HubServer) signHeartbeatResponse(w http.ResponseWriter, resp *HeartbeatResponse, hiveID string) {
	if withheld := fenceSelfServiceHeartbeatResponse(resp, hiveID); len(withheld) > 0 && s.logger != nil {
		s.logger.Warn("heartbeat: credential fence withheld fields from self-service hive",
			"hive_id", hiveID, "fields", strings.Join(withheld, ","))
	}
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
