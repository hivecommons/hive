package github

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	restAccountingWindow        = time.Hour
	defaultRESTBudgetFloor      = 500
	lowValueRESTBudgetFloor     = defaultRESTBudgetFloor
	githubCoreRateLimitResource = "core"
	// unauthenticatedCoreRateLimit is GitHub's per-IP core limit for requests
	// without credentials; any authenticated bucket is far larger.
	unauthenticatedCoreRateLimit = 60
	agentRESTCallerPrefix        = "agent:"
)

type RESTConsumer struct {
	Caller      string    `json:"caller"`
	Endpoint    string    `json:"endpoint"`
	Method      string    `json:"method"`
	Requests    int       `json:"requests"`
	Charged     int       `json:"charged"`
	NotModified int       `json:"not_modified"`
	RateLimited int       `json:"rate_limited"`
	LastStatus  int       `json:"last_status"`
	LastSeen    time.Time `json:"last_seen"`
}

type restAccountingEvent struct {
	at          time.Time
	caller      string
	method      string
	endpoint    string
	status      int
	charged     bool
	notModified bool
	rateLimited bool
}

type restAccountingStore struct {
	mu            sync.Mutex
	events        []restAccountingEvent
	now           func() time.Time
	coreRemaining int
	coreLimit     int
	coreReset     time.Time
	coreObserved  time.Time
}

var sharedRESTAccountingPtr atomic.Pointer[restAccountingStore]

func init() {
	sharedRESTAccountingPtr.Store(&restAccountingStore{now: time.Now, coreRemaining: -1})
}

// sharedRESTAccounting returns the process-wide store. The pointer is atomic so
// tests can swap it while transports in other goroutines are still recording.
func sharedRESTAccounting() *restAccountingStore { return sharedRESTAccountingPtr.Load() }

type restAccountingTransport struct{ inner http.RoundTripper }

func restAccountingWrap(inner http.RoundTripper) http.RoundTripper {
	return &restAccountingTransport{inner: inner}
}

func (t *restAccountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if resp != nil {
		RecordRESTRequest(restCallerFromContext(req.Context()), req.Method, req.URL.Path, resp.StatusCode, resp.Header)
	}
	return resp, err
}

func RecordRESTRequest(caller, method, path string, status int, h http.Header) {
	recordRESTRequest(caller, method, path, status, h, !strings.HasPrefix(caller, agentRESTCallerPrefix))
}

// RecordInjectedRESTRequest records an agent's request that was made with the
// daemon's injected credential, so its rate-limit headers describe the
// daemon's own core bucket and may update the shared snapshot.
func RecordInjectedRESTRequest(caller, method, path string, status int, h http.Header) {
	recordRESTRequest(caller, method, path, status, h, true)
}

func recordRESTRequest(caller, method, path string, status int, h http.Header, daemonCredential bool) {
	if strings.TrimSpace(caller) == "" {
		caller = "unknown"
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	endpoint := restEndpointTemplate(path)
	notModified := status == http.StatusNotModified || h.Get(etagCacheHeader) == "revalidated"
	rateLimited := status == http.StatusTooManyRequests || (status == http.StatusForbidden && h.Get("X-RateLimit-Remaining") == "0")
	charged := !notModified && endpoint != "/rate_limit"
	store := sharedRESTAccounting()
	store.record(restAccountingEvent{
		at:          store.now(),
		caller:      caller,
		method:      method,
		endpoint:    endpoint,
		status:      status,
		charged:     charged,
		notModified: notModified,
		rateLimited: rateLimited,
	}, h, daemonCredential)
}

func RESTTopConsumers(limit int) []RESTConsumer {
	return sharedRESTAccounting().top(limit)
}

func LowValueRESTWorkAllowed() bool {
	remaining, _, ok := sharedRESTAccounting().coreSnapshot()
	return !ok || remaining > lowValueRESTBudgetFloor
}

func CoreRateLimitSnapshot() (remaining int, reset time.Time, ok bool) {
	return sharedRESTAccounting().coreSnapshot()
}

func (s *restAccountingStore) record(ev restAccountingEvent, h http.Header, daemonCredential bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	s.pruneLocked(ev.at)
	if !daemonCredential {
		// A pass-through agent credential is a different bucket; it is kept
		// in the per-caller events above but never moves the shared snapshot.
		return
	}
	if strings.EqualFold(h.Get("X-RateLimit-Resource"), githubCoreRateLimitResource) || h.Get("X-RateLimit-Resource") == "" {
		if !s.sameCoreBucketLocked(ev.caller, h) {
			return
		}
		if rem, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
			s.coreRemaining = rem
			s.coreObserved = ev.at
		}
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			s.coreReset = time.Unix(reset, 0)
		}
	}
}

