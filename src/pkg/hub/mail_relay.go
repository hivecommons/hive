package hub

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

// Mail routing for hive-mx, the cluster's receive-only mail receiver
// (hivecommons/hive#18395, plan #18391 "Email invites").
//
// hive-mx holds no per-hive secret. It asks the hub which hosts it may accept
// hive@<host> for (GET /api/mail/hosts) and posts each parsed delivery to
// POST /api/mail/inbound. The hub maps the recipient host to a hosted hive,
// signs the body with that hive's relay key exactly as it signs relayed
// GitHub webhooks (signWebhookRelayHeaders), with the delivery id in
// X-Hive-Relay-Delivery, and forwards it to the spoke behind the same
// private-address guard and no-redirect client as the webhook relay. The hub
// stores nothing and logs only the delivery id.
//
// Both endpoints need the shared bearer token HIVE_HUB_MAIL_TOKEN (the same
// value hive-mx mounts from its Secret); without it they answer 404, so a hub
// that does not run hive-mx exposes nothing.
const (
	// envHubMailToken is the bearer token hive-mx authenticates with.
	envHubMailToken = "HIVE_HUB_MAIL_TOKEN"
	// mailRelayDeliveryHeader carries a non-GitHub relay's delivery id; the
	// spoke binds the relay signature to it (pkg/dashboard/webhook_relay.go).
	mailRelayDeliveryHeader = "X-Hive-Relay-Delivery"
	// mailInboundMaxBody bounds one delivery: a 2 MB message is at most
	// about 2.7 MB of base64 attachments plus the JSON around them.
	mailInboundMaxBody = 8 << 20
	// mailRelayTimeout bounds the forward to the spoke; hive-mx waits for
	// the answer so the sender's server can be told to retry.
	mailRelayTimeout = 30 * time.Second
	// mailSpokeErrBody bounds what is read of a spoke's answer.
	mailSpokeErrBody = 4096
)

// mailDeliveryIDRE is the shape of a hive-mx delivery id (16 random bytes).
var mailDeliveryIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// mailRelayClient never follows redirects, for the reason documented on
// spokeWebhookRelayClient.
var mailRelayClient = &http.Client{
	Timeout:       mailRelayTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (s *HubServer) registerMailRoutes() {
	s.mux.HandleFunc("GET "+wire.HostsPath, s.handleMailHosts)
	s.mux.HandleFunc("POST "+wire.InboundPath, s.handleMailInbound)
}

// mailAuthorized checks the hive-mx bearer token in constant time. An unset
// token disables the endpoints.
func mailAuthorized(r *http.Request) (enabled, ok bool) {
	want := strings.TrimSpace(os.Getenv(envHubMailToken))
	if want == "" {
		return false, false
	}
	got, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return true, false
	}
	return true, subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1
}

