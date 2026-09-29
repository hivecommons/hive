package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// NPS (Net Promoter Score) feedback, spoke side (issue #9610).
//
// The dashboard shows signed-in, non-read-only users an occasional 4-point
// "how is the hive working for you?" prompt. A response is posted here, to the
// spoke's own backend, which validates and rate-limits it and forwards it to
// the hub over the spoke's existing authenticated hub link: the same per-hive
// heartbeat bearer /api/heartbeat and /api/task-status present. The hub keeps
// it for hub admins only.
//
// What is forwarded: this hive's ID, the 1-4 score, the optional free text and
// the dashboard version. What is NEVER forwarded: who answered. The username is
// used only locally, as the rate-limit key, and is not in the hub payload.
//
// Off by default everywhere except hosted spokes (config.HubConfig
// .NPSFeedbackEnabled). A standalone hive (no hub link) that opts in forwards
// to the hivecommons NPS relay instead (issue #9619), but only when the
// operator has also configured a relay URL (config.HubConfig
// .NPSRelayConfigured). The relay needs no per-install credential from the
// operator: the spoke generates and self-registers its own signing key on
// first use (nps_relay.go). Without opt-in or a relay URL it sends nothing
// anywhere. The same payload, caps and rate limits apply on both paths.

const (
	// npsScoreMin / npsScoreMax bound the 4-point emoji scale.
	npsScoreMin = 1
	npsScoreMax = 4
	// npsMaxBodyBytes caps the bytes READ from a submission, whatever
	// Content-Length says (a chunked body has none), per console #16666.
	npsMaxBodyBytes = 4096
	// npsMaxFeedbackRunes caps the free-text follow-up. The UI enforces the
	// same limit; the server re-applies it.
	npsMaxFeedbackRunes = 500

	// npsRateWindow is the window for both rate limits below.
	npsRateWindow = 24 * time.Hour
	// npsMaxPerUserPerWindow: one response per signed-in user per window.
	npsMaxPerUserPerWindow = 1
	// npsMaxPerHivePerWindow bounds a multi-user hive's total per window. The
	// hub enforces its own per-hive cap as well; this keeps a noisy hive from
	// even reaching it.
	npsMaxPerHivePerWindow = 10

	// npsForwardTimeout bounds the hub or relay round-trip. The user is
	// waiting on it.
	npsForwardTimeout = 10 * time.Second
	// npsMaxHubResponseBytes caps how much of the hub's or relay's reply is
	// read.
	npsMaxHubResponseBytes = 1 << 12
	// npsHubIngestPath is the hub route responses are forwarded to
	// (pkg/hub nps.go npsIngestPath; pkg/dashboard cannot import pkg/hub).
	npsHubIngestPath = "/api/nps/ingest"

	npsCategoryPromoter  = "promoter"
	npsCategoryPassive   = "passive"
	npsCategoryDetractor = "detractor"
	// npsPromoterMin / npsPassiveMin mirror the hub's bucketing.
	npsPromoterMin = 4
	npsPassiveMin  = 2
)

// npsCategory buckets a raw 1-4 score: 4 promoter, 2-3 passive, 1 detractor.
func npsCategory(score int) string {
	switch {
	case score >= npsPromoterMin:
		return npsCategoryPromoter
	case score >= npsPassiveMin:
		return npsCategoryPassive
	default:
		return npsCategoryDetractor
	}
}

// npsRateLimiter tracks accepted submissions in memory. A restart forgets it,
// which is acceptable: the browser also backs off for 30 days after a
// submission, and the hub enforces its own durable per-hive cap.
type npsRateLimiter struct {
	mu    sync.Mutex
	users map[string][]time.Time
	hive  []time.Time
}

var (
	errNPSUserLimited = errors.New("you already sent feedback recently - thank you")
	errNPSHiveLimited = errors.New("this hive has sent its share of feedback for today - please try again tomorrow")
)

// npsPrune drops times before cutoff.
func npsPrune(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, at := range times {
		if !at.Before(cutoff) {
			kept = append(kept, at)
		}
	}
	return kept
}

// npsRemoveOne drops one occurrence of at.
func npsRemoveOne(times []time.Time, at time.Time) []time.Time {
	for i := range times {
		if times[i].Equal(at) {
			return append(times[:i], times[i+1:]...)
		}
	}
	return times
}

// reserve claims a slot for user at now, or reports which limit is hit. The
// returned release undoes the claim, for a submission that then fails, so a
// hub outage does not burn the user's quota. Reserving (rather than checking
// and recording separately) keeps two concurrent submissions from both
// passing the check.
func (l *npsRateLimiter) reserve(user string, now time.Time) (release func(), err error) {
	return l.reserveWith(user, now, npsMaxPerUserPerWindow, npsMaxPerHivePerWindow)
}

