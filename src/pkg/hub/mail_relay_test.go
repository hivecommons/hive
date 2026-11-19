package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/mailin/wire"
)

const testMailToken = "mx-token-123"

// mailTestHub is a hub with one hosted hive (subdomain, vanity URL and a
// reported dashboard URL) and an optional second hive.
func mailTestHub(t *testing.T, dashboardURL string) *HubServer {
	t.Helper()
	old := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = old })
	t.Setenv(envHubMailToken, testMailToken)
	for _, h := range []*SaaSHive{
		{ID: "hosted-acme-widgets-ab12", Subdomain: "hosted-acme-widgets-ab12.hive.hivecommons.dev", VanityURL: "https://acme.hive.hivecommons.dev", WebhookRelayKey: "relay-key-acme"},
		{ID: "hosted-beta-x-cd34", Subdomain: "hosted-beta-x-cd34.hive.hivecommons.dev"},
		// Two hives claiming one vanity host: neither gets its mail.
		{ID: "hosted-dup-1", Subdomain: "dup-1.hive.hivecommons.dev", VanityURL: "https://dup.hive.hivecommons.dev"},
		{ID: "hosted-dup-2", Subdomain: "dup-2.hive.hivecommons.dev", VanityURL: "https://dup.hive.hivecommons.dev/"},
		{ID: "hosted-dup-3", Subdomain: "dup-3.hive.hivecommons.dev", VanityURL: "dup.hive.hivecommons.dev"},
	} {
		if err := saveSaaSHive(h); err != nil {
			t.Fatal(err)
		}
	}
	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "hosted-acme-widgets-ab12", DashboardURL: dashboardURL},
		{ID: "hosted-beta-x-cd34"},
		{ID: "byo-someone--selfhosted", DashboardURL: "https://self.example.org"},
	}
	return s
}

func mailRequest(method, path, token string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestMailHosts(t *testing.T) {
	s := mailTestHub(t, "https://dash.acme.example/")
	rec := httptest.NewRecorder()
	s.handleMailHosts(rec, mailRequest(http.MethodGet, wire.HostsPath, testMailToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var hr wire.HostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hr); err != nil {
		t.Fatal(err)
	}
	want := "acme.hive.hivecommons.dev,dash.acme.example,dup-1.hive.hivecommons.dev,dup-2.hive.hivecommons.dev,dup-3.hive.hivecommons.dev,hosted-acme-widgets-ab12.hive.hivecommons.dev,hosted-beta-x-cd34.hive.hivecommons.dev"
	if got := strings.Join(hr.Hosts, ","); got != want {
		t.Fatalf("hosts\n got %s\nwant %s", got, want)
	}
}

func TestMailGate(t *testing.T) {
	s := mailTestHub(t, "https://dash.acme.example")
	for _, tc := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {"wrong", http.StatusUnauthorized}} {
		rec := httptest.NewRecorder()
		s.handleMailHosts(rec, mailRequest(http.MethodGet, wire.HostsPath, tc.token, nil))
		if rec.Code != tc.want {
			t.Errorf("token %q: %d, want %d", tc.token, rec.Code, tc.want)
		}
		rec = httptest.NewRecorder()
		s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, tc.token, []byte(`{}`)))
		if rec.Code != tc.want {
			t.Errorf("inbound token %q: %d, want %d", tc.token, rec.Code, tc.want)
		}
	}
	// No token configured: the endpoints do not exist.
	t.Setenv(envHubMailToken, "")
	rec := httptest.NewRecorder()
	s.handleMailHosts(rec, mailRequest(http.MethodGet, wire.HostsPath, testMailToken, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("mail off: %d", rec.Code)
	}
}

func TestMailRoutesRegistered(t *testing.T) {
	s := mailTestHub(t, "https://dash.acme.example")
	s.mux = http.NewServeMux()
	s.registerMailRoutes()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, mailRequest(http.MethodGet, wire.HostsPath, testMailToken, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET hosts through the mux: %d", rec.Code)
	}
}

