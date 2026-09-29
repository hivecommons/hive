package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// fakeKnowledgePrimer stands in for cmd/hive's knowledgePrimerControl: it
// records toggle calls and reports a primer registered iff the last call
// enabled it.
type fakeKnowledgePrimer struct {
	calls  []bool
	status KnowledgePrimerStatus
}

func (f *fakeKnowledgePrimer) SetKnowledgePrimer(enabled bool) KnowledgePrimerStatus {
	f.calls = append(f.calls, enabled)
	f.status.Registered = enabled
	if enabled {
		f.status.Sources = []knowledge.PrimerSource{{Name: "bead-synth-wiki", Layer: knowledge.LayerPersonal, Kind: "store"}}
	} else {
		f.status.Sources = []knowledge.PrimerSource{}
	}
	return f.status
}

func (f *fakeKnowledgePrimer) KnowledgePrimerStatus() KnowledgePrimerStatus { return f.status }

// knowledgeStatusBody is the part of GET /api/knowledge/{stats,health}
// this file asserts on.
type knowledgeStatusBody struct {
	Enabled bool                  `json:"enabled"`
	Primer  KnowledgePrimerStatus `json:"primer"`
}

// knowledgeToggleBody is the part of the PUT /api/knowledge/enabled
// response this file asserts on ("enabled" there is the legacy string echo).
type knowledgeToggleBody struct {
	Primer KnowledgePrimerStatus `json:"primer"`
}

func decodeOK[T any](t *testing.T, rec *httptest.ResponseRecorder, what string) T {
	t.Helper()
	var body T
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", what, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode: %v", what, err)
	}
	return body
}

// #9231: the boot-time auto-enabled file API (Enabled: true in its own
// config) exists on every hive, so it must not make the panel report
// knowledge as enabled while no primer is registered and no kick is primed.
func TestKnowledgeStatusReportsPrimerRegistrationNotAPIExistence(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Knowledge.Enabled = false
	deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, deps.Logger)
	primer := &fakeKnowledgePrimer{status: KnowledgePrimerStatus{Sources: []knowledge.PrimerSource{}}}
	deps.KnowledgePrimer = primer

	for _, path := range []string{"/api/knowledge/stats", "/api/knowledge/health"} {
		got := decodeOK[knowledgeStatusBody](t, doGet(s, path), path)
		if got.Enabled || got.Primer.Registered {
			t.Fatalf("%s with no primer registered: enabled=%v primer=%+v, want both false", path, got.Enabled, got.Primer)
		}
	}

	primer.SetKnowledgePrimer(true)
	for _, path := range []string{"/api/knowledge/stats", "/api/knowledge/health"} {
		got := decodeOK[knowledgeStatusBody](t, doGet(s, path), path)
		if !got.Enabled || !got.Primer.Registered {
			t.Fatalf("%s with primer registered: enabled=%v primer=%+v, want both true", path, got.Enabled, got.Primer)
		}
		if len(got.Primer.Sources) != 1 || got.Primer.Sources[0].Name != "bead-synth-wiki" {
			t.Fatalf("%s primer sources = %+v", path, got.Primer.Sources)
		}
	}
}

// #9231: PUT /api/knowledge/enabled used to persist the flag and never touch
// the primer, so enabling needed a silent restart.
func TestKnowledgeToggleRegistersAndUnregistersPrimerLive(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Knowledge.Enabled = false
	deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, deps.Logger)
	primer := &fakeKnowledgePrimer{status: KnowledgePrimerStatus{Sources: []knowledge.PrimerSource{}}}
	deps.KnowledgePrimer = primer

	on := decodeOK[knowledgeToggleBody](t, doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": true}), "enable")
	if len(primer.calls) != 1 || !primer.calls[0] {
		t.Fatalf("enable: primer calls = %v, want [true]", primer.calls)
	}
	if !on.Primer.Registered || on.Primer.RestartRequired {
		t.Fatalf("enable response primer = %+v, want registered without restart", on.Primer)
	}
	if got := decodeOK[knowledgeStatusBody](t, doGet(s, "/api/knowledge/stats"), "stats"); !got.Enabled {
		t.Fatal("stats after enable: enabled=false")
	}

	off := decodeOK[knowledgeToggleBody](t, doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": false}), "disable")
	if len(primer.calls) != 2 || primer.calls[1] {
		t.Fatalf("disable: primer calls = %v, want [true false]", primer.calls)
	}
	if off.Primer.Registered {
		t.Fatalf("disable response primer = %+v, want unregistered", off.Primer)
	}
	if deps.Knowledge == nil {
		t.Fatal("disable dropped the knowledge API; it must stay for vault browsing")
	}
	if got := decodeOK[knowledgeStatusBody](t, doGet(s, "/api/knowledge/stats"), "stats"); got.Enabled {
		t.Fatal("stats after disable: enabled=true while no primer is registered")
	}
}

// Without a live primer control the toggle can only persist the flag, and
// must say a restart is needed instead of answering a bare "updated".
func TestKnowledgeToggleWithoutPrimerControlSaysRestartRequired(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Knowledge.Enabled = false
	deps.KnowledgePrimer = nil

	got := decodeOK[knowledgeToggleBody](t, doPut(s, "/api/knowledge/enabled", map[string]bool{"enabled": true}), "enable")
	if got.Primer.Registered || !got.Primer.RestartRequired || got.Primer.RestartReason == "" {
		t.Fatalf("enable response primer = %+v, want unregistered + restart_required with a reason", got.Primer)
	}
	if !deps.Config.Knowledge.Enabled {
		t.Fatal("toggle did not persist knowledge.enabled")
	}
	if st := decodeOK[knowledgeStatusBody](t, doGet(s, "/api/knowledge/stats"), "stats"); st.Enabled || !st.Primer.RestartRequired {
		t.Fatalf("stats = %+v, want not enabled and restart_required", st)
	}
}
