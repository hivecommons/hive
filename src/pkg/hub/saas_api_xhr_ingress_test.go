package hub

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests pin the server half of #7405.
//
// Every hosted spoke Ingress carried auth-signin, so an expired hub session
// on an XHR to /api/config/agent/<name> did not come back as a 401 the
// dashboard could read: nginx turned the auth-check's 401 into a 302 to the
// hub login on a different origin, fetch() refused to follow it, and Safari
// surfaced "TypeError: Load failed". The dashboard's `res.status === 401`
// branches never fired, and the config dialog fell back to a fabricated
// all-zero config with Save armed.
//
// The fix is a dedicated Ingress for the dashboard's XHR routes under /api
// that keeps the SAME per-hive auth gate but has no auth-signin, and routes
// the resulting 401 through the error backend so an API caller receives JSON.

// apiXHRIngress returns the parsed hive-api-xhr Ingress from the rendered
// manifest, in whichever TLS mode is asked for.
func apiXHRIngress(t *testing.T, useWildcard bool) (raw string, annotations map[string]string, paths []string) {
	t.Helper()
	blocks := ingressBlocks(t, renderManifestWildcard(t, useWildcard))
	raw, ok := blocks["hive-api-xhr"]
	if !ok {
		t.Fatalf("the manifest has no hive-api-xhr Ingress; the dashboard's /api XHR routes fall "+
			"back to the page Ingress and its auth-signin redirect (#7405). Ingresses found: %v", ingressNamesOf(blocks))
	}
	var doc struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Spec struct {
			Rules []struct {
				HTTP struct {
					Paths []struct {
						Path     string `yaml:"path"`
						PathType string `yaml:"pathType"`
					} `yaml:"paths"`
				} `yaml:"http"`
			} `yaml:"rules"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("hive-api-xhr does not parse: %v\n%s", err, raw)
	}
	for _, r := range doc.Spec.Rules {
		for _, p := range r.HTTP.Paths {
			paths = append(paths, p.PathType+" "+p.Path)
		}
	}
	return raw, doc.Metadata.Annotations, paths
}

func ingressNamesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAPIXHRIngressAnswers401WithoutRedirect is the core assertion: the /api
// prefix is gated by the same auth-check as the page, but a failed check is
// NOT turned into a login redirect.
func TestAPIXHRIngressAnswers401WithoutRedirect(t *testing.T) {
	for _, useWildcard := range []bool{false, true} {
		_, ann, paths := apiXHRIngress(t, useWildcard)

		if len(paths) != 1 || paths[0] != "Prefix /api" {
			t.Errorf("useWildcard=%v: hive-api-xhr routes %v, want exactly [Prefix /api] so every dashboard XHR route is covered", useWildcard, paths)
		}

		authURL := ann["nginx.ingress.kubernetes.io/auth-url"]
		if !strings.Contains(authURL, "/api/saas/auth-check?hive=hosted-hive-x") || !strings.Contains(authURL, "uri=$request_uri") {
			t.Errorf("useWildcard=%v: hive-api-xhr does not carry the per-hive auth-check gate: %q", useWildcard, authURL)
		}
		if _, has := ann["nginx.ingress.kubernetes.io/auth-signin"]; has {
			t.Errorf("useWildcard=%v: hive-api-xhr carries auth-signin, so nginx turns an expired session into a cross-origin 302 that fetch() cannot follow (#7405)", useWildcard)
		}
		if ann["nginx.ingress.kubernetes.io/auth-response-headers"] != "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth" {
			t.Errorf("useWildcard=%v: hive-api-xhr must forward the same identity headers as the page Ingress, got %q", useWildcard, ann["nginx.ingress.kubernetes.io/auth-response-headers"])
		}

		// The 401 must reach the caller as JSON: intercept it into the error
		// backend, which selects unauthorized.json for /api/ URIs.
		codes := strings.Split(ann["nginx.ingress.kubernetes.io/custom-http-errors"], ",")
		has401 := false
		for _, c := range codes {
			if strings.TrimSpace(c) == "401" {
				has401 = true
			}
		}
		if !has401 {
			t.Errorf("useWildcard=%v: hive-api-xhr custom-http-errors=%q does not intercept 401, so an expired session answers nginx's HTML 401 page instead of JSON", useWildcard, ann["nginx.ingress.kubernetes.io/custom-http-errors"])
		}
		if ann["nginx.ingress.kubernetes.io/default-backend"] != "hive-error-pages" {
			t.Errorf("useWildcard=%v: hive-api-xhr intercepts errors without pointing at hive-error-pages", useWildcard)
		}
	}
}

// TestAPIXHRIngressDoesNotUngateSiblings: the page Ingress and the terminal
// Ingress keep their auth-signin (a browser navigation CAN follow it and that
// is how a signed-out user reaches the login page at all), and the bearer-token
// /api/v1 and public /api/contribute Ingresses stay ungated — longest-prefix
// matching keeps them ahead of /api.
func TestAPIXHRIngressDoesNotUngateSiblings(t *testing.T) {
	blocks := ingressBlocks(t, renderManifestWildcard(t, false))
	for _, name := range []string{"hive", "hive-terminal"} {
		if !strings.Contains(blocks[name], "nginx.ingress.kubernetes.io/auth-signin") {
			t.Errorf("%s lost its auth-signin; a signed-out browser navigation would get a bare 401 instead of the login page:\n%s", name, blocks[name])
		}
	}
	for _, name := range []string{"hive-api", "hive-contribute"} {
		if strings.Contains(blocks[name], "auth-url") {
			t.Errorf("%s gained an auth gate; it serves bearer-token/public traffic that has no hub session:\n%s", name, blocks[name])
		}
	}
	if !strings.Contains(blocks["hive-api"], "path: /api/v1") || !strings.Contains(blocks["hive-contribute"], "path: /api/contribute") {
		t.Error("the /api/v1 and /api/contribute paths moved; they must stay longer prefixes than /api so nginx keeps routing them to their ungated Ingresses")
	}
}

// TestVanityHostPatchCoversAPIXHRIngress: a vanity hostname is mirrored onto
// every provisioned Ingress by a JSON patch keyed on name. Leaving the new one
// out would route a vanity host's /api traffic through the page Ingress —
// and its redirect — again.
func TestVanityHostPatchCoversAPIXHRIngress(t *testing.T) {
	b, err := os.ReadFile("saas_provision.go")
	if err != nil {
		t.Fatalf("read saas_provision.go: %v", err)
	}
	src := string(b)
	re := regexp.MustCompile(`for _, ing := range \[\]string\{([^}]*)\}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not find the vanity-host Ingress patch loop in saas_provision.go")
	}
	if !strings.Contains(m[1], `"hive-api-xhr"`) {
		t.Errorf("the vanity-host patch loop does not include hive-api-xhr: %s", m[1])
	}
}

