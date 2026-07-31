package hub

import (
	"net/http"
	"sync"
	"time"
)

// URL reachability: the hub already probes every spoke's dashboard URL over
// HTTPS during the auth audit, but until now it threw the transport result
// away — a hive whose vanity host served a 503 still showed a green dot, all
// health checks passing, and a dead link in the hub table. The pod being
// healthy says nothing about whether the Route/Ingress, DNS and cert that put
// that pod on its PUBLIC hostname actually work.
//
// This file records the reachability of the URL the hub LINKS USERS TO
// (RegistryEntry.DashboardURL, which carries the vanity host once minted) and
// turns a persistent failure into an alert.
//
// The rule is deliberately conservative in the same spirit as advisoryStale:
// anything we are unsure about is NOT alarmed on. Specifically:
//   - A brand-new hive is still converging (cert-manager issuing, ingress
//     reloading), so it is exempt until urlUnreachableMinAge.
//   - A single bad probe never alerts; it takes urlUnreachableMinFailures
//     consecutive failures, so a rolling restart is invisible.
//   - When an ENTIRE cluster fails at once that is an outage or a hub-side
//     network partition, not N broken hives, so those are suppressed into one
//     cluster-level condition rather than N alerts.

const (
	// urlUnreachableMinFailures is how many CONSECUTIVE failed probes a hive
	// must accumulate before it is alerted on. The audit runs every
	// authAuditInterval (30m), so this is ~1h of sustained failure — long
	// enough that a spoke restart, an ingress reload or a brief DNS blip has
	// resolved, short enough to catch a genuinely dead link the same morning.
	urlUnreachableMinFailures = 2

	// urlUnreachableMinAge is how long after registration a hive is exempt.
	// A freshly provisioned hive legitimately 503s while cert-manager issues a
	// certificate and the ingress controller reloads; alerting during that
	// window would make every new hive page someone. This is the "not yet
	// servable (normal)" vs "broken (converged long ago)" boundary.
	urlUnreachableMinAge = 2 * time.Hour

	// urlClusterOutageRatio is the share of a cluster's probed hives that must
	// fail before the failures are treated as a single cluster-wide outage
	// instead of per-hive breakage. vllm-d has been intermittently unreachable
	// from the hub, and a partition there must not raise 30 separate alerts.
	urlClusterOutageRatio = 0.5

	// urlClusterOutageMinHives is the smallest cluster size the outage rollup
	// applies to. Below this, "most hives failed" is not evidence of an outage
	// — on a 2-hive cluster a single genuinely broken hive already crosses the
	// ratio, and rolling it up would hide a real defect.
	urlClusterOutageMinHives = 3
)

// urlHealthyStatus reports whether an HTTP status from a spoke's dashboard URL
// means the URL is genuinely serving.
//
// This is the load-bearing distinction that the auth audit's notion of
// "reachable" does NOT make: a 503 from an OpenShift router or an nginx
// controller is a perfectly successful HTTP transaction, so the transport
// succeeded — but there is no backend behind the hostname and the user gets an
// error page. Treating that as reachable is exactly how a dead vanity URL hid
// behind a green dot.
//
// Healthy responses:
//   - 200: the dashboard rendered (the auth audit separately flags this as
//     wide open; for reachability it is a success).
//   - 30x: redirected to login. The hub's own spokes do this, so it is the
//     single most common healthy answer and must never read as a failure.
//   - 401/403: an authenticated endpoint refusing an anonymous caller. This is
//     what every vllm-d spoke returns via its OAuth proxy — healthy.
//
// Anything else (5xx, 404, and any transport error) is a failure.
func urlHealthyStatus(status int) bool {
	switch {
	case status == http.StatusOK:
		return true
	case status >= 300 && status < 400:
		return true
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return true
	default:
		return false
	}
}

// urlProbeResult is one probe of one hive's dashboard URL.
type urlProbeResult struct {
	// Status is the HTTP status observed, or 0 when the request never
	// completed (DNS failure, TLS failure, timeout).
	Status int
	// Healthy is urlHealthyStatus(Status), false for a transport error.
	Healthy bool
}

// urlHealthState tracks consecutive failures per hive across audit passes. It
// is the memory that lets a transient blip be distinguished from a hive that
// converged long ago and is still failing.
type urlHealthState struct {
	mu sync.Mutex
	// consecutiveFailures[hiveID] counts probes in a row that came back
	// unhealthy. Reset to zero by any healthy probe.
	consecutiveFailures map[string]int
	// lastStatus[hiveID] is the most recent observed status, surfaced in the
	// alert reason so an operator sees "503" rather than just "unreachable".
	lastStatus map[string]int
}

func newURLHealthState() *urlHealthState {
	return &urlHealthState{
		consecutiveFailures: map[string]int{},
		lastStatus:          map[string]int{},
	}
}

