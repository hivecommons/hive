package mention

import (
	"regexp"
	"strings"
)

type Parsed struct {
	Mentioned bool
	Agent     string
	Text      string
}

var askSelectorRe = regexp.MustCompile(`(?is)^ask\s+([A-Za-z0-9_.-]+)\b\s*(.*)$`)

func Parse(body, appBotLogin string) Parsed {
	login := strings.TrimSpace(appBotLogin)
	if login == "" {
		return Parsed{}
	}
	lowerBody := strings.ToLower(body)
	needle := "@" + strings.ToLower(strings.TrimPrefix(login, "@"))
	idx := strings.Index(lowerBody, needle)
	if idx < 0 {
		return Parsed{}
	}
	end := idx + len(needle)
	if end < len(body) {
		next := body[end]
		if (next >= 'A' && next <= 'Z') || (next >= 'a' && next <= 'z') || (next >= '0' && next <= '9') || next == '-' || next == '_' || next == '[' || next == ']' {
			return Parsed{}
		}
	}
	rest := strings.TrimSpace(body[end:])
	p := Parsed{Mentioned: true, Text: rest}
	if m := askSelectorRe.FindStringSubmatch(rest); len(m) == 3 {
		p.Agent = strings.TrimSpace(m[1])
		p.Text = strings.TrimSpace(m[2])
	}
	return p
}
