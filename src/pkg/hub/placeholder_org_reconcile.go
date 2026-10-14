package hub

import (
	"context"
	"strings"
	"time"
)

const (
	// placeholderOrgSweepInterval is how often assigned hives' live org is
	// compared against the org the hub approved for them. The drift is silent
	// otherwise (#11800: ten hives ran for weeks on an "available-…" org).
	placeholderOrgSweepInterval = 30 * time.Minute

	// placeholderOrgSweepStartupDelay lets spokes re-register after a hub
	// restart before the first comparison, so a not-yet-reported org is not
	// misread as a mismatch.
	placeholderOrgSweepStartupDelay = 2 * time.Minute
)

// assignedPlaceholderOrgStuck reports whether a delivered, real-org claim is
// contradicted by the spoke: it still reports a slot-placeholder org while a
// primary repo is set. A live org of "" means the spoke has not beaten yet and
// is never a mismatch. ClaimDelivered gates it so the normal pre-adoption
// window (Assigning) is not flagged.
func assignedPlaceholderOrgStuck(sh *SaaSHive, liveOrg, livePrimaryRepo string) bool {
	if sh == nil || !sh.ClaimDelivered {
		return false
	}
	if sh.Org == "" || strings.HasPrefix(sh.Org, placeholderOrgPrefix) {
		return false
	}
	if !strings.HasPrefix(liveOrg, placeholderOrgPrefix) {
		return false
	}
	return livePrimaryRepo != "" || sh.PrimaryRepo != ""
}

// placeholderOrgMismatches returns every assigned hive whose live org
// differs from its approved org, noting which are the slot-placeholder case.
func placeholderOrgMismatches(live []RegistryEntry, saas map[string]*SaaSHive) []placeholderOrgMismatch {
	var out []placeholderOrgMismatch
	for _, e := range live {
		sh := saas[e.ID]
		if sh == nil || sh.Status == statusAvailable || sh.Org == "" || e.Org == "" {
			continue
		}
		if strings.HasPrefix(sh.Org, placeholderOrgPrefix) || strings.EqualFold(sh.Org, e.Org) {
			continue
		}
		out = append(out, placeholderOrgMismatch{
			HiveID:      e.ID,
			ApprovedOrg: sh.Org,
			LiveOrg:     e.Org,
			Placeholder: assignedPlaceholderOrgStuck(sh, e.Org, e.PrimaryRepo),
		})
	}
	return out
}

type placeholderOrgMismatch struct {
	HiveID      string
	ApprovedOrg string
	LiveOrg     string
	Placeholder bool
}

// runPlaceholderOrgSweep logs every assigned hive whose live org disagrees with
// the approved one. Hives still inside the unclaimed assignment window are
// skipped: that handshake is expected to differ until the spoke adopts.
func (s *HubServer) runPlaceholderOrgSweep() int {
	s.mu.RLock()
	live := make([]RegistryEntry, len(s.registry.Hives))
	copy(live, s.registry.Hives)
	s.mu.RUnlock()

	saas := make(map[string]*SaaSHive)
	for _, sh := range listSaaSHives() {
		shCopy := sh
		if shCopy.ClaimDelivered {
			saas[shCopy.ID] = &shCopy
		}
	}
	found := placeholderOrgMismatches(live, saas)
	for _, m := range found {
		s.logger.Warn("assigned hive live org differs from approved org",
			"hive", m.HiveID, "approved_org", m.ApprovedOrg, "live_org", m.LiveOrg,
			"slot_placeholder", m.Placeholder)
	}
	return len(found)
}

// startPlaceholderOrgSweep runs runPlaceholderOrgSweep on hub startup (after a
// short delay) and then periodically until ctx is cancelled.
func (s *HubServer) startPlaceholderOrgSweep(ctx context.Context) {
	timer := time.NewTimer(placeholderOrgSweepStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runPlaceholderOrgSweep()
			timer.Reset(placeholderOrgSweepInterval)
		}
	}
}
