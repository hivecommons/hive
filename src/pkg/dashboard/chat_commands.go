package dashboard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

const chatHintListLimit = 8

func (s *Server) chatCommandHintAnswer(query string) (string, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	q = strings.TrimSuffix(q, "?")
	q = strings.Join(strings.Fields(q), " ")
	switch {
	case q == "/help":
		return "Try `/agents`, `/beads`, `/prs`, `/governor`, `/knowledge work sources`, `/spek active campaigns`, `/spek spec runs`, `/who`, or `!help`.", true
	case strings.HasPrefix(q, "/agents stuck") || strings.HasPrefix(q, "agents: stuck"):
		return s.chatAgentsStuckAnswer(), true
	case strings.HasPrefix(q, "/agents scanner status") || strings.HasPrefix(q, "agents: scanner status"):
		return s.chatAgentNamedStatusAnswer("scanner"), true
	case strings.Contains(q, "which agent completed last"):
		return s.chatAgentCompletedLastAnswer(), true
	case strings.HasPrefix(q, "/beads high priority") || strings.HasPrefix(q, "beads: high priority"):
		return s.chatHighPriorityBeadsAnswer(), true
	case strings.HasPrefix(q, "/prs waiting on review") || strings.HasPrefix(q, "prs: waiting on review"):
		return s.chatPRsWaitingOnReviewAnswer(), true
	case strings.Contains(q, "release blocker"):
		return s.chatReleaseBlockersAnswer(), true
	case strings.HasPrefix(q, "/governor next kick") || strings.HasPrefix(q, "governor: next kick"):
		return s.chatGovernorNextKickAnswer(), true
	case strings.Contains(q, "governor paused"):
		return s.chatGovernorPausedAnswer(), true
	case strings.Contains(q, "recent kick failure"):
		return s.chatKickFailuresAnswer(), true
	case strings.HasPrefix(q, "/knowledge work sources") || strings.HasPrefix(q, "knowledge: work sources"):
		return s.chatKnowledgeWorkSourcesAnswer(), true
	case strings.Contains(q, "search docs"):
		if s.deps != nil && s.deps.ChatResponder != nil {
			return "", false
		}
		return s.chatKnowledgeSearchUnavailableAnswer(query), true
	case strings.Contains(q, "what changed in v5"):
		if s.deps != nil && s.deps.ChatResponder != nil {
			return "", false
		}
		return s.chatKnowledgeSearchUnavailableAnswer(query), true
	case q == "/who" || strings.HasPrefix(q, "/jam who is online"):
		return s.chatWhoAnswer(), true
	case strings.Contains(q, "contributor activity"):
		return s.chatContributorActivityAnswer(), true
	case strings.HasPrefix(q, "!runs spec"):
		return s.chatSpecRunsAnswer(), true
	case strings.HasPrefix(q, "/spek active campaigns") || strings.HasPrefix(q, "spek: active campaigns"):
		return s.chatSpekCampaignsAnswer(), true
	case strings.HasPrefix(q, "/spek spec runs") || q == "/spek runs" || strings.HasPrefix(q, "spek: spec runs"):
		return s.chatSpecRunsAnswer(), true
	case strings.Contains(q, "inception relay state"):
		return s.chatInceptionRelayStateAnswer(), true
	case q == "/agents":
		return s.chatAgentsAnswer(), true
	case q == "/beads":
		return s.chatBeadsAnswer(), true
	case q == "/prs":
		return s.chatPRsAnswer(), true
	case q == "/governor":
		return s.chatGovernorAnswer(), true
	case q == "/spek":
		return s.chatSpekAnswer(), true
	}
	return "", false
}

func (s *Server) chatStatusRuns() []Run {
	if status := s.chatStatusSnapshot(); status != nil && len(status.Runs) > 0 {
		return append([]Run(nil), status.Runs...)
	}
	runs, err := s.activeRuns(true)
	if err != nil {
		return nil
	}
	return runs
}

func (s *Server) chatSpecRunsAnswer() string {
	runs := s.chatStatusRuns()
	if len(runs) == 0 {
		if _, err := s.activeRuns(true); err != nil {
			return "Spec-run data is unavailable: " + strings.TrimSpace(err.Error()) + ". Check the Inception/Spektacular panels after the run lease registry starts."
		}
		return "No active spec runs are currently reported by `/api/runs`."
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StageStartedAt != runs[j].StageStartedAt {
			return runs[i].StageStartedAt > runs[j].StageStartedAt
		}
		return runs[i].Key < runs[j].Key
	})
	lines := []string{fmt.Sprintf("Spec runs (%d):", len(runs))}
	for i, run := range runs {
		if i >= chatHintListLimit {
			lines = append(lines, fmt.Sprintf("- … %d more", len(runs)-i))
			break
		}
		lines = append(lines, "- "+chatRunLine(run))
	}
	return strings.Join(lines, "\n")
}

