package dashboard

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// swapCodexProbe substitutes the codex probe seam for one test and restores it.
func swapCodexProbe(t *testing.T, fn func() ([]string, error)) {
	t.Helper()
	prev := runCodexModelProbe
	runCodexModelProbe = fn
	t.Cleanup(func() { runCodexModelProbe = prev })
}

// codexCannedStdout builds a canned app-server stdout: initialize response,
// initialized notification, then the model/list response with the given data.
func codexCannedStdout(data string) *bytes.Buffer {
	return bytes.NewBufferString(
		`{"id":1,"result":{}}` + "\n" +
			`{"method":"initialized"}` + "\n" +
			`{"id":2,"result":{"data":[` + data + `]}}` + "\n")
}

// TestCodexModelListExchange_HiddenOrdering verifies the served order: visible
// models in returned (picker) order with the isDefault entry promoted first,
// then hidden ids appended at the end — hidden ids are still valid --model
// values and must survive so auto-heal cannot migrate agents off them.
func TestCodexModelListExchange_HiddenOrdering(t *testing.T) {
	stdout := codexCannedStdout(strings.Join([]string{
		`{"id":"gpt-5.6-terra","model":"gpt-5.6-terra","displayName":"GPT-5.6 Terra"}`,
		`{"id":"gpt-5.4","model":"gpt-5.4","hidden":true}`,
		`{"id":"gpt-5.6-sol","model":"gpt-5.6-sol","isDefault":true}`,
		`{"id":"gpt-5.5","model":"gpt-5.5"}`,
		`{"id":"gpt-5.4-mini","model":"gpt-5.4-mini","hidden":true}`,
	}, ","))
	var stdin bytes.Buffer
	entries, err := codexModelListExchange(&stdin, stdout)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	got := orderCodexServedModels(entries)
	want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.5", "gpt-5.4", "gpt-5.4-mini"}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The exchange must have requested hidden models explicitly.
	if !strings.Contains(stdin.String(), `"includeHidden":true`) {
		t.Fatalf("model/list request missing includeHidden:true: %s", stdin.String())
	}
	if !strings.Contains(stdin.String(), `"clientInfo"`) {
		t.Fatalf("initialize request missing clientInfo: %s", stdin.String())
	}
}

// TestCodexModelListExchange_Malformed verifies a non-JSON protocol line is a
// hard error (→ fallback), never silently skipped.
func TestCodexModelListExchange_Malformed(t *testing.T) {
	var stdin bytes.Buffer
	if _, err := codexModelListExchange(&stdin, bytes.NewBufferString("garbage\n")); err == nil {
		t.Fatal("expected error on malformed protocol line")
	}
}

// TestCodexModelListExchange_TruncatedStream verifies a stream that ends
// before the model/list response (the shape a killed-on-timeout process
// produces: pipes close, reads hit EOF) surfaces as an error.
func TestCodexModelListExchange_TruncatedStream(t *testing.T) {
	var stdin bytes.Buffer
	stdout := bytes.NewBufferString(`{"id":1,"result":{}}` + "\n")
	if _, err := codexModelListExchange(&stdin, stdout); err == nil {
		t.Fatal("expected error on stream truncated before model/list response")
	}
}

// TestCodexModelListExchange_RPCError verifies an error response to
// model/list is an error, not an empty success.
func TestCodexModelListExchange_RPCError(t *testing.T) {
	var stdin bytes.Buffer
	stdout := bytes.NewBufferString(
		`{"id":1,"result":{}}` + "\n" +
			`{"id":2,"error":{"code":-32601,"message":"nope"}}` + "\n")
	if _, err := codexModelListExchange(&stdin, stdout); err == nil {
		t.Fatal("expected error on model/list error response")
	}
}

// TestCodexModelListExchange_EmptyData verifies zero models is treated as a
// failed probe rather than served as a live empty list.
func TestCodexModelListExchange_EmptyData(t *testing.T) {
	var stdin bytes.Buffer
	if _, err := codexModelListExchange(&stdin, codexCannedStdout("")); err == nil {
		t.Fatal("expected error on empty model/list data")
	}
}

// TestQueryCLIModels_CodexProbeSuccess verifies a successful probe is served
// live (fallback=false) through the full pipeline, including stabilize.
func TestQueryCLIModels_CodexProbeSuccess(t *testing.T) {
	swapCodexProbe(t, func() ([]string, error) {
		return []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.4"}, nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("codex")
	if r.fallback {
		t.Fatalf("live probe result must not be fallback: %+v", r)
	}
	if !equalStrings(r.models, []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.4"}) {
		t.Fatalf("got %v, want probe order preserved", r.models)
	}
}

// TestQueryCLIModels_CodexBinaryMissingFallsBack verifies the no-binary case
// (hosted spokes: the main image ships no codex) serves the static list
// marked fallback=true.
func TestQueryCLIModels_CodexBinaryMissingFallsBack(t *testing.T) {
	swapCodexProbe(t, func() ([]string, error) {
		return nil, errors.New("codex binary not on PATH")
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	r := s.queryCLIModels("codex")
	if !r.fallback {
		t.Fatalf("missing binary must serve fallback, got %+v", r)
	}
	if !equalStrings(r.models, dedupeModels(codexStaticModels)) {
		t.Fatalf("fallback must be the static list, got %v", r.models)
	}
}

// TestQueryCLIModels_CodexTimeoutFallsBack verifies a handshake timeout is a
// plain probe failure → static fallback.
func TestQueryCLIModels_CodexTimeoutFallsBack(t *testing.T) {
	swapCodexProbe(t, func() ([]string, error) {
		return nil, context.DeadlineExceeded
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.queryCLIModels("codex"); !r.fallback || len(r.models) == 0 {
		t.Fatalf("timeout must serve non-empty static fallback, got %+v", r)
	}
}

// TestQueryCLIModels_CodexResultsFeedRetention verifies live codex results
// ride the same stabilize/retention machinery as the other backends: a model
// seen in one live sample survives its absence from the next.
func TestQueryCLIModels_CodexResultsFeedRetention(t *testing.T) {
	sample := []string{"gpt-5.6-sol", "gpt-5.4"}
	swapCodexProbe(t, func() ([]string, error) {
		return append([]string(nil), sample...), nil
	})
	s := &Server{cliModels: newCLIModelCache(), logger: testLogger()}
	if r := s.queryCLIModels("codex"); !contains(r.models, "gpt-5.4") {
		t.Fatalf("first live sample missing id: %v", r.models)
	}
	sample = []string{"gpt-5.6-sol"}
	s.cliModels.entries = map[string]cliModelCacheEntry{}
	r := s.queryCLIModels("codex")
	if r.fallback {
		t.Fatalf("live result must not be fallback: %+v", r)
	}
	if !contains(r.models, "gpt-5.4") {
		t.Fatalf("retention should keep recently-seen id through one miss: %v", r.models)
	}
}

// TestExecCodexModelProbe_BinaryMissing verifies the real prober degrades
// instantly (no hang, no panic) when codex is not on PATH — the hosted-spoke
// situation.
func TestExecCodexModelProbe_BinaryMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := execCodexModelProbe(); err == nil {
		t.Fatal("expected error when codex binary is absent from PATH")
	}
}
