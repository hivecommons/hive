package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func discoveryTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func discoveryTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return key
}

// discoveryServer builds an httptest server that answers the two App-level
// endpoints discovery uses. orgInstall maps an org login to the installation
// GET /orgs/{org}/installation should return (0 = 404). listInstalls is what
// GET /app/installations returns.
type discoveryServerOpts struct {
	orgInstall     map[string]int64
	orgInstallCode int // status for a miss; 0 => 404
	listInstalls   []map[string]any
	listPages      [][]map[string]any
	listStatus     int // non-zero => fail the listing with this status
	// getInstallation answers GET /app/installations/{id} (VerifyInstallation).
	getInstallation map[string]any
}

func newDiscoveryServer(t *testing.T, opts discoveryServerOpts, hits *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt64(hits, 1)
		}
		path := r.URL.Path
		switch {
		case strings.HasPrefix(path, "/orgs/") && strings.HasSuffix(path, "/installation"):
			org := strings.TrimSuffix(strings.TrimPrefix(path, "/orgs/"), "/installation")
			if id, ok := opts.orgInstall[org]; ok && id != 0 {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"id":      id,
					"account": map[string]any{"login": org},
				})
				return
			}
			code := opts.orgInstallCode
			if code == 0 {
				code = http.StatusNotFound
			}
			w.WriteHeader(code)
			w.Write([]byte(`{"message":"Not Found"}`))
		case path == "/app/installations":
			if opts.listStatus != 0 {
				w.WriteHeader(opts.listStatus)
				w.Write([]byte(`{"message":"boom"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			list := opts.listInstalls
			if len(opts.listPages) > 0 {
				page := 1
				if got := r.URL.Query().Get("page"); got != "" {
					fmt.Sscanf(got, "%d", &page)
				}
				if page < 1 || page > len(opts.listPages) {
					list = []map[string]any{}
				} else {
					list = opts.listPages[page-1]
					if page < len(opts.listPages) {
						next := *r.URL
						q := next.Query()
						q.Set("page", fmt.Sprintf("%d", page+1))
						next.RawQuery = q.Encode()
						w.Header().Set("Link", `<http://`+r.Host+next.String()+`>; rel="next"`)
					}
				}
			}
			if list == nil {
				list = []map[string]any{}
			}
			json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(path, "/app/installations/"):
			if opts.getInstallation == nil {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(opts.getInstallation)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"unexpected path ` + path + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func installEntry(id int64, login string) map[string]any {
	return map[string]any{"id": id, "account": map[string]any{"login": login}}
}

