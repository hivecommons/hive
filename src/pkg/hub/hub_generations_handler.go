package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Admin-facing rotation endpoints (follow-on PR #4).
//
// AUTHORISATION. Both routes are wrapped in requireAdmin (saas.go), which:
//   - calls isCSRFSafe FIRST, before any identity resolution, so a cross-site
//     POST never reaches the rotation logic. Verified during the F4 work: BOTH
//     requireAuth and requireAdmin enforce it. Without that, an ambient hub
//     session cookie would make a cross-origin form POST rotate the fleet's
//     master secret — which is the single most destructive unauthenticated
//     action available on the platform.
//   - gates on the REAL logged-in user (getRealAuthUser), not the effective
//     one, and refuses every admin route while an impersonation grant is
//     active.
//
// SECRET HYGIENE. Nothing in this file logs, returns, or renders any secret
// material. The responses carry generation IDs, counts, and timestamps — an ID
// names a key, it is not a key. This is asserted by test, not just by review.

// rotateResponse is what the rotate endpoint returns. Deliberately contains no
// field that could hold key material.
type rotateResponse struct {
	OK bool `json:"ok"`
	// Current is the new minting generation's ID.
	Current int `json:"current"`
	// Previous is the demoted generation's ID, 0 if there was none.
	Previous int `json:"previous,omitempty"`
	// PreviousVerifyUntil is when the demoted generation stops being accepted.
	PreviousVerifyUntil string `json:"previous_verify_until,omitempty"`
	// LiveGenerations is every ID the hub now accepts for VERIFY, current
	// first.
	LiveGenerations []int `json:"live_generations"`
	// RotatedAt is when this rotation happened.
	RotatedAt string `json:"rotated_at"`
	// Forced records that the cooldown was overridden.
	Forced bool `json:"forced,omitempty"`
	// Note tells the operator what happens next, because the rotation is not
	// finished when this returns — the fleet converges over hours.
	Note string `json:"note"`
}

// rotateRefusedResponse is the 409 body for a refused double-rotation.
type rotateRefusedResponse struct {
	Error string `json:"error"`
	// RetryAfterSeconds is when the cooldown lapses.
	RetryAfterSeconds int `json:"retry_after_seconds"`
	// Current is the generation currently minting — unchanged by the refusal.
	Current int `json:"current"`
}

// rotationConvergenceNote explains the part an operator most needs to know and
// is most likely to get wrong: the rotation is NOT complete when the endpoint
// returns 200.
const rotationConvergenceNote = "rotation applied. The hub now mints only with the new generation; " +
	"artifacts minted under the previous one keep verifying until previous_verify_until. " +
	"Spoke-held material converges through the existing reconcile sweep at 3 patches per " +
	"15-minute cycle (~6h for the full fleet), rolling each pod once. Watch " +
	"GET /api/saas/admin/auth-rollout for per-generation counts and safe_to_retire_previous."

// handleRotateMasterKey performs a master-secret rotation.
//
// POST /api/saas/admin/rotate-master-key   body: {"force": bool}
//
// IDEMPOTENCY / DOUBLE-SUBMIT. A double-submitted POST does NOT produce two
// rotations. rotateMasterSecret holds the generation write lock across
// evaluate-generate-persist-install, so the second request observes the first
// one's lastKeyRotation and evaluateRotation refuses it with 409 — the cooldown
// IS the double-submit guard, not merely a policy on top of one. The two
// requests cannot interleave inside the critical section, so there is no window
// in which both read a pre-rotation lastKeyRotation.
//
// This is why the guard is a refusal rather than a warning: a warning would let
// a double-click strand the fleet, and a double-click is the single likeliest
// way this endpoint is called twice.
func (s *HubServer) handleRotateMasterKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		// Force overrides the cooldown. See evaluateRotation for when this is
		// legitimate (the new generation is itself compromised) and what it
		// costs (spokes still on the dropped generation 401 until the sweep
		// reaches them).
		Force bool `json:"force"`
	}
	// An absent or empty body is fine — force defaults to false, the safe
	// value. Only MALFORMED JSON is an error, so a plain POST with no body
	// performs an unforced rotation.
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
	}

	now := time.Now().UTC()
	next, decision, err := s.rotateMasterSecret(now, body.Force)

	w.Header().Set("Content-Type", "application/json")

	if errors.Is(err, errRotationTooSoon) {
		retry := int(decision.RetryAfter.Seconds())
		if retry < 0 {
			retry = 0
		}
		s.logger.Warn("master key rotation REFUSED — previous rotation still converging",
			"by", s.getRealAuthUser(r),
			"retry_after_seconds", retry,
			"current", s.currentGenerations().Current)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(rotateRefusedResponse{
			Error:             decision.Reason,
			RetryAfterSeconds: retry,
			Current:           s.currentGenerations().Current,
		})
		return
	}
	// AUDIT 8 / F19 + F21. The fleet is not fully observed, so a rotation would
	// converge the reachable spokes and strand the rest at verify_until. This is
	// a 409 like the cooldown — the request is well-formed and the hub is
	// healthy, the FLEET is not in a state where rotating is safe — but it
	// carries no RetryAfter, because unlike the cooldown it does not clear on
	// its own. It clears when every spoke becomes reachable by the convergence
	// lane.
	if errors.Is(err, errRotationWouldStrandSpokes) {
		obs := s.fleetObservation()
		s.logger.Warn("master key rotation REFUSED — fleet not fully observed, rotating would strand spokes",
			"by", s.getRealAuthUser(r),
			"considered_hives", obs.Considered,
			"unreachable_hives", obs.Unreachable,
			"unreachable_clusters", strings.Join(obs.UnreachableClusters, ","),
			"current", s.currentGenerations().Current)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(rotateRefusedResponse{
			Error:   decision.Reason,
			Current: s.currentGenerations().Current,
		})
		return
	}
	if err != nil {
		// Includes a failed PERSIST. The in-memory set is untouched in that
		// case (rotateMasterSecret persists before installing), so this is a
		// clean no-op and the operator can retry.
		s.logger.Error("master key rotation FAILED — no rotation applied",
			"by", s.getRealAuthUser(r), "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "rotation failed; no rotation was applied — see hub logs",
		})
		return
	}

	resp := rotateResponse{
		OK:        true,
		Current:   next.Current,
		RotatedAt: now.Format(time.RFC3339),
		Forced:    decision.RetryAfter > 0,
		Note:      rotationConvergenceNote,
	}
	for _, g := range next.Generations {
		resp.LiveGenerations = append(resp.LiveGenerations, g.ID)
		if g.ID != next.Current {
			resp.Previous = g.ID
			if !g.VerifyUntil.IsZero() {
				resp.PreviousVerifyUntil = g.VerifyUntil.UTC().Format(time.RFC3339)
			}
		}
	}

	// IDs and timestamps only — never the secret, and never a prefix or length
	// of it either. A generation ID names a key; it is not a key.
	s.logger.Info("master key ROTATED",
		"by", s.getRealAuthUser(r),
		"current", resp.Current,
		"previous", resp.Previous,
		"previous_verify_until", resp.PreviousVerifyUntil,
		"forced", resp.Forced,
		"path", hubGenerationsPath)

	_ = json.NewEncoder(w).Encode(resp)
}

