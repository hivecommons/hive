package config

import (
	"fmt"
	"regexp"
	"strings"
)

// KnowledgePublish is the `knowledge.publish` block: a one-way mirror that
// renders curator-promoted facts as pages in the external system behind one
// of the `knowledge.connectors` entries (see pkg/knowledge/connector). The
// vault stays the source of truth. An empty Connector disables publishing.
type KnowledgePublish struct {
	// Connector names the knowledge.connectors entry to publish through.
	Connector string `yaml:"connector,omitempty"`
	// Layers lists the vault layers whose promoted facts are published
	// (project, org, community). The personal layer is never published.
	Layers []string `yaml:"layers,omitempty"`
	// Root is the page/folder in the external system pages are written under.
	Root string `yaml:"root,omitempty"`
	// IncludeTypes restricts publishing to these fact types; empty means all.
	IncludeTypes []string `yaml:"include_types,omitempty"`
	// DryRun computes and audits the batch without writing upstream.
	DryRun bool `yaml:"dry_run,omitempty"`
	// ProposeVia is shown in each page footer as where to propose changes
	// (for example the repository URL).
	ProposeVia string `yaml:"propose_via,omitempty"`
}

// IsConfigured reports whether publishing is turned on.
func (p KnowledgePublish) IsConfigured() bool {
	return strings.TrimSpace(p.Connector) != ""
}

var (
	knowledgePublishLayers = map[string]bool{"project": true, "org": true, "community": true}
	knowledgeFactTypeRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
)

// ValidateKnowledgePublish checks the `knowledge.publish` block. Whether the
// named connector exists and supports publishing is checked when the mirror is
// built, so removing a connector never makes the whole config unloadable.
func ValidateKnowledgePublish(p KnowledgePublish) error {
	if !p.IsConfigured() {
		return nil
	}
	const field = "knowledge.publish"
	if !knowledgeConnectorNameRe.MatchString(strings.TrimSpace(p.Connector)) {
		return fmt.Errorf("%s.connector %q must be a knowledge.connectors name", field, p.Connector)
	}
	if len(p.Layers) == 0 {
		return fmt.Errorf("%s.layers is required (project, org and/or community)", field)
	}
	for i, l := range p.Layers {
		l = strings.TrimSpace(l)
		if l == "personal" {
			return fmt.Errorf("%s.layers[%d]: the personal layer is never published", field, i)
		}
		if !knowledgePublishLayers[l] {
			return fmt.Errorf("%s.layers[%d] %q must be project, org or community", field, i, p.Layers[i])
		}
	}
	root := strings.Trim(strings.TrimSpace(p.Root), "/")
	if root == "" {
		return fmt.Errorf("%s.root is required (the external page or folder to publish under)", field)
	}
	for _, seg := range strings.Split(root, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%s.root %q must be a plain page/folder path", field, p.Root)
		}
	}
	for i, t := range p.IncludeTypes {
		if !knowledgeFactTypeRe.MatchString(strings.TrimSpace(t)) {
			return fmt.Errorf("%s.include_types[%d] %q is not a fact type", field, i, t)
		}
	}
	return nil
}
