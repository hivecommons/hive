package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

const (
	resilienceTestBase     = 10 * time.Millisecond
	resilienceTestMaxDelay = 5 * time.Second
	// resilienceStallCeiling is how long a deliberately stalled upstream waits
	// if nothing cuts it; every cut test must finish well inside it.
	resilienceStallCeiling = 5 * time.Second
	resilienceCutBudget    = 3 * time.Second
	resilienceSSETail      = "TAILMARKER"
)

// setInferenceResilienceForTest installs res and restores every global the
// retry code exposes for tests.
func setInferenceResilienceForTest(t *testing.T, res config.InferenceResilience) *sleepRecorder {
	t.Helper()
	prevSleep, prevJitter := inferenceSleep, inferenceJitter
	rec := &sleepRecorder{}
	inferenceSleep = rec.sleep
	SetInferenceResilience(res)
	t.Cleanup(func() {
		inferenceResilienceSettings.Store(nil)
		inferenceSleep, inferenceJitter = prevSleep, prevJitter
	})
	return rec
}

type sleepRecorder struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

func retryResilience(maxRetries int) config.InferenceResilience {
	return config.InferenceResilience{
		RetryEnabled: true,
		MaxRetries:   maxRetries,
		BaseDelay:    resilienceTestBase,
		MaxDelay:     resilienceTestMaxDelay,
	}
}

// serveTranslatorFull is serveTranslator that also returns the response body.
func serveTranslatorFull(t *testing.T, p *GitHubProxy, route *InferenceRoute) (int, string) {
	t.Helper()
	p.inference.Set("scanner", route)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(keyResolverTestBody))
	req.Header.Set("x-api-key", "sk-hive-scanner")
	w := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// scriptedUpstream answers request n (1-based) with script(n, w, r).
type scriptedUpstream struct {
	srv  *httptest.Server
	hits atomic.Int32
}

func newScriptedUpstream(t *testing.T, script func(n int, w http.ResponseWriter, r *http.Request)) *scriptedUpstream {
	t.Helper()
	u := &scriptedUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		script(int(u.hits.Add(1)), w, r)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, keyResolverOKResponse)
}

func sseChunk(text string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", text)
}

func resilienceRoute(u *scriptedUpstream) *InferenceRoute {
	return &InferenceRoute{Backend: "litellm", Endpoint: u.srv.URL, Model: "m", APIKey: "k"}
}

func waitOrDone(r *http.Request, d time.Duration) {
	select {
	case <-r.Context().Done():
	case <-time.After(d):
	}
}

// Defaults: nothing changes.

func TestInferenceResilience_DefaultsUnchanged(t *testing.T) {
	var zero config.InferenceResilience
	if zero.Enabled() {
		t.Fatal("the zero value must be all off")
	}
	res, ignored := (*config.InferenceResilienceConfig)(nil).Resolve(func(string) string { return "" })
	if res.Enabled() || len(ignored) != 0 {
		t.Fatalf("absent block must resolve to all off, got %+v ignored %v", res, ignored)
	}

	// The client uses the very same default transport as before (only the
	// redirect policy is the egress one, hivecommons/hive#19222).
	c, err := inferenceHTTPClient(&InferenceRoute{})
	if err != nil || c.Transport != http.DefaultTransport || c.Timeout != 0 {
		t.Fatalf("default client changed: %v %v", c, err)
	}

	// A 429 is passed straight through with one upstream call, and a slow
	// upstream is not cut, with no extra log line.
	slow := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		waitOrDone(r, 300*time.Millisecond)
		writeOK(w)
	})
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	if code, _ := serveTranslatorFull(t, p, resilienceRoute(slow)); code != http.StatusOK {
		t.Fatalf("slow upstream status %d, want 200", code)
	}
	limited := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if code, _ := serveTranslatorFull(t, p, resilienceRoute(limited)); code != http.StatusTooManyRequests || limited.hits.Load() != 1 {
		t.Fatalf("429 status %d hits %d, want passthrough with one call", code, limited.hits.Load())
	}
	for _, banned := range []string{"inference upstream retrying", "inference upstream timeout"} {
		if strings.Contains(logs.String(), banned) {
			t.Fatalf("defaults logged %q:\n%s", banned, logs.String())
		}
	}
}

// Timeouts.

