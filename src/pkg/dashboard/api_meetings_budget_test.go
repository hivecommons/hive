package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

const budgetTestQuestion = "Betty, when does the release ship?"

// budgetMeetingsService adds the budget view to the fake meetings service.
// Its owner-only Live carries a skipped question's text; the budget never
// does.
type budgetMeetingsService struct{ fakeMeetingsService }

func (b *budgetMeetingsService) Budget() (any, error) {
	return map[string]any{
		"token_budget": 200000, "intent_budget": 500,
		"ledger":   map[string]any{"hourly_used": 10, "hourly_ceiling": 500000, "daily_used": 20, "daily_ceiling": 2000000},
		"meetings": []map[string]any{{"meeting_id": "mtg-1", "tokens_used": 124000, "token_budget": 200000, "percent": 62, "level": "ok", "skipped": 1}},
	}, nil
}

func (b *budgetMeetingsService) Live(id string) (any, error) {
	return map[string]any{"meeting": map[string]string{"id": id}, "live": map[string]any{"skipped": []map[string]string{{"text": budgetTestQuestion}}}}, nil
}

// A read-only role sees the meter but never question text; the text is on
// the owner-only meeting route.
func TestMeetingsBudgetReadableByAnyRoleWithoutQuestionText(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doGetRole(s, "/api/meetings/budget", "read", false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service = %d", rec.Code)
	}
	s.SetMeetingsService(&budgetMeetingsService{})
	rec := doGetRole(s, "/api/meetings/budget", "read", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("read-only role on the meter = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"percent":62`, `"hourly_ceiling":500000`, `"daily_used":20`, `"skipped":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("meter lacks %s: %s", want, body)
		}
	}
	if strings.Contains(body, "release") {
		t.Fatalf("the meter must carry no question text: %s", body)
	}
	if rec := doGetRole(s, "/api/meetings/mtg-1", "read", false); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only role on the meeting = %d, want 403", rec.Code)
	}
	if rec := doOwnerGet(s, "/api/meetings/mtg-1"); !strings.Contains(rec.Body.String(), budgetTestQuestion) {
		t.Fatalf("the owner sees the skipped question: %s", rec.Body)
	}
}

// A meetings service without a budget view answers 503, not a panic.
func TestMeetingsBudgetUnsupportedService(t *testing.T) {
	s, _ := apiServer(t)
	s.SetMeetingsService(&fakeMeetingsService{})
	if rec := doGetRole(s, "/api/meetings/budget", "read", false); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("service without a budget = %d", rec.Code)
	}
}

// The dashboard renders the meter with a text alternative and the badge for
// each budget level, and lists skipped questions; the JS is generated from
// static-src.
func TestMeetingsBudgetMeterMarkup(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"% of the meeting budget used",
		"meetingsBudgetHtml(_meetingsBudget[m.id])",
		"Not answered: budget",
		"/api/meetings/budget",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
}
