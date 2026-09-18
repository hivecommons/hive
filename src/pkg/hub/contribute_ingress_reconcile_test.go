package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#7517: #7457 put auth-url / auth-response-headers on the
// hive-contribute Ingress in the provisioning template, but the template is
// applied only at provision time, so every hosted spoke that already existed
// kept an Ingress with no auth-url and /api/contribute/me kept answering 401.
// These tests pin the reconcile that re-applies the two annotations: what
// "converged" means (the template's own values), what the sweep patches (only
// the drifted keys, by merge patch), what it leaves alone, and that the
// poller actually runs it.

// The reconcile's idea of the annotations MUST be the template's: if they
// ever disagree, a spoke the sweep has "converged" is still not what a freshly
// provisioned one looks like, and the 401 comes back on a hub URL change or a
// header rename nobody remembered to mirror. Render the template with the same
// two inputs and compare byte for byte.
func TestContributeIngressReconcileMatchesTheTemplate(t *testing.T) {
	for _, useWildcard := range []bool{false, true} {
		blocks := ingressBlocks(t, renderManifestWildcard(t, useWildcard))
		raw, ok := blocks[contributeIngressName]
		if !ok {
			t.Fatalf("useWildcard=%v: no %s Ingress in the template", useWildcard, contributeIngressName)
		}
		var doc struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatalf("%s does not parse: %v\n%s", contributeIngressName, err, raw)
		}
		// renderManifestWildcard renders ID hosted-hive-x against
		// https://hive.hivecommons.dev.
		want := contributeIngressAuthAnnotations("https://hive.hivecommons.dev", "hosted-hive-x")
		for key, value := range want {
			if got := doc.Metadata.Annotations[key]; got != value {
				t.Errorf("useWildcard=%v: reconcile expects %s = %q but the template renders %q — a converged spoke would not match a provisioned one",
					useWildcard, key, value, got)
			}
		}
	}
}

// liveIngressJSON is a hive-contribute Ingress as `kubectl get -o json` prints
// it, with whatever annotations a test wants on it. The rest of the object is
// the shape provisionHive leaves behind, so the parse is exercised on a real
// document rather than a bare annotations map.
func liveIngressJSON(t *testing.T, annotations map[string]string) []byte {
	t.Helper()
	obj := map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"name":        contributeIngressName,
			"namespace":   "hive-hosted-hosted-projectbluefin-common-nmq5",
			"annotations": annotations,
		},
		"spec": map[string]any{
			"ingressClassName": "nginx",
			"rules": []any{map[string]any{
				"host": "hosted-projectbluefin-common-nmq5.hive.hivecommons.dev",
			}},
		},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodePatch(t *testing.T, patch string) map[string]string {
	t.Helper()
	var body struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(patch), &body); err != nil {
		t.Fatalf("patch %q is not a merge patch on metadata.annotations: %v", patch, err)
	}
	return body.Metadata.Annotations
}

// The issue's spoke: provisioned before #7457, so the Ingress carries the
// cert-manager issuer and the proxy timeouts but no auth-url. Both annotations
// go on, and ONLY those two — the merge patch must not touch what is there.
func TestContributeIngressPatchAddsTheMissingAnnotations(t *testing.T) {
	want := contributeIngressAuthAnnotations("https://hive.hivecommons.dev", "hosted-projectbluefin-common-nmq5")
	pre7457 := map[string]string{
		"cert-manager.io/cluster-issuer":                 "letsencrypt-dns01",
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
		"nginx.ingress.kubernetes.io/proxy-send-timeout": "3600",
	}
	patch, err := contributeIngressAnnotationPatch(liveIngressJSON(t, pre7457), want)
	if err != nil {
		t.Fatal(err)
	}
	if patch == "" {
		t.Fatal("a pre-#7457 Ingress with no auth-url was judged converged — this is the production bug: /api/contribute/me stays 401")
	}
	got := decodePatch(t, patch)
	if len(got) != 2 {
		t.Errorf("patch touches %d annotations, want exactly the two #7457 added: %v", len(got), got)
	}
	if got[ingressAuthURLAnnotation] != "https://hive.hivecommons.dev/api/saas/auth-check?hive=hosted-projectbluefin-common-nmq5&uri=$request_uri" {
		t.Errorf("auth-url = %q", got[ingressAuthURLAnnotation])
	}
	if got[ingressAuthResponseHeadersAnnotation] != "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth" {
		t.Errorf("auth-response-headers = %q", got[ingressAuthResponseHeadersAnnotation])
	}
	for key := range pre7457 {
		if _, touched := got[key]; touched {
			t.Errorf("patch rewrites %s, which was not drifted", key)
		}
	}
	// No auth-signin: /api/contribute is public and fetch() could not follow
	// the redirect anyway (#7457). A reconcile that added one would turn an
	// anonymous leaderboard load into a cross-origin bounce.
	if _, has := got["nginx.ingress.kubernetes.io/auth-signin"]; has {
		t.Error("patch adds auth-signin to a public path")
	}
	// The value nginx needs is the literal $request_uri, not a shell- or
	// template-expanded one.
	if !strings.Contains(patch, `$request_uri`) {
		t.Errorf("patch lost the literal $request_uri: %s", patch)
	}
}

// A spoke provisioned after #7457 (or one this sweep already fixed) is a
// no-op: no patch body, so the sweep never issues a pointless kubectl patch
// and can run every 15 minutes forever without touching a converged fleet.
func TestContributeIngressPatchIsEmptyWhenConverged(t *testing.T) {
	want := contributeIngressAuthAnnotations("https://hive.hivecommons.dev", "hosted-hive-x")
	live := map[string]string{
		"cert-manager.io/cluster-issuer":                 "letsencrypt-dns01",
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
	}
	for key, value := range want {
		live[key] = value
	}
	patch, err := contributeIngressAnnotationPatch(liveIngressJSON(t, live), want)
	if err != nil {
		t.Fatal(err)
	}
	if patch != "" {
		t.Errorf("a converged Ingress produced a patch: %s", patch)
	}
}

