package dashboard

import (
	"net/http"
	"strings"
)

// contributeQueueControlsRequested reports whether GET /api/contribute/queue
// asked for the stored state behind the Operations card's queue controls
// (?controls=1). It is an explicit opt-in like ?withheld=1, so the payload
// every existing client parses stays byte-identical.
func contributeQueueControlsRequested(r *http.Request) bool {
	return r != nil && queryFlagEnabled(r.URL.Query().Get("controls"))
}

// contributeQueueControls returns the stored lists the queue controls edit:
// the operator's pinned order (Config.Hub.ContributeQueueOrder), the hold set
// (ContributeQueueHold) and each held key's note (ContributeQueueHoldReasons).
//
// The queue rows alone cannot answer what these lists hold. A pinned or held
// key whose issue is no longer admissible is not a row, and the rows' order
// mixes pinned issues with the default order. The admin MCP's
// contributor_queue.* previews read this to show an operator the list before
// and after a change, and to refuse a confirm when someone else changed it in
// between.
//
// Reasons come as a list of {key, reason} in hold order rather than a map
// keyed by issue, so the response has one stable order and no issue key is
// ever a JSON field name. Every list is non-nil, so "nothing pinned" is [] and
// never null.
func (s *Server) contributeQueueControls() map[string]any {
	order := []string{}
	hold := []string{}
	reasons := []map[string]string{}
	if s.deps != nil && s.deps.Config != nil {
		hub := s.deps.Config.Hub
		order = append(order, hub.ContributeQueueOrder...)
		hold = append(hold, hub.ContributeQueueHold...)
		for _, key := range hub.ContributeQueueHold {
			if reason := strings.TrimSpace(hub.ContributeQueueHoldReasons[key]); reason != "" {
				reasons = append(reasons, map[string]string{"key": key, "reason": reason})
			}
		}
	}
	return map[string]any{"order": order, "hold": hold, "hold_reasons": reasons}
}
