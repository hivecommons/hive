package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// This file is the TUI's client for the dashboard's /terminal reverse proxy —
// the same authenticated route the web dashboard's "▶ terminal" links use
// (pkg/dashboard/terminal_proxy.go on the Go server, the ttydProxy route in
// src/proxy/server.js on the Node proxy). Behind that proxy sits the
// container's ttyd (pinned in src/Dockerfile), which attaches to the selected
// agent's tmux session via deploy/ttyd-tmux.sh.
//
// DELIBERATELY NOT A NEW SERVER SURFACE. ttyd's port (:7681) is loopback-only
// and never published — src/deploy/quadlet/hive.container says so in as many
// words — so the ONLY path to an agent terminal on a containerized or remote
// hive is the /terminal route on the published dashboard port, behind the same
// authentication as every other dashboard request. This client therefore
// reuses that route and this package's existing credentials rather than
// growing a parallel one (kubestellar/hive#5644).

const (
	// TerminalWSPath is where ttyd's websocket lands after the proxy strips
	// the /terminal prefix: ttyd serves its socket at /ws, so the public path
	// is /terminal/ws. Pinned by TestTerminalProxyPathRewrites on the server
	// side, which is what lets this constant be trusted from here.
	TerminalWSPath = "/terminal/ws"

	// TtydCredentialEnv mirrors the entrypoint's variable of the same name.
	// The container starts ttyd with basic-auth credential
	// HIVE_TTYD_CREDENTIAL, defaulting to "hive:<HIVE_DASHBOARD_TOKEN>"
	// (deploy/entrypoint.sh). ttyd checks that credential ITSELF, on the
	// upgrade request and again in the first protocol message, independently
	// of the dashboard's own gate in front of it — so this client must be
	// able to present it, and must derive the same default from the same
	// token so that a hive deployed with the defaults needs nothing new set.
	TtydCredentialEnv = "HIVE_TTYD_CREDENTIAL"

	// ttydDefaultUser is the username half of the entrypoint's derived
	// credential. Must match deploy/entrypoint.sh's "hive:${HIVE_DASHBOARD_TOKEN}".
	ttydDefaultUser = "hive"

	// ttydSubprotocol is the websocket subprotocol ttyd registers its callback
	// under (src/server.c in tsl0922/ttyd). A dial without it is refused
	// before any message is exchanged.
	ttydSubprotocol = "tty"
)

// ttyd's wire protocol (tsl0922/ttyd src/server.h, pinned at 1.7.7 by
// src/Dockerfile). Every message is prefixed with a one-byte command; the
// FIRST client message is instead a bare JSON object ('{' is itself the
// command byte) carrying the auth token and the initial window size.
const (
	// client → server
	ttydInput          = '0'
	ttydResizeTerminal = '1'
	// server → client
	ttydOutput         = '0'
	ttydSetWindowTitle = '1'
	ttydSetPreferences = '2'
)

// TerminalSession is one attached agent terminal: a websocket speaking ttyd's
// framing, reached through the dashboard's authenticated /terminal proxy.
//
// Writes are serialized by a mutex because gorilla/websocket permits at most
// one concurrent writer, and this session is written from two goroutines by
// design: the input pump (keystrokes) and the resize watcher (SIGWINCH).
type TerminalSession struct {
	conn    *websocket.Conn
	writeMu sync.Mutex

	// authToken is what ttyd's first-message check expects: the base64 of its
	// basic-auth credential, or empty when the hive runs ttyd uncredentialed.
	authToken string
}

// ttydCredential resolves the credential ttyd itself was started with:
// the operator's explicit HIVE_TTYD_CREDENTIAL when set, otherwise the
// entrypoint's derived default "hive:<token>", otherwise nothing (a hive
// with no dashboard token starts ttyd with no credential at all — see
// deploy/entrypoint.sh, and test_ttyd_url_arg.sh which pins the precedence).
func (c *Client) ttydCredential() string {
	if c.ttydCred != "" {
		return c.ttydCred
	}
	if c.token != "" {
		return ttydDefaultUser + ":" + c.token
	}
	return ""
}

