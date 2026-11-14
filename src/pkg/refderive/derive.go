// Package refderive turns an accepted assessment request issue into derived
// project.reference_repos entries (hivecommons/hive#18139).
//
// The motivating case is the CNCF TechDocs hive: its authorized repo is
// cncf/techdocs, and an assessment request issue there (cncf/techdocs#395)
// names the requesting project's repos in the issue-form fields "Website or
// documentation repository" and "Other repositories or sites in scope". Once
// a maintainer applies triage/accepted, the hive should be able to open and
// update issues and PRs in those repos without anyone editing config.
//
// This package is the pure half: it reads facts the caller has already
// fetched (the issue, who applied the grant label and with what permission)
// and the bound workflow model's intake declaration, and returns the entries
// with their provenance, or a typed reason for refusing. It never calls
// GitHub, never touches config files and never widens an entry past issues
// and PRs. Fetching the issue, checking the App installation on the
// requesting org and merging the entries into the running config are the
// later children of the RFC.
package refderive

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// GrantLabel is the label whose presence, applied by a user with write
// access to the authorized repo, turns a request issue into a grant.
const GrantLabel = "triage/accepted"

// DefaultHost is the forge host repository URLs are read from when Options
// names none. The hive is single-host, so a URL on any other host is text.
const DefaultHost = "github.com"

// DerivedWrite is the fixed tier of every derived entry: open issues and
// PRs, comment on the hive's own items. A derived entry can never be wider.
var DerivedWrite = []string{config.ReferenceWriteIssues, config.ReferenceWritePRs}

// writePermissions are the repository permission levels GitHub reports for
// a user who may push: the applier of GrantLabel must hold one of them.
var writePermissions = map[string]bool{"admin": true, "maintain": true, "write": true}

// noResponse is what a GitHub issue form renders for an empty optional field.
const noResponse = "_No response_"

// headingPrefix opens an issue-form field in the rendered body.
const headingPrefix = "### "

// Refusal reasons. A caller that gets one leaves the hive's config untouched
// and logs the reason; none of them is a bug.
var (
	// ErrNoIntakeFields is returned when the bound model declares no
	// intake.referenceRepoFields: the hive is not meant to derive anything.
	ErrNoIntakeFields = errors.New("workflow model declares no intake.referenceRepoFields")
	// ErrClosed is returned for a closed request: the grant ended with it.
	ErrClosed = errors.New("request issue is closed")
	// ErrNotAccepted is returned when GrantLabel is not on the issue.
	ErrNotAccepted = errors.New("request issue does not carry " + GrantLabel)
	// ErrAccepterUnknown is returned when the caller could not tell who
	// applied GrantLabel; an unattributed grant is no grant.
	ErrAccepterUnknown = errors.New("cannot tell who applied " + GrantLabel)
	// ErrAccepterNoWrite is returned when the applier lacks write access to
	// the authorized repo: anyone with triage can move labels, only a
	// maintainer may hand out access to other repos.
	ErrAccepterNoWrite = errors.New(GrantLabel + " was applied by a user without write access")
	// ErrAccepterIsBot is returned when a bot or App account applied the
	// label: the hive must not be able to grant itself access by moving a
	// label on an issue it works.
	ErrAccepterIsBot = errors.New(GrantLabel + " was applied by a bot account")
)

// RequestIssue is the request as the caller fetched it. Repo is the
// authorized repo holding the issue ("owner/name"); the Accepted* fields
// describe the latest "labeled" event for GrantLabel.
type RequestIssue struct {
	Repo   string
	Number int
	Title  string
	Body   string
	Closed bool
	Labels []string
	// AcceptedBy is the login that applied GrantLabel, empty when unknown.
	AcceptedBy string
	// AcceptedByPermission is that login's permission on Repo as GitHub
	// reports it: "admin", "maintain", "write", "triage", "read" or "none".
	AcceptedByPermission string
	// AcceptedByIsBot reports that AcceptedBy is a bot or GitHub App account.
	AcceptedByIsBot bool
}

