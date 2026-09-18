package chat

import (
	"context"
	"fmt"
	"strings"
)

func (s *Service) registerBuiltinCommands() {
	s.RegisterCommand("status", func(_ context.Context, _ string) (string, error) {
		return s.cmdStatus()
	})
	s.RegisterCommand("governor", func(_ context.Context, _ string) (string, error) {
		return s.cmdGovernor()
	})
	s.RegisterCommand("help", func(_ context.Context, _ string) (string, error) {
		return s.cmdHelp(), nil
	})
	s.RegisterCommand("kick", func(_ context.Context, args string) (string, error) {
		return s.cmdAgentAction("kick", args)
	})
	s.RegisterCommand("pause", func(_ context.Context, args string) (string, error) {
		return s.cmdAgentAction("pause", args)
	})
	s.RegisterCommand("resume", func(_ context.Context, args string) (string, error) {
		return s.cmdAgentAction("resume", args)
	})
}

func (s *Service) Deliver(ctx context.Context, msg Message) {
	s.routeMessage(ctx, msg)
}

func (s *Service) routeMessage(ctx context.Context, msg Message) {
	if msg.FromBot {
		return
	}

	content := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(content, "!") {
		return
	}

	// SECURITY (F8, CWE-862): command dispatch is gated on the allowlist and FAILS
	// CLOSED. Commands (!kick / agent commands) inject prompts and can reach
	// dashboardKick, which POSTs attacker-supplied text with the privileged
	// dashboard bearer — so an unconfigured allowlist must mean "no one is
	// authorized", not "everyone is authorized". This matches the documented
	// contract on Config.AllowedUsers ("Empty = commands disabled (fail closed)").
	// An empty allowlist rejects every command; operators enable command control
	// by populating allowed_users with the specific Discord user IDs they trust.
	if len(s.allowedUsers) == 0 {
		s.logger.Warn("discord: ignoring command — allowlist is empty (commands disabled; set allowed_users to enable)",
			"user_id", msg.AuthorID, "content", content)
		return
	}
	if _, ok := s.allowedUsers[msg.AuthorID]; !ok {
		s.logger.Warn("discord: ignoring command from non-allowlisted user",
			"user_id", msg.AuthorID, "content", content)
		return
	}

	content = content[1:]

	parts := strings.SplitN(content, " ", 2)
	cmd := strings.ToLower(parts[0])
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	cmd = resolveAlias(cmd)

	s.mu.RLock()
	handler, hasCmd := s.commands[cmd]
	s.mu.RUnlock()

	if hasCmd {
		reply, err := handler(ctx, args)
		if err != nil {
			reply = fmt.Sprintf("❌ %s", err)
		}
		if reply != "" {
			s.enqueue(reply)
		}
		return
	}

	if s.isValidAgent(cmd) {
		subParts := strings.SplitN(args, " ", 2)
		action := strings.ToLower(subParts[0])
		action = resolveAlias(action)
		rest := ""
		if len(subParts) > 1 {
			rest = subParts[1]
		}

		var reply string
		var err error
		switch action {
		case "pause":
			reply, err = s.dashboardPause(cmd)
		case "resume":
			reply, err = s.dashboardResume(cmd)
		case "kick":
			reply, err = s.dashboardKick(cmd, rest)
		default:
			reply, err = s.dashboardKick(cmd, args)
		}
		if err != nil {
			reply = fmt.Sprintf("❌ %s", err)
		}
		if reply != "" {
			s.enqueue(reply)
		}
		return
	}

	s.enqueue(fmt.Sprintf("❌ Unknown command: `%s`. Try `!help`", cmd))
}

func (s *Service) isValidAgent(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.agentNames {
		if a == name {
			return true
		}
	}
	return false
}