// reserveWith is reserve with explicit per-user and per-hive caps over
// npsRateWindow, so the detractor-issue path (nps_extras.go) reuses the same
// limiter logic with its own, tighter caps.
func (l *npsRateLimiter) reserveWith(user string, now time.Time, perUser, perHive int) (release func(), err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-npsRateWindow)
	if l.users == nil {
		l.users = map[string][]time.Time{}
	}
	for u, times := range l.users {
		if kept := npsPrune(times, cutoff); len(kept) > 0 {
			l.users[u] = kept
		} else {
			delete(l.users, u)
		}
	}
	l.hive = npsPrune(l.hive, cutoff)

	if len(l.users[user]) >= perUser {
		return nil, errNPSUserLimited
	}
	if len(l.hive) >= perHive {
		return nil, errNPSHiveLimited
	}
	l.users[user] = append(l.users[user], now)
	l.hive = append(l.hive, now)
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if left := npsRemoveOne(l.users[user], now); len(left) > 0 {
			l.users[user] = left
		} else {
			delete(l.users, user)
		}
		l.hive = npsRemoveOne(l.hive, now)
	}, nil
}

// npsSanitizeFeedback trims, drops control characters other than newline,
// and caps the length in runes.
func npsSanitizeFeedback(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c == '\r' {
			continue
		}
		if c != '\n' && (unicode.IsControl(c) || c == unicode.ReplacementChar) {
			continue
		}
		b.WriteRune(c)
	}
	out := strings.TrimSpace(b.String())
	if runes := []rune(out); len(runes) > npsMaxFeedbackRunes {
		out = strings.TrimSpace(string(runes[:npsMaxFeedbackRunes]))
	}
	return out
}

// npsActor resolves the signed-in user the same way handleBannerDismissed
// does: a direct-route session at its LIVE role, else the hub-proxied identity
// the authenticate middleware has already verified. "" means not signed in.
func (s *Server) npsActor(r *http.Request) (username, role string) {
	if sess := s.sessionFromRequest(r); sess != nil {
		if live, ok := s.liveSessionRole(sess); ok {
			return sess.Username, live
		}
		return "", ""
	}
	if hubUser := r.Header.Get("X-Hive-User"); hubUser != "" {
		return hubUser, r.Header.Get("X-Hive-Role")
	}
	return "", ""
}

// npsEnabled reports whether this hive may prompt for and forward NPS
// feedback: the operator opt-in (hosted spokes default on) AND somewhere to
// forward to - a hub link, or for a standalone hive a configured relay.
func (s *Server) npsEnabled() bool {
	if s.deps == nil || s.deps.Config == nil {
		return false
	}
	hub := s.deps.Config.Hub
	if !hub.NPSFeedbackEnabled() || strings.TrimSpace(s.deps.Config.HiveID) == "" {
		return false
	}
	return hub.NPSHubLinked() || hub.NPSRelayConfigured()
}

// npsStatusResponse tells the dashboard whether to run the prompt at all.
type npsStatusResponse struct {
	Enabled          bool `json:"enabled"`
	CanSubmit        bool `json:"can_submit"`
	MaxFeedbackChars int  `json:"max_feedback_chars"`
	// Timing, GA4MeasurementID and DetractorIssue are set only when
	// CanSubmit is true; see npsStatusExtras in nps_extras.go.
	Timing           *npsTimingPayload        `json:"timing,omitempty"`
	GA4MeasurementID string                   `json:"ga4_measurement_id,omitempty"`
	DetractorIssue   *npsDetractorIssueStatus `json:"detractor_issue,omitempty"`
}

// handleNPSStatus answers GET /api/feedback/nps/status. The dashboard only
// starts its eligibility clock when can_submit is true, so a disabled hive, an
// anonymous viewer and a read-only user never see the prompt.
func (s *Server) handleNPSStatus(w http.ResponseWriter, r *http.Request) {
	enabled := s.npsEnabled()
	user, role := s.npsActor(r)
	resp := npsStatusResponse{
		Enabled:          enabled,
		CanSubmit:        enabled && user != "" && role != "read",
		MaxFeedbackChars: npsMaxFeedbackRunes,
	}
	if resp.CanSubmit {
		s.npsStatusExtras(&resp)
	}
	jsonResponse(w, resp)
}

// npsSubmitRequest is the browser's body. Score is a pointer so a missing
// score is distinguishable from an invalid one in the error message.
type npsSubmitRequest struct {
	Score    *int   `json:"score"`
	Feedback string `json:"feedback,omitempty"`
}

