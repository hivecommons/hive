package github

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// OpenTelemetry instrumentation for GitHub API spend (hivecommons/hive#13380).
//
// otelTransport is the outermost layer of githubTransportChain, so one span
// covers a call's pacing wait, conditional-request handling and the socket
// round trip. The span and the github.* metrics record through the global
// providers pkg/tracing installs; with tracing off (the default) both are
// no-ops and this layer adds only header reads.

const (
	// githubInstrumentationScope names the tracer and meter for this package.
	githubInstrumentationScope = "github.com/hivecommons/hive/pkg/github"

	// SpanGitHubRequest is the span recorded per GitHub REST/GraphQL call.
	SpanGitHubRequest = "github.request"

	// Span attributes. github.endpoint is the templated path (restEndpointTemplate:
	// numbers → {number}, SHAs → {sha}, owner/repo → {owner}/{repo}), so the
	// cardinality is bounded and no identifiers leak.
	AttrGitHubEndpoint           = "github.endpoint"
	AttrGitHubRepo               = "github.repo"
	AttrGitHubCacheHit           = "github.cache_hit"
	AttrGitHubRateLimitRemaining = "github.ratelimit.remaining"
	AttrGitHubRateLimitBucket    = "github.ratelimit.bucket"
	AttrGitHubShed               = "github.shed"
	AttrGitHubStatus             = "http.response.status_code"
	AttrGitHubMethod             = "http.request.method"
	AttrHiveCaller               = "hive.caller"
	// AttrHivePacingClass is the pacer priority class the request carried
	// (pacing_priority.go): high / normal / low.
	AttrHivePacingClass = "hive.pacing_class"

	// Metric names.
	MetricGitHubRequests           = "github.requests"             // counter {caller,endpoint,status}
	MetricGitHubRateLimitRemaining = "github.ratelimit.remaining"  // gauge {bucket}
	MetricGitHubSecondaryLimitHits = "github.secondary_limit_hits" // counter

	// unknownCaller is hive.caller when the request context carries no
	// WithRESTCaller tag (restCallerFromContext's own default is the process
	// name, which says nothing about which component spent the call).
	unknownCaller = "unknown"
	// defaultRateLimitBucket is assumed when GitHub returns rate-limit headers
	// without X-RateLimit-Resource (older proxies, 304 replays).
	defaultRateLimitBucket = githubCoreRateLimitResource
	// pacingSecondaryKind is the pacingLimitHeader value for a secondary hit.
	pacingSecondaryKind = "secondary"
	// rateLimitBudgetWindow bounds how long a bucket observation is trusted.
	rateLimitBudgetWindow = time.Hour
)

type githubInstruments struct {
	requests  metric.Int64Counter
	remaining metric.Int64Gauge
	secondary metric.Int64Counter
}

var (
	githubInstrumentsOnce sync.Once
	githubInstrumentsVal  githubInstruments
)

// instruments creates the github.* instruments on first use against the
// global MeterProvider. OTel's global provider delegates, so instruments
// created before pkg/tracing.Init runs start recording once it has.
func instruments() githubInstruments {
	githubInstrumentsOnce.Do(func() {
		m := tracing.Meter(githubInstrumentationScope)
		githubInstrumentsVal.requests, _ = m.Int64Counter(MetricGitHubRequests,
			metric.WithDescription("GitHub API calls by caller, templated endpoint and status."))
		githubInstrumentsVal.remaining, _ = m.Int64Gauge(MetricGitHubRateLimitRemaining,
			metric.WithDescription("Last observed X-RateLimit-Remaining per bucket."))
		githubInstrumentsVal.secondary, _ = m.Int64Counter(MetricGitHubSecondaryLimitHits,
			metric.WithDescription("Responses the pacer recognised as secondary rate-limit refusals."))
	})
	return githubInstrumentsVal
}

// rateLimitBudgetStore remembers the latest (remaining, limit) per bucket so
// the adaptive reconcile cadence (pkg/forgestate) can stretch its interval
// as the budget drains.
type rateLimitBudgetStore struct {
	mu      sync.Mutex
	buckets map[string]rateLimitObservation
	now     func() time.Time
}

type rateLimitObservation struct {
	remaining int
	limit     int
	observed  time.Time
}

var sharedRateLimitBudget = &rateLimitBudgetStore{buckets: map[string]rateLimitObservation{}, now: time.Now}

func (s *rateLimitBudgetStore) observe(bucket string, remaining, limit int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[bucket] = rateLimitObservation{remaining: remaining, limit: limit, observed: s.now()}
}

func (s *rateLimitBudgetStore) get(bucket string) (remaining, limit int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, found := s.buckets[bucket]
	if !found || s.now().Sub(o.observed) > rateLimitBudgetWindow {
		return 0, 0, false
	}
	return o.remaining, o.limit, true
}

