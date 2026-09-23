//go:build integration && extwork_flue

package extworksmoke

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/extwork"
	"github.com/hivecommons/hive/pkg/extwork/flue"
)

const flueSmokeVersion = "flue-smoke/1.0.0"

func TestFlueSmokeAdapterRegistersHandshakeAndDispatches(t *testing.T) {
	var dispatches atomic.Int32
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			_ = json.NewEncoder(w).Encode(map[string]string{"engine": flue.Engine, "version": flueSmokeVersion, "incarnation": "flue-smoke-incarnation"})
		case "/dispatch":
			dispatches.Add(1)
			var body struct {
				IDempotencyKey string          `json:"idempotency_key"`
				Payload        json.RawMessage `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode dispatch: %v", err)
			}
			if body.IDempotencyKey == "" || len(body.Payload) == 0 {
				t.Fatalf("dispatch body lacks key or payload: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"submission_id": "flue-task-1", "uid": "flue-smoke-incarnation", "deduplicated": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer runtime.Close()

	reg := extwork.NewRegistry()
	reg.Register(flue.Engine, flue.Factory)
	adapter, err := reg.Open(flue.Engine, map[string]string{flue.SettingEndpoint: runtime.URL, flue.SettingWorkflowVersion: flueSmokeVersion})
	if err != nil {
		t.Fatalf("open registered flue adapter: %v", err)
	}
	if adapter.Engine() != flue.Engine {
		t.Fatalf("adapter engine = %q", adapter.Engine())
	}

	payload := []byte(`{"summary":"report-only flue smoke task"}`)
	adm := extwork.Admission{
		WorkKey: "hivecommons/hive#8466", AssignmentID: "flue-smoke-task", Generation: 1, Stage: "implement",
		ContractRevision: "smoke/v1", Engine: flue.Engine, WorkflowVersion: flueSmokeVersion,
		InputRevision: "0123456789abcdef0123456789abcdef01234567",
		Authority:     extwork.AuthorityBinding{Identity: "flue-peer", Tier: "T1", Capability: flue.Capability, Mode: extwork.ModeReportOnly},
	}
	adm.RequestDigest = extwork.RequestDigest(payload)

	binding := extwork.New(adapter, extwork.NewMemoryStore(), nil, nil, extwork.ModeReportOnly)
	res, err := binding.Dispatch(context.Background(), adm, payload)
	if err != nil {
		t.Fatalf("dispatch flue smoke task: %v", err)
	}
	if !res.Started || res.Run.RemoteRunID != "flue-task-1" || res.Run.RemoteIncarnation != "flue-smoke-incarnation" {
		t.Fatalf("dispatch result = %+v", res)
	}
	if dispatches.Load() != 1 {
		t.Fatalf("dispatches = %d, want 1", dispatches.Load())
	}
}
