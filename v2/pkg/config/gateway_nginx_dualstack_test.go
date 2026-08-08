package config

import (
	"os"
	"regexp"
	"testing"
)

// nginxConfPath is the gateway config under test, relative to this package.
// It is bind-mounted over /etc/nginx/nginx.conf by v2/docker-compose.yaml.
const nginxConfPath = "../../deploy/nginx.conf"

// Ports that the gateway server blocks listen on. Named rather than inlined so
// a port change forces a deliberate edit here instead of silently un-covering
// a block.
const (
	gatewayAPIPort  = "3001"
	gatewayTTYDPort = "7681"
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

	for _, port := range []string{gatewayAPIPort, gatewayTTYDPort} {
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
