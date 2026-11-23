package hub

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
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
// stores nothing and logs only the delivery id. A refusal report
// (wire.KindReport, #18699: what hive-mx turned away for a hive and why) is
// routed the same way.
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
	// mailInboundMaxBody bounds a delivery without routing headers (an
	// older hive-mx, JSON only).
	mailInboundMaxBody = 8 << 20
	// envHubMailMaxBody overrides mailInboundDefaultMaxBody, the bound of a
	// routed delivery on this route only (#18564): 30 MB of attachments
	// sent as multipart, plus the delivery JSON.
	envHubMailMaxBody         = "HIVE_HUB_MAIL_MAX_BODY_BYTES"
	mailInboundDefaultMaxBody = 48 << 20
	// envHubMailChunkBytes overrides wire.RelayChunkBytes, the piece size
	// of a chunked relay; it is clamped to [mailMinChunkBytes,
	// wire.MaxRelayChunkBytes].
	envHubMailChunkBytes = "HIVE_HUB_MAIL_CHUNK_BYTES"
	mailMinChunkBytes    = 64 << 10
	// envHubMailSpoolDir is where a delivery is spooled while it is
	// relayed ("" is the system temp directory).
	envHubMailSpoolDir = "HIVE_HUB_MAIL_SPOOL_DIR"
	// mailRelayTimeout bounds one request to the spoke.
	mailRelayTimeout = 30 * time.Second
	// mailRelayTotalTimeout bounds the whole relay of one delivery (every
	// piece of a chunked one); hive-mx waits for the answer so the
	// sender's server can be told to retry.
	mailRelayTotalTimeout = 100 * time.Second
	// mailInboundDeadline is this route's read and write deadline, past
	// the server-wide ones, so a large delivery can be read and relayed.
	mailInboundDeadline = 2 * time.Minute
	// mailSpokeErrBody bounds what is read of a spoke's answer. The reply
	// decision (mail_reply.go) parses it, so it holds a stored mail's ids.
	mailSpokeErrBody = 16 << 10
)

// mailEnvInt64 reads a positive integer, def when unset or invalid.
func mailEnvInt64(key string, def int64) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64); err == nil && n > 0 {
		return n
	}
	return def
}

// mailChunkBytes is the piece size of a chunked relay.
func mailChunkBytes() int64 {
	return min(max(mailEnvInt64(envHubMailChunkBytes, wire.RelayChunkBytes), mailMinChunkBytes), wire.MaxRelayChunkBytes)
}

// mailDeliveryIDRE is the shape of a hive-mx delivery id (16 random bytes).
var mailDeliveryIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// mailRelayClient never follows redirects, for the reason documented on
// spokeWebhookRelayClient. Like that client, it dials only an address
// spokeWebhookRelayGuard passed, so a self-reported DashboardURL whose name
// rebinds between the guard in handleMailInbound and the dial is refused
// instead of reached (#19221).
var mailRelayClient = &http.Client{
	Timeout:       mailRelayTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: pinnedFetchTransport(func(ctx context.Context, rawURL string) bool {
		return spokeWebhookRelayGuard(ctx, rawURL)
	}),
}

func (s *HubServer) registerMailRoutes() {
	// The confirmation reply (#18746) is off unless its DKIM key is mounted.
	s.mailReply = newMailReplierFromEnv(s.logger, s.mailHostMap)
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

// mailEnvelopeFromHeaders is the routing hive-mx sends in headers, ok
// false for an older hive-mx that sends none.
func mailEnvelopeFromHeaders(h http.Header) (mailEnvelope, bool) {
	env := mailEnvelope{ID: h.Get(wire.HeaderDelivery), Kind: wire.Kind(h.Get(wire.HeaderKind)), Recipient: h.Get(wire.HeaderRecipient)}
	return env, env.ID != "" || env.Kind != "" || env.Recipient != ""
}

func mailError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// errMailSpool is a failure to spool a delivery on the hub's own disk.
var errMailSpool = errors.New("mail relay: spool unavailable")

// spoolMailBody copies a delivery body to a temp file, at most max bytes.
func spoolMailBody(w http.ResponseWriter, body io.Reader, limit int64) (*os.File, int64, error) {
	f, err := os.CreateTemp(os.Getenv(envHubMailSpoolDir), "hive-mail-*")
	if err != nil {
		return nil, 0, errMailSpool
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, io.NopCloser(body), limit))
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		var pe *os.PathError
		if errors.As(err, &pe) {
			return nil, 0, errMailSpool
		}
		return nil, 0, err
	}
	return f, n, nil
}

