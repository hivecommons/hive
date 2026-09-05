package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #5977 renewal monitoring. Once a cluster opts in with wildcard_tls_secret its
// provisioned spoke Ingresses carry no tls: block of their own, so ONE
// certificate stands behind every hosted dashboard there — ~263 hostnames
// across the two clusters in the issue. These tests are built around the two
// consequences: a renewal that silently fails takes all of them down at once,
// and the operator's opt-in assertion ("the secret is there and the controller
// serves it") had until now no check anywhere against the actual cluster.

func healthTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testCert mints a self-signed certificate with the given SANs and validity.
// In-memory and hermetic: no fixture files to drift out of date, and expiry
// boundaries can be expressed relative to a fixed `now`.
func testCert(t *testing.T, issuerCN string, dnsNames []string, notBefore, notAfter time.Time) (*x509.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	// The Issuer on a certificate comes from the PARENT's Subject, not from the
	// template's own Issuer field (which CreateCertificate ignores). So sign
	// with a separate issuer template — otherwise every test certificate would
	// report itself as its own issuer and the Issuer assertion would be vacuous.
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating issuer key: %v", err)
	}
	parent := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: issuerCN},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "wildcard-test"},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	return parsed, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// tlsSecretJSON renders what `kubectl get secret -o json` returns for a