// npsHubPayload is exactly what reaches the hub or the relay. There is no
// user field.
type npsHubPayload struct {
	HiveID           string `json:"hive_id"`
	Score            int    `json:"score"`
	Feedback         string `json:"feedback,omitempty"`
	DashboardVersion string `json:"dashboard_version,omitempty"`
}

// handleNPSSubmit answers POST /api/feedback/nps.
func (s *Server) handleNPSSubmit(w http.ResponseWriter, r *http.Request) {
	user, role := s.npsActor(r)
	if user == "" {
		jsonError(w, "you must be signed in to send feedback", http.StatusForbidden)
		return
	}
	// Defense in depth: roleEnforcement already refuses a read-only POST.
	if role == "read" {
		jsonError(w, "read-only users cannot send feedback", http.StatusForbidden)
		return
	}
	if !s.npsEnabled() {
		jsonError(w, "NPS feedback is not enabled on this hive", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, npsMaxBodyBytes+1))
	if err != nil {
		jsonError(w, "could not read request", http.StatusBadRequest)
		return
	}
	if len(body) > npsMaxBodyBytes {
		jsonError(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req npsSubmitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Score == nil || *req.Score < npsScoreMin || *req.Score > npsScoreMax {
		jsonError(w, fmt.Sprintf("score must be %d-%d", npsScoreMin, npsScoreMax), http.StatusBadRequest)
		return
	}
	score := *req.Score

	release, err := s.nps.reserve(strings.ToLower(user), time.Now())
	if err != nil {
		jsonError(w, err.Error(), http.StatusTooManyRequests)
		return
	}

	payload := npsHubPayload{
		HiveID:           strings.TrimSpace(s.deps.Config.HiveID),
		Score:            score,
		Feedback:         npsSanitizeFeedback(req.Feedback),
		DashboardVersion: versionShort,
	}
	if status, err := s.forwardNPS(r, payload); err != nil {
		release()
		if s.logger != nil {
			s.logger.Warn("nps: forward failed", "via", s.npsRoute(), "error", err, "upstream_status", status)
		}
		if status == http.StatusTooManyRequests {
			jsonError(w, errNPSHiveLimited.Error(), http.StatusTooManyRequests)
			return
		}
		jsonError(w, "could not deliver feedback - please try again later", http.StatusBadGateway)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "category": npsCategory(score)})
}

// npsRouteHub / npsRouteRelay name the two forwarding paths in logs.
const (
	npsRouteHub   = "hub"
	npsRouteRelay = "relay"
)

// npsRoute reports which path a response takes: the hub whenever the spoke
// has a hub link (a hub-linked hive never uses the relay), else the relay.
func (s *Server) npsRoute() string {
	if s.deps.Config.Hub.NPSHubLinked() {
		return npsRouteHub
	}
	return npsRouteRelay
}

// npsNoRedirectClient never follows a redirect: the hub bearer and the
// relay's signed requests must reach only the configured endpoint, and a 3xx
// is a failure here.
func npsNoRedirectClient() *http.Client {
	return &http.Client{
		Timeout: npsForwardTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// npsIsSuccess reports whether an upstream status is 2xx.
func npsIsSuccess(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// forwardNPS POSTs payload to the hub with the spoke's per-hive heartbeat
// bearer or, for a standalone hive, to the relay as a request signed with
// this install's self-registered key (nps_relay.go). It sends no cookie and
// no identity header: the request is built fresh, never copied from the
// browser's. Returns the upstream status (0 when no response) and an error
// for anything but 2xx.
func (s *Server) forwardNPS(r *http.Request, payload npsHubPayload) (int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	hub := s.deps.Config.Hub
	if s.npsRoute() == npsRouteRelay {
		relayURL := hub.EffectiveNPSRelayURL()
		if relayURL == "" {
			return 0, errors.New("no NPS relay configured (hub.nps_relay_url)")
		}
		return npsRelayForward(r.Context(), npsNoRedirectClient(), relayURL, data)
	}
	bearer := spoke.SpokeHeartbeatKey()
	if bearer == "" {
		return 0, errors.New("no hub credential (HIVE_HEARTBEAT_KEY / HIVE_HUB_SECRET) configured")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(hub.URL), "/") + npsHubIngestPath
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := npsNoRedirectClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("post: %w", err)
	}
	defer closeHTTPBody(resp.Body)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, npsMaxHubResponseBytes))
	if !npsIsSuccess(resp.StatusCode) {
		return resp.StatusCode, fmt.Errorf("upstream answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
