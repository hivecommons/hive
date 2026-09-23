package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClaimRefTarget(t *testing.T) {
	for in, want := range map[string]string{
		"hivecommons/hive#8380":                           "/api/claims/hivecommons/hive/8380",
		"https://github.com/hivecommons/hive/issues/8380": "/api/claims/hivecommons/hive/8380",
	} {
		got, err := claimRefTarget([]string{in})
		if err != nil || got != want {
			t.Fatalf("%q → %q, %v (want %q)", in, got, err, want)
		}
	}
	if got, err := claimRefTarget([]string{"hivecommons/hive", "42"}); err != nil || got != "/api/claims/hivecommons/hive/42" {
		t.Fatalf("two-arg form: %q %v", got, err)
	}
	for _, bad := range []string{"hive#1", "o/r#0", "o/r", "o/r#x"} {
		if _, err := claimRefTarget([]string{bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		} else if ExitCode(err) != ExitUsage {
			t.Fatalf("%q: exit=%d want usage", bad, ExitCode(err))
		}
	}
}

func TestClaimCommandPostsForceAndTTL(t *testing.T) {
	var got map[string]any
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"outcome":"claimed","changed":true}`)
	}))
	defer server.Close()

	out, _, err := execute(t, server, "", "claim", "o/r#7", "--until", "2h", "--session", "laptop", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/claims/o/r/7" {
		t.Fatalf("request = %s %s", method, path)
	}
	if got["force"] != false || got["ttl_s"] != float64(7200) || got["session"] != "laptop" {
		t.Fatalf("body = %#v", got)
	}
	if !strings.Contains(out, `"claimed"`) {
		t.Fatalf("stdout = %q", out)
	}

	if _, _, err := execute(t, server, "", "takeover", "o/r#7"); err != nil {
		t.Fatal(err)
	}
	if got["force"] != true {
		t.Fatalf("takeover alias did not force: %#v", got)
	}
}

func TestClaimCommandSurfacesHeldHint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"outcome":"held","changed":false,"hint":"held by bob (human) at the same rank — re-run with force to take it over"}`)
	}))
	defer server.Close()

	out, _, err := execute(t, server, "", "claim", "o/r#7", "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "re-run with force") {
		t.Fatalf("err = %v", err)
	}
	if ExitCode(err) != ExitAPI {
		t.Fatalf("exit = %d want %d", ExitCode(err), ExitAPI)
	}
	if !strings.Contains(out, `"held"`) {
		t.Fatalf("structured body not printed: %q", out)
	}
}

func TestUnclaimAndClaimsCommands(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, r.Method+" "+r.URL.Path+" force="+strings.TrimSpace(strings.Trim(jsonString(body["force"]), `"`)))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"released":true,"enabled":true,"claims":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "unclaim", "o/r#7", "--force", "--reason", "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, server, "", "claims", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "DELETE /api/claims/o/r/7 force=true" || !strings.HasPrefix(calls[1], "GET /api/claims") {
		t.Fatalf("calls = %v", calls)
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