func chatRunLine(run Run) string {
	id := firstRunNonEmpty(run.Key, run.LeaseKey, "(unknown run)")
	state := firstRunNonEmpty(run.State, run.Stage, "unknown")
	stage := firstRunNonEmpty(run.Stage, "unknown")
	assignee := firstRunNonEmpty(run.Assignee, run.ClaimedBy, "unassigned")
	detail := fmt.Sprintf("%s — state %s, stage %s, gen %d, assignee %s", id, state, stage, run.Gen, assignee)
	if run.Repo != "" {
		detail += ", repo " + run.Repo
	}
	if run.WaitingOn != "" {
		detail += ", waiting on " + string(run.WaitingOn)
	}
	if run.CurrentStep != "" {
		detail += ", step " + run.CurrentStep
	}
	return detail
}

func (s *Server) chatSpekCampaignsAnswer() string {
	campaigns, err := s.allCampaigns(nil)
	if err != nil {
		return "Campaign data is unavailable: " + strings.TrimSpace(err.Error()) + ". Check `/api/campaigns` and the Inception/Spektacular panels."
	}
	if len(campaigns) == 0 {
		for _, run := range s.chatStatusRuns() {
			campaigns = append(campaigns, campaignFromRun(run))
		}
	}
	if len(campaigns) == 0 {
		return "No active Spektacular or inception campaigns are currently reported by `/api/campaigns`."
	}
	sort.Slice(campaigns, func(i, j int) bool { return campaigns[i].LastActivity > campaigns[j].LastActivity })
	lines := []string{fmt.Sprintf("Active campaigns (%d):", len(campaigns))}
	for i, campaign := range campaigns {
		if i >= chatHintListLimit {
			lines = append(lines, fmt.Sprintf("- … %d more", len(campaigns)-i))
			break
		}
		lines = append(lines, "- "+chatCampaignLine(campaign))
	}
	return strings.Join(lines, "\n")
}

func chatCampaignLine(c Campaign) string {
	id := firstRunNonEmpty(c.ID, c.RunKey, "(unknown campaign)")
	stage := firstRunNonEmpty(c.CurrentStage, "unknown")
	status := firstRunNonEmpty(c.Status, "unknown")
	engine := firstRunNonEmpty(c.Engine, c.Type, "campaign")
	line := fmt.Sprintf("%s — %s, stage %s, status %s", id, engine, stage, status)
	if c.RunKey != "" {
		line += ", run " + c.RunKey
	}
	if c.LeaseOwner != "" {
		line += ", owner " + c.LeaseOwner
	}
	if len(c.Repos) > 0 {
		line += ", repos " + strings.Join(c.Repos, ", ")
	}
	return line
}

func (s *Server) chatAgentsStuckAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Agent status is still initializing; stuck-agent data is not available yet."
	}
	var lines []string
	for _, a := range status.Agents {
		state := strings.ToLower(strings.TrimSpace(a.State + " " + a.Busy + " " + a.StructuredStatus + " " + a.LastError + " " + a.KickOutcome))
		if a.Paused || strings.Contains(state, "stuck") || strings.Contains(state, "blocked") || strings.Contains(state, "error") || strings.Contains(state, "fail") {
			lines = append(lines, "- "+chatAgentLine(a))
		}
	}
	if len(lines) == 0 {
		return "No stuck, paused, blocked, or failing agents are currently reported."
	}
	sort.Strings(lines)
	return "Agents needing attention:\n" + strings.Join(lines, "\n")
}

func (s *Server) chatAgentNamedStatusAnswer(name string) string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Agent status is still initializing; `" + name + "` status is not available yet."
	}
	for _, a := range status.Agents {
		if strings.EqualFold(a.Name, name) || strings.EqualFold(a.ID, name) {
			return "Agent `" + name + "`: " + chatAgentLine(a)
		}
	}
	return "Agent `" + name + "` is not currently reported in hive status."
}

