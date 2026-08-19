package hub

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// checkStatusByName pulls one named check's status out of a health map, using
// the same array shape the heartbeat carries.
func checkStatusByName(t *testing.T, health map[string]any, name string) (status, detail string) {
	t.Helper()
	raw, _ := health["checks"].([]any)
	for _, v := range raw {
		ck, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := ck["name"].(string); n == name {
			status, _ = ck["status"].(string)
			detail, _ = ck["detail"].(string)
			return status, detail
		}
	}
	t.Fatalf("check %q not found in %v", name, health["checks"])
	return "", ""
}

func placeholderEntryWithHealth(health map[string]any) MyHiveEntry {
	return MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:     "pool-1",
		Org:    placeholderOrgPrefix + "3",
		Online: true,
		Health: health,
	}}
}

func authFailingHealth() map[string]any {
	return map[string]any{
		"status": "degraded",
		"fails":  2,
		"warns":  0,
		"checks": []any{
			map[string]any{"name": "ready", "status": "pass"},
			map[string]any{"name": "github_auth", "status": "fail", "detail": "no GitHub auth configured"},
			map[string]any{"name": "github_app_key", "status": "fail", "detail": "no key delivered"},
		},
	}
}

// An UNASSIGNED placeholder whose only failures are auth-class checks is the
// pool working as designed: the row must ship GREEN ("ok"), with the auth
// checks reclassified as skip and the hover detail saying why.
func TestPlaceholderRowGreenWhenOnlyAuthChecksFail(t *testing.T) {
	e := placeholderEntryWithHealth(authFailingHealth())
	original := e.Health

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusOK {
		t.Fatalf("placeholder with only auth-class failures: status = %q, want %q", st, healthStatusOK)
	}
	if fails, _ := got["fails"].(int); fails != 0 {
		t.Errorf("fails = %v, want 0 — the failing-checks pill must not render", got["fails"])
	}
	for _, name := range []string{"github_auth", "github_app_key"} {
		st, detail := checkStatusByName(t, got, name)
		if st != healthCheckStatusSkip {
			t.Errorf("%s status = %q, want %q", name, st, healthCheckStatusSkip)
		}
		if detail != placeholderAuthNote {
			t.Errorf("%s detail = %q, want the unassigned note %q", name, detail, placeholderAuthNote)
		}
	}
	if st, _ := checkStatusByName(t, got, "ready"); st != "pass" {
		t.Errorf("ready status = %q, want pass (non-auth checks untouched)", st)
	}

	// The registry's own map must not have been rewritten: Health is shared by
	// reference with the live registry snapshot.
	if st := healthStatusOf(original); st != healthStatusDegraded {
		t.Errorf("original health map was mutated: status = %q, want %q untouched", st, healthStatusDegraded)
	}
	if st, _ := checkStatusByName(t, original, "github_auth"); st != healthCheckStatusFail {
		t.Errorf("original github_auth was mutated to %q", st)
	}
}

// The SAME failing auth checks on a CLAIMED hive are a real fault: the
// sanitizer must leave the entry byte-for-byte alone.
func TestClaimedRowKeepsFailingAuthCheck(t *testing.T) {
	e := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:     "claimed-1",
		Org:    "acme",
		Online: true,
		Health: authFailingHealth(),
	}}
	e.ProvStatus = "active"

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusDegraded {
		t.Fatalf("claimed hive status = %q, want %q — the exemption must not leak past placeholders", st, healthStatusDegraded)
	}
	if st, _ := checkStatusByName(t, got, "github_auth"); st != healthCheckStatusFail {
		t.Errorf("claimed hive github_auth = %q, want fail", st)
	}
}

