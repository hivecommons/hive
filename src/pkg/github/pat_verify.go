package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// PATVerification is what VerifyPAT learned about a candidate personal access
// token before the hive adopts it (hivecommons/hive#19633). It never carries
// the token itself.
type PATVerification struct {
	// Login is the GitHub account the token authenticates as.
	Login string
	// Scopes is the LogTokenScopeCheck outcome for the hive's ACMM level.
	Scopes ScopeResult
}

// VerifyPAT checks a candidate PAT against the configured GitHub API the way
// the hive will use it, so a rotation can refuse a token that would leave the
// hive quietly broken (401s and an empty backlog behind a healthy
// /api/health). In order:
//
//  1. GET /user must succeed and name a login.
//  2. Every repository in repos ("owner/name", or a bare name under org) must
//     be readable with the token.
//  3. The boot-time scope check (LogTokenScopeCheck) for acmmLevel must not
//     report a missing scope. An undetermined result (a fine-grained PAT, or
//     an endpoint without scope headers) is accepted, as it is at boot.
//
// The first failure is returned; error messages name the step and the repo,
// never token material.
func VerifyPAT(ctx context.Context, token, apiURL, org string, repos []string, acmmLevel int, logger *slog.Logger) (*PATVerification, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("token is empty")
	}
	user, err := ValidateTokenWithContext(ctx, token, strings.TrimRight(apiURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("GitHub rejected the token at GET /user: %w", err)
	}
	if user == nil || strings.TrimSpace(user.Login) == "" {
		return nil, errors.New("GitHub did not report the account the token belongs to")
	}
	client := NewClient(token, org, repos, logger, apiURL)
	for _, repo := range repos {
		owner, name := splitRepoRef(repo, org)
		if owner == "" || name == "" {
			continue
		}
		if _, _, err := client.GetRepo(WithRESTCaller(ctx, "hive:verify_pat"), owner, name); err != nil {
			return nil, fmt.Errorf("the token cannot read %s/%s: %w", owner, name, err)
		}
	}
	res := client.LogTokenScopeCheck(ctx, logger, acmmLevel)
	if res.Status == ScopeStatusMissing {
		return nil, fmt.Errorf("the token is missing scopes this hive needs: %s", res.Detail)
	}
	return &PATVerification{Login: user.Login, Scopes: res}, nil
}