type spokeHit struct {
	path   string
	header http.Header
	body   []byte
}

func delivery(id string, kind wire.Kind, rcpt string) []byte {
	b, _ := json.Marshal(wire.Delivery{ID: id, Kind: kind, Recipient: rcpt, Invites: []wire.Invite{{UID: "u1", Passcode: "p"}}})
	return b
}

const testDeliveryID = "0123456789abcdef0123456789abcdef"

func TestMailInboundForwardsSigned(t *testing.T) {
	allowLoopbackRelay(t)
	hits := make(chan spokeHit, 4)
	var status atomic.Int32
	status.Store(http.StatusOK)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- spokeHit{r.URL.Path, r.Header.Clone(), body}
		w.WriteHeader(int(status.Load()))
	}))
	defer spoke.Close()
	s := mailTestHub(t, spoke.URL+"/")

	// A refusal report (#18699) is routed like a delivery.
	for kind, path := range map[wire.Kind]string{wire.KindInvite: wire.SpokeInvitesPath, wire.KindFiles: wire.SpokeFilesPath, wire.KindProof: wire.SpokeProofPath, wire.KindReport: wire.SpokeReportPath} {
		body := delivery(testDeliveryID, kind, "hive@ACME.hive.hivecommons.dev")
		rec := httptest.NewRecorder()
		s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, body))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: status %d %s", kind, rec.Code, rec.Body)
		}
		h := <-hits
		if h.path != path || !bytes.Equal(h.body, body) {
			t.Fatalf("%s: forwarded to %s, body equal %v", kind, h.path, bytes.Equal(h.body, body))
		}
		if h.header.Get(mailRelayDeliveryHeader) != testDeliveryID || h.header.Get("Content-Type") != "application/json" {
			t.Fatalf("%s: headers %v", kind, h.header)
		}
		ts := h.header.Get(webhookRelayTimestampHeader)
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
			t.Fatalf("timestamp %q", ts)
		}
		if want := webhookRelaySignature("relay-key-acme", ts, testDeliveryID, body); h.header.Get(webhookRelaySignatureHeader) != want {
			t.Fatalf("%s: relay signature does not verify with the hive's key", kind)
		}
	}

	// The spoke's answer maps to a retry decision for hive-mx.
	for spokeCode, want := range map[int]int{
		http.StatusUnauthorized:        http.StatusBadGateway,
		http.StatusNotFound:            http.StatusBadGateway,
		http.StatusTooManyRequests:     http.StatusBadGateway,
		http.StatusInternalServerError: http.StatusBadGateway,
		http.StatusBadRequest:          http.StatusUnprocessableEntity,
		http.StatusForbidden:           http.StatusUnprocessableEntity,
	} {
		status.Store(int32(spokeCode))
		rec := httptest.NewRecorder()
		s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindInvite, "hive@acme.hive.hivecommons.dev")))
		<-hits
		if rec.Code != want {
			t.Errorf("spoke %d: hub answered %d, want %d", spokeCode, rec.Code, want)
		}
	}
}

func TestMailInboundRejects(t *testing.T) {
	allowLoopbackRelay(t)
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected forward to %s", r.URL.Path)
	}))
	defer spoke.Close()
	s := mailTestHub(t, spoke.URL)
	cases := map[string]struct {
		body []byte
		want int
	}{
		"not json":         {[]byte("{"), http.StatusBadRequest},
		"bad id":           {delivery("not-hex", wire.KindInvite, "hive@acme.hive.hivecommons.dev"), http.StatusBadRequest},
		"bad kind":         {delivery(testDeliveryID, "spam", "hive@acme.hive.hivecommons.dev"), http.StatusBadRequest},
		"not the mailbox":  {delivery(testDeliveryID, wire.KindInvite, "bob@acme.hive.hivecommons.dev"), http.StatusBadRequest},
		"unknown host":     {delivery(testDeliveryID, wire.KindInvite, "hive@nowhere.example"), http.StatusNotFound},
		"ambiguous host":   {delivery(testDeliveryID, wire.KindInvite, "hive@dup.hive.hivecommons.dev"), http.StatusNotFound},
		"self-hosted":      {delivery(testDeliveryID, wire.KindInvite, "hive@self.example.org"), http.StatusNotFound},
		"no relay key yet": {delivery(testDeliveryID, wire.KindInvite, "hive@hosted-beta-x-cd34.hive.hivecommons.dev"), http.StatusServiceUnavailable},
		"too large":        {append([]byte(`{"id":"`), bytes.Repeat([]byte("a"), mailInboundMaxBody)...), http.StatusRequestEntityTooLarge},
	}
	for name, c := range cases {
		rec := httptest.NewRecorder()
		s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, c.body))
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
	}
}

