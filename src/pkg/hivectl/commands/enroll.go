package commands

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

type enrollOptions struct {
	acmmLevel      int
	installationID int64
	githubHost     string
}

type enrollHubClient interface {
	DoWithHeaders(ctx context.Context, method, apiPath string, query url.Values, body any, headers http.Header) (any, error)
}

type ghVerifier interface {
	Verify(ctx context.Context, host, repo string) (string, error)
}

type ghCLI struct{}

func newEnrollCommand(env *commandEnv) *cobra.Command {
	opts := &enrollOptions{acmmLevel: 2, githubHost: "github.com"}
	cmd := &cobra.Command{
		Use:   "enroll <owner/repo>",
		Short: "Enroll a repo into a Hive lite spoke",
		Example: `  hivectl enroll kubestellar/hive
  hivectl enroll kubestellar/hive --acmm-level 1
  hivectl enroll kubestellar/hive --installation-id 123456`,
		Args: argsExact(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runEnrollCommand(cmd, args[0], opts, ghCLI{})
		},
	}
	cmd.Flags().IntVar(&opts.acmmLevel, "acmm-level", 2, "requested ACMM level for lite mode (capped at L2)")
	cmd.Flags().Int64Var(&opts.installationID, "installation-id", 0, "GitHub App installation ID (optional when the hub can auto-discover it)")
	cmd.Flags().StringVar(&opts.githubHost, "github-host", "github.com", "GitHub hostname")
	return cmd
}

func (e *commandEnv) runEnrollCommand(cmd *cobra.Command, repo string, opts *enrollOptions, gh ghVerifier) error {
	if !validOutput(e.options.output) {
		return &usageError{message: fmt.Sprintf("invalid --output %q; expected table, json, yaml, or jsonl", e.options.output)}
	}
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.Contains(parts[1], "/") {
		return &usageError{message: "repo must be in owner/repo form"}
	}
	if opts.acmmLevel < 1 {
		return &usageError{message: "--acmm-level must be at least 1"}
	}
	capped := opts.acmmLevel
	if capped > 2 {
		capped = 2
	}

	repoToken, err := gh.Verify(cmd.Context(), opts.githubHost, repo)
	if err != nil {
		return err
	}
	hubToken := ""
	if e.options.tokenEnv != "" {
		hubToken = os.Getenv(e.options.tokenEnv)
	}
	if hubToken == "" {
		if !strings.EqualFold(opts.githubHost, "github.com") {
			return &usageError{message: "set HIVE_DASHBOARD_TOKEN (or --token-env) for non-github.com enrollment; the GitHub Enterprise token is used only to verify repo access"}
		}
		hubToken = repoToken
	}
	client, err := hivectl.NewClient(e.options.server, hubToken, e.options.timeout)
	if err != nil {
		return &usageError{message: err.Error()}
	}
	resp, err := runEnroll(cmd.Context(), client, liteEnrollRequest{
		Owner:          parts[0],
		Repo:           parts[1],
		InstallationID: opts.installationID,
		ACMMLevel:      capped,
		GitHubHost:     opts.githubHost,
	}, repoToken)
	if err != nil {
		return err
	}
	if e.options.output != "table" {
		return e.print(cmd, resp)
	}
	printEnrollSummary(cmd, repo, capped, opts.acmmLevel, resp)
	return nil
}

type liteEnrollRequest struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	InstallationID int64  `json:"installation_id,omitempty"`
	ACMMLevel      int    `json:"acmm_level,omitempty"`
	GitHubHost     string `json:"github_host,omitempty"`
}

func runEnroll(ctx context.Context, client enrollHubClient, req liteEnrollRequest, repoToken string) (map[string]any, error) {
	headers := http.Header{}
	if repoToken != "" {
		headers.Set("X-Hive-GitHub-Token", repoToken)
	}
	result, err := client.DoWithHeaders(ctx, http.MethodPost, "/api/saas/lite/enroll", nil, req, headers)
	if err != nil {
		return nil, err
	}
	if m, ok := result.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{"result": result}, nil
}

func printEnrollSummary(cmd *cobra.Command, repo string, acmm, requested int, resp map[string]any) {
	out := cmd.OutOrStdout()
	id, _ := resp["spoke_id"].(string)
	if id == "" {
		id, _ = resp["id"].(string)
	}
	action, _ := resp["action"].(string)
	dashboard, _ := resp["dashboard_url"].(string)
	// Best-effort CLI output: a write failure means the user's terminal/pipe is
	// already gone, and there is no one left to report it to.
	_, _ = fmt.Fprintf(out, "✓ Enrolled %s in Hive lite spoke %s\n", repo, id)
	switch action {
	case "spoke_config_update":
		_, _ = fmt.Fprintln(out, "  repo will be added to the spoke config on its next heartbeat")
	case "hosted_lite_spoke_requested":
		_, _ = fmt.Fprintln(out, "  hosted lite spoke provisioning requested")
	}
	if requested > acmm {
		_, _ = fmt.Fprintf(out, "  requested ACMM L%d capped to L%d for lite mode\n", requested, acmm)
	}
	if installation := numberString(resp["installation_id"]); installation != "" {
		_, _ = fmt.Fprintf(out, "  GitHub App installation: %s\n", installation)
	}
	if dashboard != "" {
		_, _ = fmt.Fprintf(out, "  Dashboard: %s\n", dashboard)
	}
	_, _ = fmt.Fprintln(out, "\nStarter config: advisory-only lanes at ACMM L1-L2; repo lives in the spoke hive.yaml; no repository PAT or long-lived secret is written.")
	_, _ = fmt.Fprintln(out, "\nNext steps:")
	_, _ = fmt.Fprintln(out, "  1. Wait for the spoke heartbeat/provisioning to apply the repo config.")
	_, _ = fmt.Fprintln(out, "  2. Raise ACMM from L1 to L2 only after advisory noise is acceptable.")
	_, _ = fmt.Fprintln(out, "  3. Graduate to a full spoke for execution, private runtime, or ACMM L3+.")
}

func numberString(v any) string {
	switch n := v.(type) {
	case float64:
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(int64(n), 10)
	case int64:
		if n == 0 {
			return ""
		}
		return strconv.FormatInt(n, 10)
	case string:
		return n
	default:
		return ""
	}
}

func (ghCLI) Verify(ctx context.Context, host, repo string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = "github.com"
	}
	if _, err := runGH(ctx, "auth", "status", "--hostname", host); err != nil {
		return "", fmt.Errorf("gh auth check failed for %s: %w", host, err)
	}
	target := repo
	if !strings.EqualFold(host, "github.com") {
		target = "https://" + strings.TrimRight(host, "/") + "/" + repo
	}
	if _, err := runGH(ctx, "repo", "view", target, "--json", "nameWithOwner", "--jq", ".nameWithOwner"); err != nil {
		return "", fmt.Errorf("cannot access GitHub repo %s with gh: %w", repo, err)
	}
	token, err := runGH(ctx, "auth", "token", "--hostname", host)
	if err != nil {
		return "", fmt.Errorf("gh auth token unavailable: %w", err)
	}
	return strings.TrimSpace(token), nil
}

var runGH = func(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = withoutGitHubToken(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func withoutGitHubToken(env []string) []string {
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "GH_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
