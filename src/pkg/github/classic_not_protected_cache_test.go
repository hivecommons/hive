package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// classicChecksAPI serves o/r main's classic required_status_checks answer,
// a branch payload the App may not read, and the rules, counting each read.
// Once failing is set every endpoint answers 503.
type classicChecksAPI struct {
	client                 *gh.Client
	classic, branch, rules atomic.Int64
	failing                atomic.Bool
}

func newClassicChecksAPI(t *testing.T, classic func(w http.ResponseWriter), rulesBody string) *classicChecksAPI {
	t.Helper()
	api := &classicChecksAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func() bool {
			if api.failing.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"message":"unavailable"}`))
				return true
			}
			return false
		}
		switch r.URL.Path {
		case "/repos/o/r/branches/main/protection/required_status_checks":
			api.classic.Add(1)
			if !fail() {
				classic(w)
			}
		case "/repos/o/r/branches/main":
			api.branch.Add(1)
			if !fail() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(classicForbidden))
			}
		case "/repos/o/r/rules/branches/main":
			api.rules.Add(1)
			if !fail() {
				_, _ = w.Write([]byte(rulesBody))
			}
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client := gh.NewClient(nil)
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	client.BaseURL = base
	api.client = client
	return api
}

func classicAnswer(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// classicRateLimited is a primary rate-limit 403. The reset is in the past so
// go-github does not refuse the next request itself.
func classicRateLimited(w http.ResponseWriter) {
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "0")
	w.Header().Set("X-RateLimit-Reset", "1")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation."}`))
}

// The sentinel and the raw 404 form are both "branch not protected"; a 404
// with another message, a 403 and a rate limit are not.
func TestIsClassicBranchNotProtected(t *testing.T) {
	notFound := &http.Response{StatusCode: http.StatusNotFound}
	forbidden := &http.Response{StatusCode: http.StatusForbidden}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"sentinel", gh.ErrBranchNotProtected, true},
		{"wrapped sentinel", fmt.Errorf("classic read: %w", gh.ErrBranchNotProtected), true},
		{"raw 404 branch not protected", &gh.ErrorResponse{Response: notFound, Message: "Branch not protected"}, true},
		{"wrapped raw 404", fmt.Errorf("classic read: %w", &gh.ErrorResponse{Response: notFound, Message: "Branch not protected"}), true},
		{"404 with another message", &gh.ErrorResponse{Response: notFound, Message: "Not Found"}, false},
		{"404 required checks not enabled", &gh.ErrorResponse{Response: notFound, Message: "Required status checks not enabled"}, false},
		{"403", &gh.ErrorResponse{Response: forbidden, Message: "Resource not accessible by integration"}, false},
		{"rate limit", &gh.RateLimitError{Response: forbidden, Message: "API rate limit exceeded"}, false},
		{"other error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClassicBranchNotProtected(tc.err); got != tc.want {
				t.Fatalf("isClassicBranchNotProtected(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// A raw 404 "Branch not protected" is remembered as the sentinel, never as a
// 404; a 404 with another message and a rate limit are not remembered; and a
// remembered 403/404 is not a remembered "not protected".
func TestRememberClassicNotProtectedKeepsAnswersApart(t *testing.T) {
	resetBranchRulesClassicCacheForTest(t, time.Now, time.Hour)
	notFound := &http.Response{StatusCode: http.StatusNotFound}

	if !rememberClassicNotProtected("raw", &gh.ErrorResponse{Response: notFound, Message: "Branch not protected"}) {
		t.Fatal("raw 404 branch not protected was not remembered")
	}
	if got := sharedBranchRulesClassicCache.get("raw"); got != gh.ErrBranchNotProtected || !classicNotProtectedRemembered("raw") {
		t.Fatalf("remembered raw 404 = %v, want the gh.ErrBranchNotProtected sentinel", got)
	}
	if rememberClassicNotProtected("other404", &gh.ErrorResponse{Response: notFound, Message: "Not Found"}) || sharedBranchRulesClassicCache.get("other404") != nil {
		t.Fatal("a 404 with another message was remembered as branch not protected")
	}
	if rememberClassicNotProtected("rate", &gh.RateLimitError{Message: "API rate limit exceeded"}) || sharedBranchRulesClassicCache.get("rate") != nil {
		t.Fatal("a rate limit was remembered")
	}
	sharedBranchRulesClassicCache.put("forbidden", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}, Message: "Resource not accessible by integration"})
	sharedBranchRulesClassicCache.put("notfound", &gh.ErrorResponse{Response: notFound, Message: "Not Found"})
	if classicNotProtectedRemembered("forbidden") || classicNotProtectedRemembered("notfound") {
		t.Fatal("a remembered 403/404 read as branch not protected")
	}
}