func TestMailInboundGuardAndUnreachable(t *testing.T) {
	// The production guard refuses a loopback dashboard URL.
	s := mailTestHub(t, "http://127.0.0.1:9")
	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindInvite, "hive@acme.hive.hivecommons.dev")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("private dashboard URL: %d", rec.Code)
	}

	// Allowed but nothing listening: temporary.
	allowLoopbackRelay(t)
	rec = httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindInvite, "hive@acme.hive.hivecommons.dev")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable spoke: %d", rec.Code)
	}

	// A dashboard URL that cannot form a request.
	s.registry.Hives[0].DashboardURL = "http://bad host"
	rec = httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindInvite, "hive@acme.hive.hivecommons.dev")))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("bad dashboard URL: %d", rec.Code)
	}
}

func TestMailRelayNeverFollowsRedirects(t *testing.T) {
	allowLoopbackRelay(t)
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the relay followed a redirect")
	}))
	defer internal.Close()
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusTemporaryRedirect)
	}))
	defer spoke.Close()
	s := mailTestHub(t, spoke.URL)
	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, delivery(testDeliveryID, wire.KindInvite, "hive@acme.hive.hivecommons.dev")))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("redirect answer: %d", rec.Code)
	}
}

func TestMailSpokePathsArePublic(t *testing.T) {
	for _, p := range []string{wire.SpokeInvitesPath, wire.SpokeFilesPath, wire.SpokeFilesChunkPath, wire.SpokeProofPath, wire.SpokeReportPath} {
		if !isSaaSPublicPath(p) {
			t.Errorf("%s must pass the hosted auth-check: the spoke verifies the relay signature itself", p)
		}
	}
	if isSaaSPublicPath("/api/meetings/calendar-emails") {
		t.Error("the user-facing calendar-emails API must stay behind the login gate")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "",
		"Acme.Example.":              "acme.example",
		"https://acme.example:8443/": "acme.example",
		"http://[::1]:80":            "::1",
		"https://bad host":           "",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// routedRequest is a hive-mx delivery with the routing headers.
func routedRequest(kind wire.Kind, rcpt, contentType string, body []byte) *http.Request {
	r := mailRequest(http.MethodPost, wire.InboundPath, testMailToken, body)
	r.Header.Set(wire.HeaderDelivery, testDeliveryID)
	r.Header.Set(wire.HeaderKind, string(kind))
	r.Header.Set(wire.HeaderRecipient, rcpt)
	r.Header.Set("Content-Type", contentType)
	return r
}

func recordingSpoke(t *testing.T, status func(path string, n int) int) (*httptest.Server, chan spokeHit) {
	t.Helper()
	hits := make(chan spokeHit, 64)
	var n atomic.Int32
	spoke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- spokeHit{r.URL.Path, r.Header.Clone(), body}
		w.WriteHeader(status(r.URL.Path, int(n.Add(1))))
	}))
	t.Cleanup(spoke.Close)
	return spoke, hits
}

