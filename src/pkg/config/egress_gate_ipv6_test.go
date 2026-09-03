package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
)

// These tests pin the IPv6 half of the forced-proxy egress gate (#4319).
//
// The gate's IPv4 redirect lives in entrypoint.sh's iptables block; before
// #4319 there was NO ip6tables path at all, so on any dual-stack network an
// agent resolving an AAAA record reached :443 over IPv6 without ever meeting
// the redirect — silently, because the IPv4 gate had installed successfully.
//
// Like entrypoint_boot_test.go, this extracts and EXECUTES the real gate block
// rather than grepping for rule text: shim iptables/ip6tables binaries record
// every invocation, so the assertions cover the branching logic the container
// actually runs, not a paraphrase of it. Live packet-level verification needs
// a Linux netns and is covered by src/deploy/probe_podman_ipv6_egress.sh.

// runEgressGate executes ONLY the iptables/ip6tables gate block of
// entrypoint.sh with shim netfilter binaries, returning the combined script
// output, the shim invocation log, and the exit code.
//
// shims maps binary names (for example "iptables-nft") to an exit-code policy:
// every invocation is appended to the log and succeeds, except `-nL <chain>`
// existence probes, which fail so the creation path runs. Binaries absent from
// the map are absent from PATH. ipv6Stack controls whether the block sees a
// kernel IPv6 stack (/proc/sys/net/ipv6 is rewritten onto a temp root).
func runEgressGate(t *testing.T, env map[string]string, shims []string, ipv6Stack bool) (out string, calls string, exitCode int) {
	t.Helper()
	src, err := os.ReadFile(entrypointPath)
	if err != nil {
		testutil.SkipfUnlessRequired(t, "entrypoint.sh not readable from this package: %v", err)
	}
	text := string(src)
	start := strings.Index(text, "PROXY_PORT=18443")
	if start < 0 {
		t.Fatal("could not find the start of the egress gate block (PROXY_PORT=18443); the marker moved and this test would silently cover nothing")
	}
	end := strings.Index(text, "# Drop to non-root user")
	if end < 0 || end < start {
		t.Fatal("could not find the end of the egress gate block; the marker moved and this test would silently cover nothing")
	}
	body := text[start:end]
	// The block sits inside an enclosing `if` whose opener is above the
	// extraction start; drop the one unmatched trailing `fi`.
	body = strings.TrimRight(body, " \t\n")
	if !strings.HasSuffix(body, "fi") {
		t.Fatalf("gate block does not end with the enclosing fi; got tail %q", body[len(body)-20:])
	}
	body = strings.TrimRight(strings.TrimSuffix(body, "fi"), " \t\n")

	root := t.TempDir()
	// Rewrite absolute paths onto the temp root so the test never touches the
	// real /proc or /tmp, and so the IPv6-stack presence check is controllable.
	body = strings.ReplaceAll(body, "/proc/sys/net/ipv6", root+"/proc/sys/net/ipv6")
	// Routability is probed via these two files when `ip` is absent. Rewrite
	// them onto the temp root as well, so the probe is decided by the fixture
	// rather than by the host: without this the same test passes on a machine
	// with no /proc (macOS, where the fail-safe branch runs) and fails on an
	// IPv4-only Linux CI runner, which is exactly the drift that let the
	// unroutable-IPv6 case reach production.
	body = strings.ReplaceAll(body, "/proc/net/if_inet6", root+"/proc/net/if_inet6")
	body = strings.ReplaceAll(body, "/proc/net/ipv6_route", root+"/proc/net/ipv6_route")
	body = strings.ReplaceAll(body, "/tmp/hive-ipt-err.log", root+"/hive-ipt-err.log")
	body = strings.ReplaceAll(body, "/tmp/hive-ip6t-err.log", root+"/hive-ip6t-err.log")
	if ipv6Stack {
		if err := os.MkdirAll(filepath.Join(root, "proc/sys/net/ipv6"), 0o755); err != nil {
			t.Fatal(err)
		}
		// A stack-present fixture is ROUTABLE by default: a global-scope
		// address (if_inet6 scope field 00) and a ::/0 default route
		// (ipv6_route destination prefix length 00). Tests that want the
		// unroutable pod pass the "ip-noipv6" shim, which overrides this by
		// putting an `ip` on PATH that reports neither.
		if err := os.MkdirAll(filepath.Join(root, "proc/net"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "proc/net/if_inet6"),
			[]byte("20010db8000000000000000000000001 02 40 00 80 eth0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "proc/net/ipv6_route"),
			[]byte("00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000000 00000000 00000003 eth0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	callLog := filepath.Join(root, "calls.log")
	shim := "#!/bin/sh\n" +
		"echo \"$(basename \"$0\") $*\" >> " + callLog + "\n" +
		"# Chain-existence probes must fail so the creation path is exercised.\n" +
		"case \"$*\" in *-nL*) exit 1;; esac\n" +
		"exit 0\n"
	for _, name := range shims {
		// "ip-noipv6" is not a netfilter shim: it installs an `ip` that reports
		// neither a global IPv6 address nor a default route, so the routability
		// probe sees an IPv4-only pod. Empty output + exit 0 is exactly what
		// the real `ip -6 addr show scope global` prints there.
		if name == "ip-noipv6" {
			if err := os.WriteFile(filepath.Join(binDir, "ip"),
				[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The routability probe prefers `ip` and only falls back to /proc, so the
	// host's real `ip` would decide the result on any runner that has one —
	// which is why these tests passed on macOS (no `ip`, no /proc, fail-safe
	// branch) and failed on the IPv4-only Linux CI runner. Always install an
	// `ip` shim so the fixture decides on every platform: routable by default
	// for a stack-present pod, overridden to report nothing by "ip-noipv6".
	if ipv6Stack {
		hasIPShim := false
		for _, n := range shims {
			if n == "ip-noipv6" {
				hasIPShim = true
			}
		}
		if !hasIPShim {
			if err := os.WriteFile(filepath.Join(binDir, "ip"), []byte(
				"#!/bin/sh\n"+
					"# routable fixture: a global address and a default route\n"+
					"case \"$*\" in\n"+
					"  *addr*) echo '    inet6 2001:db8::1/64 scope global';;\n"+
					"  *route*) echo 'default via fe80::1 dev eth0 metric 1024';;\n"+
					"esac\n"+
					"exit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	// python3 (uid-map bookkeeping) and sleep (retry backoff) as no-ops, and a
	// real basename for the shim itself.
	for name, script := range map[string]string{
		"python3": "#!/bin/sh\nexit 0\n",
		"sleep":   "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	prelude := "set -e\n" +
		"PATH=" + binDir + ":/usr/bin:/bin\n" +
		"EXIT_NET_ADMIN_REQUIRED=77\n" +
		"_cap_net_admin_in_bset=true\n" +
		"PROXY_UID=1001\n" +
		"HIVE_PROXY_EGRESS_MARK=0x1112\n"
	cmd := exec.Command("sh", "-c", prelude+body)
	cmd.Env = append(os.Environ(), "HIVE_PROXY_ADVISORY_OK=false")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	outB, _ := cmd.CombinedOutput()
	callsB, _ := os.ReadFile(callLog)
	return string(outB), string(callsB), cmd.ProcessState.ExitCode()
}

// TestGateInstallsBothFamilies: with iptables AND ip6tables available and an
// IPv6 stack present, the gate must establish BOTH families — the IPv4
// redirect (re-confirmed per #4319's acceptance criteria) and the IPv6 REJECT
// — and start cleanly.
func TestGateInstallsBothFamilies(t *testing.T) {
	out, calls, code := runEgressGate(t, nil,
		[]string{"iptables-nft", "ip6tables-nft"}, true)
	if code != 0 {
		t.Fatalf("gate exited %d with both families available:\n%s", code, out)
	}
	// IPv4 redirect re-confirmed in the same run.
	if !strings.Contains(calls, "iptables-nft -t nat -A HIVE_PROXY -p tcp --dport 443 -j REDIRECT --to-ports 18443") {
		t.Fatalf("IPv4 :443 redirect was not installed:\n%s", calls)
	}
	// IPv6 family closed with the same three exemptions, in order, BEFORE the
	// REJECT, and the chain hooked into OUTPUT.
	wantSeq := []string{
		"ip6tables-nft -w 10 -N HIVE_PROXY6",
		"ip6tables-nft -A HIVE_PROXY6 -m owner --uid-owner 0 -j RETURN",
		"ip6tables-nft -A HIVE_PROXY6 -m owner --uid-owner 1001 -j RETURN",
		"ip6tables-nft -A HIVE_PROXY6 -m mark --mark 0x1112 -j RETURN",
		"ip6tables-nft -A HIVE_PROXY6 -p tcp --dport 443 -j REJECT --reject-with tcp-reset",
		"ip6tables-nft -A OUTPUT -j HIVE_PROXY6",
	}
	pos := -1
	for _, want := range wantSeq {
		i := strings.Index(calls, want)
		if i < 0 {
			t.Fatalf("missing ip6tables invocation %q:\n%s", want, calls)
		}
		if i < pos {
			t.Fatalf("ip6tables invocation %q out of order (exemptions must precede the REJECT):\n%s", want, calls)
		}
		pos = i
	}
	if !strings.Contains(out, "outbound IPv6 :443 REJECTed") {
		t.Fatalf("IPv6 gate success was not logged:\n%s", out)
	}
	if strings.Contains(out, "FATAL") {
		t.Fatalf("unexpected FATAL with both families established:\n%s", out)
	}
}

// TestGateFailsClosedWithoutIP6Tables: an IPv6-capable kernel with no
// ip6tables binary means the IPv6 family CANNOT be gated — that is exactly the
// silent bypass #4319 describes, so the container must refuse to start (the
// same F5 fail-closed treatment the IPv4 redirect gets), not warn and proceed.
func TestGateFailsClosedWithoutIP6Tables(t *testing.T) {
	out, _, code := runEgressGate(t, nil, []string{"iptables-nft"}, true)
	if code == 0 {
		t.Fatalf("gate started with IPv6 unenforced (exit 0):\n%s", out)
	}
	if !strings.Contains(out, "ip6tables not found") {
		t.Fatalf("missing ip6tables was not named as the cause:\n%s", out)
	}
	if !strings.Contains(out, "FATAL") {
		t.Fatalf("no FATAL for an ungated IPv6 family:\n%s", out)
	}
}

// TestGateAdvisoryOptOutCoversIPv6: HIVE_PROXY_ADVISORY_OK=true is the single
// deliberate escape hatch; it must cover an IPv6-gate failure the same way it
// covers an IPv4 one — with the loud ADVISORY-ONLY warning, never silently.
func TestGateAdvisoryOptOutCoversIPv6(t *testing.T) {
	out, _, code := runEgressGate(t,
		map[string]string{"HIVE_PROXY_ADVISORY_OK": "true"},
		[]string{"iptables-nft"}, true)
	if code != 0 {
		t.Fatalf("advisory opt-in still exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ADVISORY-ONLY") {
		t.Fatalf("advisory mode was entered without the ADVISORY-ONLY warning:\n%s", out)
	}
}

// TestGatePassesVacuouslyWithoutIPv6Stack: a kernel with IPv6 compiled out or
// disabled at boot (/proc/sys/net/ipv6 absent) cannot carry IPv6 traffic, so
// there is nothing to gate; requiring ip6tables there would fail containers
// that have no bypass to close.
func TestGatePassesVacuouslyWithoutIPv6Stack(t *testing.T) {
	out, calls, code := runEgressGate(t, nil, []string{"iptables-nft"}, false)
	if code != 0 {
		t.Fatalf("gate exited %d on an IPv6-less kernel:\n%s", code, out)
	}
	if !strings.Contains(out, "IPv6 stack absent") {
		t.Fatalf("the vacuous pass was not logged:\n%s", out)
	}
	if regexp.MustCompile(`(?m)^ip6tables`).MatchString(calls) {
		t.Fatalf("ip6tables was invoked despite no IPv6 stack:\n%s", calls)
	}
}

// TestGatePassesVacuouslyWithoutRoutableIPv6 pins the production outage this
// check exists for: a pod CAN have the IPv6 stack compiled in — /proc/sys/net/
// ipv6 present, ::1 up for the gateway nginx and the proxy's /proc/net/tcp6
// self-lookup — while having NO global IPv6 address and NO default route. It
// cannot originate IPv6 egress, so there is no bypass to close.
//
// Fail-closing there crash-looped a healthy hive (exit 4) on an IPv4-only
// OpenShift cluster whose nodes ALSO lack the ip6tables `owner`/`REJECT`
// extensions, so the gate could not have been installed even in principle.
// Stack presence is not the property that decides whether a bypass is
// reachable; routability is.
func TestGatePassesVacuouslyWithoutRoutableIPv6(t *testing.T) {
	// `ip` present but reporting neither a global address nor a default route
	// — exactly what `ip -6 addr show scope global` / `ip -6 route show
	// default` print on the affected pods.
	out, calls, code := runEgressGate(t, nil,
		[]string{"iptables-nft", "ip6tables-nft", "ip-noipv6"}, true)
	if code != 0 {
		t.Fatalf("gate exited %d on a pod with no routable IPv6 — this is the certus crash-loop:\n%s", code, out)
	}
	if !strings.Contains(out, "not routable") {
		t.Fatalf("the vacuous pass for unroutable IPv6 was not logged:\n%s", out)
	}
	if regexp.MustCompile(`(?m)^ip6tables`).MatchString(calls) {
		t.Fatalf("ip6tables was invoked despite no routable IPv6 egress:\n%s", calls)
	}
}
