package commands

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/spf13/cobra"
)

type knowledgeSuggestOptions struct {
	action      string
	title       string
	body        string
	bodyFile    string
	stdin       bool
	factType    string
	status      string
	repo        string
	layer       string
	tags        []string
	source      string
	reason      string
	target      string
	related     []string
	suggestedBy string
	repoRoot    string
	dir         string
	prBodyFile  string
}

// knowledgeSuggestCommand writes an agent-suggested knowledge change into a
// repository checkout's carried knowledge directory and prepares the pull
// request that carries it. It runs locally and never calls the dashboard: the
// suggestion only becomes approved knowledge when a human merges that PR.
func knowledgeSuggestCommand(env *commandEnv) *cobra.Command {
	opts := &knowledgeSuggestOptions{}
	command := &cobra.Command{
		Use:   "suggest",
		Short: "Suggest a knowledge change as a reviewable repository PR",
		Long: "Writes a proposed knowledge change into the checkout's " + knowledge.DefaultSuggestionDir + " directory\n" +
			"and prints the branch, PR title and PR body to open a pull request with. Nothing reaches\n" +
			"agents until a human merges that PR; closing the PR rejects the suggestion.\n\n" +
			"Actions: add (new entry), update (rewrite --target in place), replace (new entry that\n" +
			"supersedes --target) and deprecate (mark --target out of date). --source and --reason are\n" +
			"always required. Existing entries with a similar title are recorded as dedupe hints.",
		Args: argsNone(),
		Example: "  hivectl knowledge suggest --title \"Retry relay uploads\" --body-file note.md \\\n" +
			"    --source https://github.com/acme/app/pull/12 --reason \"learned while fixing #12\" --tags relay,ops\n" +
			"  hivectl knowledge suggest --action replace --target old-relay --title \"Relay v2\" --stdin \\\n" +
			"    --source acme/app#40 --reason \"relay rewritten\" --pr-body-file pr.md\n" +
			"  hivectl knowledge suggest --action deprecate --target old-relay --source acme/app#41 --reason \"relay removed\"",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return env.runKnowledgeSuggest(cmd, opts)
		},
	}
	flags := command.Flags()
	flags.StringVar(&opts.action, "action", string(knowledge.SuggestAdd), "add, update, replace or deprecate")
	flags.StringVar(&opts.title, "title", "", "entry title (required except for deprecate)")
	flags.StringVar(&opts.body, "body", "", "entry body markdown")
	flags.StringVar(&opts.bodyFile, "body-file", "", "read the entry body from a file")
	flags.BoolVar(&opts.stdin, "stdin", false, "read the entry body from stdin")
	flags.StringVar(&opts.factType, "type", "", "fact type, e.g. gotcha, decision, pattern")
	flags.StringVar(&opts.status, "status", "", "proposed lifecycle status once merged: draft, approved or deprecated (default approved; deprecated for deprecate)")
	flags.StringVar(&opts.repo, "repo", "", "affected repository (owner/repo)")
	flags.StringVar(&opts.layer, "layer", "", "affected knowledge layer: personal, project, org or community")
	flags.StringSliceVar(&opts.tags, "tags", nil, "comma-separated tags")
	flags.StringVar(&opts.source, "source", "", "citation: PR/issue URL or owner/repo#N (required)")
	flags.StringVar(&opts.reason, "reason", "", "why the change is needed (required)")
	flags.StringVar(&opts.target, "target", "", "existing entry to update, replace or deprecate (path under the knowledge dir, without .md)")
	flags.StringSliceVar(&opts.related, "related", nil, "related or possibly duplicate entries to flag for the reviewer")
	flags.StringVar(&opts.suggestedBy, "suggested-by", "", "who is suggesting the change, recorded as provenance")
	flags.StringVar(&opts.repoRoot, "repo-root", ".", "repository checkout to write the suggestion into")
	flags.StringVar(&opts.dir, "dir", knowledge.DefaultSuggestionDir, "knowledge directory inside the repository")
	flags.StringVar(&opts.prBodyFile, "pr-body-file", "", "also write the prepared PR body to this file")
	return command
}

func (e *commandEnv) runKnowledgeSuggest(cmd *cobra.Command, opts *knowledgeSuggestOptions) error {
	sources := 0
	for _, set := range []bool{opts.body != "", opts.bodyFile != "", opts.stdin} {
		if set {
			sources++
		}
	}
	if sources > 1 {
		return &usageError{message: "use only one of --body, --body-file, or --stdin"}
	}
	body := opts.body
	switch {
	case opts.bodyFile != "":
		data, err := os.ReadFile(opts.bodyFile)
		if err != nil {
			return err
		}
		body = string(data)
	case opts.stdin:
		data, err := io.ReadAll(e.in)
		if err != nil {
			return err
		}
		body = string(data)
	}
	action, err := knowledge.ParseSuggestionAction(opts.action)
	if err != nil {
		return &usageError{message: err.Error()}
	}
	suggestion := knowledge.Suggestion{
		Action:      action,
		Title:       opts.title,
		Body:        body,
		Type:        knowledge.FactType(opts.factType),
		State:       knowledge.LifecycleState(opts.status),
		Repo:        opts.repo,
		Layer:       knowledge.LayerType(opts.layer),
		Tags:        opts.tags,
		Source:      opts.source,
		Reason:      opts.reason,
		Target:      opts.target,
		Related:     opts.related,
		SuggestedBy: opts.suggestedBy,
		SuggestedAt: time.Now().UTC(),
	}
	suggestion.Normalize()
	if err := suggestion.Validate(); err != nil {
		return &usageError{message: err.Error()}
	}
	result, err := knowledge.WriteSuggestion(opts.repoRoot, opts.dir, suggestion)
	if err != nil {
		return err
	}
	if path := strings.TrimSpace(opts.prBodyFile); path != "" {
		if err := os.WriteFile(path, []byte(result.PRBody), 0o644); err != nil {
			return err
		}
	}
	return e.print(cmd, result)
}