func TestDiscoverInstallationID(t *testing.T) {
	tests := []struct {
		name    string
		opts    discoveryServerOpts
		org     string
		wantID  int64
		wantErr bool
		// wantNoInstall asserts the error is the authoritative
		// "not installed" sentinel, not a transient failure.
		wantNoInstall bool
	}{
		{
			name: "direct org lookup succeeds",
			opts: discoveryServerOpts{
				orgInstall: map[string]int64{"open-source": 99887},
			},
			org:    "open-source",
			wantID: 99887,
		},
		{
			name: "falls back to listing when direct lookup 404s",
			opts: discoveryServerOpts{
				listInstalls: []map[string]any{
					installEntry(41414, "Andrew-Anderson"),
					installEntry(55555, "open-source"),
				},
			},
			org:    "open-source",
			wantID: 55555,
		},
		{
			name: "listing match is case-insensitive",
			opts: discoveryServerOpts{
				listInstalls: []map[string]any{installEntry(77777, "Open-Source")},
			},
			org:    "open-source",
			wantID: 77777,
		},
		{
			name: "listing match can be on a later page",
			opts: discoveryServerOpts{
				listPages: [][]map[string]any{
					{installEntry(11111, "someone-else")},
					{installEntry(22222, "open-source")},
				},
			},
			org:    "open-source",
			wantID: 22222,
		},
		{
			name: "falls back to listing when direct lookup 405s (old GHE)",
			opts: discoveryServerOpts{
				orgInstallCode: http.StatusMethodNotAllowed,
				listInstalls:   []map[string]any{installEntry(31337, "katamari")},
			},
			org:    "katamari",
			wantID: 31337,
		},
		{
			name: "no matching installation leaves nothing to adopt",
			opts: discoveryServerOpts{
				listInstalls: []map[string]any{
					installEntry(41414, "Andrew-Anderson"),
					installEntry(1, "someone-else"),
				},
			},
			org:           "open-source",
			wantErr:       true,
			wantNoInstall: true,
		},
		{
			name: "empty installation list is not a match",
			opts: discoveryServerOpts{
				listInstalls: []map[string]any{},
			},
			org:           "open-source",
			wantErr:       true,
			wantNoInstall: true,
		},
		{
			name: "multiple matching installations are ambiguous",
			opts: discoveryServerOpts{
				listInstalls: []map[string]any{
					installEntry(10, "open-source"),
					installEntry(11, "Open-Source"),
				},
			},
			org:           "open-source",
			wantErr:       true,
			wantNoInstall: true,
		},
		{
			name: "nil entries in the listing are skipped, not dereferenced",
			opts: discoveryServerOpts{
				listInstalls: []map[string]any{
					nil,
					installEntry(4242, "open-source"),
				},
			},
			org:    "open-source",
			wantID: 4242,
		},
		{
			name: "api failure fails soft with an error and no id",
			opts: discoveryServerOpts{
				listStatus: http.StatusInternalServerError,
			},
			org:     "open-source",
			wantErr: true,
			// A 500 is transient — it must NOT be reported as
			// authoritative "not installed".
			wantNoInstall: false,
		},
		{
			name: "unauthorized listing is a soft discovery error",
			opts: discoveryServerOpts{
				orgInstallCode: http.StatusUnauthorized,
				listStatus:     http.StatusUnauthorized,
			},
			org:           "open-source",
			wantErr:       true,
			wantNoInstall: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ResetInstallationDiscoveryCache()
			srv := newDiscoveryServer(t, tc.opts, nil)
			auth := &AppAuth{
				appID: 5686, installationID: 41414,
				key: discoveryTestKey(t), logger: discoveryTestLogger(),
				apiURL: srv.URL,
			}
			id, err := auth.DiscoverInstallationID(context.Background(), tc.org)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got id %d", id)
				}
				if id != 0 {
					t.Errorf("id = %d on error, want 0 so the caller leaves config untouched", id)
				}
				if got := errors.Is(err, ErrNoInstallationForOrg); got != tc.wantNoInstall {
					t.Errorf("errors.Is(ErrNoInstallationForOrg) = %v, want %v (err: %v)", got, tc.wantNoInstall, err)
				}
				// The configured (wrong) ID must be untouched.
				if auth.InstallationID() != 41414 {
					t.Errorf("installationID mutated to %d on a failed discovery", auth.InstallationID())
				}
				return
			}
			if err != nil {
				t.Fatalf("DiscoverInstallationID: %v", err)
			}
			if id != tc.wantID {
				t.Errorf("id = %d, want %d", id, tc.wantID)
			}
		})
	}
}

// A direct lookup that somehow answers with a different account must be
// rejected: adopting a plausible-but-wrong ID reproduces the exact 403 this
// feature exists to cure.
func TestDiscoverInstallationID_RejectsMismatchedAccount(t *testing.T) {
	ResetInstallationDiscoveryCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id":      41414,
			"account": map[string]any{"login": "Andrew-Anderson"},
		})
	}))
	defer srv.Close()

	auth := &AppAuth{
		appID: 5686, installationID: 41414,
		key: discoveryTestKey(t), logger: discoveryTestLogger(), apiURL: srv.URL,
	}
	id, err := auth.DiscoverInstallationID(context.Background(), "open-source")
	if err == nil {
		t.Fatalf("expected rejection, got id %d", id)
	}
	if !errors.Is(err, ErrNoInstallationForOrg) {
		t.Errorf("err = %v, want ErrNoInstallationForOrg", err)
	}
	if id != 0 {
		t.Errorf("id = %d, want 0", id)
	}
}

// A missing private key is not a discovery failure — the hive simply is not
// App-authenticated and callers must skip it.
func TestDiscoverInstallationID_MissingKeyIsSkipped(t *testing.T) {
	ResetInstallationDiscoveryCache()
	auth := &AppAuth{appID: 1, logger: discoveryTestLogger()}
	if auth.HasKey() {
		t.Fatal("HasKey() = true with no key")
	}
	id, err := auth.DiscoverInstallationID(context.Background(), "open-source")
	if err == nil {
		t.Fatal("expected an error explaining the key is absent")
	}
	if id != 0 {
		t.Errorf("id = %d, want 0", id)
	}

	// The self-heal entry point must treat it as a silent no-op, NOT an error.
	newID, err := auth.RediscoverAndAdopt(context.Background(), "open-source", discoveryTestLogger())
	if err != nil {
		t.Errorf("RediscoverAndAdopt with no key returned error %v, want nil (skip silently)", err)
	}
	if newID != 0 {
		t.Errorf("newID = %d, want 0", newID)
	}

	var nilAuth *AppAuth
	if _, err := nilAuth.RediscoverAndAdopt(context.Background(), "open-source", discoveryTestLogger()); err != nil {
		t.Errorf("nil AppAuth RediscoverAndAdopt returned %v, want nil", err)
	}
}

