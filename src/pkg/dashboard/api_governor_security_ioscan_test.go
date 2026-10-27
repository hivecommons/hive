package dashboard

import (
	"net/http"
	"testing"
)

// At L5-L6 an empty ioscan.fail_mode resolves to closed, so the dashboard
// must store "open" as written. It used to store it as empty, which read back
// as closed and left the owner no way to choose open.
func TestGovernorSecurityOpenFailModeSticksAtL6(t *testing.T) {
	s, deps := apiServer(t)
	level := 6
	deps.Config.ACMMLevel = &level

	rec := doPut(s, "/api/config/governor/security", map[string]any{"ioscanFailMode": "open"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT security = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if deps.Config.Ioscan.FailMode != "open" || deps.Config.Ioscan.FailClosedAtLevel(6) {
		t.Fatalf("open fail mode not stored explicitly: %q", deps.Config.Ioscan.FailMode)
	}
	sec := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if sec["ioscanFailMode"] != "open" || sec["ioscanFailModeSetting"] != "open" {
		t.Fatalf("security section fail mode = %v / %v, want open / open", sec["ioscanFailMode"], sec["ioscanFailModeSetting"])
	}

	// Empty still means "follow the level".
	rec = doPut(s, "/api/config/governor/security", map[string]any{"ioscanFailMode": ""})
	if rec.Code != http.StatusOK || deps.Config.Ioscan.FailMode != "" || !deps.Config.Ioscan.FailClosedAtLevel(6) {
		t.Fatalf("empty fail mode should restore the L6 default: code=%d mode=%q", rec.Code, deps.Config.Ioscan.FailMode)
	}
}

func TestGovernorSecurityClassifierControlsRoundTrip(t *testing.T) {
	s, deps := apiServer(t)
	level := 6
	deps.Config.ACMMLevel = &level

	sec := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if sec["ioscanClassifierEnabled"] != true || sec["ioscanClassifierSetting"] != "" ||
		sec["ioscanClassifierFailMode"] != "closed" || sec["ioscanClassifierFailModeSetting"] != "" {
		t.Fatalf("L6 classifier defaults wrong: %#v", sec)
	}

	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"ioscanClassifierSetting":  "off",
		"ioscanClassifierFailMode": "open",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT security = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cl := deps.Config.Ioscan.Classifier
	if cl.Enabled == nil || *cl.Enabled || cl.FailMode != "open" {
		t.Fatalf("classifier controls not stored: enabled=%v fail_mode=%q", cl.Enabled, cl.FailMode)
	}
	sec = decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if sec["ioscanClassifierEnabled"] != false || sec["ioscanClassifierSetting"] != "off" ||
		sec["ioscanClassifierFailMode"] != "open" || sec["ioscanClassifierFailModeSetting"] != "open" {
		t.Fatalf("classifier controls not reported back: %#v", sec)
	}

	rec = doPut(s, "/api/config/governor/security", map[string]any{"ioscanClassifierSetting": "on"})
	if rec.Code != http.StatusOK || deps.Config.Ioscan.Classifier.Enabled == nil || !*deps.Config.Ioscan.Classifier.Enabled {
		t.Fatalf("classifier on not stored: code=%d enabled=%v", rec.Code, deps.Config.Ioscan.Classifier.Enabled)
	}

	rec = doPut(s, "/api/config/governor/security", map[string]any{
		"ioscanClassifierSetting":  "",
		"ioscanClassifierFailMode": "",
	})
	cl = deps.Config.Ioscan.Classifier
	if rec.Code != http.StatusOK || cl.Enabled != nil || cl.FailMode != "" {
		t.Fatalf("empty should restore the level defaults: code=%d enabled=%v fail_mode=%q", rec.Code, cl.Enabled, cl.FailMode)
	}
}

func TestGovernorSecurityRejectsInvalidClassifierValues(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"switch":    {"ioscanClassifierSetting": "maybe"},
		"fail mode": {"ioscanClassifierFailMode": "panic"},
	} {
		t.Run(name, func(t *testing.T) {
			s, deps := apiServer(t)
			rec := doPut(s, "/api/config/governor/security", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if cl := deps.Config.Ioscan.Classifier; cl.Enabled != nil || cl.FailMode != "" {
				t.Fatalf("invalid value mutated config: %+v", cl)
			}
		})
	}
}

func TestGovernorSecurityClassifierBackendRoundTrip(t *testing.T) {
	s, deps := apiServer(t)

	sec := decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if sec["ioscanClassifierBackend"] != "" || sec["ioscanClassifierCliBudgetPerEval"] != float64(0) || sec["ioscanClassifierCliDailyQuota"] != float64(0) {
		t.Fatalf("classifier backend defaults wrong: %#v", sec)
	}

	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"ioscanClassifierBackend":          " claude ",
		"ioscanClassifierCliBudgetPerEval": 4,
		"ioscanClassifierCliDailyQuota":    120,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT security = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	cl := deps.Config.Ioscan.Classifier
	if cl.Backend != "claude" || cl.CLIBudgetPerEval != 4 || cl.CLIDailyQuota != 120 {
		t.Fatalf("classifier backend not stored: %+v", cl)
	}
	sec = decodeJSON(t, doGet(s, "/api/config/governor"))["security"].(map[string]any)
	if sec["ioscanClassifierBackend"] != "claude" || sec["ioscanClassifierCliBudgetPerEval"] != float64(4) || sec["ioscanClassifierCliDailyQuota"] != float64(120) {
		t.Fatalf("classifier backend not reported back: %#v", sec)
	}

	// A save that does not touch the backend keys leaves them alone.
	rec = doPut(s, "/api/config/governor/security", map[string]any{"ioscanClassifierFailMode": "open"})
	if cl := deps.Config.Ioscan.Classifier; rec.Code != http.StatusOK || cl.Backend != "claude" || cl.CLIBudgetPerEval != 4 || cl.CLIDailyQuota != 120 {
		t.Fatalf("unrelated save changed backend keys: code=%d %+v", rec.Code, cl)
	}

	rec = doPut(s, "/api/config/governor/security", map[string]any{"ioscanClassifierBackend": ""})
	if rec.Code != http.StatusOK || deps.Config.Ioscan.Classifier.Backend != "" {
		t.Fatalf("empty backend should restore the HTTP path: code=%d backend=%q", rec.Code, deps.Config.Ioscan.Classifier.Backend)
	}
}

func TestGovernorSecurityRejectsInvalidClassifierBackendValues(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"gemini backend":    {"ioscanClassifierBackend": "gemini"},
		"codex backend":     {"ioscanClassifierBackend": "codex"},
		"negative budget":   {"ioscanClassifierCliBudgetPerEval": -1},
		"budget above 64":   {"ioscanClassifierCliBudgetPerEval": 65},
		"negative quota":    {"ioscanClassifierCliDailyQuota": -5},
		"bad with good key": {"ioscanClassifierBackend": "claude", "ioscanClassifierCliDailyQuota": -1},
	} {
		t.Run(name, func(t *testing.T) {
			s, deps := apiServer(t)
			rec := doPut(s, "/api/config/governor/security", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if cl := deps.Config.Ioscan.Classifier; cl.Backend != "" || cl.CLIBudgetPerEval != 0 || cl.CLIDailyQuota != 0 {
				t.Fatalf("invalid value mutated config: %+v", cl)
			}
		})
	}
}