func TestInferenceResilience_HeaderTimeoutCutsStalledUpstream(t *testing.T) {
	for _, entry := range []string{"translator", "mitm"} {
		t.Run(entry, func(t *testing.T) {
			setInferenceResilienceForTest(t, config.InferenceResilience{ResponseHeaderTimeout: 150 * time.Millisecond})
			up := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
				waitOrDone(r, resilienceStallCeiling)
			})
			var logs bytes.Buffer
			p := newKeyResolverTestProxy(&logs)
			serve := serveTranslator
			if entry == "mitm" {
				serve = serveMITM
			}
			start := time.Now()
			code := serve(t, p, resilienceRoute(up))
			if code != http.StatusBadGateway {
				t.Fatalf("status %d, want 502", code)
			}
			if time.Since(start) > resilienceCutBudget {
				t.Fatalf("took %s, the stalled upstream was not cut at the header timeout", time.Since(start))
			}
			out := logs.String()
			if !strings.Contains(out, "inference upstream timeout") || !strings.Contains(out, "kind=response_header") || !strings.Contains(out, "backend=litellm") {
				t.Fatalf("missing timeout log naming the route and kind:\n%s", out)
			}
		})
	}
}

func TestInferenceResilience_TunedTransport(t *testing.T) {
	res := config.InferenceResilience{ConnectTimeout: 3 * time.Second, ResponseHeaderTimeout: 7 * time.Second}
	tr := tunedInferenceTransport("", http.DefaultTransport, res)
	if tr == nil || tr.ResponseHeaderTimeout != res.ResponseHeaderTimeout || tr.DialContext == nil {
		t.Fatalf("tuned transport not applied: %+v", tr)
	}
	if again := tunedInferenceTransport("", http.DefaultTransport, res); again != tr {
		t.Fatal("tuned transport must be cached per settings")
	}
	if base, _ := http.DefaultTransport.(*http.Transport); base.ResponseHeaderTimeout != 0 {
		t.Fatal("the shared default transport was mutated")
	}
	if tunedInferenceTransport("", failingRoundTripper{}, res) != nil {
		t.Fatal("a non-http.Transport base must be left alone")
	}
}

func sseUpstream(chunks int, gap time.Duration, stallAfter int) func(int, http.ResponseWriter, *http.Request) {
	return func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 1; i <= chunks; i++ {
			text := "x"
			if i == chunks {
				text = resilienceSSETail
			}
			_, _ = io.WriteString(w, sseChunk(text))
			fl.Flush()
			if stallAfter > 0 && i == stallAfter {
				waitOrDone(r, resilienceStallCeiling)
				return
			}
			waitOrDone(r, gap)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}
}

func TestInferenceResilience_IdleTimeoutCutsStalledStream(t *testing.T) {
	setInferenceResilienceForTest(t, config.InferenceResilience{IdleStreamTimeout: 200 * time.Millisecond})
	up := newScriptedUpstream(t, sseUpstream(5, 0, 1)) // one chunk, then silence
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	start := time.Now()
	code, body := serveTranslatorFull(t, p, resilienceRoute(up))
	if time.Since(start) > resilienceCutBudget {
		t.Fatalf("took %s, the idle stream was not cut", time.Since(start))
	}
	if code != http.StatusOK || strings.Contains(body, resilienceSSETail) {
		t.Fatalf("status %d; the stalled stream must end without its tail:\n%s", code, body)
	}
	out := logs.String()
	if !strings.Contains(out, "kind=idle") || !strings.Contains(out, "backend=litellm") || !strings.Contains(out, "inference SSE translation failed") {
		t.Fatalf("missing idle timeout log or the existing stream-death log:\n%s", out)
	}
	if strings.Contains(out, "x-api-key") || strings.Contains(out, "Bearer") {
		t.Fatalf("log leaked a header:\n%s", out)
	}
}

func TestInferenceResilience_IdleTimeoutSparesSlowActiveStream(t *testing.T) {
	// 8 chunks, 80ms apart: the stream lasts ~640ms, well past the 300ms idle
	// timeout, but never goes 300ms without a byte.
	setInferenceResilienceForTest(t, config.InferenceResilience{IdleStreamTimeout: 300 * time.Millisecond})
	up := newScriptedUpstream(t, sseUpstream(8, 80*time.Millisecond, 0))
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	code, body := serveTranslatorFull(t, p, resilienceRoute(up))
	if code != http.StatusOK || !strings.Contains(body, resilienceSSETail) {
		t.Fatalf("status %d; the slow but active stream must complete:\n%s", code, body)
	}
	if strings.Contains(logs.String(), "inference upstream timeout") {
		t.Fatalf("an active stream was reported as timed out:\n%s", logs.String())
	}
}

// Retry.

