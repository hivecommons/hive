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
	// core is true when the response was served from the core bucket (or
	// named no bucket), so API budget pacing can leave search and GraphQL
	// calls out of the core burn rate.
	core bool
}

type restAccountingStore struct {
	mu     sync.Mutex
	events []restAccountingEvent
	now    func() time.Time
	// coreWindows holds one entry per core rate-limit window (keyed by its
	// reset) that the hive's own responses have reported recently. See
	// coreEvidenceLocked for why it is not a single last-response-wins value.
	coreWindows []coreWindow
	coreLimit   int
}

// coreWindow is what the hive's own responses say about one core rate-limit
// window, identified by the X-RateLimit-Reset it carries.
type coreWindow struct {
	limit int
	// remaining is the lowest remaining seen for this window: within one
	// window the real value only falls, so a higher one is an older response
	// that arrived late.
	remaining int
	// reset is zero for readings that carried no reset header.
	reset time.Time
	// observed is when a response last reported this window.
	observed time.Time
}

// coreWindowPeerSpan is how recently a window must have been reported, relative
// to the newest report, to compete for "the hive's bucket". A window nobody
// has reported for this long is a burst that ended, not the bucket being spent.
const coreWindowPeerSpan = 5 * time.Minute

// maxCoreWindows bounds coreWindows. Two windows (the real one and a stray) is
// what has been seen live; the bound only stops a pathological header stream
// from growing the slice.
const maxCoreWindows = 8

var sharedRESTAccountingPtr atomic.Pointer[restAccountingStore]

func init() {
	sharedRESTAccountingPtr.Store(&restAccountingStore{now: time.Now})
}

// sharedRESTAccounting returns the process-wide store. The pointer is atomic so
// tests can swap it while transports in other goroutines are still recording.
func sharedRESTAccounting() *restAccountingStore { return sharedRESTAccountingPtr.Load() }

type restAccountingTransport struct {
	inner http.RoundTripper
	// appJWT marks the App-JWT chain (installation-token mint, installation
	// discovery). Those responses describe the App's own JWT bucket, not an
	// installation's core bucket, so they are counted per caller but never
	// move the shared core snapshot.
	appJWT bool
}

func restAccountingWrap(inner http.RoundTripper) http.RoundTripper {
	return &restAccountingTransport{inner: inner}
}

// restAccountingWrapAppJWT is restAccountingWrap for the App-JWT chain.
func restAccountingWrapAppJWT(inner http.RoundTripper) http.RoundTripper {
	return &restAccountingTransport{inner: inner, appJWT: true}
}

