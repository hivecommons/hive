package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence/complexity"
)

// complexityFetchTimeout caps one default-branch tarball download.
const complexityFetchTimeout = 10 * time.Minute

// ErrComplexityArchive is wrapped by FetchDefaultBranchTree when GitHub does
// not return the archive.
var ErrComplexityArchive = errors.New("default branch archive unavailable")

// FetchDefaultBranchTree downloads repo's ("owner/name") default branch as a
// GitHub tarball into a fresh temporary directory and returns it as a
// complexity.Checkout whose Cleanup removes the directory. Read-only: it
// uses the App's contents:read scope and never writes to the repo. It is the
// Source the monthly complexity snapshot runner uses (hivecommons/hive#12363).
func (c *Client) FetchDefaultBranchTree(ctx context.Context, repo string) (complexity.Checkout, error) {
	if c == nil || c.client == nil {
		return complexity.Checkout{}, ErrNoGitHubClient
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" {
		return complexity.Checkout{}, fmt.Errorf("FetchDefaultBranchTree: repo %q must be owner/name", repo)
	}
	branch, err := c.DefaultBranch(ctx, owner, name)
	if err != nil {
		return complexity.Checkout{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, complexityFetchTimeout)
	defer cancel()
	req, err := c.client.NewRequest(http.MethodGet, fmt.Sprintf("repos/%s/%s/tarball/%s", owner, name, branch), nil)
	if err != nil {
		return complexity.Checkout{}, err
	}
	// The archive endpoint 302s to a signed download URL; Go's client drops
	// the Authorization header on the cross-host hop, which is what GitHub
	// expects.
	resp, err := c.client.Client().Do(req.WithContext(ctx))
	if err != nil {
		return complexity.Checkout{}, fmt.Errorf("%w: %s: %w", ErrComplexityArchive, repo, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return complexity.Checkout{}, fmt.Errorf("%w: %s: %s", ErrComplexityArchive, repo, resp.Status)
	}
	dir, err := os.MkdirTemp("", "hive-complexity-")
	if err != nil {
		return complexity.Checkout{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	top, err := complexity.ExtractTarGz(resp.Body, dir, 0)
	if err != nil {
		cleanup()
		return complexity.Checkout{}, fmt.Errorf("%w: %s: %w", ErrComplexityArchive, repo, err)
	}
	return complexity.Checkout{Dir: top, Ref: branch, Cleanup: cleanup}, nil
}