// DialTerminal opens the websocket for one agent's tmux session (named
// "hive-<agent>"; session is the full name) through the dashboard's /terminal
// proxy, carrying every credential lane the route can require:
//
//   - a short-lived ?code= minted by POST /api/terminal/handoff using the
//     normal dashboard Authorization header/cookie. The shared dashboard token
//     never travels in the websocket URL.
//   - Cookie for the per-user session lanes: a spoke's hive_session, a hub's
//     hive_hub_user, and the per-hive terminal assertion that rides with it.
//     Same value, same reasoning as authorize().
//   - Authorization: Basic for ttyd ITSELF, which checks its own basic-auth
//     credential on the upgrade request (check_auth in ttyd's protocol.c)
//     before the dashboard's proxy hands the connection over. This is the
//     browser flow's basic-auth prompt, automated.
//
// ?arg= selects the tmux session, exactly as the dashboard's terminalUrl()
// builds it — ttyd forwards it to deploy/ttyd-tmux.sh because the entrypoint
// starts it with --url-arg.
//
// A refused upgrade with an HTTP status comes back as an *APIError carrying
// it, unwrapped, so the caller can classify with IsUnauthorized/IsForbidden —
// the same contract CheckCredentials keeps.
func (c *Client) DialTerminal(ctx context.Context, session string) (*TerminalSession, error) {
	code := ""
	if c.token != "" || c.cookie != "" {
		var err error
		code, err = c.createTerminalHandoff(ctx)
		if err != nil && c.cookie == "" {
			return nil, err
		}
	}

	wsURL, err := c.terminalWSURL(session, code)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	if c.cookie != "" {
		// Set, not Add — the value is already a complete Cookie header. Same
		// rule as authorize(), for the same reason.
		header.Set("Cookie", c.cookie)
	}
	cred := c.ttydCredential()
	authToken := ""
	if cred != "" {
		// ttyd stores its -c credential base64-encoded and compares BOTH the
		// upgrade request's Basic header and the first message's AuthToken
		// against that exact string, so one encoding serves both checks.
		authToken = base64.StdEncoding.EncodeToString([]byte(cred))
		header.Set("Authorization", "Basic "+authToken)
	}

	dialer := &websocket.Dialer{
		// Same isolation rationale as newTransport(): never share state with
		// other clients' teardown. The dialer has no transport to clone, but
		// gets its own proxy/handshake configuration rather than the shared
		// DefaultDialer.
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: requestTimeout,
		Subprotocols:     []string{ttydSubprotocol},
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			// The proxy answered with an HTTP status instead of upgrading —
			// this is where the dashboard's 401/403 lands. Bounded read, as
			// everywhere else in this package.
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
			_ = resp.Body.Close()
			return nil, &APIError{
				StatusCode: resp.StatusCode,
				Method:     http.MethodGet,
				Path:       TerminalWSPath,
				Body:       strings.TrimSpace(string(errBody)),
			}
		}
		return nil, fmt.Errorf("dial %s: %w", TerminalWSPath, err)
	}

	return &TerminalSession{conn: conn, authToken: authToken}, nil
}

type terminalHandoffResponse struct {
	Code string `json:"code"`
}

func (c *Client) createTerminalHandoff(ctx context.Context) (string, error) {
	var resp terminalHandoffResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/terminal/handoff", nil, &resp); err != nil {
		return "", err
	}
	if resp.Code == "" {
		return "", fmt.Errorf("terminal handoff response missing code")
	}
	return resp.Code, nil
}

