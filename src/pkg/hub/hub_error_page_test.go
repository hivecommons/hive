package hub

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestHubMissingPathReturnsFriendly404(t *testing.T) {
	s := newHubServerForTest(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/cncf-reference-architecture.html", nil)
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing path status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Page Not Found", `data-status-code="404"`, "Go to Hive Hub"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing path body does not contain %q:\n%s", want, body)
		}
	}
}

func TestHubStatusCarryingErrorPagesReturnTheirStatus(t *testing.T) {
	s := newHubServerForTest(t)

	for _, code := range []int{
		http.StatusForbidden,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/error/"+strconv.Itoa(code), nil)
			s.mux.ServeHTTP(rec, req)

			if rec.Code != code {
				t.Fatalf("status = %d, want %d", rec.Code, code)
			}
			if body := rec.Body.String(); !strings.Contains(body, `data-status-code="`+strconv.Itoa(code)+`"`) {
				t.Fatalf("body did not carry status %d:\n%s", code, body)
			}
		})
	}
}

func TestHubStaticReferencesCurrentHost(t *testing.T) {
	retiredHost := "hive." + "kubestellar.io"

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), retiredHost) {
			t.Errorf("%s still references retired hub host %s", path, retiredHost)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
