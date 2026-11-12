package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// AuthState describes a connector's external sign-in. Connectors that own
// their credential flow (OAuth through a vendor MCP server) report it here;
// the dashboard renders Connect/Disconnect from it. Tokens never appear.
type AuthState struct {
	// Connected is true when the upstream reports a signed-in account.
	Connected bool `json:"connected"`
	// Account is the signed-in identity (email or display name), if known.
	Account string `json:"account,omitempty"`
	// Pending is true while a login awaits the user's browser authorization.
	Pending bool `json:"pending"`
	// AuthURL is the authorization URL the user must open, while pending.
	AuthURL string `json:"auth_url,omitempty"`
	// Message is the last human-readable login/logout outcome.
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// Authenticator is optionally implemented by connectors whose credential is
// obtained interactively rather than from auth.env/auth.file.
type Authenticator interface {
	// AuthStatus reports the current sign-in state. It may contact the
	// upstream when refresh is true; otherwise it returns the cached state.
	AuthStatus(ctx context.Context, refresh bool) AuthState
	// Connect begins the sign-in flow and returns the state, including the
	// URL the user must open when the flow needs a browser.
	Connect(ctx context.Context) (AuthState, error)
	// Disconnect signs out and revokes the stored credential upstream.
	Disconnect(ctx context.Context) (AuthState, error)
}

var _ Authenticator = (*plaudConnector)(nil)

// AuthStatus implements Authenticator.
func (c *plaudConnector) AuthStatus(ctx context.Context, refresh bool) AuthState {
	c.mu.Lock()
	st := c.auth
	login := c.login
	c.mu.Unlock()
	if login != nil {
		select {
		case <-login.done:
			st.Pending = false
			if login.result != "" {
				st.Message = firstLine(login.result)
			}
		default:
			st.Pending = true
			if st.AuthURL == "" {
				st.AuthURL = firstHTTPSURL(login.client.Notices())
			}
		}
	}
	if !refresh || st.Pending {
		return st
	}
	client, err := c.dial(ctx)
	if err != nil {
		st.Message = err.Error()
		st.CheckedAt = c.now().UTC()
		c.setAuth(st)
		return st
	}
	defer func() { _ = client.Close() }()
	c.refreshAuth(ctx, client)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth
}

// refreshAuth asks get_current_user and caches the answer.
func (c *plaudConnector) refreshAuth(ctx context.Context, client mcpClient) {
	st := AuthState{CheckedAt: c.now().UTC()}
	text, err := client.Call(ctx, "get_current_user", nil)
	switch {
	case err != nil:
		st.Message = firstLine(err.Error())
	default:
		st.Connected = true
		st.Account = plaudAccountName(text)
	}
	c.setAuth(st)
}

func (c *plaudConnector) setAuth(st AuthState) {
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
}

// Connect implements Authenticator: it starts a long-lived MCP session,
// calls the server's login tool and keeps the process alive for
// plaudLoginWindow so the OAuth callback (localhost:8199) can land. The
// authorization URL is taken from the tool reply or the server's log output.
func (c *plaudConnector) Connect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	if l := c.login; l != nil {
		select {
		case <-l.done:
			c.login = nil
		default:
			c.mu.Unlock()
			return c.AuthStatus(ctx, false), nil
		}
	}
	c.mu.Unlock()

	loginCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plaudLoginWindow)
	client, err := c.dial(loginCtx)
	if err != nil {
		cancel()
		return c.AuthStatus(ctx, false), err
	}
	l := &plaudLogin{client: client, cancel: cancel, done: make(chan struct{})}
	c.mu.Lock()
	c.login = l
	c.auth = AuthState{Pending: true, CheckedAt: c.now().UTC()}
	c.mu.Unlock()

	go c.runLogin(loginCtx, l)

	timer := time.NewTimer(plaudLoginReplyWait)
	defer timer.Stop()
	select {
	case <-l.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return c.AuthStatus(ctx, false), nil
}

func (c *plaudConnector) runLogin(ctx context.Context, l *plaudLogin) {
	defer close(l.done)
	defer l.cancel()
	text, err := l.client.Call(ctx, "login", nil)
	if err != nil {
		text = err.Error()
	}
	l.result = text
	st := AuthState{CheckedAt: c.now().UTC(), Message: firstLine(text)}
	if url := firstHTTPSURL(text); url != "" && err == nil {
		// The tool answered with the URL only: the callback still has to
		// arrive, so keep the session alive until the window closes.
		c.mu.Lock()
		c.auth.AuthURL = url
		c.auth.Pending = true
		c.auth.Message = ""
		c.mu.Unlock()
		if !c.waitForLogin(ctx, l.client) {
			st.Message = "authorization not completed before the login window closed"
			st.AuthURL = ""
		} else {
			c.refreshAuth(ctx, l.client)
			_ = l.client.Close()
			c.clearLogin(l)
			return
		}
	} else if err == nil {
		c.refreshAuth(ctx, l.client)
		_ = l.client.Close()
		c.clearLogin(l)
		return
	}
	_ = l.client.Close()
	c.mu.Lock()
	c.auth = st
	c.mu.Unlock()
	c.clearLogin(l)
}

