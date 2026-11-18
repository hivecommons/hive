package dashboard

import "net/http"

// MeetingsBudgetService is the optional budget view of a MeetingsService
// (hivecommons/hive#18914): the per-meeting limits, the hive-wide hourly
// and daily ceilings used, and each meeting's meter. It carries numbers
// only, never question or transcript text.
type MeetingsBudgetService interface {
	Budget() (any, error)
}

// handleMeetingsBudget answers GET /api/meetings/budget. Unlike the rest
// of the meetings API it is open to every authenticated role, read-only
// included: the meter is how an operator sees that Betty is about to stop
// answering. The skipped questions' text stays on the owner-only
// GET /api/meetings/{id}.
func (s *Server) handleMeetingsBudget(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.meetingsService(w)
	if !ok {
		return
	}
	b, ok := svc.(MeetingsBudgetService)
	if !ok {
		jsonError(w, "meeting budget is not available on this hive", http.StatusServiceUnavailable)
		return
	}
	out, err := b.Budget()
	if err != nil {
		s.meetingsError(w, r, err)
		return
	}
	jsonResponse(w, out)
}
