// Package search is the core of the portfolio search index (#14491): a
// document model, the pluggable Backend interface and the default embedded,
// dependency-free inverted-index backend. It mirrors pkg/codeindex: tokenised
// posting lists, immutable snapshots published atomically, and per-repo
// coverage errors instead of silently partial results. Indexer feeds it from
// the hive's local stores when search.enabled is set; APIs and UI build on
// this package separately.
package search

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Visibility values. Private documents must not leave the spoke unless the
// operator opts the repo into federation.
const (
	Public  = "public"
	Private = "private"
)

// Built-in facet fields. Document.Fields may not reuse these names.
const (
	FieldKind       = "kind"
	FieldRepo       = "repo"
	FieldVisibility = "visibility"
)

// Document is one indexed record. ID is unique within Repo. Repo, Kind, At,
// URL and Visibility are mandatory on every document. Title and Text are
// tokenised for full-text search; Fields holds keyword values (for example
// "verdict" or "sentinel.rule") that are tokenised, filterable and facetable.
type Document struct {
	ID         string            `json:"id"`
	Repo       string            `json:"repo"`
	Kind       string            `json:"kind"`
	At         time.Time         `json:"at"`
	URL        string            `json:"url"`
	Visibility string            `json:"visibility"`
	Title      string            `json:"title,omitempty"`
	Text       string            `json:"text,omitempty"`
	Fields     map[string]string `json:"fields,omitempty"`
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// validRepo accepts GitHub owner/name pairs that are safe as path components.
func validRepo(repo string) bool {
	if !repoRE.MatchString(repo) {
		return false
	}
	owner, name, _ := strings.Cut(repo, "/")
	return owner != "." && owner != ".." && name != "." && name != ".."
}

func (d Document) validate() error {
	switch {
	case !validRepo(d.Repo):
		return fmt.Errorf("search: invalid repo %q", d.Repo)
	case d.ID == "":
		return fmt.Errorf("search: %s: document id is required", d.Repo)
	case d.Kind == "":
		return fmt.Errorf("search: %s/%s: kind is required", d.Repo, d.ID)
	case d.At.IsZero():
		return fmt.Errorf("search: %s/%s: at is required", d.Repo, d.ID)
	case d.URL == "":
		return fmt.Errorf("search: %s/%s: url is required", d.Repo, d.ID)
	case d.Visibility != Public && d.Visibility != Private:
		return fmt.Errorf("search: %s/%s: visibility must be %q or %q", d.Repo, d.ID, Public, Private)
	}
	for k := range d.Fields {
		if k == "" || k == FieldKind || k == FieldRepo || k == FieldVisibility {
			return fmt.Errorf("search: %s/%s: invalid field name %q", d.Repo, d.ID, k)
		}
	}
	return nil
}

// field returns a facet value; built-ins take precedence over Fields.
func (d Document) field(name string) string {
	switch name {
	case FieldKind:
		return d.Kind
	case FieldRepo:
		return d.Repo
	case FieldVisibility:
		return d.Visibility
	}
	return d.Fields[name]
}

func (d Document) clone() Document {
	if d.Fields != nil {
		fields := make(map[string]string, len(d.Fields))
		for k, v := range d.Fields {
			fields[k] = v
		}
		d.Fields = fields
	}
	return d
}

// tokenRE preserves identifiers, dotted keys and --flags as whole terms and
// also indexes their components, as pkg/codeindex does. Unlike codeindex,
// terms may start with a digit so issue and PR numbers ("14491") are
// searchable. Classes are ASCII and matched on original-case text so
// highlight offsets stay byte-accurate.
var tokenRE = regexp.MustCompile(`--[A-Za-z][A-Za-z0-9_-]*|[A-Za-z0-9_]+(?:[.-][A-Za-z0-9_]+)*`)
var componentRE = regexp.MustCompile(`[A-Za-z0-9]+`)

func tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range tokenRE.FindAllString(text, -1) {
		out[strings.ToLower(token)] = true
		for _, part := range componentRE.FindAllString(token, -1) {
			out[strings.ToLower(part)] = true
		}
	}
	return out
}