func TestInferenceResilience_RetriesWithinCap(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		failures   int
		maxRetries int
		wantHits   int32
		wantCode   int
	}{
		{"429 with Retry-After recovers", http.StatusTooManyRequests, "1", 1, 2, 2, http.StatusOK},
		{"500 recovers", http.StatusInternalServerError, "", 1, 2, 2, http.StatusOK},
		{"502 recovers", http.StatusBadGateway, "", 2, 2, 3, http.StatusOK},
		{"503 recovers", http.StatusServiceUnavailable, "", 2, 2, 3, http.StatusOK},
		{"504 recovers", http.StatusGatewayTimeout, "", 1, 1, 2, http.StatusOK},
		{"cap respected on 503", http.StatusServiceUnavailable, "", 100, 2, 3, http.StatusServiceUnavailable},
		{"cap respected on 429", http.StatusTooManyRequests, "", 100, 1, 2, http.StatusTooManyRequests},
		{"501 is not retried", http.StatusNotImplemented, "", 100, 3, 1, http.StatusNotImplemented},
		{"400 is not retried", http.StatusBadRequest, "", 100, 3, 1, http.StatusBadRequest},
		{"404 is not retried", http.StatusNotFound, "", 100, 3, 1, http.StatusNotFound},
		{"403 is not retried", http.StatusForbidden, "", 100, 3, 1, http.StatusForbidden},
	}
	for _, tc := range cases {
		for _, entry := range []string{"translator", "mitm"} {
			t.Run(tc.name+"/"+entry, func(t *testing.T) {
				rec := setInferenceResilienceForTest(t, retryResilience(tc.maxRetries))
				inferenceJitter = func(time.Duration) time.Duration { return 0 }
				up := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
					if n <= tc.failures {
						if tc.retryAfter != "" {
							w.Header().Set("Retry-After", tc.retryAfter)
						}
						w.WriteHeader(tc.status)
						_, _ = io.WriteString(w, `{"error":{"message":"busy"}}`)
						return
					}
					writeOK(w)
				})
				var logs bytes.Buffer
				p := newKeyResolverTestProxy(&logs)
				serve := serveTranslator
				if entry == "mitm" {
					serve = serveMITM
				}
				code := serve(t, p, resilienceRoute(up))
				if code != tc.wantCode || up.hits.Load() != tc.wantHits {
					t.Fatalf("status %d hits %d, want status %d hits %d\n%s", code, up.hits.Load(), tc.wantCode, tc.wantHits, logs.String())
				}
				if want := int(tc.wantHits) - 1; len(rec.durations()) != want {
					t.Fatalf("slept %d times, want %d", len(rec.durations()), want)
				}
				if tc.retryAfter == "1" {
					if d := rec.durations(); len(d) != 1 || d[0] != time.Second {
						t.Fatalf("Retry-After: 1 slept %v, want exactly 1s with zero jitter", d)
					}
				}
				if int(tc.wantHits) > 1 {
					out := logs.String()
					if !strings.Contains(out, "inference upstream retrying") || !strings.Contains(out, fmt.Sprintf("status=%d", tc.status)) || !strings.Contains(out, "backend=litellm") {
						t.Fatalf("missing retry log naming route and status:\n%s", out)
					}
					if strings.Contains(out, "Retry-After") || strings.Contains(out, "Bearer") {
						t.Fatalf("retry log carried a header:\n%s", out)
					}
				}
			})
		}
	}
}

func TestInferenceResilience_RetryReplaysSameRequest(t *testing.T) {
	setInferenceResilienceForTest(t, retryResilience(2))
	var mu sync.Mutex
	var bodies, auths []string
	up := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("Content-Type"))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeOK(w)
	})
	p := newKeyResolverTestProxy(&bytes.Buffer{})
	if code, _ := serveTranslatorFull(t, p, resilienceRoute(up)); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] || auths[0] != auths[1] {
		t.Fatalf("the retry must replay the same body and headers: %q %q", bodies, auths)
	}
}

func TestInferenceResilience_RetryAfterAboveMaxIsNotRetried(t *testing.T) {
	res := retryResilience(3)
	res.MaxDelay = 2 * time.Second
	rec := setInferenceResilienceForTest(t, res)
	up := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	var logs bytes.Buffer
	p := newKeyResolverTestProxy(&logs)
	if code, _ := serveTranslatorFull(t, p, resilienceRoute(up)); code != http.StatusTooManyRequests || up.hits.Load() != 1 || len(rec.durations()) != 0 {
		t.Fatalf("status %d hits %d: a Retry-After above the cap must not be retried", code, up.hits.Load())
	}
}