func TestMailInboundChunked(t *testing.T) {
	allowLoopbackRelay(t)
	t.Setenv(envHubMailChunkBytes, "1") // clamped up to mailMinChunkBytes
	spoke, hits := recordingSpoke(t, func(string, int) int { return http.StatusOK })
	s := mailTestHub(t, spoke.URL)

	body := bytes.Repeat([]byte("0123456789"), (mailMinChunkBytes*5/2)/10)
	ct := "multipart/form-data; boundary=xyz"
	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, routedRequest(wire.KindFiles, "hive@acme.hive.hivecommons.dev", ct, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var joined []byte
	for i := 0; i < 3; i++ {
		h := <-hits
		want := wire.ChunkDelivery(testDeliveryID, i, 3)
		if h.path != wire.SpokeFilesChunkPath || h.header.Get(mailRelayDeliveryHeader) != want || h.header.Get(wire.HeaderBodyType) != ct || len(h.body) > mailMinChunkBytes {
			t.Fatalf("piece %d: %s %v %d bytes", i, h.path, h.header, len(h.body))
		}
		if sig := webhookRelaySignature("relay-key-acme", h.header.Get(webhookRelayTimestampHeader), want, h.body); h.header.Get(webhookRelaySignatureHeader) != sig {
			t.Fatalf("piece %d: signature does not bind its piece id", i)
		}
		joined = append(joined, h.body...)
	}
	if !bytes.Equal(joined, body) || len(hits) != 0 {
		t.Fatalf("pieces do not join to the body (%d extra hits)", len(hits))
	}

	// An older hive-mx: JSON without routing headers, larger than a piece.
	big, _ := json.Marshal(wire.Delivery{ID: testDeliveryID, Kind: wire.KindFiles, Recipient: "hive@acme.hive.hivecommons.dev",
		Files: &wire.Files{Attachments: []wire.Attachment{{Name: "a.png", Data: bytes.Repeat([]byte("x"), mailMinChunkBytes)}}}})
	rec = httptest.NewRecorder()
	s.handleMailInbound(rec, mailRequest(http.MethodPost, wire.InboundPath, testMailToken, big))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("legacy json: %d %s", rec.Code, rec.Body)
	}
	joined = nil
	for len(joined) < len(big) {
		h := <-hits
		if h.path != wire.SpokeFilesChunkPath || h.header.Get(wire.HeaderBodyType) != wire.ContentTypeJSON {
			t.Fatalf("legacy piece %s %v", h.path, h.header)
		}
		joined = append(joined, h.body...)
	}
	if !bytes.Equal(joined, big) {
		t.Fatal("legacy pieces do not join")
	}

	// A small routed JSON delivery is forwarded in one request.
	small := delivery(testDeliveryID, wire.KindFiles, "hive@acme.hive.hivecommons.dev")
	rec = httptest.NewRecorder()
	s.handleMailInbound(rec, routedRequest(wire.KindFiles, "hive@acme.hive.hivecommons.dev", "application/json", small))
	if h := <-hits; rec.Code != http.StatusAccepted || h.path != wire.SpokeFilesPath || !bytes.Equal(h.body, small) {
		t.Fatalf("small routed: %d %s", rec.Code, h.path)
	}
}

func TestMailInboundChunkRefused(t *testing.T) {
	allowLoopbackRelay(t)
	t.Setenv(envHubMailChunkBytes, strconv.Itoa(mailMinChunkBytes))
	body := bytes.Repeat([]byte("z"), mailMinChunkBytes*3)
	for refuse, want := range map[int]int{
		http.StatusRequestEntityTooLarge: http.StatusRequestEntityTooLarge,
		http.StatusInsufficientStorage:   http.StatusBadGateway,
		http.StatusBadRequest:            http.StatusUnprocessableEntity,
		http.StatusNotFound:              http.StatusBadGateway, // a spoke without the chunk route: retry later
	} {
		spoke, hits := recordingSpoke(t, func(_ string, n int) int {
			if n == 2 {
				return refuse
			}
			return http.StatusOK
		})
		s := mailTestHub(t, spoke.URL)
		rec := httptest.NewRecorder()
		s.handleMailInbound(rec, routedRequest(wire.KindFiles, "hive@acme.hive.hivecommons.dev", "multipart/form-data; boundary=b", body))
		if rec.Code != want || len(hits) != 2 {
			t.Errorf("spoke %d on piece 2: hub %d after %d pieces, want %d after 2", refuse, rec.Code, len(hits), want)
		}
	}

	// The spoke goes away mid-relay.
	s := mailTestHub(t, "http://127.0.0.1:9")
	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, routedRequest(wire.KindFiles, "hive@acme.hive.hivecommons.dev", "multipart/form-data; boundary=b", body))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unreachable: %d", rec.Code)
	}
}

