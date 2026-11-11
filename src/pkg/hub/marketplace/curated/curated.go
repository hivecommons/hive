// Package curated embeds the hub marketplace's curated workflow models
// (hivecommons/hive#17620 phase 7). Each *.yaml here is one reviewed model
// at one version; the hub seeds the catalogue from them at startup and on
// every refresh run, publishing each name@version that is not yet present.
// Entries are immutable: a change is a new file with a new spec.meta.version,
// never an edit in place.
package curated

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

//go:embed *.yaml
var files embed.FS

// Errors a curated file can fail with.
var (
	ErrNotCurated = errors.New("curated: spec.meta.provenance must be curated")
	ErrNoVersion  = errors.New("curated: spec.meta.version is empty")
	ErrInferred   = errors.New("curated: model still carries inferred elements")
)

// Entry is one embedded curated model.
type Entry struct {
	Path  string
	Model *workflowmodel.Model
}

// Decode parses one curated model document (YAML or JSON) strictly and
// checks the curated invariants: provenance curated, a version, and no
// element left at confidence inferred. It does not compile the model; the
// seed does that through bundle.Export.
func Decode(data []byte) (*workflowmodel.Model, error) {
	var doc interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("curated: yaml: %w", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("curated: yaml to json: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m workflowmodel.Model
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("curated: not a WorkflowModel: %w", err)
	}
	if m.Spec.Meta.Provenance != workflowmodel.ProvenanceCurated {
		return nil, fmt.Errorf("%w: got %q", ErrNotCurated, m.Spec.Meta.Provenance)
	}
	if strings.TrimSpace(m.Spec.Meta.Version) == "" {
		return nil, ErrNoVersion
	}
	if n := workflowmodel.Inferred(&m); n > 0 {
		return nil, fmt.Errorf("%w: %d unconfirmed", ErrInferred, n)
	}
	return &m, nil
}

// Load decodes every embedded *.yaml in path order. Files that fail are
// reported in errs (one per file, prefixed with the path) and skipped, so
// one bad file never hides the others.
func Load() (entries []Entry, errs []error) {
	return load(files)
}

func load(fsys fs.FS) (entries []Entry, errs []error) {
	// The pattern is a constant, so Glob cannot fail on it.
	names, _ := fs.Glob(fsys, "*.yaml")
	sort.Strings(names)
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		m, err := Decode(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		entries = append(entries, Entry{Path: name, Model: m})
	}
	return entries, errs
}
