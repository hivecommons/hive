package complexity

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// monthFileLen is len("2006-01.json").
const monthFileLen = 12

// monthFiles lists a repo's snapshot files sorted by month ascending. Files
// whose name is not <YYYY-MM>.json are ignored.
func monthFiles(root, repo string) ([]string, error) {
	dir, err := Path(root, repo, "2000-01")
	if err != nil {
		return nil, err
	}
	dir = filepath.Dir(dir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) != monthFileLen || !strings.HasSuffix(name, ".json") {
			continue
		}
		if _, perr := time.Parse("2006-01", strings.TrimSuffix(name, ".json")); perr != nil {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// Prune removes a repo's oldest snapshots so at most keep remain. It returns
// how many files it removed.
func Prune(root, repo string, keep int) (int, error) {
	if keep <= 0 {
		keep = RetainMonths
	}
	files, err := monthFiles(root, repo)
	if err != nil {
		return 0, err
	}
	removed := 0
	for len(files)-removed > keep {
		if err := os.Remove(files[removed]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// RepoSeries is one repo's most recent snapshots, oldest first.
type RepoSeries struct {
	Repo      string     `json:"repo"`
	Snapshots []Snapshot `json:"snapshots"`
}

// LoadSeries reads every repo's snapshots under root, keeping the latest
// maxMonths per repo (<=0 means all), oldest first. Unreadable files are
// skipped. Repos are sorted by name. An absent root yields nil, nil.
func LoadSeries(root string, maxMonths int) ([]RepoSeries, error) {
	orgs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RepoSeries
	for _, org := range orgs {
		if !org.IsDir() {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, org.Name()))
		if err != nil {
			continue
		}
		for _, repo := range repos {
			if !repo.IsDir() {
				continue
			}
			full := org.Name() + "/" + repo.Name()
			files, err := monthFiles(root, full)
			if err != nil || len(files) == 0 {
				continue
			}
			if maxMonths > 0 && len(files) > maxMonths {
				files = files[len(files)-maxMonths:]
			}
			series := RepoSeries{Repo: full}
			for _, f := range files {
				s, lerr := Load(f)
				if lerr != nil {
					continue
				}
				series.Snapshots = append(series.Snapshots, s)
			}
			if len(series.Snapshots) > 0 {
				out = append(out, series)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out, nil
}
