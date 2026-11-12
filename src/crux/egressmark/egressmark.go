// Package egressmark stamps the hive's OWN outbound sockets with the fwmark
// (SO_MARK) that the entrypoint's forced-egress iptables rule exempts
// (`-m mark --mark <mark> -j RETURN`), so control-plane traffic the hive
// itself originates is never redirected into its own MITM proxy.
//
// It is shared by the proxy's upstream dials and by every in-cluster
// Kubernetes API client (self-upgrade, startup-probe backfill, node metrics,
// ingress probes, kubejob). The entrypoint also exempts the hive's uid with
// `-m owner`, but that match needs the xt_owner kernel module, which IBM Cloud
// IKS/ROKS and other OVN nodes do not load: there the mark is the ONLY
// exemption, and an unmarked dial to kubernetes.default.svc:443 lands in the
// proxy, whose private-range forward guard closes it ("EOF" / "unexpected eof
// while reading"), failing every spoke self-upgrade on that cluster.
package egressmark

import (
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultMark is the fwmark the entrypoint exempts when
	// HIVE_PROXY_EGRESS_MARK is unset. entrypoint.sh is the single source of
	// truth and exports the same value into the hive process.
	DefaultMark = 0x1112

	// Env is the variable entrypoint.sh exports so the Go side and the
	// iptables rule stay in lockstep.
	Env = "HIVE_PROXY_EGRESS_MARK"
)

// Mark returns the fwmark to stamp on the hive's own dials, read from
// HIVE_PROXY_EGRESS_MARK (hex with 0x prefix or decimal). It falls back to
// DefaultMark when unset or unparseable: that default matches the entrypoint
// default, so accidental drift stays on the agreed value rather than silently
// disabling the exemption.
func Mark() int {
	raw := strings.TrimSpace(os.Getenv(Env))
	if raw == "" {
		return DefaultMark
	}
	if v, err := strconv.ParseInt(raw, 0, 64); err == nil && v > 0 {
		return int(v)
	}
	return DefaultMark
}

// Dialer returns a *net.Dialer whose Control hook stamps Mark() on the socket
// before connect. The optional timeout is applied when non-zero.
//
// The mark is BEST-EFFORT: setsockopt(SO_MARK) needs CAP_NET_ADMIN in the
// effective set, and a hive running without it must still dial (on OKE the
// owner-uid exemption covers it). A mark failure is therefore logged once and
// the dial proceeds unmarked instead of aborting.
func Dialer(timeout time.Duration) *net.Dialer {
	mark := Mark()
	return &net.Dialer{
		Timeout: timeout,
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				if err := setSockMark(fd, mark); err != nil {
					warnOnce(mark, err)
				}
			})
		},
	}
}

var warnSync sync.Once

func warnOnce(mark int, err error) {
	warnSync.Do(func() {
		slog.Warn("egress SO_MARK unavailable — dialing unmarked (owner-UID exemption still applies where xt_owner is loaded); expected when the hive runs without CAP_NET_ADMIN in its effective set",
			"mark", mark, "error", err)
	})
}