// gateMail answers 404 while mail is off and 401 for a bad token; it
// returns true when the request may proceed.
func gateMail(w http.ResponseWriter, r *http.Request) bool {
	enabled, ok := mailAuthorized(r)
	switch {
	case !enabled:
		http.NotFound(w, r)
		return false
	case !ok:
		w.Header().Set("WWW-Authenticate", `Bearer realm="hive-mx"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// hostOf is the lower-cased host of a URL or bare host name, "" if none.
func hostOf(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// mailHostMap maps every host a hosted hive answers on (its subdomain, its
// vanity URL and the dashboard URL its spoke reports) to the hive id. A host
// claimed by two hives is dropped: mail for it cannot be routed safely.
func (s *HubServer) mailHostMap() map[string]string {
	dashboards := map[string]string{}
	s.mu.RLock()
	for _, e := range s.registry.Hives {
		if e.ID != "" && e.DashboardURL != "" {
			dashboards[e.ID] = e.DashboardURL
		}
	}
	s.mu.RUnlock()

	out := map[string]string{}
	conflict := map[string]bool{}
	add := func(host, id string) {
		if host == "" || conflict[host] {
			return
		}
		if prev, ok := out[host]; ok && prev != id {
			delete(out, host)
			conflict[host] = true
			return
		}
		out[host] = id
	}
	for _, h := range listSaaSHives() {
		if h.ID == "" {
			continue
		}
		add(hostOf(h.Subdomain), h.ID)
		add(hostOf(h.VanityURL), h.ID)
		add(hostOf(dashboards[h.ID]), h.ID)
	}
	return out
}

// handleMailHosts lists the hosts hive-mx accepts hive@<host> for.
func (s *HubServer) handleMailHosts(w http.ResponseWriter, r *http.Request) {
	if !gateMail(w, r) {
		return
	}
	m := s.mailHostMap()
	hosts := make([]string, 0, len(m))
	for h := range m {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(wire.HostsResponse{Hosts: hosts})
}

// mailEnvelope is the part of a delivery the hub reads to route it; the
// rest is forwarded untouched.
type mailEnvelope struct {
	ID        string    `json:"id"`
	Kind      wire.Kind `json:"kind"`
	Recipient string    `json:"recipient"`
}

func mailError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleMailInbound routes one hive-mx delivery to its hive. Status codes
// tell hive-mx whether the sender should retry: 4xx (other than 408/429)
// is permanent, everything else temporary.
func (s *HubServer) handleMailInbound(w http.ResponseWriter, r *http.Request) {
	if !gateMail(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mailInboundMaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			mailError(w, http.StatusRequestEntityTooLarge, "delivery too large")
			return
		}
		mailError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	var env mailEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		mailError(w, http.StatusBadRequest, "invalid delivery")
		return
	}
	path := wire.SpokePath(env.Kind)
	host := wire.RecipientHost(env.Recipient)
	if !mailDeliveryIDRE.MatchString(env.ID) || path == "" || host == "" {
		mailError(w, http.StatusBadRequest, "invalid delivery")
		return
	}
	hiveID, ok := s.mailHostMap()[host]
	if !ok {
		s.logger.Info("mail relay: no hive for recipient", "delivery", env.ID)
		mailError(w, http.StatusNotFound, "no such hive")
		return
	}
	h := loadSaaSHive(hiveID)
	if h == nil {
		mailError(w, http.StatusNotFound, "no such hive")
		return
	}
	dashboardURL := ""
	s.mu.RLock()
	for _, e := range s.registry.Hives {
		if e.ID == hiveID {
			dashboardURL = e.DashboardURL
			break
		}
	}
	s.mu.RUnlock()
	if h.WebhookRelayKey == "" || dashboardURL == "" {
		// The spoke has not heartbeated since the relay key was minted, or
		// has never reported its dashboard: worth retrying later.
		s.logger.Info("mail relay: hive not reachable yet", "delivery", env.ID, "hive_id", hiveID)
		mailError(w, http.StatusServiceUnavailable, "hive not reachable yet")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailRelayTimeout)
	defer cancel()
	if spokeWebhookRelayGuard(ctx, dashboardURL) {
		s.logger.Warn("mail relay: refusing private or unresolvable dashboard URL", "delivery", env.ID, "hive_id", hiveID)
		mailError(w, http.StatusBadGateway, "hive not reachable")
		return
	}
	spokeURL := strings.TrimRight(dashboardURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, spokeURL, bytes.NewReader(body))
	if err != nil {
		mailError(w, http.StatusBadGateway, "hive not reachable")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(mailRelayDeliveryHeader, env.ID)
	signWebhookRelayHeaders(req.Header, h.WebhookRelayKey, time.Now(), env.ID, body)
	resp, err := mailRelayClient.Do(req)
	if err != nil {
		s.logger.Warn("mail relay: spoke unreachable", "delivery", env.ID, "hive_id", hiveID, "error", err)
		mailError(w, http.StatusBadGateway, "hive not reachable")
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, mailSpokeErrBody))
	_ = resp.Body.Close()
	s.logger.Info("mail relay: delivered", "delivery", env.ID, "hive_id", hiveID, "kind", string(env.Kind), "status", resp.StatusCode)
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		w.WriteHeader(http.StatusAccepted)
	case code == http.StatusUnauthorized, code == http.StatusNotFound, code == http.StatusMethodNotAllowed:
		// A relay key the spoke does not hold yet, or a spoke that predates
		// the endpoint: temporary from the sender's point of view.
		mailError(w, http.StatusBadGateway, "hive did not accept the delivery yet")
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
		mailError(w, http.StatusBadGateway, "hive unavailable")
	default:
		mailError(w, http.StatusUnprocessableEntity, "hive refused the delivery")
	}
}