func TestInferenceResilience_NoRetryAfterBytesReachedAgent(t *testing.T) {
	for _, entry := range []string{"translator", "mitm"} {
		t.Run(entry, func(t *testing.T) {
			rec := setInferenceResilienceForTest(t, retryResilience(3))
			// 200 with one chunk, then the connection dies mid-stream.
			up := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sseChunk("partial"))
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			})
			var logs bytes.Buffer
			p := newKeyResolverTestProxy(&logs)
			serve := serveTranslator
			if entry == "mitm" {
				serve = serveMITM
			}
			if code := serve(t, p, resilienceRoute(up)); code != http.StatusOK {
				t.Fatalf("status %d, the stream had already started", code)
			}
			if up.hits.Load() != 1 || len(rec.durations()) != 0 {
				t.Fatalf("hits %d sleeps %d: nothing may be retried once bytes were sent", up.hits.Load(), len(rec.durations()))
			}
		})
	}
}

func TestInferenceResilience_RetryStopsWhenContextEnds(t *testing.T) {
	setInferenceResilienceForTest(t, retryResilience(3))
	ctx, cancel := context.WithCancel(context.Background())
	inferenceSleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	up := newScriptedUpstream(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	route := resilienceRoute(up)
	client, err := inferenceHTTPClient(route)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, up.srv.URL, strings.NewReader("{}"))
	first, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got := retryInferenceOnOverload(ctx, nil, client, route, "scanner", up.srv.URL, []byte("{}"), first)
	_ = got.Body.Close()
	if got != first || up.hits.Load() != 1 {
		t.Fatalf("a canceled context must return the original response without resending (hits %d)", up.hits.Load())
	}
}

// Jitter and Retry-After parsing.

func TestInferenceRetryDelay_JitterBounds(t *testing.T) {
	res := config.InferenceResilience{RetryEnabled: true, MaxRetries: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	now := time.Now()
	wantCeil := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second}
	for i, ceil := range wantCeil {
		for k := 0; k < 300; k++ {
			d, ok := inferenceRetryDelay(res, i+1, "", now)
			if !ok || d < ceil/2 || d > ceil {
				t.Fatalf("attempt %d: delay %s outside [%s, %s]", i+1, d, ceil/2, ceil)
			}
		}
	}
	for k := 0; k < 300; k++ {
		d, ok := inferenceRetryDelay(res, 1, "1", now)
		if !ok || d < time.Second || d > time.Second+res.BaseDelay {
			t.Fatalf("Retry-After delay %s outside [1s, 1s+base]", d)
		}
	}
	if _, ok := inferenceRetryDelay(res, 1, "2", now); ok {
		t.Fatal("a Retry-After above the max delay must not be retried")
	}
}

func TestParseInferenceRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"", 0, false},
		{"abc", 0, false},
		{"-5", 0, false},
		{"0", 0, true},
		{"7", 7 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		{now.Add(9 * time.Second).UTC().Format(http.TimeFormat), 9 * time.Second, true},
		{now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0, true},
		{"99999999999", config.MaxInferenceTimeoutS * time.Second, true},
	}
	for _, tc := range cases {
		got, ok := parseInferenceRetryAfter(tc.in, now)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("parse(%q) = %s,%t want %s,%t", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// 401 path untouched.

func TestInferenceResilience_401PathUntouched(t *testing.T) {
	setInferenceResilienceForTest(t, retryResilience(3))

	// No resolver: a 401 is not retried.
	none := newKeyedUpstream(t)
	p := newKeyResolverTestProxy(&bytes.Buffer{})
	route := &InferenceRoute{Backend: "litellm", Endpoint: none.srv.URL, Model: "m", APIKey: "k"}
	if code := serveTranslator(t, p, route); code != http.StatusUnauthorized || none.hits.Load() != 1 {
		t.Fatalf("status %d hits %d, a 401 with no resolver must not be retried", code, none.hits.Load())
	}

	// Rotated key: still exactly one retry with the new key.
	up := newKeyedUpstream(t, "new-key")
	var key atomic.Value
	key.Store("old-key")
	r := NewInferenceKeyResolver(t.Name(), func() (string, string) {
		k, _ := key.Load().(string)
		return k, "file:/secrets/litellm_api_key"
	})
	r.Current()
	key.Store("new-key")
	rotated := &InferenceRoute{Backend: "litellm", Endpoint: up.srv.URL, Model: "m", APIKey: "old-key", KeyResolver: r}
	if code := serveTranslator(t, p, rotated); code != http.StatusOK || up.hits.Load() != 2 || up.lastAuth() != "Bearer new-key" {
		t.Fatalf("status %d hits %d auth %q, want the single 401 retry with the new key", code, up.hits.Load(), up.lastAuth())
	}
}
