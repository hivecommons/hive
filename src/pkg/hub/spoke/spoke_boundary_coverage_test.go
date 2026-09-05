package spoke

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestHelpersAndSelfImageReleaseChannel(t *testing.T) {
	if itoa(503) != "503" {
		t.Fatal("itoa mismatch")
	}
	for _, tc := range []struct {
		ref  string
		want string
	}{
		{"", ""},
		{"nginx", ""},
		{"registry:5000/ns/img:stable@sha256:abc", "stable"},
		{"registry:5000/ns/img@sha256:abc", ""},
	} {
		if got := imageTagOf(tc.ref); got != tc.want {
			t.Fatalf("imageTagOf(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
	if got := HashDashboardToken("token"); len(got) != 64 || got == HashDashboardToken("other") {
		t.Fatalf("unexpected token hash %q", got)
	}

	for _, tc := range []struct {
		image string
		want  bool
	}{
		{"", false},
		{"repo/hive@sha256:abc", false},
		{"registry:5000/repo/hive", false},
		{"repo/hive:stable", true},
		{"repo/hive:v5-latest", true},
		{"repo/hive:c11643a", false},
	} {
		if got := imageTagIsMutable(tc.image); got != tc.want {
			t.Fatalf("imageTagIsMutable(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}

	selfImageMu.Lock()
	oldAttempted, oldCached, oldFetched := selfImageAttempted, selfImageCached, selfImageFetched
	selfImageAttempted, selfImageCached, selfImageFetched = true, "repo/hive:stable", time.Now()
	selfImageMu.Unlock()
	t.Cleanup(func() {
		selfImageMu.Lock()
		selfImageAttempted, selfImageCached, selfImageFetched = oldAttempted, oldCached, oldFetched
		selfImageMu.Unlock()
	})
	if got := SelfImageReleaseChannel(); got != "stable" {
		t.Fatalf("SelfImageReleaseChannel = %q, want stable", got)
	}
	selfImageMu.Lock()
	selfImageCached = "repo/hive:v5"
	selfImageMu.Unlock()
	if got := SelfImageReleaseChannel(); got != "" {
		t.Fatalf("SelfImageReleaseChannel non-channel = %q", got)
	}
}

func TestSpokeKeyResolutionAndRotation(t *testing.T) {
	master := "master-secret"
	seed := SSOSigningSeedFromMaster(master)
	pub := ssoPublicKeyFromSeed(seed)
	if pub == "" {
		t.Fatal("expected public key from derived seed")
	}
	if ssoPublicKeyFromSeed("bad") != "" {
		t.Fatal("invalid seed produced public key")
	}
	t.Setenv("HIVE_HUB_SECRET", master)
	t.Setenv(EnvHiveID, "hive-a")
	if got := SpokeSSOPublicKey(); got != pub {
		t.Fatalf("SpokeSSOPublicKey derived = %q, want %q", got, pub)
	}
	_, explicitPriv, _ := ed25519.GenerateKey(strings.NewReader(strings.Repeat("a", 64)))
	explicitPub := hex.EncodeToString(explicitPriv.Public().(ed25519.PublicKey))
	t.Setenv(EnvSSOPublicKey, " "+explicitPub+" ")
	t.Setenv(EnvSSOPublicKeyPrevious, explicitPub)
	if got := SpokeSSOPublicKey(); got != explicitPub {
		t.Fatalf("SpokeSSOPublicKey explicit = %q, want %q", got, explicitPub)
	}
	if got := SpokeSSOPublicKeys(); len(got) != 1 || got[0] != explicitPub {
		t.Fatalf("SpokeSSOPublicKeys dedupe = %#v", got)
	}
	t.Setenv(EnvSSOPublicKeyPrevious, "bad")
	if got := appendDistinctPublicKey([]string{pub}, explicitPub); len(got) != 2 || got[1] != explicitPub {
		t.Fatalf("appendDistinctPublicKey = %#v", got)
	}

	t.Setenv(EnvInviteKey, " injected-invite ")
	if got := SpokeInviteKey(); got != "injected-invite" {
		t.Fatalf("SpokeInviteKey explicit = %q", got)
	}
	t.Setenv(EnvInviteKey, "")
	if got := SpokeInviteKey(); got == "" || got == deriveDomainKey(master, infoInviteKey) {
		t.Fatalf("SpokeInviteKey did not derive per-hive key: %q", got)
	}

	t.Setenv(EnvHeartbeatKey, " injected-heartbeat ")
	if got := SpokeHeartbeatKey(); got != "injected-heartbeat" {
		t.Fatalf("SpokeHeartbeatKey explicit = %q", got)
	}
	t.Setenv(EnvHeartbeatKey, "")
	if got := SpokeHeartbeatKey(); got != derivePerHiveKey(master, infoHeartbeatKey, "hive-a") {
		t.Fatalf("SpokeHeartbeatKey per-hive = %q", got)
	}
	t.Setenv(EnvHiveID, "")
	if got := SpokeHeartbeatKey(); got != deriveDomainKey(master, infoHeartbeatKey) {
		t.Fatalf("SpokeHeartbeatKey legacy fallback = %q", got)
	}
}

func TestVerifySSOTokenAcrossKeys(t *testing.T) {
	now := time.Unix(1700000000, 0)
	seedA := SSOSigningSeedFromMaster("master-a")
	seedB := SSOSigningSeedFromMaster("master-b")
	token := MintSSOToken(seedB, "alice", "admin", "hive-a", now)
	user, role, idx, err := VerifySSOTokenAcrossKeys([]string{"bad", ssoPublicKeyFromSeed(seedA), ssoPublicKeyFromSeed(seedB)}, token, "hive-a", now)
	if err != nil || user != "alice" || role != "admin" || idx != 1 {
		t.Fatalf("VerifySSOTokenAcrossKeys = %q %q %d %v", user, role, idx, err)
	}
	if _, _, _, err := VerifySSOTokenAcrossKeys(nil, token, "hive-a", now); err == nil {
		t.Fatal("expected no-key error")
	}
	if _, _, _, err := VerifySSOTokenAcrossKeys([]string{ssoPublicKeyFromSeed(seedA)}, token, "hive-a", now); err == nil {
		t.Fatal("expected rejection by all keys")
	}
}

func TestKubernetesResourceHostProbes(t *testing.T) {
	ctx := context.Background()
	cfg := &inClusterConfig{BaseURL: "https://kubernetes.example", Token: "tok", Namespace: "ns one"}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer tok" {
			t.Fatalf("Authorization = %q", got)
		}
		switch req.URL.EscapedPath() {
		case "/apis/networking.k8s.io/v1/namespaces/ns%20one/ingresses":
			return jsonResponse(http.StatusOK, `{"items":[{"spec":{"rules":[{"host":" Other.EXAMPLE "},{"host":"dashboard.example"}]}}]}`), nil
		case "/apis/route.openshift.io/v1/namespaces/ns%20one/routes":
			return jsonResponse(http.StatusOK, `{"items":[{"metadata":{"name":"terminal"},"spec":{"host":"term.example"}},{"metadata":{"name":"hive-dashboard"},"spec":{"host":"Dash.EXAMPLE"}}]}`), nil
		default:
			return jsonResponse(http.StatusNotFound, `{}`), nil
		}
	})}

	found, unknown, errText := ingressHostExists(ctx, client, cfg, "dashboard.example")
	if !found || unknown || errText != "" {
		t.Fatalf("ingressHostExists = %v %v %q", found, unknown, errText)
	}
	if got := ingressServedHost(ctx, client, cfg); got != "other.example" {
		t.Fatalf("ingressServedHost = %q", got)
	}
	found, unknown, errText = routeHostExists(ctx, client, cfg, "dash.example")
	if !found || unknown || errText != "" {
		t.Fatalf("routeHostExists = %v %v %q", found, unknown, errText)
	}
	if got := routeServedHost(ctx, client, cfg); got != "dash.example" {
		t.Fatalf("routeServedHost = %q", got)
	}

	missingRouteClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{}`), nil
	})}
	found, unknown, errText = routeHostExists(ctx, missingRouteClient, cfg, "dash.example")
	if found || unknown || errText != "" {
		t.Fatalf("missing routeHostExists = %v %v %q", found, unknown, errText)
	}
	if got := routeServedHost(ctx, missingRouteClient, cfg); got != "" {
		t.Fatalf("missing routeServedHost = %q", got)
	}

	for status, want := range map[int]string{
		http.StatusForbidden:           "Kubernetes RBAC does not allow listing dashboard routes",
		http.StatusInternalServerError: "Kubernetes API returned HTTP 500",
	} {
		errClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return jsonResponse(status, `{}`), nil
		})}
		var out any
		unknown, errText = listKubernetesResource(ctx, errClient, cfg, "/resource", &out)
		if !unknown || errText != want {
			t.Fatalf("listKubernetesResource(%d) = %v %q, want %q", status, unknown, errText, want)
		}
	}
	badJSONClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{bad`), nil
	})}
	var out any
	unknown, errText = listKubernetesResource(ctx, badJSONClient, cfg, "/resource", &out)
	if !unknown || errText != "could not decode Kubernetes API response" {
		t.Fatalf("bad json = %v %q", unknown, errText)
	}
	transportErrClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}
	unknown, errText = listKubernetesResource(ctx, transportErrClient, cfg, "/resource", &out)
	if !unknown || errText == "" {
		t.Fatalf("transport error = %v %q", unknown, errText)
	}
}

