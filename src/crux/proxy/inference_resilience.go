package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hivecommons/hive/crux/config"
)

// Opt-in timeouts and bounded retry for the inference translator
// (hivecommons/hive#19057). Everything here is inert while the resolved
// settings are the all-off default: no transport is cloned, no body is wrapped,
// no retry runs, and nothing extra is logged.
//
// Logs and metrics name the route (backend) and the status, never a request
// or response body and never a header value (a Retry-After is logged only as
// the delay the translator derived from it).

const (
	inferenceRetryCountMetric   = "crux.proxy.inference.retries"
	inferenceTimeoutCountMetric = "crux.proxy.inference.timeouts"

	// inferenceDialKeepAlive matches the keep-alive of http.DefaultTransport's
	// dialer, so enabling a connect timeout changes nothing else about dialing.
	inferenceDialKeepAlive = 30 * time.Second

	// inferenceRetryAfterBase is the base used to parse a Retry-After delay.
	inferenceRetryAfterBase = 10

	// inferenceStatusNotImplemented is the one 5xx never retried: the upstream
	// says it will not do this, so a retry cannot help.
	inferenceStatusNotImplemented = http.StatusNotImplemented
)

// Timeout kinds: a fixed enum so the metric attribute stays bounded.
const (
	inferenceTimeoutConnect        = "connect"
	inferenceTimeoutResponseHeader = "response_header"
	inferenceTimeoutIdle           = "idle"
)

var (
	attrRoute       = attribute.Key("route")
	attrStatus      = attribute.Key("status")
	attrTimeoutKind = attribute.Key("kind")
)

// inferenceResilienceSettings holds the resolved settings. nil = all off.
var inferenceResilienceSettings atomic.Pointer[config.InferenceResilience]

// SetInferenceResilience installs the resolved translator timeouts and retry
// policy. The zero value (every default) leaves the translator unchanged.
func SetInferenceResilience(r config.InferenceResilience) {
	inferenceResilienceSettings.Store(&r)
}

func currentInferenceResilience() config.InferenceResilience {
	if r := inferenceResilienceSettings.Load(); r != nil {
		return *r
	}
	return config.InferenceResilience{}
}

type inferenceResilienceMetrics struct {
	retries  metric.Int64Counter
	timeouts metric.Int64Counter
}

var globalInferenceResilienceMetrics = sync.OnceValue(func() inferenceResilienceMetrics {
	meter := otel.Meter(instrumentationName)
	// Constructors only fail on an invalid name or unit; both are constants.
	retries, _ := meter.Int64Counter(inferenceRetryCountMetric,
		metric.WithDescription("Inference upstream retries after a 429 or 5xx, by route and status."))
	timeouts, _ := meter.Int64Counter(inferenceTimeoutCountMetric,
		metric.WithDescription("Inference upstream timeouts, by route and kind (connect, response_header, idle)."))
	return inferenceResilienceMetrics{retries: retries, timeouts: timeouts}
})

// ---- transport (connect and response-header timeouts) ----

type tunedTransportKey struct {
	caBundle string
	connect  time.Duration
	header   time.Duration
}

var (
	tunedTransportMu    sync.Mutex
	tunedTransportCache = map[tunedTransportKey]*http.Transport{}
)

// tunedInferenceTransport returns a clone of base with the connect and
// response-header timeouts applied, cached per (CA bundle, settings). It
// returns nil when base is not an *http.Transport, in which case the caller
// keeps base untouched.
func tunedInferenceTransport(caBundle string, base http.RoundTripper, res config.InferenceResilience) *http.Transport {
	baseT, ok := base.(*http.Transport)
	if !ok {
		return nil
	}
	key := tunedTransportKey{caBundle: caBundle, connect: res.ConnectTimeout, header: res.ResponseHeaderTimeout}
	tunedTransportMu.Lock()
	defer tunedTransportMu.Unlock()
	if t, ok := tunedTransportCache[key]; ok {
		return t
	}
	t := baseT.Clone()
	if res.ConnectTimeout > 0 {
		dialer := &net.Dialer{Timeout: res.ConnectTimeout, KeepAlive: inferenceDialKeepAlive}
		t.DialContext = dialer.DialContext
	}
	if res.ResponseHeaderTimeout > 0 {
		t.ResponseHeaderTimeout = res.ResponseHeaderTimeout
	}
	tunedTransportCache[key] = t
	return t
}

