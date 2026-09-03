package hub

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
)

func TestHandleProviderLoginPreservesProviderAndRedirectInState(t *testing.T) {
	s := &HubServer{authProviders: auth.NewRegistry(&auth.Provider{
		Name:         "github",
		DisplayName:  "GitHub",
		AuthorizeURL: "https://github.example.test/authorize",
		ClientID:     "client-id",
	})}
	req := httptest.NewRequest(http.MethodGet, "/login/GitHub?rd=%2Fhives%2Fone", nil)
	req.SetPathValue("provider", "GitHub")
	rec := httptest.NewRecorder()

	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect location: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("provider redirect is missing state")
	}
	decoded, err := url.QueryUnescape(state)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !strings.Contains(decoded, oauthStateSeparator+"github"+oauthStateSeparator+"/hives/one") {
		t.Fatalf("state = %q, want provider and redirect target", decoded)
	}
}

func TestWriteProviderPickerWithoutRedirect(t *testing.T) {
	rec := httptest.NewRecorder()
	(&HubServer{}).writeProviderPicker(rec, []*auth.Provider{
		{Name: "github", DisplayName: "GitHub"},
	}, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if strings.Contains(body, "?redirect=") {
		t.Fatalf("picker added an empty redirect query: %q", body)
	}
	if !strings.Contains(body, `href="/login/github"`) {
		t.Fatalf("picker missing provider link: %q", body)
	}
}

func TestWriteProviderPickerEscapesProviderFields(t *testing.T) {
	name := `custom\" onclick=\"alert(1)`
	display := `<Custom & SSO>`
	rec := httptest.NewRecorder()
	(&HubServer{}).writeProviderPicker(rec, []*auth.Provider{{Name: name, DisplayName: display}}, "/dashboard")

	body := rec.Body.String()
	if !strings.Contains(body, html.EscapeString(name)) {
		t.Fatalf("picker did not HTML-escape provider name: %q", body)
	}
	if !strings.Contains(body, html.EscapeString(display)) {
		t.Fatalf("picker did not HTML-escape provider label: %q", body)
	}
	if strings.Contains(body, `onclick="alert(1)`) {
		t.Fatalf("picker emitted an executable attribute: %q", body)
	}
}

func TestProviderGlyphAllKnownProviders(t *testing.T) {
	cases := []struct {
		name   string
		marker string
	}{
		{name: "github", marker: "viewBox=\"0 0 16 16\""},
		{name: "google", marker: "#4285F4"},
		{name: "ibmid", marker: "viewBox=\"0 0 512 205\""},
		{name: "redhat", marker: "viewBox=\"0 0 24 24\""},
		{name: "microsoft", marker: "#F25022"},
		{name: "custom", marker: "M14 6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			glyph := providerGlyph(tc.name)
			if !strings.Contains(glyph, "<svg") {
				t.Fatalf("providerGlyph(%q) = %q, want inline SVG", tc.name, glyph)
			}
			if !strings.Contains(glyph, tc.marker) {
				t.Fatalf("providerGlyph(%q) = %q, missing marker %q", tc.name, glyph, tc.marker)
			}
		})
	}
}
