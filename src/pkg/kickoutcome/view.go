package kickoutcome

import (
	"fmt"
	"time"
)

// View is one kick as the read API shows it. Summary is the one-line form
// every role may see; the rest is detail for owner and read-write roles.
type View struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
	// At is when the outcome happened (see Outcome.At).
	At time.Time `json:"at"`
}

// Detail is a View with the record behind it.
type Detail struct {
	View
	TriggerKind  string     `json:"trigger_kind"`
	TriggerActor string     `json:"trigger_actor,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	PRURL        string     `json:"pr_url,omitempty"`
	PRNumber     int        `json:"pr_number,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	SHA          string     `json:"sha,omitempty"`
	// RefusalCode is the kick-refusals docs reason code of a refused kick.
	RefusalCode string `json:"refusal_code,omitempty"`
	// Quote is the agent's own stand-down line, or the quoted text behind a
	// refusal. Untrusted: redacted and capped here, escaped where shown.
	Quote string `json:"quote,omitempty"`
}

// Summary is the one-line form of an outcome, without its age (the reader
// renders the age from At so it does not go stale between polls).
func Summary(r Record, o Outcome) string {
	switch o.Kind {
	case KindOpenedPR:
		if r.PRNumber > 0 {
			return fmt.Sprintf("opened PR #%d", r.PRNumber)
		}
		return "opened a PR"
	case KindPushedCommit:
		if r.PushBranch != "" {
			return "pushed a commit to " + r.PushBranch
		}
		return "pushed a commit"
	case KindCommented:
		return "commented"
	case KindStoodDown:
		return "stood down"
	case KindRefused:
		return "refused (" + r.RefusalCode + ")"
	case KindStillRunning:
		return "still running"
	}
	return "ended, no visible action"
}

// ViewOf derives the summary view of a record.
func ViewOf(r Record, now time.Time, latest bool) View {
	o := Derive(r, now, latest)
	return View{ID: r.ID, Agent: r.Agent, Kind: o.Kind, Summary: Summary(r, o), At: o.At}
}

// DetailOf derives the full view of a record.
func DetailOf(r Record, now time.Time, latest bool) Detail {
	o := Derive(r, now, latest)
	d := Detail{
		View:         View{ID: r.ID, Agent: r.Agent, Kind: o.Kind, Summary: Summary(r, o), At: o.At},
		TriggerKind:  r.TriggerKind,
		TriggerActor: r.TriggerActor,
		StartedAt:    r.StartedAt,
		EndedAt:      r.EndedAt,
		RefusalCode:  r.RefusalCode,
	}
	switch o.Kind {
	case KindOpenedPR:
		d.PRURL, d.PRNumber = r.PRURL, r.PRNumber
	case KindPushedCommit:
		d.Branch, d.SHA = r.PushBranch, r.PushSHA
	case KindStoodDown, KindRefused:
		// Only these two quote text, re-capped on the way out.
		d.Quote = Quote(o.Quote)
	}
	return d
}

// Latest is the most recent kick of every agent, as summary views.
func (s *Store) Latest() map[string]View {
	out := map[string]View{}
	now := s.clock()
	for _, a := range s.Agents() {
		if rs := s.Records(a, 1); len(rs) > 0 {
			out[a] = ViewOf(rs[0], now, true)
		}
	}
	return out
}

// Recent is the agent's last n kicks as detail views, newest first.
func (s *Store) Recent(agent string, n int) []Detail {
	now := s.clock()
	var out []Detail
	for i, r := range s.Records(agent, n) {
		out = append(out, DetailOf(r, now, i == 0))
	}
	return out
}

// LatestOf is the agent's most recent kick as a summary view.
func (s *Store) LatestOf(agent string) (View, bool) {
	rs := s.Records(agent, 1)
	if len(rs) == 0 {
		return View{}, false
	}
	return ViewOf(rs[0], s.clock(), true), true
}

func (s *Store) clock() time.Time {
	if s == nil {
		return time.Now()
	}
	return s.now()
}
