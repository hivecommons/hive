package config

import (
	"os"
	"regexp"
	"testing"
)

// nginxConfPath is the gateway config under test, relative to this package.
// It is bind-mounted over /etc/nginx/nginx.conf by src/docker-compose.yaml.
const nginxConfPath = "../../deploy/nginx.conf"

// Ports that the gateway server blocks listen on. Named rather than inlined so
// a port change forces a deliberate edit here instead of silently un-covering
// a block.
const (
	gatewayAPIPort = "3001"
	// NOTE (v4): the raw ttyd :7681 stream proxy was deliberately REMOVED from
	// nginx.conf — ttyd is only reachable through the Node proxy's
	// authenticated /terminal route. Only the API port is dual-stack-pinned.
)

// readNginxConf returns the gateway nginx config text.
func readNginxConf(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(nginxConfPath)
	if err != nil {
		t.Skipf("nginx.conf not readable from this package: %v", err)
	}
	return string(src)
}

// TestGatewayListensDualStack pins the fix for issue #2774.
//
// The gateway healthcheck (and any IPv6 client) resolves "localhost" to ::1 on
// a dual-stack host. This file is mounted over /etc/nginx/nginx.conf and does
// NOT include /etc/nginx/conf.d/*.conf, so the nginx:alpine
// 10-listen-on-ipv6-by-default.sh fixup never applies to these server blocks.
// If the `listen [::]:PORT;` directives are dropped, nothing binds the IPv6
// loopback and the probe gets ECONNREFUSED forever.
func TestGatewayListensDualStack(t *testing.T) {
	conf := readNginxConf(t)

	for _, port := range []string{gatewayAPIPort} {
		t.Run("port_"+port, func(t *testing.T) {
			// IPv4 bind must remain.
			v4 := regexp.MustCompile(`(?m)^\s*listen\s+` + port + `\s*;`)
			if !v4.MatchString(conf) {
				t.Errorf("nginx.conf lost the IPv4 `listen %s;` directive", port)
			}
			// IPv6 bind must be present alongside it.
			v6 := regexp.MustCompile(`(?m)^\s*listen\s+\[::\]:` + port + `\s*;`)
			if !v6.MatchString(conf) {
				t.Errorf("nginx.conf is missing `listen [::]:%s;` -- IPv6 loopback "+
					"is unbound, so healthchecks resolving localhost to ::1 will "+
					"fail forever (issue #2774)", port)
			}
		})
	}
}

// TestGatewayConfDoesNotRelyOnConfD guards the reason the upstream nginx:alpine
// IPv6 fixup does not save us: that script only patches conf.d/default.conf. If
// someone later adds an `include /etc/nginx/conf.d/*.conf;` here, the explicit
// listen directives above stop being the only thing standing between us and a
// silent regression, and this test's rationale should be revisited.
func TestGatewayConfDoesNotRelyOnConfD(t *testing.T) {
	conf := readNginxConf(t)
	includeConfD := regexp.MustCompile(`(?m)^\s*include\s+/etc/nginx/conf\.d/`)
	if includeConfD.MatchString(conf) {
		t.Skip("nginx.conf now includes conf.d; the upstream IPv6 fixup may apply " +
			"and the dual-stack rationale in TestGatewayListensDualStack needs review")
	}
}

// gatewaySecurityHeaders pins the defence-in-depth headers added for issue
// #3822. Each entry is a directive regex (must appear at least once per
// `location` block, i.e. at least twice total in this file's two proxied
// locations) paired with a human label for failure messages.
//
// X-Frame-Options and Content-Security-Policy are deliberately NOT included
// here: the Go dashboard (pkg/dashboard/server.go) and Node proxy (src/proxy/server.js)
// own CSP and XFO for every route they serve (DENY, or omitted in favour of a CSP
// frame-ancestors allowlist for /api/snapshot/frame-ancestors). Those headers are
// tailored per document and evolve independently of this file (see each emitter's
// own CSP-construction code for current specifics). A blanket nginx CSP or XFO
// would collide with upstream per-document logic through browser header intersection
// (issues #3315, #3822, #3941).
// Strict-Transport-Security is also excluded: this file has no `ssl`/`443`
// directive, so nginx never terminates TLS here, and asserting HSTS would
// pin a header that describes a connection this config doesn't make.
var gatewaySecurityHeaders = []struct {
	label     string
	directive *regexp.Regexp
}{
	{
		label:     `X-Content-Type-Options: nosniff`,
		directive: regexp.MustCompile(`(?m)^\s*add_header\s+X-Content-Type-Options\s+"nosniff"\s+always;`),
	},
	{
		label:     `Referrer-Policy: strict-origin-when-cross-origin`,
		directive: regexp.MustCompile(`(?m)^\s*add_header\s+Referrer-Policy\s+"strict-origin-when-cross-origin"\s+always;`),
	},
	{
		label:     `Permissions-Policy`,
		directive: regexp.MustCompile(`(?m)^\s*add_header\s+Permissions-Policy\s+"[^"]+"\s+always;`),
	},
}

// gatewayProxiedLocationCount is the number of `location` blocks in
// nginx.conf that proxy to hive_api and therefore must carry the
// defence-in-depth headers (/api/ and /). The @api_error location returns a
// static JSON body directly, not a proxied response, so it is not counted.
const gatewayProxiedLocationCount = 2

// TestGatewaySecurityHeadersPresent pins the fix for issue #3822: nginx.conf
// must add defence-in-depth headers on every proxied response, not just
// Cache-Control/Pragma. Requiring gatewayProxiedLocationCount occurrences
// (not just >=1) ensures a future edit that trims the header from one
// location while leaving it in another still fails loudly instead of passing
// on a technicality.
func TestGatewaySecurityHeadersPresent(t *testing.T) {
	conf := readNginxConf(t)

	for _, h := range gatewaySecurityHeaders {
		t.Run(h.label, func(t *testing.T) {
			matches := h.directive.FindAllString(conf, -1)
			if len(matches) < gatewayProxiedLocationCount {
				t.Errorf("nginx.conf has %d occurrence(s) of `%s`, want at least %d "+
					"(one per proxied location) -- issue #3822 regressed",
					len(matches), h.label, gatewayProxiedLocationCount)
			}
		})
	}
}

// TestGatewayDoesNotSetFrameOptionsOrHSTS documents and guards a deliberate
// omission from TestGatewaySecurityHeadersPresent: X-Frame-Options/CSP belong
// to the Go dashboard's per-route logic (server.go) and HSTS is inapplicable
// because this file never terminates TLS. If either shows up here, that's a
// signal the rationale in the comment above gatewaySecurityHeaders needs a
// fresh look (e.g. TLS termination moved into this file), not a silent green.
func TestGatewayDoesNotSetFrameOptionsOrHSTS(t *testing.T) {
	conf := readNginxConf(t)

	if regexp.MustCompile(`(?m)^\s*add_header\s+X-Frame-Options\b`).MatchString(conf) {
		t.Error("nginx.conf now sets X-Frame-Options -- this would collide with " +
			"the Go dashboard's per-route XFO/CSP logic in pkg/dashboard/server.go; " +
			"revisit the rationale in gatewaySecurityHeaders before allowing this")
	}
	if regexp.MustCompile(`(?m)^\s*add_header\s+Strict-Transport-Security\b`).MatchString(conf) {
		t.Error("nginx.conf now sets Strict-Transport-Security but has no TLS " +
			"(ssl/443) directive -- HSTS from a plaintext listener is misleading; " +
			"if TLS termination moved into this file, HSTS is now appropriate and " +
			"this guard should be updated instead of just deleted")
	}
}
