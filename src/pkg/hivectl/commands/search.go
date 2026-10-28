package commands

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

// `hivectl search` (hivecommons/hive#14641, RFC #14491 Part A): the operator
// surface over the spoke's GET /api/search. Spoke-local only: it asks the
// hive named by --server and never fans out to a hub (RFC Part B).

const (
	searchAPIPath = "/api/search"
	// searchDisabledError is the error GET /api/search answers 404 with while
	// search.enabled is off.
	searchDisabledError = "search not enabled"
	searchDisabledHint  = "search is disabled on this hive: set `search.enabled: true` in the hive config (see docs/search.md)"
)

type searchCLIHighlight struct {
	Snippet string `json:"snippet"`
}

type searchCLIHit struct {
	Repo      string             `json:"repo"`
	Kind      string             `json:"kind"`
	At        time.Time          `json:"at"`
	URL       string             `json:"url"`
	Title     string             `json:"title"`
	Redacted  bool               `json:"redacted"`
	Highlight searchCLIHighlight `json:"highlight"`
}

type searchCLIResponse struct {
	Total    int               `json:"total"`
	Hits     []searchCLIHit    `json:"hits"`
	Coverage map[string]string `json:"coverage"`
}

func newSearchCommand(env *commandEnv) *cobra.Command {
	var (
		kinds, repos []string
		from, to     string
		limit        int
	)
	cmd := &cobra.Command{
		Use:   "search [query...]",
		Short: "Search this hive's portfolio index (evidence, findings, beads, facts)",
		Long: "Search the spoke's local portfolio index through GET /api/search. Owner or\n" +
			"merger role is required. Terms match whole words case-insensitively; an empty\n" +
			"query with --kind/--repo lists the newest matching documents. --from is inclusive,\n" +
			"--to exclusive (a YYYY-MM-DD --to covers that day). Only this hive is searched.\n\n" +
			"When the hive runs with search disabled (search.enabled unset or false) the command\n" +
			"says so and exits non-zero.",
		Args: wrapArgs(cobra.ArbitraryArgs),
		Example: "  hivectl search flaky test\n" +
			"  hivectl search --kind finding,sentinel --repo hivecommons/hive token\n" +
			"  hivectl search revert --from 2026-10-01 --to 2026-10-28 -o json",
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 0 {
				return &usageError{message: "--limit cannot be negative"}
			}
			query := url.Values{}
			if text := strings.TrimSpace(strings.Join(args, " ")); text != "" {
				query.Set("q", text)
			}
			if len(kinds) > 0 {
				query.Set("kind", strings.Join(kinds, ","))
			}
			if len(repos) > 0 {
				query.Set("repo", strings.Join(repos, ","))
			}
			if from = strings.TrimSpace(from); from != "" {
				query.Set("from", from)
			}
			if to = strings.TrimSpace(to); to != "" {
				query.Set("to", to)
			}
			if limit > 0 {
				query.Set("limit", strconv.Itoa(limit))
			}
			client, err := env.client()
			if err != nil {
				return err
			}
			result, err := client.Do(cmd.Context(), http.MethodGet, searchAPIPath, query, nil)
			if err != nil {
				return searchAPIError(err)
			}
			if env.options.output != "table" {
				return env.print(cmd, result)
			}
			return printSearchTable(cmd, result)
		},
	}
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "document kinds: evidence, finding, sentinel, complexity, bead, fact")
	cmd.Flags().StringSliceVar(&repos, "repo", nil, "owner/repo to search (repeatable)")
	cmd.Flags().StringVar(&from, "from", "", "inclusive lower bound, RFC 3339 or YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "exclusive upper bound, RFC 3339 or YYYY-MM-DD")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum hits, 1–100; zero uses the server default")
	return cmd
}

// searchAPIError keeps the API error and its exit code but replaces the bare
// 404 GET /api/search answers while search is off with what to do about it.
func searchAPIError(err error) error {
	var apiErr *hivectl.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Message != searchDisabledError {
		return err
	}
	return &hivectl.APIError{StatusCode: apiErr.StatusCode, Message: searchDisabledHint, Body: apiErr.Body}
}

func printSearchTable(cmd *cobra.Command, result any) error {
	var resp searchCLIResponse
	if err := decodeResult(result, &resp, "search"); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "KIND\tREPO\tAT\tTITLE\tURL")
	for _, hit := range resp.Hits {
		title := hit.Title
		if title == "" {
			title = hit.Highlight.Snippet
		}
		if hit.Redacted {
			title += " [redacted]"
		}
		at := ""
		if !hit.At.IsZero() {
			at = hit.At.UTC().Format(time.DateOnly)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", compactString(hit.Kind), compactString(hit.Repo), at, compactString(title), compactString(hit.URL))
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "%d of %d hits\n", len(resp.Hits), resp.Total)
	repos := make([]string, 0, len(resp.Coverage))
	for repo := range resp.Coverage {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s index unreadable (%s); its hits may be missing\n", compactString(repo), compactString(resp.Coverage[repo]))
	}
	return nil
}