func TestMailInboundRoutedRejects(t *testing.T) {
	allowLoopbackRelay(t)
	spoke, _ := recordingSpoke(t, func(string, int) int {
		t.Error("unexpected forward")
		return http.StatusOK
	})
	s := mailTestHub(t, spoke.URL)
	rcpt := "hive@acme.hive.hivecommons.dev"
	cases := map[string]struct {
		req  *http.Request
		want int
	}{
		"multipart without headers": {func() *http.Request {
			r := mailRequest(http.MethodPost, wire.InboundPath, testMailToken, []byte("x"))
			r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
			return r
		}(), http.StatusBadRequest},
		"multipart invite":   {routedRequest(wire.KindInvite, rcpt, "multipart/form-data; boundary=b", []byte("x")), http.StatusBadRequest},
		"multipart no bound": {routedRequest(wire.KindFiles, rcpt, "multipart/form-data", []byte("x")), http.StatusBadRequest},
		"other type":         {routedRequest(wire.KindFiles, rcpt, "text/plain", []byte("x")), http.StatusBadRequest},
		"bad routed id": {func() *http.Request {
			r := routedRequest(wire.KindFiles, rcpt, "application/json", []byte("{}"))
			r.Header.Set(wire.HeaderDelivery, "x")
			return r
		}(), http.StatusBadRequest},
		"routed unknown host":  {routedRequest(wire.KindFiles, "hive@nowhere.example", "application/json", []byte("{}")), http.StatusNotFound},
		"routed missing kind":  {routedRequest("", rcpt, "application/json", []byte("{}")), http.StatusBadRequest},
		"routed body too long": {routedRequest(wire.KindFiles, rcpt, "application/json", bytes.Repeat([]byte("a"), 2048)), http.StatusRequestEntityTooLarge},
	}
	t.Setenv(envHubMailMaxBody, "1024")
	for name, c := range cases {
		rec := httptest.NewRecorder()
		s.handleMailInbound(rec, c.req)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
	}

	// The hub cannot spool: temporary.
	t.Setenv(envHubMailSpoolDir, t.TempDir()+"/missing")
	rec := httptest.NewRecorder()
	s.handleMailInbound(rec, routedRequest(wire.KindFiles, rcpt, "application/json", []byte("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no spool: %d", rec.Code)
	}
}

func TestMailRelaySizing(t *testing.T) {
	for env, want := range map[string]int64{
		"":         wire.RelayChunkBytes,
		"junk":     wire.RelayChunkBytes,
		"1":        mailMinChunkBytes,
		"99999999": wire.MaxRelayChunkBytes,
		"200000":   200000,
	} {
		t.Setenv(envHubMailChunkBytes, env)
		if got := mailChunkBytes(); got != want {
			t.Errorf("%q: %d, want %d", env, got, want)
		}
	}
	if mailEnvInt64(envHubMailMaxBody, mailInboundDefaultMaxBody) != mailInboundDefaultMaxBody || mailInboundDefaultMaxBody < 36700160 {
		t.Fatal("the routed body cap must hold a full 35 MiB message")
	}
	if !isSaaSPublicPath(wire.SpokeFilesChunkPath) {
		t.Fatal("the chunk route must pass the hosted auth-check: the spoke verifies each piece's relay signature")
	}
}