// noteInferenceSendError counts and logs a connect or response-header timeout
// when the matching timeout is configured. It changes nothing about how the
// caller handles err.
func noteInferenceSendError(logger *slog.Logger, agentName string, route *InferenceRoute, err error) {
	res := currentInferenceResilience()
	if err == nil || route == nil || (res.ConnectTimeout <= 0 && res.ResponseHeaderTimeout <= 0) {
		return
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		return
	}
	kind := ""
	var opErr *net.OpError
	switch {
	case res.ResponseHeaderTimeout > 0 && strings.Contains(err.Error(), "awaiting response headers"):
		kind = inferenceTimeoutResponseHeader
	case res.ConnectTimeout > 0 && errors.As(err, &opErr) && opErr.Op == "dial":
		kind = inferenceTimeoutConnect
	}
	if kind == "" {
		return
	}
	recordInferenceTimeout(logger, agentName, route.Backend, kind, 0)
}

func recordInferenceTimeout(logger *slog.Logger, agentName, backend, kind string, status int) {
	globalInferenceResilienceMetrics().timeouts.Add(context.Background(), 1,
		metric.WithAttributes(attrRoute.String(backend), attrTimeoutKind.String(kind)))
	if logger == nil {
		return
	}
	logger.Warn("inference upstream timeout", "agent", agentName, "backend", backend, "kind", kind, "status", status)
}

// ---- idle-stream timeout ----

// inferenceIdleTimeoutError is the read error of a body cut for being idle.
type inferenceIdleTimeoutError struct{ idle time.Duration }

func (e inferenceIdleTimeoutError) Error() string {
	return "inference upstream sent no bytes for " + e.idle.String()
}
func (inferenceIdleTimeoutError) Timeout() bool   { return true }
func (inferenceIdleTimeoutError) Temporary() bool { return false }

// idleTimeoutBody cuts a response body that delivers no bytes for idle. The
// timer runs only while a Read is blocked on the upstream, so a slow consumer
// never trips it and a slow but active stream (bytes keep arriving) is never
// cut.
type idleTimeoutBody struct {
	rc      io.ReadCloser
	idle    time.Duration
	timer   *time.Timer
	tripped atomic.Bool
	onTrip  func()
}

func newIdleTimeoutBody(rc io.ReadCloser, idle time.Duration, onTrip func()) *idleTimeoutBody {
	b := &idleTimeoutBody{rc: rc, idle: idle, onTrip: onTrip}
	b.timer = time.AfterFunc(idle, b.trip)
	b.timer.Stop()
	return b
}

func (b *idleTimeoutBody) trip() {
	if !b.tripped.CompareAndSwap(false, true) {
		return
	}
	if b.onTrip != nil {
		b.onTrip()
	}
	_ = b.rc.Close()
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if err != nil && !errors.Is(err, io.EOF) && b.tripped.Load() {
		err = inferenceIdleTimeoutError{idle: b.idle}
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

// applyInferenceIdleTimeout wraps resp.Body with the idle-stream timeout when
// one is configured. With the default (off) it does nothing.
func applyInferenceIdleTimeout(logger *slog.Logger, resp *http.Response, agentName string, route *InferenceRoute) {
	idle := currentInferenceResilience().IdleStreamTimeout
	if idle <= 0 || resp == nil || resp.Body == nil || route == nil {
		return
	}
	status, backend := resp.StatusCode, route.Backend
	resp.Body = newIdleTimeoutBody(resp.Body, idle, func() {
		recordInferenceTimeout(logger, agentName, backend, inferenceTimeoutIdle, status)
	})
}

// ---- bounded retry for 429 and 5xx ----

// inferenceJitter returns a uniform duration in [0, n]. A variable so tests can
// pin it.
var inferenceJitter = func(n time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(n) + 1))
}

// inferenceSleep waits d or until ctx ends. A variable so tests need not wait.
var inferenceSleep = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// inferenceRetryableStatus is true for 429 and for 5xx other than 501.
func inferenceRetryableStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	return status >= http.StatusInternalServerError && status < 600 && status != inferenceStatusNotImplemented
}