// sameCoreBucketLocked reports whether a response's rate-limit headers
// describe the hive's own core bucket, so only those may move the shared
// snapshot that drives API budget mode and the agent reserve-floor 429.
//
// Agent traffic relayed by the proxy is recorded here too, and an agent whose
// gh/curl runs without the App token (or with a credential GitHub rejects)
// gets the per-IP unauthenticated bucket back: X-RateLimit-Limit 60 with a
// few dozen remaining. Taken at face value that reading put the hive into
// "critical" budget mode and refused every agent read for up to an hour
// right after each restart, even though the real bucket had ~14k left.
func (s *restAccountingStore) sameCoreBucketLocked(caller string, h http.Header) bool {
	limitHeader := strings.TrimSpace(h.Get("X-RateLimit-Limit"))
	if limitHeader == "" {
		return true
	}
	limit, err := strconv.Atoi(limitHeader)
	if err != nil {
		return true
	}
	if limit <= unauthenticatedCoreRateLimit {
		return false
	}
	if strings.HasPrefix(caller, agentRESTCallerPrefix) {
		return s.coreLimit <= 0 || limit == s.coreLimit
	}
	s.coreLimit = limit
	return true
}

func (s *restAccountingStore) top(limit int) []RESTConsumer {
	if limit <= 0 {
		limit = 10
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	byKey := make(map[string]*RESTConsumer)
	for _, ev := range s.events {
		key := ev.caller + "\x00" + ev.method + "\x00" + ev.endpoint
		c := byKey[key]
		if c == nil {
			c = &RESTConsumer{Caller: ev.caller, Method: ev.method, Endpoint: ev.endpoint}
			byKey[key] = c
		}
		c.Requests++
		if ev.charged {
			c.Charged++
		}
		if ev.notModified {
			c.NotModified++
		}
		if ev.rateLimited {
			c.RateLimited++
		}
		if ev.at.After(c.LastSeen) {
			c.LastSeen = ev.at
			c.LastStatus = ev.status
		}
	}
	out := make([]RESTConsumer, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Charged != out[j].Charged {
			return out[i].Charged > out[j].Charged
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Endpoint < out[j].Endpoint
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *restAccountingStore) coreSnapshot() (remaining int, reset time.Time, ok bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coreRemaining < 0 || now.Sub(s.coreObserved) > restAccountingWindow {
		return 0, time.Time{}, false
	}
	return s.coreRemaining, s.coreReset, true
}

func (s *restAccountingStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-restAccountingWindow)
	idx := 0
	for idx < len(s.events) && s.events[idx].at.Before(cutoff) {
		idx++
	}
	if idx > 0 {
		copy(s.events, s.events[idx:])
		s.events = s.events[:len(s.events)-idx]
	}
}

var numericPathPart = regexp.MustCompile(`^[0-9]+$`)

func restEndpointTemplate(path string) string {
	if path == "" {
		return "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "/"
	}
	for i, part := range parts {
		if numericPathPart.MatchString(part) {
			parts[i] = "{number}"
			continue
		}
		if looksLikeSHA(part) {
			parts[i] = "{sha}"
		}
	}
	if len(parts) >= 3 && parts[0] == "repos" {
		parts[1] = "{owner}"
		parts[2] = "{repo}"
	}
	return "/" + strings.Join(parts, "/")
}

func looksLikeSHA(s string) bool {
	if len(s) < 12 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}
