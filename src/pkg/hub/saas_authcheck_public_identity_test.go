package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #7453: on a hosted spoke the Operations tab never recognised a signed-in
// user — /api/contribute/me was 401 for everyone, the owner included — because
// the identity #7364 expects (X-Hive-User from the hub's nginx) never reached
// /api/contribute: the hive-contribute Ingress had no auth-url, and even the
// gated Ingresses got a bare 200 for a public path, before the hub resolved
// the user. Both halves are pinned here.

// publicPathAuthCheck runs the auth-check for hiveID with X-Original-URI set
// to a public path, as the caller (or anonymously when username is "").
func publicPathAuthCheck(t *testing.T, s *HubServer, hiveID, uri, username string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if username == "" {
		req = httptest.NewRequest(http.MethodGet, "/api/saas/auth-check?hive="+hiveID, nil)
	} else {
		req = reqWithUser(http.MethodGet, "/api/saas/auth-check?hive="+hiveID, "", username)
	}
	req.Header.Set("X-Original-URI", uri)
	rec := httptest.NewRecorder()
	s.handleSaaSAuthCheck(rec, req)
	return rec
}

func TestSaaSAuthCheckPublicPathIdentifiesSignedInCaller(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	demotedOwnerFixture(t, s, "spoke-owner", "owned-hive")
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "reader", Hives: map[string]string{"owned-hive": "read-write"}}); err != nil {
		t.Fatal(err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "stranger", Hives: map[string]string{"other-hive": "owner"}}); err != nil {
		t.Fatal(err)
	}

	// The owner: identified, elevated to owner exactly as on the gated path.
	rec := publicPathAuthCheck(t, s, "owned-hive", "/api/contribute/me", "spoke-owner")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner on a public path: status = %d, want 200 (a public path never refuses)", rec.Code)
	}
	if got := rec.Header().Get("X-Hive-User"); got != "spoke-owner" {
		t.Errorf("X-Hive-User = %q, want spoke-owner — without it /api/contribute/me answers 401 to the hive's own owner (#7453)", got)
	}
	if got := rec.Header().Get("X-Hive-Role"); got != "owner" {
		t.Errorf("X-Hive-Role = %q, want owner (demoted stored role must not mask the true owner, #4081)", got)
	}

	// A user with a grant: identified with that grant.
	rec = publicPathAuthCheck(t, s, "owned-hive", "/api/contribute/me", "reader")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Hive-User") != "reader" || rec.Header().Get("X-Hive-Role") != "read-write" {
		t.Errorf("granted user: status=%d user=%q role=%q, want 200/reader/read-write", rec.Code, rec.Header().Get("X-Hive-User"), rec.Header().Get("X-Hive-Role"))
	}

	// A signed-in user with NO grant on this hive: still identified (it is
	// their own contribution the page shows), with the guest role that grants
	// nothing beyond anonymous — and still 200, never 403, on a public path.
	rec = publicPathAuthCheck(t, s, "owned-hive", "/api/contribute/leaderboard", "stranger")
	if rec.Code != http.StatusOK {
		t.Fatalf("grant-less user on a public path: status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Hive-User"); got != "stranger" {
		t.Errorf("grant-less user: X-Hive-User = %q, want stranger", got)
	}
	if got := rec.Header().Get("X-Hive-Role"); got != publicPathGuestRole {
		t.Errorf("grant-less user: X-Hive-Role = %q, want %q", got, publicPathGuestRole)
	}
	// The gated path is unchanged for that same user: refused.
	req := reqWithUser(http.MethodGet, "/api/saas/auth-check?hive=owned-hive", "", "stranger")
	req.Header.Set("X-Original-URI", "/api/config/governor")
	gated := httptest.NewRecorder()
	s.handleSaaSAuthCheck(gated, req)
	if gated.Code != http.StatusForbidden {
		t.Errorf("grant-less user on a gated path: status = %d, want 403 unchanged", gated.Code)
	}
}

func TestSaaSAuthCheckPublicPathStaysAnonymousWithoutSession(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	demotedOwnerFixture(t, s, "spoke-owner", "owned-hive")

	for _, uri := range []string{"/api/contribute/me", "/api/contribute/ws", "/contribute/operations", "/api/leaderboard", "/snapshot/x", ssoHandoffPath} {
		rec := publicPathAuthCheck(t, s, "owned-hive", uri, "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s anonymous: status = %d, want 200 — the leaderboard and the contributor relay must stay reachable signed out", uri, rec.Code)
		}
		for _, h := range []string{"X-Hive-User", "X-Hive-Role", proxyAuthHeader} {
			if got := rec.Header().Get(h); got != "" {
				t.Errorf("%s anonymous: %s = %q, want unset — an anonymous public request must carry no identity and no proof", uri, h, got)
			}
		}
	}
}

// TestContributeIngressAsksWhoIsCallingButNeverRedirects pins the
// provisioning half: hive-contribute carries the same auth-url and
// auth-response-headers as the page Ingress (so nginx asks the hub and copies
// the identity onto the request) and no auth-signin (the path is public;
// fetch() could not follow the redirect anyway).
func TestContributeIngressAsksWhoIsCallingButNeverRedirects(t *testing.T) {
	for _, useWildcard := range []bool{false, true} {
		blocks := ingressBlocks(t, renderManifestWildcard(t, useWildcard))
		raw, ok := blocks["hive-contribute"]
		if !ok {
			t.Fatalf("useWildcard=%v: no hive-contribute Ingress", useWildcard)
		}
		var doc struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("hive-contribute does not parse: %v\n%s", err, raw)
		}
		ann := doc.Metadata.Annotations
		authURL := ann["nginx.ingress.kubernetes.io/auth-url"]
		if !strings.Contains(authURL, "/api/saas/auth-check?hive=hosted-hive-x") || !strings.Contains(authURL, "uri=$request_uri") {
			t.Errorf("useWildcard=%v: hive-contribute has no per-hive auth-url, so X-Hive-User never reaches /api/contribute/me (#7453): %q", useWildcard, authURL)
		}
		if ann["nginx.ingress.kubernetes.io/auth-response-headers"] != "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth" {
			t.Errorf("useWildcard=%v: hive-contribute does not copy the identity headers onto the request: %q", useWildcard, ann["nginx.ingress.kubernetes.io/auth-response-headers"])
		}
		if _, has := ann["nginx.ingress.kubernetes.io/auth-signin"]; has {
			t.Errorf("useWildcard=%v: hive-contribute carries auth-signin on a public path", useWildcard)
		}
		if ann["nginx.ingress.kubernetes.io/proxy-read-timeout"] != "3600" {
			t.Errorf("useWildcard=%v: the relay's long-poll timeout was lost", useWildcard)
		}
	}
}