// terminalWSURL builds the websocket URL for one session: scheme swapped to
// ws/wss, ?arg= for the session, and a short-lived ?code= when one was minted.
func (c *Client) terminalWSURL(session, code string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse %s %q: %w", BaseURLEnv, c.baseURL, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + TerminalWSPath
	q := u.Query()
	q.Set("arg", session)
	if code != "" {
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ttydInit is the first client message: ttyd spawns the terminal process only
// after receiving it, checking AuthToken when it was started with a
// credential. Field names are ttyd's, not this package's, so they are pinned
// here exactly (parse_window_size and the AuthToken lookup in protocol.c).
type ttydInit struct {
	AuthToken string `json:"AuthToken"`
	Columns   int    `json:"columns"`
	Rows      int    `json:"rows"`
}

// Start sends the init message with the terminal's size, which is what makes
// ttyd actually run the attach script. Nothing arrives before it: ttyd holds
// the session unspawned until the client has authenticated and sized it.
func (t *TerminalSession) Start(cols, rows int) error {
	payload, err := json.Marshal(ttydInit{AuthToken: t.authToken, Columns: cols, Rows: rows})
	if err != nil {
		return fmt.Errorf("encode terminal init: %w", err)
	}
	return t.write(websocket.TextMessage, payload)
}

// SendInput forwards raw keystrokes. The payload is bytes off the wire from
// the operator's terminal, prefixed with ttyd's INPUT command — no line
// discipline, no interpretation; the remote tmux owns all of that.
func (t *TerminalSession) SendInput(p []byte) error {
	msg := make([]byte, 0, len(p)+1)
	msg = append(msg, ttydInput)
	msg = append(msg, p...)
	return t.write(websocket.BinaryMessage, msg)
}

// Resize tells ttyd the operator's terminal changed size, so the remote tmux
// client re-fits — without it, a resized local window shows a stale, clipped
// frame of the old geometry.
func (t *TerminalSession) Resize(cols, rows int) error {
	size, err := json.Marshal(struct {
		Columns int `json:"columns"`
		Rows    int `json:"rows"`
	}{cols, rows})
	if err != nil {
		return fmt.Errorf("encode terminal resize: %w", err)
	}
	msg := make([]byte, 0, len(size)+1)
	msg = append(msg, ttydResizeTerminal)
	msg = append(msg, size...)
	return t.write(websocket.BinaryMessage, msg)
}

// ReadOutput blocks for the next chunk of terminal output.
//
// ttyd interleaves three server→client message kinds; only OUTPUT carries
// bytes for the screen. SET_WINDOW_TITLE and SET_PREFERENCES are meaningful
// to a browser tab and meaningless to a terminal that already has a title
// bar and no xterm.js to configure, so they are consumed and dropped here
// rather than pushed to a caller that could only ignore them.
//
// The error is the caller's detach signal: a normal close (tmux detached,
// the session ended) surfaces as a *websocket.CloseError, anything else as
// itself. Distinguishing them is the caller's job — this method's is only to
// never return a frame that is not screen bytes.
func (t *TerminalSession) ReadOutput() ([]byte, error) {
	for {
		_, msg, err := t.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		if len(msg) == 0 {
			continue
		}
		switch msg[0] {
		case ttydOutput:
			return msg[1:], nil
		case ttydSetWindowTitle, ttydSetPreferences:
			continue
		default:
			// Unknown command: skip rather than fail. A ttyd a minor version
			// ahead must not kill an attached session over a message this
			// client does not need.
			continue
		}
	}
}

// Close tears the websocket down. Best-effort close frame first so ttyd sees
// a clean detach (and SIGHUPs the attach process promptly) rather than an
// abrupt TCP reset; the hard Close runs regardless.
func (t *TerminalSession) Close() error {
	t.writeMu.Lock()
	_ = t.conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	t.writeMu.Unlock()
	return t.conn.Close()
}

// write is the single serialized write path — see TerminalSession.
func (t *TerminalSession) write(messageType int, payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.conn.WriteMessage(messageType, payload)
}
