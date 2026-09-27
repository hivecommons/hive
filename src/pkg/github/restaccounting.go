package github

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	restAccountingWindow        = time.Hour
	defaultRESTBudgetFloor      = 500
	lowValueRESTBudgetFloor     = defaultRESTBudgetFloor
	githubCoreRateLimitResource = "core"
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
	coreReset     time.Time
	coreObserved  time.Time
}

var sharedRESTAccounting = &restAccountingStore{now: time.Now, coreRemaining: -1}

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
	sharedRESTAccounting.record(restAccountingEvent{
		at:          sharedRESTAccounting.now(),
		caller:      caller,
		method:      method,
		endpoint:    endpoint,
		status:      status,
		charged:     charged,
		notModified: notModified,
		rateLimited: rateLimited,
	}, h)
}

func RESTTopConsumers(limit int) []RESTConsumer {
	return sharedRESTAccounting.top(limit)
}

func LowValueRESTWorkAllowed() bool {
	remaining, _, ok := sharedRESTAccounting.coreSnapshot()
	return !ok || remaining > lowValueRESTBudgetFloor
}

func (s *restAccountingStore) record(ev restAccountingEvent, h http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	s.pruneLocked(ev.at)
	if strings.EqualFold(h.Get("X-RateLimit-Resource"), githubCoreRateLimitResource) || h.Get("X-RateLimit-Resource") == "" {
		if rem, err := strconv.Atoi(h.Get("X-RateLimit-Remaining")); err == nil {
			s.coreRemaining = rem
			s.coreObserved = ev.at
		}
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			s.coreReset = time.Unix(reset, 0)
		}
	}
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
