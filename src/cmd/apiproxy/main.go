package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/kubestellar/hive/pkg/apiproxy"
)

const defaultProxyHost = "127.0.0.1"

// clientAuthTokenFromEnv returns the token callers must present to the proxy.
// Fails closed: an empty token means any loopback-local process could have
// upstream API requests fulfilled with the host's API key, so the caller
// must always configure PROXY_AUTH_TOKEN.
func clientAuthTokenFromEnv(getenv func(string) string) (string, error) {
	token := getenv("PROXY_AUTH_TOKEN")
	if token == "" {
		return "", fmt.Errorf("PROXY_AUTH_TOKEN is required but not set; refusing to start an unauthenticated proxy that could expose the host upstream API key to unprivileged callers")
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
		defer f.Close()
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
		logWriter.Encode(entry)
	}

	clientAuthToken, err := clientAuthTokenFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("[apiproxy] startup aborted: %v", err)
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
