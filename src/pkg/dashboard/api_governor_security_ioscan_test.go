package dashboard

import (
	"net/http"
	"testing"
)

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
