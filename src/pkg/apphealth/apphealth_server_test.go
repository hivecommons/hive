package apphealth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Server-backed tests. The verdicts these cover are driven by the ACTUAL HTTP
// status GitHub returns, which is the whole point of the classification: a
// 401, a 403 and a 404 mean three different people have to do three different
// things, and telling the wrong one wastes real debugging time.
//
// NewAppAuthFromPEM takes the API URL, so an httptest server is enough --
// pkg/apphealth needs no access to pkg/github internals.

const testAppID = 5686
const testInstallationID = 42980

func testPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// appAuthAgainst builds a real AppAuth pointed at srv.
func appAuthAgainst(t *testing.T, srv *httptest.Server) *github.AppAuth {
	t.Helper()
	auth, err := github.NewAppAuthFromPEM(testAppID, testInstallationID, testPEM(t), quietLogger(), srv.URL)
	if err != nil {
		t.Fatalf("building app auth: %v", err)
	}
	return auth
}

// installationServer answers the installation probe with the given status and
// body, and mints tokens so coverage listing can proceed.
type installationServer struct {
	status   int
	body     string
	repos    string // JSON body for GET /installation/repositories
	repoStat int
}

func (s installationServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_test","expires_at":"2099-01-01T00:00:00Z"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	})
	mux.HandleFunc("/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		status := s.repoStat
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(s.repos))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestClassifyFailureRaisesOnActionableFault(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantRaise bool
		wantState github.AppAuthState
	}{
		{
			name:      "404 means the app is not installed and the user can fix it",
			status:    http.StatusNotFound,
			body:      `{"message":"Not Found"}`,
			wantRaise: true,
			wantState: github.AppStateNotInstalled,
		},
		{
			name:      "403 is a wrong installation",
			status:    http.StatusForbidden,
			body:      `{"message":"Resource not accessible by integration"}`,
			wantRaise: true,
			wantState: github.AppStateWrongInstallation,
		},
		{
			name:      "401 undecodable JWT is an operator key fault",
			status:    http.StatusUnauthorized,
			body:      `{"message":"A JSON web token could not be decoded"}`,
			wantRaise: true,
			wantState: github.AppStateKeyInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := installationServer{status: tt.status, body: tt.body}.start(t)
			c := Checker{BannerAttempts: 1}

			raise, msg, state := c.ClassifyFailure(context.Background(), appAuthAgainst(t, srv), "katamari", quietLogger())

			if raise != tt.wantRaise {
				t.Fatalf("raise=%v want %v (msg=%q state=%v)", raise, tt.wantRaise, msg, state)
			}
			if state != tt.wantState {
				t.Fatalf("state=%v want %v", state, tt.wantState)
			}
			if raise && msg == "" {
				t.Fatal("a raised banner with no copy leaves the operator nothing to act on")
			}
		})
	}
}

// TestClassifyFailureRetriesAnUnknownVerdict is rule 2 of #2224: a cold start
// racing DNS/egress must not be mistaken for a definitive verdict. A 500 is
// unclassifiable, so the probe is retried before the verdict is accepted, and
// it still must NOT raise.
func TestClassifyFailureRetriesAnUnknownVerdict(t *testing.T) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"server error"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := Checker{BannerAttempts: 3, RetryDelay: time.Millisecond}
	raise, msg, state := c.ClassifyFailure(context.Background(), appAuthAgainst(t, srv), "katamari", quietLogger())

	if raise {
		t.Fatalf("an unknown verdict must never raise the banner, got msg=%q", msg)
	}
	if state != github.AppStateUnknown {
		t.Fatalf("state=%v want AppStateUnknown", state)
	}
	if calls < 2 {
		t.Fatalf("an inconclusive probe must be retried before the verdict is accepted; got %d call(s)", calls)
	}
}

func TestClassifyWriteForbiddenPrefersARealDiagnosis(t *testing.T) {
	// The installation itself is broken (404 = not installed). That is the
	// accurate cause, and it must be reported INSTEAD of the repo-scope story.
	srv := installationServer{status: http.StatusNotFound, body: `{"message":"Not Found"}`}.start(t)
	var c Checker

	msg, state := c.ClassifyWriteForbidden(context.Background(), appAuthAgainst(t, srv), "katamari", "hive")

	if state == github.AppStateWriteForbidden {
		t.Fatalf("a classifiable App fault must not be masked by the repo-scope verdict, got %q", msg)
	}
	if state != github.AppStateNotInstalled {
		t.Fatalf("state=%v want AppStateNotInstalled", state)
	}
}