func TestRouteExistenceProbeAndDiscoverSpokeServedHostWithInClusterAPI(t *testing.T) {
	mode := "ingress"
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.EscapedPath() {
		case "/apis/networking.k8s.io/v1/namespaces/hive-ns/ingresses":
			if mode == "ingress" {
				_, _ = io.WriteString(w, `{"items":[{"spec":{"rules":[{"host":"Ingress.EXAMPLE"}]}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"items":[]}`)
		case "/apis/route.openshift.io/v1/namespaces/hive-ns/routes":
			if mode == "forbidden" {
				http.Error(w, "no", http.StatusForbidden)
				return
			}
			if mode == "missing" {
				_, _ = io.WriteString(w, `{"items":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"hive-dashboard"},"spec":{"host":"Route.EXAMPLE"}}]}`)
		default:
			http.NotFound(w, req)
		}
	}))
	defer api.Close()

	saDir := filepath.Join("pkg", "hub", "spoke", "testdata", "runtime-serviceaccount")
	if err := os.MkdirAll(saDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(saDir) })
	if err := os.WriteFile(filepath.Join(saDir, "token"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saDir, "namespace"), []byte("hive-ns"), 0o600); err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(saDir, "ca.crt"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	oldSADir, oldHost, oldPort := serviceAccountDir, kubernetesAPIHost, kubernetesAPIPort
	serviceAccountDir = saDir
	u, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	kubernetesAPIHost = func() string { return host }
	kubernetesAPIPort = func() string { return port }
	t.Cleanup(func() {
		serviceAccountDir, kubernetesAPIHost, kubernetesAPIPort = oldSADir, oldHost, oldPort
	})

	routeExistenceProbeCache.Lock()
	oldRouteHost := routeExistenceProbeCache.host
	oldRouteNextProbe := routeExistenceProbeCache.nextProbe
	oldRouteResult := routeExistenceProbeCache.result
	routeExistenceProbeCache.host = ""
	routeExistenceProbeCache.nextProbe = time.Time{}
	routeExistenceProbeCache.result = nil
	routeExistenceProbeCache.Unlock()
	t.Cleanup(func() {
		routeExistenceProbeCache.Lock()
		routeExistenceProbeCache.host = oldRouteHost
		routeExistenceProbeCache.nextProbe = oldRouteNextProbe
		routeExistenceProbeCache.result = oldRouteResult
		routeExistenceProbeCache.Unlock()
	})

	got := routeExistenceProbe(context.Background(), "ingress.example")
	if got.Status != RouteExistenceFound || got.Kind != "Ingress" {
		t.Fatalf("ingress routeExistenceProbe = %+v", got)
	}
	checked := routeExistenceCheckFor(context.Background(), "https://ingress.example/path", nil)
	if checked == nil || checked.Status != RouteExistenceFound || checked.Kind != "Ingress" {
		t.Fatalf("routeExistenceCheckFor = %+v", checked)
	}
	checked.Kind = "mutated"
	if cached := routeExistenceCheckFor(context.Background(), "https://ingress.example/path", nil); cached.Kind != "Ingress" {
		t.Fatalf("cached routeExistenceCheckFor was not cloned: %+v", cached)
	}
	if host := discoverSpokeServedHost(context.Background()); host != "ingress.example" {
		t.Fatalf("ingress discoverSpokeServedHost = %q", host)
	}

	mode = "route"
	got = routeExistenceProbe(context.Background(), "route.example")
	if got.Status != RouteExistenceFound || got.Kind != "Route" {
		t.Fatalf("route routeExistenceProbe = %+v", got)
	}
	if host := discoverSpokeServedHost(context.Background()); host != "route.example" {
		t.Fatalf("route discoverSpokeServedHost = %q", host)
	}

	mode = "missing"
	got = routeExistenceProbe(context.Background(), "absent.example")
	if got.Status != RouteExistenceMissing || got.Error == "" {
		t.Fatalf("missing routeExistenceProbe = %+v", got)
	}
	routeExistenceProbeCache.Lock()
	routeExistenceProbeCache.host = ""
	routeExistenceProbeCache.nextProbe = time.Time{}
	routeExistenceProbeCache.result = nil
	routeExistenceProbeCache.Unlock()
	missingCheck := routeExistenceCheckFor(context.Background(), "https://absent.example/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if missingCheck == nil || missingCheck.Status != RouteExistenceMissing {
		t.Fatalf("missing routeExistenceCheckFor = %+v", missingCheck)
	}

	mode = "forbidden"
	got = routeExistenceProbe(context.Background(), "route.example")
	if got.Status != RouteExistenceUnknown || got.Error == "" {
		t.Fatalf("forbidden routeExistenceProbe = %+v", got)
	}
}