// The result is cached so the self-heal loop cannot hammer the API on every
// beat — including the negative "not installed on this org" result.
func TestDiscoverInstallationID_CachesResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts discoveryServerOpts
	}{
		{"positive", discoveryServerOpts{orgInstall: map[string]int64{"open-source": 900}}},
		{"negative", discoveryServerOpts{listInstalls: []map[string]any{installEntry(1, "nope")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetInstallationDiscoveryCache()
			var hits int64
			srv := newDiscoveryServer(t, tc.opts, &hits)
			auth := &AppAuth{
				appID: 5686, installationID: 41414,
				key: discoveryTestKey(t), logger: discoveryTestLogger(), apiURL: srv.URL,
			}
			for i := 0; i < 5; i++ {
				auth.DiscoverInstallationID(context.Background(), "open-source")
			}
			first := atomic.LoadInt64(&hits)
			if first == 0 {
				t.Fatal("no API calls at all — server never exercised")
			}
			// Whatever the first attempt cost (1 direct, or 1 direct + 1 list),
			// it must not scale with the number of calls.
			if first > 2 {
				t.Errorf("hit the API %d times for 5 calls, want <= 2 (TTL cache)", first)
			}
		})
	}
}

// GHE base URLs must be honoured — discovery must never touch api.github.com.
func TestDiscoverInstallationID_HonoursGHEBaseURL(t *testing.T) {
	ResetInstallationDiscoveryCache()
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{
			"id":      777,
			"account": map[string]any{"login": "open-source"},
		})
	}))
	defer srv.Close()

	// Mirror the osscar hive: a GHE API URL with the /api/v3 path prefix.
	auth := &AppAuth{
		appID: 5686, installationID: 41414,
		key: discoveryTestKey(t), logger: discoveryTestLogger(),
		apiURL: srv.URL + "/api/v3",
	}
	id, err := auth.DiscoverInstallationID(context.Background(), "open-source")
	if err != nil {
		t.Fatalf("DiscoverInstallationID: %v", err)
	}
	if id != 777 {
		t.Errorf("id = %d, want 777", id)
	}
	if len(gotPaths) == 0 {
		t.Fatal("no request reached the GHE test server")
	}
	if !strings.HasPrefix(gotPaths[0], "/api/v3/") {
		t.Errorf("request path = %q, want the GHE /api/v3 prefix (base URL not honoured)", gotPaths[0])
	}
}

// The live osscar case end to end: installation 41414 belongs to
// "Andrew-Anderson", the App IS installed on "open-source" as 55555, so the
// hive must adopt 55555.
func TestRediscoverAndAdopt_WrongAccountIsCorrected(t *testing.T) {
	ResetInstallationDiscoveryCache()
	srv := newDiscoveryServer(t, discoveryServerOpts{
		getInstallation: map[string]any{
			"id":          41414,
			"account":     map[string]any{"login": "Andrew-Anderson"},
			"permissions": map[string]any{"issues": "write"},
		},
		orgInstall: map[string]int64{"open-source": 55555},
	}, nil)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "token.cache")
	if err := os.WriteFile(cachePath, []byte("stale-token"), 0o600); err != nil {
		t.Fatalf("seeding token cache: %v", err)
	}

	auth := &AppAuth{
		appID: 5686, installationID: 41414,
		key: discoveryTestKey(t), logger: discoveryTestLogger(),
		apiURL: srv.URL, cachePath: cachePath,
		cachedToken: "stale-token",
	}

	newID, err := auth.RediscoverAndAdopt(context.Background(), "open-source", discoveryTestLogger())
	if err != nil {
		t.Fatalf("RediscoverAndAdopt: %v", err)
	}
	if newID != 55555 {
		t.Fatalf("newID = %d, want 55555", newID)
	}
	if auth.InstallationID() != 55555 {
		t.Errorf("installationID = %d, want 55555 adopted in place", auth.InstallationID())
	}
	// The cached token was minted for the WRONG installation and would keep
	// 403ing on writes — it must be dropped, in memory and on disk.
	if auth.cachedToken != "" {
		t.Errorf("cachedToken = %q, want cleared after adopting a new installation", auth.cachedToken)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Errorf("stale on-disk token cache still present (stat err %v)", err)
	}
}

