package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type commandRoleContextKey struct{}

// inceptionSessionsPointer is the answer to every `!inception` subcommand. The
// /api/inception/* routes this command used to drive were retired (they now
// answer 410 Gone, replacement /api/sessions), and the interactive flow lives
// in the dashboard Sessions section, so chat no longer starts or steers one.
const inceptionSessionsPointer = "ℹ️ `!inception` is retired. Start an Inception session from the dashboard Sessions section."

// inceptionMultilineRefusal is returned, for the whole input, when the command
// text spans more than one line.
const inceptionMultilineRefusal = "❌ `!inception` takes a single line. Nothing was run. Start an Inception session from the dashboard Sessions section."

// inceptionOwnerSubcommands are the subcommands that were state-changing and
// therefore owner-gated before the command was retired. `state` and any other
// word stayed open to every allowlisted user and still do.
var inceptionOwnerSubcommands = map[string]struct{}{
	"start":   {},
	"scan":    {},
	"answer":  {},
	"skip":    {},
	"submit":  {},
	"facts":   {},
	"approve": {},
	"reset":   {},
}

func (s *Service) registerInceptionCommand() {
	s.RegisterCommand("inception", func(ctx context.Context, args string) (string, error) {
		return s.cmdInception(ctx, args)
	})
}

// cmdInception is a stub: it never calls the dashboard. Multi-line input is
// refused as a whole, owner-only subcommands keep their owner gate, and every
// other outcome is the pointer to the Sessions section.
func (s *Service) cmdInception(ctx context.Context, args string) (string, error) {
	trimmed := strings.TrimSpace(args)
	if strings.ContainsAny(trimmed, "\r\n") {
		return inceptionMultilineRefusal, nil
	}
	if fields := strings.Fields(trimmed); len(fields) > 0 {
		if _, gated := inceptionOwnerSubcommands[strings.ToLower(fields[0])]; gated {
			if err := requireCommandOwner(ctx); err != nil {
				return err.Error(), nil
			}
		}
	}
	return inceptionSessionsPointer, nil
}

func requireCommandOwner(ctx context.Context) error {
	role, _ := ctx.Value(commandRoleContextKey{}).(string)
	if !config.RoleAtLeast(role, config.RoleOwner) {
		return fmt.Errorf("❌ owner role required")
	}
	return nil
}