func chatAgentLine(a FrontendAgent) string {
	state := firstRunNonEmpty(a.State, "unknown")
	if a.Paused {
		state += " (paused"
		if a.PausedReason != "" {
			state += ": " + a.PausedReason
		}
		state += ")"
	}
	detail := firstRunNonEmpty(a.Doing, a.StructuredStatus, a.LastError, a.KickOutcomeReason)
	if detail == "" && a.NextKick != "" {
		detail = "next kick " + a.NextKick
	}
	if detail == "" {
		return a.Name + ": " + state
	}
	return a.Name + ": " + state + " — " + detail
}

func (s *Server) chatAgentCompletedLastAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil || len(status.Agents) == 0 {
		return "Agent completion data is unavailable: hive status has not reported agents yet."
	}
	var best *FrontendAgent
	for i := range status.Agents {
		if strings.TrimSpace(status.Agents[i].LastKickAt) == "" {
			continue
		}
		if best == nil || status.Agents[i].LastKickAt > best.LastKickAt {
			best = &status.Agents[i]
		}
	}
	if best == nil {
		return "Agent completion data is unavailable: the status snapshot has no `lastKickAt`/completion timestamps. Check the activity/audit panel for historical completions."
	}
	return "Most recent agent activity: " + best.Name + " at " + best.LastKickAt + " — " + firstRunNonEmpty(best.KickOutcome, best.State, "unknown outcome") + "."
}

func (s *Server) chatHighPriorityBeadsAnswer() string {
	if s.deps == nil || len(s.deps.BeadStores) == 0 {
		return "High-priority bead data is unavailable: no bead stores are configured."
	}
	var lines []string
	for name, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		store.ReadEach(beads.ListFilter{}, func(b *beads.Bead) {
			if b.Status == beads.StatusOpen && b.Priority <= beads.PriorityHigh {
				lines = append(lines, fmt.Sprintf("- %s: %s [priority %d, %s]", name, b.Title, b.Priority, b.ID))
			}
		})
	}
	if len(lines) == 0 {
		return "No open critical/high-priority beads are currently reported."
	}
	sort.Strings(lines)
	if len(lines) > chatHintListLimit {
		lines = append(lines[:chatHintListLimit], fmt.Sprintf("- … %d more", len(lines)-chatHintListLimit))
	}
	return "High-priority beads:\n" + strings.Join(lines, "\n")
}

func (s *Server) chatPRsWaitingOnReviewAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Pull request review data is still initializing."
	}
	var lines []string
	total := 0
	for _, repo := range status.Repos {
		count := len(repo.OpenPrs)
		total += count
		if count == 0 {
			continue
		}
		name := firstRunNonEmpty(repo.Full, repo.Name, "unknown repo")
		lines = append(lines, fmt.Sprintf("- %s: %d open PR(s) that may need review", name, count))
	}
	if total == 0 {
		return "No open pull requests are currently reported as waiting on review."
	}
	sort.Strings(lines)
	return fmt.Sprintf("PRs waiting on review (%d):\n%s", total, strings.Join(lines, "\n"))
}

func (s *Server) chatReleaseBlockersAnswer() string {
	var blockers []string
	if status := s.chatStatusSnapshot(); status != nil {
		for _, a := range status.Agents {
			if a.Paused || a.LastError != "" {
				blockers = append(blockers, "- agent "+chatAgentLine(a))
			}
		}
		for _, repo := range status.Repos {
			if repo.Paused {
				blockers = append(blockers, "- repo "+firstRunNonEmpty(repo.Full, repo.Name)+" paused: "+firstRunNonEmpty(repo.PauseReason, "no reason recorded"))
			}
		}
		for _, run := range status.Runs {
			if run.WaitingOn != "" {
				blockers = append(blockers, "- run "+chatRunLine(run))
			}
		}
	}
	if len(blockers) == 0 {
		return "No release blockers are currently visible in agents, repos, or spec runs. If this looks wrong, refresh `/api/status`."
	}
	sort.Strings(blockers)
	if len(blockers) > chatHintListLimit {
		blockers = append(blockers[:chatHintListLimit], fmt.Sprintf("- … %d more", len(blockers)-chatHintListLimit))
	}
	return "Open release blockers:\n" + strings.Join(blockers, "\n")
}

