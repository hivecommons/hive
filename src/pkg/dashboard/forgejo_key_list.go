package dashboard

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hivecommons/hive/crux/forgejwt"
	"github.com/hivecommons/hive/pkg/config"
)

// The spoke can serve its own forgejo.integration key list, so the host
// Forgejo fetches it from is up whenever the hive that mints the tokens is
// (hivecommons/hive#19996). It is opt-in through
// forgejo.integration.serve_key_list; with it off both paths are ordinary
// authenticated paths that answer 404.
//
// The documents hold public keys only, like any JWKS document, so the routes
// are anonymous: Forgejo fetches them with no credential and follows no
// redirect. isPublicPath admits exactly these two paths, and only while the
// setting is on; the hub's publicExactPaths admit them on a hosted spoke so
// the ingress gate does not bounce Forgejo to a login page, and this handler
// still 404s there unless the setting is on.
const (
	forgejoDiscoveryRoute = "/" + forgejwt.DiscoveryPath
	forgejoKeyListRoute   = "/" + forgejwt.KeyListPath

	// forgejoKeyListMaxAge is the Cache-Control max-age of a served
	// document. Forgejo keeps its own cache (CACHE_TTL); this only bounds an
	// intermediary's copy, so a rotation is not held up by it for long.
	forgejoKeyListMaxAge = time.Minute

	// forgejoKeyListRate and forgejoKeyListBurst bound requests per client
	// address. Forgejo fetches each document once per CACHE_TTL, so these are
	// far above any real use.
	forgejoKeyListRate  = 5
	forgejoKeyListBurst = 20
	// forgejoKeyListMaxClients bounds the per-address limiter table; past it
	// the table is cleared rather than grown.
	forgejoKeyListMaxClients = 1024
	// forgejoKeyListRetryAfterSeconds is the Retry-After of a 429.
	forgejoKeyListRetryAfterSeconds = 1
)

// forgejoKeyListState holds the rendered documents and the rate limiter.
// The zero value is ready to use.
type forgejoKeyListState struct {
	mu          sync.Mutex
	fingerprint string
	discovery   []byte
	jwks        []byte
	limiters    map[string]*rate.Limiter
}

// forgejoServedIntegration returns the forgejo.integration block when it
// asks the spoke to serve the key list, else nil.
func (s *Server) forgejoServedIntegration() *config.ForgejoIntegrationConfig {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil
	}
	in := s.deps.Config.Forgejo.Integration
	if in == nil || !in.ServeKeyList {
		return nil
	}
	return in
}

// isForgejoKeyListPath reports whether path is one of the two served paths.
func isForgejoKeyListPath(path string) bool {
	return path == forgejoDiscoveryRoute || path == forgejoKeyListRoute
}

func (s *Server) handleForgejoKeyList(w http.ResponseWriter, r *http.Request) {
	in := s.forgejoServedIntegration()
	if in == nil || !isForgejoKeyListPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	// The mux pairs HEAD with a GET pattern; serve GET only.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.forgejoKeys.allow(clientAddress(r)) {
		w.Header().Set("Retry-After", strconv.Itoa(forgejoKeyListRetryAfterSeconds))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	discovery, jwks, err := s.forgejoKeys.render(in, s.keyListLogger())
	if err != nil {
		// The error names key paths; it goes to the log only.
		s.keyListLogger().Warn("forgejo key list: cannot render the served key list; Forgejo will refuse new tokens until it can", "error", err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "key list unavailable", http.StatusServiceUnavailable)
		return
	}
	body := jwks
	if r.URL.Path == forgejoDiscoveryRoute {
		body = discovery
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(forgejoKeyListMaxAge/time.Second)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

func (s *Server) keyListLogger() *slog.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// clientAddress is the request's peer address without the port. Behind a
// reverse proxy every request shares the proxy's address, so the limit is
// then per proxy.
func clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (st *forgejoKeyListState) allow(client string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.limiters == nil || len(st.limiters) >= forgejoKeyListMaxClients {
		st.limiters = make(map[string]*rate.Limiter)
	}
	l, ok := st.limiters[client]
	if !ok {
		l = rate.NewLimiter(rate.Limit(forgejoKeyListRate), forgejoKeyListBurst)
		st.limiters[client] = l
	}
	return l.Allow()
}

// render returns the two documents, rendering them again only when the
// issuer, the key paths, or any key file's size, mode or modification time has
// changed, so a request does not parse the keys each time. Replacing a key
// file, or the boot keystore sweep sealing it in place, re-renders.
func (st *forgejoKeyListState) render(in *config.ForgejoIntegrationConfig, logger *slog.Logger) (discovery, jwks []byte, err error) {
	current := strings.TrimSpace(in.KeyFile)
	previous := in.PreviousKeyPaths()
	fp := keyListFingerprint(in.IssuerOrigin(), current, previous)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fingerprint == fp && st.jwks != nil {
		return st.discovery, st.jwks, nil
	}
	iss, err := forgejwt.ParseIssuer(in.IssuerOrigin())
	if err != nil {
		return nil, nil, err
	}
	discovery, jwks, skipped, err := forgejwt.KeyListFromFiles(iss, current, previous)
	if err != nil {
		st.fingerprint, st.discovery, st.jwks = "", nil, nil
		return nil, nil, err
	}
	for _, e := range skipped {
		logger.Warn("forgejo key list: previous_key_files entry left out of the served key list", "error", e)
	}
	st.fingerprint, st.discovery, st.jwks = fp, discovery, jwks
	return discovery, jwks, nil
}

// keyListFingerprint identifies the inputs of a rendered key list.
func keyListFingerprint(issuer, current string, previous []string) string {
	var b strings.Builder
	b.WriteString(issuer)
	for _, p := range append([]string{current}, previous...) {
		b.WriteString("\x00")
		b.WriteString(p)
		if info, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "\x00%d\x00%d\x00%o", info.Size(), info.ModTime().UnixNano(), info.Mode())
		} else {
			b.WriteString("\x00missing")
		}
	}
	return b.String()
}