// RateLimitBudget reports the most recent X-RateLimit-Remaining and
// X-RateLimit-Limit seen for bucket ("core", "search", "graphql", ...) on
// the hive's own credential. ok is false until a response for that bucket
// has been observed in the last hour. It is the budget source behind the
// adaptive reconcile interval (forgestate.AdaptiveInterval).
func RateLimitBudget(bucket string) (remaining, limit int, ok bool) {
	return sharedRateLimitBudget.get(strings.ToLower(strings.TrimSpace(bucket)))
}

// ResetRateLimitBudgetForTest clears the per-bucket observations.
func ResetRateLimitBudgetForTest() {
	sharedRateLimitBudget.mu.Lock()
	sharedRateLimitBudget.buckets = map[string]rateLimitObservation{}
	sharedRateLimitBudget.mu.Unlock()
}

type otelTransport struct{ inner http.RoundTripper }

func otelWrap(inner http.RoundTripper) http.RoundTripper { return &otelTransport{inner: inner} }

func (t *otelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := integrationReadOnlyRequest(req); err != nil {
		return nil, err
	}
	endpoint := restEndpointTemplate(req.URL.Path)
	caller := unknownCaller
	if v, ok := req.Context().Value(restCallerContextKey{}).(string); ok && strings.TrimSpace(v) != "" {
		caller = strings.TrimSpace(v)
	}
	ctx, span := tracing.StartSpan(req.Context(), SpanGitHubRequest,
		attribute.String(AttrGitHubMethod, req.Method),
		attribute.String(AttrGitHubEndpoint, endpoint),
		attribute.String(AttrHiveCaller, caller),
		attribute.String(AttrHivePacingClass, PacingClassOf(req.Context()).String()),
	)
	defer span.End()
	if repo := repoFromRequest(req.URL); repo != "" {
		span.SetAttributes(attribute.String(AttrGitHubRepo, repo))
	}

	resp, err := t.inner.RoundTrip(req.WithContext(ctx))

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	// ErrPacingShed is the class-based refusal; the prefix also catches the
	// pacer's deadline-miss refusal, which is a plain fmt error.
	shed := err != nil && (errors.Is(err, ErrPacingShed) || strings.HasPrefix(err.Error(), pacingErrPrefix))
	span.SetAttributes(
		attribute.Int(AttrGitHubStatus, status),
		attribute.Bool(AttrGitHubShed, shed),
	)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	ins := instruments()
	statusLabel := strconv.Itoa(status)
	if shed {
		statusLabel = "shed"
	} else if err != nil {
		statusLabel = "error"
	}
	ins.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("caller", caller),
		attribute.String("endpoint", endpoint),
		attribute.String("status", statusLabel),
	))
	if resp == nil {
		return resp, err
	}

	cacheHit := status == http.StatusNotModified || resp.Header.Get(etagCacheHeader) == "revalidated"
	span.SetAttributes(attribute.Bool(AttrGitHubCacheHit, cacheHit))
	if bucket, remaining, limit, ok := rateLimitHeaders(resp.Header); ok {
		span.SetAttributes(
			attribute.Int(AttrGitHubRateLimitRemaining, remaining),
			attribute.String(AttrGitHubRateLimitBucket, bucket),
		)
		// Mirror restaccounting's guard: an unauthenticated per-IP bucket
		// (limit ≤ 60) is not the hive's quota and must not drive its cadence.
		if limit > unauthenticatedCoreRateLimit || limit <= 0 {
			ins.remaining.Record(ctx, int64(remaining), metric.WithAttributes(attribute.String("bucket", bucket)))
			sharedRateLimitBudget.observe(bucket, remaining, limit)
		}
	}
	if resp.Header.Get(pacingLimitHeader) == pacingSecondaryKind {
		ins.secondary.Add(ctx, 1)
		span.SetStatus(codes.Error, "secondary rate limit")
	}
	return resp, err
}

// rateLimitHeaders reads GitHub's quota headers; ok is false when the
// response carries none.
func rateLimitHeaders(h http.Header) (bucket string, remaining, limit int, ok bool) {
	rem, err := strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Remaining")))
	if err != nil {
		return "", 0, 0, false
	}
	limit, _ = strconv.Atoi(strings.TrimSpace(h.Get("X-RateLimit-Limit")))
	bucket = strings.ToLower(strings.TrimSpace(h.Get("X-RateLimit-Resource")))
	if bucket == "" {
		bucket = defaultRateLimitBucket
	}
	return bucket, rem, limit, true
}

// repoFromRequest derives owner/name from /repos/{owner}/{repo}/... paths or
// from the first repo: qualifier of a search query; "" when neither applies.
func repoFromRequest(u *url.URL) string {
	if u == nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" {
		return parts[1] + "/" + parts[2]
	}
	if strings.HasPrefix(u.Path, "/search/") {
		for _, tok := range strings.Fields(u.Query().Get("q")) {
			if r, ok := strings.CutPrefix(tok, "repo:"); ok {
				return r
			}
		}
	}
	return ""
}
