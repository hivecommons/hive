package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type commandRoleContextKey struct{}

// inceptionRetiredReply answers every `!inception` command. The chat
// interview drove the /api/inception/* routes, which were retired in favour
// of Inception (spek) sessions (#18419) and now answer 410 Gone. A spek
// session is an interactive terminal, so chat points at the dashboard
// instead of driving it (#18565).
const inceptionRetiredReply = "ℹ️ `!inception` was retired with the old inception workflow. " +
	"Start an **Inception (spek)** session from the dashboard instead (Inception (spek) → new session), " +
	"or `POST /api/sessions`. See https://github.com/hivecommons/hive/blob/v6/src/docs/inception.md"

func (s *Service) registerInceptionCommand() {
	s.RegisterCommand("inception", func(_ context.Context, args string) (string, error) {
		return cmdInception(args), nil
	})
}

// cmdInception runs nothing. A message holding several `!inception` lines
// (a pasted interview) is refused as a whole, so no line is mistaken for an
// answer to another.
func cmdInception(args string) string {
	if n := countInceptionCommandLines(args); n > 0 {
		return fmt.Sprintf("❌ This message holds %d `!inception` lines; none of them ran. %s", n+1, inceptionRetiredReply)
	}
	return inceptionRetiredReply
}

// countInceptionCommandLines counts the further `!inception` lines after the
// first one in a multi-line message.
func countInceptionCommandLines(args string) int {
	n := 0
	for _, line := range strings.Split(args, "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.EqualFold(fields[0], "!inception") {
			n++
		}
	}
	return n
}

func requireCommandOwner(ctx context.Context) error {
	role, _ := ctx.Value(commandRoleContextKey{}).(string)
	if !config.RoleAtLeast(role, config.RoleOwner) {
		return fmt.Errorf("❌ owner role required")
	}
	return nil
}
