package hub

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// Wildcard certificate health for opted-in clusters (#5977).
//
// WHAT CHANGED UNDER US. Before the wildcard opt-in landed, a spoke's TLS was
// its own problem: each Ingress carried a tls: block naming a per-namespace
// secret, so a certificate that failed to renew took down ONE dashboard. On a
// cluster that has set wildcard_tls_secret, provisioned Ingresses omit that
// block entirely and the ingress controller's --default-ssl-certificate serves
// them — so one certificate now stands behind every hosted spoke on the
// cluster. The issue puts that at ~263 hostnames across two clusters.
//
// The blast radius of a renewal failure therefore went from one host to all of
// them, and nothing was watching. This file is the monitoring #5977 named as a
// prerequisite for enabling the opt-in fleet-wide.
//
// ── The verification gap this closes ───────────────────────────────────────
//
// wildcard_tls_secret is an operator ASSERTION. wildcard_tls.go says so
// outright: the two prerequisites (the secret exists on the cluster, and the
// controller serves it by default) "are not visible from here, which is
// precisely why this is an operator assertion rather than something inferred."
//
// That reasoning is right at PROVISION time — the provisioner must decide
// without a cluster round-trip, and guessing wrong takes the cluster down. It
// is not a reason never to check. The health build already talks to every
// cluster on a timer, so the assertion can be verified AFTER the fact, where
// being wrong costs a warning rather than an outage. Two of the failure shapes
// below are exactly that assertion turning out to be false:
//
//   - wildcardStatusMissing: the flag is set and the secret is not there. Every
//     wildcard-served spoke on the cluster is being handed ingress-nginx's
//     built-in self-signed certificate right now.
//   - wildcardStatusDomainMismatch: the secret is there and does not carry
//     "*.<cluster domain>". servesHostFromWildcard decides coverage from the
//     domain in clusters.json; this is the only place the CERTIFICATE gets a
//     say, and a mismatch means the hub is omitting tls: blocks for hosts this
//     certificate cannot serve.
//
// Neither is visible from clusters.json, and both stay silent until a user
// opens a dashboard.
//
// ── Read-only, and scoped to clusters that actually depend on it ───────────
//
// One `kubectl get secret` per opted-in cluster per health build. A cluster
// without the opt-in issues no request at all: its spokes still carry per-host
// certificates, so no wildcard is load-bearing there.

const (
	// wildcardExpiryWarnWindow is how close to expiry the certificate may get
	// before this reports it.
	//
	// It is NOT "warn early", it is "renewal is overdue". cert-manager's default
	// renewBefore is one third of the certificate's lifetime, so a 90-day Let's
	// Encrypt certificate starts renewing at 30 days remaining. Warning at 30
	// would fire on every healthy renewal, every quarter, on every opted-in
	// cluster — and an alert that is normally firing is one nobody reads.
	//
	// 21 days means the renewal window opened over a week ago and nothing came
	// of it. That is a real fault (a broken DNS-01 path, an exhausted ACME quota
	// — the very thing #5977 was filed about) and it still leaves three weeks to
	// fix it before ~263 hostnames fail validation at once.
	wildcardExpiryWarnWindow = 21 * 24 * time.Hour

	// Report statuses. Named constants because the dashboard switches on them
	// and a typo would silently render nothing.
	//
	// wildcardStatusOK is the ONLY value meaning "checked, and fine". Every
	// other value, wildcardStatusUnreadable included, is a finding.
	wildcardStatusOK             = "ok"
	wildcardStatusExpiring       = "expiring"
	wildcardStatusExpired        = "expired"
	wildcardStatusMissing        = "missing"
	wildcardStatusUnreadable     = "unreadable"
	wildcardStatusDomainMismatch = "domain_mismatch"
	wildcardStatusNotServed      = "not_served"
)

// Sentinel parse failures, so a caller can tell "this secret is not a TLS
// secret" from "this PEM is corrupt" without matching on message text.
var (
	errNoTLSCrt   = errors.New("secret has no tls.crt")
	errNoPEMBlock = errors.New("tls.crt is not PEM")
)

