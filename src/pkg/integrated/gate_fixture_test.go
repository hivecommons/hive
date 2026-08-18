package integrated

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// newIntegratedGateTestServer models the empty review inventory for older
// integration fixtures whose purpose is not review-state behavior. Gate
// inspection always reads reviews and must fail closed when that API is
// unavailable.
func newIntegratedGateTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/") && strings.HasSuffix(request.URL.Path, "/reviews") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `[]`)
			return
		}
		handler(writer, request)
	}))
}
