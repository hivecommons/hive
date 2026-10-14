package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// kickLogSourceServer is apiServer with the agent manager's kick log source
// resolver wired the way main.go wires it, so the tests can assert the live
// apply.
func kickLogSourceServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	deps.AgentMgr.SetKickLogSourceResolver(func() string {
		return deps.Config.Governor.Logging.ResolveKickLogSource()
	})
	return s, deps
}

// governorLoggingSection fetches the Logging tab payload from the governor
// config GET.
func governorLoggingSection(t *testing.T, s *Server) map[string]interface{} {
	t.Helper()
	rec := doGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor: status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Logging map[string]interface{} `json:"logging"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	return out.Logging
}

// Settings → Logging → Kick log source round-trips through the existing
// logging PUT and is reported back with its effective value (#11762, #9579).
func TestHandleGovernorLogging_KickLogSourceRoundTrip(t *testing.T) {
	t.Setenv(config.KickLogSourceEnvVar, "")
	s, deps := kickLogSourceServer(t)

	l := governorLoggingSection(t, s)
	if l["kickLogSource"] != "" || l["kickLogSourceEffective"] != config.KickLogSourceScrollback || l["kickLogSourceEnvLocked"] != false {
		t.Fatalf("default logging section = %+v", l)
	}
	if sources, ok := l["kickLogSources"].([]interface{}); !ok || len(sources) != 3 {
		t.Fatalf("kickLogSources = %+v", l["kickLogSources"])
	}

	for _, want := range config.KickLogSources() {
		rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"kickLogSource": want})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: status = %d: %s", want, rec.Code, rec.Body.String())
		}
		if got := deps.Config.Governor.Logging.KickLogSource; got != want {
			t.Errorf("saved kick_log_source = %q, want %q", got, want)
		}
		if got := deps.AgentMgr.KickLogSource(); got != want {
			t.Errorf("agent manager source after save = %q, want %q (live apply)", got, want)
		}
		l = governorLoggingSection(t, s)
		if l["kickLogSource"] != want || l["kickLogSourceEffective"] != want {
			t.Errorf("GET after PUT %s = %+v", want, l)
		}
	}

	// Absent key leaves the setting untouched; "" resets to the default.
	deps.Config.Governor.Logging.KickLogSource = config.KickLogSourceAuto
	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"level": "warn"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT level: status = %d", rec.Code)
	}
	if got := deps.Config.Governor.Logging.KickLogSource; got != config.KickLogSourceAuto {
		t.Errorf("absent key changed kick_log_source to %q", got)
	}
	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"kickLogSource": ""}); rec.Code != http.StatusOK {
		t.Fatalf("PUT reset: status = %d", rec.Code)
	}
	if got := deps.Config.Governor.Logging.KickLogSource; got != "" {
		t.Errorf("reset kick_log_source = %q, want empty", got)
	}
	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"kickLogSource": " Transcript "}); rec.Code != http.StatusOK {
		t.Fatalf("PUT mixed case: status = %d", rec.Code)
	}
	if got := deps.Config.Governor.Logging.KickLogSource; got != config.KickLogSourceTranscript {
		t.Errorf("mixed-case kick_log_source normalised to %q", got)
	}
}

func TestHandleGovernorLogging_KickLogSourceRejectsUnknown(t *testing.T) {
	t.Setenv(config.KickLogSourceEnvVar, "")
	s, deps := apiServer(t)
	deps.Config.Governor.Logging.KickLogSource = config.KickLogSourceAuto
	rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"kickLogSource": "pipe-pane"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Governor.Logging.KickLogSource; got != config.KickLogSourceAuto {
		t.Errorf("rejected value changed kick_log_source to %q", got)
	}
}

// HIVE_KICK_LOG_SOURCE wins over the saved setting; the GET says so
// (kickLogSourceEnvLocked) so the dialog can lock the select and name the
// variable, while the saved value is still persisted for when it is unset.
func TestHandleGovernorLogging_KickLogSourceEnvLocked(t *testing.T) {
	t.Setenv(config.KickLogSourceEnvVar, "transcript")
	s, deps := kickLogSourceServer(t)
	if rec := doPut(s, "/api/config/governor/logging", map[string]interface{}{"kickLogSource": "scrollback"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := deps.Config.Governor.Logging.KickLogSource; got != config.KickLogSourceScrollback {
		t.Errorf("saved kick_log_source = %q, want scrollback", got)
	}
	l := governorLoggingSection(t, s)
	if l["kickLogSource"] != config.KickLogSourceScrollback || l["kickLogSourceEffective"] != config.KickLogSourceTranscript || l["kickLogSourceEnvLocked"] != true {
		t.Errorf("env-locked logging section = %+v", l)
	}
	if got := deps.AgentMgr.KickLogSource(); got != config.KickLogSourceTranscript {
		t.Errorf("agent manager source under env override = %q, want transcript", got)
	}
}
