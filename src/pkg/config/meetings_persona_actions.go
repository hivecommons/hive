package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Meeting action requests (hivecommons/hive#18406): people ask the persona
// in a meeting, spoken or typed, to open an issue, open an RFC, start a
// Spek session or save a knowledge note. She drafts the item and it waits
// for an owner or read-write user to approve it in the dashboard; nothing
// is created without that approval, except knowledge notes when
// auto_approve.knowledge is on (no other type may be auto-approved).

// Meeting action types (meetings.persona.actions.types).
const (
	MeetingsActionIssue     = "issue"
	MeetingsActionRFC       = "rfc"
	MeetingsActionSpek      = "spek"
	MeetingsActionKnowledge = "knowledge"
)

// MeetingsActionTypes are every action type, in display order.
var MeetingsActionTypes = []string{MeetingsActionIssue, MeetingsActionRFC, MeetingsActionSpek, MeetingsActionKnowledge}

// meetingsActionAutoApprovable are the types auto_approve may turn on.
var meetingsActionAutoApprovable = map[string]bool{MeetingsActionKnowledge: true}

// Meeting action env overrides. Types is comma separated.
const (
	MeetingsActionsEnabledEnv              = "HIVE_MEETINGS_PERSONA_ACTIONS_ENABLED"
	MeetingsActionsTypesEnv                = "HIVE_MEETINGS_PERSONA_ACTIONS_TYPES"
	MeetingsActionsDefaultRepoEnv          = "HIVE_MEETINGS_PERSONA_ACTIONS_DEFAULT_REPO"
	MeetingsActionsAutoApproveKnowledgeEnv = "HIVE_MEETINGS_PERSONA_ACTIONS_AUTO_APPROVE_KNOWLEDGE"
)

// MeetingsPersonaActions is meetings.persona.actions.
type MeetingsPersonaActions struct {
	// Enabled lets people ask the persona to draft items. Nil defaults true:
	// every draft still waits for approval.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Types are the allowed action types (issue, rfc, spek, knowledge).
	// Empty allows all of them.
	Types []string `yaml:"types,omitempty" json:"types,omitempty"`
	// DefaultRepo (owner/repo) is where an issue, RFC or Spek session goes
	// when the request does not name a monitored repo. Empty falls back to
	// inbox.spek.repo, then project.primary_repo, then the first repo.
	DefaultRepo string `yaml:"default_repo,omitempty" json:"default_repo,omitempty"`
	// AutoApprove turns on approval-free creation per type. Only knowledge
	// may be true; every type defaults false.
	AutoApprove map[string]bool `yaml:"auto_approve,omitempty" json:"auto_approve,omitempty"`
}

// ResolvedMeetingsActions is the effective meetings.persona.actions.
type ResolvedMeetingsActions struct {
	Enabled     bool
	Types       []string
	DefaultRepo string
	// AutoApprove holds every type, false unless turned on.
	AutoApprove map[string]bool
}

// IsEnabled reports whether action requests are on: env, then config,
// default true.
func (a MeetingsPersonaActions) IsEnabled() bool {
	if v, ok := parseBoolEnv(MeetingsActionsEnabledEnv); ok {
		return v
	}
	return a.Enabled == nil || *a.Enabled
}

// Resolve returns the effective actions settings. Unknown types are
// dropped (Validate reports them); auto-approve is kept only for types that
// allow it.
func (a MeetingsPersonaActions) Resolve() ResolvedMeetingsActions {
	types := meetingsEnvList(MeetingsActionsTypesEnv, a.Types, MeetingsActionTypes)
	known := map[string]bool{}
	for _, t := range MeetingsActionTypes {
		known[t] = true
	}
	var allowed []string
	seen := map[string]bool{}
	for _, t := range types {
		if known[t] && !seen[t] {
			seen[t] = true
			allowed = append(allowed, t)
		}
	}
	auto := map[string]bool{}
	for _, t := range MeetingsActionTypes {
		auto[t] = meetingsActionAutoApprovable[t] && a.AutoApprove[t]
	}
	if v, ok := parseBoolEnv(MeetingsActionsAutoApproveKnowledgeEnv); ok {
		auto[MeetingsActionKnowledge] = v
	}
	return ResolvedMeetingsActions{
		Enabled:     a.IsEnabled(),
		Types:       allowed,
		DefaultRepo: strings.Trim(meetingsEnvString(MeetingsActionsDefaultRepoEnv, a.DefaultRepo, ""), "/"),
		AutoApprove: auto,
	}
}

// Allows reports whether an action type is on.
func (r ResolvedMeetingsActions) Allows(t string) bool {
	if !r.Enabled {
		return false
	}
	for _, x := range r.Types {
		if x == t {
			return true
		}
	}
	return false
}

// meetingsRepoShape is owner/repo.
var meetingsRepoShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

// Validate checks meetings.persona.actions as written and with env.
func (a MeetingsPersonaActions) Validate() error {
	known := map[string]bool{}
	for _, t := range MeetingsActionTypes {
		known[t] = true
	}
	for _, t := range meetingsEnvList(MeetingsActionsTypesEnv, a.Types, nil) {
		if !known[t] {
			return fmt.Errorf("meetings.persona.actions.types: %q is not one of %s", t, strings.Join(MeetingsActionTypes, ", "))
		}
	}
	for t, on := range a.AutoApprove {
		key := strings.ToLower(strings.TrimSpace(t))
		if !known[key] {
			return fmt.Errorf("meetings.persona.actions.auto_approve: %q is not one of %s", t, strings.Join(MeetingsActionTypes, ", "))
		}
		if on && !meetingsActionAutoApprovable[key] {
			return fmt.Errorf("meetings.persona.actions.auto_approve.%s: only knowledge notes may be auto-approved; issues, RFCs and Spek sessions always wait for approval", key)
		}
	}
	if repo := strings.Trim(meetingsEnvString(MeetingsActionsDefaultRepoEnv, a.DefaultRepo, ""), "/"); repo != "" && !meetingsRepoShape.MatchString(repo) {
		return fmt.Errorf("meetings.persona.actions.default_repo %q is not owner/repo", repo)
	}
	if raw := strings.TrimSpace(os.Getenv(MeetingsActionsAutoApproveKnowledgeEnv)); raw != "" {
		if _, ok := parseBoolEnv(MeetingsActionsAutoApproveKnowledgeEnv); !ok {
			return fmt.Errorf("%s=%q is not a boolean", MeetingsActionsAutoApproveKnowledgeEnv, raw)
		}
	}
	return nil
}

// MeetingsActionRepos returns the repos meeting drafts may target, as
// owner/repo: the active project repos.
func (c *Config) MeetingsActionRepos() []string {
	if c == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		r = strings.Trim(strings.TrimSpace(r), "/")
		if r == "" {
			return
		}
		if !strings.Contains(r, "/") && c.Project.Org != "" {
			r = c.Project.Org + "/" + r
		}
		if !meetingsRepoShape.MatchString(r) || seen[strings.ToLower(r)] {
			return
		}
		seen[strings.ToLower(r)] = true
		out = append(out, r)
	}
	for _, r := range c.ActiveRepos() {
		add(r)
	}
	if len(out) == 0 {
		add(c.Project.PrimaryRepo)
	}
	return out
}

// MeetingsActionDefaultRepo is the repo a draft goes to when the request
// names none: meetings.persona.actions.default_repo, then inbox.spek.repo,
// then project.primary_repo, then the first monitored repo.
func (c *Config) MeetingsActionDefaultRepo() string {
	if c == nil {
		return ""
	}
	qualify := func(r string) string {
		r = strings.Trim(strings.TrimSpace(r), "/")
		if r != "" && !strings.Contains(r, "/") && c.Project.Org != "" {
			r = c.Project.Org + "/" + r
		}
		return r
	}
	for _, r := range []string{c.Meetings.Persona.Actions.Resolve().DefaultRepo, c.Inbox.Spek.Repo, c.Project.PrimaryRepo} {
		if q := qualify(r); q != "" {
			return q
		}
	}
	if repos := c.MeetingsActionRepos(); len(repos) > 0 {
		return repos[0]
	}
	return ""
}
