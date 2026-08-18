package resolve

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPResolver_Gated: disabled by default -> factory refuses -> literal.
func TestHTTPResolver_Gated(t *testing.T) {
	specs := []VarSpec{{Name: "V", Type: "http", URL: "http://example.com/x", Scope: ScopeConfig}}
	reg := Build(specs, Policy{AllowHTTP: false}, nil)
	if got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil); got != "${V}" {
		t.Errorf("disabled http should leave literal, got %q", got)
	}
}

// TestHTTPResolver_RequiresAllowlist: enabled but no allowlist -> refused.
func TestHTTPResolver_RequiresAllowlist(t *testing.T) {
	specs := []VarSpec{{Name: "V", Type: "http", URL: "http://example.com/x", Scope: ScopeConfig}}
	reg := Build(specs, Policy{AllowHTTP: true}, nil)
	if got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil); got != "${V}" {
		t.Errorf("http with empty allowlist should leave literal, got %q", got)
	}
}

// TestHTTPResolver_HostNotAllowlisted: host absent from allowlist -> refused.
func TestHTTPResolver_HostNotAllowlisted(t *testing.T) {
	specs := []VarSpec{{Name: "V", Type: "http", URL: "http://evil.example.com/x", Scope: ScopeConfig}}
	reg := Build(specs, Policy{AllowHTTP: true, HTTPAllowlist: []string{"good.example.com"}}, nil)
	if got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil); got != "${V}" {
		t.Errorf("non-allowlisted host should leave literal, got %q", got)
	}
}

// TestHTTPResolver_SSRFGuardBlocksLoopback: httptest binds 127.0.0.1, which the
// SSRF dial guard blocks even when the host is allowlisted — the request never
// reaches the server, and the variable is left literal.
func TestHTTPResolver_SSRFGuardBlocksLoopback(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Write([]byte("value"))
	}))
	defer srv.Close()
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	specs := []VarSpec{{Name: "V", Type: "http", URL: srv.URL + "/x", Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowHTTP: true, HTTPAllowlist: []string{host}, HTTPTimeoutS: 5}, nil)
	if got := reg.Expand(context.Background(), "${V}", ScopeTemplate, nil); got != "${V}" {
		t.Errorf("loopback must be blocked by SSRF guard, got %q", got)
	}
	if reached {
		t.Error("request reached the server despite the SSRF guard")
	}
}

// TestHTTPResolver_HappyPath: with the private-dial guard disabled (test only),
// an allowlisted server returns a body and the header ${ENV} is expanded and
// forwarded; the body is trimmed.
func TestHTTPResolver_HappyPath(t *testing.T) {
	allowPrivateDial = true
	t.Cleanup(func() { allowPrivateDial = false })
	t.Setenv("HIVE_TEST_TOKEN", "sekret")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("the-value\n"))
	}))
	defer srv.Close()

	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	specs := []VarSpec{{
		Name: "V", Type: "http", URL: srv.URL + "/config",
		Headers: map[string]string{"Authorization": "Bearer ${HIVE_TEST_TOKEN}"},
		Scope:   ScopeTemplate,
	}}
	reg := Build(specs, Policy{AllowHTTP: true, HTTPAllowlist: []string{host}, HTTPTimeoutS: 5}, nil)
	if got := reg.Expand(context.Background(), "[${V}]", ScopeTemplate, nil); got != "[the-value]" {
		t.Errorf("got %q want [the-value]", got)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("header ${ENV} not expanded/forwarded: got %q", gotAuth)
	}
}

// TestHTTPResolver_SizeCap: a body larger than the cap is truncated to the cap.
func TestHTTPResolver_SizeCap(t *testing.T) {
	allowPrivateDial = true
	t.Cleanup(func() { allowPrivateDial = false })
	big := strings.Repeat("a", maxHTTPResponseBytes+1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	specs := []VarSpec{{Name: "V", Type: "http", URL: srv.URL, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowHTTP: true, HTTPAllowlist: []string{host}, HTTPTimeoutS: 5}, nil)
	got := reg.Expand(context.Background(), "${V}", ScopeTemplate, nil)
	if len(got) != maxHTTPResponseBytes {
		t.Errorf("response should be capped at %d bytes, got %d", maxHTTPResponseBytes, len(got))
	}
}

// TestHTTPResolver_PrivateIPBlocked confirms the disallowed-IP classifier.
func TestHTTPResolver_PrivateIPBlocked(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":   true,  // loopback
		"10.0.0.1":    true,  // RFC1918
		"192.168.1.1": true,  // RFC1918
		"172.16.0.1":  true,  // RFC1918
		"169.254.0.1": true,  // link-local
		"::1":         true,  // loopback v6
		"fc00::1":     true,  // ULA
		"8.8.8.8":     false, // public
		"1.1.1.1":     false, // public
	}
	for ipStr, want := range cases {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Fatalf("bad test IP %q", ipStr)
		}
		if got := isDisallowedIP(ip); got != want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", ipStr, got, want)
		}
	}
}

// TestExpandEnvOnly covers header ${ENV} expansion (set -> value, unset -> literal).
func TestExpandEnvOnly(t *testing.T) {
	t.Setenv("HIVE_HDR", "abc")
	if got := expandEnvOnly("Bearer ${HIVE_HDR}"); got != "Bearer abc" {
		t.Errorf("got %q", got)
	}
	if got := expandEnvOnly("x ${HIVE_UNSET_HDR} y"); got != "x ${HIVE_UNSET_HDR} y" {
		t.Errorf("unset header var should stay literal, got %q", got)
	}
}