func (s *Server) chatGovernorNextKickAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Governor kick data is still initializing."
	}
	var lines []string
	for _, a := range status.Agents {
		if strings.TrimSpace(a.NextKick) != "" || strings.TrimSpace(a.NextKickIn) != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s %s", a.Name, strings.TrimSpace(a.NextKick), strings.TrimSpace(a.NextKickIn)))
		}
	}
	if len(lines) == 0 {
		return "No next governor kicks are currently scheduled in the status snapshot."
	}
	sort.Strings(lines)
	return "Next governor kicks:\n" + strings.Join(lines, "\n")
}

func (s *Server) chatGovernorPausedAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Governor pause data is still initializing."
	}
	var lines []string
	for _, a := range status.Agents {
		if a.Paused {
			lines = append(lines, fmt.Sprintf("- %s: %s", a.Name, firstRunNonEmpty(a.PausedReason, a.PausedTrigger, "no reason recorded")))
		}
	}
	for _, repo := range status.Repos {
		if repo.Paused {
			lines = append(lines, fmt.Sprintf("- repo %s: %s", firstRunNonEmpty(repo.Full, repo.Name), firstRunNonEmpty(repo.PauseReason, "no reason recorded")))
		}
	}
	if len(lines) == 0 {
		return "The governor is not reporting any paused agents or repositories."
	}
	sort.Strings(lines)
	return "Paused governor targets:\n" + strings.Join(lines, "\n")
}

func (s *Server) chatKickFailuresAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Kick failure data is still initializing."
	}
	var lines []string
	for _, a := range status.Agents {
		if a.LastError != "" || strings.Contains(strings.ToLower(a.KickOutcome), "fail") || strings.Contains(strings.ToLower(a.KickOutcome), "question") {
			lines = append(lines, fmt.Sprintf("- %s: %s", a.Name, firstRunNonEmpty(a.LastError, a.KickOutcomeReason, a.KickOutcome)))
		}
	}
	if len(lines) == 0 {
		return "No recent kick failures are currently reported."
	}
	sort.Strings(lines)
	return "Recent kick failures:\n" + strings.Join(lines, "\n")
}

func (s *Server) chatKnowledgeWorkSourcesAnswer() string {
	if s.deps == nil || s.deps.Config == nil {
		return "Work-source data is unavailable: dashboard config is not loaded."
	}
	ws := s.deps.Config.Governor.WorkSource
	sourceType := firstRunNonEmpty(ws.Type, "github")
	lines := []string{"Configured work sources:", "- primary: " + sourceType}
	switch sourceType {
	case "github_projects":
		lines = append(lines, fmt.Sprintf("- GitHub Projects: org=%s project=%d default_repo=%s states=%s",
			firstRunNonEmpty(ws.GitHubProjects.Org, "(unset)"), ws.GitHubProjects.ProjectNumber, firstRunNonEmpty(ws.GitHubProjects.DefaultRepo, "(unset)"), strings.Join(ws.GitHubProjects.States, ",")))
	case "linear":
		lines = append(lines, fmt.Sprintf("- Linear: api_key_set=%t session_agent=%s assigned_only=%t teams=%d",
			ws.Linear.APIKey != "", firstRunNonEmpty(ws.Linear.SessionAgent, "(unset)"), ws.Linear.AssignedOnly, len(ws.Linear.Teams)))
	case "jira":
		lines = append(lines, fmt.Sprintf("- Jira: base_url=%s projects=%s repo=%s",
			firstRunNonEmpty(ws.Jira.BaseURL, "(unset)"), strings.Join(ws.Jira.ProjectKeys, ","), firstRunNonEmpty(ws.Jira.Repo, "(unset)")))
	case "gitea":
		lines = append(lines, fmt.Sprintf("- Gitea/Forgejo: base_url=%s repos=%d labels=%s",
			firstRunNonEmpty(ws.Gitea.BaseURL, "(unset)"), len(ws.Gitea.Repos), strings.Join(ws.Gitea.Labels, ",")))
	case "gitlab":
		lines = append(lines, fmt.Sprintf("- GitLab: base_url=%s repos=%d labels=%s",
			firstRunNonEmpty(ws.GitLab.BaseURL, "(default gitlab.com)"), len(ws.GitLab.Repos), strings.Join(ws.GitLab.Labels, ",")))
	default:
		if sourceType == "github" {
			lines = append(lines, "- GitHub issues/PRs from configured repositories are the active work source.")
		}
	}
	if ws.RunStages {
		lines = append(lines, "- run stages: enabled")
	}
	if ws.Wavefront.Enabled {
		lines = append(lines, "- wavefront: enabled path="+firstRunNonEmpty(ws.Wavefront.Path, ws.Wavefront.URL, "(unset)")+" repo="+firstRunNonEmpty(ws.Wavefront.Repo, "(unset)"))
	}
	return strings.Join(lines, "\n")
}