// provStatus "available" is the authoritative placeholder signal (the org
// prefix is only the fallback) — the sanitizer must honour it too, exactly as
// isPlaceholderEntry does.
func TestPlaceholderByProvStatusIsSanitized(t *testing.T) {
	e := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:     "pool-2",
		Org:    "some-org",
		Online: true,
		Health: authFailingHealth(),
	}}
	e.ProvStatus = statusAvailable

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	if st := healthStatusOf(rows[0].Health); st != healthStatusOK {
		t.Fatalf("provStatus=available placeholder: status = %q, want %q", st, healthStatusOK)
	}
}

// A placeholder-applicable failure — here "ready" failing, the not-ready case —
// must still turn the row red: only auth-class checks are designed-state.
func TestPlaceholderRowStaysDegradedOnRealFailure(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "degraded",
		"fails":  2,
		"warns":  0,
		"checks": []any{
			map[string]any{"name": "ready", "status": "fail", "detail": "not ready"},
			map[string]any{"name": "github_auth", "status": "fail", "detail": "no GitHub auth configured"},
		},
	})

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusDegraded {
		t.Fatalf("placeholder with a real failure: status = %q, want %q", st, healthStatusDegraded)
	}
	if fails, _ := got["fails"].(int); fails != 1 {
		t.Errorf("fails = %v, want 1 — only the genuine failure counts toward the pill", got["fails"])
	}
	if st, _ := checkStatusByName(t, got, "ready"); st != healthCheckStatusFail {
		t.Errorf("ready = %q, want fail — genuine failures keep their status", st)
	}
	if st, _ := checkStatusByName(t, got, "github_auth"); st != healthCheckStatusSkip {
		t.Errorf("github_auth = %q, want %q", st, healthCheckStatusSkip)
	}
}

// A warning-class non-auth check must survive as the overall status too.
func TestPlaceholderRowKeepsWarningFromRealCheck(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "degraded",
		"checks": []any{
			map[string]any{"name": "stall_detection", "status": "warn", "detail": "1 stalled"},
			map[string]any{"name": "github_auth", "status": "fail"},
		},
	})
	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	if st := healthStatusOf(rows[0].Health); st != healthStatusWarning {
		t.Fatalf("status = %q, want %q — a real warn survives the recompute", st, healthStatusWarning)
	}
}

// Zero token consumption is the DESIGNED state of an unassigned pool slot —
// no project means no workload to spend on — so the spoke's tokens check
// warning ("zero consumed") must not amber the row: it is reclassified to
// skip with the no-workload note and the row ships green.
func TestPlaceholderRowGreenWhenTokensWarnZeroConsumed(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "warning",
		"fails":  0,
		"warns":  1,
		"checks": []any{
			map[string]any{"name": "ready", "status": "pass"},
			map[string]any{"name": healthCheckTokens, "status": "warn", "detail": "zero consumed"},
		},
	})
	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusOK {
		t.Fatalf("placeholder with only the zero-consumed tokens warning: status = %q, want %q", st, healthStatusOK)
	}
	if warns, _ := got["warns"].(int); warns != 0 {
		t.Errorf("warns = %v, want 0 — the pill must not render", got["warns"])
	}
	st, detail := checkStatusByName(t, got, healthCheckTokens)
	if st != healthCheckStatusSkip {
		t.Errorf("tokens status = %q, want %q", st, healthCheckStatusSkip)
	}
	if detail != placeholderTokensNote {
		t.Errorf("tokens detail = %q, want the no-workload note %q", detail, placeholderTokensNote)
	}
}

// An UNASSIGNED placeholder whose only warning is the empty-repo repo_target
// shape is the pool working as designed — repos are set when the slot is
// claimed — so the row must ship GREEN with the check reclassified as skip.
func TestPlaceholderRowGreenWhenRepoTargetEmpty(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "warning",
		"fails":  0,
		"warns":  1,
		"checks": []any{
			map[string]any{"name": "ready", "status": "pass"},
			map[string]any{"name": healthCheckRepoTarget, "status": "warn", "detail": "Repo target misconfigured: repo is empty — expected org/repo. Fix in Settings → Repos."},
		},
	})
	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusOK {
		t.Fatalf("placeholder with only the empty-repo repo_target warning: status = %q, want %q", st, healthStatusOK)
	}
	if warns, _ := got["warns"].(int); warns != 0 {
		t.Errorf("warns = %v, want 0 — the pill must not render", got["warns"])
	}
	st, detail := checkStatusByName(t, got, healthCheckRepoTarget)
	if st != healthCheckStatusSkip {
		t.Errorf("repo_target status = %q, want %q", st, healthCheckStatusSkip)
	}
	if detail != placeholderRepoTargetNote {
		t.Errorf("repo_target detail = %q, want the unassigned note %q", detail, placeholderRepoTargetNote)
	}
}

