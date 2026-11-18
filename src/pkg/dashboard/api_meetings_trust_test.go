package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// trustFakeMeetings adds the trusted-requester part to the fake service.
type trustFakeMeetings struct {
	fakeMeetingsService
	trusted   []string
	untrusted []string
}

func (f *trustFakeMeetings) ApproveAndTrust(_ context.Context, id string, _ bool, by string) (any, string, error) {
	f.trusted = append(f.trusted, id+"|"+by)
	return map[string]any{"id": id}, "900000000000000001", nil
}

func (f *trustFakeMeetings) TrustedRequesters() (any, error) {
	return []map[string]string{{"platform": "discord", "id": "900000000000000001", "name": "andy"}}, nil
}

func (f *trustFakeMeetings) Untrust(platform, id string) (bool, error) {
	f.untrusted = append(f.untrusted, platform+"/"+id)
	return id == "900000000000000001", nil
}

func TestMeetingsTrustOwnerApprovesAndAudits(t *testing.T) {
	s, _ := apiServer(t)
	f := &trustFakeMeetings{}
	s.SetMeetingsService(f)
	rec := meetingsOwnerPost(s, "/api/meetings/mtg-1/approve", `{"always":true}`)
	if rec.Code != http.StatusOK || len(f.trusted) != 1 || f.trusted[0] != "mtg-1|octo" {
		t.Fatalf("trust = %d %s %v", rec.Code, rec.Body.String(), f.trusted)
	}
	// The approval and the trust are each audited.
	var trustAudited, approveAudited bool
	for _, e := range s.audit.Recent(5) {
		trustAudited = trustAudited || (e.Action == "meeting_requester_trust" && strings.Contains(e.Detail, "trusted_id=900000000000000001"))
		approveAudited = approveAudited || e.Action == "meeting_request_approve"
	}
	if !trustAudited || !approveAudited {
		t.Fatalf("trust=%v approve=%v audited: %+v", trustAudited, approveAudited, s.audit.Recent(5))
	}
	if rec := doOwnerGet(s, "/api/meetings"); !strings.Contains(rec.Body.String(), "trusted_requesters") {
		t.Fatalf("list lacks trusted_requesters: %s", rec.Body.String())
	}
}

func TestMeetingsTrustReadOnlyRoleRefused(t *testing.T) {
	s, _ := apiServer(t)
	f := &trustFakeMeetings{}
	s.SetMeetingsService(f)
	for _, role := range []string{"read-only", "read-write"} {
		rec := meetingsDeleteAs(s, http.MethodPost, "/api/meetings/mtg-1/approve", `{"always":true}`, role, false)
		if rec.Code != http.StatusForbidden || len(f.trusted) != 0 {
			t.Fatalf("%s trust = %d, trusted=%v", role, rec.Code, f.trusted)
		}
		rec = meetingsDeleteAs(s, http.MethodDelete, "/api/meetings/trusted/discord/900000000000000001", "", role, false)
		if rec.Code != http.StatusForbidden || len(f.untrusted) != 0 {
			t.Fatalf("%s untrust = %d", role, rec.Code)
		}
	}
}

func TestMeetingsUntrustAudits(t *testing.T) {
	s, _ := apiServer(t)
	f := &trustFakeMeetings{}
	s.SetMeetingsService(f)
	rec := doOwnerDelete(s, "/api/meetings/trusted/discord/900000000000000001")
	if rec.Code != http.StatusOK {
		t.Fatalf("untrust = %d %s", rec.Code, rec.Body.String())
	}
	if e := s.audit.Recent(1); len(e) != 1 || e[0].Action != "meeting_requester_untrust" {
		t.Fatalf("untrust not audited: %+v", e)
	}
	if rec := doOwnerDelete(s, "/api/meetings/trusted/discord/42"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", rec.Code)
	}
}
