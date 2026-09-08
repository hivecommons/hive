package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ghOverlayPollJS returns the dashboard page's pollGHAuth() body — the in-page
// sign-in overlay's poll loop, which is a different implementation from the
// standalone loginPage in server.go.
//
// The two are reached by disjoint sets of users: authenticate() serves
// loginPage only for an UNTRUSTED non-/api request, so an open hive (or one
// carrying HIVE_DASHBOARD_TOKEN) gets index.html and the overlay is the only
// sign-in path available. That is why #6216 — the overlay never sending the
// flow_id that #5748 made mandatory — survived a release with the login page's
// own tests green the whole time.
func ghOverlayPollJS(t *testing.T) string {
	t.Helper()
	html := indexHTML(t)
	start := strings.Index(html, "async function pollGHAuth()")
	if start < 0 {
		t.Fatal("pollGHAuth() not found in static/index.html — the dashboard's GitHub sign-in overlay lost its poll loop")
	}
	end := strings.Index(html[start:], "function cancelGHLogin()")
	if end < 0 {
		t.Fatal("cancelGHLogin() not found after pollGHAuth() — cannot bound the poll function")
	}
	return html[start : start+end]
}

// The regression guard for #6216.
//
// #5748 made flow_id mandatory on /api/gh-user-auth/poll: the server compares
// it against the secret minted by /start and answers 400 to any poll that
// cannot present it, BEFORE contacting GitHub or setting a cookie. That change
// taught the standalone login page and `hivectl login` to send it and missed
// this page, so every poll from the dashboard overlay was rejected and the
// login could never complete.
//
// The server-side tests (TestCovDF_GHUserAuthPoll_*) passed throughout, because
// they assert the rejection — the browser was the thing being rejected. Only an
// assertion on the client can catch this direction, which is why it lives here.
func TestGHLoginOverlayPollSendsFlowID(t *testing.T) {
	poll := ghOverlayPollJS(t)

	if !strings.Contains(poll, "/api/gh-user-auth/poll") {
		t.Fatal("pollGHAuth() no longer posts to /api/gh-user-auth/poll")
	}
	if !strings.Contains(poll, "flow_id") {
		t.Fatalf("pollGHAuth() does not send flow_id — every poll will be rejected 400 "+
			"(\"no device flow in progress\") and the overlay will spin on "+
			"\"Waiting for authorization...\" forever (#6216). Body was:\n%s", poll)
	}
	// A literal is not a binding: the value has to come from the flow the page
	// actually started, which startGHLogin captures out of the /start response.
	if !strings.Contains(poll, "ghFlowID") {
		t.Fatalf("pollGHAuth() mentions flow_id but not ghFlowID — it must send the id "+
			"minted by /api/gh-user-auth/start, not a constant. Body was:\n%s", poll)
	}
}

// startGHLogin must KEEP the flow_id. Reading it out of the /start response is
// the half of the binding that lives outside the poll loop, and the original
// bug was precisely that the response was parsed for user_code /
// verification_uri / interval and flow_id was dropped on the floor.
func TestGHLoginOverlayCapturesFlowIDFromStart(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "async function startGHLogin()")
	if start < 0 {
		t.Fatal("startGHLogin() not found in static/index.html")
	}
	end := strings.Index(html[start:], "function failGHAuth(")
	if end < 0 {
		end = strings.Index(html[start:], "async function pollGHAuth()")
	}
	if end < 0 {
		t.Fatal("cannot bound startGHLogin()")
	}
	body := html[start : start+end]
	if !strings.Contains(body, "data.flow_id") {
		t.Fatalf("startGHLogin() discards data.flow_id from the /api/gh-user-auth/start "+
			"response; pollGHAuth() then has nothing to present (#6216). Body was:\n%s", body)
	}
	if !strings.Contains(body, "ghFlowID = data.flow_id") {
		t.Fatalf("startGHLogin() reads data.flow_id but does not store it in ghFlowID, "+
			"which is what pollGHAuth() sends. Body was:\n%s", body)
	}
}