// A MALFORMED repo-target value (URL / slash / forge host) is a genuine
// misconfiguration even on a placeholder — the warning must survive.
func TestPlaceholderRowKeepsMalformedRepoTargetWarning(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "warning",
		"checks": []any{
			map[string]any{"name": healthCheckRepoTarget, "status": "warn", "detail": "Repo target misconfigured: org 'github.ibm.com' looks like a forge host — expected org/repo. Fix in Settings → Repos."},
		},
	})
	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusWarning {
		t.Fatalf("placeholder with malformed repo target: status = %q, want %q — a bad VALUE is a real fault", st, healthStatusWarning)
	}
	if st, _ := checkStatusByName(t, got, healthCheckRepoTarget); st != healthCheckStatusWarn {
		t.Errorf("repo_target = %q, want warn untouched", st)
	}
}

// The SAME empty-repo warning on a CLAIMED hive is a real misconfiguration
// (a claimed project must have repos) and must pass through untouched.
func TestClaimedRowKeepsEmptyRepoTargetWarning(t *testing.T) {
	e := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:     "claimed-3",
		Org:    "acme",
		Online: true,
		Health: map[string]any{
			"status": "warning",
			"checks": []any{
				map[string]any{"name": healthCheckRepoTarget, "status": "warn", "detail": "Repo target misconfigured: repo is empty — expected org/repo. Fix in Settings → Repos."},
			},
		},
	}}
	e.ProvStatus = "active"

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	if st := healthStatusOf(rows[0].Health); st != healthStatusWarning {
		t.Fatalf("claimed hive status = %q, want %q — the repo_target exemption must not leak", st, healthStatusWarning)
	}
}

// The SAME zero-consumed warning on a CLAIMED hive is a real signal (an
// assigned project spending nothing is worth noticing) and must pass through
// untouched.
func TestClaimedRowKeepsTokensWarning(t *testing.T) {
	e := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:     "claimed-2",
		Org:    "acme",
		Online: true,
		Health: map[string]any{
			"status": "warning",
			"checks": []any{
				map[string]any{"name": healthCheckTokens, "status": "warn", "detail": "zero consumed"},
			},
		},
	}}
	e.ProvStatus = "active"

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusWarning {
		t.Fatalf("claimed hive status = %q, want %q — the tokens exemption must not leak", st, healthStatusWarning)
	}
	if st, detail := checkStatusByName(t, got, healthCheckTokens); st != healthCheckStatusWarn || detail != "zero consumed" {
		t.Errorf("claimed hive tokens shipped as %q/%q, want warn/zero consumed untouched", st, detail)
	}
}

// A tokens warning alongside a genuine failure: the tokens check is
// neutralised but the real fault still owns the row colour.
func TestPlaceholderTokensWarnDoesNotMaskRealFailure(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "degraded",
		"checks": []any{
			map[string]any{"name": "ready", "status": "fail", "detail": "not ready"},
			map[string]any{"name": healthCheckTokens, "status": "warn", "detail": "zero consumed"},
		},
	})
	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	if st := healthStatusOf(rows[0].Health); st != healthStatusDegraded {
		t.Fatalf("status = %q, want %q — neutralising tokens must not hide the not-ready fault", st, healthStatusDegraded)
	}
}