func (t *restAccountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if resp != nil {
		caller := restCallerFromContext(req.Context())
		recordRESTRequest(caller, req.Method, req.URL.Path, resp.StatusCode, resp.Header,
			!t.appJWT && !strings.HasPrefix(caller, agentRESTCallerPrefix))
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

// RecordLocalServe records a dashboard request that was answered from memory
// without any GitHub call, so /api/gh-rate-limits lists the caller with its
// request count and Charged 0. status 304 counts as not modified.
func RecordLocalServe(caller, path string, status int) {
	if strings.TrimSpace(caller) == "" {
		caller = "unknown"
	}
	store := sharedRESTAccounting()
	store.record(restAccountingEvent{
		at:          store.now(),
		caller:      caller,
		method:      http.MethodGet,
		endpoint:    restEndpointTemplate(path),
		status:      status,
		notModified: status == http.StatusNotModified,
	}, nil, false)
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

// coreRateLimitEvidence is CoreRateLimitSnapshot with the window's limit and
// the time a response last reported it, for the budget reader and the card.
func coreRateLimitEvidence() (RateLimitEntry, bool) {
	return sharedRESTAccounting().coreEvidence()
}

func (s *restAccountingStore) record(ev restAccountingEvent, h http.Header, daemonCredential bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.core = h.Get("X-RateLimit-Resource") == "" || strings.EqualFold(h.Get("X-RateLimit-Resource"), githubCoreRateLimitResource)
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
		rem, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
		if err != nil {
			return
		}
		var reset time.Time
		if r, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && r > 0 {
			reset = time.Unix(r, 0)
		}
		limit, _ := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Limit")))
		s.observeCoreLocked(rem, limit, reset, ev.at)
	}
}

// observeCoreLocked folds one core reading into its window. A reading without
// a reset header belongs to the window most recently reported.
func (s *restAccountingStore) observeCoreLocked(remaining, limit int, reset, at time.Time) {
	s.pruneCoreLocked(at)
	idx := -1
	if reset.IsZero() {
		for i := range s.coreWindows {
			if idx < 0 || s.coreWindows[i].observed.After(s.coreWindows[idx].observed) {
				idx = i
			}
		}
	} else {
		for i := range s.coreWindows {
			if s.coreWindows[i].reset.Equal(reset) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		if len(s.coreWindows) >= maxCoreWindows {
			s.coreWindows = s.coreWindows[1:]
		}
		s.coreWindows = append(s.coreWindows, coreWindow{limit: limit, remaining: remaining, reset: reset, observed: at})
		return
	}
	w := &s.coreWindows[idx]
	if remaining < w.remaining {
		w.remaining = remaining
	}
	if limit > 0 {
		w.limit = limit
	}
	if w.reset.IsZero() {
		w.reset = reset
	}
	if at.After(w.observed) {
		w.observed = at
	}
}

// pruneCoreLocked drops windows whose reset has passed or that no response has
// reported for restAccountingWindow.
func (s *restAccountingStore) pruneCoreLocked(now time.Time) {
	kept := s.coreWindows[:0]
	for _, w := range s.coreWindows {
		if !w.reset.IsZero() && !now.Before(w.reset) {
			continue
		}
		if now.Sub(w.observed) > restAccountingWindow {
			continue
		}
		kept = append(kept, w)
	}
	s.coreWindows = kept
}

// coreEvidenceLocked picks the window the hive's own traffic is spending.
//
// It used to be "the last response wins". But two core windows are reported to
// the daemon at once on a live hive: on 2026-11-08 between 11:31 and 11:35Z
// readings of 783, 732, 716, 549 and 380 (reset 11:56:18Z, the installation
// bucket that was returning 403s) alternated every few seconds with 6806, 6790
// and 6779 (reset 12:15:11Z), and the budget mode flipped between normal and
// conserve with them. Keying by reset keeps the two apart, and among the
// windows reported within coreWindowPeerSpan of the newest report the LOWEST
// remaining is taken: the bucket being spent is the one running out, and a
// wrong pick can only make the hive more careful, never send it into 403s.
func (s *restAccountingStore) coreEvidenceLocked(now time.Time) (coreWindow, bool) {
	s.pruneCoreLocked(now)
	var newest time.Time
	for _, w := range s.coreWindows {
		if w.observed.After(newest) {
			newest = w.observed
		}
	}
	best := -1
	for i, w := range s.coreWindows {
		if newest.Sub(w.observed) > coreWindowPeerSpan {
			continue
		}
		if best < 0 || w.remaining < s.coreWindows[best].remaining ||
			(w.remaining == s.coreWindows[best].remaining && w.observed.After(s.coreWindows[best].observed)) {
			best = i
		}
	}
	if best < 0 {
		return coreWindow{}, false
	}
	return s.coreWindows[best], true
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
	out := s.consumersLocked()
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// consumersLocked returns every caller + method + endpoint row of the events
// held, most charged first. The caller prunes and holds s.mu.
func (s *restAccountingStore) consumersLocked() []RESTConsumer {
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
		if out[i].Endpoint != out[j].Endpoint {
			return out[i].Endpoint < out[j].Endpoint
		}
		// Caller and method settle rows that tie on everything above, so a
		// list cut at a limit holds the same rows on every read.
		if out[i].Caller != out[j].Caller {
			return out[i].Caller < out[j].Caller
		}
		return out[i].Method < out[j].Method
	})
	return out
}

func (s *restAccountingStore) coreSnapshot() (remaining int, reset time.Time, ok bool) {
	e, ok := s.coreEvidence()
	return e.Remaining, e.Reset, ok
}

func (s *restAccountingStore) coreEvidence() (RateLimitEntry, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.coreEvidenceLocked(now)
	if !ok {
		return RateLimitEntry{}, false
	}
	return RateLimitEntry{Limit: w.limit, Remaining: w.remaining, Reset: w.reset, ObservedAt: w.observed}, true
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