// Ref is the issue's "owner/repo#N" provenance string, the value every
// derived entry carries in DerivedFrom.
func (r RequestIssue) Ref() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// Options tune a derivation.
type Options struct {
	// Host is the forge host URLs must be on to count as repositories;
	// empty means DefaultHost.
	Host string
	// Authorized lists the project's own repos as qualified "owner/name"
	// slugs. A repo that is already authorized can never also be a
	// reference repo, so it is skipped with a reason.
	Authorized []string
}

// Skipped records a repository the issue named that did not become an entry
// and why, so a kick or the Settings editor can explain the difference.
type Skipped struct {
	Repo   string
	Reason string
}

// Skip reasons.
const (
	ReasonOutOfScope = "named under an out-of-scope field"
	ReasonAuthorized = "already an authorized repo of this project"
	ReasonOwnRepo    = "is the repo holding the request issue"
)

// Result is a successful derivation: the entries to merge and what was
// left out. Repos is empty when the issue names nothing in scope.
type Result struct {
	Repos   []config.ReferenceRepo
	Skipped []Skipped
}

// FormField is one rendered issue-form field: the heading text and the
// value below it, trimmed, with GitHub's "_No response_" read as empty.
type FormField struct {
	Heading string
	Value   string
}

// ParseIssueForm splits a GitHub issue-form body into its fields. A form
// renders each field as a "### <label>" line followed by the value until
// the next heading; text before the first heading is dropped. Headings
// repeated in the body are kept as separate fields in order.
func ParseIssueForm(body string) []FormField {
	var (
		out     []FormField
		current *FormField
		value   []string
	)
	flush := func() {
		if current == nil {
			return
		}
		current.Value = strings.TrimSpace(strings.Join(value, "\n"))
		if current.Value == noResponse {
			current.Value = ""
		}
		out = append(out, *current)
		current, value = nil, nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if heading, ok := strings.CutPrefix(line, headingPrefix); ok {
			flush()
			current = &FormField{Heading: strings.TrimSpace(heading)}
			continue
		}
		if current != nil {
			value = append(value, line)
		}
	}
	flush()
	return out
}

// repoURL matches a repository URL on a host; group 1 is the owner, group 2
// the name (with any ".git" suffix or trailing punctuation still attached).
// The host is quoted into the pattern at call time.
const repoURLPattern = `(?i)\bhttps?://(?:www\.)?%s/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)`

// bareSlug matches a whole trimmed line or list item that is exactly
// "owner/name", optionally wrapped in backticks.
var bareSlug = regexp.MustCompile("^`?([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9._-]+)`?$")

// listMarkers are the characters a Markdown list item may start with.
const listMarkers = "-*+"

// trailingPunctuation is what a URL pasted into prose may drag along.
const trailingPunctuation = ".,;:)]>'\"`"

// gitSuffix is stripped from a cloned-URL style repository name.
const gitSuffix = ".git"

// ReposInText returns the "owner/name" repositories text names, in first
// appearance order, case-insensitively deduplicated. A repository is either
// a URL on host anywhere in the text, or a bare "owner/name" that is the
// whole line or list item: a path such as `src/docs/` inside a sentence is
// never a repository, and nothing on another host is.
func ReposInText(text, host string) []string {
	if strings.TrimSpace(host) == "" {
		host = DefaultHost
	}
	urlRe, err := regexp.Compile(fmt.Sprintf(repoURLPattern, regexp.QuoteMeta(strings.TrimSuffix(host, "/"))))
	if err != nil {
		return nil
	}
	var (
		out  []string
		seen = map[string]bool{}
	)
	add := func(owner, name string) {
		name = strings.TrimRight(name, trailingPunctuation)
		name = strings.TrimSuffix(name, gitSuffix)
		if owner == "" || name == "" {
			return
		}
		slug := owner + "/" + name
		key := strings.ToLower(slug)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, slug)
	}
	for _, line := range strings.Split(text, "\n") {
		item := strings.TrimSpace(line)
		if len(item) > 1 && strings.ContainsRune(listMarkers, rune(item[0])) && item[1] == ' ' {
			item = strings.TrimSpace(item[1:])
		}
		if m := bareSlug.FindStringSubmatch(item); m != nil && !strings.HasSuffix(m[2], ".") {
			add(m[1], m[2])
			continue
		}
		for _, m := range urlRe.FindAllStringSubmatch(line, -1) {
			add(m[1], m[2])
		}
	}
	return out
}