// "Branch not protected" keeps for 15 minutes while a 403 keeps for the
// forbidden TTL; HIVE_CLASSIC_NOT_PROTECTED_TTL overrides the 15 minutes and
// 0 turns it off without touching the 403/404 entries.
func TestClassicNotProtectedTTL(t *testing.T) {
	forbidden := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusForbidden}, Message: "Resource not accessible by integration"}

	t.Run("default 15m, forbidden 1h", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		resetBranchRulesClassicCacheForTest(t, func() time.Time { return now }, time.Hour)
		sharedBranchRulesClassicCache.put("np", gh.ErrBranchNotProtected)
		sharedBranchRulesClassicCache.put("403", forbidden)
		now = now.Add(15*time.Minute + time.Second)
		if classicNotProtectedRemembered("np") {
			t.Fatal("branch not protected still remembered after 15m")
		}
		if sharedBranchRulesClassicCache.get("403") == nil {
			t.Fatal("403 forgotten after 15m, want it kept for the forbidden TTL")
		}
	})

	t.Run("env override", func(t *testing.T) {
		t.Setenv(classicNotProtectedTTLEnv, "5m")
		if got := newBranchRulesClassicCache().notProtectedTTL; got != 5*time.Minute {
			t.Fatalf("notProtectedTTL = %v, want 5m", got)
		}
	})

	t.Run("unset is 15m", func(t *testing.T) {
		t.Setenv(classicNotProtectedTTLEnv, "")
		if got := newBranchRulesClassicCache().notProtectedTTL; got != 15*time.Minute {
			t.Fatalf("notProtectedTTL = %v, want 15m", got)
		}
	})

	t.Run("zero disables", func(t *testing.T) {
		t.Setenv(classicNotProtectedTTLEnv, "0")
		old := sharedBranchRulesClassicCache
		sharedBranchRulesClassicCache = newBranchRulesClassicCache()
		t.Cleanup(func() { sharedBranchRulesClassicCache = old })
		if sharedBranchRulesClassicCache.ttl <= 0 {
			t.Skip("forbidden TTL disabled in this environment")
		}
		rememberClassicNotProtected("np", gh.ErrBranchNotProtected)
		if classicNotProtectedRemembered("np") {
			t.Fatal("branch not protected remembered with HIVE_CLASSIC_NOT_PROTECTED_TTL=0")
		}
		sharedBranchRulesClassicCache.put("403", forbidden)
		if sharedBranchRulesClassicCache.get("403") == nil {
			t.Fatal("403 not remembered; turning off the not-protected TTL must not touch it")
		}
	})
}

// The native-queue observation reads a remembered "branch not protected" as
// the same known policy the live 404 gives, with the rules still read.
func TestNativeObservationRequiredRemembersBranchNotProtected(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	resetBranchRulesClassicCacheForTest(t, func() time.Time { return now }, time.Hour)
	api := newClassicChecksAPI(t, classicAnswer(http.StatusNotFound, classicNotProtected), rulesetStrict)
	c := &Client{client: api.client}

	first, firstKnown := c.nativeObservationRequired(context.Background(), "o", "r", "main")
	if !firstKnown || !reflect.DeepEqual(first, map[string]int64{"test": 0}) {
		t.Fatalf("live read = (%v, %v), want ({test:0}, true)", first, firstKnown)
	}
	if got, known := c.nativeObservationRequired(context.Background(), "o", "r", "main"); known != firstKnown || !reflect.DeepEqual(got, first) {
		t.Fatalf("remembered read = (%v, %v), want (%v, %v)", got, known, first, firstKnown)
	}
	if n := api.classic.Load(); n != 1 {
		t.Fatalf("classic calls = %d, want 1", n)
	}
	if n := api.rules.Load(); n != 2 {
		t.Fatalf("rules calls = %d, want 2", n)
	}
	now = now.Add(defaultClassicNotProtectedTTL + time.Second)
	if got, known := c.nativeObservationRequired(context.Background(), "o", "r", "main"); known != firstKnown || !reflect.DeepEqual(got, first) {
		t.Fatalf("read after TTL = (%v, %v), want (%v, %v)", got, known, first, firstKnown)
	}
	if n := api.classic.Load(); n != 2 {
		t.Fatalf("classic calls after 15m = %d, want 2", n)
	}
}

