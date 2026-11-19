package dashboard

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Meeting research agents on Settings, Meeting attendee
// (hivecommons/hive#18590 finding 6). When Betty cannot answer an addressed
// question well she says she will dig in and sends it to the research
// agent pool (research-1..N, pkg/research). The owner sees here which
// agents that is, how many can take a request now, and a plain warning
// when none can, instead of finding out from a silent meeting.

// MeetingsResearchStatus is the research pool as the runtime sees it now.
type MeetingsResearchStatus struct {
	// Agents are the pool's agent names (research-1..N).
	Agents []string `json:"agents"`
	// Available counts the agents registered and not paused.
	Available int `json:"available"`
	// BaseAgent is the configured agent whose backend and model the
	// research agents reuse.
	BaseAgent string `json:"base_agent,omitempty"`
	// Backend and Model are what the research agents run.
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
}

// MeetingsResearchStatusFunc reports the research pool; cmd/hive installs
// it when the pool is wired.
type MeetingsResearchStatusFunc func() MeetingsResearchStatus

// MeetingsResearchView is the research section of the attendee settings.
type MeetingsResearchView struct {
	// Wired is true when this hive has a research pool.
	Wired bool `json:"wired"`
	MeetingsResearchStatus
	// PoolSize is meetings.research_pool_size as resolved.
	PoolSize int `json:"pool_size"`
	// Warning is set when a deep dive would fail right now.
	Warning string `json:"warning,omitempty"`
}

// meetingsNoResearchWarning and meetingsResearchOffWarning are the
// settings page warnings.
const (
	meetingsNoResearchWarning  = "No research agent available: %s. Betty tells the meeting she couldn't look it up instead of answering a question she needs to research."
	meetingsResearchOffWarning = "No research agent available: this hive has no research pool (meetings were off when it started, or its runtime has no queued kicks). Betty tells the meeting she couldn't look it up instead of answering a question she needs to research."
)

// SetMeetingsResearchStatus installs the research pool report (nil
// removes it).
func (s *Server) SetMeetingsResearchStatus(fn MeetingsResearchStatusFunc) {
	s.meetingsAttendee.mu.Lock()
	defer s.meetingsAttendee.mu.Unlock()
	s.meetingsAttendee.research = fn
}

func (s *Server) meetingsResearchView(m config.MeetingsConfig) MeetingsResearchView {
	s.meetingsAttendee.mu.Lock()
	fn := s.meetingsAttendee.research
	s.meetingsAttendee.mu.Unlock()
	return meetingsResearchViewOf(m, fn)
}

func meetingsResearchViewOf(m config.MeetingsConfig, fn MeetingsResearchStatusFunc) MeetingsResearchView {
	v := MeetingsResearchView{PoolSize: m.ResearchPoolSizeOrDefault()}
	if fn == nil {
		if m.IsEnabled() {
			v.Warning = meetingsResearchOffWarning
		}
		return v
	}
	v.Wired, v.MeetingsResearchStatus = true, fn()
	if v.Available == 0 {
		names := "the research agents are not registered"
		if len(v.Agents) > 0 {
			names = strings.Join(v.Agents, ", ") + " are not registered with the runtime or are paused"
		}
		v.Warning = fmt.Sprintf(meetingsNoResearchWarning, names)
	}
	return v
}
