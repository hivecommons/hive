package proxy

import (
	"fmt"
	"net/url"
	"strings"
)

// inferenceBackendErrorMessage builds the agent-facing text for a non-2xx
// answer from the inference gateway. The literal prefix
// "inference backend returned <status>:" is parsed by agent/pane_classify.go
// and the dashboard's alerting, so it must stay first and unchanged; the
// upstream URL and responding Server header are appended AFTER the body so an
// operator can tell which gateway actually answered (#9945: a 401 from a
// different gateway than the one Test Connection probed was indistinguishable).
// Userinfo and query are stripped from the URL so credentials never leak.
func inferenceBackendErrorMessage(status int, body []byte, upstreamURL, serverHdr string) string {
	msg := fmt.Sprintf("inference backend returned %d: %s", status, truncateBytes(body, 200))
	return msg + inferenceUpstreamSuffix(upstreamURL, serverHdr)
}

// inferenceBackendUnreachableMessage is the transport-failure counterpart.
func inferenceBackendUnreachableMessage(err error, upstreamURL string) string {
	return "inference backend unreachable: " + err.Error() + inferenceUpstreamSuffix(upstreamURL, "")
}

func inferenceUpstreamSuffix(upstreamURL, serverHdr string) string {
	var parts []string
	if u := redactUpstreamURL(upstreamURL); u != "" {
		parts = append(parts, "upstream: "+u)
	}
	if s := strings.TrimSpace(serverHdr); s != "" {
		parts = append(parts, "server: "+s)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func redactUpstreamURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}