func TestRouteExistenceAndSpokeServedHostFallbacks(t *testing.T) {
	if got := dashboardHost(" https://Mixed.EXAMPLE:8443/path "); got != "mixed.example" {
		t.Fatalf("dashboardHost = %q", got)
	}
	if routeExistenceCheckFor(context.Background(), "not a url", nil) != nil {
		t.Fatal("invalid route URL should not produce a check")
	}
	if got := routeExistenceProbe(context.Background(), ""); got.Status != RouteExistenceUnknown || got.Error == "" {
		t.Fatalf("empty routeExistenceProbe = %+v", got)
	}

	servedHostProbeCache.Lock()
	oldHost, oldNext, oldProbed := servedHostProbeCache.host, servedHostProbeCache.nextProbe, servedHostProbeCache.probed
	servedHostProbeCache.host = "cached.example"
	servedHostProbeCache.nextProbe = time.Now().Add(time.Hour)
	servedHostProbeCache.probed = true
	servedHostProbeCache.Unlock()
	t.Cleanup(func() {
		servedHostProbeCache.Lock()
		servedHostProbeCache.host, servedHostProbeCache.nextProbe, servedHostProbeCache.probed = oldHost, oldNext, oldProbed
		servedHostProbeCache.Unlock()
	})
	if got := SpokeServedHost(context.Background()); got != "cached.example" {
		t.Fatalf("cached SpokeServedHost = %q", got)
	}
}

