// Package posture checks the containment hive and its sandboxes assume
// (#19458): no_new_privs set, capabilities dropped, a seccomp filter and,
// where asked, a read-only root filesystem. It reports gaps; whether a gap is
// fatal is the caller's choice, driven by RequireHardenedEnv.
package posture

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

// RequireHardenedEnv turns any containment gap into a startup failure when
// set to a truthy value (1, true, yes, on).
const RequireHardenedEnv = "HIVE_REQUIRE_HARDENED"

// Status is the containment-relevant part of /proc/self/status.
type Status struct {
	NoNewPrivs    bool
	HasNoNewPrivs bool
	CapEff        uint64
	HasCapEff     bool
	Seccomp       int
	HasSeccomp    bool
}

// ParseStatus reads /proc/self/status formatted input.
func ParseStatus(r io.Reader) (Status, error) {
	var st Status
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "NoNewPrivs":
			n, err := strconv.Atoi(val)
			if err != nil {
				return st, fmt.Errorf("posture: NoNewPrivs %q: %w", val, err)
			}
			st.NoNewPrivs, st.HasNoNewPrivs = n != 0, true
		case "CapEff":
			n, err := strconv.ParseUint(val, 16, 64)
			if err != nil {
				return st, fmt.Errorf("posture: CapEff %q: %w", val, err)
			}
			st.CapEff, st.HasCapEff = n, true
		case "Seccomp":
			n, err := strconv.Atoi(val)
			if err != nil {
				return st, fmt.Errorf("posture: Seccomp %q: %w", val, err)
			}
			st.Seccomp, st.HasSeccomp = n, true
		}
	}
	return st, sc.Err()
}

// Gaps lists the containment the status lacks. A field the kernel did not
// report is a gap too, since it cannot be verified. When checkRootFS is true,
// rootWritable reports whether the root filesystem is writable.
func Gaps(st Status, checkRootFS, rootWritable bool) []string {
	var gaps []string
	if !st.HasNoNewPrivs || !st.NoNewPrivs {
		gaps = append(gaps, "no_new_privs is not set (run with --security-opt=no-new-privileges)")
	}
	if !st.HasCapEff || st.CapEff != 0 {
		gaps = append(gaps, fmt.Sprintf("effective capabilities are not empty (CapEff=%x; run with --cap-drop=all)", st.CapEff))
	}
	if !st.HasSeccomp || st.Seccomp == 0 {
		gaps = append(gaps, "no seccomp filter is active")
	}
	if checkRootFS && rootWritable {
		gaps = append(gaps, "root filesystem is writable (run with --read-only)")
	}
	return gaps
}

// RequireHardened reports whether getenv asks for gaps to be fatal.
func RequireHardened(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(RequireHardenedEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Boot checks this process, logs every gap as a warning and returns an error
// when RequireHardenedEnv is set and any gap exists.
func Boot(logger *slog.Logger, getenv func(string) string) error {
	gaps := Check()
	for _, g := range gaps {
		logger.Warn("containment gap", "gap", g)
	}
	if len(gaps) > 0 && RequireHardened(getenv) {
		return fmt.Errorf("posture: %d containment gap(s) and %s is set: %s", len(gaps), RequireHardenedEnv, strings.Join(gaps, "; "))
	}
	return nil
}
