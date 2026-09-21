package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCmdStandbyDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		status   int
		wantPath string
		wantBody map[string]string
		wantText string
	}{
		{
			name:     "lane and key",
			args:     "quality hivecommons/hive#8135",
			status:   http.StatusOK,
			wantPath: "/api/contribute/standby/dispatch",
			wantBody: map[string]string{"lane": "quality", "key": "hivecommons/hive#8135"},
			wantText: "Dispatched standby work for quality",
		},
		{
			name:     "lane only",
			args:     "scanner",
			status:   http.StatusOK,
			wantPath: "/api/contribute/standby/dispatch",
			wantBody: map[string]string{"lane": "scanner", "key": ""},
			wantText: "Dispatched standby work for scanner",
		},
		{
			name:     "dashboard refusal",
			args:     "quality",
			status:   http.StatusForbidden,
			wantPath: "/api/contribute/standby/dispatch",
			wantBody: map[string]string{"lane": "quality", "key": ""},
			wantText: "Failed to dispatch standby work",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotBody map[string]string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				w.WriteHeader(tt.status)
			}))
			defer ts.Close()

			s := newTestBot(ts, "ch")
			s.client = ts.Client()
			got, err := s.cmdStandbyDispatch(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("cmdStandbyDispatch returned error: %v", err)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tt.wantPath)
			}
			for k, want := range tt.wantBody {
				if gotBody[k] != want {
					t.Fatalf("body[%q] = %q, want %q (full body %#v)", k, gotBody[k], want, gotBody)
				}
			}
			if !strings.Contains(got, tt.wantText) {
				t.Fatalf("reply = %q, want substring %q", got, tt.wantText)
			}
		})
	}
}

func TestCmdStandbyDispatchUsage(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	got, err := s.cmdStandbyDispatch(context.Background(), "  ")
	if err != nil {
		t.Fatalf("cmdStandbyDispatch returned error: %v", err)
	}
	if !strings.Contains(got, "Usage") {
		t.Fatalf("reply = %q, want usage", got)
	}
}

func TestCmdStandbyClear(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		status   int
		wantBody map[string]string
		wantText string
	}{
		{
			name:   "required tuple",
			args:   "alice claude opus",
			status: http.StatusOK,
			wantBody: map[string]string{
				"contributor": "alice",
				"backend":     "claude",
				"model":       "opus",
			},
			wantText: "Cleared standby suspension for alice",
		},
		{
			name:   "tuple with effort",
			args:   "alice claude opus high",
			status: http.StatusOK,
			wantBody: map[string]string{
				"contributor":      "alice",
				"backend":          "claude",
				"model":            "opus",
				"reasoning_effort": "high",
			},
			wantText: "Cleared standby suspension for alice",
		},
		{
			name:   "dashboard refusal",
			args:   "alice claude opus",
			status: http.StatusForbidden,
			wantBody: map[string]string{
				"contributor": "alice",
				"backend":     "claude",
				"model":       "opus",
			},
			wantText: "Failed to clear standby suspension",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			var gotBody map[string]string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				w.WriteHeader(tt.status)
			}))
			defer ts.Close()

			s := newTestBot(ts, "ch")
			s.client = ts.Client()
			got, err := s.cmdStandbyClear(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("cmdStandbyClear returned error: %v", err)
			}
			if gotPath != "/api/contribute/standby/clear" {
				t.Fatalf("path = %q, want standby clear endpoint", gotPath)
			}
			for k, want := range tt.wantBody {
				if gotBody[k] != want {
					t.Fatalf("body[%q] = %q, want %q (full body %#v)", k, gotBody[k], want, gotBody)
				}
			}
			if !strings.Contains(got, tt.wantText) {
				t.Fatalf("reply = %q, want substring %q", got, tt.wantText)
			}
		})
	}
}

func TestCmdStandbyClearUsage(t *testing.T) {
	s := NewService(&recordingBackend{}, Config{}, discardLogger())
	got, err := s.cmdStandbyClear(context.Background(), "alice claude")
	if err != nil {
		t.Fatalf("cmdStandbyClear returned error: %v", err)
	}
	if !strings.Contains(got, "Usage") {
		t.Fatalf("reply = %q, want usage", got)
	}
}

func TestRouteMessageStandbyCommands(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantPath string
		wantText string
	}{
		{
			name:     "dispatch",
			message:  "!standby quality hivecommons/hive#8135",
			wantPath: "/api/contribute/standby/dispatch",
			wantText: "Dispatched standby work for quality",
		},
		{
			name:     "clear",
			message:  "!standby-clear alice claude opus high",
			wantPath: "/api/contribute/standby/clear",
			wantText: "Cleared standby suspension for alice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.WriteHeader(http.StatusOK)
			}))
			defer ts.Close()

			s := newTestBot(ts, "ch")
			s.client = ts.Client()
			s.registerBuiltinCommands()
			s.routeMessage(context.Background(), makeMsg("1", tt.message, false))
			var sent []string
			drainQueue(s, &sent)
			if gotPath != tt.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if len(sent) != 1 || !strings.Contains(sent[0], tt.wantText) {
				t.Fatalf("sent = %#v, want one reply containing %q", sent, tt.wantText)
			}
		})
	}
}
