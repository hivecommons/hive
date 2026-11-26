package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/forge"
)

// errForgeTokenEmpty is wrapped by requireForgeToken's refusal.
var errForgeTokenEmpty = errors.New("forge token is empty")

// requireForgeToken refuses to build a Forgejo REST client with no token.
// Forgejo answers an anonymous request for a private repo with 404, not 401,
// so a client built with an empty token reports "The target couldn't be
// found" for a repo that exists, and never names the missing token
// (hivecommons/hive#19919). source names where the token was read from, for
// the error. Other kinds keep passing an empty token through: GitLab
// answers it with a 401 that names the problem.
func requireForgeToken(kind forge.Kind, source, token string) error {
	if kind != forge.KindForgejo || strings.TrimSpace(token) != "" {
		return nil
	}
	return fmt.Errorf("forge %s: %w: %s is unset or empty, and Forgejo answers a request with no token for a private repo with 404", kind, errForgeTokenEmpty, source)
}
