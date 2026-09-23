package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl claim` / `hivectl unclaim` — issue claims (hivecommons/hive#8380).
//
// A claim says "I am working this issue" to the hive: relay contributors stop
// being offered it, hub agents are not kicked on it, and a lower-ranked holder
// (a clanker) is told to stop and move on. Ranks: human > agent > contributor
// > external. The caller of these commands is always a human, so `claim` takes
// over anything a clanker or an agent holds, warns on another human's claim
// unless --force, and `claim` again renews. `takeover` is an alias for
// `claim --force`. The same grammar is planned for issue comments (`/claim`,
// `/unclaim`) in v6.

const claimsAPIPrefix = "/api/claims"

// claimRefTarget parses `owner/repo#N` (or `owner/repo N`) into the API path.
func claimRefTarget(args []string) (string, error) {
	ref := strings.TrimSpace(strings.Join(args, "#"))
	ref = strings.TrimPrefix(ref, "https://github.com/")
	ref = strings.Replace(ref, "/issues/", "#", 1)
	repo, num, ok := strings.Cut(ref, "#")
	if !ok {
		return "", &usageError{message: fmt.Sprintf("expected owner/repo#N, got %q", ref)}
	}
	owner, name, ok := strings.Cut(strings.Trim(repo, "/"), "/")
	n, convErr := strconv.Atoi(strings.TrimSpace(num))
	if !ok || owner == "" || name == "" || convErr != nil || n <= 0 {
		return "", &usageError{message: fmt.Sprintf("expected owner/repo#N, got %q", ref)}
	}
	return fmt.Sprintf("%s/%s/%s/%d", claimsAPIPrefix, owner, name, n), nil
}

func newClaimCommand(env *commandEnv) *cobra.Command {
	var (
		force   bool
		until   time.Duration
		session string
	)
	cmd := &cobra.Command{
		Use:     "claim owner/repo#N",
		Aliases: []string{"takeover"},
		Short:   "Claim an issue so contributors and agents back off it (re-run to renew)",
		Long: "Claim an issue for yourself. Anything a relay contributor or a hive agent holds\n" +
			"is taken over and the holder is told to stop; another person's claim warns unless\n" +
			"--force. Run it again to renew before the claim lapses. `takeover` = `claim --force`.",
		Args: wrapArgs(cobra.RangeArgs(1, 2)),
		Example: "  hivectl claim hivecommons/hive#8380\n" +
			"  hivectl claim hivecommons/hive#8380 --until 6h\n" +
			"  hivectl takeover hivecommons/hive#8380",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := claimRefTarget(args)
			if err != nil {
				return err
			}
			if cmd.CalledAs() == "takeover" {
				force = true
			}
			body := map[string]any{"force": force}
			if until > 0 {
				body["ttl_s"] = int(until / time.Second)
			}
			if session != "" {
				body["session"] = session
			}
			return env.doClaim(cmd, http.MethodPost, path, body)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "take over another person's claim at the same rank")
	cmd.Flags().DurationVar(&until, "until", 0, "how long to hold the claim (default: hive policy, e.g. 4h)")
	cmd.Flags().StringVar(&session, "session", "", "session label so two of your own sessions do not renew each other")
	return cmd
}

func newUnclaimCommand(env *commandEnv) *cobra.Command {
	var (
		force  bool
		reason string
	)
	cmd := &cobra.Command{
		Use:     "unclaim owner/repo#N",
		Aliases: []string{"release"},
		Short:   "Release a claim you hold (or one held by a lower rank)",
		Args:    wrapArgs(cobra.RangeArgs(1, 2)),
		Example: "  hivectl unclaim hivecommons/hive#8380\n  hivectl unclaim hivecommons/hive#8380 --force   # owner override",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := claimRefTarget(args)
			if err != nil {
				return err
			}
			body := map[string]any{"force": force}
			if reason != "" {
				body["reason"] = reason
			}
			return env.doClaim(cmd, http.MethodDelete, path, body)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "owner override: release regardless of who holds it")
	cmd.Flags().StringVar(&reason, "reason", "", "reason recorded with the release")
	return cmd
}

func newClaimsCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:     "claims",
		Short:   "List live issue claims on this hive",
		Args:    argsNone(),
		Example: "  hivectl claims -o json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.do(cmd, http.MethodGet, claimsAPIPrefix, nil)
		},
	}
}

// doClaim is env.do plus the 409 case: the hive answers "held"/"refused" with
// a structured body (outcome, holder, hint) that is more useful printed than
// folded into a generic API error, and the exit code stays non-zero.
func (e *commandEnv) doClaim(cmd *cobra.Command, method, path string, body any) error {
	client, err := e.client()
	if err != nil {
		return err
	}
	result, err := client.Do(cmd.Context(), method, path, nil, body)
	if err != nil {
		var apiErr *hivectl.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			var payload map[string]any
			if json.Unmarshal(apiErr.Body, &payload) == nil {
				_ = e.print(cmd, payload)
				if hint, _ := payload["hint"].(string); hint != "" {
					return &hivectl.APIError{StatusCode: apiErr.StatusCode, Message: hint, Body: apiErr.Body}
				}
			}
		}
		return err
	}
	return e.print(cmd, result)
}
