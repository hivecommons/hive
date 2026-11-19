package hub

import (
	_ "embed"
	"io"
	"net/http"
	"strings"
)

//go:embed assets/profile-share.html
var profileSharePageTemplate string

var profileSharePageHTML = strings.Replace(profileSharePageTemplate, "{{CONSENT_TEXT_VERSION}}", customerConsentTextVersion, 1)

func (s *HubServer) handleProfileSharePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	_, _ = io.WriteString(w, profileSharePageHTML)
}
