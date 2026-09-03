package hub

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
	"gopkg.in/yaml.v3"
)

// errorPagesManifestPath is the custom-error backend the hive Ingress points at
// via its default-backend annotation, relative to this package.
const errorPagesManifestPath = "../../deploy/k8s/error-pages.yaml"

// errorPagesConfigMaps returns the data maps of the two ConfigMaps that make up
// the error backend: the documents it serves and the nginx config that serves
// them.
func errorPagesConfigMaps(t *testing.T) (docs, conf map[string]string) {
	t.Helper()
	raw, err := os.ReadFile(errorPagesManifestPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "error-pages.yaml not readable from this package: %v", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var obj struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		err := dec.Decode(&obj)
		if err != nil {
			break
		}
		if obj.Kind != "ConfigMap" {
			continue
		}
		switch obj.Metadata.Name {
		case "hive-error-pages":
			docs = obj.Data
		case "hive-error-pages-nginx":
			conf = obj.Data
		}
	}
	if docs == nil || conf == nil {
		t.Fatalf("error-pages.yaml no longer defines both the hive-error-pages and hive-error-pages-nginx ConfigMaps")
	}
	return docs, conf
}

// stripNginxComments drops whole-line `#` comments so an assertion about what
// the config DOES is never satisfied — or tripped — by prose describing what it
// used to do.
func stripNginxComments(conf string) string {
	var kept []string
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestErrorBackendNeverAnswers200 pins the fix for issue #4241.
//
// The hive Ingress carries custom-http-errors: "502,503" and routes those
// statuses to this backend. ingress-nginx's contract is that the backend
// reproduces the intercepted status, which it passes in X-Code. The backend
// used to ignore X-Code entirely and answer `try_files /error.html =404` — an
// unconditional 200.
//
// The consequence was silent data loss, not a cosmetic one: a bearer-authed
// POST /api/v1/prs/{owner}/{repo}/{n}/queue-automerge for a repository the hive
// MANAGES reached the API, the API answered 502 because it could not resolve
// the PR, the Ingress intercepted that 502, and the caller was handed
// `200 text/html` with nothing in it saying the merge had never been queued. A
// client that checks only the status code reads that as success.
//
// So: the catch-all location must answer with a `return` of a 5xx, and no path
// through it may produce a 200.
func TestErrorBackendNeverAnswers200(t *testing.T) {
	_, conf := errorPagesConfigMaps(t)
	def := conf["default.conf"]
	if def == "" {
		t.Fatal("hive-error-pages-nginx ConfigMap has no default.conf")
	}
	// The file documents the old shape in a comment, so every directive check
	// below runs against the live config only.
	directives := stripNginxComments(def)

	// The old shape, byte for byte the thing that produced the 200.
	if strings.Contains(directives, "try_files /error.html") {
		t.Error("the catch-all still serves error.html with try_files, which answers 200 " +
			"regardless of the intercepted status (issue #4241)")
	}

	// X-Code must actually be consulted, or the status is a guess.
	if !strings.Contains(directives, "$http_x_code") {
		t.Error("default.conf never reads $http_x_code, so it cannot reproduce the " +
			"status ingress-nginx intercepted (issue #4241)")
	}

	// Every status the hive Ingress intercepts must map back onto the response.
	for _, code := range []string{"502", "503"} {
		if !regexp.MustCompile(`return\s+` + code + `\b`).MatchString(directives) {
			t.Errorf("default.conf never returns %s, so an intercepted %s cannot reach the caller", code, code)
		}
	}

	// The only 200 in the file belongs to the kubelet probe. Anything else is a
	// success status on an error path.
	for _, line := range strings.Split(directives, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "return 200") {
			continue
		}
		if !strings.Contains(line, `"ok"`) {
			t.Errorf("default.conf returns 200 outside the healthz probe: %q", line)
		}
	}
}

// TestErrorBackendServesJSONToAPICallers pins the second half of #4241: an API
// caller must never be handed an HTML document. The client that reported the
// issue fails closed on a non-JSON body, so an HTML error page is
// indistinguishable to it from a broken deployment.
func TestErrorBackendServesJSONToAPICallers(t *testing.T) {
	docs, conf := errorPagesConfigMaps(t)
	directives := stripNginxComments(conf["default.conf"])

	// The original URI decides first — an /api/ request is an API request
	// whatever it put in Accept.
	if !strings.Contains(directives, "$http_x_original_uri") || !strings.Contains(directives, `"~^/api/"`) {
		t.Error("default.conf does not select the JSON document for /api/ requests; an API " +
			"caller can still be handed HTML (issue #4241)")
	}
	// Accept is the fallback for every other path.
	if !strings.Contains(directives, "$http_x_format") {
		t.Error("default.conf ignores X-Format, so a JSON client on a non-/api path gets HTML")
	}

	body, ok := docs["error.json"]
	if !ok {
		t.Fatal("hive-error-pages ConfigMap has no error.json for API callers to receive")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("error.json is not valid JSON, so an API caller still cannot parse the failure: %v", err)
	}
	// Clients key off ok:false. An error document that omits it, or claims
	// success, reintroduces exactly the ambiguity this issue is about.
	if okField, present := parsed["ok"]; !present {
		t.Error(`error.json has no "ok" field for clients to fail closed on`)
	} else if okField != false {
		t.Errorf(`error.json reports "ok": %v on an error path; it must be false`, okField)
	}
	if parsed["error"] == nil || parsed["error"] == "" {
		t.Error(`error.json has no "error" message`)
	}
}

// TestErrorBackendKeepsHealthProbe guards the probe the Deployment's liveness
// and readiness checks depend on: it is hit by the kubelet directly, never
// through the Ingress, so it must keep answering 200 even though every other
// response from this backend is now a 5xx.
func TestErrorBackendKeepsHealthProbe(t *testing.T) {
	_, conf := errorPagesConfigMaps(t)
	def := stripNginxComments(conf["default.conf"])
	if !regexp.MustCompile(`location\s+=?\s*/healthz`).MatchString(def) {
		t.Fatal("default.conf lost its /healthz location; the liveness and readiness probes will fail")
	}
	if !strings.Contains(def, `return 200 "ok"`) {
		t.Error("/healthz no longer returns 200; the pod will be restarted in a loop")
	}
}

// TestHiveIngressStillRoutesErrorsToThisBackend ties the two halves together. The
// backend's contract only matters while the Ingress actually delegates to it, and
// conversely the annotations below are only safe because the backend reproduces
// the status. If provisioning stops intercepting, the tests above stop guarding
// anything and should be revisited rather than silently kept green.
func TestHiveIngressStillRoutesErrorsToThisBackend(t *testing.T) {
	if !strings.Contains(k8sManifestTemplate, `nginx.ingress.kubernetes.io/custom-http-errors: "502,503"`) {
		// Fail rather than skip (#5388): k8sManifestTemplate is repo content,
		// identical everywhere, so a skip here could never mean "unsuitable
		// environment" — it means the premise of this whole suite moved and
		// every error-pages assertion above silently stopped guarding anything.
		// If dropping the interception is deliberate, re-review this file's
		// contract in the same PR rather than letting it stay green vacuously.
		t.Fatal("the hive Ingress no longer intercepts 502/503; the error backend contract " +
			"this suite pins needs re-review in the PR that removed the custom-http-errors annotation")
	}
	if !strings.Contains(k8sManifestTemplate, "nginx.ingress.kubernetes.io/default-backend: hive-error-pages") {
		t.Error("custom-http-errors is set without pointing at hive-error-pages, so intercepted " +
			"statuses fall through to the cluster default backend")
	}
}