// WildcardTLSReport is the per-cluster wildcard-certificate signal surfaced in
// fleet health.
//
// Present only for clusters that opted in with wildcard_tls_secret, since only
// those have spokes depending on it. Absent means either "this cluster does not
// use the wildcard" or "the hub could not look" — the same nil-means-unknown
// contract StuckPods and LeakedNamespaces carry on this surface, for the same
// reason: a reassuring zero the hub never verified is worse than no answer.
type WildcardTLSReport struct {
	// Secret is the ns/name the cluster asserted, echoed back so an operator
	// reading a finding knows which object to go and look at.
	Secret string `json:"secret"`

	// Status is one of the wildcardStatus* values. Anything but "ok" is a
	// finding.
	Status string `json:"status"`

	// Detail is a one-line human-readable explanation of a non-ok status.
	Detail string `json:"detail,omitempty"`

	// NotAfter is the certificate's expiry, RFC3339. Empty when no certificate
	// could be read.
	NotAfter string `json:"not_after,omitempty"`

	// DaysRemaining is whole days until NotAfter, negative once expired. A
	// pointer so "unknown" (no readable certificate) stays distinguishable from
	// "zero days left" — opposite readings of the same field.
	DaysRemaining *int `json:"days_remaining,omitempty"`

	// Issuer is the certificate's issuer CN, which is how an operator tells a
	// real Let's Encrypt certificate from a self-signed stand-in at a glance.
	Issuer string `json:"issuer,omitempty"`

	// DNSNames are the certificate's SANs, so the coverage question this issue
	// turned on ("does this actually cover the hosts we stopped issuing for?")
	// is answerable from the health payload without cluster access.
	DNSNames []string `json:"dns_names,omitempty"`

	// CoversClusterDomain records whether the SANs include "*.<cluster domain>"
	// — the exact predicate servesHostFromWildcard assumes when it omits a
	// spoke's tls: block.
	CoversClusterDomain bool `json:"covers_cluster_domain"`

	// DefaultSSLConfigured records whether ingress-nginx's
	// --default-ssl-certificate names this secret. It is false for findings
	// where the hub could not prove the controller serves the wildcard.
	DefaultSSLConfigured bool `json:"default_ssl_configured"`
}

// Healthy reports whether the certificate needs no attention.
func (r *WildcardTLSReport) Healthy() bool {
	return r != nil && r.Status == wildcardStatusOK
}

// secretTLSCert extracts the leaf certificate from the JSON of
// `kubectl get secret <name> -o json`.
//
// PURE: no kubectl, no cluster. A kubernetes.io/tls secret carries
// data["tls.crt"], base64 of a PEM chain; the LEAF is the first block, which is
// the certificate the server actually presents.
//
// Every failure returns an error rather than a nil certificate with a nil
// error, so no caller can read "no certificate" as "fine".
func secretTLSCert(raw []byte) (*x509.Certificate, error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &secret); err != nil {
		return nil, err
	}
	encoded := secret.Data["tls.crt"]
	if strings.TrimSpace(encoded) == "" {
		return nil, errNoTLSCrt
	}
	der, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return nil, errNoPEMBlock
	}
	return x509.ParseCertificate(block.Bytes)
}

// certCoversWildcard reports whether cert carries the "*.<domain>" SAN that
// servesHostFromWildcard's decision depends on.
//
// It matches the WILDCARD SAN specifically, not whether some individual host
// happens to be listed. The hub omits the tls: block for every single-label
// host under the domain — including hives that do not exist yet — so only the
// wildcard entry justifies that. A certificate listing today's hosts explicitly
// would satisfy a per-host check and break the next hive provisioned.
func certCoversWildcard(cert *x509.Certificate, domain string) bool {
	domain = normalizeDNSName(domain)
	if cert == nil || domain == "" {
		return false
	}
	want := "*." + domain
	for _, name := range cert.DNSNames {
		if normalizeDNSName(name) == want {
			return true
		}
	}
	return false
}

