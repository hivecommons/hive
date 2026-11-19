package dashboard

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
)

// The hosted Ingress in front of a hub-proxied hive intercepts 502 and 503
// (custom-http-errors "502,503", pkg/hub/saas_provision.go) and replaces the
// body with the generic "hive unavailable" page, so the owner is told the
// hive is restarting. That page is right when the spoke is unreachable (the
// Ingress itself produced the status), and wrong when the spoke answered a
// 502 carrying its own error: a Discord join Quorum refused for a missing
// permission read as "hive is restarting" (hivecommons/hive#18598), as a
// GitHub 422 on a hold label once did (#17037).
//
// withHostedGatewayPassthrough fixes that for every API route at once: on a
// request that came through the hosted Ingress (it carries the hub's
// X-Hive-Proxy-Auth proof), a JSON 502 a handler writes goes out as 424
// Failed Dependency, which the Ingress passes through with its body, and the
// original status rides along in X-Hive-Status. A spoke the Ingress cannot
// reach still gets the generic page. 503 is left alone: the dashboard's own
// clients read a spoke 503 ("not enabled") by status, and the generic page's
// retry is the right answer for a hive that is starting.
const (
	// hostedGatewayPassthroughStatus is the status a spoke-answered JSON 502
	// is sent under on a hosted request; the Ingress does not intercept it.
	hostedGatewayPassthroughStatus = http.StatusFailedDependency
	// hostedGatewayOriginalStatusHeader names the status the handler wrote.
	hostedGatewayOriginalStatusHeader = "X-Hive-Status"
)

// isHostedGatewayRequest reports whether r came through the hosted Ingress
// to an API route. The proof header's presence is enough here: this only
// picks the status an error is sent under, it grants nothing.
func isHostedGatewayRequest(r *http.Request) bool {
	return r.Header.Get(proxyAuthHeader) != "" && strings.HasPrefix(r.URL.Path, "/api/")
}

func (s *Server) withHostedGatewayPassthrough(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isHostedGatewayRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&hostedGatewayStatusWriter{ResponseWriter: w}, r)
	})
}

// hostedGatewayStatusWriter rewrites a JSON 502 to the pass-through status.
type hostedGatewayStatusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *hostedGatewayStatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *hostedGatewayStatusWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		if status == http.StatusBadGateway && isJSONContentType(w.Header().Get("Content-Type")) {
			w.Header().Set(hostedGatewayOriginalStatusHeader, "502")
			status = hostedGatewayPassthroughStatus
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func isJSONContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json")
}

func (w *hostedGatewayStatusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *hostedGatewayStatusWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *hostedGatewayStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (w *hostedGatewayStatusWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}
