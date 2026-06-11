package apiproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

const (
	defaultUpstream = "https://api.anthropic.com"
	maxBodyLog      = 64 * 1024 // 64KB max per request/response body in logs
)

// Event represents a logged API interaction.
type Event struct {
	Timestamp time.Time       `json:"ts"`
	Agent     string          `json:"agent,omitempty"`
	Direction string          `json:"direction"` // "request" or "response"
	Method    string          `json:"method,omitempty"`
	Path      string          `json:"path,omitempty"`
	Status    int             `json:"status,omitempty"`
	Model     string          `json:"model,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// EventHandler is called for each logged API event.
type EventHandler func(Event)

// Proxy is a pass-through HTTP proxy for the Anthropic Messages API.
// It forwards requests to the upstream API and logs structured events.
type Proxy struct {
	upstream     *url.URL
	reverseProxy *httputil.ReverseProxy
	handler      EventHandler
	mu           sync.RWMutex
}

// New creates a new Anthropic API proxy.
// upstreamURL defaults to https://api.anthropic.com if empty.
func New(upstreamURL string, handler EventHandler) (*Proxy, error) {
	if upstreamURL == "" {
		upstreamURL = defaultUpstream
	}
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	p := &Proxy{
		upstream: u,
		handler:  handler,
	}

	p.reverseProxy = &httputil.ReverseProxy{
		Director:       p.director,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
	}

	return p, nil
}

func (p *Proxy) director(req *http.Request) {
	req.URL.Scheme = p.upstream.Scheme
	req.URL.Host = p.upstream.Host
	req.Host = p.upstream.Host
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	if p.handler == nil {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyLog))
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), resp.Body))

	evt := Event{
		Timestamp: time.Now().UTC(),
		Direction: "response",
		Path:      resp.Request.URL.Path,
		Status:    resp.StatusCode,
	}

	if json.Valid(body) {
		evt.Body = json.RawMessage(body)
		var respBody struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(body, &respBody) == nil {
			evt.Model = respBody.Model
		}
	}

	p.handler(evt)
	return nil
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[apiproxy] upstream error: %v", err)
	http.Error(w, "proxy upstream error", http.StatusBadGateway)
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.handler != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyLog))
		if err == nil {
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))

			evt := Event{
				Timestamp: time.Now().UTC(),
				Direction: "request",
				Method:    r.Method,
				Path:      r.URL.Path,
				Agent:     r.Header.Get("X-Hive-Agent"),
			}

			if json.Valid(body) {
				evt.Body = json.RawMessage(body)
				var reqBody struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(body, &reqBody) == nil {
					evt.Model = reqBody.Model
				}
			}

			p.handler(evt)
		}
	}

	p.reverseProxy.ServeHTTP(w, r)
}