// parseInferenceRetryAfter reads a Retry-After value (delta-seconds or an HTTP
// date). ok is false when absent or malformed.
func parseInferenceRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, inferenceRetryAfterBase, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > config.MaxInferenceTimeoutS {
			secs = config.MaxInferenceTimeoutS
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		if d > config.MaxInferenceTimeoutS*time.Second {
			d = config.MaxInferenceTimeoutS * time.Second
		}
		return d, true
	}
	return 0, false
}

// inferenceRetryDelay is how long to wait before retry number attempt (1-based).
// With a usable Retry-After the wait is that delay plus jitter in [0, base]; a
// Retry-After above the max delay returns ok=false (retrying sooner than the
// upstream asked would only fail again). Without one it is exponential
// backoff, base*2^(attempt-1) capped at the max delay, with jitter in
// [delay/2, delay].
func inferenceRetryDelay(res config.InferenceResilience, attempt int, retryAfter string, now time.Time) (time.Duration, bool) {
	if ra, ok := parseInferenceRetryAfter(retryAfter, now); ok {
		if ra > res.MaxDelay {
			return 0, false
		}
		return ra + inferenceJitter(res.BaseDelay), true
	}
	d := res.BaseDelay
	for i := 1; i < attempt && d < res.MaxDelay; i++ {
		d *= 2
	}
	if d > res.MaxDelay {
		d = res.MaxDelay
	}
	half := d / 2
	return half + inferenceJitter(d-half), true
}

// retryInferenceOnOverload resends the request after a 429 or 5xx while
// retries remain, honoring Retry-After. It runs before the response is handed
// to the handler, so no byte has reached the agent when it retries: a stream
// that dies after its first byte is never retried. The request body is the
// translated payload the handler already holds in memory, so replaying it
// needs no extra buffer and no size cap. It resends the same headers the first
// attempt carried (content type and the route's current auth) and leaves key
// resolution and the 401 retry to retryInferenceOnAuthFailure, which runs
// after it. With retry off (the default) it returns resp untouched.
func retryInferenceOnOverload(ctx context.Context, logger *slog.Logger, client *http.Client, route *InferenceRoute, agentName, upstreamURL string, body []byte, resp *http.Response) *http.Response {
	res := currentInferenceResilience()
	if !res.RetryEnabled || route == nil || resp == nil {
		return resp
	}
	for attempt := 1; attempt <= res.MaxRetries && inferenceRetryableStatus(resp.StatusCode); attempt++ {
		delay, ok := inferenceRetryDelay(res, attempt, resp.Header.Get("Retry-After"), time.Now())
		if !ok {
			if logger != nil {
				logger.Warn("inference upstream retry skipped: requested wait exceeds the configured maximum",
					"agent", agentName, "backend", route.Backend, "status", resp.StatusCode,
					"max_delay_ms", res.MaxDelay.Milliseconds())
			}
			return resp
		}
		if logger != nil {
			logger.Warn("inference upstream retrying",
				"agent", agentName, "backend", route.Backend, "status", resp.StatusCode,
				"attempt", attempt, "max_retries", res.MaxRetries, "delay_ms", delay.Milliseconds())
		}
		globalInferenceResilienceMetrics().retries.Add(ctx, 1,
			metric.WithAttributes(attrRoute.String(route.Backend), attrStatus.Int(resp.StatusCode)))
		if err := inferenceSleep(ctx, delay); err != nil {
			return resp
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			return resp
		}
		req.Header.Set("Content-Type", "application/json")
		applyInferenceAuth(req, route)
		next, err := client.Do(req)
		if err != nil {
			if logger != nil {
				logger.Warn("inference upstream retry failed to send; keeping the previous response",
					"agent", agentName, "backend", route.Backend, "attempt", attempt, "error", err)
			}
			noteInferenceSendError(logger, agentName, route, err)
			return resp
		}
		_, _ = io.CopyN(io.Discard, resp.Body, inferenceRetryDrainLimit)
		_ = resp.Body.Close()
		resp = next
	}
	return resp
}
