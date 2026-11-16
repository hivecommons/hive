// Package campaignexport writes the one-time, read-only export of retired
// inception campaign data. Inception campaigns, campaign Jam and campaign
// recheck were retired in favour of Inception (spek) sessions; the endpoints
// that read their files are gone, so on the first start after the upgrade
// Hive bundles whatever is still on disk into a tarball an operator can keep.
//
// The export only reads the source files. It never moves or deletes them:
// they stay in place for one release so a rollback still finds them.
package campaignexport

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// CampaignsDir is the archived-campaign tree, relative to the data dir.
	CampaignsDir = "inception/campaigns"
	// InceptionStateFile is the live inception state, relative to the data dir.
	InceptionStateFile = "inception/state.json"
	// JamFileName is the campaign Jam store in the metrics directory.
	JamFileName = "campaign-jam.json"
	// ExportsDir is where the tarball lands, relative to the data dir.
	ExportsDir = "exports"
	// exportPrefix and exportSuffix frame campaigns-<YYYY-MM-DD>.tar.gz.
	exportPrefix = "campaigns-"
	exportSuffix = ".tar.gz"
	exportDate   = "2006-01-02"
	// exportFileMode keeps the tarball readable by the hive user only: it
	// carries idea text, Q&A, Jam threads and participant names.
	exportFileMode fs.FileMode = 0o600
	// exportDirMode is used when the exports directory has to be created.
	exportDirMode fs.FileMode = 0o700
)

// JamPath resolves campaign-jam.json the way the retired Jam store did: the
// metrics dir, else the logs dir, else the agents dir, else the working dir.
func JamPath(metricsDir, logsDir, agentsDir string) string {
	for _, base := range []string{metricsDir, logsDir, agentsDir} {
		if base = strings.TrimSpace(base); base != "" {
			return filepath.Join(base, JamFileName)
		}
	}
	return JamFileName
}

// Export writes <dataDir>/exports/campaigns-<date>.tar.gz when retired
// campaign data exists and no earlier export does. It returns the path it
// wrote, or "" when there was nothing to export or an export already exists
// (so the caller logs the path exactly once across restarts).
//
// The tarball holds inception/campaigns/** (archive state and wiki
// snapshots), inception/state.json when present, and campaign-jam.json.
// Symlinks and other non-regular files are skipped, never followed.
func Export(dataDir, jamPath string, now time.Time) (string, error) {
	campaigns := filepath.Join(dataDir, CampaignsDir)
	haveCampaigns := isDir(campaigns)
	haveJam := jamPath != "" && isRegular(jamPath)
	if !haveCampaigns && !haveJam {
		return "", nil
	}
	exports := filepath.Join(dataDir, ExportsDir)
	existing, err := filepath.Glob(filepath.Join(exports, exportPrefix+"*"+exportSuffix))
	if err != nil {
		return "", fmt.Errorf("campaign export: look for an earlier export: %w", err)
	}
	if len(existing) > 0 {
		return "", nil
	}
	if err := os.MkdirAll(exports, exportDirMode); err != nil {
		return "", fmt.Errorf("campaign export: create %s: %w", exports, err)
	}
	final := filepath.Join(exports, exportPrefix+now.UTC().Format(exportDate)+exportSuffix)
	tmp, err := os.CreateTemp(exports, "."+exportPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("campaign export: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(exportFileMode); err != nil {
		return "", fmt.Errorf("campaign export: chmod temp file: %w", err)
	}
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	if haveCampaigns {
		if err := addTree(tw, dataDir, campaigns); err != nil {
			return "", err
		}
	}
	if state := filepath.Join(dataDir, InceptionStateFile); isRegular(state) {
		if err := addFile(tw, state, InceptionStateFile); err != nil {
			return "", err
		}
	}
	if haveJam {
		if err := addFile(tw, jamPath, JamFileName); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("campaign export: finish tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("campaign export: finish gzip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("campaign export: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("campaign export: close: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("campaign export: rename to %s: %w", final, err)
	}
	ok = true
	return final, nil
}

// addTree adds every directory and regular file under root, named relative
// to base with forward slashes.
func addTree(tw *tar.Writer, base, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("campaign export: walk %s: %w", path, err)
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return fmt.Errorf("campaign export: name %s: %w", path, err)
		}
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("campaign export: stat %s: %w", path, err)
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("campaign export: header %s: %w", path, err)
			}
			hdr.Name = name + "/"
			return tw.WriteHeader(hdr)
		case d.Type().IsRegular():
			return addFile(tw, path, name)
		default:
			// Symlinks, sockets and devices are never followed or copied.
			return nil
		}
	})
}

// addFile copies one regular file into the tar under name.
func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("campaign export: open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("campaign export: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("campaign export: header %s: %w", path, err)
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("campaign export: write header %s: %w", name, err)
	}
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("campaign export: copy %s: %w", name, err)
	}
	return nil
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

func isRegular(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
