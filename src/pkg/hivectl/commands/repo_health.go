package commands

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl repo health` (hivecommons/hive#14641): the report-only per-repo
// health score from GET /api/repos/{owner}/{repo}/health (#14638), printed
// with every component's weight, n and reason so the score can be disputed.

// repoHealthUnavailable is the 503 the health endpoint answers while the
// search indexer is not wired (search.enabled off).
const repoHealthUnavailable = "repo health unavailable: search is not enabled"

type repoHealthCLIComponent struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Score        *float64 `json:"score"`
	Weight       float64  `json:"weight"`
	Contribution *float64 `json:"contribution"`
	N            int      `json:"n"`
	Reason       string   `json:"reason"`
}

type repoHealthCLIResponse struct {
	Repo       string                   `json:"repo"`
	Score      *float64                 `json:"score"`
	Status     string                   `json:"status"`
	Components []repoHealthCLIComponent `json:"components"`
	WindowDays int                      `json:"window_days"`
	MinN       int                      `json:"min_n"`
	ComputedAt time.Time                `json:"computed_at"`
}

func newRepoCommand(env *commandEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repo",
		Short:   "Per-repository reports from this hive",
		Example: "  hivectl repo health hivecommons/hive",
	}
	cmd.AddCommand(newRepoHealthCommand(env))
	return cmd
}

func newRepoHealthCommand(env *commandEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "health owner/repo",
		Short: "Show a repository's report-only health score and its components",
		Long: "Show the health score this hive computed for a repository at its last search index\n" +
			"rebuild (GET /api/repos/{owner}/{repo}/health). Each component lists its weight, sample\n" +
			"size and contribution; a component without enough local data is not_reported, never\n" +
			"zero. The score is report-only: nothing that decides a merge or Governor priority reads it.\n\n" +
			"Requires search.enabled on the hive; otherwise the command says so and exits non-zero.",
		Args: argsExact(1),
		Example: "  hivectl repo health hivecommons/hive\n" +
			"  hivectl repo health hivecommons/hive -o json",
		RunE: func(cmd *cobra.Command, args []string) error {
			owner, name, ok := strings.Cut(strings.Trim(strings.TrimSpace(args[0]), "/"), "/")
			if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return &usageError{message: fmt.Sprintf("expected owner/repo, got %q", args[0])}
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			path := "/api/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/health"
			result, err := client.Do(cmd.Context(), http.MethodGet, path, nil, nil)
			if err != nil {
				return repoHealthAPIError(err)
			}
			if env.options.output != "table" {
				return env.print(cmd, result)
			}
			return printRepoHealthTable(cmd, result)
		},
	}
}

// repoHealthAPIError keeps the API error and its exit code but replaces the
// 503 the endpoint answers while search is off with what to do about it.
func repoHealthAPIError(err error) error {
	var apiErr *hivectl.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Message != repoHealthUnavailable {
		return err
	}
	return &hivectl.APIError{StatusCode: apiErr.StatusCode, Message: searchDisabledHint, Body: apiErr.Body}
}

func healthNumber(v *float64) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

func printRepoHealthTable(cmd *cobra.Command, result any) error {
	var h repoHealthCLIResponse
	if err := decodeResult(result, &h, "health"); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	computed := ""
	if !h.ComputedAt.IsZero() {
		computed = h.ComputedAt.UTC().Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(out, "%s  score %s (%s)  window %dd  min n %d  computed %s\n",
		compactString(h.Repo), healthNumber(h.Score), compactString(h.Status), h.WindowDays, h.MinN, computed)
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "COMPONENT\tSTATUS\tSCORE\tWEIGHT\tCONTRIBUTION\tN\tREASON")
	for _, c := range h.Components {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", compactString(c.Name), compactString(c.Status), healthNumber(c.Score),
			strconv.FormatFloat(c.Weight, 'f', -1, 64), healthNumber(c.Contribution), c.N, compactString(c.Reason))
	}
	return writer.Flush()
}
