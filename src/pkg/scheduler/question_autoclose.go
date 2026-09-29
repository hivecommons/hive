package scheduler

import (
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// QuestionAutocloser is the scheduler's view of the question auto-closer
// (hivecommons/hive#9584, pkg/questionclose). The scheduler only feeds it the
// classified issue list and asks it for the scanner-lane instructions; the
// auto-closer does all GitHub I/O on its own loop, so kick building never
// waits on it.
type QuestionAutocloser interface {
	// Offer records this pass's classified issues. It must not block.
	Offer(issues []github.Issue)
	// KickSection returns the scanner-lane answer contract, or "" when the
	// feature is off.
	KickSection(issues []github.Issue) string
}

// questionAutocloserRole is the lane that answers question issues.
const questionAutocloserRole = "scanner"

// SetQuestionAutocloser attaches the question auto-closer. Nil (the default,
// and what cmd/hive passes when the feature is off) keeps every kick unchanged.
func (s *Scheduler) SetQuestionAutocloser(q QuestionAutocloser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.questionAutocloser = q
}

func (s *Scheduler) questionCloser() QuestionAutocloser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.questionAutocloser
}

// offerQuestions hands the classified list to the auto-closer, if attached.
func (s *Scheduler) offerQuestions(issues []github.Issue) {
	if q := s.questionCloser(); q != nil {
		q.Offer(issues)
	}
}

// addQuestionAnswerContract appends the answer contract to the scanner's kick
// at the post-resolution seam, so a config or repo-sourced scanner template
// cannot omit it.
func (s *Scheduler) addQuestionAnswerContract(agentName, message string, issues []github.Issue) string {
	if message == "" || s.cfg == nil || s.agentRole(agentName) != questionAutocloserRole {
		return message
	}
	q := s.questionCloser()
	if q == nil {
		return message
	}
	section := q.KickSection(issues)
	if section == "" {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + section
}
