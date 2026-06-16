package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/kubestellar/hive/v2/pkg/apiproxy"
)

func main() {
	port := flag.Int("port", 9000, "port to listen on")
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

	authToken := os.Getenv("PROXY_AUTH_TOKEN")
	if authToken == "" {
		authToken = os.Getenv("ANTHROPIC_API_KEY")
	}
	proxy, err := apiproxy.New(*upstream, handler, authToken)
	if err != nil {
		log.Fatalf("failed to create proxy: %v", err)
	}

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("[apiproxy] listening on %s → %s", addr, *upstream)
	if err := http.ListenAndServe(addr, proxy); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