// TestErrorBackendServes401AsJSON pins the error backend's half: an
// intercepted 401 is reproduced as 401 (never 503 "retry shortly", never 200)
// with a document that says "sign in", and its JSON twin is valid and fails
// closed.
func TestErrorBackendServes401AsJSON(t *testing.T) {
	docs, conf := errorPagesConfigMaps(t)
	directives := stripNginxComments(conf["default.conf"])

	if !regexp.MustCompile(`return\s+401\b`).MatchString(directives) {
		t.Error("default.conf never returns 401, so an intercepted 401 is answered with the 503 fallback and the dashboard reads an expired session as an outage (#7405)")
	}
	if !regexp.MustCompile(`error_page\s+401\s+/unauthorized\.\$hive_error_ext`).MatchString(directives) {
		t.Error("default.conf does not map 401 onto the unauthorized.{json,html} documents")
	}
	for _, name := range []string{"unauthorized.json", "unauthorized.html"} {
		if !strings.Contains(directives, "location = /"+name+" { internal; }") {
			t.Errorf("default.conf has no internal location for /%s", name)
		}
	}

	body, ok := docs["unauthorized.json"]
	if !ok {
		t.Fatal("hive-error-pages ConfigMap has no unauthorized.json for API callers to receive on 401")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unauthorized.json is not valid JSON: %v", err)
	}
	if parsed["ok"] != false {
		t.Errorf(`unauthorized.json must report "ok": false, got %v`, parsed["ok"])
	}
	if parsed["error"] != "unauthorized" {
		t.Errorf(`unauthorized.json "error" = %v, want "unauthorized" (the value clients key off)`, parsed["error"])
	}
	if html := docs["unauthorized.html"]; !strings.Contains(html, `href="/"`) {
		t.Error("unauthorized.html does not point a navigating browser back at the dashboard root, where the login redirect still lives")
	}
}