// HasWriteAccess reports whether a GitHub repository permission level lets
// its holder push: "admin", "maintain" or "write".
func HasWriteAccess(permission string) bool {
	return writePermissions[strings.ToLower(strings.TrimSpace(permission))]
}

// Accepted reports whether the issue carries GrantLabel (case-insensitive).
func (r RequestIssue) Accepted() bool {
	for _, l := range r.Labels {
		if strings.EqualFold(strings.TrimSpace(l), GrantLabel) {
			return true
		}
	}
	return false
}

// checkGrant is the gate every derivation passes before reading the body.
func checkGrant(issue RequestIssue, intake workflowmodel.Intake) error {
	ref := issue.Ref()
	switch {
	case !intake.DerivesReferenceRepos():
		return fmt.Errorf("%s: %w", ref, ErrNoIntakeFields)
	case issue.Closed:
		return fmt.Errorf("%s: %w", ref, ErrClosed)
	case !issue.Accepted():
		return fmt.Errorf("%s: %w", ref, ErrNotAccepted)
	case strings.TrimSpace(issue.AcceptedBy) == "":
		return fmt.Errorf("%s: %w", ref, ErrAccepterUnknown)
	case issue.AcceptedByIsBot:
		return fmt.Errorf("%s: %w (%s)", ref, ErrAccepterIsBot, issue.AcceptedBy)
	case !HasWriteAccess(issue.AcceptedByPermission):
		return fmt.Errorf("%s: %w (%s has %q)", ref, ErrAccepterNoWrite, issue.AcceptedBy, issue.AcceptedByPermission)
	}
	return nil
}

// headingIn reports whether heading matches one of the declared field
// labels, ignoring case and surrounding whitespace.
func headingIn(heading string, fields []string) bool {
	for _, f := range fields {
		if strings.EqualFold(strings.TrimSpace(f), strings.TrimSpace(heading)) {
			return true
		}
	}
	return false
}

// Derive turns an accepted request issue into reference-repo entries. It
// refuses with one of the Err* reasons when the issue is not a grant, and
// otherwise returns one write-tier entry per repository named under the
// model's referenceRepoFields that is not named under an outOfScopeFields
// heading, not the issue's own repo and not an authorized repo. The order
// is the order of first appearance in the body.
func Derive(issue RequestIssue, intake workflowmodel.Intake, opts Options) (Result, error) {
	if err := checkGrant(issue, intake); err != nil {
		return Result{}, err
	}
	host := opts.Host
	if strings.TrimSpace(host) == "" {
		host = DefaultHost
	}
	excluded := map[string]bool{strings.ToLower(strings.TrimSpace(issue.Repo)): true}
	authorized := make(map[string]bool, len(opts.Authorized))
	for _, a := range opts.Authorized {
		authorized[strings.ToLower(strings.TrimSpace(a))] = true
	}
	fields := ParseIssueForm(issue.Body)
	for _, f := range fields {
		if headingIn(f.Heading, intake.OutOfScopeFields) {
			for _, repo := range ReposInText(f.Value, host) {
				excluded[strings.ToLower(repo)] = true
			}
		}
	}
	var (
		res  Result
		seen = map[string]bool{}
		ref  = issue.Ref()
	)
	for _, f := range fields {
		if !headingIn(f.Heading, intake.ReferenceRepoFields) {
			continue
		}
		for _, repo := range ReposInText(f.Value, host) {
			key := strings.ToLower(repo)
			if seen[key] {
				continue
			}
			seen[key] = true
			switch {
			case key == strings.ToLower(strings.TrimSpace(issue.Repo)):
				res.Skipped = append(res.Skipped, Skipped{Repo: repo, Reason: ReasonOwnRepo})
			case excluded[key]:
				res.Skipped = append(res.Skipped, Skipped{Repo: repo, Reason: ReasonOutOfScope})
			case authorized[key]:
				res.Skipped = append(res.Skipped, Skipped{Repo: repo, Reason: ReasonAuthorized})
			default:
				res.Repos = append(res.Repos, config.ReferenceRepo{
					Repo:        repo,
					Write:       append([]string(nil), DerivedWrite...),
					DerivedFrom: ref,
				})
			}
		}
	}
	return res, nil
}