// The sanitizer only touches Health: liveness signals (online, lastHeartbeat)
// that drive the offline dot and the stale-heartbeat drift/alert rules must
// pass through untouched, so a stale placeholder still reads as degraded.
func TestPlaceholderSanitizeLeavesLivenessAlone(t *testing.T) {
	stale := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	e := placeholderEntryWithHealth(authFailingHealth())
	e.Online = false
	e.LastHeartbeat = stale

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)

	if rows[0].Online {
		t.Errorf("Online was rewritten — the offline dot must keep showing")
	}
	if rows[0].LastHeartbeat != stale {
		t.Errorf("LastHeartbeat = %q, want %q untouched", rows[0].LastHeartbeat, stale)
	}
	// And the stale-heartbeat drift rule still fires on the sanitized entry.
	rep := computeDrift(rows[0], fleetNorm{}, nil, time.Now())
	found := false
	for _, sig := range rep.Signals {
		if sig.Kind == DriftKindHeartbeatStale {
			found = true
		}
	}
	if !found {
		t.Errorf("heartbeat-stale drift signal missing on a sanitized stale placeholder: %+v", rep.Signals)
	}
}

// A health map with no recognisable checks array (the defensive map shape, or
// no checks at all) is left exactly as reported — greening a row demands
// positive evidence.
func TestPlaceholderUnknownHealthShapeUntouched(t *testing.T) {
	for name, health := range map[string]map[string]any{
		"nil":       nil,
		"no checks": {"status": "degraded"},
		"map shape": {"status": "degraded", "checks": map[string]any{"github_auth": map[string]any{"status": "fail"}}},
	} {
		e := placeholderEntryWithHealth(health)
		rows := []MyHiveEntry{e}
		sanitizePlaceholderRows(rows)
		if healthStatusOf(rows[0].Health) == healthStatusOK {
			t.Errorf("%s: shape without positive evidence was greened: %v", name, rows[0].Health)
		}
	}
}