func TestClassifyRepoCoverageReportsMissingRepos(t *testing.T) {
	srv := installationServer{
		status: http.StatusOK,
		body:   `{"id":42980,"account":{"login":"katamari"}}`,
		repos:  `{"total_count":1,"repositories":[{"full_name":"katamari/hive","name":"hive","owner":{"login":"katamari"}}]}`,
	}.start(t)

	raise, msg, state := ClassifyRepoCoverage(context.Background(), appAuthAgainst(t, srv),
		"katamari", []string{"hive", "console"}, quietLogger())

	if !raise {
		t.Fatal("a configured repo outside the installation must be reported; that is the #4360 blind spot")
	}
	if state != github.AppStateRepoNotCovered {
		t.Fatalf("state=%v want AppStateRepoNotCovered", state)
	}
	if !strings.Contains(msg, "console") {
		t.Fatalf("the verdict must name the uncovered repo, got %q", msg)
	}
}

func TestClassifyRepoCoverageSilentWhenFullyCovered(t *testing.T) {
	srv := installationServer{
		status: http.StatusOK,
		body:   `{"id":42980,"account":{"login":"katamari"}}`,
		repos:  `{"total_count":1,"repositories":[{"full_name":"katamari/hive","name":"hive","owner":{"login":"katamari"}}]}`,
	}.start(t)

	raise, msg, state := ClassifyRepoCoverage(context.Background(), appAuthAgainst(t, srv),
		"katamari", []string{"hive"}, quietLogger())

	if raise {
		t.Fatalf("a fully covered installation must stay silent, got %q", msg)
	}
	if state != github.AppStateOK {
		t.Fatalf("state=%v want AppStateOK", state)
	}
}

// TestClassifyRepoCoverageDefersWhenListingFails pins the #4360 rule that an
// error is NOT a verdict: if the listing cannot be fetched, the credentials
// are the likelier story and the other checks tell it better.
func TestClassifyRepoCoverageDefersWhenListingFails(t *testing.T) {
	srv := installationServer{
		status:   http.StatusOK,
		body:     `{"id":42980,"account":{"login":"katamari"}}`,
		repoStat: http.StatusInternalServerError,
		repos:    `{"message":"boom"}`,
	}.start(t)

	raise, msg, state := ClassifyRepoCoverage(context.Background(), appAuthAgainst(t, srv),
		"katamari", []string{"hive"}, quietLogger())

	if raise {
		t.Fatalf("a failed listing must never accuse the repos, got %q", msg)
	}
	if state != github.AppStateUnknown {
		t.Fatalf("state=%v want AppStateUnknown", state)
	}
}

// TestClassifyFailureDoesNotSleepAfterTheFinalAttempt closes the gap a
// mutation exposed: nothing else observes the guard that skips the backoff on
// the last attempt. With a single attempt and an hour-long delay, honoring the
// guard returns immediately; dropping it stalls the caller for an hour.
func TestClassifyFailureDoesNotSleepAfterTheFinalAttempt(t *testing.T) {
	srv := installationServer{status: http.StatusInternalServerError, body: `{"message":"server error"}`}.start(t)
	c := Checker{BannerAttempts: 1, RetryDelay: time.Hour}
	auth := appAuthAgainst(t, srv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.ClassifyFailure(context.Background(), auth, "katamari", quietLogger())
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ClassifyFailure slept after its final attempt; the backoff must only separate retries")
	}
}

func TestDiagnoseReportsHealthyInstallation(t *testing.T) {
	srv := installationServer{
		status: http.StatusOK,
		body:   `{"id":42980,"account":{"login":"katamari"},"permissions":{"issues":"write"}}`,
	}.start(t)
	var c Checker

	d := c.Diagnose(context.Background(), appAuthAgainst(t, srv), "katamari")
	if d.State != github.AppStateOK {
		t.Fatalf("a healthy installation must diagnose OK, got %v (%s)", d.State, d.Message())
	}
}
