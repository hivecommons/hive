package github

import (
	"os"
	"strings"
)

// proxyInjectGHAuthEnv and proxyInjectGHAuthEnabledValue mirror the constants
// of the same meaning in pkg/config. They are duplicated rather than imported
// so this package keeps no dependency on the application configuration surface
// (hivecommons/hive#5953 phase 1).
//
// This is the #1861 credential-divert switch, so the duplication is pinned by a
// BEHAVIOURAL test (config_parity_test.go) that runs both readers over the same
// env values rather than merely comparing two constants. The risk being guarded
// is failing open: a divergent reader returning false where config returns true
// would hand agents the real token while the operator believed the divert was
// on.
const (
	proxyInjectGHAuthEnv          = "HIVE_PROXY_INJECT_GH_AUTH"
	proxyInjectGHAuthEnabledValue = "true"
)

// proxyInjectGHAuth reports whether agent-facing credential files should
// receive a placeholder instead of the real token. Exact match after trimming,
// matching config.ProxyInjectGHAuth: "TRUE" and "1" are NOT enabled, which is
// the conservative reading for a security switch.
func proxyInjectGHAuth() bool {
	return strings.TrimSpace(os.Getenv(proxyInjectGHAuthEnv)) == proxyInjectGHAuthEnabledValue
}