func TestPublicURLSelfProbeAndCheckCache(t *testing.T) {
	ctx := context.Background()
	if got := publicURLSelfProbe(ctx, "ftp://bad", nil); got.Status != PublicURLSelfCheckFail || got.Error == "" {
		t.Fatalf("invalid publicURLSelfProbe = %+v", got)
	}
	okClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://127.0.0.1:3002/path?q=1" || req.Host != "public.example" {
			t.Fatalf("probe request URL=%s Host=%s", req.URL.String(), req.Host)
		}
		return jsonResponse(http.StatusFound, ``), nil
	})}
	if got := publicURLSelfProbe(ctx, "https://public.example/path?q=1", okClient); got.Status != PublicURLSelfCheckOK || got.HTTPStatus != http.StatusFound {
		t.Fatalf("ok publicURLSelfProbe = %+v", got)
	}
	failClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadGateway, ``), nil
	})}
	if got := publicURLSelfProbe(ctx, "https://public.example/", failClient); got.Status != PublicURLSelfCheckFail || got.HTTPStatus != http.StatusBadGateway || got.Error != "HTTP 502" {
		t.Fatalf("failed publicURLSelfProbe = %+v", got)
	}
	oldProbeClientForFailure := publicURLSelfProbeClient
	publicURLSelfProbeClient = failClient
	publicURLSelfProbeCache.Lock()
	publicURLSelfProbeCache.url = "https://failing.example/"
	publicURLSelfProbeCache.nextProbe = time.Time{}
	publicURLSelfProbeCache.result = nil
	publicURLSelfProbeCache.stable = nil
	publicURLSelfProbeCache.consecutiveFailures = publicURLSelfCheckMinConsecutiveFailures - 1
	publicURLSelfProbeCache.Unlock()
	failedCheck := publicURLSelfCheckFor(ctx, "https://failing.example/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if failedCheck == nil || failedCheck.Status != PublicURLSelfCheckFail || failedCheck.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("failed publicURLSelfCheckFor = %+v", failedCheck)
	}
	publicURLSelfProbeClient = oldProbeClientForFailure

	oldProbeClient := publicURLSelfProbeClient
	publicURLSelfProbeClient = okClient
	t.Cleanup(func() { publicURLSelfProbeClient = oldProbeClient })
	publicURLSelfProbeCache.Lock()
	oldCacheURL := publicURLSelfProbeCache.url
	oldCacheNextProbe := publicURLSelfProbeCache.nextProbe
	oldCacheResult := publicURLSelfProbeCache.result
	oldCacheStable := publicURLSelfProbeCache.stable
	oldCacheConsecutiveFailures := publicURLSelfProbeCache.consecutiveFailures
	publicURLSelfProbeCache.url = ""
	publicURLSelfProbeCache.nextProbe = time.Time{}
	publicURLSelfProbeCache.result = nil
	publicURLSelfProbeCache.stable = nil
	publicURLSelfProbeCache.consecutiveFailures = 0
	publicURLSelfProbeCache.Unlock()
	if checked := publicURLSelfCheckFor(ctx, "https://public.example/path?q=1", nil); checked == nil || checked.Status != PublicURLSelfCheckOK || checked.HTTPStatus != http.StatusFound {
		t.Fatalf("uncached publicURLSelfCheckFor = %+v", checked)
	}

	publicURLSelfProbeCache.Lock()
	publicURLSelfProbeCache.url = "https://cached.example/"
	publicURLSelfProbeCache.nextProbe = time.Now().Add(time.Hour)
	publicURLSelfProbeCache.result = &PublicURLSelfCheck{Status: PublicURLSelfCheckOK, HTTPStatus: http.StatusNoContent}
	publicURLSelfProbeCache.stable = nil
	publicURLSelfProbeCache.consecutiveFailures = 0
	publicURLSelfProbeCache.Unlock()
	t.Cleanup(func() {
		publicURLSelfProbeCache.Lock()
		publicURLSelfProbeCache.url = oldCacheURL
		publicURLSelfProbeCache.nextProbe = oldCacheNextProbe
		publicURLSelfProbeCache.result = oldCacheResult
		publicURLSelfProbeCache.stable = oldCacheStable
		publicURLSelfProbeCache.consecutiveFailures = oldCacheConsecutiveFailures
		publicURLSelfProbeCache.Unlock()
	})
	got := publicURLSelfCheckFor(ctx, " https://cached.example/ ", nil)
	if got == nil || got.Status != PublicURLSelfCheckOK || got.HTTPStatus != http.StatusNoContent {
		t.Fatalf("cached publicURLSelfCheckFor = %+v", got)
	}
	got.HTTPStatus = http.StatusOK
	again := publicURLSelfCheckFor(ctx, "https://cached.example/", nil)
	if again.HTTPStatus != http.StatusNoContent {
		t.Fatal("publicURLSelfCheckFor did not clone cached result")
	}

	stable := &PublicURLSelfCheck{Status: PublicURLSelfCheckOK}
	fail := PublicURLSelfCheck{Status: PublicURLSelfCheckFail, Error: "boom"}
	consecutive := 0
	if gated := gatedPublicURLSelfCheck(fail, &consecutive, &stable); gated.Status != PublicURLSelfCheckOK {
		t.Fatalf("first gated failure should preserve stable result, got %+v", gated)
	}
	consecutive = publicURLSelfCheckMinConsecutiveFailures - 1
	if gated := gatedPublicURLSelfCheck(fail, &consecutive, &stable); gated.Status != PublicURLSelfCheckFail || stable.Status != PublicURLSelfCheckFail {
		t.Fatalf("threshold gated failure = %+v stable=%+v", gated, stable)
	}
}

