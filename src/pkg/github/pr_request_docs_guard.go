package github

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// docsGuardArchiveTimeout bounds the whole Tier B precheck (#9550): download
// the head's tarball, extract it, and run the docs guard scripts CI would
// run. A slow or hanging archive fetch must not stall PR-request processing
// indefinitely.
const docsGuardArchiveTimeout = 3 * time.Minute

// docsGuardMaxArchiveBytes caps the downloaded tarball so a misbehaving
// response (or an unexpectedly huge repo) cannot exhaust the watcher's
// memory or disk.
const docsGuardMaxArchiveBytes = 200 << 20 // 200MiB

// touchesDocsGuardSurface mirrors the trigger paths in
// .github/workflows/docs-link-check.yml ('*.md', 'src/**', 'docs/**',
// 'bin/**'): any of these being touched means CI's docs-link-check job would
// run. The precheck runs the same scripts before the PR opens instead of a
// few minutes after (#9550, Tier B of #9481's unshipped remainder).
func touchesDocsGuardSurface(files []*gh.CommitFile) bool {
	for _, file := range files {
		if file == nil {
			continue
		}
		name := file.GetFilename()
		if name == "" {
			continue
		}
		if strings.HasSuffix(name, ".md") {
			return true
		}
		if strings.HasPrefix(name, "src/") || strings.HasPrefix(name, "docs/") || strings.HasPrefix(name, "bin/") {
			return true
		}
	}
	return false
}

// prRequestDocsGuardPrecheck runs the same docs guards
// .github/workflows/docs-link-check.yml runs — check-docs-links.py,
// check-api-reference-citations.sh, check-docs-citations.py — against a
// checkout of head, and returns a non-empty reason if any of them fail. This
// is Tier B of #9481's proposal (#9550): a broken relative link, heading
// anchor, or file:line citation is deterministic on the candidate branch, so
// catching it here means the agent can still amend the branch instead of
// waiting on a red CI check a few minutes later.
//
// Archive download/extraction failures (network hiccup, missing python3/bash
// in this environment) are logged and otherwise ignored here rather than
// turned into a permanent rejection: unlike a script's exit code, they are
// not a property of the candidate branch, and a candidate should not be
// rejected forever for an unrelated infrastructure failure.
func (c *Client) prRequestDocsGuardPrecheck(ctx context.Context, owner, repo, head string, files []*gh.CommitFile) string {
	if c == nil || c.client == nil {
		return ""
	}
	if !touchesDocsGuardSurface(files) {
		return ""
	}

	ctx, cancel := context.WithTimeout(ctx, docsGuardArchiveTimeout)
	defer cancel()

	checkoutDir, cleanup, err := c.downloadArchive(ctx, owner, repo, head)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("docs guard precheck: could not prepare checkout; skipping",
				"repo", owner+"/"+repo, "head", head, "error", err)
		}
		return ""
	}
	defer cleanup()

	if _, err := os.Stat(filepath.Join(checkoutDir, "src", "docs")); err != nil {
		// No src/docs in this checkout (e.g. a repo without the docs guard
		// scripts) — nothing to check.
		return ""
	}

	if out, err := runDocsGuardScript(ctx, checkoutDir, "python3", "src/scripts/check-docs-links.py", "src/docs"); err != nil {
		return fmt.Sprintf("docs guard check-docs-links.py failed on %s: %s", head, truncateGuardOutput(out))
	}
	if out, err := runDocsGuardScript(ctx, checkoutDir, "bash", "src/scripts/check-api-reference-citations.sh"); err != nil {
		return fmt.Sprintf("docs guard check-api-reference-citations.sh failed on %s: %s", head, truncateGuardOutput(out))
	}
	if out, err := runDocsGuardScript(ctx, checkoutDir, "python3", "src/scripts/check-docs-citations.py", "src/docs"); err != nil {
		return fmt.Sprintf("docs guard check-docs-citations.py failed on %s: %s", head, truncateGuardOutput(out))
	}
	return ""
}

// truncateGuardOutput keeps a failing script's combined output short enough
// to fit in a precheck rejection reason without dropping the actionable
// first lines (the reported file:line and error message normally come
// first).
func truncateGuardOutput(out string) string {
	const max = 800
	out = strings.TrimSpace(out)
	if len(out) <= max {
		return out
	}
	return out[:max] + "… (truncated)"
}

// runDocsGuardScript runs a docs guard script with dir as its working
// directory, so script-relative paths like src/scripts/... resolve the same
// way they do when CI runs them from the repo root.
func runDocsGuardScript(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return "", nil
}

// downloadArchive fetches and extracts the tarball for ref into a fresh temp
// directory, stripping GitHub's synthetic top-level "<owner>-<repo>-<sha>/"
// path component so script-relative paths resolve directly under the
// returned directory. The caller must invoke the returned cleanup func to
// remove the extracted tree.
func (c *Client) downloadArchive(ctx context.Context, owner, repo, ref string) (string, func(), error) {
	// maxRedirects=0: GetArchiveLink returns the redirect Location without
	// following it, so the actual bytes are fetched below with a plain HTTP
	// client rather than routing the download through go-github's transport.
	archiveURL, _, err := c.client.Repositories.GetArchiveLink(ctx, owner, repo, gh.Tarball, &gh.RepositoryContentGetOptions{Ref: ref}, 0)
	if err != nil {
		return "", nil, fmt.Errorf("resolving archive link for %s/%s@%s: %w", owner, repo, ref, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL.String(), nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("downloading archive for %s/%s@%s: %w", owner, repo, ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("downloading archive for %s/%s@%s: unexpected status %s", owner, repo, ref, resp.Status)
	}

	dir, err := os.MkdirTemp("", "hive-docs-guard-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	if err := extractTarGz(io.LimitReader(resp.Body, docsGuardMaxArchiveBytes+1), dir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("extracting archive for %s/%s@%s: %w", owner, repo, ref, err)
	}
	return dir, cleanup, nil
}

// extractTarGz extracts a gzip-compressed tarball into dir, stripping each
// entry's first path component (GitHub's archive root directory) and
// rejecting any entry whose resolved path would escape dir (zip-slip).
// Symlinks and other non-regular, non-directory entries are skipped: the
// docs guard scripts do not need them, and following an untrusted archive's
// symlinks is unnecessary risk.
func extractTarGz(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	root := filepath.Clean(dir) + string(os.PathSeparator)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		idx := strings.Index(name, "/")
		if idx < 0 {
			continue
		}
		name = name[idx+1:]
		if name == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("archive entry %q escapes extraction root", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeArchiveFile(target, tr); err != nil {
				return err
			}
		}
	}
}

func writeArchiveFile(target string, r io.Reader) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
