package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mustET returns a time in America/New_York. The clock is INJECTED into
// shouldAutoUpgradeNow, so none of these tests read the real wall clock and
// they behave identically regardless of the machine's TZ.
func mustET(t *testing.T, y int, mo time.Month, d, h, m int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(autoUpgradeTimezone)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v — tzdata should be embedded via time/tzdata", autoUpgradeTimezone, err)
	}
	return time.Date(y, mo, d, h, m, 0, 0, loc)
}

// TestTzdataIsEmbedded guards the Dockerfile.hub gap directly: alpine:3.21 does
// not ship the tzdata package, so without the `_ "time/tzdata"` import in
// upgrade_schedule.go this lookup fails in the container and every daily hive
// silently stops upgrading.
func TestTzdataIsEmbedded(t *testing.T) {
	loc, err := autoUpgradeLocation()
	if err != nil {
		t.Fatalf("autoUpgradeLocation() failed: %v", err)
	}
	if loc == nil || loc.String() != autoUpgradeTimezone {
		t.Fatalf("got location %v, want %s", loc, autoUpgradeTimezone)
	}
}

func TestShouldAutoUpgradeNow(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		lastFired   string
		now         time.Time
		wantAllowed bool
		wantFire    string
	}{
		{
			name:        "legacy record with empty mode behaves as instant",
			mode:        "",
			now:         mustET(t, 2026, time.July, 15, 3, 0),
			wantAllowed: true,
		},
		{
			name:        "explicit instant is never gated, even at 3am",
			mode:        AutoUpgradeModeInstant,
			now:         mustET(t, 2026, time.July, 15, 3, 0),
			wantAllowed: true,
		},
		{
			name:        "instant ignores a stale fire date",
			mode:        AutoUpgradeModeInstant,
			lastFired:   "2026-07-15",
			now:         mustET(t, 2026, time.July, 15, 18, 0),
			wantAllowed: true,
		},
		{
			name:        "daily before the window does not fire",
			mode:        AutoUpgradeModeDaily,
			now:         mustET(t, 2026, time.July, 15, autoUpgradeDailyHour-1, 59),
			wantAllowed: false,
		},
		{
			name:        "daily exactly at the window hour fires",
			mode:        AutoUpgradeModeDaily,
			now:         mustET(t, 2026, time.July, 15, autoUpgradeDailyHour, 0),
			wantAllowed: true,
			wantFire:    "2026-07-15",
		},
		{
			name:        "daily after the window fires",
			mode:        AutoUpgradeModeDaily,
			now:         mustET(t, 2026, time.July, 15, autoUpgradeDailyHour, 2),
			wantAllowed: true,
			wantFire:    "2026-07-15",
		},
		{
			name:        "daily already fired today does not fire again minutes later",
			mode:        AutoUpgradeModeDaily,
			lastFired:   "2026-07-15",
			now:         mustET(t, 2026, time.July, 15, autoUpgradeDailyHour, 4),
			wantAllowed: false,
		},
		{
			name:        "daily already fired today stays held late at night",
			mode:        AutoUpgradeModeDaily,
			lastFired:   "2026-07-15",
			now:         mustET(t, 2026, time.July, 15, 23, 59),
			wantAllowed: false,
		},
		{
			name:        "daily fires again the next day",
			mode:        AutoUpgradeModeDaily,
			lastFired:   "2026-07-15",
			now:         mustET(t, 2026, time.July, 16, autoUpgradeDailyHour, 0),
			wantAllowed: true,
			wantFire:    "2026-07-16",
		},
		{
			name:        "next day before the window still does not fire",
			mode:        AutoUpgradeModeDaily,
			lastFired:   "2026-07-15",
			now:         mustET(t, 2026, time.July, 16, 9, 0),
			wantAllowed: false,
		},
		{
			name:        "missed window does not accumulate — fires on the next cycle seen",
			mode:        AutoUpgradeModeDaily,
			lastFired:   "2026-07-13",
			now:         mustET(t, 2026, time.July, 15, 21, 30),
			wantAllowed: true,
			wantFire:    "2026-07-15",
		},
		{
			name:        "weekly on Monday does not fire",
			mode:        AutoUpgradeModeWeekly,
			now:         mustET(t, 2026, time.July, 13, 15, 0),
			wantAllowed: false,
		},
		{
			name:        "weekly on Tuesday before the hour does not fire",
			mode:        AutoUpgradeModeWeekly,
			now:         mustET(t, 2026, time.July, 14, autoUpgradeDailyHour-1, 59),
			wantAllowed: false,
		},
		{
			name:        "weekly on Tuesday at the hour fires",
			mode:        AutoUpgradeModeWeekly,
			now:         mustET(t, 2026, time.July, 14, autoUpgradeDailyHour, 0),
			wantAllowed: true,
			wantFire:    "2026-07-14",
		},
		{
			name:        "weekly missed Tuesday fires later the same week",
			mode:        AutoUpgradeModeWeekly,
			lastFired:   "2026-07-07",
			now:         mustET(t, 2026, time.July, 16, 9, 0),
			wantAllowed: true,
			wantFire:    "2026-07-16",
		},
		{
			name:        "weekly already fired this week stays held",
			mode:        AutoUpgradeModeWeekly,
			lastFired:   "2026-07-14",
			now:         mustET(t, 2026, time.July, 16, 15, 0),
			wantAllowed: false,
		},
		{
			name:        "weekly Sunday belongs to the same ISO week — still held",
			mode:        AutoUpgradeModeWeekly,
			lastFired:   "2026-07-14",
			now:         mustET(t, 2026, time.July, 19, 15, 0),
			wantAllowed: false,
		},
		{
			name:        "weekly Sunday with last week's fire date is an open window",
			mode:        AutoUpgradeModeWeekly,
			lastFired:   "2026-07-07",
			now:         mustET(t, 2026, time.July, 19, 15, 0),
			wantAllowed: true,
			wantFire:    "2026-07-19",
		},
		{
			name:        "weekly fires again the following Tuesday",
			mode:        AutoUpgradeModeWeekly,
			lastFired:   "2026-07-14",
			now:         mustET(t, 2026, time.July, 21, autoUpgradeDailyHour, 0),
			wantAllowed: true,
			wantFire:    "2026-07-21",
		},
		{
			name:        "weekly with an unparseable fire record fails open inside the window",
			mode:        AutoUpgradeModeWeekly,
			lastFired:   "not-a-date",
			now:         mustET(t, 2026, time.July, 15, 15, 0),
			wantAllowed: true,
			wantFire:    "2026-07-15",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldAutoUpgradeNow(tc.mode, tc.lastFired, tc.now)
			if got.Allowed != tc.wantAllowed {
				t.Fatalf("Allowed = %v, want %v (reason: %s)", got.Allowed, tc.wantAllowed, got.Reason)
			}
			if got.FireDate != tc.wantFire {
				t.Errorf("FireDate = %q, want %q", got.FireDate, tc.wantFire)
			}
			if got.Reason == "" {
				t.Error("Reason should always be populated")
			}
		})
	}
}