// keyGenerationsResponse is the read-only view of the generation set.
type keyGenerationsResponse struct {
	Current int `json:"current"`
	// Generations describes each live generation. No secrets.
	Generations []keyGenerationInfo `json:"generations"`
	// LastRotation is when the current generation was minted, empty on a hub
	// that has never rotated.
	LastRotation string `json:"last_rotation,omitempty"`
	// RotateAvailableIn is how long until the cooldown lapses, 0 when a
	// rotation may proceed now.
	RotateAvailableInSeconds int `json:"rotate_available_in_seconds"`
	// PersistPath is where the set is stored, so an operator can find it
	// without reading the source.
	PersistPath string `json:"persist_path"`
}

// keyGenerationInfo describes ONE generation without its secret.
type keyGenerationInfo struct {
	ID      int    `json:"id"`
	Created string `json:"created,omitempty"`
	// VerifyUntil is empty for the current generation, which never expires.
	VerifyUntil string `json:"verify_until,omitempty"`
	// Acceptable is whether this generation is accepted for VERIFY right now.
	// Computed from the wall clock, so a generation whose window has closed
	// reads false here even though it is still listed.
	Acceptable bool `json:"acceptable"`
	// Current marks the one generation that MINTS.
	Current bool `json:"current"`
}

// handleKeyGenerations serves the read-only generation view.
//
// GET /api/saas/admin/key-generations
//
// Admin-gated like its siblings. It is strictly less sensitive than the
// rotate endpoint — it returns no secret material at all — but it describes the
// hub's key posture, which is fleet-shaped information with no reason to be
// public.
func (s *HubServer) handleKeyGenerations(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	gs := s.currentGenerations()

	out := keyGenerationsResponse{PersistPath: hubGenerationsPath}
	if gs != nil {
		out.Current = gs.Current
		acceptable := make(map[int]bool, len(gs.Generations))
		for _, g := range gs.acceptableGenerations(now) {
			acceptable[g.ID] = true
		}
		for _, g := range gs.Generations {
			info := keyGenerationInfo{
				ID:         g.ID,
				Acceptable: acceptable[g.ID],
				Current:    g.ID == gs.Current,
			}
			if !g.Created.IsZero() {
				info.Created = g.Created.UTC().Format(time.RFC3339)
			}
			if !g.VerifyUntil.IsZero() {
				info.VerifyUntil = g.VerifyUntil.UTC().Format(time.RFC3339)
			}
			out.Generations = append(out.Generations, info)
		}
	}
	if last := s.lastRotationAt(); !last.IsZero() {
		out.LastRotation = last.UTC().Format(time.RFC3339)
		if d := evaluateRotation(last, now, false); !d.Allowed {
			out.RotateAvailableInSeconds = int(d.RetryAfter.Seconds())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
