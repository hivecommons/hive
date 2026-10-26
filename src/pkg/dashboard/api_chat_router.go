package dashboard

import (
	"regexp"
	"strings"
)

// Chat routing. The frontend sends slash commands as `<scope>: <args>`
// (`/agents stuck` → "agents: stuck", bare `/agents` → "agents: status") and
// free text verbatim. Before this router existed the server tokenised the
// whole message into a bag of words and matched on keywords alone — any
// message containing "agent" produced the full agent roster, any message
// containing "pr" produced per-repo PR counts, and every other word was
// discarded. That is why `/agents stuck`, `/agents scanner status`,
// `/prs waiting on review` and "Which agent completed last?" all came back
// as the same canned dump.
//
// The router now separates three cases:
//
//   - chatRouteBare: a bare command, or a one-word keyword question
//     ("agents", "prs?", "status"). The deterministic handler answers with
//     its summary, exactly as before.
//   - chatRouteGrounded: a command with arguments, or a free-text question.
//     The arguments / question are kept and answered from a scoped hive
//     state snapshot (api_chat_grounded.go). When grounding is unavailable
//     the reply is the command's usage — never the bare dump.
//   - chatRouteHelp / chatRoutePresence: the existing local answers.

type chatRouteKind int

const (
	chatRouteHelp chatRouteKind = iota
	chatRoutePresence
	chatRouteBare
	chatRouteGrounded
)

// chatRoute is the router's decision for one message.
type chatRoute struct {
	Kind chatRouteKind
	// Scope is the command dataset: agents, prs, beads, status, governor,
	// knowledge, jam, spek. "" means the question is unscoped free text and
	// the full snapshot applies.
	Scope string
	// Question is the argument text (scoped) or the whole message (unscoped)
	// a grounded answer must address. Empty for bare routes.
	Question string
	// Raw is the trimmed original message, for deterministic matchers that
	// parse their own argument vocabulary.
	Raw string
}

// chatScopeAliases maps the first token / scope prefix to a dataset.
var chatScopeAliases = map[string]string{
	"agents": "agents", "agent": "agents",
	"prs": "prs", "pr": "prs", "pull": "prs", "pulls": "prs",
	"beads": "beads", "bead": "beads", "issues": "beads", "issue": "beads",
	"status": "status", "health": "status",
	"governor": "governor", "gov": "governor",
	"knowledge": "knowledge", "kb": "knowledge",
	"jam": "jam", "swarm": "jam",
	"spek": "spek", "spectacular": "spek", "spec": "spek",
	"help": "help",
}

// chatKeywordScopes is the keyword → dataset hint used to pick the snapshot
// scope for an unprefixed free-text question. Deliberately narrow: an
// unmatched question gets the full snapshot rather than a guess.
var chatKeywordScopes = []struct {
	scope string
	words []string
}{
	{"agents", []string{"agent", "agents", "scanner", "reviewer", "fixer", "triage", "kick", "kicked", "blocked", "stuck", "idle"}},
	{"prs", []string{"pr", "prs", "pull", "pulls", "review", "ci", "merge", "merged"}},
	{"beads", []string{"bead", "beads", "issue", "issues", "actionable"}},
	{"governor", []string{"governor", "cadence"}},
}

var chatScopePrefixRE = regexp.MustCompile(`^/?([a-z]+):\s*(.*)$`)

// chatBareArgs are argument strings the frontend sends for a bare command
// (chatForwardCommand defaults an empty argument to "status").
func chatBareArgs(args string) bool {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "", "status", "list", "all":
		return true
	}
	return false
}

// chatParseRoute decides how a message is answered. It never discards
// arguments: a scoped message with arguments is a grounded route carrying
// exactly those arguments.
func chatParseRoute(query string) chatRoute {
	trimmed := strings.TrimSpace(query)
	lower := strings.ToLower(trimmed)
	route := chatParseRouteKind(trimmed, lower)
	route.Raw = trimmed
	return route
}

