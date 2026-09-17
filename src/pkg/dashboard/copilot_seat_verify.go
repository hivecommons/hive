package dashboard

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Post-login Copilot seat verification (#7309).
//
// Saving a device-flow token proves only that GitHub minted one. It does NOT
// prove the account can run inference, and the three root causes seen on #7302
// all produced a saved token plus a silently broken backend:
//
//  1. the device-flow activation failed server-side, so the token is not
//     actually good for Copilot (401),
//  2. org policy blocks the CLI's integration ID even though the seat is
//     valid (403 that is not a licence verdict),
//  3. the catalog probe rode a DIFFERENT credential than the one just logged
//     in with, so the seat being checked was never the seat in question.
//
// All three previously surfaced identically, as a per-model "(Copilot seat not
// licensed)" suffix with no indication of which credential was probed or
// whether the login had activated. Verifying once at login and reporting the
// verdict — with the credential named — turns them into three distinct states
// an adopter can self-diagnose from the screen.

// Copilot seat verification states. These are API values: the dashboard
// switches on them, so they are part of /api/copilot-auth/status's contract.
const (
	// copilotSeatUnknown: nothing has been verified (no token, or no check
	// has run yet). Never presented as a failure.
	copilotSeatUnknown = "unknown"
	// copilotSeatActive: GitHub answered 200 for this credential. The seat
	// is live and agents on this backend can run inference.
	copilotSeatActive = "active"
	// copilotSeatNoSeat: GitHub's explicit licence verdict. The account has
	// no Copilot seat; only the account owner can fix it.
	copilotSeatNoSeat = "no_seat"
	// copilotSeatBlocked: a 403 that is NOT a licence verdict. The seat may
	// be perfectly valid while an org policy refuses this integration ID —
	// cause 2 on #7302, and the state that most needs its own wording
	// because telling the owner to "check your seat" is actively wrong here.
	copilotSeatBlocked = "blocked"
	// copilotSeatRejected: a 401. The token exists but GitHub will not
	// accept it — the signature of an activation that did not complete
	// server-side (cause 1 on #7302).
	copilotSeatRejected = "rejected"
	// copilotSeatUnreachable: network failure or a 5xx. Hive-side or
	// transient, and deliberately NOT reported as an account problem.
	copilotSeatUnreachable = "unreachable"
)

// copilotSeatVerdict is the operator-facing result of one verification.
type copilotSeatVerdict struct {
	State string `json:"state"`
	// Detail is the full operator-facing sentence, including what to do
	// next. Empty for copilotSeatUnknown.
	Detail string `json:"detail,omitempty"`
	// Credential names which credential was verified, so a verdict can
	// never be misread as being about an account the operator did not use.
	Credential string `json:"credential,omitempty"`
	// CheckedAt is when the probe ran; zero when never checked.
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// copilotSeatCache holds the most recent verdict. One entry: a hive holds one
// Copilot credential at a time, and a token change is exactly when the seat
// has to be re-verified.
type copilotSeatCache struct {
	mu      sync.Mutex
	token   string
	verdict copilotSeatVerdict
}

func (c *copilotSeatCache) get(token string) (copilotSeatVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token == "" || c.token != token {
		return copilotSeatVerdict{}, false
	}
	return c.verdict, true
}

func (c *copilotSeatCache) put(token string, v copilotSeatVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.verdict = token, v
}

// copilotSeatHTTPTimeout bounds the verification probe. Shorter than the
// device-flow timeout: this runs while an operator watches the login dialog.
const copilotSeatHTTPTimeout = 15 * time.Second

// verifyCopilotSeat asks GitHub whether this exact credential can run Copilot
// inference, and renders the answer as an operator-facing verdict.
//
// It calls copilot_internal/user — the same endpoint copilotAPIHost already
// uses — because that is the authority on entitlement, and it answers before
// any model catalog is fetched. credentialName is the already-rendered
// description of what is being verified (see describeCopilotCredential); it is
// echoed into every verdict so the answer is never ambiguous about its subject.
func verifyCopilotSeat(token, credentialName string) copilotSeatVerdict {
	now := time.Now().UTC()
	if strings.TrimSpace(token) == "" {
		return copilotSeatVerdict{State: copilotSeatUnknown}
	}
	verdict := func(state, detail string) copilotSeatVerdict {
		return copilotSeatVerdict{State: state, Detail: detail, Credential: credentialName, CheckedAt: now}
	}
	subject := "the credential hive is using"
	if strings.TrimSpace(credentialName) != "" {
		subject = credentialName
	}

	req, err := http.NewRequest(http.MethodGet, copilotUserEndpointURL, nil)
	if err != nil {
		return verdict(copilotSeatUnreachable, "Could not build the verification request: "+err.Error())
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Editor-Version", copilotEditorVersion)

	resp, err := (&http.Client{Timeout: copilotSeatHTTPTimeout}).Do(req)
	if err != nil {
		return verdict(copilotSeatUnreachable,
			"Could not reach GitHub to verify "+subject+", so the seat is still unconfirmed. "+
				"This is a network problem on the hive, not a problem with the account; retry verification.")
	}
	defer closeHTTPBody(resp.Body)
	// Bounded: this body is only ever read to classify a licence verdict.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	lower := strings.ToLower(string(body))

	switch {
	case resp.StatusCode == http.StatusOK:
		return verdict(copilotSeatActive,
			"GitHub accepted "+subject+" and reports an active Copilot seat. "+
				"Agents on this backend can run inference.")

	case strings.Contains(lower, copilotNotLicensedMarker):
		return verdict(copilotSeatNoSeat,
			"Login succeeded, but GitHub reports that "+subject+" has no Copilot seat "+
				"(\"not licensed to use Copilot\"). Agents on this backend cannot run inference until "+
				"a seat is active — check it at github.com/settings/copilot, or log in again with an "+
				"account that has one.")

	case resp.StatusCode == http.StatusForbidden:
		return verdict(copilotSeatBlocked,
			"Login succeeded and the seat may well be valid, but GitHub refused this request with "+
				"HTTP 403 and did not say the account is unlicensed. The usual cause is an organization "+
				"policy blocking this integration ID rather than anything wrong with the seat. Ask an org "+
				"owner to allow the Copilot CLI integration, or set HIVE_COPILOT_INTEGRATION_ID to an "+
				"integration your org permits.")

	case resp.StatusCode == http.StatusUnauthorized:
		return verdict(copilotSeatRejected,
			"A token was saved, but GitHub rejected it with HTTP 401 — the login did not fully activate. "+
				"Run the Copilot login again and make sure the device code is approved in the browser "+
				"before the code expires.")

	default:
		return verdict(copilotSeatUnreachable,
			"GitHub returned HTTP "+itoaStatus(resp.StatusCode)+" while verifying "+subject+
				", which says nothing about the account. This is transient or upstream; retry verification.")
	}
}

// itoaStatus renders a status code without pulling strconv into this file's
// import set for a single call site.
func itoaStatus(code int) string {
	if code <= 0 {
		return "0"
	}
	var digits []byte
	for code > 0 {
		digits = append([]byte{byte('0' + code%10)}, digits...)
		code /= 10
	}
	return string(digits)
}

// copilotSeatStatus returns the verdict for the credential hive currently
// holds, verifying only when force is set or no cached verdict covers that
// exact credential.
//
// The distinction matters because /api/copilot-auth/status is polled every few
// seconds during a login: it must never make a network call per poll. The
// login path and the explicit "verify" endpoint force a check; everything else
// reads the cache and reports copilotSeatUnknown when there is nothing to read.
func (s *Server) copilotSeatStatus(force bool) copilotSeatVerdict {
	token, source := s.copilotToken()
	if token == "" {
		return copilotSeatVerdict{State: copilotSeatUnknown}
	}
	if !force {
		if v, ok := s.copilotSeat.get(token); ok {
			return v
		}
		return copilotSeatVerdict{State: copilotSeatUnknown}
	}
	v := verifyCopilotSeat(token, describeCopilotCredential(source, s.copilotTokenLogin(token)))
	s.copilotSeat.put(token, v)
	return v
}