// waitForLogin polls get_current_user until it succeeds or ctx expires.
func (c *plaudConnector) waitForLogin(ctx context.Context, client mcpClient) bool {
	for {
		if _, err := client.Call(ctx, "get_current_user", nil); err == nil {
			return true
		}
		if sleepCtx(ctx, 3*time.Second) != nil {
			return false
		}
	}
}

func (c *plaudConnector) clearLogin(l *plaudLogin) {
	c.mu.Lock()
	if c.login == l {
		c.login = nil
	}
	c.auth.Pending = false
	c.mu.Unlock()
}

// Disconnect implements Authenticator via the server's logout tool.
func (c *plaudConnector) Disconnect(ctx context.Context) (AuthState, error) {
	c.mu.Lock()
	if l := c.login; l != nil {
		l.cancel()
		c.login = nil
	}
	c.mu.Unlock()
	client, err := c.dial(ctx)
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	defer func() { _ = client.Close() }()
	text, err := client.Call(ctx, "logout", nil)
	st := AuthState{CheckedAt: c.now().UTC()}
	if err != nil {
		st.Message = firstLine(err.Error())
		c.setAuth(st)
		return st, err
	}
	st.Message = firstLine(text)
	if st.Message == "" {
		st.Message = "signed out"
	}
	c.setAuth(st)
	return st, nil
}

// plaudAccountName extracts an identity from get_current_user output.
func plaudAccountName(text string) string {
	text = strings.TrimSpace(text)
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err == nil {
		for _, key := range []string{"email", "name", "nickname", "username", "id"} {
			if s := plaudString(m, key); s != "" {
				return s
			}
		}
		if u, ok := m["user"].(map[string]any); ok {
			return plaudAccountName(mustJSON(u))
		}
	}
	return firstLine(text)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// renderPlaudNote turns get_note output into markdown. JSON replies are
// flattened: note_list entries and content/summary/markdown fields.
func renderPlaudNote(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		return text
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return text
	}
	var parts []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if s := strings.TrimSpace(t); s != "" {
				parts = append(parts, s)
			}
		case []any:
			for _, it := range t {
				walk(it)
			}
		case map[string]any:
			for _, key := range []string{"note_list", "notes", "markdown", "content", "summary", "text", "note"} {
				if inner, ok := t[key]; ok {
					walk(inner)
					return
				}
			}
			if s := plaudString(t, "title"); s != "" {
				parts = append(parts, "### "+s)
			}
		}
	}
	walk(raw)
	// Parsed JSON with no text means the note is not ready yet.
	return strings.Join(parts, "\n\n")
}

// renderPlaudTranscript turns get_transcript output into "[mm:ss] Speaker:
// text" lines when it is JSON; plain text passes through.
func renderPlaudTranscript(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		return text
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return text
	}
	var segs []any
	switch t := raw.(type) {
	case []any:
		segs = t
	case map[string]any:
		for _, key := range []string{"source_list", "segments", "transcript", "items", "data"} {
			if arr, ok := t[key].([]any); ok {
				segs = arr
				break
			}
			if s, ok := t[key].(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	var b strings.Builder
	for _, s := range segs {
		m, ok := s.(map[string]any)
		if !ok {
			if str, ok := s.(string); ok {
				b.WriteString(str + "\n")
			}
			continue
		}
		content := plaudString(m, "text", "content", "sentence")
		if content == "" {
			continue
		}
		if ts := plaudTimestamp(m); ts != "" {
			fmt.Fprintf(&b, "[%s] ", ts)
		}
		if sp := plaudString(m, "speaker", "speaker_name", "speaker_id"); sp != "" {
			b.WriteString(sp + ": ")
		}
		b.WriteString(content + "\n")
	}
	// Parsed JSON with no segments means the transcript is not ready yet.
	return b.String()
}

// plaudTimestamp formats a segment start (ms or s) as mm:ss or h:mm:ss.
func plaudTimestamp(m map[string]any) string {
	for _, key := range []string{"start_time", "start", "start_ms", "begin"} {
		switch v := m[key].(type) {
		case float64:
			// Plaud reports segment offsets in milliseconds.
			d := (time.Duration(v) * time.Millisecond).Round(time.Second)
			h := int(d.Hours())
			mm := int(d.Minutes()) % 60
			ss := int(d.Seconds()) % 60
			if h > 0 {
				return fmt.Sprintf("%d:%02d:%02d", h, mm, ss)
			}
			return fmt.Sprintf("%02d:%02d", mm, ss)
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// plaudPresignedURL pulls the presigned audio URL out of get_file output.
func plaudPresignedURL(text string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &m); err == nil {
		if u := plaudString(m, "presigned_url", "audio_url", "url"); u != "" {
			return u
		}
	}
	return firstHTTPSURL(text)
}
