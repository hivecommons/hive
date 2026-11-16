package meetllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func modelServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "" || len(req.Messages) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

func TestRunHappyPathRecordsUsage(t *testing.T) {
	srv, auth := modelServer(t, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"the answer"}}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`)
	dir := t.TempDir()
	usage := filepath.Join(dir, "usage.jsonl")
	e := env(map[string]string{
		EnvEndpoint: srv.URL + "/", EnvModel: "m", EnvAPIKey: "secret-key", EnvUsageFile: usage,
		EnvSlotsDir: filepath.Join(dir, "slots"), EnvMaxInFlight: "2", EnvTimeoutMS: "5000", EnvMaxTokens: "100",
	})
	var out, errb bytes.Buffer
	if code := Run(nil, strings.NewReader("prompt"), &out, &errb, e); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if out.String() != "the answer" || *auth != "Bearer secret-key" {
		t.Fatalf("out %q auth %q", out.String(), *auth)
	}
	if total, err := UsageTotal(usage); err != nil || total != 12 {
		t.Fatalf("usage %d, %v", total, err)
	}
	if strings.Contains(errb.String(), "secret-key") {
		t.Fatal("the key must never be printed")
	}
	// The budget stops further calls.
	e2 := env(map[string]string{EnvEndpoint: srv.URL, EnvModel: "m", EnvUsageFile: usage, EnvMaxTokens: "10"})
	out.Reset()
	errb.Reset()
	if code := Run(nil, strings.NewReader("p"), &out, &errb, e2); code != 1 || out.Len() != 0 || !strings.Contains(errb.String(), "budget") {
		t.Fatalf("over budget exit %d out %q err %q", code, out.String(), errb.String())
	}
}

func TestRunErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run(nil, strings.NewReader(""), &out, &errb, env(nil)); code != 2 {
		t.Fatalf("missing config exit %d", code)
	}
	srv, _ := modelServer(t, http.StatusInternalServerError, `{}`)
	if code := Run(nil, strings.NewReader("p"), &out, &errb, env(map[string]string{EnvEndpoint: srv.URL, EnvModel: "m"})); code != 1 {
		t.Fatalf("model error exit %d", code)
	}
	if code := Run(nil, errReader{}, &out, &errb, env(map[string]string{EnvEndpoint: srv.URL, EnvModel: "m"})); code != 1 {
		t.Fatalf("stdin error exit %d", code)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestCompleteShapes(t *testing.T) {
	ctx := context.Background()
	base := Config{Model: "m", Timeout: 5 * time.Second, MaxInFlight: 1}
	cases := map[string]string{"no choices": `{"choices":[],"usage":{"total_tokens":3}}`, "bad json": `{`}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := modelServer(t, http.StatusOK, body)
			c := base
			c.Endpoint = srv.URL + "/v1"
			if _, err := Complete(ctx, c, http.DefaultClient, "p"); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	c := base
	c.Endpoint = "http://127.0.0.1:1"
	if _, err := Complete(ctx, c, http.DefaultClient, "p"); err == nil {
		t.Fatal("want a transport error")
	}
	c.Endpoint = "http://bad host"
	if _, err := Complete(ctx, c, http.DefaultClient, "p"); err == nil {
		t.Fatal("want a request error")
	}
	if chatURL("https://x/chat/completions") != "https://x/chat/completions" || chatURL("https://x/v1") != "https://x/v1/chat/completions" {
		t.Fatal("chatURL")
	}
	// Usage cannot be recorded into a directory.
	srv, _ := modelServer(t, http.StatusOK, `{"choices":[{"message":{"content":"x"}}],"usage":{"total_tokens":1}}`)
	c = base
	c.Endpoint = srv.URL
	c.UsageFile = t.TempDir()
	if _, err := Complete(ctx, c, http.DefaultClient, "p"); err == nil {
		t.Fatal("want a usage error")
	}
	// An unreadable usage file blocks a budgeted call.
	c.MaxTokens = 5
	_ = os.MkdirAll(filepath.Join(c.UsageFile, "x"), 0o700)
	if _, err := Complete(ctx, c, http.DefaultClient, "p"); err == nil {
		t.Fatal("want a usage read error")
	}
}

func TestUsageTotal(t *testing.T) {
	p := filepath.Join(t.TempDir(), "u")
	if n, err := UsageTotal(p); err != nil || n != 0 {
		t.Fatal("missing file is zero")
	}
	_ = os.WriteFile(p, []byte("{\"tokens\":3}\ngarbage\n{\"tokens\":-1}\n{\"tokens\":4}\n"), 0o600)
	if n, _ := UsageTotal(p); n != 7 {
		t.Fatalf("total %d", n)
	}
	if err := recordUsage("", 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := recordUsage(p, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSlots(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slots")
	rel, err := acquire(context.Background(), dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := acquire(ctx, dir, 1); err == nil {
		t.Fatal("a taken slot must block until the deadline")
	}
	rel()
	rel2, err := acquire(context.Background(), dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if rel3, err := acquire(context.Background(), "", 1); err != nil {
		t.Fatal(err)
	} else {
		rel3()
	}
	blocker := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocker, []byte("x"), 0o600)
	if _, err := acquire(context.Background(), filepath.Join(blocker, "d"), 1); err == nil {
		t.Fatal("want a mkdir error")
	}
	// A slot path that is a directory cannot be opened.
	sd := t.TempDir()
	_ = os.MkdirAll(filepath.Join(sd, "slot-0"), 0o700)
	if _, err := acquire(context.Background(), sd, 1); err == nil {
		t.Fatal("want an open error")
	}
	// A busy slot during Complete times out.
	busy := filepath.Join(t.TempDir(), "busy")
	hold, _ := acquire(context.Background(), busy, 1)
	defer hold()
	if _, err := Complete(context.Background(), Config{Endpoint: "http://127.0.0.1:1", Model: "m", Timeout: 50 * time.Millisecond, SlotsDir: busy, MaxInFlight: 1}, http.DefaultClient, "p"); err == nil || !strings.Contains(err.Error(), "slot") {
		t.Fatalf("busy slot = %v", err)
	}
}

func TestCompleteSendsSystemPrompt(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	c := Config{Endpoint: srv.URL, Model: "m", Timeout: 5 * time.Second, MaxInFlight: 1, System: "be brief"}
	if out, err := Complete(context.Background(), c, srv.Client(), "q"); err != nil || out != "ok" {
		t.Fatalf("complete %q, %v", out, err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != "be brief" || got.Messages[1].Role != "user" {
		t.Fatalf("messages %+v", got.Messages)
	}
	c.System = "  "
	if _, err := Complete(context.Background(), c, srv.Client(), "q"); err != nil || len(got.Messages) != 1 {
		t.Fatalf("blank system prompt must send one message: %+v, %v", got.Messages, err)
	}
}