// summarizeWildcardTLS turns a parsed certificate into the report.
//
// PURE — now and the warn window are parameters, so every boundary below is
// testable without waiting for one.
//
// THE ORDER OF THE CHECKS IS THE POINT. Coverage is judged BEFORE expiry,
// because a certificate that does not cover the domain is already failing every
// request on the cluster and how long it has left to do that is not the
// headline. An expired certificate is likewise reported as expired rather than
// as "expiring in -3 days".
func summarizeWildcardTLS(cert *x509.Certificate, secretRef, domain string, now time.Time, warnWindow time.Duration) WildcardTLSReport {
	rep := WildcardTLSReport{Secret: secretRef}
	if cert == nil {
		rep.Status = wildcardStatusUnreadable
		rep.Detail = "the secret holds no readable certificate"
		return rep
	}

	rep.NotAfter = cert.NotAfter.UTC().Format(time.RFC3339)
	rep.Issuer = cert.Issuer.CommonName
	rep.DNSNames = append([]string(nil), cert.DNSNames...)
	rep.CoversClusterDomain = certCoversWildcard(cert, domain)

	// Whole days, truncated toward zero the way a human reads "3 days left".
	// Computed for every status, so an operator sees the clock even on a
	// mismatch.
	days := int(cert.NotAfter.Sub(now) / (24 * time.Hour))
	rep.DaysRemaining = &days

	switch {
	case !rep.CoversClusterDomain:
		rep.Status = wildcardStatusDomainMismatch
		rep.Detail = "the certificate does not carry *." + normalizeDNSName(domain) +
			", so spokes whose tls: block was omitted for this domain are not served by it"
	case !now.Before(cert.NotAfter):
		rep.Status = wildcardStatusExpired
		rep.Detail = "the certificate expired — every wildcard-served spoke on this cluster is failing TLS validation"
	case cert.NotAfter.Sub(now) < warnWindow:
		rep.Status = wildcardStatusExpiring
		rep.Detail = "renewal is overdue: cert-manager begins renewing well before this point, so the DNS-01 path or the ACME quota needs checking"
	default:
		rep.Status = wildcardStatusOK
	}
	return rep
}

