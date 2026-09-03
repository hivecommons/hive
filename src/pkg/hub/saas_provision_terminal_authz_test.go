package hub

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// renderManifestForTest renders k8sManifestTemplate with the minimal data set
// the ingress blocks need. It mirrors the real data map in provisionHive but
// keeps only the keys the ingress/route sections reference, so the test pins
// template output without pulling in cluster/OCI plumbing.
func renderManifestForTest(t *testing.T, nginx bool) string {
	t.Helper()
	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template parse: %v", err)
	}
	data := map[string]interface{}{
		"ID":               "hosted-hive-x",
		"HubPublicURL":     "https://hive.kubestellar.io",
		"Namespace":        "hive-hosted-hosted-hive-x",
		"DashboardHost":    "hosted-hive-x.hive.kubestellar.io",
		"DashboardPort":    8080,
		"TerminalPort":     7681,
		"CertIssuer":       "letsencrypt",
		"IngressClass":     "nginx",
		"IsNginxIngress":   nginx,
		"IsOpenShiftRoute": !nginx,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("template exec: %v", err)
	}
	return buf.String()
}

// docBlocks splits a multi-document YAML string into its individual documents.
func docBlocks(manifest string) []string {
	return strings.Split(manifest, "\n---\n")
}

// terminalIngressBlock returns the nginx Ingress document named "hive-terminal"
// (NOT the OpenShift Route of the same name, which is guarded by kind:).
func terminalIngressBlock(t *testing.T, manifest string) string {
	t.Helper()
	for _, block := range docBlocks(manifest) {
		if strings.Contains(block, "kind: Ingress") &&
			strings.Contains(block, "name: hive-terminal") {
			return block
		}
	}
	t.Fatalf("no nginx Ingress named hive-terminal found in rendered manifest")
	return ""
}

// TestTerminalIngressHasPerHiveAuthCheck is the finding C3 template-level guard:
// the hive-terminal nginx Ingress MUST carry the same per-hive auth-url the main
// "hive" Ingress does, scoped to THIS hive's ID. Without it, /terminal is
// authenticated hub-wide but authorized for no specific hive (CWE-862): any hub
// user reaches any tenant's shell.
func TestTerminalIngressHasPerHiveAuthCheck(t *testing.T) {
	manifest := renderManifestForTest(t, true /* nginx */)
	block := terminalIngressBlock(t, manifest)

	wantAuthURL := `nginx.ingress.kubernetes.io/auth-url: "https://hive.kubestellar.io/api/saas/auth-check?hive=hosted-hive-x&uri=$request_uri"`
	if !strings.Contains(block, wantAuthURL) {
		t.Errorf("hive-terminal ingress missing per-hive auth-url annotation.\nwant substring:\n  %s\ngot block:\n%s", wantAuthURL, block)
	}

	// The auth-signin (login redirect) and the response-header passthrough must
	// also be present so an unauthenticated caller is redirected to login and the
	// spoke receives the resolved user/role/proxy-auth on success — matching the
	// main ingress.
	for _, want := range []string{
		`nginx.ingress.kubernetes.io/auth-signin: "https://hive.kubestellar.io/login?redirect=$scheme://$http_host$request_uri"`,
		`nginx.ingress.kubernetes.io/auth-response-headers: "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("hive-terminal ingress missing annotation:\n  %s", want)
		}
	}
}

// TestMainAndTerminalIngressAuthURLMatch pins that the terminal ingress uses the
// SAME hive-scoped auth-url as the main ingress (both key off {{.ID}}), so the
// two can never drift into authorizing different hives.
func TestMainAndTerminalIngressAuthURLMatch(t *testing.T) {
	manifest := renderManifestForTest(t, true /* nginx */)

	authURL := `nginx.ingress.kubernetes.io/auth-url: "https://hive.kubestellar.io/api/saas/auth-check?hive=hosted-hive-x&uri=$request_uri"`
	// It must appear at least twice: once on the main "hive" ingress, once on
	// "hive-terminal".
	if got := strings.Count(manifest, authURL); got < 2 {
		t.Errorf("expected per-hive auth-url on BOTH main and terminal ingress (>=2 occurrences), got %d", got)
	}
}

// TestIngressAuthzEnvIsPlatformAware pins the HIVE_INGRESS_AUTHZ env flag that
// lets the Node proxy fail CLOSED on an empty allowlist WITHOUT breaking nginx:
//   - nginx lane: the flag is set ("true") because an ingress auth-proxy gates
//     /terminal, so the proxy may defer on an empty allowlist.
//   - OpenShift-Route lane: NO ingress auth-proxy, so the flag is absent and the
//     proxy is the only gate → empty allowlist fails closed. (finding C3)
func TestIngressAuthzEnvIsPlatformAware(t *testing.T) {
	const flag = "name: HIVE_INGRESS_AUTHZ"

	nginx := renderManifestForTest(t, true /* nginx */)
	if !strings.Contains(nginx, flag) {
		t.Errorf("nginx lane must set HIVE_INGRESS_AUTHZ so the proxy defers to the ingress on an empty allowlist")
	}

	openshift := renderManifestForTest(t, false /* OpenShift Route */)
	if strings.Contains(openshift, flag) {
		t.Errorf("OpenShift-Route lane must NOT set HIVE_INGRESS_AUTHZ — the proxy is the only per-hive gate and must fail closed on an empty allowlist")
	}
}