// handleMailInbound routes one hive-mx delivery to its hive. Status codes
// tell hive-mx whether the sender should retry: 4xx (other than 408/429)
// is permanent, everything else temporary.
//
// The body is spooled to disk, never held whole in memory. A files
// delivery that is multipart or larger than one relay piece goes to the
// spoke in signed pieces (relayMailChunks), each under the 1 MiB request
// limit ingress proxies apply by default; anything else is forwarded
// byte for byte as one request.
func (s *HubServer) handleMailInbound(w http.ResponseWriter, r *http.Request) {
	if !gateMail(w, r) {
		return
	}
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(mailInboundDeadline)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)

	env, routed := mailEnvelopeFromHeaders(r.Header)
	ct := r.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(ct)
	isJSON := ct == "" || mediaType == wire.ContentTypeJSON
	switch {
	case isJSON:
	case mediaType == wire.ContentTypeMultipart && routed && params["boundary"] != "" && env.Kind == wire.KindFiles:
	default:
		mailError(w, http.StatusBadRequest, "invalid delivery")
		return
	}
	limit := int64(mailInboundMaxBody)
	if routed {
		limit = mailEnvInt64(envHubMailMaxBody, mailInboundDefaultMaxBody)
	}
	spool, size, err := spoolMailBody(w, r.Body, limit)
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			mailError(w, http.StatusRequestEntityTooLarge, "delivery too large")
		case errors.Is(err, errMailSpool):
			s.logger.Warn("mail relay: cannot spool a delivery", "error", err)
			mailError(w, http.StatusServiceUnavailable, "hub cannot take the delivery now")
		default:
			mailError(w, http.StatusBadRequest, "unreadable body")
		}
		return
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()
	if !routed {
		if err := json.NewDecoder(io.NewSectionReader(spool, 0, size)).Decode(&env); err != nil {
			mailError(w, http.StatusBadRequest, "invalid delivery")
			return
		}
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
	ctx, cancel := context.WithTimeout(r.Context(), mailRelayTotalTimeout)
	defer cancel()
	if spokeWebhookRelayGuard(ctx, dashboardURL) {
		s.logger.Warn("mail relay: refusing private or unresolvable dashboard URL", "delivery", env.ID, "hive_id", hiveID)
		mailError(w, http.StatusBadGateway, "hive not reachable")
		return
	}
	base := strings.TrimRight(dashboardURL, "/")
	chunk := mailChunkBytes()
	var code int
	var answer []byte
	if env.Kind == wire.KindFiles && (!isJSON || size > chunk) {
		if ct == "" {
			ct = wire.ContentTypeJSON
		}
		pieces := (size + chunk - 1) / chunk
		if pieces > wire.MaxRelayChunks {
			mailError(w, http.StatusRequestEntityTooLarge, "delivery too large")
			return
		}
		code, answer, err = relayMailChunks(ctx, base+wire.SpokeFilesChunkPath, h.WebhookRelayKey, env.ID, ct, spool, size, chunk)
	} else {
		var body []byte
		body, err = io.ReadAll(io.NewSectionReader(spool, 0, size))
		if err == nil {
			code, answer, err = relayMailOnce(ctx, base+path, h.WebhookRelayKey, env.ID, wire.ContentTypeJSON, nil, body)
		}
	}
	if err != nil {
		s.logger.Warn("mail relay: spoke unreachable", "delivery", env.ID, "hive_id", hiveID, "error", err)
		mailError(w, http.StatusBadGateway, "hive not reachable")
		return
	}
	s.logger.Info("mail relay: delivered", "delivery", env.ID, "hive_id", hiveID, "kind", string(env.Kind), "status", code, "bytes", size)
	switch {
	case code >= 200 && code < 300:
		w.WriteHeader(http.StatusAccepted)
		if env.Kind == wire.KindFiles {
			s.mailReply.dispatch(mailReplyJob{HiveID: hiveID, Host: host, Spool: spool, Size: size, MediaType: mediaType, Boundary: params["boundary"], Answer: answer})
		}
	case code == http.StatusUnauthorized, code == http.StatusNotFound, code == http.StatusMethodNotAllowed:
		// A relay key the spoke does not hold yet, or a spoke that predates
		// the endpoint: temporary from the sender's point of view.
		mailError(w, http.StatusBadGateway, "hive did not accept the delivery yet")
	case code == http.StatusRequestTimeout, code == http.StatusTooManyRequests, code >= 500:
		mailError(w, http.StatusBadGateway, "hive unavailable")
	case code == http.StatusRequestEntityTooLarge:
		mailError(w, http.StatusRequestEntityTooLarge, "hive refused the delivery as too large")
	default:
		mailError(w, http.StatusUnprocessableEntity, "hive refused the delivery")
	}
}

// relayMailOnce posts one signed request to the spoke and returns its
// status and the start of its answer (read by the reply decision only).
// delivery is the relay delivery id the signature binds.
func relayMailOnce(ctx context.Context, target, key, delivery, contentType string, extra http.Header, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range extra {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(mailRelayDeliveryHeader, delivery)
	signWebhookRelayHeaders(req.Header, key, time.Now(), delivery, body)
	resp, err := mailRelayClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, mailSpokeErrBody))
	_ = resp.Body.Close()
	return resp.StatusCode, answer, nil
}

// relayMailChunks sends a spooled files delivery to the spoke in pieces of
// at most chunk bytes, in order, each signed with its own relay delivery
// id (wire.ChunkDelivery). The spoke stores the pieces and processes the
// whole body when the last one arrives; a piece it refuses stops the
// relay, and its status is the answer.
func relayMailChunks(ctx context.Context, target, key, id, bodyType string, spool io.ReaderAt, size, chunk int64) (int, []byte, error) {
	pieces := int((size + chunk - 1) / chunk)
	if pieces == 0 {
		pieces = 1
	}
	extra := http.Header{}
	extra.Set(wire.HeaderBodyType, bodyType)
	buf := make([]byte, chunk)
	code := 0
	var answer []byte
	for i := 0; i < pieces; i++ {
		off := int64(i) * chunk
		n, err := spool.ReadAt(buf[:min(chunk, size-off)], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, nil, err
		}
		code, answer, err = relayMailOnce(ctx, target, key, wire.ChunkDelivery(id, i, pieces), "application/octet-stream", extra, buf[:n])
		if err != nil {
			return 0, nil, err
		}
		if code < 200 || code >= 300 {
			return code, answer, nil
		}
	}
	return code, answer, nil
}