func TestClusterMetricParsingBranches(t *testing.T) {
	if got, ok := parseNodeStatsSummaryDisk([]byte(`{"node":{"fs":{"capacityBytes":10,"usedBytes":3}}}`)); !ok || got.capacityBytes != 10 || got.usedBytes != 3 {
		t.Fatalf("parseNodeStatsSummaryDisk valid = %+v %v", got, ok)
	}
	if _, ok := parseNodeStatsSummaryDisk([]byte(`bad`)); ok {
		t.Fatal("invalid stats summary parsed")
	}
	if _, ok := parseNodeStatsSummaryDisk([]byte(`{"node":{"fs":{"capacityBytes":0,"usedBytes":0}}}`)); ok {
		t.Fatal("zero capacity stats summary parsed")
	}
	if got := parseK8sCPU("250000000n"); got != 250 {
		t.Fatalf("parseK8sCPU nanos = %d", got)
	}
	if got := parseK8sCPU("2"); got != 2000 {
		t.Fatalf("parseK8sCPU cores = %d", got)
	}
	if got := parseK8sMemory("2Gi"); got != 2*giToBytes {
		t.Fatalf("parseK8sMemory Gi = %d", got)
	}
	if got := parseK8sMemory("42"); got != 42 {
		t.Fatalf("parseK8sMemory raw = %d", got)
	}
}
