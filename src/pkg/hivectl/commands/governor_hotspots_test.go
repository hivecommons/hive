package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGovernorHotspotsCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hotspots" || r.URL.Query().Get("repo") != "acme/widget" {
			t.Fatalf("request = %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"repo":"acme/widget","count":1,"hotspots":[{"file":"pkg/a.go","prs":7}]}`)
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "governor", "hotspots", "--repo", "acme/widget")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"file": "pkg/a.go"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}