// The reason #6216 was an eternal spinner rather than an error message: the
// 400 body is {"ok":false,"error":"..."} with no `status` field, so it matched
// none of pollGHAuth's status branches, the function returned without touching
// the timer, and setInterval fired again. The standalone login page already
// guards this ("Any other shape is terminal"); the overlay did not.
//
// This asserts the overlay now stops on a response it does not understand, so a
// future protocol change surfaces as a message instead of a silent hang.
func TestGHLoginOverlayStopsOnUnrecognizedResponse(t *testing.T) {
	poll := ghOverlayPollJS(t)

	if !strings.Contains(poll, "resp.ok") {
		t.Fatalf("pollGHAuth() never inspects resp.ok, so an HTTP error body with no "+
			"`status` field falls through every branch and the loop keeps firing — "+
			"the eternal spinner of #6216. Body was:\n%s", poll)
	}
	// Stopping means clearing the timer. failGHAuth is the shared helper that
	// does it; requiring it here keeps the error and unrecognized paths from
	// drifting apart.
	if !strings.Contains(poll, "failGHAuth(") {
		t.Fatalf("pollGHAuth() does not route terminal responses through failGHAuth(), "+
			"which is what clears the interval; without it the overlay keeps polling "+
			"after a fatal answer. Body was:\n%s", poll)
	}
}

// failGHAuth is what makes a terminal response terminal. If it stops painting
// the message or stops clearing the timer, #6216's symptom comes straight back
// in a new disguise.
func TestGHLoginOverlayFailHelperClearsTimerAndShowsMessage(t *testing.T) {
	html := indexHTML(t)
	start := strings.Index(html, "function failGHAuth(")
	if start < 0 {
		t.Fatal("failGHAuth() not found in static/index.html")
	}
	end := strings.Index(html[start:], "async function pollGHAuth()")
	if end < 0 {
		t.Fatal("cannot bound failGHAuth()")
	}
	body := html[start : start+end]
	for _, want := range []string{
		"clearInterval(ghPollTimer)", // stop polling
		"ghPollTimer = null",         // and let a later start re-arm cleanly
		"gh-poll-status",             // say so in the overlay
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failGHAuth() is missing %q; a terminal response would not actually "+
				"terminate the poll or would not be visible. Body was:\n%s", want, body)
		}
	}
}

// The two sides meeting: start a flow the way the overlay does, then poll with
// the LITERAL body JSON.stringify({flow_id: ghFlowID}) produces, as an
// anonymous browser (no role headers — both routes are public).
//
// Every other poll test marshals a Go map, so none of them pins the wire shape
// the page actually emits. #6216 was exactly a wire-shape mismatch — an empty
// body where the server required a field — and it survived because the client
// and server were only ever tested apart.
func TestGHUserAuthPollAcceptsBrowserBodyShape(t *testing.T) {
	s, _, _ := dfServer(t, "authorization_pending", "octocat")

	startRec := httptest.NewRecorder()
	s.mux.ServeHTTP(startRec, httptest.NewRequest(http.MethodPost, "/api/gh-user-auth/start", nil))
	if startRec.Code != http.StatusOK {
		t.Fatalf("start: want 200, got %d body=%s", startRec.Code, startRec.Body.String())
	}
	var started map[string]any
	if err := json.Unmarshal(startRec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decoding start response: %v", err)
	}
	flowID, _ := started["flow_id"].(string)
	if flowID == "" {
		t.Fatalf("start response carried no flow_id: %s", startRec.Body.String())
	}

	// Byte-for-byte what the overlay now sends.
	raw := `{"flow_id":"` + flowID + `"}`
	pollRec := httptest.NewRecorder()
	pollReq := httptest.NewRequest(http.MethodPost, "/api/gh-user-auth/poll", strings.NewReader(raw))
	pollReq.Header.Set("Content-Type", "application/json")
	s.mux.ServeHTTP(pollRec, pollReq)

	if pollRec.Code != http.StatusOK {
		t.Fatalf("the browser's poll body was rejected with %d (%s) — the overlay would "+
			"spin forever again (#6216); body sent: %s",
			pollRec.Code, strings.TrimSpace(pollRec.Body.String()), raw)
	}
	var polled map[string]any
	if err := json.Unmarshal(pollRec.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decoding poll response: %v", err)
	}
	// The mock is still authorization_pending, so this is the steady state the
	// overlay must keep polling through — and it is a shape the page now names
	// explicitly rather than falling into its terminal branch.
	if polled["status"] != "pending" {
		t.Fatalf("want status=pending, got %v (%s)", polled["status"], pollRec.Body.String())
	}
}