// A remembered "branch not protected" still leaves a failed rules read
// unknown on the native-queue path, as a live one does.
func TestNativeObservationRequiredRulesFailureStaysUnknown(t *testing.T) {
	resetBranchRulesClassicCacheForTest(t, time.Now, time.Hour)
	api := newClassicChecksAPI(t, classicAnswer(http.StatusNotFound, classicNotProtected), rulesetStrict)
	c := &Client{client: api.client}
	if _, known := c.nativeObservationRequired(context.Background(), "o", "r", "main"); !known {
		t.Fatal("live read unknown, want known")
	}
	api.failing.Store(true)
	if _, known := c.nativeObservationRequired(context.Background(), "o", "r", "main"); known {
		t.Fatal("remembered not-protected with a failed rules read is known, want unknown")
	}
}

// A rate-limited classic read is not remembered on the native-queue path.
func TestNativeObservationRequiredDoesNotRememberRateLimit(t *testing.T) {
	resetBranchRulesClassicCacheForTest(t, time.Now, time.Hour)
	api := newClassicChecksAPI(t, classicRateLimited, rulesetStrict)
	c := &Client{client: api.client}
	for i := 0; i < 2; i++ {
		if _, known := c.nativeObservationRequired(context.Background(), "o", "r", "main"); known {
			t.Fatalf("read %d known, want unknown after a rate limit", i)
		}
	}
	if n := api.classic.Load(); n != 2 {
		t.Fatalf("classic calls = %d, want 2 (rate limit not remembered)", n)
	}
}

// ReadBranchRules and the native-queue observation share the entry; a 403
// that ReadBranchRules remembers does not stand in for the native read.
func TestClassicNotProtectedSharedByLaneReaders(t *testing.T) {
	t.Run("not protected", func(t *testing.T) {
		resetBranchRulesClassicCacheForTest(t, time.Now, time.Hour)
		api := newClassicChecksAPI(t, classicAnswer(http.StatusNotFound, classicNotProtected), rulesetStrict)
		rules := ReadBranchRules(context.Background(), api.client, "o", "r", "main", nil, false)
		_, nativeKnown := (&Client{client: api.client}).nativeObservationRequired(context.Background(), "o", "r", "main")
		if n := api.classic.Load(); n != 1 {
			t.Fatalf("classic calls = %d, want 1 across both readers", n)
		}
		if !rules.Known || !nativeKnown {
			t.Fatalf("answers: rules=%+v native known=%v", rules, nativeKnown)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		resetBranchRulesClassicCacheForTest(t, time.Now, time.Hour)
		api := newClassicChecksAPI(t, classicAnswer(http.StatusForbidden, classicForbidden), rulesetStrict)
		_ = ReadBranchRules(context.Background(), api.client, "o", "r", "main", nil, false)
		if _, known := (&Client{client: api.client}).nativeObservationRequired(context.Background(), "o", "r", "main"); known {
			t.Fatal("native read after a remembered 403 is known, want the live 403 (unknown)")
		}
		if n := api.classic.Load(); n != 2 {
			t.Fatalf("classic calls = %d, want 2 (a remembered 403 is ReadBranchRules' own)", n)
		}
	})
}

// The required-check policy does not use the remembered answer. There a
// failed rules read after "not protected" counts as known-empty, so a
// remembered answer would turn an outage (the classic read failing too) into
// known-empty. Every read asks the classic endpoint, and once everything
// fails the policy falls back to config or unknown exactly as before.
func TestRequiredStatusCheckPolicyDoesNotUseClassicNotProtectedCache(t *testing.T) {
	for _, tc := range []struct {
		name        string
		configKnown bool
		wantKnown   bool
		wantSource  string
	}{
		{"no config is unknown", false, false, "none"},
		{"config is the fallback", true, true, "config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(resetRequiredChecksForbiddenCacheForTest(time.Now, time.Hour))
			resetBranchRulesClassicCacheForTest(t, time.Now, time.Hour)
			api := newClassicChecksAPI(t, classicAnswer(http.StatusNotFound, classicNotProtected), `[]`)
			config := map[string]bool{"ci": true}
			for i := 0; i < 2; i++ {
				_, known, _, _, source, _ := RequiredStatusCheckPolicy(context.Background(), api.client, "o", "r", "main", config, tc.configKnown)
				if !known || source != "protection-unprotected" {
					t.Fatalf("read %d = known %v source %q, want known-empty protection-unprotected", i, known, source)
				}
			}
			api.failing.Store(true)
			required, known, fromConfig, _, source, _ := RequiredStatusCheckPolicy(context.Background(), api.client, "o", "r", "main", config, tc.configKnown)
			if known != tc.wantKnown || source != tc.wantSource || fromConfig != tc.configKnown {
				t.Fatalf("read during outage = required %v known %v source %q fromConfig %v, want known %v source %q (not known-empty)", required, known, source, fromConfig, tc.wantKnown, tc.wantSource)
			}
			if n := api.classic.Load(); n != 3 {
				t.Fatalf("classic calls = %d, want 3 (the policy path never replays not-protected)", n)
			}
		})
	}
}
