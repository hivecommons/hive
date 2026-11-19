// Package perspsrc parses a live-linked review perspective set: a
// kind: ReviewPerspectives YAML document kept in a GitHub repo and named by
// review.perspectives_source (hivecommons/hive#17975), so maintainers can add,
// remove and re-describe perspectives through ordinary PRs.
//
// It is the review-set analogue of pkg/defsrc (which live-sources a whole
// agent definition) and follows the same design principles:
//
//   - Validate before use: a document is only accepted when it would load as
//     inline config would, via review.NewPerspectiveSet. A document that would
//     not is an error, never a silent fall back to the built-in set.
//   - Bounded input: a document over maxDocumentBytes is rejected outright.
//
// The document covers perspective names and focus text only. Cost and
// dispatch settings (combined_perspectives, plan_match,
// max_perspectives_per_pr) stay operator-owned in the hive config.
//
// This package, not pkg/config, owns the dependency on pkg/review, so
// pkg/config never has to import pkg/review.
package perspsrc

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/review"
	"gopkg.in/yaml.v3"
)

// maxDocumentBytes caps how large a ReviewPerspectives document may be. A real
// one is a short list (well under a kilobyte per perspective); this bounds
// memory/log blast radius if a source path points at something huge.
const maxDocumentBytes = 512 * 1024 // 512 KiB

// Kind is the required `kind` value of a review perspective document.
const Kind = "ReviewPerspectives"

// Document is the YAML format of a live-linked review perspective set.
type Document struct {
	Kind         string  `yaml:"kind" json:"kind"`
	Perspectives []Entry `yaml:"perspectives" json:"perspectives"`
}

// Entry is one perspective. Focus is optional for a built-in perspective
// (empty keeps the built-in text) and required for a hive-defined one.
type Entry struct {
	Name  string `yaml:"name" json:"name"`
	Focus string `yaml:"focus,omitempty" json:"focus,omitempty"`
}

// ParseDocument parses and validates a ReviewPerspectives document, returning
// the perspective names in dispatch order and the focus text keyed by name, in
// the same shape as review.perspectives and review.perspective_prompts.
func ParseDocument(doc string) (names []string, focus map[string]string, err error) {
	if len(doc) > maxDocumentBytes {
		return nil, nil, fmt.Errorf("perspectives document is %d bytes, over the limit of %d", len(doc), maxDocumentBytes)
	}
	var d Document
	if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
		return nil, nil, fmt.Errorf("invalid perspectives YAML: %w", err)
	}
	if d.Kind != Kind {
		return nil, nil, fmt.Errorf("expected kind %q, got %q", Kind, d.Kind)
	}
	if len(d.Perspectives) == 0 {
		return nil, nil, fmt.Errorf("perspectives list is empty")
	}
	names = make([]string, 0, len(d.Perspectives))
	focus = map[string]string{}
	seen := map[string]bool{}
	for i, e := range d.Perspectives {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return nil, nil, fmt.Errorf("perspectives[%d].name is required", i)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, nil, fmt.Errorf("duplicate perspective %q", name)
		}
		seen[key] = true
		names = append(names, name)
		if f := strings.TrimSpace(e.Focus); f != "" {
			focus[name] = f
		}
	}
	if _, err := review.NewPerspectiveSet(names, focus); err != nil {
		return nil, nil, err
	}
	return names, focus, nil
}
