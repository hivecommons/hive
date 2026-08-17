package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// swapGooseProbe substitutes the goose ACP probe seam for one test and
// restores it (mirrors swapSDKHelper for the copilot SDK probe).
func swapGooseProbe(t *testing.T, fn func(ctx context.Context) (*gooseACPModels, error)) {
	t.Helper()
	prev := runGooseACPProbe
	runGooseACPProbe = fn
	t.Cleanup(func() { runGooseACPProbe = prev })
}

// gooseProbeMissing is the seam stand-in for "goose not on PATH" — today's
// normal on hosted spokes.
func gooseProbeMissing(ctx context.Context) (*gooseACPModels, error) {
	return nil, errGooseBinaryMissing
}

// fakeGooseACP runs a canned ACP agent over pipes and returns the client-side
// ends plus a channel that reports whether session/close was sent. The
// sessionNewResult string is served verbatim as the session/new result.
func fakeGooseACP(t *testing.T, sessionNewResult string) (io.WriteCloser, io.Reader, <-chan string) {
	t.Helper()
	clientR, clientW := io.Pipe() // agent -> client
	agentR, agentW := io.Pipe()   // client -> agent
	closed := make(chan string, 1)
	go func() {
		defer clientW.Close()
		dec := json.NewDecoder(agentR)
		for {
			var msg struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := dec.Decode(&msg); err != nil {
				return
			}
			switch msg.Method {
			case "initialize":
				fmt.Fprintf(clientW, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1,"agentCapabilities":{"sessionCapabilities":{"close":true}}}}`+"\n", msg.ID)
			case "session/new":
				// Interleave a notification first — the client must skip it.
				fmt.Fprintln(clientW, `{"jsonrpc":"2.0","method":"session/update","params":{}}`)
				fmt.Fprintf(clientW, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", msg.ID, sessionNewResult)
			case "session/close":
				var p struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				select {
				case closed <- p.SessionID:
				default:
				}
				fmt.Fprintf(clientW, `{"jsonrpc":"2.0","id":%d,"result":null}`+"\n", msg.ID)
			}
		}
	}()
	return agentW, clientR, closed
}

// TestGooseACPExchange_ModelsPresent verifies the full happy path: the live
// inventory is parsed and session/close is sent for the created session.
func TestGooseACPExchange_ModelsPresent(t *testing.T) {
	w, r, closed := fakeGooseACP(t, `{"sessionId":"sess-1","models":{"currentModelId":"qwen2.5","availableModels":[{"modelId":"qwen2.5","name":"Qwen"},{"modelId":"llama3.3"},{"modelId":""}]}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inv, err := gooseACPExchange(ctx, w, r, t.TempDir())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if inv.CurrentModelID != "qwen2.5" {
		t.Fatalf("currentModelId = %q, want qwen2.5", inv.CurrentModelID)
	}
	if !equalStrings(inv.AvailableModels, []string{"qwen2.5", "llama3.3"}) {
		t.Fatalf("availableModels = %v (empty ids must be dropped)", inv.AvailableModels)
	}
	select {
	case id := <-closed:
		if id != "sess-1" {
			t.Fatalf("session/close for %q, want sess-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session/close was never sent — probe sessions would accumulate in sessions.db")
	}
}

// TestGooseACPExchange_ModelsAbsent verifies the clean fallback signal: an
// unconfigured (or pre-1.37) goose omits the models key entirely, and the
// session that was still created gets closed.
func TestGooseACPExchange_ModelsAbsent(t *testing.T) {
	w, r, closed := fakeGooseACP(t, `{"sessionId":"sess-2","modes":{"currentModeId":"auto"}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gooseACPExchange(ctx, w, r, t.TempDir()); !errors.Is(err, errGooseModelsAbsent) {
		t.Fatalf("err = %v, want errGooseModelsAbsent", err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("session/close must be sent even when models are absent")
	}
}

// TestGooseACPExchange_MalformedResult verifies a result whose models field
// has the wrong shape is an error (→ fallback), not a panic or a bogus list.
func TestGooseACPExchange_MalformedResult(t *testing.T) {
	w, r, _ := fakeGooseACP(t, `{"sessionId":"sess-3","models":"nope"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := gooseACPExchange(ctx, w, r, t.TempDir()); err == nil {
		t.Fatal("expected error on malformed session/new result")
	}
}

// TestGooseACPExchange_Timeout verifies a hung agent (garbage output, no
// response) is bounded by ctx rather than blocking the backends endpoint.
func TestGooseACPExchange_Timeout(t *testing.T) {
	clientR, clientW := io.Pipe()
	agentR, agentW := io.Pipe()
	go func() {
		// Emit non-JSON chatter and never answer.
		fmt.Fprintln(clientW, "not json at all")
		_, _ = io.Copy(io.Discard, agentR)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := gooseACPExchange(ctx, agentW, clientR, t.TempDir())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
	_ = clientW.Close()
}

// TestDiscoverGooseModels_LiveInventory verifies a successful probe yields a
// live (fallback=false) list with goose's current model first.
func TestDiscoverGooseModels_LiveInventory(t *testing.T) {
	t.Setenv("GOOSE_MODEL", "")
	swapGooseProbe(t, func(ctx context.Context) (*gooseACPModels, error) {
		return &gooseACPModels{
			CurrentModelID:  "qwen2.5",
			AvailableModels: []string{"qwen2.5", "llama3.3", "deepseek-r1"},
		}, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGooseModels()
	if r.fallback {
		t.Fatalf("live inventory must not be fallback: %+v", r)
	}
	if !equalStrings(r.models, []string{"qwen2.5", "llama3.3", "deepseek-r1"}) {
		t.Fatalf("models = %v, want current-first deduped inventory", r.models)
	}
}

// TestDiscoverGooseModels_EnvPinFirst verifies a GOOSE_MODEL env pin outranks
// currentModelId for first position and is deduped against the inventory.
func TestDiscoverGooseModels_EnvPinFirst(t *testing.T) {
	t.Setenv("GOOSE_MODEL", "llama3.3")
	swapGooseProbe(t, func(ctx context.Context) (*gooseACPModels, error) {
		return &gooseACPModels{
			CurrentModelID:  "qwen2.5",
			AvailableModels: []string{"qwen2.5", "llama3.3"},
		}, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGooseModels()
	if !equalStrings(r.models, []string{"llama3.3", "qwen2.5"}) {
		t.Fatalf("models = %v, want env pin first with no duplicate", r.models)
	}
	if r.fallback {
		t.Fatal("env-pinned live inventory is still a live result")
	}
}

// TestDiscoverGooseModels_BinaryMissingFallsBack verifies the real exec path:
// with goose absent from PATH (today's normal on hosted spokes) the provider
// static fallback is served, silently.
func TestDiscoverGooseModels_BinaryMissingFallsBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no goose binary
	t.Setenv("GOOSE_PROVIDER", "anthropic")
	t.Setenv("GOOSE_MODEL", "")
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGooseModels()
	if !r.fallback {
		t.Fatalf("missing binary must serve fallback, got %+v", r)
	}
	if !contains(r.models, "claude-opus-4-8") {
		t.Fatalf("fallback should be the anthropic static list, got %v", r.models)
	}
}

// TestDiscoverGooseModels_ProbeErrorFallsBack verifies any probe error
// (spawn failure, timeout, protocol error) lands on the static fallback.
func TestDiscoverGooseModels_ProbeErrorFallsBack(t *testing.T) {
	t.Setenv("GOOSE_PROVIDER", "ollama")
	t.Setenv("GOOSE_MODEL", "")
	swapGooseProbe(t, func(ctx context.Context) (*gooseACPModels, error) {
		return nil, errors.New("boom")
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGooseModels()
	if !r.fallback || !contains(r.models, "llama3.3") {
		t.Fatalf("probe error must serve the ollama static fallback, got %+v", r)
	}
}

// TestDiscoverGooseModels_ModelsAbsentFallsBack verifies the absent-models
// signal (unconfigured goose) falls back cleanly.
func TestDiscoverGooseModels_ModelsAbsentFallsBack(t *testing.T) {
	t.Setenv("GOOSE_PROVIDER", "")
	t.Setenv("GOOSE_MODEL", "")
	swapGooseProbe(t, func(ctx context.Context) (*gooseACPModels, error) {
		return nil, errGooseModelsAbsent
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.discoverGooseModels()
	if !r.fallback || !equalStrings(r.models, []string{"default"}) {
		t.Fatalf("unconfigured goose must fall back to [default], got %+v", r)
	}
}

// TestQueryCLIModels_GooseLiveThroughStabilize verifies the live goose list
// flows through the query pipeline (cache + stabilize) as a non-fallback
// result with the pinned model still first.
func TestQueryCLIModels_GooseLiveThroughStabilize(t *testing.T) {
	t.Setenv("GOOSE_MODEL", "")
	swapGooseProbe(t, func(ctx context.Context) (*gooseACPModels, error) {
		return &gooseACPModels{
			CurrentModelID:  "qwen2.5",
			AvailableModels: []string{"qwen2.5", "llama3.3"},
		}, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("goose")
	if r.fallback {
		t.Fatalf("live goose discovery through queryCLIModels must not be fallback: %+v", r)
	}
	if len(r.models) == 0 || r.models[0] != "qwen2.5" {
		t.Fatalf("pinned/current model must stay first through stabilize, got %v", r.models)
	}
	// A second successful probe missing llama3.3 must retain it (stabilize).
	swapGooseProbe(t, func(ctx context.Context) (*gooseACPModels, error) {
		return &gooseACPModels{CurrentModelID: "qwen2.5", AvailableModels: []string{"qwen2.5"}}, nil
	})
	s.cliModels.entries = map[string]cliModelCacheEntry{} // bust the TTL cache
	r = s.queryCLIModels("goose")
	if !contains(r.models, "llama3.3") {
		t.Fatalf("recently-seen model must be retained through a transient miss, got %v", r.models)
	}
}
