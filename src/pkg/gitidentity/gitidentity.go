// Package gitidentity resolves hive's lane-scoped git identity.
package gitidentity

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultBotEmailDomain matches hive_git_bot_identity in deploy/entrypoint.sh.
const DefaultBotEmailDomain = "hive.kubestellar.io"

// AgentIdentity returns the lane-distinct commit identity
// "<agent> <agent>@<HIVE_GIT_BOT_EMAIL_DOMAIN>" (#9478). The address is the
// bracket-free form DCO accepts. The domain is validated the same way the
// entrypoint does, falling back to the default. A name that is not a safe email
// local-part yields ok=false so callers leave git's own resolution untouched
// rather than export or enforce a malformed ident.
func AgentIdentity(agentName string) (name, email string, ok bool) {
	if !isIdentToken(agentName, true) {
		return "", "", false
	}
	domain := strings.TrimSpace(os.Getenv("HIVE_GIT_BOT_EMAIL_DOMAIN"))
	if !isIdentToken(domain, false) {
		domain = DefaultBotEmailDomain
	}
	return agentName, agentName + "@" + domain, true
}

// AppLinkedAgentIdentity returns the lane name with the App bot's GitHub
// noreply address. GitHub links commits carrying that address to the App bot
// account, while the human-readable name and `git commit -s` trailer still
// identify the lane that did the work.
func AppLinkedAgentIdentity(agentName string, appID int64, botLogin string) (name, email string, ok bool) {
	if !isIdentToken(agentName, true) || appID <= 0 {
		return "", "", false
	}
	botLogin = strings.TrimSpace(botLogin)
	if botLogin == "" || strings.ContainsAny(botLogin, " <>\n\r\t") {
		return "", "", false
	}
	return agentName, fmt.Sprintf("%s+%s@users.noreply.github.com", strconv.FormatInt(appID, 10), botLogin), true
}

func isIdentToken(s string, allowUnderscore bool) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
		case r == '_' && allowUnderscore:
		default:
			return false
		}
	}
	return true
}
