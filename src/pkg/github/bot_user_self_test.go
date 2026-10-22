package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestResolveSelfLoginPanicReleasesPending(t *testing.T) {
	const panicValue = "lookup transport panic"
	var pending <-chan struct{}
	var calls int
	c := &Client{}
	c.client = gh.NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			var lookup bool
			_, pending, lookup = c.selfLoginLookupState()
			if pending == nil || lookup {
				t.Fatal("transport called without a pending lookup")
			}
			panic(panicValue)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"login":"pat-hive"}`)),
		}, nil
	})})

	func() {
		defer func() {
			if got := recover(); got != panicValue {
				t.Fatalf("panic = %v, want %q", got, panicValue)
			}
		}()
		c.resolveSelfLogin(context.Background())
	}()

	// A waiter using context.Background must be released by channel closure,
	// not just by its own cancellation. Check before retrying to avoid hanging
	// the test when the cleanup is missing.
	select {
	case <-pending:
	default:
		t.Fatal("lookup panic stranded pending waiters")
	}
	c.selfLoginMu.Lock()
	stillPending := c.selfLoginPending != nil
	c.selfLoginMu.Unlock()
	if stillPending {
		t.Fatal("lookup panic left a stale pending channel")
	}
	if login := c.resolveSelfLogin(context.Background()); login != "pat-hive" {
		t.Fatalf("login after panic = %q, want pat-hive", login)
	}
	if calls != 2 {
		t.Fatalf("GET /user calls = %d, want 2", calls)
	}
}

func TestResolveSelfLoginLeaderCancellationDoesNotPoisonLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	c := &Client{}
	c.client = gh.NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		if err := r.Context().Err(); err != nil {
			t.Errorf("leader cancellation reached shared request: %v", err)
			return nil, err
		}
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > botUserLookupTimeout {
			t.Error("shared request is not bounded by botUserLookupTimeout")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"login":"pat-hive"}`)),
		}, nil
	})})
	if login := c.resolveSelfLogin(ctx); login != "pat-hive" {
		t.Fatalf("leader login = %q, want pat-hive", login)
	}
	if login := c.resolveSelfLogin(context.Background()); login != "pat-hive" {
		t.Fatalf("subsequent login = %q, want pat-hive", login)
	}
	if calls != 1 {
		t.Fatalf("GET /user calls = %d, want 1", calls)
	}
	if !c.selfLoginFailedAt.IsZero() {
		t.Fatal("leader cancellation started the failure cooldown")
	}
}

func TestResolveSelfLoginSharesInflightLookup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", fail), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-release
				if fail {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"login":"pat-hive","id":1}`)
			}))
			defer server.Close()
			// Release the handler even if an assertion fails before publication.
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})
			results := make(chan string, 9)
			go func() { results <- c.resolveSelfLogin(context.Background()) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("lookup did not start")
			}

			// Cache reads and cancelled waiters must not wait for network I/O.
			read := make(chan string, 1)
			go func() { read <- c.cachedSelfLogin() }()
			select {
			case login := <-read:
				if login != "" {
					t.Fatalf("in-flight cached login = %q", login)
				}
			case <-time.After(time.Second):
				t.Fatal("cache read blocked behind lookup")
			}
			login, pending, lookup := c.selfLoginLookupState()
			if login != "" || pending == nil || lookup {
				t.Fatalf("in-flight state = (%q, %v, %v)", login, pending, lookup)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			go func() { read <- c.resolveSelfLogin(ctx) }()
			select {
			case login := <-read:
				if login != "" {
					t.Fatalf("cancelled waiter login = %q", login)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled waiter blocked behind lookup")
			}
			for i := 0; i < 8; i++ {
				go func() { results <- c.resolveSelfLogin(context.Background()) }()
			}
			close(release)
			want := "pat-hive"
			if fail {
				want = ""
			}
			for i := 0; i < 9; i++ {
				select {
				case login := <-results:
					if login != want {
						t.Errorf("login = %q, want %q", login, want)
					}
				case <-time.After(time.Second):
					t.Fatal("lookup waiter did not finish")
				}
			}
			select {
			case <-pending:
			default:
				t.Fatal("in-flight lookup was not closed")
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("GET /user calls = %d, want 1", n)
			}
			if fail {
				c.selfLoginMu.Lock()
				c.selfLoginFailedAt = time.Now().Add(-selfLoginRetryAfter)
				c.selfLoginMu.Unlock()
				if login := c.resolveSelfLogin(context.Background()); login != "" {
					t.Fatalf("failed retry login = %q", login)
				}
				if n := calls.Load(); n != 2 {
					t.Fatalf("GET /user calls after retry = %d, want 2", n)
				}
			}
		})
	}
}

func TestMarkerAuthorTrustedResolvesOwnLogin(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"pat-hive","id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})

	for i := 0; i < 5; i++ {
		if !c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("pat-hive")}) {
			t.Fatal("pat-hive marker should be trusted")
		}
		if !c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("PAT-Hive")}) {
			t.Fatal("login match should be case-insensitive")
		}
		if c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("mallory")}) {
			t.Fatal("mallory marker must not be trusted")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("GET /user calls = %d, want 1", n)
	}
}

func TestMarkerAuthorTrustedFallsBackWhenSelfLookupFails(t *testing.T) {
	var calls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})

	for i := 0; i < 5; i++ {
		if !c.markerAuthorTrusted(&gh.User{Login: gh.Ptr("mallory")}) {
			t.Fatal("legacy any-author fallback should apply when the lookup fails")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("GET /user calls = %d, want 1 within the retry-after window", n)
	}
}

func TestCachedSelfLoginDoesNotBlockDuringInflightResolve(t *testing.T) {
	entered := make(chan struct{})
	gate := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"login":"pat-hive","id":1}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hivecommons/hive"})

	resolved := make(chan string, 1)
	go func() { resolved <- c.resolveSelfLogin(t.Context()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("GET /user was never issued")
	}

	cached := make(chan string, 1)
	go func() { cached <- c.cachedSelfLogin() }()
	select {
	case got := <-cached:
		if got != "" {
			t.Fatalf("cachedSelfLogin = %q before the lookup finished, want empty", got)
		}
	case <-time.After(2 * time.Second):
		close(gate)
		t.Fatal("cachedSelfLogin blocked while resolveSelfLogin was in flight")
	}

	close(gate)
	select {
	case got := <-resolved:
		if got != "pat-hive" {
			t.Fatalf("resolveSelfLogin = %q, want pat-hive", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolveSelfLogin did not finish")
	}
	if got := c.cachedSelfLogin(); got != "pat-hive" {
		t.Fatalf("cachedSelfLogin = %q after resolve, want pat-hive", got)
	}
}