// When the installation already belongs to the right account, nothing changes
// and no discovery call is made.
func TestRediscoverAndAdopt_CorrectAccountIsLeftAlone(t *testing.T) {
	ResetInstallationDiscoveryCache()
	srv := newDiscoveryServer(t, discoveryServerOpts{
		getInstallation: map[string]any{
			"id":          55555,
			"account":     map[string]any{"login": "open-source"},
			"permissions": map[string]any{"issues": "write"},
		},
		// A direct lookup would answer with a DIFFERENT id; if it is ever
		// consulted here, the assertion below catches it.
		orgInstall: map[string]int64{"open-source": 12345},
	}, nil)

	auth := &AppAuth{
		appID: 5686, installationID: 55555,
		key: discoveryTestKey(t), logger: discoveryTestLogger(), apiURL: srv.URL,
	}
	newID, err := auth.RediscoverAndAdopt(context.Background(), "open-source", discoveryTestLogger())
	if err != nil {
		t.Fatalf("RediscoverAndAdopt: %v", err)
	}
	if newID != 0 {
		t.Errorf("newID = %d, want 0 (no change)", newID)
	}
	if auth.InstallationID() != 55555 {
		t.Errorf("installationID = %d, want 55555 unchanged", auth.InstallationID())
	}
}

// Wrong account, but the App is genuinely not installed on the target org:
// leave the config untouched so the existing "check your installation_id"
// banner stands.
func TestRediscoverAndAdopt_NoInstallationLeavesConfigUntouched(t *testing.T) {
	ResetInstallationDiscoveryCache()
	srv := newDiscoveryServer(t, discoveryServerOpts{
		getInstallation: map[string]any{
			"id":      41414,
			"account": map[string]any{"login": "Andrew-Anderson"},
		},
		listInstalls: []map[string]any{installEntry(41414, "Andrew-Anderson")},
	}, nil)

	auth := &AppAuth{
		appID: 5686, installationID: 41414,
		key: discoveryTestKey(t), logger: discoveryTestLogger(), apiURL: srv.URL,
	}
	newID, err := auth.RediscoverAndAdopt(context.Background(), "open-source", discoveryTestLogger())
	if err == nil {
		t.Fatal("expected an error reporting the app is not installed on the org")
	}
	if !errors.Is(err, ErrNoInstallationForOrg) {
		t.Errorf("err = %v, want ErrNoInstallationForOrg", err)
	}
	if newID != 0 {
		t.Errorf("newID = %d, want 0", newID)
	}
	if auth.InstallationID() != 41414 {
		t.Errorf("installationID = %d, want the configured 41414 left untouched", auth.InstallationID())
	}
}

// A verification failure (API down) must never cause a guess.
func TestRediscoverAndAdopt_VerifyFailureNeverGuesses(t *testing.T) {
	ResetInstallationDiscoveryCache()
	auth := &AppAuth{
		appID: 5686, installationID: 41414,
		key: discoveryTestKey(t), logger: discoveryTestLogger(),
		apiURL: "http://127.0.0.1:1", // connection refused
	}
	newID, err := auth.RediscoverAndAdopt(context.Background(), "open-source", discoveryTestLogger())
	if err == nil {
		t.Fatal("expected a verification error")
	}
	if newID != 0 || auth.InstallationID() != 41414 {
		t.Errorf("newID = %d, installationID = %d — must not change when verification fails",
			newID, auth.InstallationID())
	}
}

// An empty org means we have nothing to match against — never guess.
func TestRediscoverAndAdopt_EmptyOrgIsNoOp(t *testing.T) {
	ResetInstallationDiscoveryCache()
	auth := &AppAuth{
		appID: 1, installationID: 2,
		key: discoveryTestKey(t), logger: discoveryTestLogger(),
	}
	newID, err := auth.RediscoverAndAdopt(context.Background(), "  ", discoveryTestLogger())
	if err != nil || newID != 0 {
		t.Errorf("newID = %d, err = %v — want a silent no-op", newID, err)
	}
	if _, err := auth.DiscoverInstallationID(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty target org")
	}
}
