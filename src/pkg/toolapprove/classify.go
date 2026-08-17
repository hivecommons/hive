package toolapprove

import (
	"strings"
)

// IsReadOnly reports whether tool is an inspection / read-only tool.
func IsReadOnly(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	switch t {
	case "read", "glob", "grep", "read_file", "view_file", "list_dir",
		"grep_search", "glob_search", "read_browser_page", "read_url_content",
		"websearch", "web_search", "fetchurl", "fetch_url":
		return true
	}

	// GitHub read tools (Claude MCP & Copilot syntax)
	if strings.HasPrefix(t, "mcp__github__get_") ||
		strings.HasPrefix(t, "mcp__github__list_") ||
		strings.HasPrefix(t, "mcp__github__search_") ||
		strings.HasPrefix(t, "mcp__github__pull_request_read") ||
		strings.HasPrefix(t, "mcp__github__issue_read") ||
		strings.HasPrefix(t, "mcp__github__repository_read") ||
		strings.HasPrefix(t, "mcp__github__branch_read") {
		return true
	}

	if strings.HasPrefix(t, "github-mcp-server(get_") ||
		strings.HasPrefix(t, "github-mcp-server(list_") ||
		strings.HasPrefix(t, "github-mcp-server(search_") ||
		strings.HasPrefix(t, "github-mcp-server(read_") {
		return true
	}

	return false
}

// IsDirectPRCreation reports whether tool performs direct PR creation without hive-open-pr.
func IsDirectPRCreation(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	if t == "mcp__github__create_pull_request" ||
		t == "mcp__github__create_pull_request_with_copilot" {
		return true
	}
	if strings.HasPrefix(t, "github-mcp-server(create_pull_request") {
		return true
	}
	return false
}

// IsDirectPRMerge reports whether tool performs direct PR merge without hive-merge.
func IsDirectPRMerge(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	if t == "mcp__github__merge_pull_request" {
		return true
	}
	if strings.HasPrefix(t, "github-mcp-server(merge_pull_request") {
		return true
	}
	return false
}

// IsGitHubWrite reports whether tool writes to GitHub (issues, PRs, comments, labels).
func IsGitHubWrite(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	if IsDirectPRCreation(tool) || IsDirectPRMerge(tool) {
		return true
	}
	if strings.HasPrefix(t, "mcp__github__create_") ||
		strings.HasPrefix(t, "mcp__github__update_") ||
		strings.HasPrefix(t, "mcp__github__add_") ||
		strings.HasPrefix(t, "mcp__github__delete_") {
		return true
	}
	if strings.HasPrefix(t, "github-mcp-server(create_") ||
		strings.HasPrefix(t, "github-mcp-server(update_") ||
		strings.HasPrefix(t, "github-mcp-server(add_") ||
		strings.HasPrefix(t, "github-mcp-server(delete_") {
		return true
	}
	return false
}

// IsGitHubIssueOperation reports whether tool interacts with issues/comments/labels.
func IsGitHubIssueOperation(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	if strings.Contains(t, "issue") || strings.Contains(t, "comment") || strings.Contains(t, "label") {
		return IsGitHubWrite(tool)
	}
	return false
}

// IsGitHubPROperation reports whether tool interacts with PRs or merge operations.
func IsGitHubPROperation(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	if strings.Contains(t, "pull_request") || strings.Contains(t, "merge") {
		return IsGitHubWrite(tool)
	}
	return false
}

// IsLocalWrite reports whether tool modifies local files.
func IsLocalWrite(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	switch t {
	case "write", "edit", "write_to_file", "replace_file_content", "edit_file", "notebookeditcell":
		return true
	}
	return false
}

// IsBash reports whether tool executes shell / system commands.
func IsBash(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	switch t {
	case "bash", "run_command", "exec", "terminal":
		return true
	}
	return false
}

// IsSubagent reports whether tool invokes or delegates to subagents.
func IsSubagent(tool string) bool {
	t := strings.ToLower(strings.TrimSpace(tool))
	switch t {
	case "agent", "invoke_subagent", "define_subagent", "subagent_sync":
		return true
	}
	return false
}

// IsSideEffectful reports whether tool has external side effects (writes, execution, mutation).
func IsSideEffectful(tool string) bool {
	return !IsReadOnly(tool)
}

// IsReadOnly reports whether this tool request is read-only.
func (r ToolRequest) IsReadOnly() bool {
	return IsReadOnly(r.Tool)
}

// IsSideEffectful reports whether this tool request is side-effectful.
func (r ToolRequest) IsSideEffectful() bool {
	return IsSideEffectful(r.Tool)
}