func chatParseRouteKind(trimmed, lower string) chatRoute {

	// `scope: args` (frontend forwarding) or `/scope args` (typed slash).
	// The prefix is ASCII, so offsets in lower and trimmed coincide.
	scope, args, scoped := "", "", false
	if m := chatScopePrefixRE.FindStringSubmatch(lower); m != nil {
		if s, ok := chatScopeAliases[m[1]]; ok {
			scope, scoped = s, true
			args = strings.TrimSpace(trimmed[strings.Index(trimmed, ":")+1:])
		}
	} else if strings.HasPrefix(lower, "/") {
		rest := strings.TrimLeft(trimmed[1:], " \t")
		fields := strings.Fields(strings.ToLower(rest))
		if len(fields) > 0 {
			if s, ok := chatScopeAliases[fields[0]]; ok {
				scope, scoped = s, true
				args = strings.TrimSpace(rest[len(fields[0]):])
			}
		}
	}

	tokens := chatIntentTokens(trimmed)
	if scoped {
		switch {
		case scope == "help":
			return chatRoute{Kind: chatRouteHelp}
		case scope == "jam" && (chatBareArgs(args) || chatPresenceIntent(chatIntentTokens(args))):
			return chatRoute{Kind: chatRoutePresence}
		case chatBareArgs(args):
			return chatRoute{Kind: chatRouteBare, Scope: scope}
		}
		return chatRoute{Kind: chatRouteGrounded, Scope: scope, Question: args}
	}

	// Unprefixed. Presence wording keeps its local answer.
	if chatPresenceIntent(tokens) {
		return chatRoute{Kind: chatRoutePresence}
	}

	// A one- or two-word keyword message ("AGENTS!!!", "prs?", "hive
	// status") is the bare command; anything longer is a real question.
	fields := strings.Fields(chatIntentSplitRE.ReplaceAllString(lower, " "))
	if len(fields) > 0 && len(fields) <= 2 {
		for _, f := range fields {
			if s, ok := chatScopeAliases[f]; ok {
				if s == "help" {
					return chatRoute{Kind: chatRouteHelp}
				}
				return chatRoute{Kind: chatRouteBare, Scope: s}
			}
		}
	}

	return chatRoute{Kind: chatRouteGrounded, Scope: chatGuessScope(tokens), Question: trimmed}
}

// chatGuessScope picks the snapshot scope for free text from its keywords;
// "" (everything) when the words point at more than one dataset or none.
func chatGuessScope(tokens map[string]bool) string {
	found := ""
	for _, ks := range chatKeywordScopes {
		for _, w := range ks.words {
			if tokens[w] {
				if found != "" && found != ks.scope {
					return ""
				}
				found = ks.scope
				break
			}
		}
	}
	return found
}

// chatScopeUsage is the deterministic reply for a scoped question grounding
// could not answer. It names the command, repeats the question so the user
// sees it was read, and never substitutes the bare dump.
func chatScopeUsage(scope, question, reason string) string {
	cmd := "/" + scope
	var b strings.Builder
	b.WriteString("I can't answer `" + cmd + " " + question + "` right now")
	if reason != "" {
		b.WriteString(" (" + reason + ")")
	}
	b.WriteString(".\n\n")
	switch scope {
	case "agents":
		b.WriteString("Bare `/agents` lists every agent with its state. ")
	case "prs":
		b.WriteString("Bare `/prs` gives open pull request counts per repository. ")
	case "beads":
		b.WriteString("Bare `/beads` gives bead counts per store. ")
	case "status":
		b.WriteString("Bare `/status` gives the hive summary. ")
	default:
		b.WriteString("Bare `" + cmd + "` gives the summary. ")
	}
	b.WriteString("Questions with arguments are answered from the live hive state when `chat.grounded_answers.enabled` is on and a governor reviewer endpoint is configured.")
	return b.String()
}
