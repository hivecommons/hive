package hub

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveSessionCookieParentDomain(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		host    string
		want    string
		wantErr bool
	}{
		{name: "unset keeps registrable domain", raw: "", host: "hive.example.org", want: "example.org"},
		{name: "blank keeps registrable domain", raw: "   ", host: "hive.example.org", want: "example.org"},
		{name: "equal to hub host", raw: "hive.example.org", host: "hive.example.org", want: "hive.example.org"},
		{name: "parent of hub host", raw: "hive.example.org", host: "hub.hive.example.org", want: "hive.example.org"},
		{name: "registrable domain", raw: "example.org", host: "hive.example.org", want: "example.org"},
		{name: "leading dot", raw: ".hive.example.org", host: "hive.example.org", want: "hive.example.org"},
		{name: "trailing dot", raw: "hive.example.org.", host: "hive.example.org", want: "hive.example.org"},
		{name: "uppercase", raw: "HIVE.Example.ORG", host: "hive.example.org", want: "hive.example.org"},
		{name: "wider than registrable domain", raw: "org", host: "hive.example.org", want: "hive.example.org", wantErr: true},
		{name: "multi-label public suffix", raw: "co.uk", host: "hive.example.co.uk", want: "hive.example.co.uk", wantErr: true},
		{name: "unrelated domain", raw: "example.com", host: "hive.example.org", want: "hive.example.org", wantErr: true},
		{name: "sibling does not cover hub host", raw: "dibs.example.org", host: "hive.example.org", want: "hive.example.org", wantErr: true},
		{name: "child of hub host", raw: "a.hive.example.org", host: "hive.example.org", want: "hive.example.org", wantErr: true},
		{name: "suffix off a label boundary", raw: "ive.example.org", host: "hive.example.org", want: "hive.example.org", wantErr: true},
		{name: "host without registrable domain", raw: "localhost", host: "localhost", want: "localhost", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveSessionCookieParentDomain(tc.raw, tc.host)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("resolveSessionCookieParentDomain(%q, %q) = %q, %v; want %q, error %v",
					tc.raw, tc.host, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestSessionCookieDomainOverride(t *testing.T) {
	const hub = "hive.example.org"
	cases := []struct {
		name     string
		override string
		host     string
		want     string
	}{
		{name: "unset: hub host", override: "", host: hub, want: ".example.org"},
		{name: "unset: sibling", override: "", host: "dibs.example.org", want: ".example.org"},
		{name: "override: hub host", override: hub, host: hub, want: ".hive.example.org"},
		{name: "override: hub host with port", override: hub, host: hub + ":443", want: ".hive.example.org"},
		{name: "override: spoke", override: hub, host: "abc123." + hub, want: ".hive.example.org"},
		{name: "override: sibling is outside the scope", override: hub, host: "dibs.example.org", want: ""},
		{name: "override: localhost", override: hub, host: "localhost:3001", want: ""},
		{name: "invalid override fails narrow", override: "example.com", host: hub, want: ".hive.example.org"},
		{name: "wider override fails narrow", override: "org", host: "abc123." + hub, want: ".hive.example.org"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", "https://"+hub)
			t.Setenv("HIVE_HUB_SESSION_COOKIE_DOMAIN", tc.override)
			if got := sessionCookieDomain(tc.host); got != tc.want {
				t.Fatalf("sessionCookieDomain(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestSessionCookieReachesAgreesWithCookieDomainMatchesUnderOverride(t *testing.T) {
	const hub = "hive.example.org"
	targets := map[string]bool{
		"https://" + hub + "/dashboard":       true,
		"https://abc123." + hub + "/":         true,
		"https://dibs.example.org/":           false,
		"https://example.org/":                false,
		"https://intranet.example.org/":       false,
		"http://localhost:3001/":              false,
		"https://hive.example.org.evil.test/": false,
	}
	for _, override := range []string{hub, "example.com"} {
		t.Run(override, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", "https://"+hub)
			t.Setenv("HIVE_HUB_SESSION_COOKIE_DOMAIN", override)
			for target, want := range targets {
				host, _ := originHost(target)
				matches := cookieDomainMatches(sessionCookieDomain(hub), hub, strings.ToLower(host))
				reaches := sessionCookieReaches(hub, target)
				if reaches != want || matches != want {
					t.Errorf("target %q: sessionCookieReaches = %v, cookieDomainMatches = %v, want %v",
						target, reaches, matches, want)
				}
			}
		})
	}
}

// Narrowing the scope must also expire the copy minted under the old wide
// scope, or that copy keeps reaching every service on the registrable domain.
func TestSetSessionCookiesExpiresWideCopyWhenOverrideNarrows(t *testing.T) {
	const hub = "hive.example.org"
	cases := []struct {
		name        string
		override    string
		wantLive    string
		wantExpired []string
	}{
		{name: "unset", override: "", wantLive: "example.org", wantExpired: []string{"kubestellar.io"}},
		{name: "narrowed", override: hub, wantLive: hub, wantExpired: []string{"kubestellar.io", "example.org"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HIVE_HUB_PUBLIC_URL", "https://"+hub)
			t.Setenv("HIVE_HUB_LEGACY_COOKIE_DOMAIN", "")
			t.Setenv("HIVE_HUB_SESSION_COOKIE_DOMAIN", tc.override)
			r := httptest.NewRequest(http.MethodGet, "https://"+hub+"/api/auth/user", nil)
			w := httptest.NewRecorder()
			setSessionCookies(w, r, "session-value")

			var live []string
			var expired []string
			for _, c := range w.Result().Cookies() {
				domain := strings.TrimPrefix(c.Domain, ".")
				if c.MaxAge < 0 {
					expired = append(expired, domain)
				} else if c.Value == "session-value" {
					live = append(live, domain)
				}
			}
			if len(live) != 1 || live[0] != tc.wantLive {
				t.Errorf("live cookie domains = %v, want [%s]", live, tc.wantLive)
			}
			if strings.Join(expired, ",") != strings.Join(tc.wantExpired, ",") {
				t.Errorf("expired cookie domains = %v, want %v", expired, tc.wantExpired)
			}
		})
	}
}

func TestSessionCookieExcludesSpokes(t *testing.T) {
	cases := []struct {
		name     string
		override string
		spoke    string
		want     bool
	}{
		{name: "equal", override: "hive.example.org", spoke: "hive.example.org", want: false},
		{name: "spoke under override", override: "example.org", spoke: "hive.example.org", want: false},
		{name: "unrelated", override: "hive.example.org", spoke: "spokes.example.com", want: true},
		{name: "sibling under same parent", override: "hub.example.org", spoke: "spokes.example.org", want: true},
		{name: "suffix off a label boundary", override: "ive.example.org", spoke: "hive.example.org", want: true},
		{name: "spoke is parent of override", override: "hive.example.org", spoke: "example.org", want: true},
		{name: "case differs", override: "HIVE.Example.ORG", spoke: "hive.example.org", want: false},
		{name: "trailing dots", override: "hive.example.org.", spoke: "x.hive.example.org.", want: false},
		{name: "leading dot", override: ".example.org", spoke: "hive.example.org", want: false},
		{name: "override empty", override: "", spoke: "hive.example.org", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionCookieExcludesSpokes(tc.override, tc.spoke); got != tc.want {
				t.Fatalf("sessionCookieExcludesSpokes(%q, %q) = %v, want %v", tc.override, tc.spoke, got, tc.want)
			}
		})
	}
}

func TestSessionCookieParentDomainWarnsOncePerPairWhenSpokesExcluded(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	spokeExclusionLogged.Clear()

	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hive.example.org")
	t.Setenv("HIVE_HUB_SESSION_COOKIE_DOMAIN", "hive.example.org")
	t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "spokes.example.org")
	for range 3 {
		if got := sessionCookieParentDomain(); got != "hive.example.org" {
			t.Fatalf("cookie domain = %q, want hive.example.org", got)
		}
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("warnings = %d, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "spokes.example.org") || !strings.Contains(buf.String(), "hive.example.org") {
		t.Fatalf("warning must name both values:\n%s", buf.String())
	}

	buf.Reset()
	t.Setenv("HIVE_HUB_SPOKE_DOMAIN", "x.hive.example.org")
	sessionCookieParentDomain()
	if buf.Len() != 0 {
		t.Fatalf("unexpected log when spokes are covered:\n%s", buf.String())
	}
}
