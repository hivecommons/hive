package hub

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Auth audit: the hub periodically probes every registered spoke's
// UNAUTHENTICATED /api/status and flags any hive that answers 200 as WIDE OPEN.
// A protected spoke redirects to login (30x) or returns 401 for an
// unauthenticated request; a 200 means the dashboard is served to anyone with
// the URL — the exact drift we've hit when a spoke was provisioned with an empty
// auth_token or lost its hub-nginx gating. This is a safety net that catches
// that drift automatically instead of relying on someone noticing.

const (
	// authAuditInterval is how often the full fleet is probed. Security drift is
	// rare and probing every spoke has a cost, so this is deliberately slow.
	authAuditInterval = 30 * time.Minute

	// authAuditStartupDelay lets the hub finish booting (and spokes settle after
	// a fleet-wide rollout) before the first probe, so a spoke that is merely
	// mid-restart isn't misread.
	authAuditStartupDelay = 5 * time.Minute

	// authAuditProbeTimeout bounds each per-spoke probe. Firewalled/unreachable
	// spokes (vllm-d from the hub) simply time out and are reported as
	// "unreachable", never as open.
	authAuditProbeTimeout = 8 * time.Second

	// authAuditPath is the unauthenticated endpoint probed. It returns hive
	// status data on an open spoke (200) and redirects/401s on a protected one.
	authAuditPath = "/api/status"
)

// StartAuthAudit runs the periodic wide-open audit until ctx is cancelled.
// Launch as a goroutine alongside the other hub background loops.
func (s *HubServer) StartAuthAudit(ctx context.Context) {
	s.logger.Info("auth audit started", "interval", authAuditInterval.String(), "startup_delay", authAuditStartupDelay.String())

	// A dedicated client that does NOT follow redirects: a 30x is the SIGNAL
	// that a spoke is protected (redirecting to login), so following it would
	// mask the very thing we're checking.
	client := &http.Client{
		Timeout: authAuditProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			// Spokes use public CAs; this is a read-only unauthenticated probe of
			// our own fleet, so keep default verification. A private-CA spoke that
			// fails TLS is reported unreachable, not open.
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}

	timer := time.NewTimer(authAuditStartupDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runAuthAudit(ctx, client)
			timer.Reset(authAuditInterval)
		}
	}
}

// runAuthAudit probes every registered spoke once and alerts on any that are
// wide open. It never blocks the caller for long: probes run sequentially with
// a tight per-probe timeout (the fleet is small and this runs off-cadence).
func (s *HubServer) runAuthAudit(ctx context.Context, client *http.Client) {
	s.mu.RLock()
	hives := make([]RegistryEntry, len(s.registry.Hives))
	copy(hives, s.registry.Hives)
	s.mu.RUnlock()

	var open []string
	probed, unreachable := 0, 0
	for _, h := range hives {
		// Only probe hives that report a real dashboard URL (heartbeating spokes).
		base := strings.TrimRight(h.DashboardURL, "/")
		if base == "" || strings.Contains(base, "localhost") {
			continue
		}
		probed++
		wideOpen, reachable := probeWideOpen(ctx, client, base)
		if !reachable {
			unreachable++
			continue
		}
		if wideOpen {
			open = append(open, h.ID)
		}
	}

	if len(open) == 0 {
		s.logger.Info("auth audit: no wide-open spokes", "probed", probed, "unreachable", unreachable)
		return
	}

	// WIDE OPEN found — log loudly and push an ntfy alert if configured.
	msg := fmt.Sprintf("%d WIDE-OPEN hive(s) — unauthenticated %s returned 200: %s",
		len(open), authAuditPath, strings.Join(open, ", "))
	s.logger.Warn("auth audit: WIDE-OPEN spokes detected", "count", len(open), "hives", strings.Join(open, ", "))
	s.pushAuthAuditAlert(msg)
}

// probeWideOpen fetches base+authAuditPath with NO auth. Returns (wideOpen,
// reachable). wideOpen is true only on a 200 (dashboard served to anyone).
// A 30x/401/403 means protected. A transport error means unreachable (e.g. a
// firewalled vllm-d spoke the hub can't reach), never counted as open.
func probeWideOpen(ctx context.Context, client *http.Client, base string) (wideOpen, reachable bool) {
	reqCtx, cancel := context.WithTimeout(ctx, authAuditProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+authAuditPath, nil)
	if err != nil {
		return false, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, true
}

// pushAuthAuditAlert sends a high-priority ntfy notification when a wide-open
// spoke is found, if HIVE_NTFY_SERVER + HIVE_NTFY_TOPIC are configured. It is
// best-effort: failure to alert is logged but never fatal. The log Warn above
// is always emitted regardless, so an external log-based alert still fires.
func (s *HubServer) pushAuthAuditAlert(message string) {
	server := strings.TrimRight(os.Getenv("HIVE_NTFY_SERVER"), "/")
	topic := os.Getenv("HIVE_NTFY_TOPIC")
	if server == "" || topic == "" {
		return // no ntfy configured — the Warn log is the alert channel
	}
	req, err := http.NewRequest(http.MethodPost, server+"/"+topic, strings.NewReader(message))
	if err != nil {
		s.logger.Warn("auth audit: failed to build ntfy request", "error", err)
		return
	}
	req.Header.Set("Title", "Hive: WIDE-OPEN spoke detected")
	req.Header.Set("Priority", "high")
	req.Header.Set("Tags", "warning,lock")
	client := &http.Client{Timeout: authAuditProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("auth audit: ntfy push failed", "error", err)
		return
	}
	_ = resp.Body.Close()
}