// kubernetes.io/tls secret holding pemBytes.
func tlsSecretJSON(t *testing.T, pemBytes []byte) []byte {
	t.Helper()
	payload := map[string]any{
		"data": map[string]string{
			"tls.crt": base64.StdEncoding.EncodeToString(pemBytes),
			"tls.key": base64.StdEncoding.EncodeToString([]byte("not-a-real-key")),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling secret: %v", err)
	}
	return raw
}

// ── secretTLSCert ──────────────────────────────────────────────────────────

func TestSecretTLSCertReadsTheLeaf(t *testing.T) {
	want, pemBytes := testCert(t, "R3", []string{"*.hive.hivecommons.dev"},
		time.Now().Add(-time.Hour), time.Now().Add(60*24*time.Hour))

	got, err := secretTLSCert(tlsSecretJSON(t, pemBytes))
	if err != nil {
		t.Fatalf("secretTLSCert: %v", err)
	}
	if !got.NotAfter.Equal(want.NotAfter) {
		t.Errorf("NotAfter = %v, want %v", got.NotAfter, want.NotAfter)
	}
	if len(got.DNSNames) != 1 || got.DNSNames[0] != "*.hive.hivecommons.dev" {
		t.Errorf("DNSNames = %v, want [*.hive.hivecommons.dev]", got.DNSNames)
	}
}

// A chain presents the LEAF first, and the leaf is what the server serves. A
// reader that took the last block would report the CA's expiry — years out —
// and never warn.
func TestSecretTLSCertTakesTheFirstBlockOfAChain(t *testing.T) {
	leaf, leafPEM := testCert(t, "R3", []string{"*.hive.hivecommons.dev"},
		time.Now().Add(-time.Hour), time.Now().Add(10*24*time.Hour))
	_, caPEM := testCert(t, "ISRG Root X1", []string{"ca.example"},
		time.Now().Add(-time.Hour), time.Now().Add(3650*24*time.Hour))

	got, err := secretTLSCert(tlsSecretJSON(t, append(append([]byte{}, leafPEM...), caPEM...)))
	if err != nil {
		t.Fatalf("secretTLSCert: %v", err)
	}
	if !got.NotAfter.Equal(leaf.NotAfter) {
		t.Errorf("read the wrong certificate from the chain: NotAfter = %v, want the leaf's %v",
			got.NotAfter, leaf.NotAfter)
	}
}

func TestSecretTLSCertFailsClosed(t *testing.T) {
	_, pemBytes := testCert(t, "R3", []string{"*.hive.hivecommons.dev"},
		time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	valid := tlsSecretJSON(t, pemBytes)

	cases := []struct {
		name string
		raw  []byte
	}{
		{"not JSON", []byte("<html>403</html>")},
		{"no data section", []byte(`{"metadata":{"name":"hive-wildcard-tls"}}`)},
		{"no tls.crt key", []byte(`{"data":{"tls.key":"aaaa"}}`)},
		{"empty tls.crt", []byte(`{"data":{"tls.crt":"  "}}`)},
		{"tls.crt is not base64", []byte(`{"data":{"tls.crt":"!!!not base64!!!"}}`)},
		{"tls.crt is not PEM", []byte(`{"data":{"tls.crt":"` +
			base64.StdEncoding.EncodeToString([]byte("just some text")) + `"}}`)},
		{"PEM body is not a certificate", []byte(`{"data":{"tls.crt":"` +
			base64.StdEncoding.EncodeToString(pem.EncodeToMemory(
				&pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")})) + `"}}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := secretTLSCert(tc.raw)
			if err == nil {
				t.Fatalf("want an error, got a certificate: %+v", cert)
			}
			if cert != nil {
				t.Errorf("a failed parse must not also return a certificate, got %+v", cert)
			}
		})
	}

	// Control: the same function does succeed on a good secret, so the cases
	// above are failing for their stated reason and not because the helper is
	// broken.
	if _, err := secretTLSCert(valid); err != nil {
		t.Fatalf("control case failed: %v", err)
	}
}

// ── certCoversWildcard ─────────────────────────────────────────────────────

func TestCertCoversWildcard(t *testing.T) {
	const domain = "hive.hivecommons.dev"
	cases := []struct {
		name     string
		dnsNames []string
		want     bool
	}{
		{"the wildcard SAN", []string{"*.hive.hivecommons.dev"}, true},
		{"the fleet wildcard as actually issued", []string{
			"*.hive.hivecommons.dev", "*.lke.hive.hivecommons.dev", "hive.hivecommons.dev",
		}, true},
		{"case and trailing dot are not significant", []string{"*.HIVE.HiveCommons.DEV."}, true},

		// The hub omits the tls: block for every single-label host under the
		// domain, INCLUDING hives that do not exist yet. Only the wildcard entry
		// justifies that; a certificate listing today's hosts explicitly would
		// pass a per-host check and break the next hive provisioned.
		{"today's hosts listed explicitly is not coverage", []string{
			"hosted-a-ab12.hive.hivecommons.dev", "hosted-b-cd34.hive.hivecommons.dev",
		}, false},
		{"the apex alone is not coverage", []string{"hive.hivecommons.dev"}, false},
		{"a wildcard one level up is not coverage", []string{"*.hivecommons.dev"}, false},
		{"a wildcard for another domain", []string{"*.hive.kubestellar.io"}, false},
		{"no SANs at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, _ := testCert(t, "R3", tc.dnsNames,
				time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			if got := certCoversWildcard(cert, domain); got != tc.want {
				t.Errorf("certCoversWildcard(%v, %q) = %v, want %v", tc.dnsNames, domain, got, tc.want)
			}
		})
	}

	t.Run("nil certificate", func(t *testing.T) {
		if certCoversWildcard(nil, domain) {
			t.Error("a nil certificate must never read as covering the domain")
		}
	})
	t.Run("empty domain", func(t *testing.T) {
		cert, _ := testCert(t, "R3", []string{"*.hive.hivecommons.dev"},
			time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if certCoversWildcard(cert, "") {
			t.Error("an empty domain must never read as covered")
		}
	})
}

// ── summarizeWildcardTLS ───────────────────────────────────────────────────

func TestSummarizeWildcardTLSStatuses(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	const domain = "hive.hivecommons.dev"
	const secretRef = "hive-hub/hive-wildcard-tls"

	cases := []struct {
		name     string
		dnsNames []string
		notAfter time.Time
		want     string
		wantDays int
	}{
		{
			name:     "a freshly renewed certificate is ok",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now.Add(89 * 24 * time.Hour),
			want:     wildcardStatusOK,
			wantDays: 89,
		},
		{
			// cert-manager starts renewing a 90-day certificate at 30 days out,
			// so 30 days remaining is the NORMAL state of a healthy fleet and
			// must stay quiet. Warning here would fire every quarter on every
			// opted-in cluster.
			name:     "inside cert-manager's own renewal window is still ok",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now.Add(30 * 24 * time.Hour),
			want:     wildcardStatusOK,
			wantDays: 30,
		},
		{
			name:     "exactly at the warn window is still ok",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now.Add(wildcardExpiryWarnWindow),
			want:     wildcardStatusOK,
			wantDays: 21,
		},
		{
			name:     "just inside the warn window is expiring",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now.Add(wildcardExpiryWarnWindow - time.Minute),
			want:     wildcardStatusExpiring,
			wantDays: 20,
		},
		{
			name:     "a day out is expiring, not expired",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now.Add(24 * time.Hour),
			want:     wildcardStatusExpiring,
			wantDays: 1,
		},
		{
			// The instant of expiry counts as expired: a certificate is invalid
			// at NotAfter, not after it.
			name:     "exactly at NotAfter is expired",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now,
			want:     wildcardStatusExpired,
			wantDays: 0,
		},
		{
			name:     "past NotAfter is expired with negative days",
			dnsNames: []string{"*.hive.hivecommons.dev"},
			notAfter: now.Add(-3 * 24 * time.Hour),
			want:     wildcardStatusExpired,
			wantDays: -3,
		},
		{
			// The check that had no home before this: the hub decided to omit
			// tls: blocks from the domain in clusters.json, and the certificate
			// does not actually carry that wildcard.
			name:     "a certificate that does not cover the domain",
			dnsNames: []string{"*.hive.kubestellar.io"},
			notAfter: now.Add(80 * 24 * time.Hour),
			want:     wildcardStatusDomainMismatch,
			wantDays: 80,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cert, _ := testCert(t, "R3", tc.dnsNames, now.Add(-time.Hour), tc.notAfter)
			rep := summarizeWildcardTLS(cert, secretRef, domain, now, wildcardExpiryWarnWindow)

			if rep.Status != tc.want {
				t.Errorf("Status = %q, want %q (detail: %s)", rep.Status, tc.want, rep.Detail)
			}
			if rep.Secret != secretRef {
				t.Errorf("Secret = %q, want %q", rep.Secret, secretRef)
			}
			if rep.DaysRemaining == nil {
				t.Fatal("DaysRemaining must be set whenever a certificate was read")
			}
			if *rep.DaysRemaining != tc.wantDays {
				t.Errorf("DaysRemaining = %d, want %d", *rep.DaysRemaining, tc.wantDays)
			}
			if rep.NotAfter == "" {
				t.Error("NotAfter must be reported whenever a certificate was read")
			}
			if rep.Issuer != "R3" {
				t.Errorf("Issuer = %q, want R3 — an operator tells a real certificate from a stand-in by this", rep.Issuer)
			}
			// Healthy() and Status must never disagree.
			if rep.Healthy() != (tc.want == wildcardStatusOK) {
				t.Errorf("Healthy() = %v but Status = %q", rep.Healthy(), rep.Status)
			}
			// Every finding must explain itself; "ok" needs no detail.
			if tc.want != wildcardStatusOK && strings.TrimSpace(rep.Detail) == "" {
				t.Errorf("status %q carries no Detail — a finding an operator cannot act on", rep.Status)
			}
		})
	}
}

// Coverage is judged before expiry: a certificate that cannot serve the domain
// is already failing every request, and reporting it as "expiring" would send
// an operator to renew a certificate that would still be wrong.
func TestSummarizeWildcardTLSCoverageOutranksExpiry(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	cert, _ := testCert(t, "R3", []string{"*.hive.kubestellar.io"},
		now.Add(-90*24*time.Hour), now.Add(2*24*time.Hour))

	rep := summarizeWildcardTLS(cert, "hive-hub/hive-wildcard-tls",
		"hive.hivecommons.dev", now, wildcardExpiryWarnWindow)

	if rep.Status != wildcardStatusDomainMismatch {
		t.Errorf("Status = %q, want %q — a certificate for the wrong domain is not merely expiring",
			rep.Status, wildcardStatusDomainMismatch)
	}
	if rep.CoversClusterDomain {
		t.Error("CoversClusterDomain must be false for a certificate that does not carry the wildcard")
	}
}

func TestSummarizeWildcardTLSNilCertIsUnreadableNotHealthy(t *testing.T) {
	rep := summarizeWildcardTLS(nil, "hive-hub/hive-wildcard-tls",
		"hive.hivecommons.dev", time.Now(), wildcardExpiryWarnWindow)

	if rep.Status != wildcardStatusUnreadable {
		t.Errorf("Status = %q, want %q", rep.Status, wildcardStatusUnreadable)
	}
	if rep.Healthy() {
		t.Error("an unreadable certificate must never report Healthy")
	}
	if rep.DaysRemaining != nil {
		t.Error("DaysRemaining must stay nil when nothing was read — a zero would read as 'expires today'")
	}
}

func TestWildcardTLSReportHealthyOnNil(t *testing.T) {
	var rep *WildcardTLSReport
	if rep.Healthy() {
		t.Error("a nil report means UNKNOWN and must never read as healthy")
	}
}

// ── splitSecretRef ─────────────────────────────────────────────────────────

func TestSplitSecretRef(t *testing.T) {
	cases := []struct {
		in               string
		wantNS, wantName string
		wantOK           bool
	}{
		{"hive-hub/hive-wildcard-tls", "hive-hub", "hive-wildcard-tls", true},
		{"  hive-hub/hive-wildcard-tls  ", "hive-hub", "hive-wildcard-tls", true},
		// A bare name would resolve in whatever namespace kubectl defaults to,
		// which could be a DIFFERENT secret that happens to share the name.
		{"hive-wildcard-tls", "", "", false},
		{"a/b/c", "", "", false},
		{"/hive-wildcard-tls", "", "", false},
		{"hive-hub/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			ns, name, ok := splitSecretRef(tc.in)
			if ok != tc.wantOK || ns != tc.wantNS || name != tc.wantName {
				t.Errorf("splitSecretRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.in, ns, name, ok, tc.wantNS, tc.wantName, tc.wantOK)
			}
		})
	}
}

// ── kubectlSaysNotFound ────────────────────────────────────────────────────

// The distinction this draws decides whether an operator is told their
// certificate is GONE or that the hub could not look. It must fail toward the
// second: only a recognised not-found convicts.
func TestKubectlSaysNotFound(t *testing.T) {
	exitErrWith := func(stderr string) error {
		return &exec.ExitError{Stderr: []byte(stderr)}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"kubectl's not-found message", exitErrWith(
			`Error from server (NotFound): secrets "hive-wildcard-tls" not found`), true},
		{"lowercase variant", exitErrWith(`error: secret not found`), true},

		{"an unreachable API server is not a missing secret", exitErrWith(
			`Unable to connect to the server: dial tcp 10.0.0.1:443: i/o timeout`), false},
		{"an RBAC denial is not a missing secret", exitErrWith(
			`Error from server (Forbidden): secrets "hive-wildcard-tls" is forbidden`), false},
		{"an expired kubeconfig is not a missing secret", exitErrWith(
			`error: You must be logged in to the server (Unauthorized)`), false},
		{"a timeout is not a missing secret", exitErrWith(
			`error: the server was unable to return a response in the time allotted`), false},

		// Not an ExitError at all: kubectl never ran (binary missing, context
		// cancelled). Nothing was learned about the secret.
		{"a non-exit error", errors.New("exec: \"kubectl\": executable file not found in $PATH"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kubectlSaysNotFound(tc.err); got != tc.want {
				t.Errorf("kubectlSaysNotFound = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIngressNginxDefaultSSLArgsCover(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"equals form", "--watch-ingress-without-class --default-ssl-certificate=hive-hub/hive-wildcard-tls", true},
		{"separate value form", "--watch-ingress-without-class --default-ssl-certificate hive-hub/hive-wildcard-tls", true},
		{"missing flag", "--watch-ingress-without-class", false},
		{"wrong secret", "--default-ssl-certificate=other/wildcard", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ingressNginxDefaultSSLArgsCover(tc.args, "hive-hub/hive-wildcard-tls"); got != tc.want {
				t.Errorf("ingressNginxDefaultSSLArgsCover = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── collectWildcardTLSHealth vetoes ────────────────────────────────────────

// Each of these returns before any kubectl runs, which is also why the test can
// assert them without a cluster: a veto that shelled out first would hang here.
func TestCollectWildcardTLSHealthVetoes(t *testing.T) {
	ctx := context.Background()
	reachable := func(c *ClusterConfig) *ClusterConfig {
		c.KubeconfigPath = "/nonexistent/kubeconfig-for-test"
		return c
	}

	cases := []struct {
		name    string
		cluster *ClusterConfig
	}{
		{"nil cluster", nil},
		{
			// No opt-in: spokes here still carry per-host certificates, so no
			// wildcard is load-bearing and a report would be noise.
			name: "cluster has not opted in",
			cluster: reachable(&ClusterConfig{
				ID: "lke648397", Domain: "lke.hive.hivecommons.dev", IngressType: "nginx",
			}),
		},
		{
			name: "opted in but blank",
			cluster: reachable(&ClusterConfig{
				ID: "lke648397", Domain: "lke.hive.hivecommons.dev",
				IngressType: "nginx", WildcardTLSSecret: "   ",
			}),
		},
		{
			// Routes terminate TLS with edge termination and no secret; the same
			// veto servesHostFromWildcard applies.
			name: "OpenShift Route cluster",
			cluster: reachable(&ClusterConfig{
				ID: "ocp", Domain: "apps.example.com",
				IngressType: ingressTypeOpenShiftRoute, WildcardTLSSecret: "hive-hub/hive-wildcard-tls",
			}),
		},
		{
			// The hub genuinely cannot look, so it must claim nothing.
			name: "kubectl cannot reach the cluster",
			cluster: &ClusterConfig{
				ID: "pull-only", Domain: "hive.hivecommons.dev", IngressType: "nginx",
				WildcardTLSSecret: "hive-hub/hive-wildcard-tls", PullOnly: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rep := collectWildcardTLSHealth(ctx, tc.cluster, time.Second, time.Now(), healthTestLogger()); rep != nil {
				t.Errorf("want no report, got %+v", rep)
			}
		})
	}
}

// A malformed opt-in is reported rather than ignored: spokes on this cluster
// are already omitting their tls: block on the strength of this value.
func TestCollectWildcardTLSHealthMalformedSecretRef(t *testing.T) {
	cluster := &ClusterConfig{
		ID: "hive-oke", Domain: "hive.hivecommons.dev", IngressType: "nginx",
		WildcardTLSSecret: "hive-wildcard-tls", // no namespace
		KubeconfigPath:    "/nonexistent/kubeconfig-for-test",
	}
	rep := collectWildcardTLSHealth(context.Background(), cluster, time.Second, time.Now(), healthTestLogger())
	if rep == nil {
		t.Fatal("a malformed wildcard_tls_secret must be reported, not silently skipped")
	}
	if rep.Status != wildcardStatusUnreadable {
		t.Errorf("Status = %q, want %q", rep.Status, wildcardStatusUnreadable)
	}
	if rep.Healthy() {
		t.Error("a malformed reference must never read as healthy")
	}
}

// ── the report survives the JSON round-trip the dashboard reads ────────────

func TestWildcardTLSReportJSONShape(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	cert, _ := testCert(t, "R3", []string{"*.hive.hivecommons.dev"},
		now.Add(-time.Hour), now.Add(5*24*time.Hour))
	rep := summarizeWildcardTLS(cert, "hive-hub/hive-wildcard-tls", "hive.hivecommons.dev", now, wildcardExpiryWarnWindow)

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshalling report: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshalling report: %v", err)
	}
	// The dashboard switches on these two, so a rename must fail here rather
	// than render nothing on a cluster whose certificate is about to expire.
	for _, key := range []string{"status", "secret", "days_remaining", "covers_cluster_domain", "default_ssl_configured"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("report JSON is missing %q: %s", key, raw)
		}
	}
	if decoded["status"] != wildcardStatusExpiring {
		t.Errorf("status = %v, want %q", decoded["status"], wildcardStatusExpiring)
	}
}

// ── collectWildcardTLSHealth against a scripted kubectl ────────────────────
//
// The vetoes above return before kubectl runs, so they say nothing about the
// branches that matter most in production. These do: a wildcard secret that is
// GONE on an opted-in cluster is the loudest finding this file can produce, and
// it is reached only through kubectl's exit status.

// installWildcardKubectl writes a fake kubectl to a temp dir and prepends it to
// PATH for the test, following installScriptedKubectl next door. mode selects
// which cluster the fake stands in for.
func installWildcardKubectl(t *testing.T, mode, payload string) {
	t.Helper()
	dir := t.TempDir()
	var body string
	switch mode {
	case "found":
		body = "case \"$*\" in\n" +
			"  *'get deployment ingress-nginx-controller'*) printf '%s\\n' '--default-ssl-certificate=hive-hub/hive-wildcard-tls'; exit 0 ;;\n" +
			"  *'get secret hive-wildcard-tls'*) cat <<'JSON'\n" + payload + "\nJSON\nexit 0 ;;\n" +
			"esac\n" +
			"echo 'unexpected kubectl args:' \"$*\" >&2\nexit 1\n"
	case "nodefault":
		body = "case \"$*\" in\n" +
			"  *'get deployment ingress-nginx-controller'*) printf '%s\\n' '--watch-ingress-without-class'; exit 0 ;;\n" +
			"  *'get secret hive-wildcard-tls'*) cat <<'JSON'\n" + payload + "\nJSON\nexit 0 ;;\n" +
			"esac\n" +
			"echo 'unexpected kubectl args:' \"$*\" >&2\nexit 1\n"
	case "notfound":
		// kubectl's real shape: a non-zero exit with the message on STDERR,
		// which is where kubectlSaysNotFound reads it from.
		body = "case \"$*\" in\n" +
			"  *'get deployment ingress-nginx-controller'*) printf '%s\\n' '--default-ssl-certificate=hive-hub/hive-wildcard-tls'; exit 0 ;;\n" +
			"esac\n" +
			`echo 'Error from server (NotFound): secrets "hive-wildcard-tls" not found' >&2` + "\nexit 1\n"
	case "unreachable":
		body = `echo 'Unable to connect to the server: dial tcp 10.0.0.1:443: i/o timeout' >&2` + "\nexit 1\n"
	default:
		t.Fatalf("unknown fake kubectl mode %q", mode)
	}
	script := "#!/bin/sh\n" + body
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func optedInCluster() *ClusterConfig {
	return &ClusterConfig{
		ID:                "hive-oke",
		Domain:            "hive.hivecommons.dev",
		IngressType:       "nginx",
		WildcardTLSSecret: "hive-hub/hive-wildcard-tls",
		InCluster:         true,
	}
}

// The critical finding: the operator asserted the secret exists, and it does
// not. Every spoke provisioned without a tls: block on this cluster is being
// served ingress-nginx's self-signed certificate right now.
func TestCollectWildcardTLSHealthReportsAMissingSecret(t *testing.T) {
	installWildcardKubectl(t, "notfound", "")

	rep := collectWildcardTLSHealth(context.Background(), optedInCluster(), time.Second, time.Now(), healthTestLogger())
	if rep == nil {
		t.Fatal("a missing wildcard secret on an opted-in cluster must be REPORTED, not omitted")
	}
	if rep.Status != wildcardStatusMissing {
		t.Errorf("Status = %q, want %q", rep.Status, wildcardStatusMissing)
	}
	if rep.Healthy() {
		t.Error("a missing certificate must never read as healthy")
	}
	if rep.Secret != "hive-hub/hive-wildcard-tls" {
		t.Errorf("Secret = %q, want the asserted reference", rep.Secret)
	}
}

// An unreachable cluster taught nothing about the certificate, so it must claim
// nothing. Reporting "missing" here would tell an operator their certificate
// vanished when the truth was a network blip.
func TestCollectWildcardTLSHealthUnreachableClusterReportsNothing(t *testing.T) {
	installWildcardKubectl(t, "unreachable", "")

	if rep := collectWildcardTLSHealth(context.Background(), optedInCluster(), time.Second, time.Now(), healthTestLogger()); rep != nil {
		t.Errorf("want no report when the cluster could not be reached, got %+v", rep)
	}
}

func TestCollectWildcardTLSHealthReportsDefaultSSLNotConfigured(t *testing.T) {
	_, pemBytes := testCert(t, "R3",
		[]string{"*.hive.hivecommons.dev"},
		time.Now().Add(-24*time.Hour), time.Now().Add(75*24*time.Hour))
	installWildcardKubectl(t, "nodefault", string(tlsSecretJSON(t, pemBytes)))

	rep := collectWildcardTLSHealth(context.Background(), optedInCluster(), time.Second, time.Now(), healthTestLogger())
	if rep == nil {
		t.Fatal("an opted-in cluster whose controller does not serve the wildcard must be reported")
	}
	if rep.Status != wildcardStatusNotServed {
		t.Errorf("Status = %q, want %q", rep.Status, wildcardStatusNotServed)
	}
	if rep.Healthy() {
		t.Error("a controller that does not serve the wildcard must never read as healthy")
	}
	if rep.DefaultSSLConfigured {
		t.Error("DefaultSSLConfigured = true when ingress-nginx lacks the default SSL flag")
	}
}

func TestCollectWildcardTLSHealthReadsALiveCertificate(t *testing.T) {
	_, pemBytes := testCert(t, "R3",
		[]string{"*.hive.hivecommons.dev", "*.lke.hive.hivecommons.dev", "hive.hivecommons.dev"},
		time.Now().Add(-24*time.Hour), time.Now().Add(75*24*time.Hour))
	installWildcardKubectl(t, "found", string(tlsSecretJSON(t, pemBytes)))

	rep := collectWildcardTLSHealth(context.Background(), optedInCluster(), time.Second, time.Now(), healthTestLogger())
	if rep == nil {
		t.Fatal("want a report for an opted-in cluster whose secret is readable")
	}
	if rep.Status != wildcardStatusOK {
		t.Errorf("Status = %q, want %q (detail: %s)", rep.Status, wildcardStatusOK, rep.Detail)
	}
	if !rep.CoversClusterDomain {
		t.Error("CoversClusterDomain = false for a certificate carrying *.hive.hivecommons.dev")
	}
	if !rep.DefaultSSLConfigured {
		t.Error("DefaultSSLConfigured = false even though ingress-nginx names the wildcard as its default certificate")
	}
	if len(rep.DNSNames) != 3 {
		t.Errorf("DNSNames = %v, want all three SANs carried through for the operator", rep.DNSNames)
	}
}

// A secret that exists but is not a TLS secret is a finding, not silence.
func TestCollectWildcardTLSHealthUnparseableSecret(t *testing.T) {
	installWildcardKubectl(t, "found", `{"data":{"ca.crt":"aaaa"}}`)

	rep := collectWildcardTLSHealth(context.Background(), optedInCluster(), time.Second, time.Now(), healthTestLogger())
	if rep == nil {
		t.Fatal("want a report when the secret exists but holds no certificate")
	}
	if rep.Status != wildcardStatusUnreadable {
		t.Errorf("Status = %q, want %q", rep.Status, wildcardStatusUnreadable)
	}
	if rep.Healthy() {
		t.Error("an unreadable secret must never read as healthy")
	}
}