// TestShouldAutoUpgradeNowDST is the reason autoUpgradeTimezone is a ZONE NAME
// and not a fixed offset. On these dates a hardcoded UTC-5 or UTC-4 would put
// the daily boundary an hour off, firing early or holding a hive an extra day.
// Each case asserts on a wall-clock ET time that is only correct if the offset
// is resolved from tzdata.
func TestShouldAutoUpgradeNowDST(t *testing.T) {
	tests := []struct {
		name        string
		now         time.Time
		wantOffset  int // seconds east of UTC, proving which side of DST we are on
		wantAllowed bool
	}{
		{
			// Spring forward 2026: DST begins Sunday March 8. The day before is
			// still EST (UTC-5).
			name:        "day before spring forward is EST, the minute before holds",
			now:         mustET(t, 2026, time.March, 7, autoUpgradeDailyHour-1, 59),
			wantOffset:  -5 * 60 * 60,
			wantAllowed: false,
		},
		{
			name:        "day before spring forward is EST, the window fires",
			now:         mustET(t, 2026, time.March, 7, autoUpgradeDailyHour, 0),
			wantOffset:  -5 * 60 * 60,
			wantAllowed: true,
		},
		{
			// After the transition the same wall-clock hour is EDT (UTC-4).
			// A hardcoded -5 offset would treat this instant as 16:00 and hold.
			name:        "day after spring forward is EDT, the window still fires",
			now:         mustET(t, 2026, time.March, 9, autoUpgradeDailyHour, 0),
			wantOffset:  -4 * 60 * 60,
			wantAllowed: true,
		},
		{
			name:        "day after spring forward is EDT, the minute before still holds",
			now:         mustET(t, 2026, time.March, 9, autoUpgradeDailyHour-1, 59),
			wantOffset:  -4 * 60 * 60,
			wantAllowed: false,
		},
		{
			// Fall back 2026: DST ends Sunday November 1. The day before is EDT.
			name:        "day before fall back is EDT, the window fires",
			now:         mustET(t, 2026, time.October, 31, autoUpgradeDailyHour, 0),
			wantOffset:  -4 * 60 * 60,
			wantAllowed: true,
		},
		{
			// After the transition the window hour is EST again. A hardcoded -4 offset
			// would treat this as an hour later — still firing, but the minute-before case
			// below is what a wrong offset actually breaks.
			name:        "day after fall back is EST, the window fires",
			now:         mustET(t, 2026, time.November, 2, autoUpgradeDailyHour, 0),
			wantOffset:  -5 * 60 * 60,
			wantAllowed: true,
		},
		{
			name:        "day after fall back is EST, the minute before holds",
			now:         mustET(t, 2026, time.November, 2, autoUpgradeDailyHour-1, 59),
			wantOffset:  -5 * 60 * 60,
			wantAllowed: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, off := tc.now.Zone(); off != tc.wantOffset {
				t.Fatalf("zone offset = %d, want %d — tzdata disagrees about DST on this date", off, tc.wantOffset)
			}
			got := shouldAutoUpgradeNow(AutoUpgradeModeDaily, "", tc.now)
			if got.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v (reason: %s)", got.Allowed, tc.wantAllowed, got.Reason)
			}
		})
	}
}

