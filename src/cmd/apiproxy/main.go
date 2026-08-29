package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/kubestellar/hive/pkg/apiproxy"
)

const defaultProxyHost = "127.0.0.1"

// errMissingClientAuthToken reports the fail-closed startup contract: without a
// client auth token any co-resident loopback process could have its requests
// fulfilled with the host upstream key, so the proxy refuses to start.
var errMissingClientAuthToken = errors.New("PROXY_AUTH_TOKEN is required: without it the proxy would accept unauthenticated callers and grant them the host upstream key")

// clientAuthTokenFromEnv returns the token callers must present to the proxy,
// or an error when it is unset so the caller can fail closed.
func clientAuthTokenFromEnv(getenv func(string) string) (string, error) {
	token := getenv("PROXY_AUTH_TOKEN")
	if token == "" {
		return "", errMissingClientAuthToken
	}
	return token, nil
}

// upstreamAPIKeyFromEnv returns the credential the proxy swaps in on the
// outbound request when the caller has no upstream credential of its own.
func upstreamAPIKeyFromEnv(getenv func(string) string) string {
	return getenv("ANTHROPIC_API_KEY")
}

func main() {
	port := flag.Int("port", 9000, "port to listen on")
	host := flag.String("host", defaultProxyHost, "host address to listen on (default localhost; set to 0.0.0.0 to expose externally)")
	upstream := flag.String("upstream", "https://api.anthropic.com", "upstream API URL")
	logFile := flag.String("log", "", "log file path (default: stdout)")
	flag.Parse()

	var logWriter *json.Encoder
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatalf("failed to open log file: %v", err)
		}
		defer func() { _ = f.Close() }() // process runs under ListenAndServe until killed; defer is unreachable in normal operation
		logWriter = json.NewEncoder(f)
	} else {
		logWriter = json.NewEncoder(os.Stdout)
	}

	handler := func(evt apiproxy.Event) {
		entry := struct {
			Timestamp string          `json:"ts"`
			Agent     string          `json:"agent,omitempty"`
			Direction string          `json:"direction"`
			Method    string          `json:"method,omitempty"`
			Path      string          `json:"path"`
			Status    int             `json:"status,omitempty"`
			Model     string          `json:"model,omitempty"`
			SSEType   string          `json:"sse_type,omitempty"`
			BodySize  int             `json:"body_size"`
			Body      json.RawMessage `json:"body,omitempty"`
		}{
			Timestamp: evt.Timestamp.Format(time.RFC3339),
			Agent:     evt.Agent,
			Direction: evt.Direction,
			Method:    evt.Method,
			Path:      evt.Path,
			Status:    evt.Status,
			Model:     evt.Model,
			SSEType:   evt.SSEType,
			BodySize:  len(evt.Body),
		}
		if evt.SSEType != "" && len(evt.Body) > 0 {
			entry.Body = evt.Body
		}
		if err := logWriter.Encode(entry); err != nil {
			log.Printf("apiproxy: failed to write event log entry: %v", err)
		}
	}

	clientAuthToken, err := clientAuthTokenFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("[sec-check] refusing to start: %v", err)
	}
	upstreamAPIKey := upstreamAPIKeyFromEnv(os.Getenv)
	proxy, err := apiproxy.New(*upstream, handler, upstreamAPIKey, clientAuthToken)
	if err != nil {
		log.Fatalf("failed to create proxy: %v", err)
	}

	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Printf("[apiproxy] listening on %s → %s", addr, *upstream)
	if err := http.ListenAndServe(addr, proxy); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
