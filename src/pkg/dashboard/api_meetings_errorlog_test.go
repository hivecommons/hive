package dashboard

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// A failed meetings call is logged with its error: the hosted ingress
// replaces a 502 body with its generic page, so the spoke log is the only
// place the owner can read why a Discord join was refused.
func TestMeetingsCallFailureIsLogged(t *testing.T) {
	s, _ := apiServer(t)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f := &fakeMeetingsService{}
	s.SetMeetingsService(f)

	const refusal = "quorum meeting.join: blocked: the bot has no permission to connect to that voice channel"
	f.err = errors.New(refusal)
	for _, path := range []string{"/api/meetings/mtg-1/approve", "/api/meetings"} {
		buf.Reset()
		rec := meetingsOwnerPost(s, path, `{"platform":"discord","guild_id":"1","channel_id":"2","voice":true}`)
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), refusal) {
			t.Fatalf("POST %s = %d %s", path, rec.Code, rec.Body.String())
		}
		got := buf.String()
		for _, want := range []string{"level=WARN", "[meetings] call failed", "path=" + path, "status=502", "no permission to connect"} {
			if !strings.Contains(got, want) {
				t.Errorf("POST %s log missing %q:\n%s", path, want, got)
			}
		}
	}

	// A caller mistake is logged too, at Info.
	buf.Reset()
	f.err = fmt.Errorf("%w: mtg-9", ErrMeetingNotFound)
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-9/approve", ``); rec.Code != http.StatusNotFound {
		t.Fatalf("approve unknown = %d", rec.Code)
	}
	if got := buf.String(); !strings.Contains(got, "level=INFO") || !strings.Contains(got, "status=404") || !strings.Contains(got, "mtg-9") {
		t.Errorf("not-found log = %s", got)
	}
}

// meetingsActionsError keeps its 409 for disabled actions and logs it.
func TestMeetingsActionsErrorIsLogged(t *testing.T) {
	s, _ := apiServer(t)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	s.SetMeetingsActionsService(&fakeActionsService{err: fmt.Errorf("%w: off", ErrMeetingActionsDisabled)})
	if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/approve", ``, "owner", true); rec.Code != http.StatusConflict {
		t.Fatalf("approve disabled = %d", rec.Code)
	}
	if got := buf.String(); !strings.Contains(got, "[meetings] call failed") || !strings.Contains(got, "status=409") {
		t.Errorf("actions log = %s", got)
	}
}

// A nil logger never panics.
func TestMeetingsErrorNilLogger(t *testing.T) {
	s, _ := apiServer(t)
	s.logger = nil
	f := &fakeMeetingsService{err: errors.New("quorum down")}
	s.SetMeetingsService(f)
	if rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/approve", ``); rec.Code != http.StatusBadGateway {
		t.Fatalf("approve = %d", rec.Code)
	}
}
