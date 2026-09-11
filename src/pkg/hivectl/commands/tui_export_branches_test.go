package commands

// Coverage for exportCachedSessionForTUI's degradation branches (tui.go),
// which login_test.go's happy-path and precedence tests do not reach: the
// unlocatable session store, the HIVE_DASHBOARD_URL→DefaultBaseURL fallback,
// and the empty cache. Every one of these must degrade to "the TUI starts
// with whatever credentials the environment carries" — never an error, never
// a clobbered HIVE_DASHBOARD_COOKIE.

import (
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	tuiclient "github.com/hivecommons/hive/pkg/tui/client"
)

// TestTUIExportDefaultsBaseURLWhenEnvUnset: with HIVE_DASHBOARD_URL unset the
// cache must be keyed by the TUI's DefaultBaseURL — the URL the TUI will
// actually dial — so a plain `hivectl login` still hands off to a plain
// `hivectl tui`.
func TestTUIExportDefaultsBaseURLWhenEnvUnset(t *testing.T) {
	store := isolatedStore(t)
	t.Setenv(tuiclient.BaseURLEnv, "")
	t.Setenv(hivectl.CookieEnv, "")

	if err := store.Save(tuiclient.DefaultBaseURL, hivectl.Session{
		Cookie: "hive_session=default-url", ObtainedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	exportCachedSessionForTUI()
	if got := os.Getenv(hivectl.CookieEnv); got != "hive_session=default-url" {
		t.Errorf("%s = %q, want the session cached under DefaultBaseURL exported", hivectl.CookieEnv, got)
	}
}

// TestTUIExportNoCachedSessionLeavesEnvUntouched: an empty cache is the
// ordinary "never logged in" state; the export must leave the cookie variable
// exactly as it found it.
func TestTUIExportNoCachedSessionLeavesEnvUntouched(t *testing.T) {
	isolatedStore(t) // fresh, empty cache
	t.Setenv(tuiclient.BaseURLEnv, "http://localhost:39999")
	t.Setenv(hivectl.CookieEnv, "")

	exportCachedSessionForTUI()
	if got := os.Getenv(hivectl.CookieEnv); got != "" {
		t.Errorf("%s = %q, want empty when the cache holds no session", hivectl.CookieEnv, got)
	}
}

// TestTUIExportUnlocatableStoreDegrades: when the session cache location
// cannot even be determined (no XDG_CONFIG_HOME, no HOME), the export must
// silently degrade rather than fail — the TUI's own preflight explains any
// resulting rejection.
func TestTUIExportUnlocatableStoreDegrades(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv(hivectl.CookieEnv, "")

	exportCachedSessionForTUI()
	if got := os.Getenv(hivectl.CookieEnv); got != "" {
		t.Errorf("%s = %q, want empty when the session store is unlocatable", hivectl.CookieEnv, got)
	}
}
