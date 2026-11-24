package hub

import (
	"net/http"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/askai"
	"github.com/hivecommons/hive/pkg/config"
)

// Ask hub relay (phase 4, #19183). Hosted hives ask through the hub so the
// Kapa key never leaves it. A hive authenticates with the per-hive heartbeat
// bearer it already holds; no new secret is issued. The route exists only when
// Ask is on at the hub.

// registeredRelayWarnOnce says once that ask.relay.hives=registered is not
// served in this round.
var registeredRelayWarnOnce sync.Once

// relayAllowsHive reports whether the relay serves hiveID: hosted hives only
// in this round, minus any hive the operator disabled.
//
// TODO(#19177): serving self-hosted hives registered with this hub
// (AskRelayRegistered) waits for an operator decision; until then it behaves
// as AskRelayHosted.
func (s *HubServer) relayAllowsHive(hiveID string) bool {
	if s.ask == nil {
		return false
	}
	settings := s.ask.settings
	if settings.DisabledHives[hiveID] {
		return false
	}
	switch settings.RelayHives {
	case config.AskRelayNone:
		return false
	case config.AskRelayRegistered:
		registeredRelayWarnOnce.Do(func() {
			s.logger.Warn("ask: relay for registered self-hosted hives is not served yet; serving hosted hives only")
		})
	}
	return askHiveIsHosted(hiveID)
}

// askHiveIsHosted: a hive this hub provisioned and runs, never a self-service
// (byo-) registration.
func askHiveIsHosted(hiveID string) bool {
	if strings.HasPrefix(hiveID, SelfServiceHiveIDPrefix) {
		return false
	}
	return loadSaaSHive(hiveID) != nil
}

// handleAskRelay answers POST /api/hub/ask/relay for a hive.
func (s *HubServer) handleAskRelay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth := r.Header.Get("Authorization")
	hiveID := r.Header.Get(askai.RelayHiveIDHeader)
	if !strings.HasPrefix(auth, "Bearer ") || hiveID == "" || hiveID != sanitizeHeartbeatField(hiveID) || !isValidName(hiveID) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.hubSecret == "" || !s.verifyHeartbeatBearer(strings.TrimPrefix(auth, "Bearer "), hiveID) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req askai.Request
	if !decodeAskBody(w, r, &req) {
		return
	}
	if !s.relayAllowsHive(hiveID) {
		s.logger.Info("audit: ask request", "hive", hiveID, "outcome", string(askai.OutcomeRelayDisabled))
		writeAskJSON(w, http.StatusOK, s.ask.svc.RelayDisabledResult())
		return
	}
	// The hive is the bearer's hive; the person is never sent.
	req.Person, req.HiveID = "", hiveID
	writeAskJSON(w, http.StatusOK, s.ask.svc.Relay(r.Context(), req))
}
