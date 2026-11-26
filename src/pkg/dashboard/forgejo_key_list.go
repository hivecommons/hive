package dashboard

import (
	"errors"
	"fmt"
	"log/slog"
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
//
// A cached document is served to every request; only re-reading the key
// files is rate limited (forgejoKeyListRenderRate).
const (
	forgejoDiscoveryRoute = "/" + forgejwt.DiscoveryPath
	forgejoKeyListRoute   = "/" + forgejwt.KeyListPath

	// forgejoKeyListMaxAge is the Cache-Control max-age of a served
	// document. Forgejo keeps its own cache (CACHE_TTL); this only bounds an
	// intermediary's copy, so a rotation is not held up by it for long.
	forgejoKeyListMaxAge = time.Minute

	// forgejoKeyListRenderRate and forgejoKeyListRenderBurst bound how often
	// the key files are read and parsed again. Only a request that misses the
	// cache (a changed key file, or a key that cannot be read) spends a
	// token; a cached document is always served. Limiting every request
	// instead would let one anonymous client, sharing a reverse proxy's
	// address with Forgejo, keep Forgejo's fetch on 429 and so break every
	// clone and push.
	forgejoKeyListRenderRate  = 1
	forgejoKeyListRenderBurst = 5
	// forgejoKeyListRetryAfterSeconds is the Retry-After of a 429.
	forgejoKeyListRetryAfterSeconds = 1
)

// errKeyListRenderLimited is render's answer when the cache is stale and
// the render budget is spent.
var errKeyListRenderLimited = errors.New("forgejo key list: render rate limit reached")

// forgejoKeyListState holds the rendered documents and the render limiter.
// The zero value is ready to use.
type forgejoKeyListState struct {
	mu          sync.Mutex
	fingerprint string
	discovery   []byte
	jwks        []byte
	renders     *rate.Limiter
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
	discovery, jwks, err := s.forgejoKeys.render(in, s.keyListLogger())
	if errors.Is(err, errKeyListRenderLimited) {
		w.Header().Set("Retry-After", strconv.Itoa(forgejoKeyListRetryAfterSeconds))
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
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

// render returns the two documents, rendering them again only when the
// issuer, the key paths, or any key file's size, mode or modification time has
// changed, so a request does not parse the keys each time. Replacing a key
// file, or the boot keystore sweep sealing it in place, re-renders. A
// re-render spends a token of the render limiter; with none left it returns
// errKeyListRenderLimited.
func (st *forgejoKeyListState) render(in *config.ForgejoIntegrationConfig, logger *slog.Logger) (discovery, jwks []byte, err error) {
	current := strings.TrimSpace(in.KeyFile)
	previous := in.PreviousKeyPaths()
	fp := keyListFingerprint(in.IssuerOrigin(), current, previous)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.fingerprint == fp && st.jwks != nil {
		return st.discovery, st.jwks, nil
	}
	if st.renders == nil {
		st.renders = rate.NewLimiter(rate.Limit(forgejoKeyListRenderRate), forgejoKeyListRenderBurst)
	}
	if !st.renders.Allow() {
		return nil, nil, errKeyListRenderLimited
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