// Only the drifted key goes in the patch. A spoke whose auth-url points at a
// previous hub URL (or a hand-edited one) is corrected; the headers it already
// carries correctly are left out of the body.
func TestContributeIngressPatchReplacesOnlyTheStaleValue(t *testing.T) {
	want := contributeIngressAuthAnnotations("https://hive.hivecommons.dev", "hosted-hive-x")
	live := map[string]string{
		ingressAuthURLAnnotation:             "https://hive.kubestellar.io/api/saas/auth-check?hive=hosted-hive-x&uri=$request_uri",
		ingressAuthResponseHeadersAnnotation: ingressAuthResponseHeaders,
	}
	patch, err := contributeIngressAnnotationPatch(liveIngressJSON(t, live), want)
	if err != nil {
		t.Fatal(err)
	}
	got := decodePatch(t, patch)
	if len(got) != 1 || got[ingressAuthURLAnnotation] != want[ingressAuthURLAnnotation] {
		t.Errorf("patch = %v, want only the corrected auth-url", got)
	}
}

// An Ingress with no annotations at all (kubectl prints no `annotations` key)
// still gets both — the absent map must read as "everything missing", not as
// a parse failure.
func TestContributeIngressPatchHandlesNoAnnotationsAtAll(t *testing.T) {
	want := contributeIngressAuthAnnotations("https://hive.hivecommons.dev", "hosted-hive-x")
	patch, err := contributeIngressAnnotationPatch([]byte(`{"metadata":{"name":"hive-contribute"}}`), want)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodePatch(t, patch); len(got) != 2 {
		t.Errorf("patch = %v, want both annotations", got)
	}
}

// Unparseable output is an error, never a patch: the sweep must not patch
// blind onto an object it could not read.
func TestContributeIngressPatchRefusesToPatchBlind(t *testing.T) {
	want := contributeIngressAuthAnnotations("https://hive.hivecommons.dev", "hosted-hive-x")
	for _, raw := range []string{"", "not json", "Error from server (NotFound): ingresses.networking.k8s.io \"hive-contribute\" not found"} {
		patch, err := contributeIngressAnnotationPatch([]byte(raw), want)
		if err == nil || patch != "" {
			t.Errorf("raw %q: patch=%q err=%v, want an error and no patch", raw, patch, err)
		}
	}
}

// The values are per hive and per hub: two hives never share an auth-url
// (the hub scopes the auth-check by ?hive=), and the hub URL is whatever
// hubPublicURL() says at sweep time, not a constant.
func TestContributeIngressAuthAnnotationsArePerHive(t *testing.T) {
	a := contributeIngressAuthAnnotations("https://hub.example", "hive-a")
	b := contributeIngressAuthAnnotations("https://hub.example", "hive-b")
	if a[ingressAuthURLAnnotation] == b[ingressAuthURLAnnotation] {
		t.Error("two hives share an auth-url; the hub could not tell whose grant to check")
	}
	if !strings.HasPrefix(a[ingressAuthURLAnnotation], "https://hub.example/api/saas/auth-check?hive=hive-a&") {
		t.Errorf("auth-url = %q", a[ingressAuthURLAnnotation])
	}
}

// Same predicate shape as netAdminSweepEligible, for the same reason: the
// steady-state fleet carries "" and "available", not "running", and a
// placeholder must be right before it is claimed.
func TestContributeIngressSweepEligible(t *testing.T) {
	for _, status := range []string{"", "available", "assigned", "running", "error", " available "} {
		if !contributeIngressSweepEligible(status) {
			t.Errorf("status %q is not swept, but live spokes carry it", status)
		}
	}
	if contributeIngressSweepEligible("provisioning") {
		t.Error("a hive still provisioning was selected; its Ingress is being applied from the template right now")
	}
}

// The poller-loop throttle lets the sweep run once per interval. Driven on
// the timestamp directly, as the NET_ADMIN throttle test is, rather than
// running the kubectl-shelling body.
func TestContributeIngressReconcileThrottle(t *testing.T) {
	s := &HubServer{clusterUnreachableUntil: map[string]time.Time{}}
	due := func() bool {
		s.clusterUnreachableMu.Lock()
		defer s.clusterUnreachableMu.Unlock()
		d := s.lastContributeIngressReconcile.IsZero() ||
			time.Since(s.lastContributeIngressReconcile) >= contributeIngressReconcileInterval
		if d {
			s.lastContributeIngressReconcile = time.Now()
		}
		return d
	}
	if !due() {
		t.Fatal("first call must be due")
	}
	if due() {
		t.Fatal("second call inside the interval must not be due")
	}
	s.lastContributeIngressReconcile = time.Now().Add(-contributeIngressReconcileInterval - time.Second)
	if !due() {
		t.Fatal("a call after the interval must be due again")
	}
}

// The sweep exists only if the poller calls it. A reconcile that is written,
// tested and never wired is the NET_ADMIN story again (#2674), so pin the
// call site in saas_sha_poller.go next to its siblings.
func TestContributeIngressReconcileIsWiredIntoThePoller(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("saas_sha_poller.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "s.reconcileContributeIngressIfDue()") {
		t.Fatal("saas_sha_poller.go never calls reconcileContributeIngressIfDue(); the hive-contribute auth-url drift is never repaired")
	}
}
