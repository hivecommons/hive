package lanes

import (
	"crypto/subtle"
	"fmt"
	"strings"
)

const (
	minTokenBytes = 32
	maxTokenBytes = 4096
)

type Authenticator struct{ tokens map[string]string }

// Bind resolves administrator-selected environment bindings, not anything in
// an envelope or project header. Tokens must be distinct, including from the
// existing agent-control token.
func Bind(cfg Config, getenv func(string) string, adminToken string) (*Authenticator, error) {
	a := &Authenticator{tokens: map[string]string{}}
	for _, p := range cfg.Principals {
		token := getenv(p.TokenEnv)
		if len(token) < minTokenBytes || len(token) > maxTokenBytes || strings.ContainsAny(token, "\r\n\t ") || token == adminToken {
			return nil, fmt.Errorf("%w: missing, short or overlapping project credential", ErrInvalid)
		}
		for _, existing := range a.tokens {
			if existing == token {
				return nil, fmt.Errorf("%w: duplicate project credential", ErrInvalid)
			}
		}
		a.tokens[p.Project] = token
	}
	return a, nil
}

func (a *Authenticator) Project(token string) (string, bool) {
	if a == nil {
		return "", false
	}
	project := ""
	for p, expected := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			project = p
		}
	}
	return project, project != ""
}
