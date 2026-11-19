package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// ProvisionedSubdir holds one record per repository of the model labels
// the stage engine created there (workflowmodel.ProvisionedLabels):
//
//	workflows/provisioned/<owner>__<repo>.json
const ProvisionedSubdir = "provisioned"

var provisionedFileRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// provisionedPath is the record's file for repo; "" when repo is not an
// owner/name the file name can carry.
func (s *Store) provisionedPath(repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return ""
	}
	file := provisionedFileRe.ReplaceAllString(owner, "-") + "__" + provisionedFileRe.ReplaceAllString(name, "-") + ".json"
	return filepath.Join(s.dir(), ProvisionedSubdir, file)
}

// LoadProvisioned implements workflowmodel.ProvisionedStore.
func (s *Store) LoadProvisioned(repo string) (*workflowmodel.ProvisionedLabels, error) {
	path := s.provisionedPath(repo)
	if path == "" {
		return nil, fmt.Errorf("provisioned labels: repository %q is not owner/name", repo)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec workflowmodel.ProvisionedLabels
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("provisioned labels for %s: %w", repo, err)
	}
	return &rec, nil
}

// SaveProvisioned implements workflowmodel.ProvisionedStore.
func (s *Store) SaveProvisioned(rec workflowmodel.ProvisionedLabels) error {
	path := s.provisionedPath(rec.Repo)
	if path == "" {
		return fmt.Errorf("provisioned labels: repository %q is not owner/name", rec.Repo)
	}
	rec.Repo = strings.ToLower(strings.TrimSpace(rec.Repo))
	if rec.Labels == nil {
		rec.Labels = []string{}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, b)
}

// DeleteProvisioned implements workflowmodel.ProvisionedStore; a missing
// record is not an error.
func (s *Store) DeleteProvisioned(repo string) error {
	path := s.provisionedPath(repo)
	if path == "" {
		return fmt.Errorf("provisioned labels: repository %q is not owner/name", repo)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var _ workflowmodel.ProvisionedStore = (*Store)(nil)