// End-to-end through the handler: the My Hives payload must ship a green
// placeholder and an unchanged degraded claimed hive in the same response —
// this is what pins the sanitizer CALL in handleMyHives, not just the helper.
func TestMyHivesPlaceholderRowShipsGreenHealth(t *testing.T) {
	const owner = "placeholder-green-owner"
	const token = "ghp_placeholder_green"

	dir := t.TempDir()
	origUsers, origTimeline := saasUsersDir, timelineDir
	saasUsersDir = dir + "/users"
	timelineDir = dir + "/timeline"
	t.Cleanup(func() { saasUsersDir, timelineDir = origUsers, origTimeline })

	cleanup := helperSetupAuthUser(t, token, owner)
	defer cleanup()

	now := time.Now().UTC().Format(time.RFC3339)
	s := NewHubServer(0, slog.Default(), "test", "v2")
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{
		{
			ID: "pool-slot-1", Name: "available-3/pending", Org: placeholderOrgPrefix + "3",
			Owner: owner, Online: true, LastHeartbeat: now,
			Health: authFailingHealth(),
		},
		{
			ID: "claimed-9", Name: "acme/widget", Org: "acme",
			Owner: owner, Online: true, LastHeartbeat: now,
			Health: authFailingHealth(),
		},
	}
	s.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/saas/my-hives", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.handleMyHives(rec, req)
	if rec.Code != 200 {
		t.Fatalf("handleMyHives = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Hives []MyHiveEntry `json:"hives"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]MyHiveEntry{}
	for _, h := range resp.Hives {
		byID[h.ID] = h
	}

	pool, ok := byID["pool-slot-1"]
	if !ok {
		t.Fatalf("placeholder missing from response")
	}
	if st := healthStatusOf(pool.Health); st != healthStatusOK {
		t.Errorf("placeholder shipped status %q, want %q", st, healthStatusOK)
	}
	if st, detail := checkStatusByName(t, pool.Health, "github_auth"); st != healthCheckStatusSkip || detail != placeholderAuthNote {
		t.Errorf("placeholder github_auth shipped as %q/%q, want skip with the unassigned note", st, detail)
	}

	claimed, ok := byID["claimed-9"]
	if !ok {
		t.Fatalf("claimed hive missing from response")
	}
	if st := healthStatusOf(claimed.Health); st != healthStatusDegraded {
		t.Errorf("claimed hive shipped status %q, want %q — sanitizing must be placeholder-only", st, healthStatusDegraded)
	}
}

// An UNASSIGNED placeholder whose agents check fails only because agents await
// the owner's CLI login ("0 running, N paused, M need login: ...") is the pool
// working as designed — no owner has claimed the slot or run the CLI login yet,
// exactly like github_auth being unconfigured by design. The row must ship
// GREEN with the agents check reclassified to skip and the login-pending note.
// This is the live available-oke-12 case.
func TestPlaceholderRowGreenWhenAgentsAwaitLogin(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "degraded",
		"fails":  1,
		"warns":  0,
		"checks": []any{
			map[string]any{"name": "ready", "status": "pass"},
			map[string]any{"name": "github_auth", "status": "fail", "detail": "no GitHub auth configured"},
			map[string]any{"name": healthCheckAgents, "status": "fail", "detail": "0 running, 1 paused, 4 need login: guide, quality, scanner, supervisor"},
		},
	})

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusOK {
		t.Fatalf("placeholder with login-pending agents: status = %q, want %q", st, healthStatusOK)
	}
	if fails, _ := got["fails"].(int); fails != 0 {
		t.Errorf("fails = %v, want 0 — a login-pending agents check must not degrade a placeholder", got["fails"])
	}
	st, detail := checkStatusByName(t, got, healthCheckAgents)
	if st != healthCheckStatusSkip {
		t.Errorf("agents status = %q, want %q", st, healthCheckStatusSkip)
	}
	if detail != placeholderAgentsNote {
		t.Errorf("agents detail = %q, want the login-pending note %q", detail, placeholderAgentsNote)
	}
}

// A placeholder whose agents are genuinely DOWN (crash-looping / exiting, the
// "down:" bucket) is a real fault even on an unassigned slot: the agents check
// must NOT be exempted and the row must stay degraded. This preserves the
// original intent — only the by-design login-pending shape is greened.
func TestPlaceholderRowStaysDegradedWhenAgentsCrashLoop(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "degraded",
		"fails":  1,
		"warns":  0,
		"checks": []any{
			map[string]any{"name": "ready", "status": "pass"},
			map[string]any{"name": "github_auth", "status": "fail", "detail": "no GitHub auth configured"},
			map[string]any{"name": healthCheckAgents, "status": "fail", "detail": "0 running, 2 down: scanner, supervisor"},
		},
	})

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusDegraded {
		t.Fatalf("placeholder with crash-looping agents: status = %q, want %q — down agents are a real fault", st, healthStatusDegraded)
	}
	if st, _ := checkStatusByName(t, got, healthCheckAgents); st != healthCheckStatusFail {
		t.Errorf("agents status = %q, want fail — crash-looping agents keep their status even on a placeholder", st)
	}
	// The auth check alongside is still exempted, so the ONLY thing degrading
	// the row is the genuine agents failure.
	if fails, _ := got["fails"].(int); fails != 1 {
		t.Errorf("fails = %v, want 1 — only the genuine agents failure counts", got["fails"])
	}
}

// A placeholder with agents both DOWN and needing login: the presence of a
// "down:" agent makes the whole agents fail genuine — the exemption requires
// positive evidence that NO agent is actually broken. The row stays degraded.
func TestPlaceholderAgentsDownAndLoginStaysDegraded(t *testing.T) {
	e := placeholderEntryWithHealth(map[string]any{
		"status": "degraded",
		"checks": []any{
			map[string]any{"name": healthCheckAgents, "status": "fail", "detail": "0 running, 1 down: crashed, 2 need login: needy-a, needy-b"},
		},
	})
	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusDegraded {
		t.Fatalf("status = %q, want %q — a down agent alongside login-pending ones is still a real fault", st, healthStatusDegraded)
	}
	if st, _ := checkStatusByName(t, got, healthCheckAgents); st != healthCheckStatusFail {
		t.Errorf("agents status = %q, want fail", st)
	}
}