// observe records one probe result and returns the running consecutive-failure
// count for that hive.
func (u *urlHealthState) observe(hiveID string, res urlProbeResult) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastStatus[hiveID] = res.Status
	if res.Healthy {
		delete(u.consecutiveFailures, hiveID)
		return 0
	}
	u.consecutiveFailures[hiveID]++
	return u.consecutiveFailures[hiveID]
}

// snapshot returns a copy of the current per-hive failure counts and last
// statuses, for building alerts without holding the lock.
func (u *urlHealthState) snapshot() (failures map[string]int, statuses map[string]int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	failures = make(map[string]int, len(u.consecutiveFailures))
	for k, v := range u.consecutiveFailures {
		failures[k] = v
	}
	statuses = make(map[string]int, len(u.lastStatus))
	for k, v := range u.lastStatus {
		statuses[k] = v
	}
	return failures, statuses
}

// forget drops hives that are no longer in the registry so the maps do not
// grow without bound as pool slots are recycled.
func (u *urlHealthState) forget(known map[string]bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id := range u.consecutiveFailures {
		if !known[id] {
			delete(u.consecutiveFailures, id)
		}
	}
	for id := range u.lastStatus {
		if !known[id] {
			delete(u.lastStatus, id)
		}
	}
}

// clusterOutage reports whether a cluster's failures should be rolled up into
// a single outage rather than alerted per hive. Requires both a minimum
// cluster size and a majority of its probed hives failing.
func clusterOutage(failed, probed int) bool {
	if probed < urlClusterOutageMinHives {
		return false
	}
	return float64(failed)/float64(probed) >= urlClusterOutageRatio
}

// urlUnreachableAlerts builds the per-hive unreachable-URL alerts for the
// current fleet. It is fed into evaluateAlerts' driftAlerts parameter, the
// pre-existing seam for signals computed outside the evaluator, so this shares
// all of the ack/dedup/first-seen machinery rather than adding a parallel one.
//
// A hive is alerted on only when ALL of these hold, so that "still converging"
// never reads as "broken":
//   - it has failed urlUnreachableMinFailures probes in a row;
//   - it registered at least urlUnreachableMinAge ago;
//   - its cluster is not in a whole-cluster outage.
func (s *HubServer) urlUnreachableAlerts(hives []RegistryEntry, now time.Time) []Alert {
	// A HubServer built directly by a test has no urlHealth; no probe has run,
	// so there is nothing to report.
	if s == nil || s.urlHealth == nil {
		return nil
	}
	failures, statuses := s.urlHealth.snapshot()
	if len(failures) == 0 {
		return nil
	}

	// Recompute the per-cluster tallies from the failure map so the outage
	// suppression here matches what the audit logged.
	clusterProbed := map[string]int{}
	clusterFailed := map[string]int{}
	for _, h := range hives {
		if h.DashboardURL == "" {
			continue
		}
		clusterProbed[h.ClusterID]++
		if failures[h.ID] > 0 {
			clusterFailed[h.ClusterID]++
		}
	}

	var alerts []Alert
	for _, h := range hives {
		n := failures[h.ID]
		if n < urlUnreachableMinFailures {
			continue
		}
		if !urlUnreachableEligible(h.RegisteredAt, now) {
			continue
		}
		if clusterOutage(clusterFailed[h.ClusterID], clusterProbed[h.ClusterID]) {
			continue
		}
		alerts = append(alerts, Alert{
			HiveID:   h.ID,
			HiveName: h.Name,
			Type:     AlertTypeURLUnreachable,
			// Critical: the user-facing link is dead. Every other signal on this
			// hive can look green, so nothing else will surface it.
			Severity: AlertSeverityCritical,
			Reason:   urlUnreachableReason(h.DashboardURL, statuses[h.ID]),
		})
	}
	return alerts
}

// urlUnreachableFacet reports whether a single hive should show the dead-link
// pill, and why. It applies the SAME gates as urlUnreachableAlerts (minimum
// consecutive failures, minimum age, cluster-outage suppression) by reusing the
// alert list itself, so the pill and the alert panel can never disagree about
// which hives are affected — the contract advisoryStale established.
func urlUnreachableFacet(alerts []Alert, hiveID string) (bool, string) {
	for _, a := range alerts {
		if a.HiveID == hiveID && a.Type == AlertTypeURLUnreachable {
			return true, a.Reason
		}
	}
	return false, ""
}

// urlUnreachableReason renders an operator-facing explanation that names the
// URL and what it actually returned, so the fix is obvious from the alert.
func urlUnreachableReason(url string, status int) string {
	if status == 0 {
		return "public URL " + url + " did not respond (DNS, TLS or timeout)"
	}
	return "public URL " + url + " returned HTTP " + itoa(status)
}

// urlUnreachableEligible reports whether a hive is old enough to be alerted on
// for an unreachable URL. A hive with no parseable registration time is NOT
// eligible — unknown is never alarmed on.
func urlUnreachableEligible(registeredAt string, now time.Time) bool {
	if registeredAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, registeredAt)
	if err != nil {
		return false
	}
	return now.Sub(t) >= urlUnreachableMinAge
}
