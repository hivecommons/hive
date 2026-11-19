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
	if existing, _ := filepath.Glob(filepath.Join(exports, exportPrefix+"*"+exportSuffix)); len(existing) > 0 {
		return "", nil
	}
	if err := os.MkdirAll(exports, exportDirMode); err != nil {
		return "", fmt.Errorf("campaign export: create %s: %w", exports, err)
	}
	tmp, err := os.CreateTemp(exports, "."+exportPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("campaign export: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	err = writeArchive(tmp, dataDir, haveCampaigns, jamPath, haveJam)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	final := filepath.Join(exports, exportPrefix+now.UTC().Format(exportDate)+exportSuffix)
	if err == nil {
		err = os.Rename(tmpName, final)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("campaign export: %w", err)
	}
	return final, nil
}

// writeArchive streams the gzipped tar into f (already created 0600 by
// os.CreateTemp) and syncs it.
func writeArchive(f *os.File, dataDir string, haveCampaigns bool, jamPath string, haveJam bool) error {
	if err := f.Chmod(exportFileMode); err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var err error
	if haveCampaigns {
		err = addTree(tw, dataDir, filepath.Join(dataDir, CampaignsDir))
	}
	if state := filepath.Join(dataDir, InceptionStateFile); err == nil && isRegular(state) {
		err = addFile(tw, state, InceptionStateFile)
	}
	if err == nil && haveJam {
		err = addFile(tw, jamPath, JamFileName)
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	return err
}

// addTree adds every directory and regular file under root, named relative
// to base with forward slashes. Symlinks, sockets and devices are never
// followed or copied.
func addTree(tw *tar.Writer, base, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path) // path is always under base
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(exportDirMode)})
		case d.Type().IsRegular():
			return addFile(tw, path, name)
		default:
			return nil
		}
	})
}

// addFile copies one regular file into the tar under name.
func addFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(exportFileMode), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
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
