package config

import (
	"fmt"
	"strings"
)

// WorkflowBinding assigns a stored workflow model (name@version) to a
// repository (hivecommons/hive#17620). Repositories without a binding keep
// the built-in hive-default behaviour unchanged.
type WorkflowBinding struct {
	Repo    string `yaml:"repo" json:"repo"`
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}

// Key returns the name@version identity of the bound model.
func (b WorkflowBinding) Key() string { return b.Name + "@" + b.Version }

// WorkflowBindingFor returns the binding covering repo, if any. repo may be
// bare or org-qualified.
func (c *Config) WorkflowBindingFor(repo string) (WorkflowBinding, bool) {
	if c == nil {
		return WorkflowBinding{}, false
	}
	key := repoPauseKey(c.Project.Org, repo)
	if key == "" {
		return WorkflowBinding{}, false
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	for _, b := range c.Project.WorkflowBindings {
		if repoPauseKey(c.Project.Org, b.Repo) == key {
			return b, true
		}
	}
	return WorkflowBinding{}, false
}

// SetWorkflowBinding binds repo to name@version, replacing any existing
// binding for the repo. An empty name removes the binding. It reports whether
// the config changed.
func (c *Config) SetWorkflowBinding(repo, name, version string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("nil config")
	}
	repo = strings.TrimSpace(repo)
	key := repoPauseKey(c.Project.Org, repo)
	if key == "" {
		return false, fmt.Errorf("workflow binding: empty repo")
	}
	name, version = strings.TrimSpace(name), strings.TrimSpace(version)
	if name != "" && version == "" {
		return false, fmt.Errorf("workflow binding: %s has no version", name)
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	kept := c.Project.WorkflowBindings[:0:0]
	changed := false
	for _, b := range c.Project.WorkflowBindings {
		if repoPauseKey(c.Project.Org, b.Repo) == key {
			changed = changed || name == "" || b.Name != name || b.Version != version
			continue
		}
		kept = append(kept, b)
	}
	if name != "" {
		if len(kept) == len(c.Project.WorkflowBindings) {
			changed = true
		}
		kept = append(kept, WorkflowBinding{Repo: repo, Name: name, Version: version})
	}
	if changed {
		c.Project.WorkflowBindings = kept
	}
	return changed, nil
}

func validateWorkflowBindings(bs []WorkflowBinding) error {
	seen := map[string]bool{}
	for i, b := range bs {
		if strings.TrimSpace(b.Repo) == "" {
			return fmt.Errorf("project.workflow_bindings[%d]: repo is required", i)
		}
		if strings.TrimSpace(b.Name) == "" || strings.TrimSpace(b.Version) == "" {
			return fmt.Errorf("project.workflow_bindings[%d] (%s): name and version are required", i, b.Repo)
		}
		k := strings.ToLower(b.Repo)
		if seen[k] {
			return fmt.Errorf("project.workflow_bindings: %s bound twice", b.Repo)
		}
		seen[k] = true
	}
	return nil
}