// The SAME login-pending agents failure on a CLAIMED hive is a real signal (an
// owner-actionable "log in your CLI" prompt) and must pass through untouched:
// the exemption applies ONLY to placeholders.
func TestClaimedRowKeepsLoginPendingAgentsFail(t *testing.T) {
	e := MyHiveEntry{RegistryEntry: RegistryEntry{
		ID:     "claimed-3",
		Org:    "acme",
		Online: true,
		Health: map[string]any{
			"status": "degraded",
			"checks": []any{
				map[string]any{"name": healthCheckAgents, "status": "fail", "detail": "0 running, 1 needs login: supervisor"},
			},
		},
	}}
	e.ProvStatus = "active"

	rows := []MyHiveEntry{e}
	sanitizePlaceholderRows(rows)
	got := rows[0].Health

	if st := healthStatusOf(got); st != healthStatusDegraded {
		t.Fatalf("claimed hive status = %q, want %q — the agents exemption must not leak past placeholders", st, healthStatusDegraded)
	}
	if st, detail := checkStatusByName(t, got, healthCheckAgents); st != healthCheckStatusFail || !strings.Contains(detail, "needs login") {
		t.Errorf("claimed hive agents shipped as %q/%q, want fail with the login detail untouched", st, detail)
	}
}

// agentsFailIsLoginPending is the discrimination at the heart of the fix:
// login-pending detail (need/needs login, no "down:") is by-design; anything
// with a "down:" crash marker, or an unrecognised shape with neither marker, is
// left degrading.
func TestAgentsFailIsLoginPending(t *testing.T) {
	cases := []struct {
		detail string
		want   bool
	}{
		{"0 running, 1 paused, 4 need login: guide, quality, scanner, supervisor", true},
		{"0 running, 1 needs login: supervisor", true},
		{"1 running, 1 needs login: wedged", true},
		{"0 running, 2 down: scanner, supervisor", false},
		{"0 running, 1 down: crashed, 2 need login: needy-a, needy-b", false},
		{"0 running, 1 down: crashed", false},
		{"3 running", false}, // no login-pending evidence → not exempt
		{"", false},
	}
	for _, c := range cases {
		if got := agentsFailIsLoginPending(c.detail); got != c.want {
			t.Errorf("agentsFailIsLoginPending(%q) = %v, want %v", c.detail, got, c.want)
		}
	}
}

// The dashboard's own placeholder handling: healthBadge must carry the
// unassigned line instead of forcing App-degraded, and hiveStatusFlags must
// mirror the same placeholder guard so dot and chip agree. String-anchored on
// the raw dashboardHTML, same as the other JS structure tests.
func TestDashboardPlaceholderRowsRenderGreen(t *testing.T) {
	if !strings.Contains(dashboardHTML, "Unassigned — GitHub auth not configured by design") {
		t.Errorf("healthBadge lost its unassigned hover line — a placeholder row would claim a GitHub App state it cannot have")
	}
	if n := strings.Count(dashboardHTML, "!isPlaceholderHive(h) && !!h.githubAppRequired"); n < 2 {
		t.Errorf("hiveStatusFlags placeholder guard appears %d times, want 2 (appMissing and degraded) — dropping either lets an unassigned row read as degraded", n)
	}
	if !strings.Contains(dashboardHTML, "if (isPlaceholderHive(h)) {\n        /* An UNASSIGNED pool slot has no GitHub auth BY DESIGN") {
		t.Errorf("healthBadge no longer short-circuits the GitHub-App degraded rules for placeholders")
	}
}
