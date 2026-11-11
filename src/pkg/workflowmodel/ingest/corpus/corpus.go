// Package corpus embeds the CNCF workflow-survey corpus: the list of
// projects (and their GitHub repositories) whose CI/CD pipeline files were
// surveyed when designing the workflow model (#17620). The hub marketplace
// refresh seeds its `ingested-unreviewed` tier from this list, and
// cmd/workflow-survey falls back to it when a corpus directory carries no
// projects.json of its own.
package corpus

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

//go:embed corpus.json
var corpusJSON []byte

// Project is one surveyed CNCF project.
type Project struct {
	Name     string   `json:"name"`
	Maturity string   `json:"maturity"`
	Category string   `json:"category"`
	Repos    []string `json:"repos"`
}

var (
	loadOnce sync.Once
	loaded   []Project
	loadErr  error
)

func load() ([]Project, error) {
	loadOnce.Do(func() {
		var ps []Project
		if err := json.Unmarshal(corpusJSON, &ps); err != nil {
			loadErr = err
			return
		}
		loaded = ps
	})
	return loaded, loadErr
}

// Projects returns a copy of the embedded project list, sorted by name.
func Projects() []Project {
	ps, err := load()
	if err != nil {
		panic("corpus: embedded corpus.json is invalid: " + err.Error())
	}
	out := make([]Project, len(ps))
	for i, p := range ps {
		out[i] = p
		out[i].Repos = append([]string(nil), p.Repos...)
	}
	return out
}

// Repos returns every distinct `owner/repo` in the corpus, lower-cased and
// sorted, so callers iterate deterministically.
func Repos() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range Projects() {
		for _, r := range p.Repos {
			r = strings.ToLower(strings.TrimSpace(r))
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
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
