// Package corpus embeds the CNCF workflow-survey corpus: the list of
// projects (and their GitHub repositories) whose CI/CD pipeline files were
// surveyed when designing the workflow model (#17620). The hub marketplace
// refresh seeds its `ingested-unreviewed` tier from this list, and
// cmd/workflow-survey falls back to it when a corpus directory carries no
// projects.json of its own.
//
// corpus.json is the survey's projects.json and is regenerated from the
// CNCF landscape; do not hand-edit it. extra.json is the operator-added
// list of repositories that are not landscape projects but whose workflow
// the marketplace should still ingest (cncf/techdocs, for one). Add a repo
// there: {"name", "category", "repos": ["owner/repo"], "why"}. Extra
// entries carry no maturity.
package corpus

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed corpus.json
var corpusJSON []byte

//go:embed extra.json
var extraJSON []byte

// Project is one surveyed CNCF project.
type Project struct {
	Name     string   `json:"name"`
	Maturity string   `json:"maturity"`
	Category string   `json:"category"`
	Repos    []string `json:"repos"`
}

// Extra is one operator-added, non-landscape repository group (extra.json).
type Extra struct {
	Name     string   `json:"name"`
	Category string   `json:"category,omitempty"`
	Repos    []string `json:"repos"`
	// Why records the reason the entry is in the corpus.
	Why string `json:"why,omitempty"`
}

var (
	loadOnce    sync.Once
	loaded      []Project
	loadedExtra []Extra
	loadErr     error
)

func load() ([]Project, []Extra, error) {
	loadOnce.Do(func() {
		var ps []Project
		if err := json.Unmarshal(corpusJSON, &ps); err != nil {
			loadErr = fmt.Errorf("corpus.json: %w", err)
			return
		}
		var xs []Extra
		if err := json.Unmarshal(extraJSON, &xs); err != nil {
			loadErr = fmt.Errorf("extra.json: %w", err)
			return
		}
		loaded, loadedExtra = ps, xs
	})
	return loaded, loadedExtra, loadErr
}

// Projects returns a copy of the embedded project list, sorted by name.
func Projects() []Project {
	ps, _, err := load()
	if err != nil {
		panic("corpus: embedded corpus is invalid: " + err.Error())
	}
	out := make([]Project, len(ps))
	for i, p := range ps {
		out[i] = p
		out[i].Repos = append([]string(nil), p.Repos...)
	}
	return out
}

// Extras returns a copy of the operator-added non-landscape entries.
func Extras() []Extra {
	_, xs, err := load()
	if err != nil {
		panic("corpus: embedded corpus is invalid: " + err.Error())
	}
	out := make([]Extra, len(xs))
	for i, x := range xs {
		out[i] = x
		out[i].Repos = append([]string(nil), x.Repos...)
	}
	return out
}

// Repos returns every distinct `owner/repo` in the corpus — survey
// projects plus extras — lower-cased and sorted, so callers iterate
// deterministically.
func Repos() []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}
	for _, p := range Projects() {
		for _, r := range p.Repos {
			add(r)
		}
	}
	for _, x := range Extras() {
		for _, r := range x.Repos {
			add(r)
		}
	}
	sort.Strings(out)
	return out
}

// Maturity maps lower-cased `owner/repo` to the CNCF maturity of the project
// that owns it (graduated/incubating/sandbox). Mirrors the shape consumed by
// cmd/workflow-survey's loadProjects.
func Maturity() map[string]string {
	out := map[string]string{}
	for _, p := range Projects() {
		for _, r := range p.Repos {
			out[strings.ToLower(r)] = p.Maturity
		}
	}
	return out
}
