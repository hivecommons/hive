package hub

import (
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// Watermark pool replenisher.
//
// The placeholder pool ("Available slot" hives an admin assigns to approved
// requests) was seeded by hand; when it ran dry, approvals failed with
// "provision more placeholders". This control loop keeps each cluster's pool
// at a configured watermark: when clean available placeholders drop below
// pool_min, it provisions up to pool_target through the bounded provisioning
// queue, gated by the same capacity checks manual provisioning uses
// (max_hives, global cap). pool_target == 0 disables the loop for a cluster,
// which is also the default — nothing changes until an operator opts in via
// clusters.json.

const (
	// poolReplenishInterval throttles the replenish sweep on the mega-poller
	// tick. Co-prime-ish with the other lane periods (7/9/15 min) so the lanes
	// spread rather than align.
	poolReplenishInterval = 11 * time.Minute
	// poolReplenishBackoff is how long a cluster's replenishing is suspended
	// after a seed provision fails, so a broken cluster/OCI tenancy is not
	// hammered every sweep.
	poolReplenishBackoff = 30 * time.Minute
	// placeholderACMMLevel matches the level hand-seeded pool slots have
	// always carried: L2 advisory, the default assignment starting point.
	placeholderACMMLevel = 2
)

// replenishPoolsIfDue runs the watermark sweep at most once per
// poolReplenishInterval. Same IfDue pattern and mutex as the other throttled
// poller lanes (poller-loop-only state under clusterUnreachableMu).
func (s *HubServer) replenishPoolsIfDue() {
	s.clusterUnreachableMu.Lock()
	if time.Since(s.lastPoolReplenish) < poolReplenishInterval {
		s.clusterUnreachableMu.Unlock()
		return
	}
	s.lastPoolReplenish = time.Now()
	s.clusterUnreachableMu.Unlock()
	s.replenishPools()
}

// countCleanAvailable counts assignable pool inventory on a cluster using the
// same predicate the approve/assign paths use (findAvailablePlaceholder):
// statusAvailable AND admin-owned.
func countCleanAvailable(clusterID string) int {
	n := 0
	for _, h := range listSaaSHives() {
		if h.Status == statusAvailable && isHubAdmin(h.Owner) && clusterIDForHive(&h) == clusterID {
			n++
		}
	}
	return n
}

func (s *HubServer) replenishPools() {
	for id := range s.clusters {
		cluster := s.clusters[id]
		if cluster.PoolTarget <= 0 {
			continue // opt-in per cluster
		}
		if !cluster.KubectlReachable() {
			// A pull-only cluster's infra cannot be provisioned from the hub.
			s.logger.Warn("pool replenish skipped — cluster not kubectl-reachable", "cluster", id)
			continue
		}
		s.clusterUnreachableMu.Lock()
		until, backingOff := s.poolReplenishHold[id]
		s.clusterUnreachableMu.Unlock()
		if backingOff && time.Now().Before(until) {
			continue
		}

		avail := countCleanAvailable(id)
		min := cluster.PoolMin
		if min <= 0 {
			min = cluster.PoolTarget // min unset → treat target as the floor
		}
		if avail >= min {
			continue
		}
		want := cluster.PoolTarget - avail
		s.logger.Info("pool below watermark — replenishing",
			"cluster", id, "available", avail, "pool_min", min,
			"pool_target", cluster.PoolTarget, "provisioning", want)
		for i := 0; i < want; i++ {
			// Re-check capacity per seed: earlier seeds in this burst count
			// once their records exist.
			if maxSaaSHivesTotal > 0 && len(listSaaSHives()) >= maxSaaSHivesTotal {
				s.logger.Warn("pool replenish stopped — global hive cap reached", "cluster", id)
				return
			}
			if full, n := clusterAtMaxHives(&cluster); full {
				s.logger.Warn("pool replenish stopped — cluster at max_hives",
					"cluster", id, "count", n, "max_hives", cluster.MaxHives)
				break
			}
			if err := s.seedPlaceholder(&cluster); err != nil {
				s.logger.Error("pool replenish seed failed — backing off",
					"cluster", id, "error", err, "backoff", poolReplenishBackoff)
				s.clusterUnreachableMu.Lock()
				s.poolReplenishHold[id] = time.Now().Add(poolReplenishBackoff)
				s.clusterUnreachableMu.Unlock()
				break
			}
		}
	}
}

// placeholderSuffix builds the date+random tail placeholder identities carry
// (e.g. "260821-x3k9"), matching the shape of the hand-seeded fleet.
func placeholderSuffix(now time.Time) string {
	chars := "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return now.UTC().Format("060102") + "-" + string(b)
}

// provisionHiveFn indirects provisionHive so tests can stub the heavy
// kubectl/manifest work while exercising the replenisher's record lifecycle.
var provisionHiveFn = provisionHive

// seedPlaceholder creates one pool placeholder record for the cluster and
// enqueues its infrastructure provision on the bounded queue. The record is
// written status "provisioning" and flips to statusAvailable only after
// provisionHive succeeds, so the assign path can never hand out a slot whose
// infra was never applied. A failed seed marks the record "error" — visible
// in My Hives like any failed provision, cleanable with the existing tools.
func (s *HubServer) seedPlaceholder(cluster *ClusterConfig) error {
	slug := sanitize(strings.ReplaceAll(cluster.ID, "-", ""))
	org := placeholderOrgPrefix + slug + "-" + placeholderSuffix(time.Now())
	hiveID := "hosted-" + org
	if loadSaaSHive(hiveID) != nil {
		return fmt.Errorf("placeholder id collision: %s", hiveID)
	}
	h := &SaaSHive{
		ID:          hiveID,
		Owner:       primaryHubAdmin(),
		ProjectName: fmt.Sprintf("Available slot (%s) %s", cluster.ID, strings.TrimPrefix(org, placeholderOrgPrefix+slug+"-")),
		Org:         org,
		Repos:       []string{},
		ACMMLevel:   placeholderACMMLevel,
		ClusterID:   cluster.ID,
		Status:      "provisioning",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Subdomain:   hiveID + "." + cluster.Domain,
		// Pool slots track stable and auto-upgrade so inventory never rots on
		// an old image while waiting to be assigned — same posture as the
		// hand-seeded fleet.
		TrackedChannel: "stable",
		AutoUpgrade:    true,
		IsPublic:       false,
	}
	if err := saveSaaSHive(h); err != nil {
		return fmt.Errorf("save placeholder record: %w", err)
	}

	seedRecord := *h
	seedRecord.Repos = append([]string(nil), h.Repos...)
	// Placeholders provision with NO GitHub credentials (nothing to scan until
	// assignment delivers the real project); the app/token fields stay empty.
	req := CreateHiveRequest{
		Org:         h.Org,
		ProjectName: h.ProjectName,
		ACMMLevel:   h.ACMMLevel,
		ClusterID:   cluster.ID,
	}
	enqueueProvision(cluster.ID, func() {
		ph := &seedRecord
		cl := s.clusterForHive(ph)
		if cl == nil {
			ph.Status = "error"
			ph.Error = "no cluster config available"
			_ = saveSaaSHive(ph)
			return
		}
		if err := provisionHiveFn(ph, &req, cl, s.appKeysByAppID(), s.logger); err != nil {
			ph.Status = "error"
			ph.Error = err.Error()
			_ = saveSaaSHive(ph)
			s.logger.Warn("pool placeholder provision failed — backing off cluster",
				"hive_id", ph.ID, "error", err, "backoff", poolReplenishBackoff)
			// The provision runs async on the queue, so the sweep's own
			// error branch never sees this failure — set the hold here.
			s.clusterUnreachableMu.Lock()
			s.poolReplenishHold[cl.ID] = time.Now().Add(poolReplenishBackoff)
			s.clusterUnreachableMu.Unlock()
			return
		}
		ph.Status = statusAvailable
		_ = saveSaaSHive(ph)
		s.logger.Info("pool placeholder provisioned", "hive_id", ph.ID, "cluster", cl.ID)
	})
	return nil
}
