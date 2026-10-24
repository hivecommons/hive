package commands

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContributeTokenCommands(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		method, path string
	}{
		{[]string{"list"}, http.MethodGet, "/api/contributors"},
		{[]string{"revoke", "alice"}, http.MethodPost, "/api/contributors/alice/token/revoke"},
		{[]string{"revoke", "--all", "--yes"}, http.MethodPost, "/api/contributors/tokens/revoke-all"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"revoked":1}`))
			}))
			defer server.Close()
			args := append([]string{"--output", "json", "contribute", "tokens"}, tc.args...)
			if _, _, err := execute(t, server, "", args...); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestContributeBulkRevokeRequiresConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unconfirmed revoke reached API") }))
	defer server.Close()
	for _, args := range [][]string{{"--all"}, {"--all", "--yes", "alice"}, {}} {
		cmd := append([]string{"contribute", "tokens", "revoke"}, args...)
		if _, _, err := execute(t, server, "", cmd...); err == nil {
			t.Fatal("invalid revoke accepted")
		}
	}
}

func TestContributeLoginUsesActiveProfile(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "contribute", "login"); err != nil {
		t.Fatalf("contribute login: %v", err)
	}
	if len(h.reissuer.calls) != 1 || !strings.HasSuffix(h.reissuer.calls[0], " gh-token") || !strings.Contains(h.reissuer.calls[0], "acme.example") {
		t.Fatalf("reissue calls = %v", h.reissuer.calls)
	}
	if got := h.profiles(t).Profiles[0].RegistrationToken; got != "reissued-token" {
		t.Fatalf("active token = %q", got)
	}
}

func TestContributeLoginExplicitProfile(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "contribute", "login", "other"); err != nil {
		t.Fatalf("contribute login other: %v", err)
	}
	if len(h.reissuer.calls) != 1 || !strings.Contains(h.reissuer.calls[0], "other.example") {
		t.Fatalf("reissue calls = %v", h.reissuer.calls)
	}
	set := h.profiles(t)
	for _, p := range set.Profiles {
		if p.Name == "other" && p.RegistrationToken != "reissued-token" {
			t.Fatalf("other token = %q", p.RegistrationToken)
		}
		if p.Name == "acme" && p.RegistrationToken != "tok-acme" {
			t.Fatalf("acme token changed to %q", p.RegistrationToken)
		}
	}
}

func TestContributeLoginNoActiveProfile(t *testing.T) {
	h := newHivesHarness(t)
	set := twoHives()
	set.Active = ""
	h.seed(t, set)
	err := h.run(t, "", "contribute", "login")
	var usage *usageError
	if !errors.As(err, &usage) {
		t.Fatalf("err = %v, want usageError", err)
	}
	if len(h.reissuer.calls) != 0 {
		t.Fatalf("reissue called without an active profile: %v", h.reissuer.calls)
	}
}

func TestContributeLoginErrors(t *testing.T) {
	t.Run("no profiles configured", func(t *testing.T) {
		h := newHivesHarness(t)
		if err := h.run(t, "", "contribute", "login"); err == nil || !strings.Contains(err.Error(), "no hives configured") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dependencies unavailable", func(t *testing.T) {
		newHivesHarness(t)
		hivesDepsFor = func(*commandEnv) (*hivesDeps, error) { return nil, errors.New("dependencies unavailable") }
		root := NewRootCommand(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
		root.SetArgs([]string{"contribute", "login"})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "dependencies unavailable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reissue fails", func(t *testing.T) {
		h := newHivesHarness(t)
		h.seed(t, twoHives())
		h.reissuer.err = errors.New("hub unavailable")
		if err := h.run(t, "", "contribute", "login", "acme"); err == nil || !strings.Contains(err.Error(), "hub unavailable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("invalid profile name", func(t *testing.T) {
		h := newHivesHarness(t)
		h.seed(t, twoHives())
		var usage *usageError
		if err := h.run(t, "", "contribute", "login", "bad name!"); !errors.As(err, &usage) {
			t.Fatalf("err = %v, want usageError", err)
		}
	})
}