// TestShouldAutoUpgradeNowDayIdentityIsET pins day identity to ET rather than
// UTC. At 22:00 ET the UTC date has already rolled over to the next day; if the
// day key were computed in UTC, a hive that fired at 18:00 ET would be allowed
// to fire again four hours later.
func TestShouldAutoUpgradeNowDayIdentityIsET(t *testing.T) {
	now := mustET(t, 2026, time.July, 15, 22, 0)
	if now.UTC().Format(autoUpgradeDateFormat) == now.Format(autoUpgradeDateFormat) {
		t.Fatal("test precondition: expected the UTC date to differ from the ET date at 22:00 ET")
	}
	got := shouldAutoUpgradeNow(AutoUpgradeModeDaily, "2026-07-15", now)
	if got.Allowed {
		t.Errorf("Allowed = true, want false — day identity must be evaluated in ET, not UTC")
	}
}

func TestAutoUpgradeModeValidation(t *testing.T) {
	tests := []struct {
		mode      string
		wantValid bool
		wantNorm  string
	}{
		{mode: "", wantValid: true, wantNorm: AutoUpgradeModeInstant},
		{mode: AutoUpgradeModeInstant, wantValid: true, wantNorm: AutoUpgradeModeInstant},
		{mode: AutoUpgradeModeDaily, wantValid: true, wantNorm: AutoUpgradeModeDaily},
		{mode: AutoUpgradeModeWeekly, wantValid: true, wantNorm: AutoUpgradeModeWeekly},
		{mode: "Daily", wantValid: false, wantNorm: AutoUpgradeModeInstant},
		{mode: "Weekly", wantValid: false, wantNorm: AutoUpgradeModeInstant},
		{mode: "hourly", wantValid: false, wantNorm: AutoUpgradeModeInstant},
		{mode: "instant ", wantValid: false, wantNorm: AutoUpgradeModeInstant},
		{mode: "off", wantValid: false, wantNorm: AutoUpgradeModeInstant},
	}
	for _, tc := range tests {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			if got := isValidAutoUpgradeMode(tc.mode); got != tc.wantValid {
				t.Errorf("isValidAutoUpgradeMode(%q) = %v, want %v", tc.mode, got, tc.wantValid)
			}
			if got := normalizeAutoUpgradeMode(tc.mode); got != tc.wantNorm {
				t.Errorf("normalizeAutoUpgradeMode(%q) = %q, want %q", tc.mode, got, tc.wantNorm)
			}
		})
	}
}

