package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

// authConn is a fake connector implementing connector.Authenticator.
type authConn struct {
	blockingConn
	state connector.AuthState
}

func (c *authConn) AuthStatus(context.Context, bool) connector.AuthState { return c.state }
func (c *authConn) Connect(context.Context) (connector.AuthState, error) {
	c.state = connector.AuthState{Pending: true, AuthURL: "https://auth.example/authorize"}
	return c.state, nil
}
func (c *authConn) Disconnect(context.Context) (connector.AuthState, error) {
	c.state = connector.AuthState{Message: "signed out"}
	return c.state, nil
}

func decodeAuthState(t *testing.T, body []byte) connector.AuthState {
	t.Helper()
	var st connector.AuthState
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return st
}

func TestKnowledgeConnectorAuthEndpoints(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doGet(s, "/api/knowledge/connectors/wiki/auth"); rec.Code != http.StatusNotFound {
		t.Fatalf("auth without syncer = %d, want 404", rec.Code)
	}

	conn := &authConn{state: connector.AuthState{Connected: true, Account: "andy@example.com"}}
	deps.KnowledgeConnectors = newTestConnectorSyncer(t, conn)

	rec := doGet(s, "/api/knowledge/connectors/wiki/auth")
	if rec.Code != http.StatusOK {
		t.Fatalf("auth status = %d: %s", rec.Code, rec.Body.String())
	}
	if st := decodeAuthState(t, rec.Body.Bytes()); !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("state = %+v", st)
	}
	if rec := doGet(s, "/api/knowledge/connectors/nope/auth"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown connector = %d, want 404", rec.Code)
	}
	if rec := doPostNoOwner(s, "/api/knowledge/connectors/wiki/auth/connect", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner connect = %d, want 403", rec.Code)
	}
	rec = doPost(s, "/api/knowledge/connectors/wiki/auth/connect", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("connect = %d: %s", rec.Code, rec.Body.String())
	}
	if st := decodeAuthState(t, rec.Body.Bytes()); !st.Pending || st.AuthURL != "https://auth.example/authorize" {
		t.Fatalf("connect state = %+v", st)
	}
	rec = doPost(s, "/api/knowledge/connectors/wiki/auth/disconnect", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect = %d: %s", rec.Code, rec.Body.String())
	}
	if st := decodeAuthState(t, rec.Body.Bytes()); st.Connected || st.Message != "signed out" {
		t.Fatalf("disconnect state = %+v", st)
	}
}

func TestKnowledgeConnectorAuthNotInteractive(t *testing.T) {
	s, deps := apiServer(t)
	deps.KnowledgeConnectors = newTestConnectorSyncer(t, &blockingConn{})
	if rec := doGet(s, "/api/knowledge/connectors/wiki/auth"); rec.Code != http.StatusBadRequest {
		t.Fatalf("non-authenticator connector = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
