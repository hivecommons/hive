package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// hiveIDFilePath is the persistent file where the Hive ID is stored.
const hiveIDFilePath = "/data/hive-id"

// hiveIDPattern constrains a Hive ID to a DNS-label-safe token.
//
// The previous validator was displayNamePattern, which permits spaces and
// uppercase (#7247). That was wrong on two counts: it contradicted the UI's
// own documented "hive-adjective-noun" format, and the Hive ID is not a
// display name — it is an infrastructure identifier that ends up in
// Kubernetes namespaces (the "hive-hosted-" prefix in pkg/hubbackup), in
// hosted spoke subdomains, and in the hub's registry keys. A value with a
// space in it cannot be any of those things.
//
// Existing IDs are NOT re-validated: nothing validates HiveID at config load,
// so this tightening applies only to a future edit and cannot strand a hive
// that already carries a legacy value.
var hiveIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// hiveIDLockReason reports whether this hive's ID is owned by the hub and
// therefore must not be edited locally, along with the operator-facing reason.
//
// On a hub-managed spoke the Hive ID is the hub's PRIMARY KEY for this hive.
// Changing it locally does not rename the hive, it orphans it: the registry
// entry and ownership records still point at the old ID, heartbeat signatures
// and their replay guard are bound to it, SSO token verification checks it,
// self-upgrade and branch-switch target it, alert acknowledgements reference
// it, and quota is accounted against it. The spoke would go offline from the
// hub's point of view while believing itself healthy.
//
// Detection mirrors the release-channel selector's rule: hub management is a
// server-side determination, surfaced to the UI rather than guessed by it.
// hiveIDLockedByHub is the pure form of the rule, so the status builder (a
// free function over *config.Config) and the HTTP handlers share one
// definition instead of drifting apart.
func hiveIDLockedByHub(cfg *config.Config) (locked bool, reason string) {
	if cfg == nil {
		return false, ""
	}
	if cfg.Hub.Enabled && strings.TrimSpace(cfg.Hub.URL) != "" {
		return true, hiveIDHubLockReason
	}
	return false, ""
}

// hiveIDHubLockReason is the single operator-facing explanation, shared by the
// API error body and the disabled UI control so they cannot disagree.
const hiveIDHubLockReason = "This hive is managed by the hub, which uses the Hive ID as its primary key for registration, heartbeat signing, SSO, upgrades, alerts, and quota. Renaming it here would orphan the hive rather than rename it. Change it from the hub."

func (s *Server) hiveIDLockReason() (locked bool, reason string) {
	if s.deps == nil {
		return false, ""
	}
	return hiveIDLockedByHub(s.deps.Config)
}

func (s *Server) handleHiveIDGet(w http.ResponseWriter, r *http.Request) {
	id := ""
	if s.deps != nil && s.deps.Config != nil {
		id = s.deps.Config.HiveID
	}
	locked, reason := s.hiveIDLockReason()
	jsonResponse(w, map[string]any{
		"id":         id,
		"editable":   !locked,
		"lockReason": reason,
	})
}

func (s *Server) handleHiveIDSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// Refuse before touching the body: on a hub-managed spoke there is no
	// input that would make this safe. 409 Conflict, not 403 — the caller's
	// credentials are fine; the request conflicts with the hive's managed
	// state.
	if locked, reason := s.hiveIDLockReason(); locked {
		jsonError(w, reason, http.StatusConflict)
		return
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil || body.ID == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}

	const maxHiveIDLen = 64
	if len(body.ID) > maxHiveIDLen {
		jsonError(w, fmt.Sprintf("id must be at most %d characters", maxHiveIDLen), http.StatusBadRequest)
		return
	}
	if !hiveIDPattern.MatchString(body.ID) {
		jsonError(w, "id must be lowercase alphanumeric and hyphens, starting and ending with alphanumeric (e.g. hive-bold-hawk)", http.StatusBadRequest)
		return
	}
	body.ID = sanitizeString(body.ID)

	if s.deps != nil && s.deps.Config != nil {
		s.deps.Config.HiveID = body.ID
	}

	// Persist the new ID to disk so it survives restarts
	tmpHiveID := hiveIDFilePath + ".tmp"
	if err := os.WriteFile(tmpHiveID, []byte(body.ID+"\n"), 0o644); err != nil {
		s.logger.Warn("failed to persist hive ID", "error", err)
	} else if err := os.Rename(tmpHiveID, hiveIDFilePath); err != nil {
		s.logger.Warn("failed to rename hive ID file", "error", err)
	}

	s.auditFromRequest(r, "set_hive_id", auditDetail("id", body.ID), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "id": body.ID})
}
