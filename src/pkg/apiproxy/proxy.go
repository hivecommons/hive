package apiproxy

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const (
	defaultUpstream    = "https://api.anthropic.com"
	maxBodyLog         = 64 * 1024 // 64KB max per request/response body in logs
	sseContentType     = "text/event-stream"
	sseDataPrefix      = "data: "
	sseEventMsgStart   = "message_start"
	sseEventMsgDelta   = "message_delta"
	sseEventMsgStop    = "message_stop"
	sseEventContentDlt = "content_block_delta"
	bearerPrefix       = "Bearer "
)

// Event represents a logged API interaction.
type Event struct {
	Timestamp time.Time       `json:"ts"`
	Agent     string          `json:"agent,omitempty"`
	Direction string          `json:"direction"` // "request", "response", or "sse"
	Method    string          `json:"method,omitempty"`
	Path      string          `json:"path,omitempty"`
	Status    int             `json:"status,omitempty"`
	Model     string          `json:"model,omitempty"`
	SSEType   string          `json:"sse_type,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
}

// EventHandler is called for each logged API event.
type EventHandler func(Event)

type Proxy struct {
	upstream     *url.URL
	reverseProxy *httputil.ReverseProxy
	handler      EventHandler
	// apiKey is the upstream credential swapped in on the outbound request.
	apiKey string
	// clientAuthToken is the credential callers must present to this proxy. It
	// is deliberately distinct from apiKey so a configured upstream key is
	// never an implicit credential for anyone who can reach the listener. It
	// is required: when empty every request is refused with 503.
	clientAuthToken string
}

func New(upstreamURL string, handler EventHandler, apiKey, clientAuthToken string) (*Proxy, error) {
	if upstreamURL == "" {
		upstreamURL = defaultUpstream
	}
	u, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	p := &Proxy{
		upstream:        u,
		handler:         handler,
		apiKey:          apiKey,
		clientAuthToken: clientAuthToken,
	}

	p.reverseProxy = &httputil.ReverseProxy{
		Director:       p.director,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
		FlushInterval:  -1, // flush SSE chunks immediately
		Transport: &http.Transport{
			DisableCompression: true, // get plaintext SSE for logging
		},
	}

	return p, nil
}

func isValidAuth(req *http.Request) bool {
	const minKeyLen = 20
	if auth := req.Header.Get("Authorization"); auth != "" && len(auth) > minKeyLen {
		return true
	}
	if key := req.Header.Get("X-Api-Key"); key != "" && !strings.HasPrefix(key, "sk-ant-dummy") && len(key) > minKeyLen {
		return true
	}
	return false
}

// validateClientAuth reports whether the caller presented the configured client
// auth token in either an `Authorization: Bearer` or `X-Api-Key` header. The
// comparison is constant time so the token cannot be recovered through response
// timing.
func validateClientAuth(r *http.Request, expectedToken string) bool {
	if expectedToken == "" {
		return false
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, bearerPrefix) {
		token := strings.TrimPrefix(auth, bearerPrefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) == 1 {
			return true
		}
	}
	key := r.Header.Get("X-Api-Key")
	return key != "" && subtle.ConstantTimeCompare([]byte(key), []byte(expectedToken)) == 1
}

// stripClientAuth removes headers whose value is exactly the proxy-local gate
// token so it is never forwarded upstream.
func stripClientAuth(r *http.Request, token string) {
	if auth := r.Header.Get("Authorization"); strings.TrimPrefix(auth, bearerPrefix) == token && strings.HasPrefix(auth, bearerPrefix) {
		r.Header.Del("Authorization")
	}
	if r.Header.Get("X-Api-Key") == token {
		r.Header.Del("X-Api-Key")
	}
}

func (p *Proxy) director(req *http.Request) {
	req.URL.Scheme = p.upstream.Scheme
	req.URL.Host = p.upstream.Host
	req.Host = p.upstream.Host

	// Remove client's Accept-Encoding so upstream sends uncompressed SSE.
	// Negligible overhead for a local proxy; allows line-by-line SSE parsing.
	req.Header.Del("Accept-Encoding")

	// Swap in the upstream key when the caller did not bring an upstream
	// credential of its own. Gate credentials have already been stripped by
	// ServeHTTP, so anything left here is a caller-supplied upstream key.
	if p.apiKey != "" && !isValidAuth(req) {
		req.Header.Set("Authorization", bearerPrefix+p.apiKey)
		req.Header.Del("X-Api-Key")
	}
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	if p.handler == nil {
		return nil
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, sseContentType) {
		resp.Body = p.wrapSSEBody(resp.Body, resp.Request.URL.Path, resp.StatusCode)
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

// wrapSSEBody wraps a streaming response body, emitting an Event for each
// meaningful SSE data line as it passes through to the client.
func (p *Proxy) wrapSSEBody(orig io.ReadCloser, path string, status int) io.ReadCloser {
	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = orig.Close() }() // upstream response body, already fully read here
		defer func() { _ = pw.Close() }()   // io.PipeWriter.Close never returns a non-nil error

		lineCount := 0
		scanner := bufio.NewScanner(orig)
		scanner.Buffer(make([]byte, maxBodyLog), maxBodyLog)

		var (
			totalBytes int
			model      string
			textParts  []string
		)

		for scanner.Scan() {
			line := scanner.Bytes()
			lineCount++
			totalBytes += len(line) + 1

			if _, err := pw.Write(line); err != nil {
				return
			}
			if _, err := pw.Write([]byte("\n")); err != nil {
				return
			}

			if !bytes.HasPrefix(line, []byte(sseDataPrefix)) {
				continue
			}
			data := line[len(sseDataPrefix):]
			if len(data) == 0 || string(data) == "[DONE]" {
				continue
			}

			var sseData struct {
				Type    string `json:"type"`
				Message *struct {
					Model string `json:"model"`
					Usage *struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"message"`
				Delta *struct {
					Type       string `json:"type"`
					Text       string `json:"text"`
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage *struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(data, &sseData) != nil {
				continue
			}

			switch sseData.Type {
			case sseEventMsgStart:
				if sseData.Message != nil {
					model = sseData.Message.Model
				}
				p.handler(Event{
					Timestamp: time.Now().UTC(),
					Direction: "sse",
					Path:      path,
					Status:    status,
					SSEType:   sseEventMsgStart,
					Model:     model,
					Body:      json.RawMessage(data),
				})

			case sseEventContentDlt:
				if sseData.Delta != nil && sseData.Delta.Text != "" {
					textParts = append(textParts, sseData.Delta.Text)
				}

			case sseEventMsgDelta:
				p.handler(Event{
					Timestamp: time.Now().UTC(),
					Direction: "sse",
					Path:      path,
					SSEType:   sseEventMsgDelta,
					Model:     model,
					Body:      json.RawMessage(data),
				})

			case sseEventMsgStop:
				fullText := strings.Join(textParts, "")
				summary := map[string]interface{}{
					"type":        "stream_complete",
					"model":       model,
					"text_length": len(fullText),
					"total_bytes": totalBytes,
				}
				if len(fullText) <= maxBodyLog {
					summary["text"] = fullText
				}
				summaryJSON, _ := json.Marshal(summary)
				p.handler(Event{
					Timestamp: time.Now().UTC(),
					Direction: "sse",
					Path:      path,
					SSEType:   sseEventMsgStop,
					Model:     model,
					Body:      json.RawMessage(summaryJSON),
				})
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[apiproxy] SSE scanner error: %v", err)
		}
	}()

	return pr
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("[apiproxy] upstream error: %v", err)
	http.Error(w, "proxy upstream error", http.StatusBadGateway)
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Fail closed when no gate token is configured: an unauthenticated relay
	// would let any co-resident loopback process spend the host upstream key.
	if p.clientAuthToken == "" {
		http.Error(w, "proxy misconfigured: client auth token required", http.StatusServiceUnavailable)
		return
	}

	// Gate on the client-provided auth token, which is separate from the
	// upstream key: a configured upstream key must never act as an implicit
	// credential for anyone who can reach this listener. Rejected requests are
	// neither logged nor forwarded upstream.
	if !validateClientAuth(r, p.clientAuthToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// The gate token is proxy-local and must never reach the upstream. Once
	// stripped, any remaining credential is a caller-supplied upstream key
	// and the director decides whether to swap in the configured one.
	stripClientAuth(r, p.clientAuthToken)

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