func (s *Server) chatKnowledgeSearchUnavailableAnswer(query string) string {
	if s.deps != nil && s.deps.Knowledge != nil {
		return "Knowledge search is configured, but Hive Chat has no deterministic handler for `" + strings.TrimSpace(query) + "` yet. Use the Knowledge panel search endpoint or ask a narrower command."
	}
	return "Knowledge search is unavailable: no chat model/responder or knowledge API is configured for Hive Chat. Configure the dashboard chat responder or knowledge vault, then retry `" + strings.TrimSpace(query) + "`."
}

func (s *Server) chatWhoAnswer() string {
	var lines []string
	if status := s.chatStatusSnapshot(); status != nil {
		running := 0
		for _, a := range status.Agents {
			if strings.EqualFold(a.State, "running") || strings.EqualFold(a.State, "idle") {
				running++
			}
		}
		lines = append(lines, fmt.Sprintf("- agents reported: %d (%d running/idle)", len(status.Agents), running))
	}
	if s.contributeHub != nil {
		fleet := s.contributeHub.FleetSnapshot()
		lines = append(lines, fmt.Sprintf("- contributor agents online: %d", len(fleet.Clankers)))
		for i, c := range fleet.Clankers {
			if i >= chatHintListLimit {
				lines = append(lines, fmt.Sprintf("- … %d more contributors", len(fleet.Clankers)-i))
				break
			}
			name := firstRunNonEmpty(c.GitHubUsername, c.ContributorID, "(unknown)")
			lines = append(lines, "- "+name+": "+firstRunNonEmpty(c.Role, c.AssignedRole, "contributor")+" "+firstRunNonEmpty(c.Model, c.CLIBackend, ""))
		}
	} else {
		lines = append(lines, "- contributor agents online: unavailable (contributor hub not started)")
	}
	if len(lines) == 0 {
		return "Presence data is unavailable: status and contributor hub are not initialized."
	}
	return "Who is online:\n" + strings.Join(lines, "\n")
}

func (s *Server) chatContributorActivityAnswer() string {
	if s.contributeHub == nil {
		return "Contributor activity is unavailable: contributor hub is not started."
	}
	activity := s.contributeHub.RecentActivity()
	if len(activity) == 0 {
		return "No contributor activity has been retained yet."
	}
	if len(activity) > chatHintListLimit {
		activity = activity[len(activity)-chatHintListLimit:]
	}
	lines := []string{"Recent contributor activity:"}
	for _, entry := range activity {
		who := firstRunNonEmpty(entry.Username, "unknown contributor")
		action := firstRunNonEmpty(entry.Action, "activity")
		task := strings.TrimSpace(entry.Task)
		if task != "" {
			action += " " + task
		}
		lines = append(lines, "- "+firstRunNonEmpty(entry.Timestamp, "(no timestamp)")+" "+who+": "+action)
	}
	return strings.Join(lines, "\n")
}

func (s *Server) chatInceptionRelayStateAnswer() string {
	parts := []string{}
	if s.deps != nil && s.deps.Inception != nil {
		if state := s.deps.Inception.GetState(); state != nil {
			parts = append(parts, fmt.Sprintf("inception engine active: idea=%s phase=%s", firstRunNonEmpty(state.IdeaSlug, "(unslugged)"), state.Phase))
		} else {
			parts = append(parts, "inception engine configured but no active state is loaded")
		}
	} else {
		parts = append(parts, "inception engine is not configured")
	}
	if s.contributeHub != nil {
		fleet := s.contributeHub.FleetSnapshot()
		parts = append(parts, "relay contributors online: "+strconv.Itoa(len(fleet.Clankers)))
	} else {
		parts = append(parts, "relay contributor hub is not started")
	}
	if runs := s.chatStatusRuns(); len(runs) > 0 {
		parts = append(parts, "spec runs visible: "+strconv.Itoa(len(runs)))
	} else {
		parts = append(parts, "spec runs visible: 0")
	}
	parts = append(parts, "If a relay follow-up is missing, it is because Hive Chat has no asynchronous dashboard bot responder for this text; this command now reports the local relay state directly.")
	return "Inception relay state: " + strings.Join(parts, "; ") + "."
}