// splitSecretRef splits a "namespace/name" reference.
//
// Both halves must be non-empty. A bare name would be looked up in whatever
// namespace kubectl defaults to, which is not the operator's stated intent and
// could silently read a DIFFERENT secret that happens to share the name.
func splitSecretRef(ref string) (namespace, name string, ok bool) {
	parts := strings.Split(strings.TrimSpace(ref), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	namespace = strings.TrimSpace(parts[0])
	name = strings.TrimSpace(parts[1])
	if namespace == "" || name == "" {
		return "", "", false
	}
	return namespace, name, true
}

func ingressNginxDefaultSSLArgsCover(args, secretRef string) bool {
	fields := strings.Fields(args)
	for i, arg := range fields {
		if strings.TrimSpace(arg) == "--default-ssl-certificate="+secretRef {
			return true
		}
		if strings.TrimSpace(arg) == "--default-ssl-certificate" && i+1 < len(fields) && strings.TrimSpace(fields[i+1]) == secretRef {
			return true
		}
	}
	return false
}

// kubectlSaysNotFound reports whether a failed kubectl invocation failed
// because the object does not exist, rather than because the cluster could not
// be reached.
//
// It reads the stderr captured on exec.ExitError (Output() populates it). This
// is string matching on a CLI's output, which is not a contract — so it is
// written to fail toward UNKNOWN: anything unrecognised is treated as an
// unreachable cluster and reported as no data, never as a missing certificate.
// Being wrong that way costs a warning that should have been louder; being
// wrong the other way tells an operator their certificate vanished when the
// truth was a network blip.
func kubectlSaysNotFound(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	stderr := strings.ToLower(string(exitErr.Stderr))
	return strings.Contains(stderr, "notfound") || strings.Contains(stderr, "not found")
}

// collectWildcardTLSHealth reads the cluster's asserted wildcard secret and
// summarizes it.
//
// Returns nil — the signal is omitted entirely — when there is nothing to say:
//
//   - the cluster has not opted in. Its spokes keep per-host certificates, so
//     no wildcard is load-bearing there and a report would be noise.
//   - OpenShift Route clusters, which never used this path. The same veto
//     servesHostFromWildcard applies, restated here so the two cannot drift
//     into disagreeing about which clusters this feature touches.
//   - kubectl cannot reach the cluster, so the hub genuinely does not know.
//
// A secret that is ABSENT is emphatically not one of those cases: it returns a
// wildcardStatusMissing report, because on an opted-in cluster that is the
// loudest finding this file can produce.
func collectWildcardTLSHealth(ctx context.Context, cluster *ClusterConfig, timeout time.Duration, now time.Time, logger *slog.Logger) *WildcardTLSReport {
	if cluster == nil || !cluster.KubectlReachable() {
		return nil
	}
	secretRef := strings.TrimSpace(cluster.WildcardTLSSecret)
	if secretRef == "" || cluster.IngressType == ingressTypeOpenShiftRoute {
		return nil
	}
	namespace, name, ok := splitSecretRef(secretRef)
	if !ok {
		if logger != nil {
			logger.Warn("wildcard TLS check: malformed wildcard_tls_secret — spokes on this cluster omit their tls: block on the strength of this value",
				"cluster", cluster.ID, "wildcard_tls_secret", secretRef)
		}
		return &WildcardTLSReport{
			Secret: secretRef,
			Status: wildcardStatusUnreadable,
			Detail: `wildcard_tls_secret is not in "namespace/name" form, so the hub cannot look it up`,
		}
	}

	args, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", timeout.String(),
		"get", "deployment", "ingress-nginx-controller", "-n", "ingress-nginx",
		"-o", "jsonpath={.spec.template.spec.containers[*].args[*]}").Output()
	if err != nil {
		if kubectlSaysNotFound(err) {
			return &WildcardTLSReport{
				Secret: secretRef,
				Status: wildcardStatusNotServed,
				Detail: "the cluster opted in to wildcard TLS but the ingress-nginx controller deployment was not found, so the hub cannot prove the wildcard is served by default",
			}
		}
		if logger != nil {
			logger.Warn("wildcard TLS check: could not read ingress-nginx controller args — reporting UNKNOWN, not healthy",
				"cluster", cluster.ID, "secret", secretRef, "error", err)
		}
		return nil
	}
	if !ingressNginxDefaultSSLArgsCover(string(args), secretRef) {
		return &WildcardTLSReport{
			Secret: secretRef,
			Status: wildcardStatusNotServed,
			Detail: "the cluster opted in to wildcard TLS but ingress-nginx does not advertise --default-ssl-certificate=" + secretRef + ", so spokes without tls blocks may be served the controller's self-signed certificate",
		}
	}

	out, err := kubectlForClusterContext(ctx, cluster,
		"--request-timeout", timeout.String(),
		"get", "secret", name, "-n", namespace, "-o", "json").Output()
	if err != nil {
		// Usually the secret being absent, which on an opted-in cluster is the
		// critical finding — but an unreachable API server, an expired
		// kubeconfig and an RBAC denial all fail here too, and those must not be
		// reported as "your certificate is gone". kubectlSaysNotFound draws that
		// line and defaults to unknown.
		if kubectlSaysNotFound(err) {
			return &WildcardTLSReport{
				Secret: secretRef,
				Status: wildcardStatusMissing,
				Detail: "the cluster opted in to wildcard TLS but this secret does not exist — spokes provisioned without a tls: block are being served the ingress controller's self-signed certificate",
			}
		}
		if logger != nil {
			logger.Warn("wildcard TLS check: could not read the wildcard secret — reporting UNKNOWN, not healthy",
				"cluster", cluster.ID, "secret", secretRef, "error", err)
		}
		return nil
	}

	cert, perr := secretTLSCert(out)
	if perr != nil {
		return &WildcardTLSReport{
			Secret: secretRef,
			Status: wildcardStatusUnreadable,
			Detail: "the secret exists but its tls.crt could not be parsed: " + perr.Error(),
		}
	}
	rep := summarizeWildcardTLS(cert, secretRef, cluster.Domain, now, wildcardExpiryWarnWindow)
	rep.DefaultSSLConfigured = true
	return &rep
}