// TestHandleToggleAutoUpgradeModePersistence covers the API end of the feature:
// a valid mode is stored, an unknown one is rejected with 400 and changes
// nothing, and a request that omits the field (an older client) lands as
// instant.
func TestHandleToggleAutoUpgradeModePersistence(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantMode   string
		wantOn     bool
	}{
		{
			name:       "daily mode is stored",
			body:       `{"auto_upgrade":true,"auto_upgrade_mode":"daily"}`,
			wantStatus: http.StatusOK,
			wantMode:   AutoUpgradeModeDaily,
			wantOn:     true,
		},
		{
			name:       "instant mode is stored",
			body:       `{"auto_upgrade":true,"auto_upgrade_mode":"instant"}`,
			wantStatus: http.StatusOK,
			wantMode:   AutoUpgradeModeInstant,
			wantOn:     true,
		},
		{
			name:       "omitted mode from an older client reads as instant",
			body:       `{"auto_upgrade":true}`,
			wantStatus: http.StatusOK,
			wantMode:   "",
			wantOn:     true,
		},
		{
			name:       "unknown mode is rejected with 400",
			body:       `{"auto_upgrade":true,"auto_upgrade_mode":"fortnightly"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "weekly mode is accepted and stored",
			body:       `{"auto_upgrade":true,"auto_upgrade_mode":"weekly"}`,
			wantStatus: http.StatusOK,
			wantMode:   AutoUpgradeModeWeekly,
			wantOn:     true,
		},
		{
			name:       "off with a mode still stores the mode",
			body:       `{"auto_upgrade":false,"auto_upgrade_mode":"daily"}`,
			wantStatus: http.StatusOK,
			wantMode:   AutoUpgradeModeDaily,
			wantOn:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			mkUser(t, "alice")
			saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice"})
			s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string)}

			rec := httptest.NewRecorder()
			req := setPathValue(reqWithUser(http.MethodPut, "/au", tc.body, "alice"), "id", "h1")
			s.handleToggleAutoUpgrade(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			got := loadSaaSHive("h1")
			if got == nil {
				t.Fatal("hive record disappeared")
			}
			if tc.wantStatus != http.StatusOK {
				// A rejected request must not have mutated anything.
				if got.AutoUpgrade || got.AutoUpgradeMode != "" {
					t.Errorf("rejected request mutated the record: %+v", got)
				}
				return
			}
			if got.AutoUpgrade != tc.wantOn {
				t.Errorf("AutoUpgrade = %v, want %v", got.AutoUpgrade, tc.wantOn)
			}
			if got.AutoUpgradeMode != tc.wantMode {
				t.Errorf("AutoUpgradeMode = %q, want %q", got.AutoUpgradeMode, tc.wantMode)
			}
		})
	}
}

// TestHandleToggleAutoUpgradeClearsLastFired verifies that changing mode resets
// the day's fire record, so a hive switched to daily after an instant upgrade
// earlier the same day is not held until tomorrow.
func TestHandleToggleAutoUpgradeClearsLastFired(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{
		ID: "h1", Owner: "alice",
		AutoUpgrade:          true,
		AutoUpgradeMode:      AutoUpgradeModeDaily,
		AutoUpgradeLastFired: "2026-07-15",
	})
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string)}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPut,
		"/au", `{"auto_upgrade":true,"auto_upgrade_mode":"daily"}`, "alice"), "id", "h1")
	s.handleToggleAutoUpgrade(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("h1"); got == nil || got.AutoUpgradeLastFired != "" {
		t.Errorf("AutoUpgradeLastFired not cleared: %+v", got)
	}
}

// TestManualUpgradeUnaffectedByMode pins the rule that the schedule gates ONLY
// the automatic path. handleUpgradeHive is the manual button's handler; it must
// never consult AutoUpgradeMode. A daily-mode hive that is being held by the
// scheduler must still be manually upgradable at any hour.
func TestManualUpgradeUnaffectedByMode(t *testing.T) {
	// The scheduler holds this hive: daily mode, 09:00 ET, before the window.
	held := shouldAutoUpgradeNow(AutoUpgradeModeDaily, "", mustET(t, 2026, time.July, 15, 9, 0))
	if held.Allowed {
		t.Fatal("test precondition: expected the scheduler to hold this hive")
	}

	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{
		ID: "h1", Owner: "alice", Status: "running",
		AutoUpgrade:     true,
		AutoUpgradeMode: AutoUpgradeModeDaily,
	})
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, heartbeatUpgrade: make(map[string]string)}
	// This hive must be COLLECTIBLE, so that the only candidate reason for a
	// refusal below is the schedule — which is what this test is about. The
	// manual path refuses a hive that cannot collect an upgrade (it is pull-only
	// delivery; see pullonly_upgrade.go), and that gate is deliberately
	// independent of AutoUpgradeMode.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser(http.MethodPost, "/upgrade", `{}`, "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)

	// The manual path may fail for environmental reasons in a unit test (no
	// cluster to kubectl against), but it must NOT be refused on account of the
	// hive's schedule: a 403/409-style "not allowed" is the failure this guards.
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusConflict {
		t.Errorf("manual upgrade was refused for a daily-mode hive: %d %s", rec.Code, rec.Body.String())
	}
	// The stored mode must be untouched by a manual upgrade.
	if got := loadSaaSHive("h1"); got == nil || got.AutoUpgradeMode != AutoUpgradeModeDaily {
		t.Errorf("manual upgrade changed the schedule: %+v", got)
	}
}
