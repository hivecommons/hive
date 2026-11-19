package knowledge

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func redirectReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := &http.Request{URL: u, Header: http.Header{}}
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Cookie", "s=1")
	r.Header.Set("X-IBM-Project-ID", "p")
	r.Header.Set("Accept", "application/json")
	return r
}

func TestStripCredentialsOnCrossHostRedirect(t *testing.T) {
	cases := []struct {
		name, from, to string
		wantStripped   bool
	}{
		{"same host", "https://wiki.example/a", "https://wiki.example/b", false},
		{"same host other case", "https://wiki.example/a", "https://WIKI.example/b", false},
		{"other host", "https://wiki.example/a", "https://other.example/b", true},
		{"subdomain", "https://wiki.example/a", "https://evil.wiki.example/b", true},
		{"other port", "https://wiki.example/a", "https://wiki.example:8443/b", true},
		{"https downgrade", "https://wiki.example/a", "http://wiki.example/b", true},
		{"http upgrade", "http://wiki.example/a", "https://wiki.example/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := redirectReq(t, tc.to)
			StripCredentialsOnCrossHostRedirect(req, []*http.Request{redirectReq(t, tc.from)})
			for _, h := range []string{"Authorization", "Cookie", "X-IBM-Project-ID"} {
				if got := req.Header.Get(h) == ""; got != tc.wantStripped {
					t.Errorf("%s stripped = %v, want %v", h, got, tc.wantStripped)
				}
			}
			if req.Header.Get("Accept") == "" {
				t.Error("non-credential header was stripped")
			}
		})
	}
}

func TestStripCredentialsOnCrossHostRedirectNoVia(t *testing.T) {
	req := redirectReq(t, "https://other.example/")
	StripCredentialsOnCrossHostRedirect(req, nil)
	StripCredentialsOnCrossHostRedirect(req, []*http.Request{nil})
	if req.Header.Get("Authorization") == "" {
		t.Fatal("credentials stripped without a prior hop to compare against")
	}
}

// The knowledge clients that send credentials (llm-wiki, Context7 and the
// connector HTTP client via CheckRedirectNoPrivate) strip them off-host while
// keeping the private-host and hop-limit checks.
func TestKnowledgeRedirectPolicyStripsCredentialsOffHost(t *testing.T) {
	stubDocRedirectResolver(t, map[string][]string{
		"wiki.example":     {"93.184.216.34"},
		"other.example":    {"93.184.216.35"},
		"internal.example": {"10.0.0.7"},
	})
	via := []*http.Request{redirectReq(t, "https://wiki.example/a")}

	for name, check := range map[string]func(*http.Request, []*http.Request) error{
		"knowledgeRedirectPolicy": knowledgeRedirectPolicy,
		"CheckRedirectNoPrivate":  CheckRedirectNoPrivate,
	} {
		t.Run(name, func(t *testing.T) {
			off := redirectReq(t, "https://other.example/b")
			if err := check(off, via); err != nil {
				t.Fatal(err)
			}
			if off.Header.Get("Authorization") != "" {
				t.Error("off-host hop kept Authorization")
			}
			same := redirectReq(t, "https://wiki.example/b")
			if err := check(same, via); err != nil {
				t.Fatal(err)
			}
			if same.Header.Get("Authorization") == "" {
				t.Error("same-host hop lost Authorization")
			}
			if err := check(redirectReq(t, "https://internal.example/"), via); err == nil {
				t.Error("redirect to a private host was followed")
			}
		})
	}
}

func TestKnowledgeClientsUseCredentialStrippingPolicy(t *testing.T) {
	want := reflect.ValueOf(knowledgeRedirectPolicy).Pointer()
	if got := reflect.ValueOf(context7Client.CheckRedirect).Pointer(); got != want {
		t.Error("context7Client does not use knowledgeRedirectPolicy")
	}
	c := NewClient("https://wiki.example", nil)
	if got := reflect.ValueOf(c.httpClient.CheckRedirect).Pointer(); got != want {
		t.Error("NewClient does not use knowledgeRedirectPolicy")
	}
}
